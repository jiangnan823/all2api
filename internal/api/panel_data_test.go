package api

// M5a panel-data endpoint tests: wire shape and aggregation correctness,
// empty pool / empty task list, ring overflow (>500 lines keeps the newest),
// the withAPIKey 401 style on all four endpoints, and the noctx red line
// (every request goes through httptest.NewRequestWithContext).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/observability"
	"all2api/internal/pool"
	"all2api/internal/schedule"
)

// --- fixtures ---

// stubTaskSource is the deterministic TaskSource for /api/tasks tests.
type stubTaskSource struct {
	tasks []*schedule.Task
}

func (s *stubTaskSource) GetTasks() []*schedule.Task { return s.tasks }

func newPanelMux(t *testing.T, h *host.Host, sched TaskSource, stats *schedule.StatsMemory, ring *observability.Ring) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	RegisterPanelData(mux, h, sched, stats, ring)
	return mux
}

func panelGet(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func loadM5Config(t *testing.T, body string) {
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
		// Restore an unauthenticated default config for later tests.
		_ = config.Load(filepath.Join(dir, "no-key.json"))
		_ = os.WriteFile(filepath.Join(dir, "no-key.json"), []byte(`{"listen":":0"}`), 0o600)
		_ = config.Load(filepath.Join(dir, "no-key.json"))
	})
}

// --- GET /api/accounts ---

func TestAccountsShapeAndAggregation(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "qa")
	now := time.Now()
	h.Pool.AddAccount(&pool.Account{
		ID: "a1", Platform: "qa", Status: "active",
		Credit: 500, Health: 0.9, LastSelected: now.Add(-time.Hour),
	})
	h.Pool.AddAccount(&pool.Account{
		ID: "a2", Platform: "qa", Status: "cooling",
		Credit: 200, Health: 0.5, CooldownEnd: now.Add(30 * time.Minute),
	})
	h.Pool.AddAccount(&pool.Account{
		ID: "a3", Platform: "qa", Status: "disabled",
		Credit: 0, Health: 0,
	})
	// An unknown status must not silently inflate any bucket (still counted
	// in total and credit_total).
	h.Pool.AddAccount(&pool.Account{
		ID: "a4", Platform: "qa", Status: "elsewhere", Credit: 42,
	})

	mux := newPanelMux(t, h, nil, nil, nil)
	rec := panelGet(t, mux, "/api/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	// Raw wire keys first: the frontend batch aligns against these names.
	body := rec.Body.String()
	for _, key := range []string{
		`"accounts"`, `"platform"`, `"id"`, `"status"`, `"credit"`, `"health"`,
		`"expires_at"`, `"cooldown_end"`, `"last_selected"`,
		`"total"`, `"active"`, `"cooling"`, `"disabled"`, `"credit_total"`,
		`"unix"`, `"local"`,
	} {
		if !strings.Contains(body, key) {
			t.Fatalf("wire key %s missing from /api/accounts body: %s", key, body)
		}
	}

	var resp struct {
		Accounts []struct {
			Platform     string             `json:"platform"`
			ID           string             `json:"id"`
			Status       string             `json:"status"`
			Credit       int64              `json:"credit"`
			Health       float64            `json:"health"`
			ExpiresAt    *adapter.TimeLocal `json:"expires_at"`
			CooldownEnd  *adapter.TimeLocal `json:"cooldown_end"`
			LastSelected *adapter.TimeLocal `json:"last_selected"`
		} `json:"accounts"`
		Total       int   `json:"total"`
		Active      int   `json:"active"`
		Cooling     int   `json:"cooling"`
		Disabled    int   `json:"disabled"`
		CreditTotal int64 `json:"credit_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode accounts response: %v", err)
	}
	if resp.Total != 4 || resp.Active != 1 || resp.Cooling != 1 || resp.Disabled != 1 {
		t.Fatalf("aggregation drift: total=%d active=%d cooling=%d disabled=%d, want 4/1/1/1",
			resp.Total, resp.Active, resp.Cooling, resp.Disabled)
	}
	if resp.CreditTotal != 742 {
		t.Fatalf("credit_total = %d, want 742 (500+200+0+42)", resp.CreditTotal)
	}
	if len(resp.Accounts) != 4 {
		t.Fatalf("accounts array = %d entries, want 4", len(resp.Accounts))
	}

	byID := map[string]struct {
		status      string
		cooldownSet bool
		lastSelSet  bool
	}{}
	for _, a := range resp.Accounts {
		byID[a.ID] = struct {
			status      string
			cooldownSet bool
			lastSelSet  bool
		}{a.Status, a.CooldownEnd != nil, a.LastSelected != nil}
	}
	a1 := byID["a1"]
	if a1.status != "active" || !a1.lastSelSet || a1.cooldownSet {
		t.Fatalf("a1 shape wrong: %+v", a1)
	}
	a2 := byID["a2"]
	if a2.status != "cooling" || !a2.cooldownSet {
		t.Fatalf("a2 shape wrong: %+v", a2)
	}
	a3 := byID["a3"]
	if a3.status != "disabled" {
		t.Fatalf("a3 shape wrong: %+v", a3)
	}
	// Zero times must be null on the wire, never "0001-01-01...".
	if !strings.Contains(body, `"expires_at":null`) {
		t.Fatalf("zero expires_at must serialize as null: %s", body)
	}
}

func TestAccountsEmptyPool(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t) // no sources, no accounts
	mux := newPanelMux(t, h, nil, nil, nil)
	rec := panelGet(t, mux, "/api/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts (empty pool): got %d, want 200", rec.Code)
	}
	var resp accountsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accounts == nil || len(resp.Accounts) != 0 {
		t.Fatalf("empty pool must deliver [] not %v", resp.Accounts)
	}
	if resp.Total != 0 || resp.Active != 0 || resp.Cooling != 0 || resp.Disabled != 0 || resp.CreditTotal != 0 {
		t.Fatalf("empty aggregation must be zeroed: %+v", resp)
	}
	// nil host must degrade to the same honest empty shape.
	rec = panelGet(t, newPanelMux(t, nil, nil, nil, nil), "/api/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts (nil host): got %d, want 200", rec.Code)
	}
}

// --- GET /api/tasks ---

func TestTasksShapeWithLastRun(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	sched := &stubTaskSource{tasks: []*schedule.Task{
		{Name: "signin", CronSpec: "0 9,21 * * *", Enabled: true},
		{Name: "active_check", CronSpec: "0 10 * * *", Enabled: false},
	}}
	stats := schedule.NewStatsMemory()
	stats.Recorder(nil)(schedule.RunStats{Task: "signin", Total: 3, Success: 2, Failed: 1, Skipped: 0})

	mux := newPanelMux(t, nil, sched, stats, nil)
	rec := panelGet(t, mux, "/api/tasks")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	body := rec.Body.String()
	for _, key := range []string{`"tasks"`, `"name"`, `"cron"`, `"enabled"`, `"last_run"`, `"total"`, `"success"`, `"failed"`, `"skipped"`, `"at"`} {
		if !strings.Contains(body, key) {
			t.Fatalf("wire key %s missing from /api/tasks body: %s", key, body)
		}
	}

	var resp struct {
		Tasks []struct {
			Name    string `json:"name"`
			Cron    string `json:"cron"`
			Enabled bool   `json:"enabled"`
			LastRun *struct {
				Name    string            `json:"name"`
				Total   int               `json:"total"`
				Success int               `json:"success"`
				Failed  int               `json:"failed"`
				Skipped int               `json:"skipped"`
				At      adapter.TimeLocal `json:"at"`
			} `json:"last_run"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tasks response: %v", err)
	}
	if len(resp.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(resp.Tasks))
	}
	// Deterministic (sorted) delivery: active_check first.
	if resp.Tasks[0].Name != "active_check" || resp.Tasks[0].LastRun != nil {
		t.Fatalf("active_check must sort first and have last_run=null: %+v", resp.Tasks[0])
	}
	if resp.Tasks[1].Name != "signin" {
		t.Fatalf("tasks must be delivered name-sorted: %+v", resp.Tasks)
	}
	lr := resp.Tasks[1].LastRun
	if lr == nil || lr.Total != 3 || lr.Success != 2 || lr.Failed != 1 || lr.Skipped != 0 {
		t.Fatalf("signin last_run counters wrong: %+v", lr)
	}
	if lr.At.Unix == 0 || !strings.Contains(body, `"at":{"unix"`) {
		t.Fatalf("last_run.at must carry the Q9 dual time: %+v", lr.At)
	}
}

func TestTasksEmptyAndNeverRun(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	// Empty task list.
	rec := panelGet(t, newPanelMux(t, nil, &stubTaskSource{}, schedule.NewStatsMemory(), nil), "/api/tasks")
	var empty tasksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if empty.Tasks == nil || len(empty.Tasks) != 0 {
		t.Fatalf("empty scheduler must deliver [] not %v", empty.Tasks)
	}
	// Registered task that never ran: last_run must be JSON null.
	sched := &stubTaskSource{tasks: []*schedule.Task{{Name: "cat_travel", CronSpec: "0 9,21 * * *", Enabled: true}}}
	rec = panelGet(t, newPanelMux(t, nil, sched, schedule.NewStatsMemory(), nil), "/api/tasks")
	if !strings.Contains(rec.Body.String(), `"last_run":null`) {
		t.Fatalf("never-run task must carry last_run null: %s", rec.Body)
	}
	// nil scheduler degrades to the same empty shape.
	rec = panelGet(t, newPanelMux(t, nil, nil, nil, nil), "/api/tasks")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tasks":[]`) {
		t.Fatalf("nil scheduler must degrade to {tasks:[]}: %d %s", rec.Code, rec.Body)
	}
}

// --- GET /api/models ---

func TestModelsLocalCatalogs(t *testing.T) {
	loadM5Config(t, `{"listen":":0","model_map":{"deepseek-v4-flash":"workbuddy/deepseek-v4-flash","custom-model":"workbuddy/custom-model"}}`)

	rec := panelGet(t, newPanelMux(t, nil, nil, nil, nil), "/api/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, key := range []string{`"source":"local"`, `"models"`, `"provider"`, `"id"`, `"name"`, `"context_size"`, `"supports_tool"`, `"supports_stream"`, `"is_free"`} {
		if !strings.Contains(body, key) {
			t.Fatalf("wire key %s missing from /api/models body: %s", key, body)
		}
	}

	var resp modelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	if resp.Source != "local" {
		t.Fatalf("source must be the honesty marker %q, got %q", "local", resp.Source)
	}

	qoder := 0
	workbuddy := 0
	for _, m := range resp.Models {
		switch m.Provider {
		case "qoder":
			qoder++
			if !strings.HasPrefix(m.ID, "qoder/") {
				t.Fatalf("qoder model id must be namespaced: %+v", m)
			}
		case "workbuddy":
			workbuddy++
			if !strings.HasPrefix(m.ID, "workbuddy/") {
				t.Fatalf("workbuddy model id must be namespaced: %+v", m)
			}
		default:
			t.Fatalf("unexpected provider %q", m.Provider)
		}
	}
	// The qoder static table has 13 entries (chat.go defaultModelMap).
	if qoder != 13 {
		t.Fatalf("qoder catalog = %d entries, want 13 (static table)", qoder)
	}
	// workbuddy: model_map entries first (custom-model), defaults deduped
	// (deepseek-v4-flash appears once even though it is in both the map and
	// the default list) → 1 custom + 6 defaults = 7.
	if workbuddy != 7 {
		t.Fatalf("workbuddy catalog = %d entries, want 7 (map 2 with 1 deduped + 6 defaults): %+v", workbuddy, resp.Models)
	}
	// Deterministic order: provider asc, then id asc → qoder block first.
	if resp.Models[0].Provider != "qoder" {
		t.Fatalf("models must be delivered provider-sorted (qoder < workbuddy): %+v", resp.Models[0])
	}
}

// --- GET /api/logs ---

func TestLogsRingServed(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	ring := observability.NewRing(observability.DefaultRingCapacity)
	ring.Append(observability.Entry{TS: time.Now(), Level: observability.LevelInfo, Msg: "credit seed: qa/a1 = 500 credits"})
	ring.Append(observability.Entry{TS: time.Now(), Level: observability.LevelError, Msg: "credit seed: qa/a2: boom"})

	rec := panelGet(t, newPanelMux(t, nil, nil, nil, ring), "/api/logs")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, key := range []string{`"logs"`, `"level"`, `"msg"`, `"ts"`, `"info"`, `"error"`} {
		if !strings.Contains(body, key) {
			t.Fatalf("wire key %s missing from /api/logs body: %s", key, body)
		}
	}

	var resp logsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode logs response: %v", err)
	}
	if len(resp.Logs) != 2 {
		t.Fatalf("logs = %d, want 2", len(resp.Logs))
	}
	if resp.Logs[0].Level != "info" || resp.Logs[0].Msg != "credit seed: qa/a1 = 500 credits" {
		t.Fatalf("first entry wrong: %+v", resp.Logs[0])
	}
	if resp.Logs[1].Level != "error" || resp.Logs[1].Msg != "credit seed: qa/a2: boom" {
		t.Fatalf("second entry wrong: %+v", resp.Logs[1])
	}
	if resp.Logs[0].TS == nil || resp.Logs[0].TS.Unix == 0 {
		t.Fatalf("log ts must carry the Q9 dual time: %+v", resp.Logs[0].TS)
	}
}

func TestLogsRingOverflowKeepsNewest(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	cap := 8
	ring := observability.NewRing(cap)
	for i := 0; i < cap*3; i++ {
		ring.Append(observability.Entry{TS: time.Now(), Level: observability.LevelInfo, Msg: fmt.Sprintf("line-%d", i)})
	}
	got := ring.Snapshot()
	if len(got) != cap {
		t.Fatalf("ring holds %d entries, want capacity %d", len(got), cap)
	}
	if got[0].Msg != fmt.Sprintf("line-%d", cap*2) {
		t.Fatalf("oldest kept entry = %s, want line-%d (newest-first eviction)", got[0].Msg, cap*2)
	}
	if got[cap-1].Msg != fmt.Sprintf("line-%d", cap*3-1) {
		t.Fatalf("newest entry = %s, want line-%d", got[cap-1].Msg, cap*3-1)
	}
}

func TestLogsEmptyAndNilRing(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	rec := panelGet(t, newPanelMux(t, nil, nil, nil, nil), "/api/logs")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"logs":[]`) {
		t.Fatalf("nil ring must degrade to {logs:[]}: %d %s", rec.Code, rec.Body)
	}
	rec = panelGet(t, newPanelMux(t, nil, nil, nil, observability.NewRing(4)), "/api/logs")
	if !strings.Contains(rec.Body.String(), `"logs":[]`) {
		t.Fatalf("empty ring must deliver []: %s", rec.Body)
	}
}

// --- guards and method routing ---

func TestPanelDataRequiresAPIKeyWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":0","api_key":"m5k"}`), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(dir, "no-key.json"), []byte(`{"listen":":0"}`), 0o600)
		_ = config.Load(filepath.Join(dir, "no-key.json"))
	})

	h := newTestHost(t)
	mux := newPanelMux(t, h, &stubTaskSource{}, schedule.NewStatsMemory(), observability.NewRing(4))

	for _, path := range []string{"/api/accounts", "/api/tasks", "/api/models", "/api/logs"} {
		rec := panelGet(t, mux, path)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without key: got %d, want 401 (429-endpoint error style)", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"error":"unauthorized"`) {
			t.Fatalf("GET %s 401 body must reuse the unified error shape: %s", path, rec.Body)
		}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		req.Header.Set("X-API-Key", "m5k")
		recOK := httptest.NewRecorder()
		mux.ServeHTTP(recOK, req)
		if recOK.Code != http.StatusOK {
			t.Fatalf("GET %s with key: got %d, want 200", path, recOK.Code)
		}
	}
}

func TestPanelDataMethodsRoutedTo405(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t)
	mux := newPanelMux(t, h, &stubTaskSource{}, schedule.NewStatsMemory(), observability.NewRing(4))
	for _, path := range []string{"/api/accounts", "/api/tasks", "/api/models", "/api/logs"} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s: got %d, want 405 from the method-specific pattern", path, rec.Code)
		}
	}
}
