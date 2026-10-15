package api

// auth_login_test.go pins the M8 browser-login endpoint state machine with a
// fake backend (the loginBackend seam is tag-neutral, so this suite runs in
// every build variant): the frozen wire shapes, pending→done progression,
// expired on unknown/TTL-expired/consumed states, error with the session
// kept, persistence through AddWorkbuddyCredential, the 404-without-store
// guard and the withAPIKey guard. Credential material only ever lands in
// t.TempDir auth files (red line: the real auth dir is never touched).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"all2api/internal/host"
	"all2api/internal/observability"
	"all2api/internal/schedule"
)

// fakeLoginBackend serves canned poll results; the tests assert the handler
// state machine around them, not the upstream (that is the wire test's job).
type fakeLoginBackend struct {
	startState string
	startURL   string
	startErr   error
	polls      []loginPollResult // consumed front-first, one per Poll
	pollStates []string
}

func (f *fakeLoginBackend) Start(context.Context) (string, string, error) {
	if f.startErr != nil {
		return "", "", f.startErr
	}
	return f.startState, f.startURL, nil
}

func (f *fakeLoginBackend) Poll(_ context.Context, state string) loginPollResult {
	f.pollStates = append(f.pollStates, state)
	if len(f.polls) == 0 {
		return loginPollResult{Status: "pending"}
	}
	res := f.polls[0]
	f.polls = f.polls[1:]
	return res
}

// resetLoginState gives every test a clean session store and clock.
func resetLoginState(t *testing.T) {
	t.Helper()
	clean := func() {
		loginSessionStore.mu.Lock()
		loginSessionStore.sessions = map[string]loginSession{}
		loginSessionStore.mu.Unlock()
		loginNow = time.Now
	}
	clean()
	t.Cleanup(clean)
}

// newLoginTestMux wires the two login handlers over the given backend with a
// temp auth dir loaded (unless withDir is false — the uninitialized-store
// branch). Mirrors newAddAccountMux's host wiring.
func newLoginTestMux(t *testing.T, b loginBackend, withDir bool) (*http.ServeMux, *host.Host, string) {
	t.Helper()
	resetLoginState(t)
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	dir := t.TempDir()
	if withDir {
		if _, err := h.Creds.LoadAuthDir(dir); err != nil {
			t.Fatalf("LoadAuthDir: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/accounts/workbuddy/login-start", withAPIKey(handleLoginStart(b)))
	mux.HandleFunc("GET /api/accounts/workbuddy/login-poll", withAPIKey(handleLoginPoll(b, h)))
	return mux, h, dir
}

func doJSONReq(t *testing.T, mux *http.ServeMux, method, path, body string) map[string]any {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, rd)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: got %d, want 200 (body %s)", method, path, rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: body not JSON: %v (%s)", method, path, err, rec.Body)
	}
	return out
}

func TestLoginStartEndpoint(t *testing.T) {
	b := &fakeLoginBackend{startState: "st-1", startURL: "https://auth.example/wb?state=st-1"}
	mux, _, _ := newLoginTestMux(t, b, true)
	out := doJSONReq(t, mux, http.MethodPost, "/api/accounts/workbuddy/login-start", "{}")
	if out["state"] != "st-1" || out["login_url"] != "https://auth.example/wb?state=st-1" {
		t.Fatalf("start response = %v", out)
	}
	// The state is now pollable (session stored).
	if _, ok := loginSessionStore.get("st-1"); !ok {
		t.Fatal("state not stored after login-start")
	}
}

func TestLoginStartUpstreamFailure(t *testing.T) {
	b := &fakeLoginBackend{startErr: errors.New("auth state: upstream 500")}
	mux, _, _ := newLoginTestMux(t, b, true)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/workbuddy/login-start", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "生成登录链接失败") {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestLoginPollStateMachinePendingDoneExpired(t *testing.T) {
	b := &fakeLoginBackend{polls: []loginPollResult{
		{Status: "pending"},
		{Status: "done", Cred: &loginCredential{
			AccessToken: "at-1", RefreshToken: "rt-1", UID: "uid-1",
			Nickname: "浏览器登录号", EnterpriseID: "ent-1", Domain: "d.example",
			Realm: "cn", ExpiresAt: 1800000000,
		}},
	}}
	mux, h, dir := newLoginTestMux(t, b, true)
	loginSessionStore.put("st-1")

	// Round 1: pending.
	out := doJSONReq(t, mux, http.MethodGet, "/api/accounts/workbuddy/login-poll?state=st-1", "")
	if out["status"] != "pending" {
		t.Fatalf("poll 1 = %v", out)
	}
	// Round 2: done — uid/nickname on the wire, credential persisted.
	out = doJSONReq(t, mux, http.MethodGet, "/api/accounts/workbuddy/login-poll?state=st-1", "")
	if out["status"] != "done" || out["uid"] != "uid-1" || out["nickname"] != "浏览器登录号" {
		t.Fatalf("poll 2 = %v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-uid-1.json")); err != nil {
		t.Fatalf("auth file not persisted: %v", err)
	}
	if got := h.Creds.NicknameFor("workbuddy", "uid-1"); got != "浏览器登录号" {
		t.Fatalf("nickname = %q", got)
	}
	registered := false
	for _, a := range h.Pool.SnapshotAccounts() {
		if a.Platform == "workbuddy" && a.ID == "uid-1" && a.Status == "active" {
			registered = true
		}
	}
	if !registered {
		t.Fatal("done poll did not register the account in the pool")
	}
	// Round 3: the consumed state is gone — expired.
	out = doJSONReq(t, mux, http.MethodGet, "/api/accounts/workbuddy/login-poll?state=st-1", "")
	if out["status"] != "expired" {
		t.Fatalf("poll 3 = %v, want expired", out)
	}
	// The backend saw exactly the two forwarded polls.
	if len(b.pollStates) != 2 {
		t.Fatalf("backend polls = %v", b.pollStates)
	}
}

func TestLoginPollUnknownStateExpired(t *testing.T) {
	b := &fakeLoginBackend{}
	mux, _, _ := newLoginTestMux(t, b, true)
	out := doJSONReq(t, mux, http.MethodGet, "/api/accounts/workbuddy/login-poll?state=never-started", "")
	if out["status"] != "expired" {
		t.Fatalf("unknown state = %v, want expired", out)
	}
	if len(b.pollStates) != 0 {
		t.Fatalf("unknown state leaked to the backend: %v", b.pollStates)
	}
}

func TestLoginPollTTLExpired(t *testing.T) {
	b := &fakeLoginBackend{}
	mux, _, _ := newLoginTestMux(t, b, true)
	now := time.Unix(1800000000, 0)
	loginNow = func() time.Time { return now }
	loginSessionStore.put("st-ttl")

	// Inside the TTL: pollable (backend error class here, not expired — the
	// point is the session was found).
	b.polls = []loginPollResult{{Status: "pending"}}
	out := doJSONReq(t, mux, http.MethodGet, "/api/accounts/workbuddy/login-poll?state=st-ttl", "")
	if out["status"] != "pending" {
		t.Fatalf("in-TTL poll = %v", out)
	}
	// Beyond the TTL: expired, and the dead state is never forwarded.
	now = now.Add(loginSessionTTL + time.Minute)
	out = doJSONReq(t, mux, http.MethodGet, "/api/accounts/workbuddy/login-poll?state=st-ttl", "")
	if out["status"] != "expired" {
		t.Fatalf("TTL-expired poll = %v, want expired", out)
	}
	if len(b.pollStates) != 1 {
		t.Fatalf("expired state leaked to the backend: %v", b.pollStates)
	}
}

func TestLoginPollErrorKeepsSession(t *testing.T) {
	b := &fakeLoginBackend{polls: []loginPollResult{
		{Status: "error", Message: "获取账号信息失败: upstream 502"},
		{Status: "done", Cred: &loginCredential{
			AccessToken: "at-2", RefreshToken: "rt-2", UID: "uid-2", Nickname: "重试成功",
			Realm: "cn",
		}},
	}}
	mux, h, dir := newLoginTestMux(t, b, true)
	loginSessionStore.put("st-err")

	out := doJSONReq(t, mux, http.MethodGet, "/api/accounts/workbuddy/login-poll?state=st-err", "")
	if out["status"] != "error" || out["error"] != "获取账号信息失败: upstream 502" {
		t.Fatalf("poll 1 = %v", out)
	}
	if _, ok := loginSessionStore.get("st-err"); !ok {
		t.Fatal("error poll consumed the session; transient failures must stay recoverable")
	}
	// A later poll can still complete the flow.
	out = doJSONReq(t, mux, http.MethodGet, "/api/accounts/workbuddy/login-poll?state=st-err", "")
	if out["status"] != "done" || out["uid"] != "uid-2" {
		t.Fatalf("poll 2 = %v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-uid-2.json")); err != nil {
		t.Fatalf("auth file not persisted after retry: %v", err)
	}
	_ = h
}

func TestLoginPollPersistFailure(t *testing.T) {
	// Store wired but auth dir never loaded: AddWorkbuddyCredential refuses —
	// the handler must surface that as the error class, not 500/panic.
	b := &fakeLoginBackend{polls: []loginPollResult{
		{Status: "done", Cred: &loginCredential{
			AccessToken: "at-3", RefreshToken: "rt-3", UID: "uid-3", Realm: "cn",
		}},
	}}
	mux, _, _ := newLoginTestMux(t, b, false)
	loginSessionStore.put("st-pf")
	out := doJSONReq(t, mux, http.MethodGet, "/api/accounts/workbuddy/login-poll?state=st-pf", "")
	if out["status"] != "error" {
		t.Fatalf("poll = %v, want error", out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "凭证落盘失败") {
		t.Fatalf("error = %v", out["error"])
	}
	// The failed state is kept: fixing the store and re-polling can complete.
	if _, ok := loginSessionStore.get("st-pf"); !ok {
		t.Fatal("persist failure consumed the session")
	}
}

func TestLoginPollMissingStateParam(t *testing.T) {
	b := &fakeLoginBackend{}
	mux, _, _ := newLoginTestMux(t, b, true)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/accounts/workbuddy/login-poll", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if len(b.pollStates) != 0 {
		t.Fatalf("backend polled without state: %v", b.pollStates)
	}
}

func TestLoginRoutesAbsentWithoutStore(t *testing.T) {
	resetLoginState(t)
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	// h.Creds deliberately left nil.
	mux := http.NewServeMux()
	RegisterPanelData(mux, h, schedule.NewScheduler(), schedule.NewStatsMemory(), observability.NewRing(8))
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/accounts/workbuddy/login-start"},
		{http.MethodGet, "/api/accounts/workbuddy/login-poll?state=x"},
	} {
		req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s without store: got %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestLoginEndpointsRequireAPIKey(t *testing.T) {
	resetLoginState(t)
	loadM5Config(t, `{"listen":":0","api_key":"secret"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	dir := t.TempDir()
	if _, err := h.Creds.LoadAuthDir(dir); err != nil {
		t.Fatalf("LoadAuthDir: %v", err)
	}
	// Registration path (401 branch only — without the key the guard rejects
	// before any handler runs, so the real backend constructor is never
	// driven against the real upstream).
	mux := http.NewServeMux()
	registerLogin(mux, h)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/accounts/workbuddy/login-start"},
		{http.MethodGet, "/api/accounts/workbuddy/login-poll?state=x"},
	} {
		req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without key: got %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
	// With the key the guard passes through to the handler — asserted over
	// the fake backend so no test traffic ever reaches the real endpoints.
	fakeMux := http.NewServeMux()
	b := &fakeLoginBackend{startState: "st-k", startURL: "https://auth.example/k"}
	fakeMux.HandleFunc("POST /api/accounts/workbuddy/login-start", withAPIKey(handleLoginStart(b)))
	fakeMux.HandleFunc("GET /api/accounts/workbuddy/login-poll", withAPIKey(handleLoginPoll(b, h)))
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/accounts/workbuddy/login-start"},
		{http.MethodGet, "/api/accounts/workbuddy/login-poll?state=st-k"},
	} {
		authed := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader("{}"))
		authed.Header.Set("X-API-Key", "secret")
		rec2 := httptest.NewRecorder()
		fakeMux.ServeHTTP(rec2, authed)
		if rec2.Code == http.StatusUnauthorized {
			t.Fatalf("%s %s with key: got 401 (body %s)", tc.method, tc.path, rec2.Body)
		}
	}
}
