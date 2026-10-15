//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package workbuddy

// Growth-surface tests (growth.go) run exclusively against a local httptest
// stub — no real endpoint is ever contacted (task red line). Stub reply
// shapes are copied from the wb2api reference implementation and its recorded
// wire facts (panel/upstream/tasks.go + tasks_test.go): the {code,msg,data}
// envelope, data.tasks[] with flat OR object progress, the web claim route
// with the code in the path and x-client-platform: web, and the mp header
// variants. Unlike the tasks.go stub, this one is header-aware: the default
// and miniprogram lists share one path, so replies fork on
// X-Client-Platform.

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
)

// growthCall records one growth round trip the stub saw.
type growthCall struct {
	Method    string
	Path      string
	Body      string
	Platform  string // X-Client-Platform
	Referer   string
	Origin    string
	Auth      string
	UID       string
	Tenant    string
	UserAgent string
}

// growthStub is the header-aware fake upstream for the growth surface.
type growthStub struct {
	mu    sync.Mutex
	calls []growthCall

	listBody   string // default-口径 list reply body
	mpListBody string // mp-口径 list reply body; "" serves listBody
	listStatus int    // 0 → 200 (HTTP-status fault injection for the list)

	acceptStatus int // 0 → 200
	acceptBody   string
	claimFn      func(code, platform string) (int, string) // per-claim reply; nil → ok+100
}

func (s *growthStub) record(c growthCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}

func (s *growthStub) snapshot() []growthCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]growthCall(nil), s.calls...)
}

func (s *growthStub) serve(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, r.ContentLength)
	if r.ContentLength > 0 {
		_, _ = r.Body.Read(body)
	}
	s.record(growthCall{
		Method:    r.Method,
		Path:      r.URL.Path,
		Body:      string(body),
		Platform:  r.Header.Get("X-Client-Platform"),
		Referer:   r.Header.Get("Referer"),
		Origin:    r.Header.Get("Origin"),
		Auth:      r.Header.Get("Authorization"),
		UID:       r.Header.Get("X-User-Id"),
		Tenant:    r.Header.Get("X-Tenant-Id"),
		UserAgent: r.Header.Get("User-Agent"),
	})

	switch {
	case r.Method == http.MethodGet && r.URL.Path == pathGrowthTasks:
		if s.listStatus != 0 && s.listStatus != http.StatusOK {
			w.WriteHeader(s.listStatus)
		}
		if r.Header.Get("X-Client-Platform") == mpClientPlatform && s.mpListBody != "" {
			fmt.Fprint(w, s.mpListBody)
			return
		}
		fmt.Fprint(w, s.listBody)
	case r.Method == http.MethodPost && r.URL.Path == pathGrowthTasksAccept:
		if s.acceptStatus != 0 && s.acceptStatus != http.StatusOK {
			w.WriteHeader(s.acceptStatus)
		}
		if s.acceptBody != "" {
			fmt.Fprint(w, s.acceptBody)
			return
		}
		fmt.Fprint(w, `{"code":0,"msg":"ok","data":{}}`)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, pathGrowthClaimPrefix) && strings.HasSuffix(r.URL.Path, "/claim"):
		code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, pathGrowthClaimPrefix), "/claim")
		var reply statusBody
		if s.claimFn != nil {
			st, body := s.claimFn(code, r.Header.Get("X-Client-Platform"))
			reply = statusBody{st, body}
		} else {
			reply = statusBody{0, `{"code":0,"msg":"ok","data":{"already_claimed":false,"credit":100,"energy":5}}`}
		}
		if reply.status != 0 && reply.status != http.StatusOK {
			w.WriteHeader(reply.status)
		}
		fmt.Fprint(w, reply.body)
	default:
		fmt.Fprint(w, `{"code":0,"msg":"ok","data":{}}`)
	}
}

type statusBody struct {
	status int
	body   string
}

// newGrowthStub serves the fake upstream and returns a task client pointed at
// it for all three realm bases (chat/billing/web), plus the standard test
// credential (CN realm, ent-7 tenant).
func newGrowthStub(t *testing.T) (*growthStub, *TaskClient, *Credential) {
	t.Helper()
	stub := &growthStub{}
	srv := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(srv.Close)
	client := NewTaskClient(TaskClientOptions{
		ChatBaseURL:    srv.URL,
		BillingBaseURL: srv.URL,
		WebBaseURL:     srv.URL,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second}, // guards against hangs on stub bugs
	})
	cred := &Credential{
		AccessToken:  "growth-access-token",
		RefreshToken: "growth-refresh-token",
		UID:          "uid-9",
		EnterpriseID: "ent-7",
		Domain:       "www.codebuddy.cn",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	return stub, client, cred
}

// taskOf builds one upstream list row (flat progress form).
func taskOf(code, acceptStatus string, current, target int64) string {
	return fmt.Sprintf(`{"task_code":%q,"title":"上游标题-%s","reward_credit":100,"reward_energy":5,"has_reward":true,"accept_status":%q,"status":"","current":%d,"target":%d}`,
		code, code, acceptStatus, current, target)
}

// lockedTaskOf is taskOf with the upstream unlock gate set.
func lockedTaskOf(code string) string {
	return strings.Replace(taskOf(code, "not_accepted", 0, 1), `"has_reward":true`, `"has_reward":true,"locked":true`, 1)
}

// envelope wraps an inner data JSON fragment.
func envelope(data string) string { return `{"code":0,"msg":"ok","data":` + data + `}` }

// ---------- list ----------

func TestGrowthTasksMergesMPAndMapsTitles(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	stub.listBody = envelope(fmt.Sprintf(`{"tasks":[%s,%s,{"task_code":"Model_chat_GLM5.2","title":"","reward_credit":100,"accept_status":"accepted","progress":{"current":1,"target":1}}]}`,
		taskOf("automation_1", "completed", 1, 1),
		taskOf("chat_5", "in_progress", 3, 5),
	))
	stub.mpListBody = envelope(`{"tasks":[` +
		taskOf("chat_5", "in_progress", 3, 5) + `,` + // duplicate code → dropped
		taskOf("school_season", "not_accepted", 0, 0) + `]}`)

	tasks, err := c.GrowthTasks(context.Background(), cred)
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("task count = %d, want 4 (merged + deduped): %+v", len(tasks), tasks)
	}
	byCode := map[string]GrowthTask{}
	for _, task := range tasks {
		byCode[task.Code] = task
	}

	// automation_1: accept_status completed → claimable; the Go-side title
	// map wins over the upstream title.
	a := byCode["automation_1"]
	if a.Status != "claimable" || !a.Claimable {
		t.Errorf("automation_1 status = %q claimable=%v, want claimable/true", a.Status, a.Claimable)
	}
	if a.Title != "设置自动化任务" {
		t.Errorf("automation_1 title = %q, want the Go-side map title", a.Title)
	}
	if a.RewardCredit != 100 || a.RewardEnergy != 5 {
		t.Errorf("automation_1 rewards = %d/%d, want 100/5", a.RewardCredit, a.RewardEnergy)
	}

	// chat_5: mid-progress, mapped title, present once.
	ch := byCode["chat_5"]
	if ch.Status != "in_progress" || ch.Current != 3 || ch.Target != 5 {
		t.Errorf("chat_5 = %q %d/%d, want in_progress 3/5", ch.Status, ch.Current, ch.Target)
	}
	if ch.Title != "完成 5 次对话" {
		t.Errorf("chat_5 title = %q, want the mapped title", ch.Title)
	}

	// Model_chat_GLM5.2: the progress OBJECT overrides the (absent) flat
	// counters → full progress on the default realm marks it claimable.
	m := byCode["Model_chat_GLM5.2"]
	if !m.Claimable || m.Status != "claimable" || m.Current != 1 || m.Target != 1 {
		t.Errorf("Model_chat_GLM5.2 = %q %d/%d claimable=%v, want claimable 1/1", m.Status, m.Current, m.Target, m.Claimable)
	}

	// school_season: mp-exclusive, survives the merge.
	s := byCode["school_season"]
	if !s.MP || s.Status != "not_accepted" {
		t.Errorf("school_season mp=%v status=%q, want mp/true not_accepted", s.MP, s.Status)
	}
	if s.Title != "校园日" {
		t.Errorf("school_season title = %q, want the mapped title", s.Title)
	}

	// Both list variants were requested; the mp one carried the marker.
	var sawDefault, sawMP bool
	for _, call := range stub.snapshot() {
		if call.Path == pathGrowthTasks && call.Method == http.MethodGet {
			if call.Platform == mpClientPlatform {
				sawMP = true
			} else {
				sawDefault = true
			}
		}
	}
	if !sawDefault || !sawMP {
		t.Errorf("list calls: default=%v mp=%v, want both", sawDefault, sawMP)
	}
}

func TestGrowthTasksUpstreamErrorSurfaces(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	stub.listStatus = http.StatusInternalServerError
	stub.listBody = `{"code":500,"msg":"internal"}`
	_, err := c.GrowthTasks(context.Background(), cred)
	if err == nil {
		t.Fatal("list upstream 500 must surface as an error")
	}
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusInternalServerError {
		t.Fatalf("err = %v, want a classified 500 UpstreamError", err)
	}
}

func TestGrowthTasksBadBodyIsAnError(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	stub.listBody = "<html>not json</html>"
	if _, err := c.GrowthTasks(context.Background(), cred); err == nil {
		t.Fatal("unparseable list body must surface as an error")
	}
}

// ---------- accept ----------

func TestGrowthAcceptAllCountsAndSkipsSettled(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	list := envelope(`{"tasks":[` +
		taskOf("automation_1", "not_accepted", 0, 1) + `,` + // → accept (default)
		taskOf("chat_5", "accepted", 0, 5) + `,` + // already accepted → skip
		taskOf("Model_chat_GLM5.2", "claimed", 1, 1) + `,` + // claimed → skip
		lockedTaskOf("locked_chain") + `,` + // locked → skip
		taskOf("create_canvas", "completed", 1, 1) + `]}`) // completed → skip
	stub.listBody = list
	stub.mpListBody = envelope(`{"tasks":[` + taskOf("school_season", "not_accepted", 0, 1) + `]}`)

	n, err := c.GrowthAcceptAll(context.Background(), cred)
	if err != nil {
		t.Fatalf("GrowthAcceptAll: %v", err)
	}
	if n != 2 {
		t.Fatalf("accepted = %d, want 2 (default automation_1 + mp school_season)", n)
	}

	var defCodes, mpCodes string
	for _, call := range stub.snapshot() {
		if call.Path != pathGrowthTasksAccept {
			continue
		}
		switch call.Platform {
		case mpClientPlatform:
			mpCodes = call.Body
		default:
			defCodes = call.Body
		}
	}
	if defCodes != `{"task_codes":["automation_1"]}` {
		t.Errorf("default accept body = %s", defCodes)
	}
	if mpCodes != `{"task_codes":["school_season"]}` {
		t.Errorf("mp accept body = %s (mp codes must ride the mp header)", mpCodes)
	}
}

func TestGrowthAcceptAllBatchesOfTwenty(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	var rows []string
	for i := 0; i < 25; i++ {
		rows = append(rows, taskOf(fmt.Sprintf("bulk_%02d", i), "not_accepted", 0, 1))
	}
	stub.listBody = envelope(`{"tasks":[` + strings.Join(rows, ",") + `]}`)

	n, err := c.GrowthAcceptAll(context.Background(), cred)
	if err != nil {
		t.Fatalf("GrowthAcceptAll: %v", err)
	}
	if n != 25 {
		t.Fatalf("accepted = %d, want 25", n)
	}
	var batches [][]string
	for _, call := range stub.snapshot() {
		if call.Path == pathGrowthTasksAccept && call.Platform != mpClientPlatform {
			var sent struct {
				TaskCodes []string `json:"task_codes"`
			}
			if err := json.Unmarshal([]byte(call.Body), &sent); err != nil {
				t.Fatalf("accept body: %v", err)
			}
			batches = append(batches, sent.TaskCodes)
		}
	}
	if len(batches) != 2 || len(batches[0]) != 20 || len(batches[1]) != 5 {
		t.Fatalf("batches = %v, want [20 5]", func() []int {
			out := []int{}
			for _, b := range batches {
				out = append(out, len(b))
			}
			return out
		}())
	}
}

func TestGrowthAcceptAllUpstreamFailure(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	stub.listBody = envelope(`{"tasks":[` + taskOf("automation_1", "not_accepted", 0, 1) + `]}`)
	stub.acceptStatus = http.StatusInternalServerError
	stub.acceptBody = `{"code":500,"msg":"internal"}`
	n, err := c.GrowthAcceptAll(context.Background(), cred)
	if err == nil {
		t.Fatal("accept upstream failure must surface as an error")
	}
	if n != 0 {
		t.Fatalf("accepted = %d, want 0", n)
	}
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusInternalServerError {
		t.Fatalf("err = %v, want a classified 500 UpstreamError", err)
	}
}

// ---------- claim ----------

func TestGrowthClaimAutoSweepsClaimableOnly(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	stub.listBody = envelope(`{"tasks":[` +
		taskOf("automation_1", "in_progress", 1, 1) + `,` + // full progress, unclaimed → claim
		taskOf("chat_5", "claimed", 5, 5) + `,` + // claimed → skip
		taskOf("Model_chat_GLM5.2", "in_progress", 0, 1) + `]}`) // not full → skip
	// mp list adds nothing new (same codes) → single claimable task.
	stub.mpListBody = stub.listBody

	claimed, credit, err := c.GrowthClaimAuto(context.Background(), cred)
	if err != nil {
		t.Fatalf("GrowthClaimAuto: %v", err)
	}
	if claimed != 1 || credit != 100 {
		t.Fatalf("claimed=%d credit=%d, want 1/100", claimed, credit)
	}

	calls := stub.snapshot()
	var claimCalls []growthCall
	for _, call := range calls {
		if strings.HasSuffix(call.Path, "/claim") {
			claimCalls = append(claimCalls, call)
		}
	}
	if len(claimCalls) != 1 {
		t.Fatalf("claim calls = %d, want 1", len(claimCalls))
	}
	got := claimCalls[0]
	if want := pathGrowthClaimPrefix + "automation_1/claim"; got.Path != want {
		t.Errorf("claim path = %q, want %q (code rides the PATH)", got.Path, want)
	}
	if got.Platform != "web" {
		t.Errorf("x-client-platform = %q, want web", got.Platform)
	}
	if !strings.Contains(got.Referer, "profile/growth-center") {
		t.Errorf("Referer = %q, want the growth-center page", got.Referer)
	}
	if got.Body != "" {
		t.Errorf("claim must carry no body, got %q", got.Body)
	}
	if got.Auth != "Bearer growth-access-token" || got.UID != "uid-9" || got.Tenant != "ent-7" {
		t.Errorf("claim identity headers = %q/%q/%q", got.Auth, got.UID, got.Tenant)
	}
}

func TestGrowthClaimMPFallsBackToWeb(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	stub.mpListBody = envelope(`{"tasks":[` + taskOf("school_season", "in_progress", 1, 1) + `]}`)
	stub.listBody = envelope(`{"tasks":[]}`)
	stub.claimFn = func(code, platform string) (int, string) {
		if platform == mpClientPlatform {
			// Chat domain 400 on this path → the recorded web fallback.
			return http.StatusBadRequest, `{"code":400,"msg":"task not completed"}`
		}
		return 0, envelope(`{"already_claimed":false,"credit":200,"energy":5}`)
	}

	claimed, credit, err := c.GrowthClaimAuto(context.Background(), cred)
	if err != nil {
		t.Fatalf("GrowthClaimAuto: %v", err)
	}
	if claimed != 1 || credit != 200 {
		t.Fatalf("claimed=%d credit=%d, want 1/200 (web fallback)", claimed, credit)
	}
	var sawMP, sawWeb bool
	for _, call := range stub.snapshot() {
		if !strings.HasSuffix(call.Path, "/claim") {
			continue
		}
		switch call.Platform {
		case mpClientPlatform:
			sawMP = true
		case "web":
			sawWeb = true
		}
	}
	if !sawMP || !sawWeb {
		t.Errorf("claim attempts mp=%v web=%v, want the mp attempt THEN the web fallback", sawMP, sawWeb)
	}
}

func TestGrowthClaimAlreadyClaimedCountsButPaysNothing(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	stub.listBody = envelope(`{"tasks":[` + taskOf("automation_1", "completed", 1, 1) + `]}`)
	stub.claimFn = func(string, string) (int, string) {
		return 0, envelope(`{"already_claimed":true,"credit":100,"energy":5}`)
	}
	claimed, credit, err := c.GrowthClaimAuto(context.Background(), cred)
	if err != nil {
		t.Fatalf("GrowthClaimAuto: %v", err)
	}
	if claimed != 1 || credit != 0 {
		t.Fatalf("claimed=%d credit=%d, want 1/0 (already_claimed folds to no new credit)", claimed, credit)
	}
}

func TestGrowthClaimAutoAbortsOnFirstFailure(t *testing.T) {
	stub, c, cred := newGrowthStub(t)
	stub.listBody = envelope(`{"tasks":[` +
		taskOf("automation_1", "completed", 1, 1) + `,` +
		taskOf("create_canvas", "completed", 1, 1) + `]}`)
	stub.claimFn = func(string, string) (int, string) {
		return http.StatusBadRequest, `{"code":400,"msg":"task not completed"}`
	}
	claimed, credit, err := c.GrowthClaimAuto(context.Background(), cred)
	if err == nil {
		t.Fatal("a rejected claim must surface as an error")
	}
	if claimed != 0 || credit != 0 {
		t.Fatalf("claimed=%d credit=%d, want 0/0", claimed, credit)
	}
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want a classified UpstreamError", err)
	}
}
