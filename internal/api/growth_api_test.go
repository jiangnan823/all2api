package api

// Growth endpoint tests: handler behavior against a stubbed growthBackend
// (frozen wire shape, accept/claim counters, upstream-500 → 502 passthrough,
// credential miss → 404, wrong platform → 400) plus the route registration.
// The end-to-end variant with the REAL workbuddy client over an httptest
// upstream lives in growth_api_wire_test.go (workbuddy-tagged, like the wire
// file it exercises). No test here ever touches the network.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"all2api/internal/adapter"
	"all2api/internal/host"
	"all2api/internal/observability"
	"all2api/internal/schedule"
)

// errUpstreamBoom mimics a classified upstream failure.
func errUpstreamBoom() error {
	return &adapter.UpstreamError{
		Status:  http.StatusInternalServerError,
		Reason:  adapter.ReasonPlatformDown,
		Message: "upstream internal error",
	}
}

// fakeGrowthBackend scripts the three seam calls and records invocation.
type fakeGrowthBackend struct {
	tasks    []growthTaskWire
	taskErr  error
	acceptN  int
	acceptEr error
	claimN   int
	claimCr  int64
	claimErr error

	called map[string]int
}

func (f *fakeGrowthBackend) rec(name string) {
	if f.called == nil {
		f.called = map[string]int{}
	}
	f.called[name]++
}

func (f *fakeGrowthBackend) Tasks(context.Context, string, string) ([]growthTaskWire, error) {
	f.rec("tasks")
	return f.tasks, f.taskErr
}

func (f *fakeGrowthBackend) AcceptAll(context.Context, string, string) (int, error) {
	f.rec("accept")
	return f.acceptN, f.acceptEr
}

func (f *fakeGrowthBackend) ClaimAuto(context.Context, string, string) (int, int64, error) {
	f.rec("claim")
	return f.claimN, f.claimCr, f.claimErr
}

// newGrowthMux mounts the three handlers over the given backend on a bare
// mux (the handler-level tests do not need the store).
func newGrowthMux(b growthBackend) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/accounts/{id}/growth-tasks", withAPIKey(handleGrowthTasks(b)))
	mux.HandleFunc("POST /api/accounts/{id}/growth-accept", withAPIKey(handleGrowthAccept(b)))
	mux.HandleFunc("POST /api/accounts/{id}/growth-claim-auto", withAPIKey(handleGrowthClaim(b)))
	return mux
}

func growthDo(t *testing.T, mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ---------- GET growth-tasks ----------

func TestGrowthTasksFrozenWireShape(t *testing.T) {
	b := &fakeGrowthBackend{tasks: []growthTaskWire{{
		Code: "automation_1", Title: "设置自动化任务",
		ProgressDone: 1, ProgressTotal: 1,
		RewardCredit: 100, RewardEnergy: 5,
		Status: "claimed",
	}}}
	rec := growthDo(t, newGrowthMux(b), http.MethodGet, "/api/accounts/workbuddy:uid-1/growth-tasks")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	// Exact frozen keys — no extras, no renames (the frontend is built
	// against this shape verbatim).
	var doc struct {
		Source string           `json:"source"`
		Tasks  []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Source != "upstream" {
		t.Errorf("source = %q, want upstream", doc.Source)
	}
	if len(doc.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(doc.Tasks))
	}
	task := doc.Tasks[0]
	for key, want := range map[string]any{
		"code": "automation_1", "title": "设置自动化任务",
		"progress_done": float64(1), "progress_total": float64(1),
		"reward_credit": float64(100), "reward_energy": float64(5),
		"status": "claimed",
	} {
		got, ok := task[key]
		if !ok {
			t.Errorf("missing frozen key %q", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if len(task) != 7 {
		t.Errorf("task carries %d keys, want exactly the 7 frozen ones (%v)", len(task), task)
	}
}

func TestGrowthTasksEmptyStaysAnArray(t *testing.T) {
	b := &fakeGrowthBackend{tasks: []growthTaskWire{}}
	rec := growthDo(t, newGrowthMux(b), http.MethodGet, "/api/accounts/workbuddy:uid-1/growth-tasks")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"tasks":[]`) {
		t.Fatalf("empty tasks must serialize as [], got %s", body)
	}
	if !strings.Contains(rec.Body.String(), `"source":"upstream"`) {
		t.Fatalf("missing source marker: %s", rec.Body)
	}
}

func TestGrowthTasksUpstreamFailureIs502WithChineseReason(t *testing.T) {
	b := &fakeGrowthBackend{taskErr: errUpstreamBoom()}
	rec := growthDo(t, newGrowthMux(b), http.MethodGet, "/api/accounts/workbuddy:uid-1/growth-tasks")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502 (upstream failure passes through as-is)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "查询成长任务失败") || !strings.Contains(body, "upstream internal error") {
		t.Fatalf("error body must carry the Chinese frame + the upstream message, got %s", body)
	}
	if strings.Contains(body, "Bearer") || strings.Contains(body, "access") {
		t.Fatal("error body must never echo credential material")
	}
}

// ---------- POST growth-accept ----------

func TestGrowthAcceptCounts(t *testing.T) {
	b := &fakeGrowthBackend{acceptN: 3}
	rec := growthDo(t, newGrowthMux(b), http.MethodPost, "/api/accounts/workbuddy:uid-1/growth-accept")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var doc struct {
		Accepted int `json:"accepted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Accepted != 3 {
		t.Fatalf("accepted = %d, want 3", doc.Accepted)
	}
	if b.called["accept"] != 1 {
		t.Fatalf("backend AcceptAll called %d times, want 1", b.called["accept"])
	}
}

// ---------- POST growth-claim-auto ----------

func TestGrowthClaimAccumulates(t *testing.T) {
	b := &fakeGrowthBackend{claimN: 2, claimCr: 300}
	rec := growthDo(t, newGrowthMux(b), http.MethodPost, "/api/accounts/workbuddy:uid-1/growth-claim-auto")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var doc struct {
		Claimed      int   `json:"claimed"`
		CreditGained int64 `json:"credit_gained"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Claimed != 2 || doc.CreditGained != 300 {
		t.Fatalf("claimed/credit_gained = %d/%d, want 2/300", doc.Claimed, doc.CreditGained)
	}
}

func TestGrowthClaimUpstreamFailureIs502(t *testing.T) {
	b := &fakeGrowthBackend{claimErr: errUpstreamBoom()}
	rec := growthDo(t, newGrowthMux(b), http.MethodPost, "/api/accounts/workbuddy:uid-1/growth-claim-auto")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", rec.Code)
	}
}

// ---------- error mapping ----------

func TestGrowthUnknownAccountIs404(t *testing.T) {
	for name, path := range map[string]string{
		"tasks":  "/api/accounts/workbuddy:ghost/growth-tasks",
		"accept": "/api/accounts/workbuddy:ghost/growth-accept",
		"claim":  "/api/accounts/workbuddy:ghost/growth-claim-auto",
	} {
		method := http.MethodGet
		if name != "tasks" {
			method = http.MethodPost
		}
		// Every seam method reports the credential miss, not just Tasks.
		b := &fakeGrowthBackend{
			taskErr:  &growthAccountError{msg: "credential store: no credential"},
			acceptEr: &growthAccountError{msg: "credential store: no credential"},
			claimErr: &growthAccountError{msg: "credential store: no credential"},
		}
		rec := growthDo(t, newGrowthMux(b), method, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", name, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), "账号不存在") {
			t.Errorf("%s: error body = %s, want the Chinese 404 reason", name, rec.Body)
		}
	}
}

func TestGrowthWrongPlatformIs400(t *testing.T) {
	for _, path := range []string{
		"/api/accounts/qoder:pat-1/growth-tasks",
		"/api/accounts/qoder:pat-1/growth-accept",
		"/api/accounts/trae:acc/growth-claim-auto",
	} {
		method := http.MethodGet
		if strings.HasSuffix(path, "-auto") || strings.HasSuffix(path, "accept") {
			method = http.MethodPost
		}
		rec := growthDo(t, newGrowthMux(&fakeGrowthBackend{}), method, path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), "该平台无成长任务面") {
			t.Errorf("%s: error body = %s, want the frozen wording", path, rec.Body)
		}
	}
}

func TestGrowthMalformedSegmentIs400(t *testing.T) {
	rec := growthDo(t, newGrowthMux(&fakeGrowthBackend{}), http.MethodGet, "/api/accounts/workbuddy/growth-tasks")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestGrowthNilBackendAnswersHonestly(t *testing.T) {
	rec := growthDo(t, newGrowthMux(nil), http.MethodGet, "/api/accounts/workbuddy:uid-1/growth-tasks")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "未包含 workbuddy 平台") {
		t.Fatalf("error body = %s, want the build-limitation reason", rec.Body)
	}
}

// ---------- registration (real host, no network) ----------

func TestGrowthRoutesRegistered(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	mux := http.NewServeMux()
	RegisterPanelData(mux, h, schedule.NewScheduler(), schedule.NewStatsMemory(), observability.NewRing(8))

	// Malformed segment → 400 proves the route is mounted (and never reaches
	// the backend, so no upstream contact is possible here).
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/accounts/noseparator/growth-tasks", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}

	// Unknown account → 404 through the REAL backend (store miss short-
	// circuits before any upstream call).
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/accounts/workbuddy:ghost/growth-tasks", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 (unknown account through the real backend)", rec.Code)
	}
}
