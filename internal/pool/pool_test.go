package pool

import (
	"context"
	"testing"
	"time"
)

// A released lease must make the account immediately selectable again: the
// router releases on attempt completion, and without that a single-eligible
// account pool would answer no_available_account for the whole lease TTL.
func TestReleaseLeaseAllowsImmediateReselect(t *testing.T) {
	s := NewWeightedRandomSelector(0, 3, 60)
	s.AddAccount(&Account{ID: "a", Platform: "p", Status: "active", Credit: 10, Health: 1})

	first, err := s.Select(context.Background(), "p")
	if err != nil || first == nil {
		t.Fatalf("first select: acct=%v err=%v", first, err)
	}
	if second, _ := s.Select(context.Background(), "p"); second != nil {
		t.Fatalf("leased account must not be selectable twice, got %q", second.ID)
	}

	s.ReleaseLease("a")
	again, err := s.Select(context.Background(), "p")
	if err != nil || again == nil {
		t.Fatalf("after release the account must be selectable: acct=%v err=%v", again, err)
	}
	if again.ID != "a" {
		t.Fatalf("re-select picked %q, want the only account %q", again.ID, "a")
	}

	// Releasing an account with no lease (TTL backstop already fired, or a
	// double release) is a harmless no-op.
	s.ReleaseLease("ghost")
}

// TestExpiredCooldownReturnsToRotation pins the 2026-10-20 incident shape:
// UpdateHealth("rate_limited") cools an account for 2 minutes, but nothing
// ever reset the stored Status string when the window closed — Select gated
// on the stale field, so the platform's only account answered
// no_available_account forever after a single 429 (until a manual recover or
// process restart). Eligibility must be the live view: cooling only while
// the window is open.
func TestExpiredCooldownReturnsToRotation(t *testing.T) {
	s := NewWeightedRandomSelector(0, 3, 60)
	s.AddAccount(&Account{ID: "a", Platform: "p", Status: "active", Credit: 10, Health: 1})

	s.UpdateHealth("p", "a", "rate_limited", 0)
	if got, _ := s.Select(context.Background(), "p"); got != nil {
		t.Fatalf("account must sit out while cooling, got %q", got.ID)
	}

	// Expire the window the way time passing would: the stored Status stays
	// "cooling" (nothing rewrites it), the live view must read active.
	s.applyAccount("p", "a", func(acc *Account) {
		acc.CooldownEnd = time.Now().Add(-time.Second)
	})
	if acc := s.accounts["p"]["a"]; acc.Status != "cooling" || acc.EffectiveStatus() != "active" {
		t.Fatalf("expired cooldown: status=%q effective=%q, want stored cooling / effective active",
			acc.Status, acc.EffectiveStatus())
	}
	got, err := s.Select(context.Background(), "p")
	if err != nil || got == nil || got.ID != "a" {
		t.Fatalf("after expiry Select must return the account: acct=%v err=%v", got, err)
	}
	s.ReleaseLease("a")

	// The sticky seam must agree with Select.
	s.applyAccount("p", "a", func(acc *Account) { acc.CooldownEnd = time.Now().Add(-time.Second) })
	if acc := s.LeaseIfEligible("p", "a"); acc == nil {
		t.Fatal("LeaseIfEligible must reuse the account once its cooldown expired")
	}

	// A still-open window keeps both seams out, and disabled stays disabled.
	s.applyAccount("p", "a", func(acc *Account) { acc.CooldownEnd = time.Now().Add(time.Minute) })
	if got, _ := s.Select(context.Background(), "p"); got != nil {
		t.Fatal("account must sit out while the window is open")
	}
	if acc := s.LeaseIfEligible("p", "a"); acc != nil {
		t.Fatal("sticky seam must refuse while the window is open")
	}
	s.applyAccount("p", "a", func(acc *Account) { acc.CooldownEnd = time.Time{}; acc.Status = "disabled" })
	if got, _ := s.Select(context.Background(), "p"); got != nil {
		t.Fatal("disabled must stay out of rotation")
	}
}
