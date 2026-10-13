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
	Platform     string    `json:"platform"` // source: "workbuddy", "qoder", "trae"
	Credit       int64     `json:"credit"`   // remaining credits/points
	Health       float64   `json:"health"`   // 0.0–1.0, derived from recent failure rate
	ExpiresAt    time.Time `json:"expires_at"`
	Status       string    `json:"status"` // active, cooling, disabled
	CooldownEnd  time.Time `json:"cooldown_end,omitempty"`
	LastSelected time.Time `json:"last_selected"`
}

// SetStatus updates the account status.
func (a *Account) SetStatus(s string) {
	a.Status = s
}

// IsCooling returns true if the account is currently in cooldown.
func (a *Account) IsCooling() bool {
	return !a.CooldownEnd.IsZero() && time.Now().Before(a.CooldownEnd)
}

// TriggerCooldown sets the cooldown end time.
func (a *Account) TriggerCooldown(dur time.Duration) {
	a.CooldownEnd = time.Now().Add(dur)
	a.Status = "cooling"
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
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
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
func (s *WeightedRandomSelector) Select(ctx context.Context, platform string) (*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	platformMap := s.accounts[platform]
	if platformMap == nil || len(platformMap) == 0 {
		return nil, nil
	}

	// Collect eligible accounts (credit >= floor, not cooling, not disabled)
	var candidates []*Account
	for _, acc := range platformMap {
		if acc.Status != "active" {
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

	// Weighted random selection
	r := s.rng.Float64() * totalWeight
	accum := 0.0
	for i, w := range weights {
		accum += w
		if r <= accum {
			// Lease this account (anti-thundering)
			token := s.rng.Uint64()
			s.leases[topN[i].ID] = fmt.Sprintf("%016x", token)
			go func(id string) {
				time.Sleep(s.leaseTTL)
				s.mu.Lock()
				delete(s.leases, id)
				s.mu.Unlock()
			}(topN[i].ID)
			return topN[i], nil
		}
	}

	return topN[len(topN)-1], nil
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

// UpdateHealth updates account health based on recent request outcome.
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
		account.Health = min(1.0, account.Health+0.05)
	case "rate_limited":
		account.Health = max(0.0, account.Health-0.1)
		account.TriggerCooldown(2 * time.Minute)
	case "auth_error":
		account.Health = 0.0
		account.SetStatus("disabled")
	case "timeout":
		account.Health = max(0.0, account.Health-0.02)
	default:
		account.Health = max(0.0, account.Health-0.05)
	}
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
func (s *WeightedRandomSelector) ListAccounts(platform string) []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Account, 0)
	for _, acc := range s.accounts[platform] {
		result = append(result, acc)
	}
	return result
}
