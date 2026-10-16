package router

// M10-B2 sticky-session tests: registry TTL semantics, the consultative
// reuse path (eligible bound account rides again; an ineligible one is
// dropped and re-selected), and the config-key plumbing (0 = off).

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"all2api/internal/host"
)

// newStickyHost gives every test an isolated host whose pool carries two
// eligible qa accounts (newTestHost/addAccount live in router_test.go).
func newStickyHost(t *testing.T) *host.Host {
	t.Helper()
	h := newTestHost(t)
	addAccount(t, h, "acc-1", "qa")
	addAccount(t, h, "acc-2", "qa")
	return h
}

// --- registry semantics ---

func TestStickyLookupSlideAndExpiry(t *testing.T) {
	s := NewStickyRegistry()
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	ttl := 30 * time.Minute

	s.Bind("caller-a", "m1", "qa", "acc-1", ttl)
	if platform, id := s.Lookup("caller-a", "m1", ttl); platform != "qa" || id != "acc-1" {
		t.Fatalf("lookup = %s/%s, want qa/acc-1", platform, id)
	}

	// Sliding TTL: a lookup 20 minutes in keeps the binding alive 30 more.
	now = now.Add(20 * time.Minute)
	if _, id := s.Lookup("caller-a", "m1", ttl); id != "acc-1" {
		t.Fatalf("live binding lost mid-session: %q", id)
	}
	// ...and the extra 20 + 30 > original expiry is proven 40 minutes in.
	now = now.Add(20 * time.Minute)
	if _, id := s.Lookup("caller-a", "m1", ttl); id != "acc-1" {
		t.Fatalf("sliding TTL must extend on use, lost at +40min: %q", id)
	}

	// Without any use, the same binding expires at 30 minutes.
	now = now.Add(11 * time.Minute) // last refresh at +40, so +51 > 70 original? no: refreshed to +70; +51 is live — walk past it
	now = now.Add(20 * time.Minute) // +71: expired
	if platform, id := s.Lookup("caller-a", "m1", ttl); id != "" {
		t.Fatalf("expired binding must drop on sight, got %s/%s", platform, id)
	}
	if n := s.ActiveBindings(); n != 0 {
		t.Fatalf("ActiveBindings after expiry = %d, want 0", n)
	}
}

func TestStickyIsolationAndGuards(t *testing.T) {
	s := NewStickyRegistry()
	s.Bind("caller-a", "m1", "qa", "acc-1", time.Minute)
	// Different caller or model never sees the binding.
	if _, id := s.Lookup("caller-b", "m1", time.Minute); id != "" {
		t.Fatalf("caller-b leaked into caller-a binding: %q", id)
	}
	if _, id := s.Lookup("caller-a", "m2", time.Minute); id != "" {
		t.Fatalf("m2 leaked into m1 binding: %q", id)
	}
	// Rebind wins.
	s.Bind("caller-a", "m1", "qa", "acc-2", time.Minute)
	if _, id := s.Lookup("caller-a", "m1", time.Minute); id != "acc-2" {
		t.Fatalf("rebind ignored: %q", id)
	}
	// Drop forgets exactly one binding.
	s.Drop("caller-a", "m1")
	if n := s.ActiveBindings(); n != 0 {
		t.Fatalf("Drop left %d bindings", n)
	}
	// Degenerate inputs never bind.
	s.Bind("", "m1", "qa", "acc-1", time.Minute)
	s.Bind("caller-a", "m1", "qa", "", time.Minute)
	s.Bind("caller-a", "m1", "qa", "acc-1", 0)
	if n := s.ActiveBindings(); n != 0 {
		t.Fatalf("degenerate binds landed: %d", n)
	}
	// Nil-receiver safety (zero-value Router parity).
	var nilRegistry *StickyRegistry
	nilRegistry.Bind("a", "m", "qa", "acc", time.Minute)
	if _, id := nilRegistry.Lookup("a", "m", time.Minute); id != "" {
		t.Fatal("nil registry must look up empty")
	}
	if n := nilRegistry.ActiveBindings(); n != 0 {
		t.Fatal("nil registry must report 0 bindings")
	}
}

func TestStickyTTLConfigKey(t *testing.T) {
	if got := stickyTTL(30); got != 30*time.Minute {
		t.Fatalf("stickyTTL(30) = %v, want 30m (frozen default)", got)
	}
	if got := stickyTTL(0); got != 0 {
		t.Fatalf("stickyTTL(0) = %v, want 0 (0=关)", got)
	}
	if got := stickyTTL(-5); got != 0 {
		t.Fatalf("stickyTTL(-5) = %v, want 0 (nonsense disables)", got)
	}
}

// --- consultative reuse through the router selection seam ---

func TestSelectAccountBindsAndReuses(t *testing.T) {
	h := newStickyHost(t)
	rt := &Router{host: h, sticky: NewStickyRegistry()}
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/v1/chat/completions", nil)
	req.Header.Set("X-API-Key", "caller-a")

	// First call goes through Select and binds whatever it chose.
	first, err := rt.selectAccount(req, "qa", "m1", 30*time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first select: acc=%v err=%v", first, err)
	}
	h.Pool.ReleaseLease(first.ID)
	if _, id := rt.sticky.Lookup("caller-a", "m1", 30*time.Minute); id != first.ID {
		t.Fatalf("binding = %q, want the selected %q", id, first.ID)
	}

	// Second call must return the SAME account (advisory lease, no re-draw).
	second, err := rt.selectAccount(req, "qa", "m1", 30*time.Minute)
	if err != nil || second == nil || second.ID != first.ID {
		t.Fatalf("sticky reuse returned %v (want %s), err %v", second, first.ID, err)
	}
	h.Pool.ReleaseLease(second.ID)
}

func TestSelectAccountDropsIneligibleBinding(t *testing.T) {
	h := newStickyHost(t)
	p := h.Pool
	rt := &Router{host: h, sticky: NewStickyRegistry()}
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/v1/chat/completions", nil)
	req.Header.Set("X-API-Key", "caller-a")

	first, err := rt.selectAccount(req, "qa", "m1", 30*time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first select: acc=%v err=%v", first, err)
	}
	p.ReleaseLease(first.ID)
	// The bound account cools down: stickiness must not bypass pool health.
	p.SetAccountCooldown("qa", first.ID, time.Now().Add(time.Minute))
	second, err := rt.selectAccount(req, "qa", "m1", 30*time.Minute)
	if err != nil || second == nil {
		t.Fatalf("re-select after cooldown: acc=%v err=%v", second, err)
	}
	if second.ID == first.ID {
		t.Fatal("bound account is cooling: selection must have re-drawn")
	}
	p.ReleaseLease(second.ID)
	// The registry now points at the replacement.
	if _, id := rt.sticky.Lookup("caller-a", "m1", 30*time.Minute); id != second.ID {
		t.Fatalf("rebind missing: registry=%q want %q", id, second.ID)
	}
}

func TestSelectAccountTTLOffMeansPlainSelect(t *testing.T) {
	h := newStickyHost(t)
	rt := &Router{host: h, sticky: NewStickyRegistry()}
	req := httptest.NewRequestWithContext(context.Background(), "POST", "/v1/chat/completions", nil)
	req.Header.Set("X-API-Key", "caller-a")

	first, err := rt.selectAccount(req, "qa", "m1", 0)
	if err != nil || first == nil {
		t.Fatalf("select with ttl=0: acc=%v err=%v", first, err)
	}
	h.Pool.ReleaseLease(first.ID)
	if n := rt.sticky.ActiveBindings(); n != 0 {
		t.Fatalf("ttl=0 must not bind, got %d bindings", n)
	}
}
