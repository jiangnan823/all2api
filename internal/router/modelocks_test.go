package router

// M9b model-lock registry tests: TTL semantics (Retry-After hint vs default
// cooldown, clamping, re-booking), aggregation (per model+platform counts and
// earliest unlock, deterministic order, expired pruning) and the two dispatch
// hooks — Select skips a locked (model, account) pair without a send, and a
// classified 429 from any failure branch (non-stream, stream-start, mid-stream)
// books a lock alongside the untouched breaker. Registries are always injected
// (Router.locks / fake clock) — never the process-wide DefaultModelLocks.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/pool"
)

// fakeClock is the injectable registry clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.Local)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestRegistry(c *fakeClock) *ModelLockRegistry {
	m := NewModelLockRegistry()
	m.now = c.Now
	return m
}

// ---------- registry: TTL semantics ----------

func TestModelLockTTLDefaultAndAutoUnlock(t *testing.T) {
	c := newFakeClock()
	m := newTestRegistry(c)

	m.NoteRateLimited("m1", "wb", "a1", 0) // no hint → default cooldown
	if !m.Locked("m1", "wb", "a1") {
		t.Fatal("pair should be locked right after NoteRateLimited")
	}
	c.Advance(DefaultModelLockTTL - time.Second)
	if !m.Locked("m1", "wb", "a1") {
		t.Fatal("pair should still be locked 1s before the default TTL runs out")
	}
	c.Advance(2 * time.Second)
	if m.Locked("m1", "wb", "a1") {
		t.Fatal("pair should auto-unlock after the default TTL")
	}
	if got := m.Snapshot(); len(got) != 0 {
		t.Fatalf("expired lock must be pruned from the panel view, got %d rows", len(got))
	}
}

func TestModelLockTTLHintAndClamp(t *testing.T) {
	c := newFakeClock()
	m := newTestRegistry(c)

	m.NoteRateLimited("m", "wb", "hint", 45*time.Second)
	c.Advance(44 * time.Second)
	if !m.Locked("m", "wb", "hint") {
		t.Fatal("45s hint: must still be locked at 44s")
	}
	c.Advance(2 * time.Second)
	if m.Locked("m", "wb", "hint") {
		t.Fatal("45s hint: must be unlocked at 46s")
	}

	m.NoteRateLimited("m", "wb", "clamp", 3*time.Hour) // beyond maxModelLockTTL
	c.Advance(maxModelLockTTL - time.Second)
	if !m.Locked("m", "wb", "clamp") {
		t.Fatal("3h hint must clamp to maxModelLockTTL: still locked at max-1s")
	}
	c.Advance(2 * time.Second)
	if m.Locked("m", "wb", "clamp") {
		t.Fatal("3h hint must clamp to maxModelLockTTL: unlocked at max+1s")
	}
}

func TestModelLockRebookingShortens(t *testing.T) {
	c := newFakeClock()
	m := newTestRegistry(c)

	m.NoteRateLimited("m", "wb", "a1", time.Hour)
	m.NoteRateLimited("m", "wb", "a1", time.Minute) // latest observation wins
	c.Advance(2 * time.Minute)
	if m.Locked("m", "wb", "a1") {
		t.Fatal("re-booking with a shorter hint must shorten the lock")
	}
}

// ---------- registry: aggregation ----------

func TestModelLockSnapshotAggregate(t *testing.T) {
	c := newFakeClock()
	m := newTestRegistry(c)

	m.NoteRateLimited("m1", "wb", "a1", time.Minute)    // unlock T0+60s
	m.NoteRateLimited("m1", "wb", "a2", 30*time.Second) // unlock T0+30s
	m.NoteRateLimited("m1", "qoder", "a9", 10*time.Second)
	m.NoteRateLimited("m2", "wb", "a3", 0) // default 2m

	got := m.Snapshot()
	if len(got) != 3 {
		t.Fatalf("want 3 aggregated rows, got %d: %+v", len(got), got)
	}
	// Deterministic order: model asc, then platform asc.
	want := []struct {
		model, platform string
		n               int
		earliest        time.Time
	}{
		{"m1", "qoder", 1, c.Now().Add(10 * time.Second)},
		{"m1", "wb", 2, c.Now().Add(30 * time.Second)},
		{"m2", "wb", 1, c.Now().Add(2 * time.Minute)},
	}
	for i, w := range want {
		g := got[i]
		if g.Model != w.model || g.Platform != w.platform || g.LockedAccounts != w.n {
			t.Errorf("row %d = %s/%s n=%d, want %s/%s n=%d", i, g.Model, g.Platform, g.LockedAccounts, w.model, w.platform, w.n)
		}
		if !g.EarliestUnlock.Equal(w.earliest) {
			t.Errorf("row %d earliest = %v, want %v", i, g.EarliestUnlock, w.earliest)
		}
		if g.Reason != ModelLockReason {
			t.Errorf("row %d reason = %q, want %q", i, g.Reason, ModelLockReason)
		}
	}

	// Advance past m1's locks: both m1 rows auto-unlock and vanish.
	c.Advance(time.Minute + time.Second)
	got = m.Snapshot()
	if len(got) != 1 || got[0].Model != "m2" {
		t.Fatalf("after expiry only m2 should remain, got %+v", got)
	}
}

// ---------- registry: nil safety ----------

func TestModelLockRegistryNilSafety(t *testing.T) {
	var m *ModelLockRegistry
	if m.Locked("m", "wb", "a") {
		t.Fatal("nil registry must report unlocked")
	}
	m.NoteRateLimited("m", "wb", "a", 0) // must not panic
	if got := m.Snapshot(); got != nil {
		t.Fatalf("nil registry snapshot = %+v, want nil", got)
	}
}

// ---------- retryHintFromError ----------

func TestRetryHintFromError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"rate limited with hint", &adapter.UpstreamError{Status: 429, Reason: adapter.ReasonRateLimited, Message: "slow down (retry after 1m30s)"}, 90 * time.Second},
		{"rate limited bare status", &adapter.UpstreamError{Status: 429, Reason: adapter.ReasonRateLimited, Message: "too many"}, 0},
		{"429 without canonical reason", &adapter.UpstreamError{Status: 429, Message: "busy (retry after 15s)"}, 15 * time.Second},
		{"non-429 reason ignores hint", &adapter.UpstreamError{Status: 500, Reason: adapter.ReasonPlatformDown, Message: "down (retry after 15s)"}, 0},
		{"unparseable hint", &adapter.UpstreamError{Status: 429, Reason: adapter.ReasonRateLimited, Message: "busy (retry after soon)"}, 0},
		{"non-upstream error", context.DeadlineExceeded, 0},
	}
	for _, tc := range cases {
		if got := retryHintFromError(tc.err); got != tc.want {
			t.Errorf("%s: retryHintFromError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ---------- dispatch hooks ----------

// newLocksMux builds the /v1 chat surface against an isolated registry (the
// production Register hardwires DefaultModelLocks): one fake sender serving
// platform "p1", pooled with one named account per id. The same sender is
// both registered and returned, so test-time knob tweaks (failFirst, …) apply.
func newLocksMux(t *testing.T, ml *ModelLockRegistry, accountIDs ...string) (*http.ServeMux, *fakeSender) {
	t.Helper()
	fs := &fakeSender{fakeAdapter: fakeAdapter{name: "p1", caps: adapter.CapChatCompletion}, resp: &adapter.ChatResponse{ID: "ok"}}
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	for _, id := range accountIDs {
		h.Pool.AddAccount(&pool.Account{ID: id, Platform: "p1", Status: "active", Credit: 1000, Health: 1.0})
	}
	rt := &Router{host: h, lookup: reg, locks: ml}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", rt.handleChatCompletions)
	return mux, fs
}

func TestDispatchSkipsModelLockedAccount(t *testing.T) {
	c := newFakeClock()
	ml := newTestRegistry(c)
	ml.NoteRateLimited("m1", "p1", "a1", 0) // a1 locked for m1 — booked out of band

	mux, fs := newLocksMux(t, ml, "a1", "a2")
	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200 (a2 serves the request)", rec.Code, rec.Body)
	}
	calls, _ := fs.callLog()
	if len(calls) != 1 || calls[0] != "p1:a2" {
		t.Fatalf("calls = %v, want exactly [p1:a2] — the locked a1 must never be sent to", calls)
	}
	if !ml.Locked("m1", "p1", "a1") {
		t.Fatal("pre-existing lock must survive a request that never touched a1")
	}
	if ml.Locked("m1", "p1", "a2") {
		t.Fatal("a successful send must not book a lock")
	}
}

func TestDispatchBooks429LockWithHint(t *testing.T) {
	c := newFakeClock()
	ml := newTestRegistry(c)
	mux, fs := newLocksMux(t, ml, "a1", "a2")
	fs.failFirst = 1
	fs.failErr = &adapter.UpstreamError{Status: 429, Reason: adapter.ReasonRateLimited, Message: "slow down (retry after 30s)"}

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200 (retry reaches a2)", rec.Code, rec.Body)
	}
	calls, _ := fs.callLog()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want 2 attempts", calls)
	}
	locked := strings.Split(calls[0], ":")[1] // first attempt is the 429'd one
	if !ml.Locked("m1", "p1", locked) {
		t.Fatalf("first attempt %q must be model-locked for m1", calls[0])
	}
	other := "a2"
	if locked == "a2" {
		other = "a1"
	}
	if ml.Locked("m1", "p1", other) {
		t.Fatalf("successful account %q must not be locked", other)
	}
	// The 30s hint governs the TTL: unlocked by +31s (a default 2m cooldown
	// would still be locked here).
	c.Advance(31 * time.Second)
	if ml.Locked("m1", "p1", locked) {
		t.Fatal("lock must expire after the upstream 30s retry hint, not the default TTL")
	}
}

func TestDispatchStreamStart429BooksLock(t *testing.T) {
	c := newFakeClock()
	ml := newTestRegistry(c)
	mux, fs := newLocksMux(t, ml, "a1", "a2")
	fs.chunks = []adapter.StreamChunk{{ID: "c1"}}
	fs.failFirst = 1
	// Stream-start failures reach the breaker as statusCode 0; the
	// model-lock booking must still classify the 429 from the error.
	fs.failErr = &adapter.UpstreamError{Status: 429, Reason: adapter.ReasonRateLimited, Message: "busy (retry after 45s)"}

	req := `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := postJSON(t, mux, "/v1/chat/completions", req, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200 (stream retry reaches a2)", rec.Code, rec.Body)
	}
	calls, _ := fs.callLog()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want 2 attempts", calls)
	}
	locked := strings.Split(calls[0], ":")[1]
	if !ml.Locked("m1", "p1", locked) {
		t.Fatalf("stream-start 429 on %q must book a model lock", calls[0])
	}
}

func TestDispatchMidStream429BooksLock(t *testing.T) {
	ml := newTestRegistry(newFakeClock())
	fs := &fakeSender{
		fakeAdapter: fakeAdapter{name: "p1", caps: adapter.CapChatCompletion},
		chunks:      []adapter.StreamChunk{{ID: "c1"}},
		streamErr:   &adapter.UpstreamError{Status: 429, Reason: adapter.ReasonRateLimited, Message: "busy"},
	}
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	h.Pool.AddAccount(&pool.Account{ID: "a1", Platform: "p1", Status: "active", Credit: 1000, Health: 1.0})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", (&Router{host: h, lookup: reg, locks: ml}).handleChatCompletions)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (mid-stream failure travels in-band)", rec.Code)
	}
	if !ml.Locked("m1", "p1", "a1") {
		t.Fatal("mid-stream 429 must book a model lock")
	}
}
