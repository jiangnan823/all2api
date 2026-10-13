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
	ReasonRateLimited  Reason = "rate_limited"  // 429
	ReasonNotFound     Reason = "not_found"     // 404
	ReasonPayment      Reason = "payment"       // 402
	ReasonConsecutive  Reason = "consecutive"   // too many failures
	ReasonDisabled     Reason = "disabled"      // manual disable switch
)

// Config for a single source's circuit breaker.
type Config struct {
	CooldownSec         int64   `json:"cooldown_sec"`           // base cooldown duration
	SoftRateMax         int64   `json:"soft_rate_max"`          // max 429s before escalation
	ConsecutiveFailures int     `json:"consecutive_failures"`   // threshold for circuit open
	BackoffMultiplier   float64 `json:"backoff_multiplier"`     // exponential backoff factor
	InitialBackoffSec   int64   `json:"initial_backoff_sec"`    // initial exponential backoff start
}

// DefaultConfig returns sensible defaults matching Panel behavior.
func DefaultConfig() Config {
	return Config{
		CooldownSec:         60,
		SoftRateMax:         600,
		ConsecutiveFailures: 5,
		BackoffMultiplier:   2.0,
		InitialBackoffSec:   60,
	}
}

// Breaker manages the circuit state for one source.
type Breaker struct {
	mu      sync.RWMutex
	config  Config
	state   State
	reason  Reason
	openedAt time.Time
	lastFailure time.Time
	failures  int
	nextReset time.Time
	accountState map[string]*AccountState // optional per-account tracking
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
	return &Breaker{
		config:       cfg,
		state:        StateClosed,
		accountState: make(map[string]*AccountState),
	}
}

// Allow checks if a request should pass through the circuit.
// Returns true if allowed, false if blocked.
func (b *Breaker) Allow() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(b.openedAt).Seconds() >= float64(b.config.CooldownSec) {
			return false // still in cooldown
		}
		// Transition to half-open for probe
		return false
	default:
		return true // half-open allows limited traffic
	}
}

// Record records a response outcome from the adapter.
func (b *Breaker) Record(statusCode int, accountID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	b.lastFailure = now

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
		// Rate limited — exponential backoff from soft_rate_max ceiling
		b.handleRateLimit(now)
	case 404:
		// Short cooldown only
		b.cooldown(b.config.CooldownSec)
	case 402:
		// Hard cooldown until next day 04:00
		nextDay := now.Add(24 * time.Hour)
		hour, _ := nextDay.Clock()
		if hour < 4 {
			nextDay = nextDay.Add(time.Duration(4-hour) * time.Hour)
		} else {
			nextDay = nextDay.Add(24*time.Hour + time.Duration(4-hour) * time.Hour)
		}
		b.hardCooldown(nextDay)
	default:
		if statusCode >= 500 || statusCode == 0 {
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
	b.reason = ""
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
func (b *Breaker) handleRateLimit(now time.Time) {
	exponential := int64(float64(b.config.InitialBackoffSec) * 
		b.config.BackoffMultiplier * float64(b.failures))
	backoffSec := min(exponential, b.config.SoftRateMax)

	b.cooldown(backoffSec)
}

// cooldown sets the circuit to open for dur seconds.
func (b *Breaker) cooldown(dur int64) {
	b.state = StateOpen
	b.openedAt = time.Now()
	b.reason = ReasonRateLimited
	b.failures++
	if b.failures >= b.config.ConsecutiveFailures {
		b.cooldown(dur)
	}
}

// hardCooldown sets the circuit to open until a specific time.
func (b *Breaker) hardCooldown(until time.Time) {
	b.state = StateOpen
	b.nextReset = until
	b.reason = ReasonPayment
}

// trip permanently opens the circuit until manually reset.
func (b *Breaker) trip(reason Reason) {
	b.state = StateOpen
	b.openedAt = time.Now()
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
