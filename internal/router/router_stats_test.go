package router

// M6a observation-plane tests over the dispatch loop: every selected account
// is booked as in flight (+1 at Select) and the decrement pairs with the
// existing UpdateHealth completion points, so in_flight returns to 0 after
// any outcome while success/fail counters and LastSuccess accumulate in the
// pool. All requests go through httptest.NewRequestWithContext (noctx gate).

import (
	"net/http"
	"testing"

	"all2api/internal/adapter"
	"all2api/internal/host"
	"all2api/internal/pool"
)

// statAccount finds one account in the pool snapshot (value copy).
func statAccount(t *testing.T, h *host.Host, platform, id string) pool.Account {
	t.Helper()
	for _, acc := range h.Pool.SnapshotAccounts() {
		if acc.Platform == platform && acc.ID == id {
			return acc
		}
	}
	t.Fatalf("account %s/%s not in pool", platform, id)
	return pool.Account{}
}

func TestDispatchSuccessCountsAndDrainsInFlight(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("stat1")
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "stat1")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("stat1/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	acc := statAccount(t, h, "stat1", "a1")
	if acc.SuccessCount != 1 || acc.FailCount != 0 {
		t.Fatalf("counters: success=%d fail=%d, want 1/0", acc.SuccessCount, acc.FailCount)
	}
	if acc.InFlight != 0 {
		t.Fatalf("in_flight must drain to 0 once the response is written, got %d", acc.InFlight)
	}
	if acc.LastSuccess.IsZero() {
		t.Fatalf("LastSuccess must be stamped by a success")
	}
}

func TestDispatchStreamSuccessCounts(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("stat1s")
	fs.chunks = cannedChunks()
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "stat1s")
	mux := newMux(h, reg)

	body := `{"model":"stat1s/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := postJSON(t, mux, "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream send: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	acc := statAccount(t, h, "stat1s", "a1")
	if acc.SuccessCount != 1 || acc.FailCount != 0 || acc.InFlight != 0 {
		t.Fatalf("stream counters: success=%d fail=%d in_flight=%d, want 1/0/0 (a finished stream is a completed send)",
			acc.SuccessCount, acc.FailCount, acc.InFlight)
	}
	if acc.LastSuccess.IsZero() {
		t.Fatalf("stream success must stamp LastSuccess")
	}
}

func TestDispatchRetryCountsFailAndSuccess(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("stat2")
	fs.failFirst = 1
	fs.failErr = &adapter.UpstreamError{Status: 500, Reason: adapter.ReasonPlatformDown, Message: "boom"}
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "stat2")
	addAccount(t, h, "a2", "stat2")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("stat2/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry send: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	// Weighted-random pick decides which account took the failed attempt:
	// exactly one fail + one success across the pool, both drained.
	a1 := statAccount(t, h, "stat2", "a1")
	a2 := statAccount(t, h, "stat2", "a2")
	if a1.FailCount+a2.FailCount != 1 || a1.SuccessCount+a2.SuccessCount != 1 {
		t.Fatalf("counters: fails=%d successes=%d, want 1/1 (a1 %+v a2 %+v)",
			a1.FailCount+a2.FailCount, a1.SuccessCount+a2.SuccessCount, a1, a2)
	}
	if a1.InFlight != 0 || a2.InFlight != 0 {
		t.Fatalf("in_flight must drain on both accounts: %d/%d", a1.InFlight, a2.InFlight)
	}
	failed, succeeded := a1, a2
	if a1.FailCount == 0 {
		failed, succeeded = a2, a1
	}
	if !failed.LastSuccess.IsZero() {
		t.Fatalf("failed account must not gain LastSuccess")
	}
	if succeeded.LastSuccess.IsZero() {
		t.Fatalf("successful account must stamp LastSuccess")
	}
}
