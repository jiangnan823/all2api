package api

// Tests for POST /api/config (M2 second batch). Coverage: the legal-patch
// full chain (t.TempDir file + GET readback proves the hot swap), the
// ConfigSaveResponse field-error shape (400 + per-field entries, never one
// generic sentence), unknown keys, malformed JSON, the 401 guard state, and
// atomicity (a failed write leaves the original file and the hot config
// untouched). All requests go through httptest.NewRequestWithContext
// (noctx red line).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"all2api/internal/adapter"
	"all2api/internal/config"
)

func saveEnv(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write temp config: %v", err)
		}
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	t.Cleanup(func() {
		// Point Shared at a throwaway path so later tests never write into
		// this temp dir through the shared singleton.
		_ = config.Load(filepath.Join(t.TempDir(), "unused.json"))
	})
	return path
}

func doPost(t *testing.T, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux, nil)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeSave(t *testing.T, rec *httptest.ResponseRecorder) configSaveResponse {
	t.Helper()
	var resp configSaveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode ConfigSaveResponse: %v (body %s)", err, rec.Body)
	}
	return resp
}

// TestConfigSaveHappyPathFullChain: a legal patch passes validation, is
// written to disk, hot-swaps into config.Shared, and the very next GET
// /api/config reads the new values back (the contract's "applied = server
// readback" guarantee).
func TestConfigSaveHappyPathFullChain(t *testing.T) {
	path := saveEnv(t, `{"listen":":0","pool":{"credit_floor":100,"max_retries":3,"cooldown_sec":60}}`)

	patch := `[
		{"key":"pool.credit_floor","value":250},
		{"key":"sources.qoder.throttle.steady_rpm","value":8},
		{"key":"sources.qoder.throttle.min_gap_ms","value":[900,200]},
		{"key":"sources.qoder.advanced.request_fingerprint_rewrite","value":true},
		{"key":"router.responses_api.enabled","value":true},
		{"key":"schedule.task_cron","value":"30 10 * * *"}
	]`
	rec := doPost(t, "/api/config", patch, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/config: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	resp := decodeSave(t, rec)
	if !resp.OK || resp.Applied == nil || len(resp.FieldErrors) != 0 {
		t.Fatalf("unexpected save response: ok=%v applied=%v errs=%v", resp.OK, resp.Applied, resp.FieldErrors)
	}

	// Hot state: the runtime sees the merged values immediately.
	cfg := config.Shared.Get()
	if cfg.Pool.CreditFloor != 250 {
		t.Fatalf("pool.credit_floor hot value: got %d, want 250", cfg.Pool.CreditFloor)
	}
	q := cfg.Sources["qoder"]
	if q.Throttle.SteadyRPM != 8 {
		t.Fatalf("qoder steady_rpm hot value: got %d, want 8", q.Throttle.SteadyRPM)
	}
	if q.Throttle.MinGapMS != [2]int{200, 900} {
		t.Fatalf("min_gap_ms must normalize to ascending interval [200,900], got %v", q.Throttle.MinGapMS)
	}
	if !q.Advanced.RequestFingerprintRewrite {
		t.Fatal("fingerprint rewrite must hot-swap to true")
	}
	if !cfg.Router.Responses.Enabled {
		t.Fatal("responses_api.enabled must hot-swap to true")
	}
	if cfg.Schedule.TaskCron != "30 10 * * *" {
		t.Fatalf("task_cron hot value: got %q", cfg.Schedule.TaskCron)
	}
	// Unpatched fields survive the merge (patch semantics, not replace).
	if cfg.Sources["qoder"].Throttle.DailyCap != 300 {
		t.Fatalf("unpatched daily_cap must keep the default 300, got %d", cfg.Sources["qoder"].Throttle.DailyCap)
	}

	// Disk state: same values, written to the loaded path (same resolution chain).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	var onDisk config.Config
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("saved config is not valid JSON: %v (%s)", err, raw)
	}
	if onDisk.Pool.CreditFloor != 250 || onDisk.Sources["qoder"].Throttle.SteadyRPM != 8 {
		t.Fatalf("disk state diverges from hot state: %s", raw)
	}

	// GET /api/config readback: applied payload and fresh GET must agree.
	getRec := doGet(t, "/api/config")
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /api/config after save: got %d", getRec.Code)
	}
	var panel panelConfigResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &panel); err != nil {
		t.Fatalf("decode PanelConfig: %v", err)
	}
	if panel.Throttle["qoder"].SteadyRPM != 8 || !panel.Advanced["qoder"].FingerprintRewrite {
		t.Fatalf("GET readback must reflect the saved values: %s", getRec.Body)
	}
}

// TestConfigSaveRoundTripRestartParity: the written file must reproduce the
// effective hot state after a "restart" (config.Load from the saved file) —
// defaults filled at Load are persisted, not lost, so save→restart never
// changes semantics behind the operator's back.
func TestConfigSaveRoundTripRestartParity(t *testing.T) {
	path := saveEnv(t, `{"listen":":0"}`)

	rec := doPost(t, "/api/config", `[{"key":"pool.max_retries","value":5}]`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/config: got %d (body %s)", rec.Code, rec.Body)
	}

	hot := config.Shared.Get()
	if err := config.Load(path); err != nil {
		t.Fatalf("reload saved config (simulated restart): %v", err)
	}
	reread := config.Shared.Get()
	if reread.Pool.MaxRetries != 5 {
		t.Fatalf("saved value must survive the restart round trip, got %d", reread.Pool.MaxRetries)
	}
	if reread.Pool != hot.Pool {
		t.Fatalf("pool state must be identical across save→restart:\n hot %+v\n disk %+v", hot.Pool, reread.Pool)
	}
	if got := reread.Sources["qoder"].Throttle.SteadyRPM; got != 4 {
		t.Fatalf("contract default steady_rpm must survive the round trip, got %d", got)
	}
	if got := reread.Sources["workbuddy"].Throttle.JitterMinutes; got != 15 {
		t.Fatalf("contract default jitter must survive the round trip, got %d", got)
	}
}

// TestConfigSaveFieldLevelErrors: wrong-shape values answer 400 with
// ConfigSaveResponse.field_errors carrying per-field entries, per the v0.5
// named FieldError shape. Pass 1 (shape: unknown key / wrong JSON type /
// bad cron / fractional number) short-circuits before anything is mutated —
// pass-1 entries cannot be mixed with pass-2 value errors in one response
// because pass 2 never runs when pass 1 fails.
func TestConfigSaveFieldLevelErrors(t *testing.T) {
	saveEnv(t, `{"listen":":0"}`)

	rec := doPost(t, "/api/config", `[
		{"key":"pool.max_retries","value":"three"},
		{"key":"sources.qoder.throttle.min_gap_ms","value":[100,900,5]},
		{"key":"sources.qoder.throttle.steady_rpm","value":4.5},
		{"key":"schedule.task_cron","value":"0 25 * * *"}
	]`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid patch: got %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	resp := decodeSave(t, rec)
	if resp.OK || resp.Applied != nil {
		t.Fatalf("rejected save must not report ok/applied: %+v", resp)
	}
	if len(resp.FieldErrors) != 4 {
		t.Fatalf("want one FieldError per bad entry, got %d: %+v", len(resp.FieldErrors), resp.FieldErrors)
	}
	byKey := make(map[string]adapter.FieldError, len(resp.FieldErrors))
	for _, fe := range resp.FieldErrors {
		byKey[fe.Key] = fe
	}
	if fe := byKey["pool.max_retries"]; fe.Code == adapter.CodeRange || fe.Message == "" {
		t.Fatalf("wrong type must be a distinct rejection with a message key: %+v", fe)
	}
	if fe := byKey["sources.qoder.throttle.steady_rpm"]; fe.Code == adapter.CodeRange {
		t.Fatalf("4.5 is a shape rejection (fractional), not a range rejection: %+v", fe)
	}
	if _, ok := byKey["schedule.task_cron"]; !ok {
		t.Fatal("hour 25 must be rejected as cron shape")
	}
	if _, ok := byKey["sources.qoder.throttle.min_gap_ms"]; !ok {
		t.Fatal("3-element min_gap_ms must be rejected per-field")
	}

	// All-or-nothing: the hot config keeps every old value.
	cfg := config.Shared.Get()
	if cfg.Pool.MaxRetries != 3 || cfg.Sources["qoder"].Throttle.SteadyRPM != 4 {
		t.Fatalf("rejected patch must leave hot config untouched: %+v", cfg.Pool)
	}
}

// TestConfigSaveRangeBoundary: over-max answers 400 with exactly one
// code=range FieldError (§5.20 sign-off path); at-max applies.
func TestConfigSaveRangeBoundary(t *testing.T) {
	saveEnv(t, `{"listen":":0"}`)

	rec := doPost(t, "/api/config", `[{"key":"sources.qoder.throttle.steady_rpm","value":121}]`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-max: got %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	resp := decodeSave(t, rec)
	if len(resp.FieldErrors) != 1 || resp.FieldErrors[0].Code != adapter.CodeRange ||
		resp.FieldErrors[0].Key != "sources.qoder.throttle.steady_rpm" {
		t.Fatalf("over-max must yield exactly one code=range error on the key: %+v", resp.FieldErrors)
	}
	if got := config.Shared.Get().Sources["qoder"].Throttle.SteadyRPM; got != 4 {
		t.Fatalf("range rejection must leave the hot value untouched, got %d", got)
	}

	rec = doPost(t, "/api/config", `[{"key":"sources.qoder.throttle.steady_rpm","value":120}]`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("at-max: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if got := config.Shared.Get().Sources["qoder"].Throttle.SteadyRPM; got != 120 {
		t.Fatalf("at-max must apply, got %d", got)
	}
}

// trimFloat renders integral float64s without exponent notation for the
// small boundary values used above.
func trimFloat(f float64) string {
	b, _ := json.Marshal(int(f))
	return string(b)
}

// TestConfigSaveUnknownKeyRejected: keys outside the delivered FieldDef set
// are rejected (conservative M2 decision — silent ignore would let a typo'd
// danger-zone key report success while saving nothing). Read-only anchors
// (exhaust_402_until, notice_version) must not become patchable either.
func TestConfigSaveUnknownKeyRejected(t *testing.T) {
	saveEnv(t, `{"listen":":0"}`)

	for _, key := range []string{
		"max_rps",             // retired per-second key (G1 deprecation)
		"throttle.steady_rpm", // top-level throttle map is removed (G1)
		"sources.qoder.throttle.backoff.exhaust_402_until", // read-only anchor (PM: 不进可编辑集)
		"notice_version",                    // server-held version, not editable
		"sources.ghost.throttle.steady_rpm", // unknown source id
		"sources.qoder.credentials_dir",     // delivered? no — not in the field set
		"totally.made.up",                   // garbage
	} {
		rec := doPost(t, "/api/config", `[{"key":"`+key+`","value":1}]`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unknown key %q: got %d, want 400 (body %s)", key, rec.Code, rec.Body)
		}
		resp := decodeSave(t, rec)
		if len(resp.FieldErrors) != 1 || resp.FieldErrors[0].Key != key {
			t.Fatalf("unknown key %q must yield one field error on that key: %+v", key, resp.FieldErrors)
		}
	}

	// halve_budget_after_429 is delivered per-source-filtered (qoder only):
	// patchable for qoder, unknown for workbuddy (same predicate both sides).
	rec := doPost(t, "/api/config", `[{"key":"sources.qoder.throttle.halve_budget_after_429","value":true}]`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("qoder halve_budget patch: got %d (body %s)", rec.Code, rec.Body)
	}
	rec = doPost(t, "/api/config", `[{"key":"sources.workbuddy.throttle.halve_budget_after_429","value":true}]`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("workbuddy halve_budget patch must be unknown (filter parity): got %d", rec.Code)
	}
	rec = doPost(t, "/api/config", `[{"key":"sources.trae.throttle.jitter_minutes","value":10}]`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trae jitter patch must be unknown (jitterSupported parity): got %d", rec.Code)
	}
}

// TestConfigSaveMalformedJSON: non-JSON bodies and non-array shapes answer
// plain 400 without leaking into field_errors (the patch never parsed).
func TestConfigSaveMalformedJSON(t *testing.T) {
	saveEnv(t, `{"listen":":0"}`)

	for _, body := range []string{
		`{not json`,
		`{"key":"pool.max_retries","value":5}`,
		`"a string"`,
		`null`,
		`[{"key":"pool.max_retries"}]`, // missing value
		`[{"value":5}]`,                // missing key
		`[{"key":"pool.max_retries","value":5,"evil":1}]`, // unknown member on the element
	} {
		rec := doPost(t, "/api/config", body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed body %q: got %d, want 400 (body %s)", body, rec.Code, rec.Body)
		}
	}
	// Empty patch is a legal no-op (applied echoes current state).
	rec := doPost(t, "/api/config", `[]`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty patch: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if resp := decodeSave(t, rec); !resp.OK || resp.Applied == nil {
		t.Fatalf("empty patch must apply (no-op): %+v", resp)
	}
}

// TestConfigSaveRequiresAPIKeyWhenConfigured: the POST sits behind the same
// withAPIKey guard as the GET — configured key + missing/wrong header → 401,
// correct header → through.
func TestConfigSaveRequiresAPIKeyWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":0","api_key":"k1"}`), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	t.Cleanup(func() {
		_ = config.Load(filepath.Join(t.TempDir(), "unused.json"))
	})

	rec := doPost(t, "/api/config", `[{"key":"pool.max_retries","value":2}]`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: got %d, want 401 (body %s)", rec.Code, rec.Body)
	}

	rec = doPost(t, "/api/config", `[{"key":"pool.max_retries","value":2}]`, map[string]string{"X-API-Key": "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: got %d, want 401", rec.Code)
	}

	rec = doPost(t, "/api/config", `[{"key":"pool.max_retries","value":2}]`, map[string]string{"X-API-Key": "k1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid key: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if got := config.Shared.Get().Pool.MaxRetries; got != 2 {
		t.Fatalf("authenticated save must apply: max_retries=%d", got)
	}
}

// TestConfigSaveMethodNotAllowed: the mux pattern is "POST /api/config"-less
// registration (single handler does its own method dispatch, matching the
// existing surface), so a GET on the save endpoint must 405 rather than
// silently re-serve the read view.
func TestConfigSaveMethodNotAllowed(t *testing.T) {
	saveEnv(t, `{"listen":":0"}`)

	mux := http.NewServeMux()
	Register(mux, nil)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config must keep serving the read view: got %d", rec.Code)
	}

	req = httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/api/config", strings.NewReader(`[]`))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/config: got %d, want 405", rec.Code)
	}
}

// TestConfigSaveAtomicWriteFailurePreservesOriginal: when the write fails,
// the endpoint reports 500, the original file is byte-identical, and the hot
// config is rolled back — memory never diverges from disk. Failure injection
// holds an exclusive OS lock on the target file (Windows renames fail with
// sharing violations; POSIX would replace the entry under the fd, so the
// test is Windows-only like the dev loop that runs it).
func TestConfigSaveAtomicWriteFailurePreservesOriginal(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skipf("failure injection relies on Windows file sharing semantics, GOOS=%s", runtime.GOOS)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{"listen":":0","pool":{"credit_floor":111,"max_retries":3,"cooldown_sec":60}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write original config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	t.Cleanup(func() {
		_ = config.Load(filepath.Join(t.TempDir(), "unused.json"))
	})

	// Hold the target open without FILE_SHARE_DELETE: the atomic write's
	// os.Rename over the live file must fail after the temp file is durable.
	lock, err := os.Open(path)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer lock.Close()

	rec := doPost(t, "/api/config", `[{"key":"pool.credit_floor","value":222}]`, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("write must fail: got %d, want 500 (body %s)", rec.Code, rec.Body)
	}

	// Original file untouched, byte for byte.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(raw) != original {
		t.Fatalf("failed save must not touch the original file:\n got %q\nwant %q", raw, original)
	}
	// Hot config rolled back to the pre-patch snapshot.
	if got := config.Shared.Get().Pool.CreditFloor; got != 111 {
		t.Fatalf("hot config must roll back after failed write, got %d", got)
	}
	// No temp litter left behind.
	lock.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Fatalf("failed save left litter behind: %s", e.Name())
		}
	}
}

// TestWriteConfigFileAtomicCreatesDirectory: the atomic write creates the
// config directory when it does not exist (0700, trail.go posture) and the
// file lands at 0600 — a first save into a fresh data dir must not require
// manual mkdir. (Unit-level: the handler picks its path from Shared, which a
// test cannot reset to "" to reach the main.go defaults fallback.)
func TestWriteConfigFileAtomicCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf", "deeper", "config.json")
	if err := writeConfigFileAtomic(path, config.Defaults()); err != nil {
		t.Fatalf("atomic write into fresh dirs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file must exist: %v", err)
	}
	if dirInfo, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("config dir must exist: %v", err)
	} else if !dirInfo.Mode().IsDir() {
		t.Fatalf("config path parent must be a directory")
	}
	// Windows has no POSIX permission bits (0600/0700 map to the read-only
	// attribute at most); the perm constants are exercised on the POSIX side
	// of CI. Here: file readable and parseable is the meaningful assertion.
	var back config.Config
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("written file must be valid JSON: %v (%s)", err, raw)
	}
	// Compare against the live default rather than a literal so a default-port
	// change does not silently invalidate this round-trip assertion.
	if want := config.Defaults().Listen; back.Listen != want {
		t.Fatalf("defaults must round-trip through the file, got listen=%q want %q", back.Listen, want)
	}
	// No temp litter.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Fatalf("successful save left temp litter: %s", e.Name())
		}
	}
}

// TestConfigSaveNoTearBetweenReaders: concurrent GET snapshots during a save
// must each observe a whole config — either fully old or fully new — never a
// mixed struct. Runs the save N times while readers hammer Get().
func TestConfigSaveNoTearBetweenReaders(t *testing.T) {
	saveEnv(t, `{"listen":":0"}`)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			cfg := config.Shared.Get() // would race on unsynchronized maps
			if cfg.Pool.CreditFloor != 100 && cfg.Pool.CreditFloor != 250 {
				t.Errorf("torn read: credit_floor=%d is neither old (100) nor new (250)", cfg.Pool.CreditFloor)
				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		v := 250
		if i%2 == 1 {
			v = 100
		}
		body := `[{"key":"pool.credit_floor","value":` + trimFloat(float64(v)) + `}]`
		if rec := doPost(t, "/api/config", body, nil); rec.Code != http.StatusOK {
			t.Fatalf("save %d: got %d (body %s)", i, rec.Code, rec.Body)
		}
	}
	close(stop)
	<-done
}
