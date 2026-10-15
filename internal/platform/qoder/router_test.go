//go:build qoder || full || (!qoder && !trae && !workbuddy)

package qoder_test

// Router integration: with a qoder PAT account in the pool and the real
// sender wired to a local fake upstream, /v1/chat/completions must reach the
// upstream both via the "qoder/model" prefix and via the bare model name
// (the registry's only chat-capable adapter in this test binary). The fake is
// a minimal local stub — no real endpoint is contacted (task red line).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/platform/qoder"
	"all2api/internal/pool"
	"all2api/internal/router"
)

const fakePAT = "pt-" + "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// stubUpstream answers the exchange chain and one enveloped SSE chat stream.
type stubUpstream struct {
	mu    sync.Mutex
	calls int
}

// chatCalls snapshots the upstream chat counter.
func (u *stubUpstream) chatCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *stubUpstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobToken/exchange", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"jt-fake","refresh_token":"jrt-fake","expires_in":86400}`)
	})
	mux.HandleFunc("/api/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"user-1","name":"tester"}`)
	})
	mux.HandleFunc("/algo/api/v3/user/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"userType":"personal_standard"}`)
	})
	mux.HandleFunc("/algo/api/v2/service/pro/sse/agent_chat_generation", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls++
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		inner := func(body string) string {
			return "data: " + mustJSON(map[string]any{"body": body, "statusCode": "OK", "statusCodeValue": 200}) + "\n\n"
		}
		fmt.Fprint(w,
			inner(`{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)+
				inner(`{"choices":[{"index":0,"delta":{"content":"routed"},"finish_reason":null}]}`)+
				inner(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+
				inner("[DONE]"))
	})
	return mux
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRoutedThroughGatewayBareNameAndPrefix(t *testing.T) {
	u := &stubUpstream{}
	srv := httptest.NewServer(u.handler())
	t.Cleanup(srv.Close)

	a, ok := adapter.Default.Get("qoder").(*qoder.Adapter)
	if !ok {
		t.Fatal("qoder adapter not registered in this test binary")
	}
	a.Configure(qoder.ClientOptions{
		AuthBaseURL:    srv.URL,
		OpenAPIBaseURL: srv.URL,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		Credentials:    qoderCredentialSource{},
	})
	t.Cleanup(func() { a.Configure(qoder.ClientOptions{}) }) // production-neutral restore

	cfg := config.Defaults()
	cfg.Pool.CreditFloor = 0
	// In the default/full variants this test binary also carries the
	// workbuddy adapter, and bare-name routing follows registry registration
	// order. Disable the workbuddy source (the 一键摘源 switch) so the bare
	// model name fails over to the enabled qoder adapter — the documented
	// routing semantics under test.
	cfg.Sources["workbuddy"] = config.SourceConfig{Enabled: false}
	// The router consults the process-wide config (hot-read per request), so
	// the disable switch must land there, not only in the host's copy.
	prevCfg := config.Shared.Get()
	config.Shared.Update(cfg)
	t.Cleanup(func() { config.Shared.Update(prevCfg) })
	gwHost := host.New(cfg)
	// Two accounts: the pool's anti-thundering lease holds each selected
	// account for its TTL, so the second routed request needs a second
	// candidate.
	gwHost.Pool.AddAccount(&pool.Account{ID: "fp-1", Platform: "qoder", Status: "active", Credit: 10})
	gwHost.Pool.AddAccount(&pool.Account{ID: "fp-2", Platform: "qoder", Status: "active", Credit: 10})

	mux := http.NewServeMux()
	router.Register(mux, gwHost, adapter.Default)
	apiSrv := httptest.NewServer(mux)
	t.Cleanup(apiSrv.Close)

	post := func(model string) (int, string) {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, apiSrv.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 8192)
		n, _ := resp.Body.Read(buf)
		return resp.StatusCode, string(buf[:n])
	}

	for _, model := range []string{"qoder/GLM-5.3", "GLM-5.3"} {
		status, body := post(model)
		if status != http.StatusOK || !strings.Contains(body, "routed") {
			t.Errorf("model %q: status %d body %s, want 200 with the upstream content", model, status, body)
		}
	}
	if u.chatCalls() != 2 {
		t.Errorf("upstream chat calls = %d, want one per routed request", u.chatCalls())
	}
}

// qoderCredentialSource stands in for the host credential store (the host
// package cannot be imported here without a test-only cycle).
type qoderCredentialSource struct{}

func (qoderCredentialSource) CredentialFor(_ context.Context, acct *pool.Account) (*qoder.PATCredential, error) {
	if acct == nil {
		return nil, errors.New("no account")
	}
	return &qoder.PATCredential{PAT: fakePAT}, nil
}
