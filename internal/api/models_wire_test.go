//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package api

// End-to-end /api/models test over the REAL wire catalog: a host credential
// store loaded from a t.TempDir fake auth file (never a real credential file
// — red line), a workbuddy TaskClient pointed at a local httptest stub
// (never the real upstream — red line), and the handler mounted the way
// RegisterPanelData mounts it. Covers the CredentialFor lookup path, the
// upstream entry mapping (credit multiplier / thinking tiers / windows) and
// the source honesty marker in one pass.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"all2api/internal/adapter"
	"all2api/internal/host"
	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
)

// catalogWireStub counts directory hits and scripts one reply.
type catalogWireStub struct {
	mu    sync.Mutex
	calls int
	body  string
	auth  string
}

func (s *catalogWireStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls++
		s.auth = r.Header.Get("Authorization")
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(s.body))
	})
}

func (s *catalogWireStub) seen() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.auth
}

// stubAccounts is the accountSource stub: one workbuddy pool account.
type stubAccounts struct{ accts []pool.Account }

func (s stubAccounts) SnapshotAccounts() []pool.Account { return s.accts }

func TestModelsEndpointEndToEnd(t *testing.T) {
	stub := &catalogWireStub{body: `{"code":0,"msg":"ok","data":{"models":[` +
		`{"id":"deepseek-v4-flash","name":"DeepSeek V4 Flash","credits":"x0.05 credits","isDefault":true,` +
		`"maxInputTokens":128000,"maxOutputTokens":32768,"supportsToolCall":true,"supportsImages":true,` +
		`"reasoning":{"defaultEffort":"low","supportedEfforts":["low","high"]}},` +
		`{"id":"plain-one","name":"Plain","maxInputTokens":0}` +
		`]}}`}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)

	// Fake credential in a temp dir (never a real credential file — red line).
	dir := t.TempDir()
	authJSON := `{"account":{"uid":"wb-models-1","nickname":"模型号"},"auth":{"accessToken":"fake-access","refreshToken":"fake-refresh","expiresAt":1800000000,"realm":"cn"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-wb-models-1.json"), []byte(authJSON), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}
	if n, err := h.Creds.LoadAuthDir(dir); err != nil || n != 1 {
		t.Fatalf("LoadAuthDir = %d, %v; want 1, nil", n, err)
	}

	cat := newWBModelCatalog(h.Creds, stubAccounts{accts: []pool.Account{{ID: "wb-models-1", Platform: "workbuddy"}}},
		workbuddy.TaskClientOptions{ChatBaseURL: srv.URL})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models", withAPIKey(modelsEndpointHandler(cat)))

	rec := panelGet(t, mux, "/api/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if calls, auth := stub.seen(); calls != 1 || auth != "Bearer fake-access" {
		t.Fatalf("upstream calls/auth = %d/%q, want 1/Bearer fake-access", calls, auth)
	}

	body := rec.Body.String()
	for _, key := range []string{
		`"id":"workbuddy/deepseek-v4-flash"`,
		`"model":"deepseek-v4-flash"`,
		`"credit_multiplier":0.05`,
		`"is_default":true`,
		`"thinking_tiers":["low","high"]`,
		`"context_size":128000`,
		`"max_output":32768`,
		`"supports_tool":true`,
		`"supports_stream":true`,
		`"supports_vision":true`,
	} {
		if !strings.Contains(body, key) {
			t.Fatalf("wire key %s missing from /api/models body: %s", key, body)
		}
	}
	// The factless upstream entry (maxInputTokens omitted) nulls its unknowns.
	var resp modelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	var plain *modelWire
	for i := range resp.Models {
		if resp.Models[i].Model == "plain-one" {
			plain = &resp.Models[i]
		}
	}
	if plain == nil {
		t.Fatalf("plain-one row missing: %s", body)
	}
	if plain.ContextSize != nil || plain.MaxOutput != nil || plain.CreditMultiplier != nil || plain.ThinkingTiers != nil {
		t.Fatalf("factless row must null unknowns: %+v", plain)
	}
	if plain.SupportsVision != nil {
		t.Fatalf("factless row must leave supports_vision null (never a fabricated false): %+v", plain)
	}
	// Source honesty: live workbuddy directory served → never "local". The
	// default build also mounts the qoder static table (mixed); workbuddy-only
	// builds answer pure "upstream".
	if resp.Source != "upstream" && resp.Source != "mixed" {
		t.Fatalf("source = %q, want upstream or mixed", resp.Source)
	}
	// 60s cache: an immediate second request must not re-hit the upstream.
	rec = panelGet(t, mux, "/api/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("second GET: got %d, want 200", rec.Code)
	}
	if calls, _ := stub.seen(); calls != 1 {
		t.Fatalf("upstream calls after cached repeat = %d, want 1", calls)
	}

	// refresh=1 bypasses the cache and hits the upstream again.
	rec = panelGet(t, mux, "/api/models?refresh=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh GET: got %d, want 200", rec.Code)
	}
	if calls, _ := stub.seen(); calls != 2 {
		t.Fatalf("upstream calls after refresh = %d, want 2", calls)
	}
}

func TestModelsSeamNilWithoutCredentials(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool) // empty store
	if cat := newModelsCatalog(h); cat == nil {
		t.Fatal("empty store still yields a seam (Fetch misses → static fallback)")
	}
	if cat := newModelsCatalog(nil); cat != nil {
		t.Fatalf("nil host must yield nil seam, got %T", cat)
	}

	// A seam over an account-less pool misses: Fetch returns ok=false.
	cat := newWBModelCatalog(h.Creds, stubAccounts{}, workbuddy.TaskClientOptions{})
	if rows, ok := cat.Fetch(t.Context(), false); ok || rows != nil {
		t.Fatalf("account-less Fetch = %v, %v; want nil, false", rows, ok)
	}
}

// TestModelsQoderStaticVisionTable checks the M10-B1 static vision capability
// table over the qoder half of the endpoint (static-only mount, source local):
// known vision-capable display names go out true, names absent from the table
// stay null (undeclared, never a fabricated false). Skipped in builds that do
// not compile the qoder platform — the table then has no rows to check.
func TestModelsQoderStaticVisionTable(t *testing.T) {
	if adapter.Default.Get("qoder") == nil {
		t.Skip("qoder platform not compiled into this build")
	}
	loadM5Config(t, `{"listen":":0"}`)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models", withAPIKey(modelsEndpointHandler(nil)))
	rec := panelGet(t, mux, "/api/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var resp modelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	if resp.Source != "local" {
		t.Fatalf("static-only mount must label source local, got %q", resp.Source)
	}

	byName := map[string]*modelWire{}
	qoder := 0
	for i := range resp.Models {
		m := &resp.Models[i]
		if m.Provider != "qoder" {
			continue
		}
		qoder++
		byName[m.Name] = m
	}
	if qoder != 13 {
		t.Fatalf("qoder static catalog = %d entries, want 13", qoder)
	}
	// Every table row declares true on the wire.
	for _, name := range []string{"Qwen3.8-Max", "Qwen3.8-Flash", "Qwen3.7-Max", "Qwen3.7-Plus", "Qwen3.7-Flash",
		"DeepSeek-V4-Pro", "DeepSeek-Flash", "GLM-5.3", "GLM-5.3-Flash", "GLM-5.2", "Kimi-K3", "Kimi-K2.8-Preview"} {
		m := byName[name]
		if m == nil {
			t.Fatalf("known qoder model %q missing from the static catalog", name)
		}
		if m.SupportsVision == nil || !*m.SupportsVision {
			t.Fatalf("%q must declare supports_vision true, got %v", name, m.SupportsVision)
		}
	}
	// Unknown/undeclared names stay null — never a fabricated false.
	if m := byName["MiniMax-M2.7"]; m == nil || m.SupportsVision != nil {
		t.Fatalf("undeclared MiniMax-M2.7 must keep supports_vision null, got %+v", m)
	}
}
