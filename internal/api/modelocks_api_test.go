package api

// M9b GET /api/model-locks integration tests: frozen wire shape (snake_case
// keys verbatim), total_accounts joined from the pool (not the registry),
// earliest_unlock = the soonest unlock among a model's locked accounts,
// deterministic row order, {"locks":[]} (never null) when nothing is locked,
// and the withAPIKey/405 surface discipline. M10-B1 adds the scope/available/
// total keys (availability joined inside the registry via BindPool) and the
// POST /api/model-locks/unlock endpoint. Tests always inject an isolated
// router.ModelLockRegistry — never router.DefaultModelLocks.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/pool"
	"all2api/internal/router"
)

// locksMux builds the /api/model-locks surface exactly as api.Register wires
// it (GET aggregate view + POST unlock), but against an isolated registry.
func locksMux(h *host.Host, locks *router.ModelLockRegistry) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/model-locks", withAPIKey(handleModelLocks(h, locks)))
	mux.HandleFunc("POST /api/model-locks/unlock", withAPIKey(handleModelLockUnlock(locks)))
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
	for _, key := range []string{"model", "scope", "platform", "locked_accounts", "total_accounts", "available", "total", "earliest_unlock", "reason"} {
		if _, ok := row[key]; !ok {
			t.Fatalf("row missing frozen wire key %q: %s", key, rows)
		}
	}
	if len(row) != 9 {
		t.Fatalf("row must carry exactly the 9 frozen keys (M9b 6 + M10 scope/available/total), got %d: %s", len(row), rows)
	}
	if string(row["model"]) != `"deepseek-v4-flash"` || string(row["platform"]) != `"workbuddy"` {
		t.Fatalf("model/platform mismatch: %s", rows)
	}
	if string(row["scope"]) != `"workbuddy"` {
		t.Fatalf("scope must be the platform domain, got %s", row["scope"])
	}
	if string(row["locked_accounts"]) != "2" || string(row["total_accounts"]) != "4" {
		t.Fatalf("counts mismatch (locked 2 of pool 4): %s", rows)
	}
	// M10 availability join: a1/a2 are locked for this model, a3/a4 remain
	// selectable → available 2, total = available + locked = 4.
	if string(row["available"]) != "2" || string(row["total"]) != "4" {
		t.Fatalf("availability mismatch (2 free of 4 relevant): %s", rows)
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
			Scope          string `json:"scope"`
			LockedAccounts int    `json:"locked_accounts"`
			TotalAccounts  int    `json:"total_accounts"`
			Available      int    `json:"available"`
			Total          int    `json:"total"`
		} `json:"locks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Locks) != 2 {
		t.Fatalf("want 2 rows, got %+v", resp.Locks)
	}
	if resp.Locks[0].Model != "m1" || resp.Locks[0].Platform != "qoder" || resp.Locks[0].Scope != "qoder" || resp.Locks[0].LockedAccounts != 2 || resp.Locks[0].TotalAccounts != 2 {
		t.Fatalf("row0 = %+v, want m1/qoder 2 locked of 2", resp.Locks[0])
	}
	// Both qoder accounts are locked for m1 → nothing selectable, total = 0+2.
	if resp.Locks[0].Available != 0 || resp.Locks[0].Total != 2 {
		t.Fatalf("row0 availability = %d/%d, want 0/2", resp.Locks[0].Available, resp.Locks[0].Total)
	}
	if resp.Locks[1].Model != "m2" || resp.Locks[1].Platform != "workbuddy" || resp.Locks[1].LockedAccounts != 1 || resp.Locks[1].TotalAccounts != 1 {
		t.Fatalf("row1 = %+v, want m2/workbuddy 1 locked of 1", resp.Locks[1])
	}
	if resp.Locks[1].Available != 0 || resp.Locks[1].Total != 1 {
		t.Fatalf("row1 availability = %d/%d, want 0/1", resp.Locks[1].Available, resp.Locks[1].Total)
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

// TestModelLocksAvailabilityJoinStates pins the M10 availability semantics:
// only accounts the pool would select right now (active, not paused, not
// cooling) count toward available, and an account this model has locked is
// subtracted from it — total = available + locked_accounts. Accounts that are
// cooling/paused/disabled belong to neither column.
func TestModelLocksAvailabilityJoinStates(t *testing.T) {
	h := newTestHost(t)
	addPoolAccount(t, h, "workbuddy", "a1") // active, locked for m1
	addPoolAccount(t, h, "workbuddy", "a2") // active, free
	h.Pool.AddAccount(&pool.Account{ID: "a3", Platform: "workbuddy", Status: "active", Credit: 1000,
		CooldownEnd: time.Now().Add(5 * time.Minute)}) // cooling
	h.Pool.AddAccount(&pool.Account{ID: "a4", Platform: "workbuddy", Status: "active", Credit: 1000, Paused: true})
	h.Pool.AddAccount(&pool.Account{ID: "a5", Platform: "workbuddy", Status: "disabled", Credit: 1000})
	addPoolAccount(t, h, "qoder", "q1") // other platform: never joins workbuddy rows

	locks := router.NewModelLockRegistry()
	locks.NoteRateLimited("m1", "workbuddy", "a1", time.Minute)

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
			Available      int    `json:"available"`
			Total          int    `json:"total"`
		} `json:"locks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Locks) != 1 {
		t.Fatalf("want 1 row, got %+v", resp.Locks)
	}
	row := resp.Locks[0]
	if row.LockedAccounts != 1 || row.Available != 1 || row.Total != 2 {
		t.Fatalf("availability = locked %d / available %d / total %d, want 1/1/2", row.LockedAccounts, row.Available, row.Total)
	}
	if row.TotalAccounts != 5 {
		t.Fatalf("total_accounts = %d, want 5 (raw pool size, not the availability join)", row.TotalAccounts)
	}
}

// ---------- POST /api/model-locks/unlock ----------

func postUnlock(t *testing.T, mux *http.ServeMux, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/model-locks/unlock", strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestModelLockUnlockSingleModel(t *testing.T) {
	h := newTestHost(t)
	addPoolAccount(t, h, "workbuddy", "a1")
	addPoolAccount(t, h, "workbuddy", "a2")
	locks := router.NewModelLockRegistry()
	locks.NoteRateLimited("m1", "workbuddy", "a1", time.Minute)
	locks.NoteRateLimited("m1", "workbuddy", "a2", time.Minute)
	locks.NoteRateLimited("m2", "workbuddy", "a1", time.Minute)
	mux := locksMux(h, locks)

	rec := postUnlock(t, mux, `{"model":"m1"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s, want 200", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != "{\"unlocked\":2}\n" {
		t.Fatalf("body = %q, want {\"unlocked\":2}", got)
	}
	if locks.Locked("m1", "workbuddy", "a1") || locks.Locked("m1", "workbuddy", "a2") {
		t.Fatal("both m1 locks must be gone after the m1 unlock")
	}
	if !locks.Locked("m2", "workbuddy", "a1") {
		t.Fatal("the m2 lock must survive an m1-scoped unlock")
	}
}

func TestModelLockUnlockWholePool(t *testing.T) {
	h := newTestHost(t)
	locks := router.NewModelLockRegistry()
	mux := locksMux(h, locks)

	// {} and {"model":""} and a bodyless POST all mean "unlock everything";
	// each iteration re-books the same two locks (the previous one emptied the
	// registry by definition).
	for _, body := range []string{`{}`, `{"model":""}`, ``} {
		locks.NoteRateLimited("m1", "workbuddy", "a1", time.Minute)
		locks.NoteRateLimited("m2", "qoder", "q1", time.Minute)
		rec := postUnlock(t, mux, body, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %q: status = %d, want 200", body, rec.Code)
		}
		if got := rec.Body.String(); got != "{\"unlocked\":2}\n" {
			t.Fatalf("POST %q: body = %q, want {\"unlocked\":2}", body, got)
		}
		if n := len(locks.Snapshot()); n != 0 {
			t.Fatalf("whole-pool unlock left %d rows", n)
		}
	}

	// Unlocking an empty pool is an honest 0, never an error.
	rec := postUnlock(t, mux, `{}`, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"unlocked\":0}\n" {
		t.Fatalf("empty-pool unlock = %d %s, want 200 {\"unlocked\":0}", rec.Code, rec.Body)
	}
}

func TestModelLockUnlockSurfaceDiscipline(t *testing.T) {
	h := newTestHost(t)
	locks := router.NewModelLockRegistry()
	mux := locksMux(h, locks)

	// GET on the unlock endpoint is 405 (method-specific pattern).
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/model-locks/unlock", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/model-locks/unlock = %d, want 405", rec.Code)
	}

	// Malformed JSON is 400 — never silently treated as whole-pool unlock.
	if rec := postUnlock(t, mux, `not-json`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", rec.Code)
	}

	// The endpoint sits behind withAPIKey like the rest of the panel surface.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":0","api_key":"k1"}`), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(dir, "no-key.json"), []byte(`{"listen":":0"}`), 0o600)
		_ = config.Load(filepath.Join(dir, "no-key.json"))
	})
	if rec := postUnlock(t, mux, `{}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unlock = %d, want 401", rec.Code)
	}
	if rec := postUnlock(t, mux, `{}`, map[string]string{"X-API-Key": "k1"}); rec.Code != http.StatusOK {
		t.Fatalf("keyed unlock = %d, want 200", rec.Code)
	}
}
