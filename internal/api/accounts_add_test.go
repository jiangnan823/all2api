package api

// Add-account endpoint tests: happy paths (workbuddy JSON paste, qoder PAT),
// validation failures (missing uid / token, bad prefix, bad JSON body), auth
// guard, pool registration and file persistence. The noctx red line applies:
// httptest.NewRequestWithContext everywhere.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"all2api/internal/host"
	"all2api/internal/observability"
	"all2api/internal/schedule"
)

func newAddAccountMux(t *testing.T) (*http.ServeMux, *host.Host, string) {
	t.Helper()
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	// Point the store at a temp auth dir (LoadAuthDir records it; the dir is
	// created on demand by the atomic write).
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := h.Creds.LoadAuthDir(dir); err != nil {
		t.Fatalf("LoadAuthDir: %v", err)
	}
	mux := http.NewServeMux()
	RegisterPanelData(mux, h, schedule.NewScheduler(), schedule.NewStatsMemory(), observability.NewRing(8))
	return mux, h, dir
}

const VALID_WB_JSON = `{"account":{"uid":"new-uid-1","nickname":"测试号"},"auth":{"accessToken":"tok","refreshToken":"ref","expiresAt":1800000000,"realm":"cn"}}`

func TestAddWorkbuddyHappyPath(t *testing.T) {
	mux, h, dir := newAddAccountMux(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/workbuddy", strings.NewReader(VALID_WB_JSON))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"id":"new-uid-1"`) {
		t.Fatalf("response missing id: %s", rec.Body)
	}
	// Pool registered + file persisted.
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-new-uid-1.json")); err != nil {
		t.Fatalf("auth file not persisted: %v", err)
	}
	if got := h.Creds.NicknameFor("workbuddy", "new-uid-1"); got != "测试号" {
		t.Fatalf("nickname = %q", got)
	}
}

func TestAddWorkbuddyValidation(t *testing.T) {
	mux, _, dir := newAddAccountMux(t)
	for name, body := range map[string]string{
		"bad json":      `{"account":`,
		"missing uid":   `{"account":{},"auth":{"accessToken":"t","refreshToken":"r"}}`,
		"missing token": `{"account":{"uid":"x"},"auth":{"refreshToken":"r"}}`,
	} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/workbuddy", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", name, rec.Code)
		}
	}
	// No file may exist after only failed attempts.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("failed adds must not write files, found %d", len(entries))
	}
}

func TestAddQoderHappyAndInvalid(t *testing.T) {
	mux, _, dir := newAddAccountMux(t)
	// happy
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/qoder", strings.NewReader(`{"pat":"pt-abcdef123456"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("qoder add: got %d (body %s)", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "qoder-pat.txt")); err != nil {
		t.Fatalf("pat file not persisted: %v", err)
	}
	// invalid prefix
	bad := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/qoder", strings.NewReader(`{"pat":"sk-xyz"}`))
	brec := httptest.NewRecorder()
	mux.ServeHTTP(brec, bad)
	if brec.Code != http.StatusBadRequest {
		t.Fatalf("bad prefix: got %d, want 400", brec.Code)
	}
	// idempotent re-add (same PAT) is still 200
	ok2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/qoder", strings.NewReader(`{"pat":"pt-abcdef123456"}`))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, ok2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("idempotent re-add: got %d", rec2.Code)
	}
}

func TestAddRequiresAuthAndPoolRegistration(t *testing.T) {
	loadM5Config(t, `{"listen":":0","api_key":"secret"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	dir := t.TempDir()
	if _, err := h.Creds.LoadAuthDir(dir); err != nil {
		t.Fatalf("LoadAuthDir: %v", err)
	}
	mux := http.NewServeMux()
	RegisterPanelData(mux, h, schedule.NewScheduler(), schedule.NewStatsMemory(), observability.NewRing(8))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/workbuddy", strings.NewReader(VALID_WB_JSON))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: got %d, want 401", rec.Code)
	}

	// With the key: registers into the pool (Select can find it).
	authed := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/accounts/workbuddy", strings.NewReader(VALID_WB_JSON))
	authed.Header.Set("X-API-Key", "secret")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, authed)
	if rec2.Code != http.StatusOK {
		t.Fatalf("authed add: got %d (body %s)", rec2.Code, rec2.Body)
	}
	// Registration check via snapshot (Select is floor/lease-gated and the
	// freshly added account carries credit 0 until the async seeding lands).
	registered := false
	for _, a := range h.Pool.SnapshotAccounts() {
		if a.Platform == "workbuddy" && a.ID == "new-uid-1" && a.Status == "active" {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("added account not registered in pool")
	}
}
