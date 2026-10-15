//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package api

// End-to-end growth endpoint test over the REAL wire backend: a host
// credential store loaded from a t.TempDir fake auth file (never a real
// credential file — red line), a workbuddy TaskClient pointed at a local
// httptest stub (never the real upstream — red line), and the three handlers
// mounted the way registerGrowth mounts them. Covers the CredentialFor
// lookup path, the frozen wire shape with the Go-side Chinese titles, the
// accept counter and the claim credit sum in one pass.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"all2api/internal/host"
	"all2api/internal/platform/workbuddy"
)

// wireStub is a minimal growth upstream: it serves the recorded wb2api
// response shapes and counts the calls it saw.
type wireStub struct {
	mu    sync.Mutex
	calls map[string]int
}

func (s *wireStub) seen(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

func (s *wireStub) handler() http.Handler {
	s.mu.Lock()
	s.calls = map[string]int{}
	s.mu.Unlock()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls[r.URL.Path]++
		s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			// Default realm list: one claimable (accept_status completed),
			// one in-progress, one already claimed. Shapes copied from the
			// wb2api reference parse table.
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[` +
				`{"task_code":"automation_1","title":"设置自动化任务","reward_credit":100,"reward_energy":5,"has_reward":true,"accept_status":"completed","status":"","current":1,"target":1},` +
				`{"task_code":"chat_5","title":"每日对话","reward_credit":100,"accept_status":"in_progress","current":3,"target":5},` +
				`{"task_code":"Model_chat_GLM5.2","title":"模型对话","accept_status":"claimed","current":1,"target":1},` +
				`{"task_code":"playbook_prompt","title":"灵感案例","accept_status":"not_accepted","current":0,"target":1}]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"already_claimed":false,"credit":100,"energy":5}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	})
}

func TestGrowthEndpointsEndToEnd(t *testing.T) {
	stub := &wireStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)

	// Fake credential in a temp dir (t.TempDir — never a real credential
	// file), loaded through the store's normal path so CredentialFor serves it.
	dir := t.TempDir()
	authJSON := `{"account":{"uid":"wb-growth-1","nickname":"成长号"},"auth":{"accessToken":"fake-access","refreshToken":"fake-refresh","expiresAt":1800000000,"realm":"cn"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-wb-growth-1.json"), []byte(authJSON), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}
	if n, err := h.Creds.LoadAuthDir(dir); err != nil || n != 1 {
		t.Fatalf("LoadAuthDir = %d, %v; want 1, nil", n, err)
	}

	backend := newWBGrowthBackend(h.Creds, workbuddy.TaskClientOptions{
		ChatBaseURL:    srv.URL,
		BillingBaseURL: srv.URL,
		WebBaseURL:     srv.URL,
	})
	mux := newGrowthMux(backend)

	// GET growth-tasks: frozen shape, merged list, mapped titles, statuses.
	rec := growthDo(t, mux, http.MethodGet, "/api/accounts/workbuddy:wb-growth-1/growth-tasks")
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var doc struct {
		Source string           `json:"source"`
		Tasks  []growthTaskWire `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Source != "upstream" || len(doc.Tasks) != 4 {
		t.Fatalf("source/tasks = %q/%d, want upstream/4", doc.Source, len(doc.Tasks))
	}
	byCode := map[string]growthTaskWire{}
	for _, task := range doc.Tasks {
		byCode[task.Code] = task
	}
	if got := byCode["automation_1"]; got.Status != "claimable" || got.Title != "设置自动化任务" ||
		got.ProgressDone != 1 || got.ProgressTotal != 1 || got.RewardCredit != 100 || got.RewardEnergy != 5 {
		t.Errorf("automation_1 = %+v, want claimable 1/1 100c/5e with the mapped title", got)
	}
	if got := byCode["chat_5"]; got.Status != "in_progress" || got.ProgressDone != 3 || got.ProgressTotal != 5 {
		t.Errorf("chat_5 = %+v, want in_progress 3/5", got)
	}
	if got := byCode["Model_chat_GLM5.2"]; got.Status != "claimed" {
		t.Errorf("Model_chat_GLM5.2 status = %q, want claimed", got.Status)
	}

	// POST growth-accept: the not_accepted and the in_progress task go out
	// (wb2api semantics — only accepted/completed/claimed/locked are settled;
	// accept is idempotent so in_progress re-signups are harmless).
	rec = growthDo(t, mux, http.MethodPost, "/api/accounts/workbuddy:wb-growth-1/growth-accept")
	if rec.Code != http.StatusOK {
		t.Fatalf("accept: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var acc struct {
		Accepted int `json:"accepted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode accept: %v", err)
	}
	if acc.Accepted != 2 {
		t.Fatalf("accepted = %d, want 2 (playbook_prompt not_accepted + chat_5 in_progress; the rest settled)", acc.Accepted)
	}
	if stub.seen("/v2/activity/growth/tasks/accept") != 1 {
		t.Fatalf("accept upstream calls = %d, want 1", stub.seen("/v2/activity/growth/tasks/accept"))
	}

	// POST growth-claim-auto: the single claimable task pays 100.
	rec = growthDo(t, mux, http.MethodPost, "/api/accounts/workbuddy:wb-growth-1/growth-claim-auto")
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var cl struct {
		Claimed      int   `json:"claimed"`
		CreditGained int64 `json:"credit_gained"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cl); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if cl.Claimed != 1 || cl.CreditGained != 100 {
		t.Fatalf("claimed/credit_gained = %d/%d, want 1/100", cl.Claimed, cl.CreditGained)
	}
	if want := "/activity/growth/tasks/automation_1/claim"; stub.seen(want) != 1 {
		t.Fatalf("claim upstream calls for %s = %d, want 1", want, stub.seen(want))
	}

	// Unknown account through the real store → 404 (no upstream contact:
	// the stub would count a list call, and none fires).
	before := stub.seen("/v2/activity/growth/tasks")
	rec = growthDo(t, mux, http.MethodGet, "/api/accounts/workbuddy:ghost/growth-tasks")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account: got %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	if stub.seen("/v2/activity/growth/tasks") != before {
		t.Fatal("unknown account must fail at the credential lookup, before any upstream call")
	}

	// Upstream 500 passes through as 502 with the reason shown.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	t.Cleanup(failSrv.Close)
	failing := newWBGrowthBackend(h.Creds, workbuddy.TaskClientOptions{
		ChatBaseURL:    failSrv.URL,
		BillingBaseURL: failSrv.URL,
		WebBaseURL:     failSrv.URL,
	})
	rec = growthDo(t, newGrowthMux(failing), http.MethodGet, "/api/accounts/workbuddy:wb-growth-1/growth-tasks")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream 500: got %d, want 502 (body %s)", rec.Code, rec.Body)
	}
}
