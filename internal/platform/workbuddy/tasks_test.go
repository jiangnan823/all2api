//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package workbuddy

// Task-surface tests (tasks.go) run exclusively against httptest local stubs —
// no real endpoint is ever contacted (task hard constraint). Stub replies are
// derived from the recorded wire facts in docs/upstream/workbuddy-protocol.md
// §2/§5/§7 (envelope shapes, idempotent codes, travel state machine, balance
// nesting).

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
	"all2api/internal/pool"
)

// taskCall records one task round-trip the stub saw.
type taskCall struct {
	Method     string
	Path       string
	Body       string
	Auth       string
	UID        string
	Tenant     string
	Domain     string
	Gate       string
	Origin     string
	UserAgent  string
	RefreshTok string
}

// taskStub is the local fake upstream: per-path scriptable replies plus call
// recording. Replies not scripted default to the success envelope.
type taskStub struct {
	mu      sync.Mutex
	replies map[string]taskReply
	calls   []taskCall
}

type taskReply struct {
	status int // 0 → 200
	body   string
}

func (s *taskStub) script(path string, r taskReply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replies == nil {
		s.replies = map[string]taskReply{}
	}
	s.replies[path] = r
}

func (s *taskStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		call := taskCall{
			Method:     r.Method,
			Path:       r.URL.Path,
			Body:       string(body),
			Auth:       r.Header.Get("Authorization"),
			UID:        r.Header.Get("X-User-Id"),
			Tenant:     r.Header.Get("X-Tenant-Id"),
			Domain:     r.Header.Get("X-Domain"),
			Gate:       r.Header.Get("X-CodeBuddy-Request"),
			Origin:     r.Header.Get("Origin"),
			UserAgent:  r.Header.Get("User-Agent"),
			RefreshTok: r.Header.Get("X-Refresh-Token"),
		}
		s.mu.Lock()
		s.calls = append(s.calls, call)
		reply, ok := s.replies[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			reply = taskReply{status: http.StatusOK, body: `{"code":0,"msg":"ok","data":{}}`}
		}
		if reply.status != 0 && reply.status != http.StatusOK {
			w.WriteHeader(reply.status)
		}
		fmt.Fprint(w, reply.body)
	})
}

// snapshot copies the recorded calls under the lock.
func (s *taskStub) snapshot() []taskCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]taskCall(nil), s.calls...)
}

func (s *taskStub) callsFor(path string) []taskCall {
	var out []taskCall
	for _, c := range s.snapshot() {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// newTaskStub serves the fake upstream and returns a task client pointed at
// it for both realms, plus the standard test credential.
func newTaskStub(t *testing.T) (*taskStub, *TaskClient, *Credential) {
	t.Helper()
	stub := &taskStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	client := NewTaskClient(TaskClientOptions{
		ChatBaseURL:    srv.URL,
		BillingBaseURL: srv.URL,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second}, // guards against hangs on stub bugs
	})
	cred := &Credential{
		AccessToken:  "task-access-token",
		RefreshToken: "task-refresh-token",
		UID:          "uid-9",
		EnterpriseID: "ent-7",
		Domain:       "www.codebuddy.cn",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	return stub, client, cred
}

// ---------- sign-in ----------

func TestSigninSuccessExtractsAward(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	stub.script(pathDailyCheckin, taskReply{body: `{"code":0,"msg":"ok","data":{"reward_credit":5}}`})

	delta, err := c.Signin(context.Background(), cred)
	if err != nil {
		t.Fatalf("Signin: %v", err)
	}
	if delta != 5 {
		t.Fatalf("delta = %d, want 5", delta)
	}

	calls := stub.callsFor(pathDailyCheckin)
	if len(calls) != 1 {
		t.Fatalf("checkin calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", call.Method)
	}
	if call.Body != "{}" {
		t.Errorf("body = %q, want the empty object", call.Body)
	}
	if call.Auth != "Bearer task-access-token" {
		t.Errorf("Authorization = %q", call.Auth)
	}
}

func TestSigninIdempotentCodesAreSuccess(t *testing.T) {
	for name, body := range map[string]string{
		"code 10001": `{"code":10001,"msg":"您今日已签到","data":null}`,
		"code 14001": `{"code":14001,"msg":"已签到","data":null}`,
	} {
		stub, c, cred := newTaskStub(t)
		stub.script(pathDailyCheckin, taskReply{body: body})
		delta, err := c.Signin(context.Background(), cred)
		if err != nil {
			t.Fatalf("%s: err = %v, want idempotent success", name, err)
		}
		if delta != 0 {
			t.Fatalf("%s: delta = %d, want 0 (nothing new earned by this run)", name, delta)
		}
	}
}

func TestSigninQuotaCodeWithoutWordingIsQuotaError(t *testing.T) {
	// Disambiguation (report §6 vs §7): 14001 WITHOUT the 已签到 wording is the
	// chat-domain quota-exhaustion meaning and must classify as quota, not as
	// the idempotent success.
	stub, c, cred := newTaskStub(t)
	stub.script(pathDailyCheckin, taskReply{body: `{"code":14001,"msg":"credit quota exhausted","data":null}`})

	_, err := c.Signin(context.Background(), cred)
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Reason != adapter.ReasonQuotaExhausted {
		t.Fatalf("err = %v, want quota_exhausted", err)
	}
}

func TestSigninAwardFieldBestEffort(t *testing.T) {
	// The check-in reply's award field name is not pinned by the report; an
	// unknown shape must award 0 rather than invent a number.
	stub, c, cred := newTaskStub(t)
	stub.script(pathDailyCheckin, taskReply{body: `{"code":0,"msg":"ok","data":{"streak_days":3}}`})

	delta, err := c.Signin(context.Background(), cred)
	if err != nil {
		t.Fatalf("Signin: %v", err)
	}
	if delta != 0 {
		t.Fatalf("delta = %d, want 0 for an unrecognized award shape", delta)
	}
}

// ---------- billing header family ----------

func TestBillingHeaderFamilySingleSegmentUA(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	if _, err := c.Signin(context.Background(), cred); err != nil {
		t.Fatalf("Signin: %v", err)
	}

	call := stub.callsFor(pathDailyCheckin)[0]
	// Billing UA is the single WorkBuddy/<ver> segment — no platform or CLI
	// suffixes (report §7 BillingHeaders).
	if call.UserAgent != "WorkBuddy/"+wireClientVersion {
		t.Errorf("billing UA = %q, want %q", call.UserAgent, "WorkBuddy/"+wireClientVersion)
	}
	if strings.Contains(call.UserAgent, "CLI/") {
		t.Errorf("billing UA %q must not carry the CLI segment", call.UserAgent)
	}
	// X-Tenant-Id is the billing-domain identity header (report §3).
	if call.Tenant != "ent-7" {
		t.Errorf("X-Tenant-Id = %q, want ent-7", call.Tenant)
	}
	if call.UID != "uid-9" {
		t.Errorf("X-User-Id = %q, want uid-9", call.UID)
	}
	if call.Domain != "www.codebuddy.cn" {
		t.Errorf("X-Domain = %q, want www.codebuddy.cn", call.Domain)
	}
	if call.Gate != "1" {
		t.Errorf("X-CodeBuddy-Request = %q, want 1", call.Gate)
	}
	if call.Origin != wireOriginCN {
		t.Errorf("Origin = %q, want %q", call.Origin, wireOriginCN)
	}
}

func TestTravelHeaderFamilyKeepsChatUA(t *testing.T) {
	// The growth endpoints live on the chat base with the full chat family —
	// the three-segment UA stays (contrast with the billing single segment).
	stub, c, cred := newTaskStub(t)
	stub.script(pathTravelStatus, taskReply{body: `{"code":0,"msg":"ok","data":{"status":"traveling"}}`})

	if _, err := c.TravelStatus(context.Background(), cred); err != nil {
		t.Fatalf("TravelStatus: %v", err)
	}
	call := stub.callsFor(pathTravelStatus)[0]
	ua := call.UserAgent
	if !strings.HasPrefix(ua, "WorkBuddy/"+wireClientVersion+" WorkBuddy/") || !strings.Contains(ua, " CLI/") {
		t.Errorf("travel UA = %q, want the three-segment chat UA", ua)
	}
	if call.Method != http.MethodGet {
		t.Errorf("travel status method = %s, want GET", call.Method)
	}
}

// ---------- checkin activity status ----------

func TestCheckinActivityStatusMustPost(t *testing.T) {
	// GET answers 404 upstream (report §7): the client must speak POST.
	stub, c, cred := newTaskStub(t)
	stub.script(pathCheckinStatus, taskReply{body: `{"code":0,"msg":"ok","data":{"active":true,"today_checked_in":true,"streak_days":3,"daily_credit":3}}`})

	data, err := c.CheckinActivityStatus(context.Background(), cred)
	if err != nil {
		t.Fatalf("CheckinActivityStatus: %v", err)
	}
	if data["active"] != true || data["today_checked_in"] != true {
		t.Errorf("status data = %v, want the parsed activity object", data)
	}
	call := stub.callsFor(pathCheckinStatus)[0]
	if call.Method != http.MethodPost {
		t.Errorf("method = %s, want POST (GET answers 404 upstream)", call.Method)
	}
}

// ---------- active report ----------

func TestReportActiveInjectsUserId(t *testing.T) {
	stub, c, cred := newTaskStub(t)

	if err := c.ReportActive(context.Background(), cred); err != nil {
		t.Fatalf("ReportActive: %v", err)
	}
	calls := stub.callsFor(pathReport)
	if len(calls) != 1 {
		t.Fatalf("report calls = %d, want 1", len(calls))
	}
	var events []map[string]any
	if err := json.Unmarshal([]byte(calls[0].Body), &events); err != nil {
		t.Fatalf("report body not an event array: %v (%q)", err, calls[0].Body)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0]["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode = %v, want chat_request_send", events[0]["eventCode"])
	}
	// userId is the required field — missing it means a 200 the upstream
	// silently drops (report §7).
	if events[0]["userId"] != "uid-9" {
		t.Errorf("userId = %v, want uid-9 injected from the credential", events[0]["userId"])
	}
	// Full client shape: the live endpoint rejected a bare {eventCode,userId}
	// event (2026-10-15 10:00 cron, 4/4 accounts failed), so the event must
	// carry the reference's full field set, not a minimal subset.
	for _, key := range []string{"timestamp", "mode", "conversationId", "requestId", "requestModelId", "presentAt", "rootRequestId", "parentConversationId", "agentName", "agentType"} {
		if _, ok := events[0][key]; !ok {
			t.Errorf("event missing %q — live upstream rejects minimal shapes", key)
		}
	}
	if events[0]["mode"] != "craft" {
		t.Errorf("mode = %v, want craft (client shape)", events[0]["mode"])
	}
	if events[0]["conversationId"] != events[0]["rootRequestId"] {
		t.Errorf("synthetic conversationId/rootRequestId mismatch: %v vs %v", events[0]["conversationId"], events[0]["rootRequestId"])
	}
}

func TestReportActiveRequiresUID(t *testing.T) {
	stub, c, _ := newTaskStub(t)
	noUID := &Credential{AccessToken: "tok"} // uid missing

	if err := c.ReportActive(context.Background(), noUID); err == nil {
		t.Fatal("missing uid must error before the wire call (200-but-dropped upstream)")
	}
	if n := len(stub.callsFor(pathReport)); n != 0 {
		t.Fatalf("report calls = %d, want 0", n)
	}
}

// ---------- cat travel state machine ----------

func TestTravelCycleIdleDeparts(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	stub.script(pathTravelStatus, taskReply{body: `{"code":0,"msg":"ok","data":{"status":"idle"}}`})

	delta, err := c.TravelCycle(context.Background(), cred)
	if err != nil {
		t.Fatalf("TravelCycle(idle): %v", err)
	}
	if delta != 0 {
		t.Fatalf("delta = %d, want 0 on the depart leg", delta)
	}
	if calls := stub.callsFor(pathTravelDepart); len(calls) != 1 {
		t.Fatalf("depart calls = %d, want 1", len(calls))
	} else if calls[0].Body != `{"location_id":1}` {
		t.Errorf("depart body = %q, want {\"location_id\":1}", calls[0].Body)
	}
	if calls := stub.callsFor(pathTravelClaim); len(calls) != 0 {
		t.Errorf("claim calls = %d, want 0 (one step per cycle)", len(calls))
	}
}

func TestTravelCycleTravelingIsNoop(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	stub.script(pathTravelStatus, taskReply{body: `{"code":0,"msg":"ok","data":{"status":"traveling"}}`})

	delta, err := c.TravelCycle(context.Background(), cred)
	if err != nil {
		t.Fatalf("TravelCycle(traveling): %v", err)
	}
	if delta != 0 {
		t.Fatalf("delta = %d, want 0 while traveling", delta)
	}
	if n := len(stub.snapshot()); n != 1 {
		t.Fatalf("wire calls = %d, want exactly the one status call", n)
	}
}

func TestTravelCycleArrivedClaimsReward(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	stub.script(pathTravelStatus, taskReply{body: `{"code":0,"msg":"ok","data":{"status":"arrived","record_id":42}}`})
	stub.script(pathTravelClaim, taskReply{body: `{"code":0,"msg":"ok","data":{"reward_credit":8}}`})

	delta, err := c.TravelCycle(context.Background(), cred)
	if err != nil {
		t.Fatalf("TravelCycle(arrived): %v", err)
	}
	if delta != 8 {
		t.Fatalf("delta = %d, want the claimed reward_credit 8", delta)
	}
	calls := stub.callsFor(pathTravelClaim)
	if len(calls) != 1 {
		t.Fatalf("claim calls = %d, want 1", len(calls))
	}
	if calls[0].Body != `{"record_id":42}` {
		t.Errorf("claim body = %q, want the status-provided record id", calls[0].Body)
	}
}

func TestTravelCycleDailyLimitIsCompletion(t *testing.T) {
	t.Run("status leg", func(t *testing.T) {
		stub, c, cred := newTaskStub(t)
		stub.script(pathTravelStatus, taskReply{body: `{"code":77,"msg":"daily_limit_reached","data":null}`})
		delta, err := c.TravelCycle(context.Background(), cred)
		if err != nil || delta != 0 {
			t.Fatalf("delta=%d err=%v, want (0, nil)", delta, err)
		}
		if n := len(stub.snapshot()); n != 1 {
			t.Fatalf("wire calls = %d, want exactly the one status call", n)
		}
	})
	t.Run("depart leg", func(t *testing.T) {
		stub, c, cred := newTaskStub(t)
		stub.script(pathTravelStatus, taskReply{body: `{"code":0,"msg":"ok","data":{"status":"idle"}}`})
		stub.script(pathTravelDepart, taskReply{body: `{"code":2,"msg":"daily_limit_reached","data":null}`})
		delta, err := c.TravelCycle(context.Background(), cred)
		if err != nil || delta != 0 {
			t.Fatalf("delta=%d err=%v, want (0, nil)", delta, err)
		}
	})
	t.Run("claim leg", func(t *testing.T) {
		stub, c, cred := newTaskStub(t)
		stub.script(pathTravelStatus, taskReply{body: `{"code":0,"msg":"ok","data":{"status":"arrived","record_id":7}}`})
		stub.script(pathTravelClaim, taskReply{body: `{"code":1,"msg":"daily_limit_reached","data":null}`})
		delta, err := c.TravelCycle(context.Background(), cred)
		if err != nil || delta != 0 {
			t.Fatalf("delta=%d err=%v, want (0, nil)", delta, err)
		}
	})
}

func TestTravelCycleUnknownStateErrors(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	stub.script(pathTravelStatus, taskReply{body: `{"code":0,"msg":"ok","data":{"status":"hibernating"}}`})

	if _, err := c.TravelCycle(context.Background(), cred); err == nil || !strings.Contains(err.Error(), "unknown travel state") {
		t.Fatalf("err = %v, want the explicit unknown-state error", err)
	}
}

// ---------- balance query ----------

func TestGetUserResourceFiltersCreditsUnit(t *testing.T) {
	stub, c, cred := newTaskStub(t)
	stub.script(pathGetUserResource, taskReply{body: `{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[` +
		`{"CapacityUnit":"tokens","CycleCapacityRemain":999000},` +
		`{"CapacityUnit":"credits","CycleCapacityRemain":2500}` +
		`]}}}}`})

	credits, err := c.GetUserResource(context.Background(), cred)
	if err != nil {
		t.Fatalf("GetUserResource: %v", err)
	}
	if credits != 2500 {
		t.Fatalf("credits = %d, want 2500 (CapacityUnit=credits row, tokens row skipped)", credits)
	}
	call := stub.callsFor(pathGetUserResource)[0]
	if call.Method != http.MethodPost || call.Body != "{}" {
		t.Errorf("call = %s %s, want POST with the empty object body", call.Method, call.Body)
	}
}

func TestGetUserResourceToleratesMissingData(t *testing.T) {
	// 容错 contract: any missing path element is an explicit error — never a
	// panic, never an invented 0 (a fabricated 0 would silently park the
	// account below the credit floor).
	for name, body := range map[string]string{
		"no data":        `{"code":0,"msg":"ok"}`,
		"no Response":    `{"code":0,"msg":"ok","data":{}}`,
		"no Data":        `{"code":0,"msg":"ok","data":{"Response":{}}}`,
		"no Accounts":    `{"code":0,"msg":"ok","data":{"Response":{"Data":{}}}}`,
		"no credits row": `{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[{"CapacityUnit":"tokens","CycleCapacityRemain":1}]}}}}`,
		"not json":       `<html>gateway noise</html>`,
	} {
		stub, c, cred := newTaskStub(t)
		stub.script(pathGetUserResource, taskReply{body: body})
		credits, err := c.GetUserResource(context.Background(), cred)
		if err == nil {
			t.Fatalf("%s: err = nil, want an explicit shape error", name)
		}
		if credits != 0 {
			t.Fatalf("%s: credits = %d, want 0", name, credits)
		}
	}
}

// ---------- refresh (token keepalive) ----------

// memCreds is the in-memory credential source/sink pair the Refresh tests use.
type memCreds struct {
	mu   sync.Mutex
	cred *Credential
	sunk *Credential
}

func (m *memCreds) CredentialFor(_ context.Context, _ *pool.Account) (*Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cred, nil
}

func (m *memCreds) AcceptRefreshed(_ context.Context, _ *pool.Account, cred *Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sunk = cred
	return nil
}

func TestTaskClientRefreshChain(t *testing.T) {
	stub := &taskStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	store := &memCreds{cred: &Credential{
		AccessToken:  "stale-access",
		RefreshToken: "stored-refresh",
		UID:          "uid-9",
		ExpiresAt:    time.Now().Add(-time.Hour), // expired
	}}
	c := NewTaskClient(TaskClientOptions{
		ChatBaseURL: srv.URL,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
		Credentials: store,
		Refresher:   store,
	})
	stub.script(wireRefreshPath, taskReply{body: `{"code":0,"msg":"ok","data":{"accessToken":"rotated-access","refreshToken":"rotated-refresh","expiresIn":5184000,"domain":"www.codebuddy.cn"}}`})

	rotated, err := c.Refresh(context.Background(), &pool.Account{ID: "uid-9", Platform: "workbuddy"})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if rotated.AccessToken != "rotated-access" || rotated.RefreshToken != "rotated-refresh" {
		t.Fatalf("rotated pair = %s/%s, want the stub's rotation", rotated.AccessToken, rotated.RefreshToken)
	}
	if rotated.ExpiresAt.IsZero() || !rotated.ExpiresAt.After(time.Now().Add(24*time.Hour)) {
		t.Errorf("ExpiresAt = %v, want now+60d from expiresIn", rotated.ExpiresAt)
	}
	if store.sunk == nil || store.sunk.AccessToken != "rotated-access" {
		t.Error("sink did not receive the rotated pair (write-back chain broken)")
	}

	call := stub.callsFor(wireRefreshPath)[0]
	if call.RefreshTok != "stored-refresh" {
		t.Errorf("X-Refresh-Token = %q, want the stored refresh token", call.RefreshTok)
	}
	if call.Auth != "" {
		t.Errorf("refresh request carried Authorization %q — the refresh endpoint never takes a bearer", call.Auth)
	}
}
