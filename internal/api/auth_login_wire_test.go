//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// auth_login_wire_test.go runs the full three-step chain end-to-end over the
// REAL workbuddy login client: a host credential store loaded from a
// t.TempDir auth dir (never a real credential file — red line), a
// workbuddy.LoginClient pointed at a local httptest stub (never the real
// upstream — red line), and the two handlers mounted the way registerLogin
// mounts them (the registration/guard paths are pinned in auth_login_test.go,
// which is build-independent).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"all2api/internal/host"
	"all2api/internal/platform/workbuddy"
)

// newLoginWireMux mounts the production handler pair over a login client
// whose endpoint base is the stub server. The stub script: state issues
// st-e2e, the token endpoint answers the documented 11217 "login ing" marker
// once and then the token bundle, the account endpoint completes the
// identity. Anything else is a test failure (no stray upstream calls).
func newLoginWireMux(t *testing.T) (mux *http.ServeMux, h *host.Host, dir string, tokenCalls *atomic.Int64) {
	t.Helper()
	resetLoginState(t)
	loadM5Config(t, `{"listen":":0"}`)
	h = newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	dir = t.TempDir()
	if _, err := h.Creds.LoadAuthDir(dir); err != nil {
		t.Fatalf("LoadAuthDir: %v", err)
	}

	tokenCalls = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/plugin/auth/state" && r.URL.RawQuery == "platform=CLI":
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"state":"st-e2e","authUrl":"https://auth.example/wb?state=st-e2e"}}`))
		case r.URL.Path == "/v2/plugin/auth/token" && r.URL.RawQuery == "state=st-e2e":
			if tokenCalls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"code":11217,"msg":"login ing","data":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"at-e2e","refreshToken":"rt-e2e","expiresIn":3600,"domain":"e2e.example"}}`))
		case r.URL.Path == "/v2/plugin/login/account" && r.URL.RawQuery == "state=st-e2e":
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"uid":"uid-e2e","enterpriseId":"ent-e2e","nickname":"端到端号"}}`))
		default:
			t.Errorf("unexpected upstream call %s %s", r.URL.Path, r.URL.RawQuery)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found","data":null}`))
		}
	}))
	t.Cleanup(srv.Close)

	backend := newWBLoginBackend(workbuddy.LoginClientOptions{BaseURL: srv.URL})
	mux = http.NewServeMux()
	mux.HandleFunc("POST /api/accounts/workbuddy/login-start", withAPIKey(handleLoginStart(backend)))
	mux.HandleFunc("GET /api/accounts/workbuddy/login-poll", withAPIKey(handleLoginPoll(backend, h)))
	return mux, h, dir, tokenCalls
}

func loginStartReq(t *testing.T, mux *http.ServeMux) (state, loginURL string) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/workbuddy/login-start", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login-start: got %d (body %s)", rec.Code, rec.Body)
	}
	var out struct {
		State    string `json:"state"`
		LoginURL string `json:"login_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.State == "" || out.LoginURL == "" {
		t.Fatalf("login-start body = %s (err %v)", rec.Body, err)
	}
	return out.State, out.LoginURL
}

func loginPollReq(t *testing.T, mux *http.ServeMux, state string) map[string]any {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/accounts/workbuddy/login-poll?state="+state, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login-poll: got %d (body %s)", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("login-poll body: %v (%s)", err, rec.Body)
	}
	return out
}

func TestLoginWireFullChain(t *testing.T) {
	mux, h, dir, tokenCalls := newLoginWireMux(t)

	// Step 1: login-start hands out the state + authorization URL.
	state, loginURL := loginStartReq(t, mux)
	if state != "st-e2e" || !strings.Contains(loginURL, "auth.example") {
		t.Fatalf("start = %q / %q", state, loginURL)
	}

	// Step 2: first poll rides the 11217 branch → pending.
	if out := loginPollReq(t, mux, state); out["status"] != "pending" {
		t.Fatalf("poll 1 = %v, want pending", out)
	}
	// Step 3: second poll completes — done + uid + nickname, persisted.
	out := loginPollReq(t, mux, state)
	if out["status"] != "done" || out["uid"] != "uid-e2e" || out["nickname"] != "端到端号" {
		t.Fatalf("poll 2 = %v", out)
	}
	// The credential went through AddWorkbuddyCredential: auth file with the
	// account/auth blocks (realm key included), nickname in the store, pool
	// registration visible in the snapshot.
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-uid-e2e.json"))
	if err != nil {
		t.Fatalf("auth file: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("auth file parse: %v", err)
	}
	auth, _ := doc["auth"].(map[string]any)
	account, _ := doc["account"].(map[string]any)
	if auth == nil || account == nil {
		t.Fatalf("auth file shape = %s", raw)
	}
	if auth["accessToken"] != "at-e2e" || auth["refreshToken"] != "rt-e2e" || auth["realm"] != "cn" {
		t.Fatalf("auth block = %v", auth)
	}
	if account["uid"] != "uid-e2e" || account["nickname"] != "端到端号" {
		t.Fatalf("account block = %v", account)
	}
	if got := h.Creds.NicknameFor("workbuddy", "uid-e2e"); got != "端到端号" {
		t.Fatalf("store nickname = %q", got)
	}
	registered := false
	for _, a := range h.Pool.SnapshotAccounts() {
		if a.Platform == "workbuddy" && a.ID == "uid-e2e" && a.Status == "active" {
			registered = true
		}
	}
	if !registered {
		t.Fatal("account not registered in pool after done")
	}
	// Consumed: a third poll expires without touching the upstream again.
	callsAfterDone := tokenCalls.Load()
	if out := loginPollReq(t, mux, state); out["status"] != "expired" {
		t.Fatalf("poll 3 = %v, want expired", out)
	}
	if tokenCalls.Load() != callsAfterDone {
		t.Fatal("expired poll was forwarded upstream")
	}
}

func TestLoginWireTTLNeverForwarded(t *testing.T) {
	mux, _, _, tokenCalls := newLoginWireMux(t)

	state, _ := loginStartReq(t, mux)

	// Re-store the session under a stubbed clock, then age it past the TTL:
	// the poll must answer expired and never contact the upstream token
	// endpoint for the dead state (the TTL is ours, the upstream never
	// learns of it).
	now := time.Unix(1800000000, 0)
	loginNow = func() time.Time { return now }
	loginSessionStore.put(state)
	now = now.Add(loginSessionTTL + time.Minute)

	if out := loginPollReq(t, mux, state); out["status"] != "expired" {
		t.Fatalf("poll = %v, want expired", out)
	}
	if tokenCalls.Load() != 0 {
		t.Fatal("TTL-expired state was forwarded upstream")
	}
}
