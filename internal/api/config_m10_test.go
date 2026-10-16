package api

// M10-B2 config-surface tests: the new keys are delivered as editable
// FieldDefs, accepted (and validated) by POST /api/config, and reset by
// restore-defaults — the delivery/validation pair cannot drift because both
// read the same spec tables.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"all2api/internal/config"
)

func TestConfigFieldsDeliverM10Keys(t *testing.T) {
	saveEnv(t, `{"listen":":0"}`)
	mux := http.NewServeMux()
	Register(mux, nil)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/config/fields", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fields status = %d", rec.Code)
	}

	var groups []struct {
		Group  string `json:"group"`
		Fields []struct {
			Key  string `json:"key"`
			Type string `json:"type"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	delivered := map[string]string{}
	for _, g := range groups {
		for _, f := range g.Fields {
			delivered[f.Key] = f.Type
		}
	}
	// Sticky TTL: a number in the pooling group.
	if delivered["sticky_session_ttl_min"] != "number" {
		t.Fatalf("sticky_session_ttl_min delivered as %q, want number", delivered["sticky_session_ttl_min"])
	}
	// Every per-platform cron key delivered as cron, exactly once.
	wantCrons := []string{
		"schedule.signin_cron_workbuddy", "schedule.signin_cron_qoder", "schedule.signin_cron_trae",
		"schedule.task_cron_workbuddy", "schedule.task_cron_qoder", "schedule.task_cron_trae",
		"schedule.token_keepalive_workbuddy", "schedule.token_keepalive_qoder", "schedule.token_keepalive_trae",
		"schedule.cat_travel_cron",
	}
	for _, key := range wantCrons {
		if delivered[key] != "cron" {
			t.Fatalf("%s delivered as %q, want cron", key, delivered[key])
		}
		delete(delivered, key)
	}
	// Duplicates would mean a second delivery of the same key.
	for _, key := range wantCrons {
		for _, g := range groups {
			n := 0
			for _, f := range g.Fields {
				if f.Key == key {
					n++
				}
			}
			if n > 1 {
				t.Fatalf("%s delivered %d times in group %s", key, n, g.Group)
			}
		}
	}
}

func TestConfigSaveAcceptsM10Keys(t *testing.T) {
	path := saveEnv(t, `{"listen":":0"}`)
	apiKey := config.Shared.Get().APIKey // unset: open-LAN config, no header needed

	patch := `[
		{"key":"sticky_session_ttl_min","value":45},
		{"key":"schedule.signin_cron_workbuddy","value":"0 8 * * *"},
		{"key":"schedule.cat_travel_cron","value":"30 20 * * *"}
	]`
	rec := doPost(t, "/api/config", patch, map[string]string{"X-API-Key": apiKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d body %s", rec.Code, rec.Body)
	}

	cfg := config.Shared.Get()
	if cfg.StickySessionTTLMin != 45 {
		t.Fatalf("sticky ttl = %d, want 45", cfg.StickySessionTTLMin)
	}
	if cfg.Schedule.SigninCronWorkbuddy != "0 8 * * *" {
		t.Fatalf("signin_cron_workbuddy = %q", cfg.Schedule.SigninCronWorkbuddy)
	}
	if cfg.Schedule.CatTravelCron != "30 20 * * *" {
		t.Fatalf("cat_travel_cron = %q", cfg.Schedule.CatTravelCron)
	}
	// The raw passthrough (GET /api/config) must carry the new keys too —
	// the panel renders them from the same document.
	if !strings.Contains(rec.Body.String(), `"sticky_session_ttl_min":45`) {
		t.Fatalf("applied payload missing sticky key: %s", rec.Body)
	}
	_ = path
}

func TestConfigSaveM10Validation(t *testing.T) {
	saveEnv(t, `{"listen":":0"}`)
	apiKey := config.Shared.Get().APIKey

	cases := []struct {
		name string
		body string
	}{
		{"bad cron rejected", `[{"key":"schedule.signin_cron_qoder","value":"0 25 * * *"}]`},
		{"unknown key still rejected", `[{"key":"schedule.signin_cron_nosuch","value":"0 8 * * *"}]`},
		{"sticky range floor", `[{"key":"sticky_session_ttl_min","value":-1}]`},
		{"sticky range ceiling", `[{"key":"sticky_session_ttl_min","value":1441}]`},
		{"sticky must be integral", `[{"key":"sticky_session_ttl_min","value":1.5}]`},
	}
	for _, tc := range cases {
		rec := doPost(t, "/api/config", tc.body, map[string]string{"X-API-Key": apiKey})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %s)", tc.name, rec.Code, rec.Body)
		}
	}
	// The empty-string cron is the documented fallback reset — it saves.
	rec := doPost(t, "/api/config",
		`[{"key":"schedule.signin_cron_workbuddy","value":"0 8 * * *"}]`,
		map[string]string{"X-API-Key": apiKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("baseline save failed: %d %s", rec.Code, rec.Body)
	}
	rec = doPost(t, "/api/config",
		`[{"key":"schedule.signin_cron_workbuddy","value":""}]`,
		map[string]string{"X-API-Key": apiKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty platform cron (fallback reset) must save: %d %s", rec.Code, rec.Body)
	}
	if got := config.Shared.Get().Schedule.SigninCronWorkbuddy; got != "" {
		t.Fatalf("reset platform cron = %q, want empty", got)
	}
	// 0 stays a legal sticky value (0=关).
	rec = doPost(t, "/api/config", `[{"key":"sticky_session_ttl_min","value":0}]`,
		map[string]string{"X-API-Key": apiKey})
	if rec.Code != http.StatusOK || config.Shared.Get().StickySessionTTLMin != 0 {
		t.Fatalf("sticky 0 (off) must save: %d %s", rec.Code, rec.Body)
	}
}

func TestRestoreDefaultsResetsM10Keys(t *testing.T) {
	mux, _ := newDefaultsTestEnv(t)
	// Seed non-default M10 values.
	rec := doPost(t, "/api/config",
		`[{"key":"sticky_session_ttl_min","value":0},
		   {"key":"schedule.signin_cron_workbuddy","value":"0 6 * * *"},
		   {"key":"schedule.cat_travel_cron","value":"0 23 * * *"}]`,
		map[string]string{"X-API-Key": "sk-test"})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed save failed: %d %s", rec.Code, rec.Body)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/config/restore-defaults", nil)
	req.Header.Set("X-API-Key", "sk-test")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d %s", rec.Code, rec.Body)
	}

	cfg := config.Shared.Get()
	if cfg.StickySessionTTLMin != 30 {
		t.Fatalf("sticky ttl after restore = %d, want the 30 default", cfg.StickySessionTTLMin)
	}
	if cfg.Schedule.SigninCronWorkbuddy != "" || cfg.Schedule.CatTravelCron != "" {
		t.Fatalf("platform crons after restore = %q/%q, want empty (global fallback)",
			cfg.Schedule.SigninCronWorkbuddy, cfg.Schedule.CatTravelCron)
	}
}

func TestDefaultsCarryM10Baseline(t *testing.T) {
	cfg := config.Defaults()
	if cfg.StickySessionTTLMin != 30 {
		t.Fatalf("default sticky ttl = %d, want 30", cfg.StickySessionTTLMin)
	}
	// Per-platform keys default empty = frozen-global fallback.
	if cfg.Schedule.SigninCronWorkbuddy != "" || cfg.Schedule.TaskCronQoder != "" ||
		cfg.Schedule.TokenKeepaliveTrae != "" || cfg.Schedule.CatTravelCron != "" {
		t.Fatalf("platform cron defaults must be empty: %+v", cfg.Schedule)
	}
}
