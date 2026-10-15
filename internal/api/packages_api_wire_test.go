//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package api

// End-to-end packages endpoint test over the REAL wire backend: a host
// credential store loaded from a t.TempDir fake auth file (never a real
// credential file — red line), a workbuddy TaskClient pointed at a local
// httptest stub (never the real upstream — red line), and the two handlers
// mounted the way registerPackages mounts them. Covers the CredentialFor
// lookup path, the FEFO multi-pack shape with the credits filter, the
// aggregate fold (total_remain/nearest/daily_burn) and the per-account error
// annotation in one pass.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"all2api/internal/host"
	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
)

// pkgStub is a minimal billing upstream: the recorded get-user-resource
// package spread (trial pack + fission pack + a token wallet that must never
// leak in) plus failure scripting per mode.
type pkgStub struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (s *pkgStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls++
		fail := s.fail
		s.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v2/billing/meter/get-user-resource" {
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
			return
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"msg":"billing down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[` +
			`{"CapacityUnit":"tokens","PackageName":"Token钱包","CycleCapacityRemain":999000,"CycleCapacitySize":888000,"CycleEndTime":"2026-12-01 00:00:00"},` +
			`{"CapacityUnit":"credits","PackageName":"十一月裂变包","CycleCapacityRemain":1200,"CycleCapacitySize":1500,"CycleEndTime":"2026-11-30 23:59:59"},` +
			`{"CapacityUnit":"credits","PackageName":"CodeBuddy个人体验版","CycleCapacityRemain":306,"CycleCapacitySize":500,"CycleEndTime":"2026-10-31 23:59:59"},` +
			`{"CapacityUnit":"credits","PackageName":"终身包","CycleCapacityRemain":88,"CycleCapacitySize":100}` +
			`]}}}}`))
	})
}

func TestPackagesEndpointsEndToEnd(t *testing.T) {
	stub := &pkgStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	h.Pool.AddAccount(&pool.Account{Platform: "workbuddy", ID: "wb-pkg-1"})
	h.Pool.AddAccount(&pool.Account{Platform: "workbuddy", ID: "wb-pkg-2"})

	// Fake credentials in a temp dir (t.TempDir — never a real credential
	// file), loaded through the store's normal path so CredentialFor serves
	// both accounts.
	dir := t.TempDir()
	for _, uid := range []string{"wb-pkg-1", "wb-pkg-2"} {
		authJSON := `{"account":{"uid":"` + uid + `","nickname":"套餐号"},"auth":{"accessToken":"fake-access","refreshToken":"fake-refresh","expiresAt":1800000000,"realm":"cn"}}`
		if err := os.WriteFile(filepath.Join(dir, "workbuddy-"+uid+".json"), []byte(authJSON), 0o600); err != nil {
			t.Fatalf("seed auth file: %v", err)
		}
	}
	if n, err := h.Creds.LoadAuthDir(dir); err != nil || n != 2 {
		t.Fatalf("LoadAuthDir = %d, %v; want 2, nil", n, err)
	}

	backend := newWBPackagesBackend(h.Creds, workbuddy.TaskClientOptions{
		ChatBaseURL:    srv.URL,
		BillingBaseURL: srv.URL,
		WebBaseURL:     srv.URL,
	})
	mux := newPackagesMux(backend, h)

	// GET single-account packages: FEFO order, credits-only rows, frozen keys.
	rec := packagesDo(t, mux, "/api/accounts/workbuddy:wb-pkg-1/packages")
	if rec.Code != http.StatusOK {
		t.Fatalf("packages: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var doc struct {
		Packages []pkgBatchWire `json:"packages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Packages) != 3 {
		t.Fatalf("packages = %d, want 3 (token wallet filtered)", len(doc.Packages))
	}
	want := []struct {
		name   string
		remain int64
		total  int64
		local  string
	}{
		{"CodeBuddy个人体验版", 306, 500, "2026-10-31 23:59:59"},
		{"十一月裂变包", 1200, 1500, "2026-11-30 23:59:59"},
		{"终身包", 88, 100, ""},
	}
	for i, w := range want {
		got := doc.Packages[i]
		if got.PackageName != w.name || got.Remain != w.remain || got.Total != w.total {
			t.Errorf("packages[%d] = %+v, want %s %d/%d (FEFO ascending)", i, got, w.name, w.remain, w.total)
		}
		if w.local == "" {
			if got.CycleEnd != nil {
				t.Errorf("packages[%d].cycle_end = %v, want null (never expires)", i, got.CycleEnd)
			}
			continue
		}
		if got.CycleEnd == nil || got.CycleEnd.Local != w.local {
			t.Errorf("packages[%d].cycle_end = %v, want local %q (UTC+8 wall clock)", i, got.CycleEnd, w.local)
		}
	}

	// GET /api/packages: both accounts folded, deterministic order, nickname
	// from the store, error-free entries.
	rec = packagesDo(t, mux, "/api/packages")
	if rec.Code != http.StatusOK {
		t.Fatalf("aggregate: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var agg struct {
		Accounts []pkgAccountWire `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatalf("decode aggregate: %v", err)
	}
	if len(agg.Accounts) != 2 {
		t.Fatalf("aggregate accounts = %d, want 2", len(agg.Accounts))
	}
	first, second := agg.Accounts[0], agg.Accounts[1]
	if first.Platform != "workbuddy" || first.ID != "wb-pkg-1" || first.Nickname != "套餐号" {
		t.Errorf("first account = %+v, want wb-pkg-1 with the stored nickname", first)
	}
	if first.Error != "" || len(first.Batches) != 3 {
		t.Errorf("first account fold = %+v, want a clean 3-batch fold", first)
	}
	if first.TotalRemain == nil || *first.TotalRemain != 1594 {
		t.Errorf("total_remain = %v, want 1594 (306+1200+88)", first.TotalRemain)
	}
	if first.Nearest == nil || first.Nearest.Local != "2026-10-31 23:59:59" {
		t.Errorf("nearest = %v, want the earliest dated expiry", first.Nearest)
	}
	if first.DailyBurn == nil || *first.DailyBurn <= 0 {
		t.Errorf("daily_burn = %v, want the positive burn-rate fold", first.DailyBurn)
	}
	if second.ID != "wb-pkg-2" {
		t.Fatalf("second account id = %q, want wb-pkg-2 (deterministic order)", second.ID)
	}
	if second.TotalRemain == nil || *second.TotalRemain != 1594 {
		t.Errorf("second total_remain = %v, want 1594", second.TotalRemain)
	}

	// Unknown account → 404 at the credential lookup, before any upstream
	// call (the stub call count must not move).
	before := stubCalls(stub)
	rec = packagesDo(t, mux, "/api/accounts/workbuddy:ghost/packages")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account: got %d, want 404 (body %s)", rec.Code, rec.Body)
	}
	if stubCalls(stub) != before {
		t.Fatal("unknown account must fail at the credential lookup, before any upstream call")
	}

	// Upstream 500 → 502 with the upstream reason shown verbatim.
	stub.mu.Lock()
	stub.fail = true
	stub.mu.Unlock()
	rec = packagesDo(t, mux, "/api/accounts/workbuddy:wb-pkg-1/packages")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream 500: got %d, want 502 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "billing down") {
		t.Fatalf("error body = %s, want the upstream message verbatim", rec.Body)
	}
	// The aggregate keeps going when one account's probe fails: both entries
	// stay, the failed one annotated.
	rec = packagesDo(t, mux, "/api/packages")
	if rec.Code != http.StatusOK {
		t.Fatalf("aggregate under upstream 500: got %d, want 200 (per-account annotation)", rec.Code)
	}
	agg.Accounts = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatalf("decode aggregate under failure: %v", err)
	}
	if len(agg.Accounts) != 2 {
		t.Fatalf("aggregate accounts = %d, want 2 under upstream failure", len(agg.Accounts))
	}
	for _, a := range agg.Accounts {
		if a.Error == "" {
			t.Errorf("%s: error annotation missing under upstream 500", a.ID)
		}
	}
}

func stubCalls(s *pkgStub) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
