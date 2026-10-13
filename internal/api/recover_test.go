package api

// Integration tests for POST /api/accounts/{id}/recover (TC-M1-034) and the
// halve_budget_after_429 per-source FieldDef filter (test registration A).
// These run in CI (no local Go toolchain by owner-approved deferral).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/pool"
)

// fakeAdapter implements adapter.Adapter with a canned HealthCheck result.
type fakeAdapter struct {
	name string
	hs   *adapter.HealthStatus
	err  error
}

func (f *fakeAdapter) Name() string         { return f.name }
func (f *fakeAdapter) AuthRequired() bool    { return true }
func (f *fakeAdapter) ConvertRequest(*adapter.UnifiedRequest) (*adapter.PlatformRequest, error) {
	return nil, nil
}
func (f *fakeAdapter) ParseResponse(*adapter.PlatformResponse) (*adapter.UnifiedResponse, error) {
	return nil, nil
}
func (f *fakeAdapter) HealthCheck(context.Context) (*adapter.HealthStatus, error) {
	return f.hs, f.err
}
func (f *fakeAdapter) GetTasks(context.Context) ([]*adapter.TaskState, error) {
	return nil, nil
}
func (f *fakeAdapter) ExecuteTask(context.Context, string) error { return nil }
func (f *fakeAdapter) Capabilities() adapter.CapSet               { return adapter.CapSet{} }
func (f *fakeAdapter) GetTaskSchema(context.Context) ([]*adapter.TaskSchema, error) {
	return nil, nil
}
func (f *fakeAdapter) CredentialStatus() (*adapter.CredentialInfo, error) {
	return &adapter.CredentialInfo{Supported: false}, nil
}
func (f *fakeAdapter) ListModels(context.Context) ([]*adapter.ModelInfo, error) {
	return nil, nil
}

func registerFake(t *testing.T, a *fakeAdapter) {
	t.Helper()
	if err := adapter.Default.Register(a); err != nil {
		t.Fatalf("register fake adapter %q: %v", a.name, err)
	}
}

func newTestHost(t *testing.T, sources ...string) *host.Host {
	t.Helper()
	cfg := config.Config{
		Pool:    config.PoolConfig{CreditFloor: 100, MaxRetries: 3, CooldownSec: 60},
		Sources: map[string]config.SourceConfig{},
	}
	for _, s := range sources {
		cfg.Sources[s] = config.SourceConfig{Enabled: true}
	}
	return host.New(cfg)
}

func postRecover(t *testing.T, h *host.Host, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux, h)
	req := httptest.NewRequest(http.MethodPost, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// tripConsecutive opens the breaker through the consecutive-failure path,
// which latches noAutoRecover (PM 裁一: tripped circuits never self-heal).
func tripConsecutive(t *testing.T, h *host.Host, source string) {
	t.Helper()
	br, ok := h.Breaker(source)
	if !ok {
		t.Fatalf("no breaker for source %q", source)
	}
	for i := 0; i < 5; i++ { // circuit.DefaultConfig(): ConsecutiveFailures = 5
		br.Record(http.StatusInternalServerError, "")
	}
	if !br.IsTripped() {
		t.Fatalf("breaker for %q should be tripped after 5 consecutive failures", source)
	}
	if br.Allow() {
		t.Fatalf("tripped (noAutoRecover) breaker must not allow traffic before recover")
	}
}

func TestRecoverTrippedCircuit(t *testing.T) {
	registerFake(t, &fakeAdapter{
		name: "trec",
		hs:   &adapter.HealthStatus{Reason: adapter.ReasonOK},
	})
	h := newTestHost(t, "trec")
	tripConsecutive(t, h, "trec")

	h.Pool.AddAccount(&pool.Account{
		ID: "a1", Platform: "trec", Status: "cooling",
		CooldownEnd: time.Now().Add(time.Hour), Credit: 500, Health: 1.0,
	})

	rec := postRecover(t, h, "/api/accounts/trec:a1/recover")
	if rec.Code != http.StatusOK {
		t.Fatalf("recover with valid credential: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var resp recoverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.Circuit.State != "closed" || !resp.PoolRecovered {
		t.Fatalf("unexpected recover result: %+v", resp)
	}

	br, _ := h.Breaker("trec")
	if br.IsTripped() || !br.Allow() {
		t.Fatalf("breaker must be closed and allowing traffic after recover")
	}
	accs := h.Pool.ListAccounts("trec")
	if len(accs) != 1 || accs[0].Status != "active" || !accs[0].CooldownEnd.IsZero() {
		t.Fatalf("account cooldown must be cleared and status back to active: %+v", accs)
	}
}

func TestRecoverRefusedWhenCredentialInvalid(t *testing.T) {
	registerFake(t, &fakeAdapter{
		name: "tref",
		hs:   &adapter.HealthStatus{Reason: adapter.ReasonAuthExpired},
	})
	h := newTestHost(t, "tref")
	tripConsecutive(t, h, "tref")

	rec := postRecover(t, h, "/api/accounts/tref:a1/recover")
	if rec.Code != http.StatusConflict {
		t.Fatalf("recover with expired credential: got %d, want 409 (body %s)", rec.Code, rec.Body)
	}
	var resp recoverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OK || resp.Circuit.State != "open" || resp.ReasonCode != adapter.ReasonAuthExpired {
		t.Fatalf("recovery must leave circuit open and report reason: %+v", resp)
	}
	br, _ := h.Breaker("tref")
	if br.Allow() {
		t.Fatalf("refused recover must not re-open traffic path")
	}
}

func TestRecoverUnknownSourceAndBadID(t *testing.T) {
	h := newTestHost(t, "tknown")
	if rec := postRecover(t, h, "/api/accounts/tghost:a1/recover"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown source: got %d, want 404", rec.Code)
	}
	if rec := postRecover(t, h, "/api/accounts/nocolon/recover"); rec.Code != http.StatusBadRequest {
		t.Fatalf("account id without source namespace: got %d, want 400", rec.Code)
	}
}

func TestRecoverRequiresAPIKeyWhenConfigured(t *testing.T) {
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

	h := newTestHost(t, "tauth")
	mux := http.NewServeMux()
	Register(mux, h)

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/tauth:a1/recover", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key with api_configured: got %d, want 401", rec.Code)
	}

	req.Header.Set("X-API-Key", "k1")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("valid key must pass the guard")
	}
}

func TestHalveBudgetFieldDefSourceFilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":0","sources":{"qoder":{"enabled":true},"workbuddy":{"enabled":true}}}`), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	mux := http.NewServeMux()
	Register(mux, nil) // recover route irrelevant here; nil host must not panic
	req := httptest.NewRequest(http.MethodGet, "/api/config/fields", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config/fields: got %d, want 200", rec.Code)
	}

	var groups []struct {
		Group  string `json:"group"`
		Fields []struct {
			Key string `json:"key"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("decode fields: %v", err)
	}

	keyPresent := func(group, key string) bool {
		for _, g := range groups {
			if g.Group != group {
				continue
			}
			for _, f := range g.Fields {
				if f.Key == key {
					return true
				}
			}
		}
		return false
	}
	const halveKey = "sources.%s.throttle.halve_budget_after_429"
	if !keyPresent("sources_throttle.qoder", fmt.Sprintf(halveKey, "qoder")) {
		t.Fatalf("qoder (signin-posture source) must expose halve_budget_after_429")
	}
	if keyPresent("sources_throttle.workbuddy", fmt.Sprintf(halveKey, "workbuddy")) {
		t.Fatalf("non-signin source must not expose halve_budget_after_429 (test registration A)")
	}
}
