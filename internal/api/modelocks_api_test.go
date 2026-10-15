package api

// M9b GET /api/model-locks integration tests: frozen wire shape (snake_case
// keys verbatim), total_accounts joined from the pool (not the registry),
// earliest_unlock = the soonest unlock among a model's locked accounts,
// deterministic row order, {"locks":[]} (never null) when nothing is locked,
// and the withAPIKey/405 surface discipline. Tests always inject an isolated
// router.ModelLockRegistry — never router.DefaultModelLocks.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/pool"
	"all2api/internal/router"
)

// locksMux builds the /api/model-locks route exactly as api.Register wires it,
// but against an isolated registry.
func locksMux(h *host.Host, locks *router.ModelLockRegistry) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/model-locks", withAPIKey(handleModelLocks(h, locks)))
	return mux
}

func getLocks(t *testing.T, mux *http.ServeMux, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/model-locks", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func addPoolAccount(t *testing.T, h *host.Host, platform, id string) {
	t.Helper()
	h.Pool.AddAccount(&pool.Account{ID: id, Platform: platform, Status: "active", Credit: 1000, Health: 1.0})
}

func TestModelLocksWireShapeVerbatim(t *testing.T) {
	h := newTestHost(t)
	for _, id := range []string{"a1", "a2", "a3", "a4"} {
		addPoolAccount(t, h, "workbuddy", id)
	}
	locks := router.NewModelLockRegistry()
	locks.NoteRateLimited("deepseek-v4-flash", "workbuddy", "a1", time.Minute)
	locks.NoteRateLimited("deepseek-v4-flash", "workbuddy", "a2", 30*time.Second)

	rec := getLocks(t, locksMux(h, locks), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Key check stays on the raw map so a renamed wire key fails loudly —
	// the contract is frozen and the frontend develops in parallel.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	rows, ok := raw["locks"]
	if !ok {
		t.Fatalf("missing top-level key locks: %s", rec.Body.String())
	}
	if len(raw) != 1 {
		t.Fatalf("top-level must carry only locks, got %d keys", len(raw))
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(rows, &arr); err != nil {
		t.Fatalf("locks must be an array: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("want 1 lock row, got %d", len(arr))
	}
	row := arr[0]
	for _, key := range []string{"model", "platform", "locked_accounts", "total_accounts", "earliest_unlock", "reason"} {
		if _, ok := row[key]; !ok {
			t.Fatalf("row missing frozen wire key %q: %s", key, rows)
		}
	}
	if len(row) != 6 {
		t.Fatalf("row must carry exactly the 6 frozen keys, got %d: %s", len(row), rows)
	}
	if string(row["model"]) != `"deepseek-v4-flash"` || string(row["platform"]) != `"workbuddy"` {
		t.Fatalf("model/platform mismatch: %s", rows)
	}
	if string(row["locked_accounts"]) != "2" || string(row["total_accounts"]) != "4" {
		t.Fatalf("counts mismatch (locked 2 of pool 4): %s", rows)
	}
	if string(row["reason"]) != `"429 rate limit"` {
		t.Fatalf("reason must be the frozen %q, got %s", router.ModelLockReason, row["reason"])
	}
	var eu map[string]json.RawMessage
	if err := json.Unmarshal(row["earliest_unlock"], &eu); err != nil {
		t.Fatalf("earliest_unlock must be an object: %v", err)
	}
	if _, ok := eu["unix"]; !ok {
		t.Fatalf("earliest_unlock missing unix: %s", row["earliest_unlock"])
	}
	var local string
	if err := json.Unmarshal(eu["local"], &local); err != nil || local == "" {
		t.Fatalf("earliest_unlock.local must be a non-empty string: %s", row["earliest_unlock"])
	}
	// The earliest unlock is the 30s-hint lock (a2), not the 1m one.
	var typed struct {
		Locks []struct {
			EarliestUnlock struct {
				Unix int64 `json:"unix"`
			} `json:"earliest_unlock"`
		} `json:"locks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &typed); err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	if delta := time.Until(time.Unix(typed.Locks[0].EarliestUnlock.Unix, 0)); delta > 35*time.Second || delta < 25*time.Second {
		t.Fatalf("earliest unlock should be ~30s out (the a2 hint), got %v", delta)
	}
}

func TestModelLocksSortedRowsAndCounts(t *testing.T) {
	h := newTestHost(t)
	addPoolAccount(t, h, "workbuddy", "wb1")
	addPoolAccount(t, h, "qoder", "q1")
	addPoolAccount(t, h, "qoder", "q2")
	locks := router.NewModelLockRegistry()
	locks.NoteRateLimited("m2", "workbuddy", "wb1", 0)
	locks.NoteRateLimited("m1", "qoder", "q1", 0)
	locks.NoteRateLimited("m1", "qoder", "q2", 0)

	rec := getLocks(t, locksMux(h, locks), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Locks []struct {
			Model          string `json:"model"`
			Platform       string `json:"platform"`
			LockedAccounts int    `json:"locked_accounts"`
			TotalAccounts  int    `json:"total_accounts"`
		} `json:"locks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Locks) != 2 {
		t.Fatalf("want 2 rows, got %+v", resp.Locks)
	}
	if resp.Locks[0].Model != "m1" || resp.Locks[0].Platform != "qoder" || resp.Locks[0].LockedAccounts != 2 || resp.Locks[0].TotalAccounts != 2 {
		t.Fatalf("row0 = %+v, want m1/qoder 2 locked of 2", resp.Locks[0])
	}
	if resp.Locks[1].Model != "m2" || resp.Locks[1].Platform != "workbuddy" || resp.Locks[1].LockedAccounts != 1 || resp.Locks[1].TotalAccounts != 1 {
		t.Fatalf("row1 = %+v, want m2/workbuddy 1 locked of 1", resp.Locks[1])
	}
}

func TestModelLocksEmptyIsEmptyArrayNotMethodPost(t *testing.T) {
	h := newTestHost(t)
	locks := router.NewModelLockRegistry()

	rec := getLocks(t, locksMux(h, locks), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "{\"locks\":[]}\n" {
		t.Fatalf("empty registry must encode {\"locks\":[]}, got %q", got)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/model-locks", nil)
	recPost := httptest.NewRecorder()
	locksMux(h, locks).ServeHTTP(recPost, req)
	if recPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/model-locks = %d, want 405", recPost.Code)
	}
}

func TestModelLocksRequiresAPIKeyWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":0","api_key":"k1"}`), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "no-key.json"), []byte(`{"listen":":0"}`), 0o600); err != nil {
		t.Fatalf("write restore config: %v", err)
	}
	t.Cleanup(func() {
		// Restore an unauthenticated config for any later test in this package.
		_ = config.Load(filepath.Join(dir, "no-key.json"))
	})

	h := newTestHost(t)
	locks := router.NewModelLockRegistry()
	mux := locksMux(h, locks)

	if rec := getLocks(t, mux, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: got %d, want 401", rec.Code)
	}
	if rec := getLocks(t, mux, map[string]string{"X-API-Key": "k1"}); rec.Code != http.StatusOK {
		t.Fatalf("right key: got %d, want 200", rec.Code)
	}
}
