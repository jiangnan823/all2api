package pool

// M10-B2 pool additions: the cooldown reason record (status_note surface) and
// the sticky-session lease seam (LeaseIfEligible).

import (
	"testing"
	"time"
)

func newTestPool() *WeightedRandomSelector {
	return NewWeightedRandomSelector(100, 3, 60)
}

// --- cooldown reason recording ---

func TestTriggerCooldownReasonRecordsCause(t *testing.T) {
	a := &Account{ID: "a1", Platform: "qa", Status: "active"}
	if a.StatusNote() != "" {
		t.Fatalf("status_note before any cooldown = %q, want empty", a.StatusNote())
	}
	a.TriggerCooldownReason(time.Minute, CooldownReasonRateLimit)
	if !a.IsCooling() {
		t.Fatal("account must be cooling after TriggerCooldownReason")
	}
	if got := a.StatusNote(); got != "429 rate limit" {
		t.Fatalf("status_note = %q, want the frozen 429 phrase", got)
	}
	// A stale reason must not outlive its cooldown.
	a.CooldownEnd = time.Now().Add(-time.Second)
	if got := a.StatusNote(); got != "" {
		t.Fatalf("status_note after cooldown expiry = %q, want empty", got)
	}
}

func TestTriggerCooldownNoReasonStaysEmpty(t *testing.T) {
	a := &Account{ID: "a1", Platform: "qa", Status: "active"}
	a.TriggerCooldown(time.Minute)
	if !a.IsCooling() || a.StatusNote() != "" {
		t.Fatalf("bare TriggerCooldown must cool without a note: cooling=%v note=%q", a.IsCooling(), a.StatusNote())
	}
}

func TestUpdateHealthRateLimitRecords429Reason(t *testing.T) {
	s := newTestPool()
	s.AddAccount(&Account{ID: "a1", Platform: "qa", Status: "active", Credit: 500, Health: 1})
	s.UpdateHealth("qa", "a1", "rate_limited", 0)

	s.mu.RLock()
	acc := s.accounts["qa"]["a1"]
	reason := acc.CooldownReason
	cooling := acc.IsCooling()
	s.mu.RUnlock()

	if !cooling || reason != CooldownReasonRateLimit {
		t.Fatalf("429 cooldown reason = %q (cooling=%v), want the frozen phrase", reason, cooling)
	}
}

func TestSetAccountCooldownRecordsTaskFailedReason(t *testing.T) {
	s := newTestPool()
	s.AddAccount(&Account{ID: "a1", Platform: "qa", Status: "active", Credit: 500})
	if !s.SetAccountCooldown("qa", "a1", time.Now().Add(time.Minute)) {
		t.Fatal("SetAccountCooldown on a known account must succeed")
	}
	if got := s.SnapshotAccounts()[0].StatusNote(); got != CooldownReasonTaskFailed {
		t.Fatalf("task-failure cooldown note = %q, want %q", got, CooldownReasonTaskFailed)
	}
	// Unknown account stays a false, not a fabricated write.
	if s.SetAccountCooldown("qa", "ghost", time.Now().Add(time.Minute)) {
		t.Fatal("SetAccountCooldown on unknown account must return false")
	}
}

// --- LeaseIfEligible (sticky reuse seam) ---

func addEligible(s *WeightedRandomSelector, id string) {
	s.AddAccount(&Account{ID: id, Platform: "qa", Status: "active", Credit: 500, Health: 1})
}

func TestLeaseIfEligibleLeasesAndBlocksSecondLease(t *testing.T) {
	s := newTestPool()
	addEligible(s, "a1")

	acc := s.LeaseIfEligible("qa", "a1")
	if acc == nil || acc.ID != "a1" {
		t.Fatalf("first lease = %+v, want a1", acc)
	}
	// While leased, the same account is not reusable — anti-thundering parity.
	if again := s.LeaseIfEligible("qa", "a1"); again != nil {
		t.Fatal("second lease while held must fail")
	}
	s.ReleaseLease("a1")
	if acc = s.LeaseIfEligible("qa", "a1"); acc == nil {
		t.Fatal("lease after release must succeed")
	}
}

func TestLeaseIfEligibleRejectsIneligible(t *testing.T) {
	s := newTestPool()

	// One account per filter case; every mutation makes it ineligible under
	// exactly the same filters Select applies.
	cases := map[string]func(*Account){
		"disabled":    func(a *Account) { a.Status = "disabled" },
		"below floor": func(a *Account) { a.Credit = 50 },
		"cooling":     func(a *Account) { a.TriggerCooldown(time.Minute) },
		"paused":      func(a *Account) { a.Paused = true },
	}
	for name, breakIt := range cases {
		id := "acc-" + string(rune(len(name))) // deterministic distinct ids
		a := &Account{ID: id, Platform: "qa", Status: "active", Credit: 500, Health: 1}
		breakIt(a)
		s.AddAccount(a)
		if got := s.LeaseIfEligible("qa", id); got != nil {
			t.Fatalf("%s: LeaseIfEligible must reject, got %v", name, got)
		}
	}
}

func TestLeaseIfEligibleUnknownAccountNil(t *testing.T) {
	s := newTestPool()
	if acc := s.LeaseIfEligible("qa", "ghost"); acc != nil {
		t.Fatalf("unknown account lease = %v, want nil", acc)
	}
}
