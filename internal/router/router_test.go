package router

// /v1 router integration tests: auth (open mode + enforced key), model
// routing (platform prefix + bare-name auto-route), pool orchestration
// (empty pool 503, retry-to-next-account, upstream error passthrough) and
// the no-sender honest 503. All requests go through httptest.NewRequestWithContext
// (noctx is a hard gate in CI).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"all2api/internal/adapter"
	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/pool"
)

// ---------- programmable fakes ----------

// fakeAdapter implements adapter.Adapter with no upstream-send capability.
type fakeAdapter struct {
	name string
	caps adapter.CapType
}

func (f *fakeAdapter) Name() string       { return f.name }
func (f *fakeAdapter) AuthRequired() bool { return true }
func (f *fakeAdapter) ConvertRequest(*adapter.UnifiedRequest) (*adapter.PlatformRequest, error) {
	return nil, nil
}
func (f *fakeAdapter) ParseResponse(*adapter.PlatformResponse) (*adapter.UnifiedResponse, error) {
	return nil, nil
}
func (f *fakeAdapter) HealthCheck(context.Context) (*adapter.HealthStatus, error) {
	return &adapter.HealthStatus{Reason: adapter.ReasonOK}, nil
}
func (f *fakeAdapter) GetTasks(context.Context) ([]*adapter.TaskState, error) {
	return nil, nil
}
func (f *fakeAdapter) ExecuteTask(context.Context, string) error { return nil }
func (f *fakeAdapter) Capabilities() adapter.CapSet {
	return adapter.CapSet{Value: f.caps}
}
func (f *fakeAdapter) GetTaskSchema(context.Context) ([]*adapter.TaskSchema, error) {
	return nil, nil
}
func (f *fakeAdapter) CredentialStatus() (*adapter.CredentialInfo, error) {
	return &adapter.CredentialInfo{Supported: false}, nil
}
func (f *fakeAdapter) ListModels(context.Context) ([]*adapter.ModelInfo, error) {
	return nil, nil
}

// fakeSender layers the adapter.Sender contract on fakeAdapter with knobs:
// canned responses/chunks, fail-first-N failures, persistent send errors and
// per-send call recording ("<platform>:<account>").
type fakeSender struct {
	fakeAdapter

	resp       *adapter.ChatResponse
	chunks     []adapter.StreamChunk
	failFirst  int           // first N send attempts fail with failErr
	failErr    error         // failure for the failFirst attempts (default generic)
	sendErr    error         // persistent failure once failFirst is exhausted
	streamErr  error         // emitted on errCh after all chunks relayed
	holdStream bool          // emit chunk[0], then block until ctx cancels
	consumed   chan struct{} // holdStream: closed once the handler pulled chunk[0]
	cancelSeen chan struct{} // holdStream: closed when the send ctx is observed done

	mu    sync.Mutex
	calls []string
	reqs  []adapter.ChatRequest
}

func (f *fakeSender) record(acct *pool.Account, req adapter.ChatRequest) {
	f.mu.Lock()
	f.calls = append(f.calls, acct.Platform+":"+acct.ID)
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
}

func (f *fakeSender) callLog() ([]string, []adapter.ChatRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := append([]string(nil), f.calls...)
	reqs := append([]adapter.ChatRequest(nil), f.reqs...)
	return calls, reqs
}

// failNow applies the failFirst/sendErr policy; ok=false means the attempt failed.
func (f *fakeSender) failNow() (error, bool) {
	if f.failFirst > 0 {
		f.failFirst--
		e := f.failErr
		if e == nil {
			e = errors.New("fake upstream failure")
		}
		return e, false
	}
	if f.sendErr != nil {
		return f.sendErr, false
	}
	return nil, true
}

func (f *fakeSender) SendChat(_ context.Context, acct *pool.Account, req adapter.ChatRequest) (*adapter.ChatResponse, error) {
	f.record(acct, req)
	if e, ok := f.failNow(); !ok {
		return nil, e
	}
	resp := *f.resp
	return &resp, nil
}

func (f *fakeSender) SendChatStream(ctx context.Context, acct *pool.Account, req adapter.ChatRequest) (<-chan adapter.StreamChunk, <-chan error, error) {
	f.record(acct, req)
	if e, ok := f.failNow(); !ok {
		return nil, nil, e
	}
	chunks := make(chan adapter.StreamChunk)
	errCh := make(chan error, 1)
	go func() {
		if f.holdStream {
			// Unbuffered send completes only once the handler received the
			// chunk — the close(consumed) afterwards is the test sync point.
			chunks <- f.chunks[0]
			close(f.consumed)
			<-ctx.Done()
			close(f.cancelSeen)
			return
		}
		for _, c := range f.chunks {
			select {
			case chunks <- c:
			case <-ctx.Done():
				return
			}
		}
		if f.streamErr != nil {
			// Error path deliberately leaves chunks open: the handler must
			// take the errCh branch, not a chunks-closed [DONE].
			errCh <- f.streamErr
			return
		}
		close(chunks)
	}()
	return chunks, errCh, nil
}

// ---------- helpers ----------

// loadRouterConfig loads a config body and restores open mode on cleanup so
// later tests never inherit an api_key or a responses flag (same pattern as
// internal/api/recover_test.go).
func loadRouterConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	t.Cleanup(func() {
		open := filepath.Join(dir, "open.json")
		_ = os.WriteFile(open, []byte(`{"listen":":0"}`), 0o600)
		_ = config.Load(open)
	})
}

func newTestHost(t *testing.T) *host.Host {
	t.Helper()
	return host.New(config.Config{
		Pool:    config.PoolConfig{CreditFloor: 100, MaxRetries: 3, CooldownSec: 60},
		Sources: map[string]config.SourceConfig{},
	})
}

func addAccount(t *testing.T, h *host.Host, id, platform string) {
	t.Helper()
	h.Pool.AddAccount(&pool.Account{
		ID: id, Platform: platform, Status: "active", Credit: 1000, Health: 1.0,
	})
}

func newRegistry(t *testing.T, ads ...adapter.Adapter) *adapter.Registry {
	t.Helper()
	reg := adapter.NewRegistry()
	for _, a := range ads {
		if err := reg.Register(a); err != nil {
			t.Fatalf("register adapter %q: %v", a.Name(), err)
		}
	}
	return reg
}

func newMux(h *host.Host, lookup AdapterLookup) *http.ServeMux {
	mux := http.NewServeMux()
	Register(mux, h, lookup)
	return mux
}

func postJSON(t *testing.T, mux *http.ServeMux, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func cannedChatResponse(platform string) *adapter.ChatResponse {
	finish := "stop"
	return &adapter.ChatResponse{
		ID:      "chatcmpl-" + platform,
		Object:  "chat.completion",
		Created: 1770000000,
		Model:   "bare-model",
		Choices: []adapter.ChatChoice{{
			Index:        0,
			Message:      adapter.ChatMessage{Role: "assistant", Content: json.RawMessage(`"hello from ` + platform + `"`)},
			FinishReason: &finish,
		}},
		Usage: &adapter.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	}
}

func newSender(platform string) *fakeSender {
	return &fakeSender{
		fakeAdapter: fakeAdapter{name: platform, caps: adapter.CapChatCompletion},
		resp:        cannedChatResponse(platform),
	}
}

// decodeErrEnvelope pulls the OpenAI error object out of a response body.
func decodeErrEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (msg, errType, code string) {
	t.Helper()
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, rec.Body)
	}
	return env.Error.Message, env.Error.Type, env.Error.Code
}

// fmtChatBody keeps the request bodies consistent across tests.
func fmtChatBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
}

// ---------- auth ----------

func TestV1AuthRejectsMissingOrWrongKey(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0","api_key":"k1"}`)
	reg := newRegistry(t, newSender("ra"))
	h := newTestHost(t)
	// Two accounts: each successful request leases its account for the
	// pool's 30s anti-thundering TTL, so back-to-back successes need both.
	addAccount(t, h, "a1", "ra")
	addAccount(t, h, "a2", "ra")
	mux := newMux(h, reg)
	body := `{"model":"ra/m","messages":[{"role":"user","content":"hi"}]}`

	rec := postJSON(t, mux, "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key with api_key configured: got %d, want 401 (body %s)", rec.Code, rec.Body)
	}
	if msg, errType, code := decodeErrEnvelope(t, rec); code != "invalid_api_key" || errType != "invalid_request_error" || msg == "" {
		t.Fatalf("401 envelope: got (%q,%q,%q), want invalid_api_key/invalid_request_error/non-empty", msg, errType, code)
	}

	rec = postJSON(t, mux, "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: got %d, want 401", rec.Code)
	}

	rec = postJSON(t, mux, "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer k1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("correct bearer: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	rec = postJSON(t, mux, "/v1/chat/completions", body, map[string]string{"X-API-Key": "k1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("correct x-api-key: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

func TestV1AuthOpenModeWhenNoKey(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`) // no api_key → open mode
	reg := newRegistry(t, newSender("rop"))
	h := newTestHost(t)
	addAccount(t, h, "a1", "rop")
	mux := newMux(h, reg)

	// No auth headers at all: the request must pass auth — a ghost model
	// proves it reached routing (404), where an enforced key would 401 first.
	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"ghost/m"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("open mode must pass auth: got %d, want 404 from routing (body %s)", rec.Code, rec.Body)
	}
	rec = postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rop/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("open mode full chain: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

func TestV1MethodNotAllowed(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	mux := newMux(newTestHost(t), newRegistry(t))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/chat/completions: got %d, want 405", rec.Code)
	}
}

// ---------- request validation ----------

func TestV1InvalidBodyAndMissingModel(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	mux := newMux(newTestHost(t), newRegistry(t))

	rec := postJSON(t, mux, "/v1/chat/completions", "not-json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("broken JSON: got %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if _, errType, _ := decodeErrEnvelope(t, rec); errType != "invalid_request_error" {
		t.Fatalf("400 envelope type: got %q, want invalid_request_error", errType)
	}

	rec = postJSON(t, mux, "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing model: got %d, want 400 (body %s)", rec.Code, rec.Body)
	}
}

// ---------- model routing ----------

func TestV1PrefixRouting(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	reg := newRegistry(t, newSender("rp1"), newSender("rp2"))
	h := newTestHost(t)
	addAccount(t, h, "a1", "rp1")
	addAccount(t, h, "a2", "rp2")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rp2/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prefix route: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var resp adapter.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != "chatcmpl-rp2" {
		t.Fatalf("must be served by rp2, got %q", resp.ID)
	}
	calls, reqs := reg.Get("rp2").(*fakeSender).callLog()
	if len(calls) != 1 || calls[0] != "rp2:a2" {
		t.Fatalf("rp2 call log: got %v, want [rp2:a2]", calls)
	}
	if len(reqs) != 1 || reqs[0].Model != "m" {
		t.Fatalf("upstream must see the bare model, got %q", reqs[0].Model)
	}
}

func TestV1BareModelAutoRoute(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	first, second := newSender("rb1"), newSender("rb2")
	reg := newRegistry(t, first, second) // registration order decides "first capable"
	h := newTestHost(t)
	addAccount(t, h, "a1", "rb1")
	addAccount(t, h, "a2", "rb2")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("bare model route: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	calls, _ := first.callLog()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "rb1:") {
		t.Fatalf("bare model must route to first capable adapter rb1, got %v", calls)
	}
	if c2, _ := second.callLog(); len(c2) != 0 {
		t.Fatalf("second capable adapter must stay idle, got %v", c2)
	}
}

// A bare model name must not be captured by a registered-but-credential-less
// platform: the first capable adapter (registration order) whose pool is empty
// is skipped in favour of the next capable adapter that actually has accounts
// (regression: qoder registering after workbuddy shadowed it for bare names
// until a qoder PAT was loaded).
func TestV1BareModelSkipsEmptyPoolPlatform(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	empty, stocked := newSender("rbempty"), newSender("rbstocked")
	reg := newRegistry(t, empty, stocked)
	h := newTestHost(t)
	addAccount(t, h, "a2", "rbstocked") // rbstocked has an account, rbempty none
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("bare model route: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if c, _ := empty.callLog(); len(c) != 0 {
		t.Fatalf("credential-less platform must not receive the call, got %v", c)
	}
	calls, _ := stocked.callLog()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "rbstocked:") {
		t.Fatalf("bare model must route to the platform with accounts, got %v", calls)
	}
}

// When every enabled platform's pool is empty the first candidate is used
// anyway, so the dispatch path reports no_available_account (a real "nothing
// loaded" state) instead of 404 model_not_found (which would claim the model
// does not exist).
func TestV1BareModelAllPoolsEmptyKeepsFirst(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	first, second := newSender("rb1"), newSender("rb2")
	reg := newRegistry(t, first, second)
	h := newTestHost(t) // no accounts at all
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("m"), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("all-empty pools: got %d, want 503 no_available_account (body %s)", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != "no_available_account" {
		t.Fatalf("code = %q, want no_available_account", env.Error.Code)
	}
}

func TestV1UnknownModel404(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	// Registry with NO CapChatCompletion adapter: neither prefix nor bare
	// name can resolve.
	reg := newRegistry(t, &fakeAdapter{name: "nocap"})
	mux := newMux(newTestHost(t), reg)

	for _, model := range []string{"ghost/m", "m"} {
		rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody(model), nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("model %q: got %d, want 404 (body %s)", model, rec.Code, rec.Body)
		}
		if msg, errType, code := decodeErrEnvelope(t, rec); code != "model_not_found" || errType != "invalid_request_error" || !strings.Contains(msg, model) {
			t.Fatalf("404 envelope for %q: got (%q,%q,%q)", model, msg, errType, code)
		}
	}
}

// ---------- pool orchestration ----------

func TestV1PoolEmptyOverloaded503(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	reg := newRegistry(t, newSender("rempty"))
	mux := newMux(newTestHost(t), reg) // no accounts added

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rempty/m"), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty pool: got %d, want 503 (body %s)", rec.Code, rec.Body)
	}
	if msg, errType, code := decodeErrEnvelope(t, rec); code != "no_available_account" || errType != "server_error" || !strings.Contains(msg, "rempty") {
		t.Fatalf("503 envelope: got (%q,%q,%q)", msg, errType, code)
	}
}

func TestV1NoSenderRegistered503(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	reg := newRegistry(t, &fakeAdapter{name: "nosend", caps: adapter.CapChatCompletion})
	h := newTestHost(t)
	addAccount(t, h, "a1", "nosend")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("nosend/m"), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("capable adapter without Sender: got %d, want 503 (body %s)", rec.Code, rec.Body)
	}
	if msg, _, code := decodeErrEnvelope(t, rec); code != "no_upstream_sender" || !strings.Contains(msg, "nosend") {
		t.Fatalf("503 envelope: got (%q,%q)", msg, code)
	}
}

// Sequential requests against a single eligible account must both succeed:
// the send-path lease is released when the attempt completes, not after the
// 30s TTL (live regression 2026-10-15 — a real client's second request within
// 30s got no_available_account while the only funded account sat healthy).
func TestV1SequentialCallsSingleAccount(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rseq")
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "only", "rseq")
	mux := newMux(h, reg)

	for i := 1; i <= 2; i++ {
		rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rseq/m"), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: got %d, want 200 (body %s)", i, rec.Code, rec.Body)
		}
	}
	if calls, _ := fs.callLog(); len(calls) != 2 {
		t.Fatalf("sends = %d, want 2 (both calls must reach the single account)", len(calls))
	}
}

func TestV1NonStreamFullChain(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rfull")
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rfull")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rfull/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("full chain: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: got %q, want application/json", ct)
	}
	var resp adapter.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 || resp.Choices[0].Message.Role != "assistant" {
		t.Fatalf("unexpected response shape: %+v", resp)
	}
	// snake_case wire contract (contract-json-tags-v0.5.md §1).
	for _, key := range []string{`"finish_reason"`, `"total_tokens"`, `"prompt_tokens"`} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Fatalf("wire body must carry snake_case key %s: %s", key, rec.Body)
		}
	}
	calls, reqs := fs.callLog()
	if len(calls) != 1 || calls[0] != "rfull:a1" {
		t.Fatalf("send call log: got %v, want [rfull:a1]", calls)
	}
	if len(reqs[0].Messages) != 1 || string(reqs[0].Messages[0].Content) != `"hi"` {
		t.Fatalf("upstream request messages must pass through, got %+v", reqs[0].Messages)
	}
}

func TestV1UpstreamErrorPassthrough(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)

	classified := newSender("rerr")
	classified.sendErr = &adapter.UpstreamError{
		Status: 429, Reason: adapter.ReasonRateLimited, Message: "quota boom",
	}
	reg := newRegistry(t, classified)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rerr")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rerr/m"), nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("classified upstream error: got %d, want 429 (body %s)", rec.Code, rec.Body)
	}
	if msg, errType, code := decodeErrEnvelope(t, rec); msg != "quota boom" || errType != "rate_limit_error" || code != "rate_limited" {
		t.Fatalf("429 envelope: got (%q,%q,%q)", msg, errType, code)
	}

	opaque := newSender("rerr2")
	opaque.sendErr = errors.New("connection reset by peer")
	reg2 := newRegistry(t, opaque)
	h2 := newTestHost(t)
	addAccount(t, h2, "a1", "rerr2")
	mux2 := newMux(h2, reg2)

	rec = postJSON(t, mux2, "/v1/chat/completions", fmtChatBody("rerr2/m"), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("opaque upstream error: got %d, want 502 (body %s)", rec.Code, rec.Body)
	}
	if _, errType, _ := decodeErrEnvelope(t, rec); errType != "server_error" {
		t.Fatalf("502 envelope type: got %q, want server_error", errType)
	}
}

func TestV1RetryAdvancesToNextAccount(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rretry")
	fs.failFirst = 1
	fs.failErr = &adapter.UpstreamError{Status: 500, Reason: adapter.ReasonPlatformDown, Message: "first attempt boom"}
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rretry")
	addAccount(t, h, "a2", "rretry")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rretry/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry to next account: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	calls, _ := fs.callLog()
	if len(calls) != 2 {
		t.Fatalf("both accounts must be tried, got %v", calls)
	}
	// Selection order is weighted-random, but the failed (leased) account must
	// never be re-picked: the two calls cover both accounts exactly once.
	seen := map[string]bool{}
	for _, c := range calls {
		if seen[c] {
			t.Fatalf("account %s was retried on the same lease — pool retry semantics broken: %v", c, calls)
		}
		seen[c] = true
	}
}

// TestModelCatalogGateRejectsUnknownModel pins the 2026-10-21 incident fix:
// workbuddy/DeepSeek-Flash (a qoder catalog name) must die at the gateway as
// model_not_found — never reach the upstream whose "request illegal" reply
// classified as auth_expired and disabled every healthy workbuddy account
// one retry at a time.
func TestModelCatalogGateRejectsUnknownModel(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rcat")
	fs2 := newSender("rcat2")
	reg := newRegistry(t, fs, fs2)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rcat")
	addAccount(t, h, "a2", "rcat2")
	mux := newMux(h, reg)

	previous := DefaultModelLister
	t.Cleanup(func() { DefaultModelLister = previous })
	DefaultModelLister = func(ctx context.Context) []PublicModel {
		return []PublicModel{
			{ID: "deepseek-v4-flash", OwnedBy: "rcat"},
			{ID: "DeepSeek-Flash", OwnedBy: "other"},
		}
	}

	// Known-bad: the name belongs to another platform's catalog → 404, and
	// the sender must never be contacted.
	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rcat/DeepSeek-Flash"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-catalog model: got %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code != "model_not_found" {
		t.Fatalf("error envelope = %s (err %v)", rec.Body.String(), err)
	}
	if calls, _ := fs.callLog(); len(calls) != 0 {
		t.Fatalf("sender must not be contacted for a catalog-rejected model, got %v", calls)
	}

	// Known-good: a listed name flows to the sender as before.
	rec = postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rcat/deepseek-v4-flash"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("listed model: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	// Platform absent from the catalog → cannot judge → pass through
	// (rcat2 has an account but no catalog rows at all).
	rec = postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rcat2/whatever-new"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlisted platform must pass through: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	// No lister wired → the upstream remains the authority.
	DefaultModelLister = nil
	rec = postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rcat/DeepSeek-Flash"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("no lister must pass through: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

// TestRestrictedAccountStopsRotation pins the 11140 red line: when the
// upstream answers with a risk-control account restriction, the dispatch
// loop must abort instead of rotating to the next account — the restriction
// is device-fingerprint-scoped, and rotating is how one restricted account
// becomes a banned pool (the 2026-10-21 workbuddy incident burned all four
// accounts from one request).
func TestRestrictedAccountStopsRotation(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	first := newSender("rstop1")
	second := newSender("rstop2")
	reg := newRegistry(t, first, second)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rstop1")
	addAccount(t, h, "a2", "rstop2")
	mux := newMux(h, reg)

	first.sendErr = &adapter.UpstreamError{Status: http.StatusForbidden,
		Reason: adapter.ReasonAccountRestricted, Message: "workbuddy: request illegal"}

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rstop1/m"), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restricted: got %d, want 403 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "account_restricted") {
		t.Fatalf("error envelope must carry account_restricted, got %s", rec.Body.String())
	}
	if calls, _ := second.callLog(); len(calls) != 0 {
		t.Fatalf("second account must stay untouched after a restriction, got %v", calls)
	}
}

// TestRestrictedAccountStopsStreamStartRotation covers the stream path of
// the 11140 red line: a stream that fails before committing must abort the
// dispatch loop on a risk-control restriction — the second account stays
// untouched and the client sees the honest 403.
func TestRestrictedAccountStopsStreamStartRotation(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	first := newSender("rstr1")
	second := newSender("rstr2")
	reg := newRegistry(t, first, second)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rstr1")
	addAccount(t, h, "a2", "rstr2")
	mux := newMux(h, reg)

	first.sendErr = &adapter.UpstreamError{Status: http.StatusForbidden,
		Reason: adapter.ReasonAccountRestricted, Message: "workbuddy: request illegal"}

	body := `{"model":"rstr1/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := postJSON(t, mux, "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restricted stream start: got %d, want 403 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "account_restricted") {
		t.Fatalf("error envelope must carry account_restricted, got %s", rec.Body.String())
	}
	if calls, _ := second.callLog(); len(calls) != 0 {
		t.Fatalf("second account must stay untouched after a restriction, got %v", calls)
	}
}

// TestModelCatalogGateUsesCache pins the gate's cache discipline: the panel
// catalog build dials upstreams (qoder's model/list has no TTL), so the
// per-request gate must serve from the 60s snapshot — N requests, one build.
func TestModelCatalogGateUsesCache(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rcache")
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rcache")
	mux := newMux(h, reg)

	previous := DefaultModelLister
	t.Cleanup(func() { DefaultModelLister = previous })
	calls := 0
	DefaultModelLister = func(ctx context.Context) []PublicModel {
		calls++
		return []PublicModel{{ID: "deepseek-v4-flash", OwnedBy: "rcache"}}
	}

	for i := 0; i < 3; i++ {
		rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rcache/deepseek-v4-flash"), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200 (body %s)", i, rec.Code, rec.Body)
		}
	}
	if calls != 1 {
		t.Fatalf("catalog built %d times for 3 requests, want exactly 1 (cache must absorb the rest)", calls)
	}
}

// TestModelCatalogGateMatchesNamespacedRow: when another platform's bare row
// shadows this platform's (the catalog de-duplicates bare names first-wins),
// the namespaced row must still satisfy the gate instead of hard-404ing a
// model the platform genuinely serves.
func TestModelCatalogGateMatchesNamespacedRow(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rshadow")
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rshadow")
	mux := newMux(h, reg)

	previous := DefaultModelLister
	t.Cleanup(func() { DefaultModelLister = previous })
	DefaultModelLister = func(ctx context.Context) []PublicModel {
		return []PublicModel{
			{ID: "m", OwnedBy: "other"},           // shadowed the bare row
			{ID: "rshadow/m", OwnedBy: "rshadow"}, // namespaced row survives
		}
	}

	rec := postJSON(t, mux, "/v1/chat/completions", fmtChatBody("rshadow/m"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("namespaced-row model: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}
