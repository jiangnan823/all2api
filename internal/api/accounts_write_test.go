package api

// M6a account write endpoint tests (pause/resume/remove): frozen wire shapes,
// the {platform}:{id} parsing parity with recover, the 404 unknown-account
// mapping, the file+store+pool removal end state, the M6a /api/accounts wire
// keys, and the withAPIKey guard. Every request goes through
// httptest.NewRequestWithContext (noctx gate).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"all2api/internal/config"
	"all2api/internal/host"
)

// qoderFixturePAT is the PAT planted by newWriteHost; qoderTestUID is the
// account id the store derives from it (first 8 hex chars of the SHA-256 —
// mirrors host.patFingerprint; a drift fails the fixture loudly via 404s).
const qoderFixturePAT = "pt-writeapi"

var qoderTestUID = patFingerprintForTests(qoderFixturePAT)

// newWriteHost builds a host whose credential store is loaded from a temp
// auth dir (one workbuddy account + one qoder PAT) and registers the panel
// mux (read + add + write routes included).
func newWriteHost(t *testing.T) (*host.Host, *http.ServeMux, string) {
	t.Helper()
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t)
	h.Creds = host.NewCredentialStore(h.Pool)
	dir := t.TempDir()
	wbBody := `{"account":{"uid":"wb-w","nickname":"w"},"auth":{"accessToken":"tok","expiresAt":0,"realm":"cn"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-wb-w.json"), []byte(wbBody), 0o600); err != nil {
		t.Fatalf("seed workbuddy auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qoder-pat.txt"), []byte(qoderFixturePAT+"\n"), 0o600); err != nil {
		t.Fatalf("seed pat file: %v", err)
	}
	if loaded, err := h.Creds.LoadAuthDir(dir); err != nil || loaded != 2 {
		t.Fatalf("LoadAuthDir: loaded=%d err=%v, want 2/nil", loaded, err)
	}
	mux := http.NewServeMux()
	RegisterPanelData(mux, h, nil, nil, nil)
	return h, mux, dir
}

func writePost(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func patFingerprintForTests(pat string) string {
	sum := sha256.Sum256([]byte(pat))
	return hex.EncodeToString(sum[:])[:8]
}

// assertPaused reads the pool snapshot for the workbuddy fixture account.
func assertPaused(t *testing.T, h *host.Host, id string, want bool) {
	t.Helper()
	for _, acc := range h.Pool.SnapshotAccounts() {
		if acc.Platform == "workbuddy" && acc.ID == id {
			if acc.Paused != want {
				t.Fatalf("pool paused flag = %v, want %v", acc.Paused, want)
			}
			return
		}
	}
	t.Fatalf("account workbuddy/%s missing from pool", id)
}

func TestAccountPauseResumeWireAndEffect(t *testing.T) {
	h, mux, _ := newWriteHost(t)

	rec := writePost(t, mux, "/api/accounts/workbuddy:wb-w/pause")
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"paused":true`) {
		t.Fatalf("pause wire body = %s, want {ok:true,paused:true}", body)
	}
	assertPaused(t, h, "wb-w", true)

	// The paused flag must ride the /api/accounts wire too.
	rec = panelGet(t, mux, "/api/accounts")
	if !strings.Contains(rec.Body.String(), `"paused":true`) {
		t.Fatalf("paused account must show paused:true on /api/accounts: %s", rec.Body)
	}

	rec = writePost(t, mux, "/api/accounts/workbuddy:wb-w/resume")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"paused":false`) {
		t.Fatalf("resume wire body = %s, want {ok:true,paused:false}", body)
	}
	assertPaused(t, h, "wb-w", false)
}

func TestAccountWriteUnknownAccountIs404(t *testing.T) {
	_, mux, _ := newWriteHost(t)
	for _, path := range []string{
		"/api/accounts/workbuddy:wb-ghost/pause",
		"/api/accounts/workbuddy:wb-ghost/resume",
		"/api/accounts/workbuddy:wb-ghost/remove",
	} {
		rec := writePost(t, mux, path)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("POST %s: got %d, want 404 (body %s)", path, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("404 body must carry the error key: %s", rec.Body)
		}
	}
}

func TestAccountWriteMalformedIDIs400(t *testing.T) {
	_, mux, _ := newWriteHost(t)
	for _, path := range []string{
		"/api/accounts/nocolon/pause",
		"/api/accounts/nocolon/resume",
		"/api/accounts/nocolon/remove",
		"/api/accounts/:wb-w/pause",
		"/api/accounts/wb-w:/remove",
	} {
		rec := writePost(t, mux, path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST %s: got %d, want 400 (body %s)", path, rec.Code, rec.Body)
		}
	}
}

func TestAccountRemoveDeletesFileStorePool(t *testing.T) {
	h, mux, dir := newWriteHost(t)
	path := filepath.Join(dir, "workbuddy-wb-w.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture file must exist: %v", err)
	}

	rec := writePost(t, mux, "/api/accounts/workbuddy:wb-w/remove")
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("remove wire body = %s, want {ok:true}", body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credential file must be gone, stat err = %v", err)
	}
	if got := h.Creds.Count(); got != 1 {
		t.Fatalf("store count = %d, want 1 (qoder account untouched)", got)
	}
	if got := len(h.Pool.SnapshotAccounts()); got != 1 {
		t.Fatalf("pool size = %d, want 1", got)
	}
	// config.json is never part of account removal: the auth dir gains none.
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("auth dir must not gain a config.json, stat err = %v", err)
	}
	// The removed account is gone from /api/accounts and a second remove 404s.
	rec = panelGet(t, mux, "/api/accounts")
	if strings.Contains(rec.Body.String(), "wb-w") {
		t.Fatalf("removed account must not appear on /api/accounts: %s", rec.Body)
	}
	rec = writePost(t, mux, "/api/accounts/workbuddy:wb-w/remove")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second remove: got %d, want 404", rec.Code)
	}
}

func TestAccountRemoveQoderDeletesPatFile(t *testing.T) {
	h, mux, dir := newWriteHost(t)

	rec := writePost(t, mux, "/api/accounts/qoder:"+qoderTestUID+"/remove")
	if rec.Code != http.StatusOK {
		t.Fatalf("qoder remove: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "qoder-pat.txt")); !os.IsNotExist(err) {
		t.Fatalf("qoder-pat.txt must be gone, stat err = %v", err)
	}
	if h.Creds.Count() != 1 {
		t.Fatalf("store count = %d, want 1 (workbuddy account untouched)", h.Creds.Count())
	}
}

func TestAccountsWireM6aKeys(t *testing.T) {
	h, mux, _ := newWriteHost(t)
	// Feed the observation plane directly (the dispatch wiring is covered end
	// to end by internal/router router_stats_test.go).
	h.Pool.UpdateHealth("workbuddy", "wb-w", "success", 0)
	h.Pool.UpdateHealth("workbuddy", "wb-w", "success", 0)
	h.Pool.UpdateHealth("qoder", qoderTestUID, "error", 0)
	h.Pool.InFlightAdd("workbuddy", "wb-w", +1)
	h.Pool.SetAccountCreditTotal("workbuddy", "wb-w", 6679)

	rec := panelGet(t, mux, "/api/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, key := range []string{`"success"`, `"fail"`, `"in_flight"`, `"paused"`, `"last_success"`, `"credit_total"`} {
		if !strings.Contains(body, key) {
			t.Fatalf("wire key %s missing from /api/accounts body: %s", key, body)
		}
	}

	var resp accountsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]accountWire{}
	for _, a := range resp.Accounts {
		byID[a.ID] = a
	}
	wb := byID["wb-w"]
	if wb.Success != 2 || wb.Fail != 0 || wb.InFlight != 1 || wb.Paused || wb.CreditTotal != 6679 {
		t.Fatalf("wb-w M6a fields wrong: %+v", wb)
	}
	if wb.LastSuccess == nil || wb.LastSuccess.Unix == 0 || wb.LastSuccess.Local == "" {
		t.Fatalf("wb-w last_success must carry the Q9 dual form, got %+v", wb.LastSuccess)
	}
	qo := byID[qoderTestUID]
	if qo.Fail != 1 || qo.Success != 0 || qo.CreditTotal != 0 {
		t.Fatalf("qoder M6a fields wrong: %+v", qo)
	}
	if qo.LastSuccess != nil {
		t.Fatalf("account without a success must show last_success:null, got %+v", qo.LastSuccess)
	}
}

func TestAccountWriteRequiresAPIKeyWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":0","api_key":"m6k"}`), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	t.Cleanup(func() {
		noKey := filepath.Join(dir, "no-key.json")
		_ = os.WriteFile(noKey, []byte(`{"listen":":0"}`), 0o600)
		_ = config.Load(noKey)
	})

	h := newTestHost(t)
	h.Creds = host.NewCredentialStore(h.Pool)
	authDir := t.TempDir()
	body := `{"account":{"uid":"a1","nickname":"k"},"auth":{"accessToken":"tok","expiresAt":0,"realm":"cn"}}`
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-a1.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}
	if loaded, err := h.Creds.LoadAuthDir(authDir); err != nil || loaded != 1 {
		t.Fatalf("LoadAuthDir: loaded=%d err=%v, want 1/nil", loaded, err)
	}
	mux := http.NewServeMux()
	RegisterPanelData(mux, h, nil, nil, nil)

	for _, p := range []string{"/api/accounts/workbuddy:a1/pause", "/api/accounts/workbuddy:a1/resume", "/api/accounts/workbuddy:a1/remove"} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, p, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("POST %s without key: got %d, want 401", p, rec.Code)
		}
		req.Header.Set("X-API-Key", "m6k")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s with key: got %d, want 200 (body %s)", p, rec.Code, rec.Body)
		}
	}
}
