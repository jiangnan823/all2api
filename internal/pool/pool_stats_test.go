package pool

// M6a observation-plane tests: success/fail counting and LastSuccess stamping
// in UpdateHealth, in-flight add/delta bookkeeping, the selection pause
// switch and the credit-total write-back. All reads go through
// SnapshotAccounts value copies — the same seam /api/accounts consumes.

import (
	"context"
	"testing"
	"time"
)

// snapshotOne finds one account in the pool snapshot (value copy).
func snapshotOne(t *testing.T, s *WeightedRandomSelector, platform, id string) Account {
	t.Helper()
	for _, acc := range s.SnapshotAccounts() {
		if acc.Platform == platform && acc.ID == id {
			return acc
		}
	}
	t.Fatalf("account %s/%s not in pool snapshot", platform, id)
	return Account{}
}

func TestUpdateHealthCountsSuccessAndStampsLastSuccess(t *testing.T) {
	s := NewWeightedRandomSelector(0, 3, 60)
	s.AddAccount(&Account{ID: "a", Platform: "p", Status: "active", Credit: 10, Health: 0.5})

	before := time.Now()
	s.UpdateHealth("p", "a", "success", 0)
	s.UpdateHealth("p", "a", "success", 0)

	acc := snapshotOne(t, s, "p", "a")
	if acc.SuccessCount != 2 || acc.FailCount != 0 {
		t.Fatalf("success counting: success=%d fail=%d, want 2/0", acc.SuccessCount, acc.FailCount)
	}
	if acc.LastSuccess.Before(before) {
		t.Fatalf("LastSuccess = %v, want stamped at/after %v", acc.LastSuccess, before)
	}
	if acc.Health <= 0.5 || acc.Health > 1.0 {
		t.Fatalf("health = %v, want raised above the 0.5 start (capped at 1.0) by two successes", acc.Health)
	}
}

func TestUpdateHealthCountsEveryFailureClass(t *testing.T) {
	s := NewWeightedRandomSelector(0, 3, 60)
	s.AddAccount(&Account{ID: "a", Platform: "p", Status: "active", Credit: 10, Health: 1})

	s.UpdateHealth("p", "a", "rate_limited", 0)
	s.UpdateHealth("p", "a", "timeout", 0)
	s.UpdateHealth("p", "a", "auth_error", 0)
	s.UpdateHealth("p", "a", "error", 0)

	acc := snapshotOne(t, s, "p", "a")
	if acc.FailCount != 4 || acc.SuccessCount != 0 {
		t.Fatalf("fail counting: fail=%d success=%d, want 4/0", acc.FailCount, acc.SuccessCount)
	}
	if !acc.LastSuccess.IsZero() {
		t.Fatalf("LastSuccess must stay zero without a success, got %v", acc.LastSuccess)
	}
	// auth_error parks the account in a bounded cooldown (2026-10-21: one
	// dispatch retry storm of upstream 403s must not permanently disable
	// healthy accounts); rate_limited keeps its shorter cooldown.
	if acc.Status != "cooling" || !acc.IsCooling() {
		t.Fatalf("status=%q cooling=%v, want cooling via the auth_error cooldown", acc.Status, acc.IsCooling())
	}
	if !acc.CooldownEnd.After(time.Now().Add(14 * time.Minute)) {
		t.Fatalf("auth cooldown ends %v, want ~15 minutes out", acc.CooldownEnd)
	}
}

func TestUpdateHealthUnknownAccountIsNoop(t *testing.T) {
	s := NewWeightedRandomSelector(0, 3, 60)
	s.UpdateHealth("p", "ghost", "success", 0)
	if got := len(s.SnapshotAccounts()); got != 0 {
		t.Fatalf("unknown account must not materialize, got %d entries", got)
	}
}

func TestInFlightAddRoundTrip(t *testing.T) {
	s := NewWeightedRandomSelector(0, 3, 60)
	s.AddAccount(&Account{ID: "a", Platform: "p", Status: "active", Credit: 10})

	s.InFlightAdd("p", "a", +1)
	s.InFlightAdd("p", "a", +1)
	if acc := snapshotOne(t, s, "p", "a"); acc.InFlight != 2 {
		t.Fatalf("in_flight = %d, want 2", acc.InFlight)
	}
	s.InFlightAdd("p", "a", -1)
	s.InFlightAdd("p", "a", -1)
	if acc := snapshotOne(t, s, "p", "a"); acc.InFlight != 0 {
		t.Fatalf("in_flight must return to 0, got %d", acc.InFlight)
	}
	// Unpaired decrements and unknown accounts must not drive it negative
	// (the wire value counts physical requests).
	s.InFlightAdd("p", "a", -1)
	s.InFlightAdd("p", "ghost", -1)
	if acc := snapshotOne(t, s, "p", "a"); acc.InFlight != 0 {
		t.Fatalf("in_flight must clamp at 0, got %d", acc.InFlight)
	}
}

func TestSelectSkipsPausedAccounts(t *testing.T) {
	s := NewWeightedRandomSelector(0, 3, 60)
	s.AddAccount(&Account{ID: "a", Platform: "p", Status: "active", Credit: 10})
	s.AddAccount(&Account{ID: "b", Platform: "p", Status: "active", Credit: 10})

	if !s.SetAccountPaused("p", "a", true) {
		t.Fatalf("pause a: want true (account exists)")
	}
	if acc := snapshotOne(t, s, "p", "a"); !acc.Paused {
		t.Fatalf("a must read back paused=true")
	}
	for i := 0; i < 20; i++ {
		acc, err := s.Select(context.Background(), "p")
		if err != nil || acc == nil {
			t.Fatalf("select %d: acct=%v err=%v (b must remain eligible)", i, acc, err)
		}
		if acc.ID == "a" {
			t.Fatalf("paused account a must never be selected")
		}
		s.ReleaseLease(acc.ID)
	}

	// Pause the last eligible account: no candidate at all.
	if !s.SetAccountPaused("p", "b", true) {
		t.Fatalf("pause b: want true")
	}
	if acc, _ := s.Select(context.Background(), "p"); acc != nil {
		t.Fatalf("fully paused pool must select nil, got %q", acc.ID)
	}

	// Resume re-opens the rotation; unknown accounts report false.
	if !s.SetAccountPaused("p", "a", false) {
		t.Fatalf("resume a: want true")
	}
	if acc, _ := s.Select(context.Background(), "p"); acc == nil || acc.ID != "a" {
		t.Fatalf("resumed pool must select again, got %v", acc)
	}
	if s.SetAccountPaused("p", "ghost", true) {
		t.Fatalf("pause ghost must report false")
	}
}

func TestSetAccountCreditTotal(t *testing.T) {
	s := NewWeightedRandomSelector(0, 3, 60)
	s.AddAccount(&Account{ID: "a", Platform: "p", Status: "active", Credit: 10})
	if !s.SetAccountCreditTotal("p", "a", 6679) {
		t.Fatalf("set total: want true")
	}
	if acc := snapshotOne(t, s, "p", "a"); acc.CreditTotal != 6679 || acc.Credit != 10 {
		t.Fatalf("credit=%d total=%d, want 10/6679 (balance untouched by the total write)", acc.Credit, acc.CreditTotal)
	}
	if s.SetAccountCreditTotal("p", "ghost", 1) {
		t.Fatalf("unknown account must report false")
	}
}
