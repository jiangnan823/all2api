//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package host

// End-to-end tests for the M3b schedule upstream: auth files in a t.TempDir →
// CredentialStore → newScheduleUpstream (workbuddy.TaskClient) → the real
// schedule handlers → pool write-back, all over an httptest stub upstream.
// No real endpoint is contacted and no real auth file is read (task red
// lines). Synthesized requests use httptest.NewRequestWithContext (noctx
// forbids the context-free variant).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
	"all2api/internal/schedule"
)

// stubTaskUpstream scripts per-path replies and records every call (method,
// path, body, uid) for the wire-shape assertions.
type stubTaskUpstream struct {
	mu      sync.Mutex
	replies map[string]stubTaskReply
	calls   []stubTaskCall
}

type stubTaskReply struct {
	status int // 0 → 200
	body   string
}

type stubTaskCall struct {
	Method string
	Path   string
	Body   string
	UID    string
}

func (u *stubTaskUpstream) script(path string, r stubTaskReply) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.replies == nil {
		u.replies = map[string]stubTaskReply{}
	}
	u.replies[path] = r
}

func (u *stubTaskUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		u.mu.Lock()
		u.calls = append(u.calls, stubTaskCall{Method: r.Method, Path: r.URL.Path, Body: string(body), UID: r.Header.Get("X-User-Id")})
		reply, ok := u.replies[r.URL.Path]
		u.mu.Unlock()
		if !ok {
			reply = stubTaskReply{status: http.StatusOK, body: `{"code":0,"msg":"ok","data":{}}`}
		}
		if reply.status != 0 && reply.status != http.StatusOK {
			w.WriteHeader(reply.status)
		}
		fmt.Fprint(w, reply.body)
	})
}

func (u *stubTaskUpstream) callsFor(path string) []stubTaskCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []stubTaskCall
	for _, c := range u.calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// wireScheduleUpstream serves the stub upstream, loads one auth file (uid-s)
// into a fresh store+pool, and returns the real schedule upstream pointed at
// the stub plus the assembled handler deps — the exact production chain with
// stub bases.
func wireScheduleUpstream(t *testing.T, u *stubTaskUpstream, creditFloor int64) (*pool.WeightedRandomSelector, *CredentialStore, schedule.Upstream, *schedule.Deps) {
	t.Helper()
	srv := httptest.NewServer(u.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	writeAuthFile(t, dir, "uid-s", fakeAccessToken("SCHEDFAKE"), fakeRefreshToken("SCHEDREF"), time.Now().Add(time.Hour).Unix())

	sel := pool.NewWeightedRandomSelector(creditFloor, 3, 60)
	store := NewCredentialStore(sel)
	if loaded, err := store.LoadAuthDir(dir); err != nil || loaded != 1 {
		t.Fatalf("LoadAuthDir: loaded=%d err=%v, want 1/nil", loaded, err)
	}
	// Mirror the main.go credit-seeding step: auth files carry no balance, so
	// the tests start from a seeded 500 instead of the registration default 0.
	sel.SetAccountCredit(WorkbuddyPlatform, "uid-s", 500)
	up := newWBScheduleUpstream(store, workbuddy.TaskClientOptions{
		ChatBaseURL:    srv.URL,
		BillingBaseURL: srv.URL,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		Credentials:    store,
		Refresher:      store,
	})
	deps := &schedule.Deps{
		Source:          NewScheduleSource(sel),
		Upstream:        up,
		Store:           NewScheduleStore(sel),
		FailureCooldown: time.Minute,
	}
	return sel, store, up, deps
}

func poolAccount(sel *pool.WeightedRandomSelector, platform, id string) pool.Account {
	for _, a := range sel.ListAccounts(platform) {
		if a.ID == id {
			return *a // value copy — callers compare fields, never mutate
		}
	}
	return pool.Account{}
}

func TestScheduleUpstreamSigninWritesCreditOnce(t *testing.T) {
	u := &stubTaskUpstream{}
	sel, _, _, deps := wireScheduleUpstream(t, u, 100)
	u.script("/v2/billing/meter/daily-checkin", stubTaskReply{body: `{"code":0,"msg":"ok","data":{"reward_credit":5}}`})

	h := schedule.NewSigninHandler(deps)
	if err := h(nil); err != nil {
		t.Fatalf("signin batch: %v", err)
	}
	acc := poolAccount(sel, WorkbuddyPlatform, "uid-s")
	if acc.Credit != 505 {
		t.Fatalf("credit = %d, want 505 (500 + awarded 5)", acc.Credit)
	}
	if acc.Status != schedule.AccountActive {
		t.Fatalf("status = %q, want active after success", acc.Status)
	}

	// Second run the same day: the idempotent 已签到 reply is success with
	// delta 0 — the credit must NOT be awarded twice.
	u.script("/v2/billing/meter/daily-checkin", stubTaskReply{body: `{"code":10001,"msg":"您今日已签到","data":null}`})
	if err := h(nil); err != nil {
		t.Fatalf("idempotent signin batch: %v", err)
	}
	if acc := poolAccount(sel, WorkbuddyPlatform, "uid-s"); acc.Credit != 505 {
		t.Fatalf("credit after idempotent rerun = %d, want still 505 (no double count)", acc.Credit)
	}
}

func TestScheduleUpstreamCatTravelStateCycle(t *testing.T) {
	u := &stubTaskUpstream{}
	sel, _, _, deps := wireScheduleUpstream(t, u, 100)

	// Run 1 (09:00 shape): idle → depart, delta 0.
	u.script("/activity/growth/buddy/travel/status", stubTaskReply{body: `{"code":0,"msg":"ok","data":{"status":"idle"}}`})
	u.script("/activity/growth/buddy/travel/depart", stubTaskReply{body: `{"code":0,"msg":"ok","data":{}}`})
	if err := schedule.NewCatTravelHandler(deps)(nil); err != nil {
		t.Fatalf("cat_travel depart batch: %v", err)
	}
	if acc := poolAccount(sel, WorkbuddyPlatform, "uid-s"); acc.Credit != 500 {
		t.Fatalf("credit after depart leg = %d, want 500 (no reward on depart)", acc.Credit)
	}
	if calls := u.callsFor("/activity/growth/buddy/travel/depart"); len(calls) != 1 || calls[0].Body != `{"location_id":1}` {
		t.Fatalf("depart calls = %+v, want one {\"location_id\":1}", calls)
	}

	// Run 2 (21:00 shape): arrived → claim, reward written back.
	u.script("/activity/growth/buddy/travel/status", stubTaskReply{body: `{"code":0,"msg":"ok","data":{"status":"arrived","record_id":7}}`})
	u.script("/activity/growth/buddy/travel/claim", stubTaskReply{body: `{"code":0,"msg":"ok","data":{"reward_credit":8}}`})
	if err := schedule.NewCatTravelHandler(deps)(nil); err != nil {
		t.Fatalf("cat_travel claim batch: %v", err)
	}
	if acc := poolAccount(sel, WorkbuddyPlatform, "uid-s"); acc.Credit != 508 {
		t.Fatalf("credit after claim = %d, want 508 (500 + reward 8)", acc.Credit)
	}
	if calls := u.callsFor("/activity/growth/buddy/travel/claim"); len(calls) != 1 || calls[0].Body != `{"record_id":7}` {
		t.Fatalf("claim calls = %+v, want one {\"record_id\":7}", calls)
	}
}

func TestScheduleUpstreamTokenKeepaliveRotatesAndWritesBack(t *testing.T) {
	u := &stubTaskUpstream{}
	sel, store, _, deps := wireScheduleUpstream(t, u, 100)
	rotAccess, rotRefresh := fakeAccessToken("SCHEDNEW"), fakeRefreshToken("SCHEDREFNEW")
	u.script("/v2/plugin/auth/token/refresh", stubTaskReply{body: fmt.Sprintf(
		`{"code":0,"msg":"ok","data":{"accessToken":%q,"refreshToken":%q,"expiresIn":5184000,"domain":"www.codebuddy.cn"}}`, rotAccess, rotRefresh)})

	if err := schedule.NewTokenKeepaliveHandler(deps)(nil); err != nil {
		t.Fatalf("token_keepalive batch: %v", err)
	}

	// Pool expiry synced forward by the 60-day expiresIn (both via the
	// AcceptRefreshed chain and the handler's SetExpiry write-back).
	acc := poolAccount(sel, WorkbuddyPlatform, "uid-s")
	if !acc.ExpiresAt.After(time.Now().Add(24 * time.Hour)) {
		t.Fatalf("pool expiry = %v, want the rotated 60d horizon", acc.ExpiresAt)
	}
	// Store serves the rotated pair.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/probe", nil)
	cred, err := store.CredentialFor(req.Context(), &pool.Account{ID: "uid-s", Platform: WorkbuddyPlatform})
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != rotAccess || cred.RefreshToken != rotRefresh {
		t.Error("store does not serve the rotated pair after the keepalive refresh")
	}
}

func TestScheduleUpstreamActiveCheckReportsPresence(t *testing.T) {
	u := &stubTaskUpstream{}
	sel, _, _, deps := wireScheduleUpstream(t, u, 100)

	if err := schedule.NewActiveCheckHandler(deps)(nil); err != nil {
		t.Fatalf("active_check batch: %v", err)
	}
	calls := u.callsFor("/v2/report")
	if len(calls) != 1 {
		t.Fatalf("report calls = %d, want 1", len(calls))
	}
	if !strings.Contains(calls[0].Body, `"eventCode":"chat_request_send"`) || !strings.Contains(calls[0].Body, `"userId":"uid-s"`) {
		t.Errorf("report body = %q, want the chat_request_send event with the uid injected", calls[0].Body)
	}
	if acc := poolAccount(sel, WorkbuddyPlatform, "uid-s"); acc.Status != schedule.AccountActive {
		t.Errorf("status = %q, want active after success", acc.Status)
	}
}

func TestScheduleUpstreamMissingCredentialFailsNotSkips(t *testing.T) {
	u := &stubTaskUpstream{}
	sel, _, up, _ := wireScheduleUpstream(t, u, 100)
	// A pooled account with no credential in the store.
	sel.AddAccount(&pool.Account{ID: "uid-ghost", Platform: WorkbuddyPlatform, Credit: 500, Status: "active"})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/probe", nil)
	_, err := up.Signin(req.Context(), &schedule.Account{ID: "uid-ghost", Platform: WorkbuddyPlatform, Status: schedule.AccountActive})
	if err == nil {
		t.Fatal("missing credential must error explicitly")
	}
	if errors.Is(err, schedule.ErrUnsupportedTask) {
		t.Errorf("err = %v, want the store's explicit no-credential error, not a capability skip", err)
	}
	if !strings.Contains(err.Error(), "no credential for workbuddy/uid-ghost") {
		t.Errorf("err = %v, want the credential-store lookup error", err)
	}

	// Through the handler: counted Failed (never Skipped), account cooled.
	deps := &schedule.Deps{
		Source:          NewScheduleSource(sel),
		Upstream:        up,
		Store:           NewScheduleStore(sel),
		FailureCooldown: time.Minute,
	}
	if err := schedule.NewSigninHandler(deps)(nil); err != nil {
		t.Fatalf("batch must isolate the per-account failure: %v", err)
	}
	acc := poolAccount(sel, WorkbuddyPlatform, "uid-ghost")
	if acc.Status != schedule.AccountCooling || acc.CooldownEnd.IsZero() {
		t.Fatalf("ghost account = %+v, want cooling after the explicit failure", acc)
	}
}

func TestScheduleUpstreamNonWorkbuddyPlatformSkips(t *testing.T) {
	_, _, up, _ := wireScheduleUpstream(t, &stubTaskUpstream{}, 100)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/probe", nil)
	acc := &schedule.Account{ID: "q1", Platform: "qoder", Status: schedule.AccountActive}

	if _, err := up.Signin(req.Context(), acc); !errors.Is(err, schedule.ErrUnsupportedTask) {
		t.Fatalf("Signin on qoder: err = %v, want ErrUnsupportedTask wrap", err)
	}
	if err := up.ActiveCheck(req.Context(), acc); !errors.Is(err, schedule.ErrUnsupportedTask) {
		t.Fatalf("ActiveCheck on qoder: err = %v, want ErrUnsupportedTask wrap", err)
	}
	if _, err := up.CatTravel(req.Context(), acc); !errors.Is(err, schedule.ErrUnsupportedTask) {
		t.Fatalf("CatTravel on qoder: err = %v, want ErrUnsupportedTask wrap", err)
	}
	if _, err := up.TokenKeepalive(req.Context(), acc); !errors.Is(err, schedule.ErrUnsupportedTask) {
		t.Fatalf("TokenKeepalive on qoder: err = %v, want ErrUnsupportedTask wrap", err)
	}
}

func TestScheduleUpstreamAuthExpiredDisablesAccount(t *testing.T) {
	u := &stubTaskUpstream{}
	sel, _, _, deps := wireScheduleUpstream(t, u, 100)
	// Chat-domain session death on check-in; the refresh chain then fails too
	// (500), so the credential is dead — the handler must DISABLE, not cool.
	u.script("/v2/billing/meter/daily-checkin", stubTaskReply{status: http.StatusUnauthorized, body: `{"code":12153,"msg":"Offline user session not found"}`})
	u.script("/v2/plugin/auth/token/refresh", stubTaskReply{status: http.StatusInternalServerError, body: `{"code":11140,"msg":"refresh rejected"}`})

	if err := schedule.NewSigninHandler(deps)(nil); err != nil {
		t.Fatalf("batch must isolate the per-account failure: %v", err)
	}
	if acc := poolAccount(sel, WorkbuddyPlatform, "uid-s"); acc.Status != schedule.AccountDisabled {
		t.Fatalf("status = %q, want disabled after dead-credential signin", acc.Status)
	}
}

// ---------- credit seeding ----------

// seedFixture builds a two-account store over an httptest billing stub and
// returns the pool, the store and a seeding client pointed at the stub.
func seedFixture(t *testing.T, u *stubTaskUpstream) (*pool.WeightedRandomSelector, *CredentialStore, *workbuddy.TaskClient) {
	t.Helper()
	srv := httptest.NewServer(u.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	writeAuthFile(t, dir, "uid-s", fakeAccessToken("SEEDFAKE1"), fakeRefreshToken("SEEDREF1"), time.Now().Add(time.Hour).Unix())
	writeAuthFile(t, dir, "uid-s2", fakeAccessToken("SEEDFAKE2"), fakeRefreshToken("SEEDREF2"), time.Now().Add(time.Hour).Unix())

	sel := pool.NewWeightedRandomSelector(100, 3, 60)
	store := NewCredentialStore(sel)
	if loaded, err := store.LoadAuthDir(dir); err != nil || loaded != 2 {
		t.Fatalf("LoadAuthDir: loaded=%d err=%v, want 2/nil", loaded, err)
	}
	client := workbuddy.NewTaskClient(workbuddy.TaskClientOptions{
		ChatBaseURL:    srv.URL,
		BillingBaseURL: srv.URL,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
	})
	return sel, store, client
}

func TestSeedCreditsWritesPoolBalances(t *testing.T) {
	u := &stubTaskUpstream{}
	sel, store, client := seedFixture(t, u)
	u.script("/v2/billing/meter/get-user-resource", stubTaskReply{body: `{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[{"CapacityUnit":"credits","CycleCapacityRemain":250}]}}}}`})

	seeded, failed := store.seedCreditsWith(client)
	if seeded != 2 || failed != 0 {
		t.Fatalf("seeded=%d failed=%d, want 2/0", seeded, failed)
	}
	if acc := poolAccount(sel, WorkbuddyPlatform, "uid-s"); acc.Credit != 250 {
		t.Errorf("uid-s credit = %d, want 250", acc.Credit)
	}
	if acc := poolAccount(sel, WorkbuddyPlatform, "uid-s2"); acc.Credit != 250 {
		t.Errorf("uid-s2 credit = %d, want 250", acc.Credit)
	}
	calls := u.callsFor("/v2/billing/meter/get-user-resource")
	if len(calls) != 2 {
		t.Fatalf("get-user-resource calls = %d, want one per account", len(calls))
	}
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.UID] = true
	}
	if !seen["uid-s"] || !seen["uid-s2"] {
		t.Errorf("balance queries seen for uids %v, want both accounts queried with their own identity", seen)
	}
}

func TestSeedCreditsFailureIsPerAccount(t *testing.T) {
	u := &stubTaskUpstream{}
	sel, store, client := seedFixture(t, u)
	// The balance query dies: every account stays at 0, no panic, counted
	// failed — best-effort contract.
	u.script("/v2/billing/meter/get-user-resource", stubTaskReply{status: http.StatusInternalServerError, body: `{"code":500,"msg":"billing down"}`})

	seeded, failed := store.seedCreditsWith(client)
	if seeded != 0 || failed != 2 {
		t.Fatalf("seeded=%d failed=%d, want 0/2", seeded, failed)
	}
	for _, id := range []string{"uid-s", "uid-s2"} {
		if acc := poolAccount(sel, WorkbuddyPlatform, id); acc.Credit != 0 {
			t.Errorf("%s credit = %d, want still 0 (no invented balance)", id, acc.Credit)
		}
	}
}
