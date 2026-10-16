package api

// M10-B2 snapshot extension tests: the three new top-level aggregates
// (paused_count / sticky_sessions / credit_sum), the per-account status_note
// column, and the /api/tasks "platform" wire field.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"all2api/internal/host"
	"all2api/internal/pool"
	"all2api/internal/router"
	"all2api/internal/schedule"
)

func TestAccountsSnapshotM10Aggregates(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "qa")
	h.Creds = host.NewCredentialStore(h.Pool)

	now := time.Now()
	h.Pool.AddAccount(&pool.Account{ID: "a1", Platform: "qa", Status: "active", Credit: 500, CreditTotal: 800, Health: 0.9})
	h.Pool.AddAccount(&pool.Account{ID: "a2", Platform: "qa", Status: "cooling",
		Credit: 200, CreditTotal: 400, Health: 0.5, CooldownEnd: now.Add(30 * time.Minute), CooldownReason: pool.CooldownReasonRateLimit})
	h.Pool.AddAccount(&pool.Account{ID: "a3", Platform: "qa", Status: "disabled", Credit: 0, Paused: true})
	// A non-429 cooldown renders the task-failure phrase.
	h.Pool.AddAccount(&pool.Account{ID: "a4", Platform: "qa", Status: "cooling",
		Credit: 300, CooldownEnd: now.Add(time.Minute), CooldownReason: pool.CooldownReasonTaskFailed})

	// One live sticky binding counts. The registry is isolated first (the
	// isolateLedger precedent): other tests' /v1 traffic may have left
	// bindings in the process-global DefaultSticky, and the assertion wants
	// an exact count.
	oldSticky := router.DefaultSticky
	router.DefaultSticky = router.NewStickyRegistry()
	t.Cleanup(func() { router.DefaultSticky = oldSticky })
	router.DefaultSticky.Bind("caller-a", "m1", "qa", "a1", time.Minute)

	rec := panelGet(t, newPanelMux(t, h, nil, nil, nil), "/api/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts: %d (body %s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, key := range []string{`"paused_count"`, `"sticky_sessions"`, `"credit_sum"`, `"status_note"`} {
		if !strings.Contains(body, key) {
			t.Fatalf("M10 wire key %s missing: %s", key, body)
		}
	}

	var resp struct {
		Accounts []struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			StatusNote string `json:"status_note"`
		} `json:"accounts"`
		PausedCount    int   `json:"paused_count"`
		StickySessions int   `json:"sticky_sessions"`
		CreditSum      int64 `json:"credit_sum"`
		CreditTotal    int64 `json:"credit_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	notes := map[string]string{}
	for _, a := range resp.Accounts {
		notes[a.ID] = a.StatusNote
	}
	if got := notes["a2"]; got != "429 rate limit" {
		t.Fatalf("a2 status_note = %q, want the frozen 429 phrase", got)
	}
	if got := notes["a4"]; got != "task failed" {
		t.Fatalf("a4 status_note = %q, want the task-failure phrase", got)
	}
	if got := notes["a1"]; got != "" {
		t.Fatalf("a1 status_note = %q, want empty (never cooled)", got)
	}
	if resp.PausedCount != 1 {
		t.Fatalf("paused_count = %d, want 1", resp.PausedCount)
	}
	if resp.StickySessions != 1 {
		t.Fatalf("sticky_sessions = %d, want 1 (only live bindings count)", resp.StickySessions)
	}
	// credit_sum = the remaining-credit sum; credit_total = the paired
	// capacity sum (积分剩余/总额 double value), seeded 800+400 above.
	if resp.CreditSum != 1000 {
		t.Fatalf("credit_sum = %d, want 1000", resp.CreditSum)
	}
	if resp.CreditTotal != 1200 {
		t.Fatalf("credit_total = %d, want 1200 (capacity sum, not remain)", resp.CreditTotal)
	}
}

func TestTasksWireCarriesPlatform(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	sched := &stubTaskSource{tasks: []*schedule.Task{
		{Name: "signin@workbuddy", Source: "workbuddy", CronSpec: "0 9,21 * * *", Enabled: true},
		{Name: "signin@qoder", Source: "qoder", CronSpec: "0 9,21 * * *", Enabled: true},
		{Name: "cat_travel@workbuddy", Source: "workbuddy", CronSpec: "0 9,21 * * *", Enabled: true},
	}}

	rec := panelGet(t, newPanelMux(t, nil, sched, nil, nil), "/api/tasks")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"platform"`) {
		t.Fatalf("M10 platform key missing from /api/tasks: %s", rec.Body)
	}
	var resp struct {
		Tasks []struct {
			Name     string `json:"name"`
			Platform string `json:"platform"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]string{}
	for _, task := range resp.Tasks {
		byName[task.Name] = task.Platform
	}
	if byName["signin@workbuddy"] != "workbuddy" || byName["cat_travel@workbuddy"] != "workbuddy" {
		t.Fatalf("workbuddy tasks lost their platform: %+v", byName)
	}
	if byName["signin@qoder"] != "qoder" {
		t.Fatalf("qoder task platform = %q", byName["signin@qoder"])
	}
}
