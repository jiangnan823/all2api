// Package circuit implements per-source independent circuit breakers adapted from Panel design.
// Key features: 429 exponential backoff (600s start), 404 short cooldown (60s),
// 402 hard cooldown (next day 04:00), consecutive failure熔断, in-flight lease limiting.
package circuit

import (
	"sync"
	"time"
)

// State represents the circuit breaker state for a single source+account.
type State int

const (
	StateClosed   State = iota // normal operation, requests pass through
	StateOpen                  // circuit tripped, requests blocked
	StateHalfOpen              // testing recovery, limited requests allowed
)

// Reason describes why the circuit is open.
type Reason string

const (
	ReasonRateLimited    Reason = "rate_limited"    // 429
	ReasonNotFound       Reason = "not_found"       // 404
	ReasonQuotaExhausted Reason = "quota_exhausted" // 402
	ReasonConsecutive    Reason = "consecutive"     // too many failures
	ReasonDisabled       Reason = "disabled"        // manual disable switch
)

// Config for a single source's circuit breaker.
type Config struct {
	CooldownSec         int64   `json:"cooldown_sec"`          // base cooldown duration
	SoftRateMax         int64   `json:"soft_rate_max"`         // max 429s before escalation
	ConsecutiveFailures int     `json:"consecutive_failures"`  // threshold for circuit open
	BackoffMultiplier   float64 `json:"backoff_multiplier"`    // exponential backoff factor
	InitialBackoffSec   int64   `json:"initial_backoff_sec"`   // initial exponential backoff start
	TripAutoRecoverSec  int64   `json:"trip_auto_recover_sec"` // consecutive-failure trip window before a half-open probe
}

// DefaultConfig returns sensible defaults matching Panel behavior.
func DefaultConfig() Config {
	return Config{
		CooldownSec:         60,
		SoftRateMax:         600,
		ConsecutiveFailures: 5,
		BackoffMultiplier:   2.0,
		InitialBackoffSec:   60,
		TripAutoRecoverSec:  1800,
	}
}

// Breaker manages the circuit state for one source.
type Breaker struct {
	mu            sync.RWMutex
	config        Config
	state         State
	reason        Reason
	openedAt      time.Time
	failures      int
	rateLimitHits int // consecutive 429 count — sole backoff exponent (P2: not polluted by 404/5xx failures)
	nextReset     time.Time
	accountState  map[string]*AccountState // optional per-account tracking
}

// AccountState tracks circuit state per account within a source.
type AccountState struct {
	CoolingEnd   time.Time
	FailCount    int
	TotalCalls   int
	LastResponse int
}

// NewBreaker creates a new circuit breaker for the given source.
func NewBreaker(source string, cfg Config) *Breaker {
	if cfg.TripAutoRecoverSec <= 0 {
		cfg.TripAutoRecoverSec = DefaultConfig().TripAutoRecoverSec
	}
	return &Breaker{
		config:       cfg,
		state:        StateClosed,
		accountState: make(map[string]*AccountState),
	}
}

// Allow checks if a request should pass through the circuit.
// Returns true if allowed, false if blocked.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		deadline := b.nextReset
		if deadline.IsZero() {
			deadline = b.openedAt.Add(time.Duration(b.config.CooldownSec) * time.Second)
		}
		if time.Now().Before(deadline) {
			return false // still in cooldown
		}
		// Cooldown expired: transition to half-open and allow a probe.
		b.state = StateHalfOpen
		b.nextReset = time.Time{}
		return true
	default:
		return true // half-open allows limited probe traffic
	}
}

// Record records a response outcome from the adapter.
func (b *Breaker) Record(statusCode int, accountID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()

	// Track per-account state
	if accountID != "" {
		if _, ok := b.accountState[accountID]; !ok {
			b.accountState[accountID] = &AccountState{}
		}
		st := b.accountState[accountID]
		st.TotalCalls++
		st.LastResponse = statusCode
	}

	switch statusCode {
	case 429:
		// Rate limited — exponential backoff capped at soft_rate_max
		b.handleRateLimit(now)
	case 404:
		// Short cooldown only
		b.cooldown(b.config.CooldownSec, ReasonNotFound)
	case 402:
		// Hard cooldown until the next upcoming 04:00 local time
		// (frozen contract: sources.<id>.throttle.backoff.exhaust_402_until = "next-04:00").
		// The previous "+24h then clock-adjust" formula landed on the day after
		// tomorrow's 04:00 — one full day too late.
		resetAt := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
		if !resetAt.After(now) {
			resetAt = resetAt.AddDate(0, 0, 1)
		}
		b.hardCooldown(resetAt)
	default:
		if statusCode >= 200 && statusCode < 400 {
			// Success (R-A / TC-M1-009 closure path): clears the consecutive
			// counter, and a half-open probe closes the circuit back to
			// StateClosed so recovery is automatic — no caller has to invoke
			// Reset() explicitly for the probe to land.
			b.failures = 0
			b.rateLimitHits = 0
			if b.state == StateHalfOpen {
				b.state = StateClosed
				b.reason = ""
				b.nextReset = time.Time{}
			}
		} else if statusCode >= 500 || statusCode == 0 {
			// Server error or connection error
			b.failures++
			if b.failures >= b.config.ConsecutiveFailures {
				b.trip(ReasonConsecutive)
			}
		}
	}
}

// Reset resets the breaker to closed state after successful probe.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClosed
	b.failures = 0
	b.rateLimitHits = 0
	b.reason = ""
	b.nextReset = time.Time{}
}

// State returns current breaker state.
func (b *Breaker) State() State {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.state
}

// Reason returns why the circuit is open.
func (b *Breaker) Reason() Reason {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.reason
}

// IsTripped returns true if the circuit is currently open.
func (b *Breaker) IsTripped() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.state == StateOpen
}

// handleRateLimit applies exponential backoff for 429 responses.
// Geometric growth InitialBackoffSec * multiplier^failures, capped at SoftRateMax.
// The previous formula (initial * multiplier * failures) was linear, not exponential.
func (b *Breaker) handleRateLimit(now time.Time) {
	backoff := float64(b.config.InitialBackoffSec)
	for i := 0; i < b.rateLimitHits; i++ {
		backoff *= b.config.BackoffMultiplier
		if backoff >= float64(b.config.SoftRateMax) {
			backoff = float64(b.config.SoftRateMax)
			break
		}
	}

	b.cooldown(int64(backoff), ReasonRateLimited)
	b.rateLimitHits++
}

// cooldown sets the circuit to open for dur seconds with the given reason.
// The previous version recursed into itself once failures crossed the
// consecutive threshold, causing unbounded recursion (stack overflow).
func (b *Breaker) cooldown(dur int64, reason Reason) {
	b.state = StateOpen
	b.openedAt = time.Now()
	b.nextReset = b.openedAt.Add(time.Duration(dur) * time.Second)
	b.reason = reason
	b.failures++
}

// hardCooldown sets the circuit to open until a specific time.
func (b *Breaker) hardCooldown(until time.Time) {
	b.state = StateOpen
	b.openedAt = time.Now()
	b.nextReset = until
	b.reason = ReasonQuotaExhausted
}

// trip opens the circuit after consecutive upstream failures. The window is
// bounded (TripAutoRecoverSec, 30min default): an unattended gateway must
// probe its way back after a transient upstream blip. The 2026-10-21
// incident had one ~30-second workbuddy hiccup at 03:05 park the platform
// behind a permanently open circuit for six hours because nothing but a
// manual recover could clear the latch. A half-open probe that succeeds
// closes the circuit (Record's 2xx path); five fresh consecutive failures
// re-trip it. Sources that must stay off have sources.<id>.enabled:false —
// the breaker is the wrong tool for a manual off-switch.
func (b *Breaker) trip(reason Reason) {
	b.state = StateOpen
	b.openedAt = time.Now()
	b.nextReset = b.openedAt.Add(time.Duration(b.config.TripAutoRecoverSec) * time.Second)
	b.reason = reason
	b.failures = 0 // reset counter on trip
}

// GetAllAccountStates returns per-account state snapshot (for panel P2 view).
func (b *Breaker) GetAllAccountStates() map[string]*AccountState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make(map[string]*AccountState)
	for k, v := range b.accountState {
		result[k] = v
	}
	return result
}
