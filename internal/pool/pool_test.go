package pool

import (
	"context"
	"testing"
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
