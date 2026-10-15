package router

// M8 request-ledger tests: exactly one entry per client request lands in the
// process ledger, whatever the outcome — success (non-stream + stream),
// upstream error, stream_error (committed 200 + in-band error frame),
// interrupted (client cancel mid-dispatch and mid-stream), pool exhaustion
// and unroutable models. All requests go through httptest.NewRequestWithContext.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/usage"
)

// freshLedger swaps in an empty ledger for this test (dispatches from earlier
// tests in the package must not leak into these assertions).
func freshLedger(t *testing.T) {
	t.Helper()
	usage.Default = usage.New()
}

// lastEntry returns the single entry the dispatch under test recorded.
func lastEntry(t *testing.T) usage.RequestEntry {
	t.Helper()
	snap := usage.Default.Snapshot()
	if len(snap) == 0 {
		t.Fatal("dispatch recorded no ledger entry")
	}
	return snap[len(snap)-1]
}

func ledgerCount() int { return len(usage.Default.Snapshot()) }

func TestLedgerNonStreamSuccess(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	reg := newRegistry(t, newSender("m8wb"))
	h := newTestHost(t)
	addAccount(t, h, "0123456789abcdef", "m8wb")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("m8wb/deepseek-v4-flash"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	if ledgerCount() != 1 {
		t.Fatalf("one request must be exactly one entry, got %d", ledgerCount())
	}
	e := lastEntry(t)
	if e.Result != usage.ResultSuccess {
		t.Fatalf("result = %q, want success", e.Result)
	}
	if e.Model != "deepseek-v4-flash" {
		t.Fatalf("model = %q, want the bare name", e.Model)
	}
	if e.Platform != "m8wb" {
		t.Fatalf("platform = %q, want m8wb", e.Platform)
	}
	if e.AccountID != "01234567" {
		t.Fatalf("account_id = %q, want the 8-char short code", e.AccountID)
	}
	if e.PromptTokens != 3 || e.CompletionTokens != 4 {
		t.Fatalf("tokens = %d/%d, want 3/4 from the upstream usage block", e.PromptTokens, e.CompletionTokens)
	}
	if e.RequestID != "chatcmpl-m8wb" {
		t.Fatalf("request_id = %q, want chatcmpl-m8wb", e.RequestID)
	}
	if e.Latency < 0 {
		t.Fatalf("latency must never be negative, got %v", e.Latency)
	}
	if e.TS.After(time.Now()) {
		t.Fatalf("ts must be the dispatch start, got %v", e.TS)
	}
}

func TestLedgerUpstreamErrorRow(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("m8err")
	fs.sendErr = &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "upstream exploded"}
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "m8err")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("m8err/m"), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 passthrough", rec.Code)
	}
	e := lastEntry(t)
	if e.Result != usage.ResultError || e.Platform != "m8err" || e.AccountID != "a1" || e.Model != "m" {
		t.Fatalf("error row drifted: %+v", e)
	}
	if e.PromptTokens != -1 || e.CompletionTokens != -1 || e.RequestID != "" {
		t.Fatalf("failed send has no tokens/request id: %+v", e)
	}
}

func TestLedgerPoolExhaustedRow(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	reg := newRegistry(t, newSender("m8empty"))
	h := newTestHost(t) // no accounts pooled
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("m8empty/m"), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	e := lastEntry(t)
	if e.Result != usage.ResultError || e.Platform != "m8empty" || e.AccountID != "" {
		t.Fatalf("pool-exhausted row drifted: %+v", e)
	}
}

func TestLedgerUnroutableModelRow(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	reg := newRegistry(t, newSender("m8x"))
	h := newTestHost(t)
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("nowhere/m8lost"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	e := lastEntry(t)
	if e.Result != usage.ResultError {
		t.Fatalf("result = %q, want error", e.Result)
	}
	// The routing prefix never resolved: bare model kept, platform stays empty.
	if e.Model != "m8lost" || e.Platform != "" || e.AccountID != "" {
		t.Fatalf("unroutable row drifted: %+v", e)
	}
}

func TestLedgerStreamSuccessRow(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("m8s")
	fs.chunks = cannedChunks()
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "m8s")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"m8s/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("stream did not complete: %d %s", rec.Code, rec.Body)
	}
	e := lastEntry(t)
	if e.Result != usage.ResultSuccess {
		t.Fatalf("result = %q, want success", e.Result)
	}
	if e.RequestID != "cc" {
		t.Fatalf("request_id = %q, want cc from the chunk frames", e.RequestID)
	}
	// StreamChunk carries no usage block today: tokens stay unknown (wire null).
	if e.PromptTokens != -1 || e.CompletionTokens != -1 {
		t.Fatalf("stream tokens must stay unknown, got %d/%d", e.PromptTokens, e.CompletionTokens)
	}
}

func TestLedgerStreamMidStreamErrorRow(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("m8se")
	fs.chunks = cannedChunks()
	fs.streamErr = &adapter.UpstreamError{Status: http.StatusTooManyRequests, Reason: adapter.ReasonRateLimited, Message: "slow down"}
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "m8se")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"m8se/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("committed stream keeps status 200, got %d", rec.Code)
	}
	e := lastEntry(t)
	if e.Result != usage.ResultStreamError {
		t.Fatalf("result = %q, want stream_error", e.Result)
	}
	if e.RequestID != "cc" || e.AccountID != "a1" {
		t.Fatalf("stream_error row drifted: %+v", e)
	}
}

func TestLedgerStreamStartFailureRetriesIntoOneEntry(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("m8f")
	fs.failFirst = 1 // stream-start failure → retry on the next account
	fs.chunks = cannedChunks()
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "aaaa0000", "m8f")
	addAccount(t, h, "bbbb1111", "m8f")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"m8f/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("retry stream did not complete: %d %s", rec.Code, rec.Body)
	}
	if ledgerCount() != 1 {
		t.Fatalf("retries fold into one entry, got %d", ledgerCount())
	}
	calls, _ := fs.callLog()
	if len(calls) != 2 {
		t.Fatalf("two stream attempts expected, got %v", calls)
	}
	lastAcct := strings.TrimPrefix(calls[1], "m8f:")
	e := lastEntry(t)
	if e.Result != usage.ResultSuccess || e.AccountID != lastAcct {
		t.Fatalf("entry must describe the last (successful) attempt: entry=%+v calls=%v", e, calls)
	}
	if e.AccountID == strings.TrimPrefix(calls[0], "m8f:") {
		t.Fatalf("retry must advance to a different account: %v", calls)
	}
}

func TestLedgerClientCancelMidStreamInterrupted(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rgone")
	fs.chunks = cannedChunks()[:1]
	fs.holdStream = true
	fs.consumed = make(chan struct{})
	fs.cancelSeen = make(chan struct{})
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rgone")
	mux := newMux(h, reg)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"rgone/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-fs.consumed:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never pulled the first chunk")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler must return once the client context cancels")
	}

	e := lastEntry(t)
	if e.Result != usage.ResultInterrupted {
		t.Fatalf("result = %q, want interrupted (client went away)", e.Result)
	}
	if e.RequestID != "cc" {
		t.Fatalf("request_id = %q, want the id observed before the cancel", e.RequestID)
	}
}

func TestLedgerClientGoneBeforeAttemptInterrupted(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("m8gone")
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "m8gone")
	mux := newMux(h, reg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client is already gone when dispatch starts
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m8gone/m","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	e := lastEntry(t)
	if e.Result != usage.ResultInterrupted {
		t.Fatalf("result = %q, want interrupted", e.Result)
	}
	if e.Platform != "m8gone" || e.AccountID != "" {
		t.Fatalf("pre-attempt interruption has platform but no account: %+v", e)
	}
}

func TestLedgerAggregateFeedsFromDispatch(t *testing.T) {
	freshLedger(t)
	loadRouterConfig(t, `{"listen":":0"}`)
	reg := newRegistry(t, newSender("m8agg"))
	h := newTestHost(t)
	addAccount(t, h, "a1", "m8agg")
	mux := newMux(h, reg)

	_ = postJSON(t, mux, "/v1/chat/completions", fmtChatBody("m8agg/m"), nil)
	_ = postJSON(t, mux, "/v1/chat/completions", fmtChatBody("m8agg/m"), nil)

	agg := usage.Default.Aggregate()
	if agg.Window != usage.WindowProcess || agg.Requests != 2 || agg.Fail != 0 {
		t.Fatalf("aggregate after two successes = %+v", agg)
	}
	bucket := agg.ByPlatform["m8agg"]
	if bucket.Requests != 2 || bucket.Fail != 0 {
		t.Fatalf("by_platform[m8agg] = %+v, want {2 0}", bucket)
	}
	if got := agg.ByModel["m"]; got.Requests != 2 {
		t.Fatalf("by_model[m] = %+v, want requests=2", got)
	}
	if agg.PromptTokens != 6 || agg.CompletionTokens != 8 {
		t.Fatalf("token sums = %d/%d, want 6/8", agg.PromptTokens, agg.CompletionTokens)
	}
}
