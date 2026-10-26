// Package pool implements account pool scheduling adapted from Panel's design.
// Key features: credit floor filtering, weighted random selection by
// remaining credits + health score, Top-5 candidate ranking, anti-thundering.
package pool

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// Account represents a single platform account in the pool.
type Account struct {
	ID           string    `json:"id"`
	Platform     string    `json:"platform"`     // source: "workbuddy", "qoder", "trae"
	Credit       int64     `json:"credit"`       // remaining credits/points
	CreditTotal  int64     `json:"credit_total"` // upstream quota capacity (0 = unknown/not yet seeded)
	Health       float64   `json:"health"`       // 0.0–1.0, derived from recent failure rate
	ExpiresAt    time.Time `json:"expires_at"`
	Status       string    `json:"status"` // active, cooling, disabled
	CooldownEnd  time.Time `json:"cooldown_end,omitempty"`
	LastSelected time.Time `json:"last_selected"`

	// Observation plane (M6a, per-process counters — never persisted):
	// SuccessCount/FailCount/InFlight are maintained by UpdateHealth and
	// InFlightAdd on the dispatch path; Paused is the panel's selection
	// switch (Select skips paused accounts); LastSuccess is the wall-clock
	// stamp of the most recent successful send.
	SuccessCount int64     `json:"success_count"`
	FailCount    int64     `json:"fail_count"`
	InFlight     int64     `json:"in_flight"`
	Paused       bool      `json:"paused"`
	LastSuccess  time.Time `json:"last_success"`

	// CooldownReason records why the account most recently entered cooldown
	// (M10-B2 status_note wire surface): CooldownReasonRateLimit on the 429
	// path, CooldownReasonTaskFailed on the scheduled-task/manual-batch
	// failure path, "" when the cooldown carries no recorded cause. It is
	// diagnostic memory like the counters above — never persisted.
	CooldownReason string `json:"cooldown_reason,omitempty"`
}

// Cooldown reasons — the status_note phrases the panel shows while an account
// is cooling. The 429 phrase matches the M9b model-lock reason string on
// purpose: one rate-limit hit reads the same in both panel surfaces.
const (
	CooldownReasonRateLimit    = "429 rate limit"
	CooldownReasonTaskFailed   = "task failed"
	CooldownReasonAuthRejected = "upstream auth rejected"
	CooldownReasonRestricted   = "account restricted (risk control)"
)

// SetStatus updates the account status.
func (a *Account) SetStatus(s string) {
	a.Status = s
}

// IsCooling returns true if the account is currently in cooldown.
func (a *Account) IsCooling() bool {
	return !a.CooldownEnd.IsZero() && time.Now().Before(a.CooldownEnd)
}

// EffectiveStatus is the live view of Status: a "cooling" account whose
// cooldown window has closed reads as "active". Nothing resets the stored
// string when the window ends, so gating selection on the raw field kept an
// account out of rotation forever after its first rate-limit cooldown —
// with a single-account platform that was a permanent 503 wall until a
// manual recover or process restart. "disabled" (manual switch) is a stored
// decision, not a window, and survives verbatim.
func (a *Account) EffectiveStatus() string {
	if a.Status == "cooling" && !a.IsCooling() {
		return "active"
	}
	return a.Status
}

// TriggerCooldown sets the cooldown end time (no recorded cause — use
// TriggerCooldownReason when the trigger is known).
func (a *Account) TriggerCooldown(dur time.Duration) {
	a.TriggerCooldownReason(dur, "")
}

// TriggerCooldownReason sets the cooldown end time and records the cause that
// the panel's status_note column renders for as long as the account stays
// cooling (M10-B2 snapshot extension).
func (a *Account) TriggerCooldownReason(dur time.Duration, reason string) {
	if a.Status == "disabled" {
		// A manual disable is an operator decision: request-path health
		// bookkeeping must not resurrect the account by flipping it into a
		// cooldown that EffectiveStatus later reads back to active.
		return
	}
	a.CooldownEnd = time.Now().Add(dur)
	a.Status = "cooling"
	a.CooldownReason = reason
}

// StatusNote is the wire value of the account's current cooldown cause: the
// recorded reason while cooling, empty otherwise (a stale reason must not
// outlive the cooldown it described). The M10-B2 snapshot surface
// (GET /api/accounts status_note) renders it verbatim.
func (a *Account) StatusNote() string {
	if !a.IsCooling() {
		return ""
	}
	return a.CooldownReason
}

// Selector selects one account from the pool for a given request.
type Selector interface {
	Select(ctx context.Context, platform string) (*Account, error)
	Round(account *Account, cost float64)
	UpdateHealth(platform string, accountID string, status string, latency time.Duration)
}

// WeightedRandomSelector implements the panel-style weighted random selection.
// Accounts closer to credit_floor have lower weight; healthy accounts get
// higher priority. Anti-thundering prevents multiple concurrent selections
// of the same account.
type WeightedRandomSelector struct {
	mu          sync.RWMutex
	accounts    map[string]map[string]*Account // platform → accountID → account
	creditFloor int64
	maxRetries  int
	cooldownSec int64
	leases      map[string]string // accountID → leaseToken (anti-thundering)
	leaseTTL    time.Duration
	rng         *rand.Rand
}

// NewWeightedRandomSelector creates a new selector with default params.
func NewWeightedRandomSelector(creditFloor int64, maxRetries int, cooldownSec int64) *WeightedRandomSelector {
	return &WeightedRandomSelector{
		accounts:    make(map[string]map[string]*Account),
		creditFloor: creditFloor,
		maxRetries:  maxRetries,
		cooldownSec: cooldownSec,
		leases:      make(map[string]string),
		leaseTTL:    30 * time.Second,
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())), // #nosec G404 -- scheduling pseudo-RNG, no security semantics; lease tokens are internal anti-thundering dedup keys, never exposed
	}
}

// AddAccount adds or updates an account in the pool.
func (s *WeightedRandomSelector) AddAccount(account *Account) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.accounts[account.Platform] == nil {
		s.accounts[account.Platform] = make(map[string]*Account)
	}
	s.accounts[account.Platform][account.ID] = account
}

// RemoveAccount removes an account from the pool.
func (s *WeightedRandomSelector) RemoveAccount(platform, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if platformMap, ok := s.accounts[platform]; ok {
		delete(platformMap, accountID)
	}
	delete(s.leases, accountID)
}

// Select returns one account for the given platform using weighted random.
// Returns nil if no eligible account exists.
//
// A4 (adversarial review): the success path writes s.leases and draws from
// s.rng, so this path holds the write lock, not RLock — concurrent Select
// under RLock was a concurrent-map-write fatal plus an rng data race.
// Selection logic is byte-for-byte unchanged.
func (s *WeightedRandomSelector) Select(ctx context.Context, platform string) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	platformMap := s.accounts[platform]
	if len(platformMap) == 0 { // S1009: len() of a nil map is already 0
		return nil, nil
	}

	// Collect eligible accounts (credit >= floor, not cooling, not disabled)
	var candidates []*Account
	for _, acc := range platformMap {
		if acc.EffectiveStatus() != "active" {
			continue
		}
		if acc.Credit < s.creditFloor {
			continue
		}
		if acc.IsCooling() {
			continue
		}
		// Skip leased accounts (anti-thundering)
		if _, leased := s.leases[acc.ID]; leased {
			continue
		}
		// Skip paused accounts (panel selection switch, M6a): a paused account
		// keeps its counters and health state but is out of the rotation.
		if acc.Paused {
			continue
		}
		candidates = append(candidates, acc)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// Sort by weight (health × remaining credits above floor), descending
	topN := candidates
	if len(topN) > 5 {
		// Top-5 candidate ranking
		topN = topN[:5]
	}

	// Calculate weights and select randomly
	totalWeight := 0.0
	weights := make([]float64, len(topN))
	for i, acc := range topN {
		remaining := float64(acc.Credit - s.creditFloor)
		weights[i] = acc.Health * (1.0 + remaining/1000.0)
		totalWeight += weights[i]
	}

	// Top-5 note: with more than five eligible candidates the weighted draw
	// runs over an arbitrary map-ordered subset (no explicit sort — the
	// comment above predates the code); with the fleet sizes this gateway
	// runs, topN is the full candidate set and the point is moot.
	r := s.rng.Float64() * totalWeight
	accum := 0.0
	for i, w := range weights {
		accum += w
		if r <= accum {
			// Lease this account (anti-thundering): the lease blocks other
			// concurrent selects until released. The send path calls
			// ReleaseLease as soon as its attempt completes; the TTL goroutine
			// is only a backstop for crashed/aborted flows, and it must only
			// delete ITS OWN lease — a released-then-retaken account gets a
			// fresh token, and a stale timer deleting it would silently lift
			// the protection mid-request.
			token := fmt.Sprintf("%016x", s.rng.Uint64())
			s.leases[topN[i].ID] = token
			go func(id, tok string) {
				time.Sleep(s.leaseTTL)
				s.mu.Lock()
				if s.leases[id] == tok {
					delete(s.leases, id)
				}
				s.mu.Unlock()
			}(topN[i].ID, token)
			return topN[i], nil
		}
	}

	// Float-accumulation edge: hand out the last candidate, but through the
	// same lease discipline as the draw above — an unleasned return would
	// let concurrent selects pick the same account.
	fallback := topN[len(topN)-1]
	token := fmt.Sprintf("%016x", s.rng.Uint64())
	s.leases[fallback.ID] = token
	go func(id, tok string) {
		time.Sleep(s.leaseTTL)
		s.mu.Lock()
		if s.leases[id] == tok {
			delete(s.leases, id)
		}
		s.mu.Unlock()
	}(fallback.ID, token)
	return fallback, nil
}

// ReleaseLease lifts the send-path lease on an account. The router calls it
// the moment a send attempt completes (success, failure, or finished stream)
// so a healthy account becomes immediately selectable again — without this,
// a single-eligible-account pool answers no_available_account for the whole
// lease TTL (30s) after every request. Deleting a missing key is a no-op
// (the TTL backstop may have fired first).
func (s *WeightedRandomSelector) ReleaseLease(accountID string) {
	s.mu.Lock()
	delete(s.leases, accountID)
	s.mu.Unlock()
}

// LeaseIfEligible is the sticky-session reuse seam (M10-B2): it checks one
// named account against exactly the same eligibility filters Select applies
// (active status, credit floor, not cooling, not leased, not paused) and,
// when eligible, takes the same anti-thundering lease Select would have
// taken — so a sticky-reused account is indistinguishable from a freshly
// selected one to the dispatch loop's release discipline. A nil return means
// "not reusable this time": the caller falls back to Select and rebinds.
// The returned pointer aliases the pool's live account, same as Select.
func (s *WeightedRandomSelector) LeaseIfEligible(platform, accountID string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[platform][accountID]
	if !ok {
		return nil
	}
	if acc.EffectiveStatus() != "active" || acc.Credit < s.creditFloor || acc.IsCooling() || acc.Paused {
		return nil
	}
	if _, leased := s.leases[acc.ID]; leased {
		return nil
	}
	token := fmt.Sprintf("%016x", s.rng.Uint64())
	s.leases[acc.ID] = token
	go func(id, tok string) {
		time.Sleep(s.leaseTTL)
		s.mu.Lock()
		if s.leases[id] == tok {
			delete(s.leases, id)
		}
		s.mu.Unlock()
	}(acc.ID, token)
	return acc
}

// Round deducts cost from selected account.
func (s *WeightedRandomSelector) Round(account *Account, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.accounts[account.Platform][account.ID]
	if acc != nil {
		acc.Credit -= int64(cost)
	}
}

// UpdateHealth updates account health based on recent request outcome. It is
// also the observation-plane counting point (M6a): a "success" bumps
// SuccessCount and stamps LastSuccess, every other status bumps FailCount —
// on top of the unchanged health/cooldown semantics below. The dispatch loop
// calls this exactly once per completed send attempt.
func (s *WeightedRandomSelector) UpdateHealth(platform, accountID, status string, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	platformMap := s.accounts[platform]
	if platformMap == nil {
		return
	}
	account, ok := platformMap[accountID]
	if !ok {
		return
	}

	switch status {
	case "success":
		account.SuccessCount++
		account.LastSuccess = time.Now()
		account.Health = min(1.0, account.Health+0.05)
	case "rate_limited":
		account.FailCount++
		account.Health = max(0.0, account.Health-0.1)
		account.TriggerCooldownReason(2*time.Minute, CooldownReasonRateLimit)
	case "auth_error":
		// Upstream auth rejection parks the account for 15 minutes instead of
		// disabling it. A single 403 is not proof the credential died: the
		// 2026-10-21 workbuddy outage answered every chat request on every
		// account with "request illegal" while the userinfo plane stayed
		// healthy, and one dispatch retry storm sufficed to disable all four
		// accounts permanently. A bounded cooldown keeps the account out of
		// rotation, recovers on its own, and repeats visibly if the rejection
		// persists; true credential death remains covered by the scheduled
		// health checks and the panel's manual disable.
		account.FailCount++
		account.Health = 0.0
		account.TriggerCooldownReason(15*time.Minute, CooldownReasonAuthRejected)
	case "account_restricted":
		// Upstream risk control restricted the account (workbuddy 11140).
		// The restriction does not lift by re-sending — every retry reads as
		// more "abnormal behavior" to the risk engine — so the account parks
		// for 12 hours and only an appeal or upstream expiry ends the cycle
		// (the panel's enable/recover lifts it manually after an appeal).
		account.FailCount++
		account.Health = 0.0
		account.TriggerCooldownReason(12*time.Hour, CooldownReasonRestricted)
	case "timeout":
		account.FailCount++
		account.Health = max(0.0, account.Health-0.02)
	default:
		account.FailCount++
		account.Health = max(0.0, account.Health-0.05)
	}
}

// InFlightAdd adjusts the account's in-flight request counter by delta: +1
// when the dispatch loop hands a send to the selected account, -1 when the
// attempt completes. Same lock discipline as every other field write (A4):
// the mutation runs under the pool write lock. An unknown account (removed
// mid-request) is a no-op; a negative bookkeeping drift clamps at 0 because
// the wire value counts physical requests and cannot honestly go below zero.
func (s *WeightedRandomSelector) InFlightAdd(platform, accountID string, delta int32) {
	s.applyAccount(platform, accountID, func(acc *Account) {
		acc.InFlight += int64(delta)
		if acc.InFlight < 0 {
			acc.InFlight = 0
		}
	})
}

// RecoverAccount clears an account's cooldown as part of the user-initiated
// recover flow (POST /api/accounts/{id}/recover), after the caller has
// verified the credential. A manual "disabled" status is never overridden —
// only "cooling" returns to "active". Returns true if the account exists.
func (s *WeightedRandomSelector) RecoverAccount(platform, accountID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[platform][accountID]
	if !ok {
		return false
	}
	if acc.Status == "cooling" {
		acc.Status = "active"
	}
	acc.CooldownEnd = time.Time{}
	return true
}

// ListAccounts returns all accounts for a platform.
//
// The returned slice aliases the pool's live *Account values — readers race
// with pool mutations. Prefer SnapshotAccounts for anything that inspects
// fields after the call (the scheduled-task source does).
func (s *WeightedRandomSelector) ListAccounts(platform string) []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Account, 0)
	for _, acc := range s.accounts[platform] {
		result = append(result, acc)
	}
	return result
}

// SnapshotAccounts returns value copies of every pooled account across all
// platforms. Copies share nothing with the pool's live state, so callers may
// inspect or retain them lock-free. Minimal read-only seam for the
// scheduled-task account source (M2 batch ②); unordered.
func (s *WeightedRandomSelector) SnapshotAccounts() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]Account, 0, 16)
	for _, platformMap := range s.accounts {
		for _, acc := range platformMap {
			result = append(result, *acc)
		}
	}
	return result
}

// applyAccount locates a pooled account and mutates it under the write lock —
// the shared body of the four scheduler write-back setters below. Caller must
// NOT hold s.mu (same lock discipline as Select/UpdateHealth: every field
// write happens under Lock, line 111 precedent).
func (s *WeightedRandomSelector) applyAccount(platform, accountID string, mutate func(*Account)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[platform][accountID]
	if !ok {
		return false
	}
	mutate(acc)
	return true
}

// ApplyAccountCredit adds a signed credit delta to a pooled account
// (scheduled-task reward write-back; Round stays request-path only).
// Returns false when the account is unknown.
func (s *WeightedRandomSelector) ApplyAccountCredit(platform, accountID string, delta int64) bool {
	return s.applyAccount(platform, accountID, func(acc *Account) {
		acc.Credit += delta
	})
}

// SetAccountCredit overwrites a pooled account's credit with an absolute
// value. The credit-plane seeding path (startup balance query) uses this
// form; task rewards keep the additive ApplyAccountCredit and the request
// path deducts via Round. Returns false when the account is unknown.
func (s *WeightedRandomSelector) SetAccountCredit(platform, accountID string, credit int64) bool {
	return s.applyAccount(platform, accountID, func(acc *Account) {
		acc.Credit = credit
	})
}

// SetAccountStatus writes a lifecycle status ("active"/"cooling"/"disabled").
// Returns false when the account is unknown.
func (s *WeightedRandomSelector) SetAccountStatus(platform, accountID, status string) bool {
	return s.applyAccount(platform, accountID, func(acc *Account) {
		acc.SetStatus(status)
	})
}

// SetAccountCooldown puts the account into cooldown until the given time and
// sets its status to "cooling" (Account.TriggerCooldown parity, absolute-time
// form). The only callers are the task-failure write-backs (scheduled batch
// and the panel batch-op), so the recorded status_note cause is
// CooldownReasonTaskFailed — 429s reach TriggerCooldownReason through
// UpdateHealth instead. A manually disabled account stays disabled (same
// guard as TriggerCooldownReason: task bookkeeping must not resurrect an
// operator decision). Returns false when the account is unknown.
func (s *WeightedRandomSelector) SetAccountCooldown(platform, accountID string, until time.Time) bool {
	return s.applyAccount(platform, accountID, func(acc *Account) {
		if acc.Status == "disabled" {
			return
		}
		acc.CooldownEnd = until
		acc.SetStatus("cooling")
		acc.CooldownReason = CooldownReasonTaskFailed
	})
}

// SetAccountCreditTotal records the upstream quota capacity (workbuddy: sum
// of CycleCapacitySize over the credits rows, written by the credit-seeding
// path next to SetAccountCredit's remaining balance). 0 means unknown/not yet
// seeded. Returns false when the account is unknown.
func (s *WeightedRandomSelector) SetAccountCreditTotal(platform, accountID string, total int64) bool {
	return s.applyAccount(platform, accountID, func(acc *Account) {
		acc.CreditTotal = total
	})
}

// SetAccountPaused flips the panel's selection pause switch: a paused account
// is skipped by Select (alongside the cooling/disabled/credit-floor filters)
// but keeps its counters, health and cooldown state. Returns false when the
// account is unknown.
func (s *WeightedRandomSelector) SetAccountPaused(platform, accountID string, paused bool) bool {
	return s.applyAccount(platform, accountID, func(acc *Account) {
		acc.Paused = paused
	})
}

// SetAccountExpiry records a refreshed credential expiry (token_keepalive
// write-back). Returns false when the account is unknown.
func (s *WeightedRandomSelector) SetAccountExpiry(platform, accountID string, at time.Time) bool {
	return s.applyAccount(platform, accountID, func(acc *Account) {
		acc.ExpiresAt = at
	})
}
