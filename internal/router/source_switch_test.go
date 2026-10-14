package router

// /v1 consumption of the per-source control plane (§9.21-e.2 closure):
// sources.<id>.enabled=false removes a platform from /v1 (prefix addressing
// answers model_not_found; bare-name auto-routing fails over to the next
// enabled platform), and host.Breaker gates dispatch (open → 503) and is fed
// by send outcomes (429/5xx/0 count as failures, 2xx closes a half-open
// probe) — thresholds live solely in the circuit package.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/circuit"
	"all2api/internal/config"
	"all2api/internal/host"
)

// hostWithSources builds a Host whose breakers mirror the config sources the
// way host.New derives them in production (one breaker per source stanza).
func hostWithSources(t *testing.T, sources map[string]config.SourceConfig) *host.Host {
	t.Helper()
	return host.New(config.Config{
		Pool:    config.PoolConfig{CreditFloor: 100, MaxRetries: 3, CooldownSec: 60},
		Sources: sources,
	})
}

// tripWith429s drives the breaker into StateOpen through its own public
// Record semantics (exponential-backoff cooldown on 429) — no test-only
// back doors into circuit state.
func tripWith429s(t *testing.T, br *circuit.Breaker) {
	t.Helper()
	for i := 0; i < 2; i++ {
		br.Record(http.StatusTooManyRequests, "acc")
	}
	if !br.IsTripped() {
		t.Fatalf("breaker must be open after two 429 records (state %v)", br.State())
	}
}

// ---------- sources.<id>.enabled=false ----------

func TestV1DisabledPlatformPrefixIs404(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0","sources":{"rgone-s":{"enabled":false}}}`)
	fs := newSender("rgone-s")
	reg := newRegistry(t, fs)
	h := hostWithSources(t, config.Shared.Get().Sources)
	addAccount(t, h, "a1", "rgone-s")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rgone-s/m"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled platform via prefix: got %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	if msg, errType, code := decodeErrEnvelope(t, rec); code != "model_not_found" || errType != "invalid_request_error" || !strings.Contains(msg, "rgone-s/m") {
		t.Fatalf("404 envelope: got (%q,%q,%q), want model_not_found/invalid_request_error mentioning the model", msg, errType, code)
	}
	if calls, _ := fs.callLog(); len(calls) != 0 {
		t.Fatalf("disabled platform must never be sent to, got %v", calls)
	}
}

func TestV1DisabledPlatformBareNameFailsOver(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0","sources":{"rdis":{"enabled":false},"rdis2":{"enabled":true}}}`)
	disabled, enabled := newSender("rdis"), newSender("rdis2")
	reg := newRegistry(t, disabled, enabled) // registration order: disabled first
	h := hostWithSources(t, config.Shared.Get().Sources)
	addAccount(t, h, "a1", "rdis")
	addAccount(t, h, "a2", "rdis2")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("bare model over a disabled first platform: got %d, want 200 via failover (body %s)", rec.Code, rec.Body)
	}
	if calls, _ := disabled.callLog(); len(calls) != 0 {
		t.Fatalf("disabled platform must stay idle, got %v", calls)
	}
	if calls, _ := enabled.callLog(); len(calls) != 1 || !strings.HasPrefix(calls[0], "rdis2:") {
		t.Fatalf("traffic must fail over to the enabled platform rdis2, got %v", calls)
	}

	// All capable platforms disabled: bare-name routing has nowhere to land —
	// same model_not_found envelope as an unknown model.
	loadRouterConfig(t, `{"listen":":0","sources":{"rdis":{"enabled":false},"rdis2":{"enabled":false}}}`)
	h2 := hostWithSources(t, config.Shared.Get().Sources)
	addAccount(t, h2, "a1", "rdis")
	addAccount(t, h2, "a2", "rdis2")
	mux2 := newMux(h2, reg)
	rec = postJSON(t, mux2, "/v1/chat/completions", fmtChatBody("m"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("all platforms disabled: got %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	if _, errType, code := decodeErrEnvelope(t, rec); code != "model_not_found" || errType != "invalid_request_error" {
		t.Fatalf("all-disabled 404 envelope: got type %q code %q", errType, code)
	}
}

func TestV1MissingSourceStanzaStaysRoutable(t *testing.T) {
	// A platform without a sources.<id> stanza is not under source management:
	// only the explicit enabled:false switch (一键摘源) removes it from /v1.
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rstanza")
	reg := newRegistry(t, fs)
	h := newTestHost(t) // empty sources map → no stanza for rstanza
	addAccount(t, h, "a1", "rstanza")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rstanza/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform without source stanza must stay routable: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

func TestV1DisabledResponsesRouteRejectsToo(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0","router":{"responses_api":{"enabled":true}},"sources":{"rdisr":{"enabled":false}}}`)
	reg := newRegistry(t, newSender("rdisr"))
	h := hostWithSources(t, config.Shared.Get().Sources)
	addAccount(t, h, "a1", "rdisr")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/responses", `{"model":"rdisr/m","input":"ping"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("responses against disabled platform: got %d, want 404 (body %s)", rec.Code, rec.Body)
	}
}

// ---------- host.Breaker consumption ----------

func TestV1BreakerOpenBlocksDispatch(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0","sources":{"rbrk":{"enabled":true}}}`)
	fs := newSender("rbrk")
	reg := newRegistry(t, fs)
	h := hostWithSources(t, config.Shared.Get().Sources)
	addAccount(t, h, "a1", "rbrk")
	br, ok := h.Breaker("rbrk")
	if !ok {
		t.Fatal("host must expose a breaker for a configured source")
	}
	tripWith429s(t, br)
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rbrk/m"), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("open breaker: got %d, want 503 (body %s)", rec.Code, rec.Body)
	}
	if msg, errType, code := decodeErrEnvelope(t, rec); code != "platform_breaker_open" || errType != "server_error" || !strings.Contains(msg, "rbrk") {
		t.Fatalf("503 envelope: got (%q,%q,%q), want platform_breaker_open/server_error naming the platform", msg, errType, code)
	}
	if calls, _ := fs.callLog(); len(calls) != 0 {
		t.Fatalf("open breaker must short-circuit before any send, got %v", calls)
	}

	// No breaker entry (platform outside the sources map): pass through — the
	// pool stays the only guard, requests are not blocked by an absent stanza.
	hFree := newTestHost(t) // empty sources map → no breaker for rbrk
	addAccount(t, hFree, "a1", "rbrk")
	mux2 := newMux(hFree, reg)
	rec = postJSON(t, mux2, "/v1/chat/completions", fmtChatBody("rbrk/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform without breaker stanza must pass the gate: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

func TestV1SendOutcomesFeedBreakerTripAndReset(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0","sources":{"rfeed":{"enabled":true}}}`)
	fs := newSender("rfeed")
	reg := newRegistry(t, fs)
	h := hostWithSources(t, config.Shared.Get().Sources)
	addAccount(t, h, "a1", "rfeed")
	addAccount(t, h, "a2", "rfeed")
	br, _ := h.Breaker("rfeed")
	mux := newMux(h, reg)

	// One 429 already opens the breaker (the rate-limit cooldown path fires on
	// the first hit — InitialBackoffSec exponential backoff), and the very
	// next attempt inside the same request is then gate-blocked: the loop
	// checks the breaker per attempt, so one caller cannot hammer a platform
	// that just broke mid-request.
	fs.failFirst = 1
	fs.failErr = &adapter.UpstreamError{Status: 429, Reason: adapter.ReasonRateLimited, Message: "slow down"}
	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rfeed/m"), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("retry past mid-request trip: got %d, want 503 platform_breaker_open (body %s)", rec.Code, rec.Body)
	}
	if _, _, code := decodeErrEnvelope(t, rec); code != "platform_breaker_open" {
		t.Fatalf("mid-request gate envelope code: got %q, want platform_breaker_open", code)
	}
	if !br.IsTripped() {
		t.Fatalf("one recorded 429 must trip the breaker (state %v)", br.State())
	}
	if reason := br.Reason(); reason != circuit.ReasonRateLimited {
		t.Fatalf("trip reason: got %q, want rate_limited", reason)
	}
	if calls, _ := fs.callLog(); len(calls) != 1 {
		t.Fatalf("exactly the one 429 attempt must have reached the sender, got %v", calls)
	}

	// Opaque transport errors count as failures too (statusCode 0 bucket):
	// five in a row cross the consecutive-failure threshold and hard-trip.
	// Five distinct accounts keep the pool's anti-thundering leases from
	// starving the retry loop — each failure must really reach the breaker.
	loadRouterConfig(t, `{"listen":":0","sources":{"rfeed2":{"enabled":true}}}`)
	fs2 := newSender("rfeed2")
	fs2.sendErr = errors.New("connection reset by peer")
	reg2 := newRegistry(t, fs2)
	h2 := hostWithSources(t, config.Shared.Get().Sources)
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		addAccount(t, h2, id, "rfeed2")
	}
	br2, _ := h2.Breaker("rfeed2")
	mux2 := newMux(h2, reg2)

	rec = postJSON(t, mux2, "/v1/chat/completions", fmtChatBody("rfeed2/m"), nil)
	// Wire status: once the fifth failure hard-trips the breaker, the next
	// loop iteration's gate refuses — the request ends 503 platform_breaker_open
	// rather than the last attempt's 502 passthrough. The trip and the five
	// burned attempts are what this section actually asserts.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("opaque transport failures to consecutive trip: got %d, want 503 platform_breaker_open (body %s)", rec.Code, rec.Body)
	}
	if _, _, code := decodeErrEnvelope(t, rec); code != "platform_breaker_open" {
		t.Fatalf("post-trip envelope code: got %q, want platform_breaker_open", code)
	}
	if !br2.IsTripped() || br2.Reason() != circuit.ReasonConsecutive {
		t.Fatalf("5 consecutive transport failures must hard-trip consecutive, got state %v reason %q", br2.State(), br2.Reason())
	}
	if calls, _ := fs2.callLog(); len(calls) != 5 {
		t.Fatalf("retry loop must have burned five accounts on the consecutive trip, got %v", calls)
	}

	// A recorded success closes a half-open probe back to closed: after the
	// hard trip, Reset (the panel recover path) re-opens the gate, a success
	// lands, and the next request flows with the breaker closed.
	br2.Reset()
	if br2.IsTripped() {
		t.Fatal("manual Reset must close the breaker")
	}
	fs2.sendErr = nil
	rec = postJSON(t, mux2, "/v1/chat/completions", fmtChatBody("rfeed2/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-reset recovery request: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if st := br2.State(); st != circuit.StateClosed {
		t.Fatalf("success must leave the breaker closed, got %v", st)
	}
}

func TestV1ClientCancelDoesNotFeedBreaker(t *testing.T) {
	// A cancelled client context is not an upstream verdict: the dispatch loop
	// bails before recording anything, so the breaker stays untouched.
	loadRouterConfig(t, `{"listen":":0","sources":{"rcxl":{"enabled":true}}}`)
	fs := newSender("rcxl")
	fs.chunks = cannedChunks()
	fs.holdStream = true
	fs.consumed = make(chan struct{})
	fs.cancelSeen = make(chan struct{})
	reg := newRegistry(t, fs)
	h := hostWithSources(t, config.Shared.Get().Sources)
	addAccount(t, h, "a1", "rcxl")
	br, _ := h.Breaker("rcxl")
	mux := newMux(h, reg)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"rcxl/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(handlerDone)
	}()

	select {
	case <-fs.consumed:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never pulled the first chunk")
	}
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler must return once the client context cancels")
	}

	if st := br.State(); st != circuit.StateClosed {
		t.Fatalf("client cancellation must not move the breaker, got state %v", st)
	}
}

func TestV1StreamOutcomesFeedBreaker(t *testing.T) {
	// Mid-stream failure keeps the committed 200 on the wire but must still
	// feed the breaker with the upstream classification (500 → failure count).
	loadRouterConfig(t, `{"listen":":0","sources":{"rstrbrk":{"enabled":true}}}`)
	fs := newSender("rstrbrk")
	fs.chunks = cannedChunks()
	fs.streamErr = &adapter.UpstreamError{Status: 500, Reason: adapter.ReasonPlatformDown, Message: "died mid-stream"}
	reg := newRegistry(t, fs)
	h := hostWithSources(t, config.Shared.Get().Sources)
	addAccount(t, h, "a1", "rstrbrk")
	br, _ := h.Breaker("rstrbrk")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"rstrbrk/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mid-stream failure keeps committed 200: got %d (body %s)", rec.Code, rec.Body)
	}
	// The 500 was recorded (failure counter = 1, under the trip threshold):
	// observable by finishing the trip with the remaining four failures via
	// the breaker's own consecutive-failure path.
	for i := 0; i < 4; i++ {
		br.Record(http.StatusInternalServerError, "acc")
	}
	if !br.IsTripped() || br.Reason() != circuit.ReasonConsecutive {
		t.Fatalf("mid-stream 500 plus four more failures must trip consecutive, got state %v reason %q", br.State(), br.Reason())
	}
}
