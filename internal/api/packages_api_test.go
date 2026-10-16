package api

// Packages endpoint tests: handler behavior against a stubbed packagesBackend
// (frozen wire shape with exact keys, FEFO order passthrough, cycle_end null
// folding, upstream-500 → 502 passthrough, credential miss → 404, wrong
// platform → 400) plus the aggregate fan-out (per-account fold, daily_burn
// math, one failed account annotated without hiding the rest) and the route
// registration. The end-to-end variant with the REAL workbuddy client over an
// httptest upstream lives in packages_api_wire_test.go (workbuddy-tagged, like
// the wire file it exercises). No test here ever touches the network or a real
// credential file.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"all2api/internal/host"
	"all2api/internal/observability"
	"all2api/internal/pool"
	"all2api/internal/schedule"
)

// fakePackagesBackend scripts the seam call and records invocations. The
// aggregate handler probes concurrently, so every access is mutex-guarded —
// without it the race detector trips on the shared call log (found in CI
// test/race, not a production defect).
type fakePackagesBackend struct {
	batches map[string][]pkgBatch // key: accountID
	err     map[string]error      // key: accountID

	mu     sync.Mutex
	called []string
}

func (f *fakePackagesBackend) Packages(_ context.Context, _, accountID string) ([]pkgBatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = append(f.called, accountID)
	if err, ok := f.err[accountID]; ok {
		return nil, err
	}
	return f.batches[accountID], nil
}

func wbBatches() []pkgBatch {
	// Already FEFO (as the workbuddy client contract guarantees): the earliest
	// expiry first, the never-expiring pack last.
	end1 := time.Date(2026, 10, 31, 23, 59, 59, 0, time.FixedZone("UTC+8", 8*60*60))
	end2 := time.Date(2026, 11, 30, 23, 59, 59, 0, time.FixedZone("UTC+8", 8*60*60))
	return []pkgBatch{
		{PackageName: "CodeBuddy个人体验版", Remain: 306, Total: 500, CycleEnd: end1},
		{PackageName: "十一月裂变包", Remain: 1200, Total: 1500, CycleEnd: end2},
		{PackageName: "终身包", Remain: 88, Total: 100},
	}
}

// newPackagesMux mounts the two handlers over the given backend (the
// handler-level tests do not need the store).
func newPackagesMux(b packagesBackend, h *host.Host) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/accounts/{id}/packages", withAPIKey(handleAccountPackages(b)))
	mux.HandleFunc("GET /api/packages", withAPIKey(handlePackagesAgg(b, h)))
	return mux
}

func packagesDo(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ---------- GET /api/accounts/{platform}:{id}/packages ----------

func TestAccountPackagesFrozenWireShape(t *testing.T) {
	b := &fakePackagesBackend{batches: map[string][]pkgBatch{"uid-1": wbBatches()}}
	rec := packagesDo(t, newPackagesMux(b, nil), "/api/accounts/workbuddy:uid-1/packages")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var doc struct {
		Packages []map[string]any `json:"packages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Packages) != 3 {
		t.Fatalf("packages = %d, want 3", len(doc.Packages))
	}
	first := doc.Packages[0]
	for key, want := range map[string]any{
		"package_name": "CodeBuddy个人体验版",
		"remain":       float64(306),
		"total":        float64(500),
	} {
		got, ok := first[key]
		if !ok {
			t.Errorf("missing frozen key %q", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if len(first) != 4 {
		t.Errorf("row carries %d keys, want exactly the 4 frozen ones (%v)", len(first), first)
	}
	end, ok := first["cycle_end"].(map[string]any)
	if !ok {
		t.Fatalf("cycle_end = %v, want the {unix,local} object", first["cycle_end"])
	}
	if _, ok := end["unix"].(float64); !ok {
		t.Errorf("cycle_end.unix = %v, want a number", end["unix"])
	}
	if end["local"] != "2026-10-31 23:59:59" {
		t.Errorf("cycle_end.local = %v, want the UTC+8 wall clock rendered in gateway-local layout", end["local"])
	}
}

func TestAccountPackagesFefoOrderPassthrough(t *testing.T) {
	// The handler must preserve the client's FEFO order verbatim — the panel
	// renders row one as the nearest expiry.
	b := &fakePackagesBackend{batches: map[string][]pkgBatch{"uid-1": wbBatches()}}
	rec := packagesDo(t, newPackagesMux(b, nil), "/api/accounts/workbuddy:uid-1/packages")
	var doc struct {
		Packages []pkgBatchWire `json:"packages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"CodeBuddy个人体验版", "十一月裂变包", "终身包"}
	for i, name := range want {
		if doc.Packages[i].PackageName != name {
			t.Errorf("packages[%d] = %q, want %q", i, doc.Packages[i].PackageName, name)
		}
	}
}

func TestAccountPackagesCycleEndNullForNoExpiry(t *testing.T) {
	b := &fakePackagesBackend{batches: map[string][]pkgBatch{"uid-1": {
		{PackageName: "终身包", Remain: 88, Total: 100},
	}}}
	rec := packagesDo(t, newPackagesMux(b, nil), "/api/accounts/workbuddy:uid-1/packages")
	if !strings.Contains(rec.Body.String(), `"cycle_end":null`) {
		t.Fatalf("never-expiring batch must serialize cycle_end as null, got %s", rec.Body)
	}
}

func TestAccountPackagesErrorMapping(t *testing.T) {
	t.Run("upstream failure is 502 with the Chinese frame", func(t *testing.T) {
		b := &fakePackagesBackend{err: map[string]error{"uid-1": errUpstreamBoom()}}
		rec := packagesDo(t, newPackagesMux(b, nil), "/api/accounts/workbuddy:uid-1/packages")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("got %d, want 502 (upstream failure passes through as-is)", rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "查询积分批次失败") || !strings.Contains(body, "upstream internal error") {
			t.Fatalf("error body must carry the Chinese frame + the upstream message, got %s", body)
		}
		if strings.Contains(rec.Body.String(), "Bearer") || strings.Contains(rec.Body.String(), "task-access") {
			t.Fatal("error body must never echo credential material")
		}
	})
	t.Run("credential miss is 404", func(t *testing.T) {
		b := &fakePackagesBackend{err: map[string]error{"uid-1": &growthAccountError{msg: "credential store: no credential"}}}
		rec := packagesDo(t, newPackagesMux(b, nil), "/api/accounts/workbuddy:uid-1/packages")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "账号不存在") {
			t.Fatalf("error body = %s, want the Chinese 404 reason", rec.Body)
		}
	})
	t.Run("other failure carries the reason verbatim", func(t *testing.T) {
		b := &fakePackagesBackend{err: map[string]error{"uid-1": errors.New("no credits row")}}
		rec := packagesDo(t, newPackagesMux(b, nil), "/api/accounts/workbuddy:uid-1/packages")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("got %d, want 502", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "no credits row") {
			t.Fatalf("error body = %s, want the verbatim reason", rec.Body)
		}
	})
}

func TestAccountPackagesWrongPlatformAndSegment(t *testing.T) {
	b := &fakePackagesBackend{}
	rec := packagesDo(t, newPackagesMux(b, nil), "/api/accounts/qoder:pat-1/packages")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "该平台无积分套餐面") {
		t.Fatalf("error body = %s, want the frozen wording", rec.Body)
	}
	if len(b.called) != 0 {
		t.Fatalf("backend called %d times, want 0 (platform rejected before the seam)", len(b.called))
	}
	rec = packagesDo(t, newPackagesMux(b, nil), "/api/accounts/workbuddy/packages")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed segment: got %d, want 400", rec.Code)
	}
}

func TestPackagesNilBackendAnswersHonestly(t *testing.T) {
	for _, path := range []string{"/api/accounts/workbuddy:uid-1/packages", "/api/packages"} {
		rec := packagesDo(t, newPackagesMux(nil, nil), path)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s: got %d, want 502", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "未包含 workbuddy 平台") {
			t.Errorf("%s: error body = %s, want the build-limitation reason", path, rec.Body)
		}
	}
}

// ---------- GET /api/packages ----------

func newAggHost(t *testing.T, ids ...string) *host.Host {
	t.Helper()
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	for _, id := range ids {
		h.Pool.AddAccount(&pool.Account{Platform: "workbuddy", ID: id})
	}
	return h
}

func TestPackagesAggFrozenWireShapeAndFold(t *testing.T) {
	h := newAggHost(t, "uid-1")
	b := &fakePackagesBackend{batches: map[string][]pkgBatch{"uid-1": wbBatches()}}
	// daily_burn exact fold: the handler's now sits between the test's
	// before/after wall clocks, so ceil(days) can only be one of the two
	// boundary values — accept exactly those two folds.
	before := time.Now()
	rec := packagesDo(t, newPackagesMux(b, h), "/api/packages")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(doc.Accounts))
	}
	acc := doc.Accounts[0]
	for key, want := range map[string]any{
		"platform":     "workbuddy",
		"id":           "uid-1",
		"total_remain": float64(1594), // 306 + 1200 + 88
	} {
		got, ok := acc[key]
		if !ok {
			t.Errorf("missing frozen key %q", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if acc["nickname"] != "" {
		t.Errorf("nickname = %v, want the empty string (no display name stored)", acc["nickname"])
	}
	if len(acc) != 7 {
		// platform,id,nickname,total_remain,nearest,daily_burn,batches — error omitted on success.
		t.Errorf("account carries %d keys, want the 7 frozen ones (%v)", len(acc), acc)
	}
	nearest, ok := acc["nearest"].(map[string]any)
	if !ok {
		t.Fatalf("nearest = %v, want the {unix,local} object", acc["nearest"])
	}
	if nearest["local"] != "2026-10-31 23:59:59" {
		t.Errorf("nearest.local = %v, want the earliest dated expiry", nearest["local"])
	}
	batches, ok := acc["batches"].([]any)
	if !ok || len(batches) != 3 {
		t.Fatalf("batches = %v, want the 3 trimmed rows", acc["batches"])
	}
	row0, ok := batches[0].(map[string]any)
	if !ok {
		t.Fatalf("batches[0] = %v, want an object", batches[0])
	}
	if row0["remain"] != float64(306) {
		t.Errorf("batches[0].remain = %v, want 306", row0["remain"])
	}
	if _, has := row0["package_name"]; has {
		t.Error("aggregate batch rows carry no package_name (frozen trimmed shape)")
	}
	if len(row0) != 2 {
		t.Errorf("aggregate batch row carries %d keys, want exactly remain+cycle_end (%v)", len(row0), row0)
	}
	// daily_burn exact fold: the handler's now sits between the test's
	// before/after wall clocks, so ceil(days) can only be one of the two
	// boundary values — accept exactly those two folds.
	unix := int64(nearest["unix"].(float64))
	d1 := int64(math.Ceil(time.Until(time.Unix(unix, 0)).Hours() / 24))
	d2 := int64(math.Ceil(time.Unix(unix, 0).Sub(before).Hours() / 24))
	wantBurns := map[float64]bool{}
	for _, d := range []int64{d1, d2} {
		if d >= 1 {
			wantBurns[math.Round(float64(1594)/float64(d))] = true
		}
	}
	burn, okBurn := acc["daily_burn"].(float64)
	if !okBurn || !wantBurns[burn] {
		t.Errorf("daily_burn = %v, want one of %v (1594 ÷ ceil(days to nearest))", acc["daily_burn"], wantBurns)
	}
}

// TestDailyBurnForFold pins the burn-rate arithmetic deterministically:
// total ÷ ceil(days), floor at one day, null (false) once the nearest expiry
// has passed.
func TestDailyBurnForFold(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	zone := time.FixedZone("UTC+8", 8*60*60)
	cases := []struct {
		name     string
		total    int64
		nearest  time.Time
		wantBurn int64
		wantOK   bool
	}{
		{"11 whole days", 550, now.Add(11 * 24 * time.Hour), 50, true},
		{"fractional day ceils up", 550, now.Add(11*24*time.Hour + time.Hour), 46, true}, // ceil(11.04)=12 → 550/12
		{"expiring later today", 500, time.Date(2026, 10, 16, 0, 0, 0, 0, zone), 500, true},
		{"already expired", 500, now.Add(-time.Hour), 0, false},
	}
	for _, tc := range cases {
		burn, ok := dailyBurnFor(tc.total, tc.nearest, now)
		if ok != tc.wantOK || burn != tc.wantBurn {
			t.Errorf("%s: dailyBurnFor = (%d,%v), want (%d,%v)", tc.name, burn, ok, tc.wantBurn, tc.wantOK)
		}
	}
	// The named acceptance fold: 1594 remain over 19d15h59m59s → ceil 20 days.
	end := time.Date(2026, 11, 4, 3, 59, 59, 0, time.UTC)
	burn, ok := dailyBurnFor(1594, end, now)
	if !ok || burn != 80 {
		t.Errorf("acceptance fold = (%d,%v), want (80,true) (1594 ÷ 20 days)", burn, ok)
	}
}

func TestPackagesAggNoDatedBatchMeansNulls(t *testing.T) {
	h := newAggHost(t, "uid-1")
	b := &fakePackagesBackend{batches: map[string][]pkgBatch{"uid-1": {
		{PackageName: "终身包", Remain: 88, Total: 100},
	}}}
	rec := packagesDo(t, newPackagesMux(b, h), "/api/packages")
	var doc struct {
		Accounts []pkgAccountWire `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	acc := doc.Accounts[0]
	if acc.Nearest != nil || acc.DailyBurn != nil {
		t.Fatalf("nearest/daily_burn = %v/%v, want null/null (no dated batch)", acc.Nearest, acc.DailyBurn)
	}
	if acc.TotalRemain == nil || *acc.TotalRemain != 88 {
		t.Fatalf("total_remain = %v, want 88", acc.TotalRemain)
	}
}

func TestPackagesAggFailedAccountAnnotatedNotHidden(t *testing.T) {
	h := newAggHost(t, "uid-1", "uid-2")
	b := &fakePackagesBackend{
		batches: map[string][]pkgBatch{"uid-1": wbBatches()},
		err:     map[string]error{"uid-2": errUpstreamBoom()},
	}
	rec := packagesDo(t, newPackagesMux(b, h), "/api/packages")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — one failed account must not fail the aggregate", rec.Code)
	}
	var doc struct {
		Accounts []pkgAccountWire `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2 (failure annotated in place)", len(doc.Accounts))
	}
	// Deterministic delivery order: by id regardless of probe completion.
	if doc.Accounts[0].ID != "uid-1" || doc.Accounts[1].ID != "uid-2" {
		t.Fatalf("ids = %s,%s — want uid-1,uid-2 (deterministic order)", doc.Accounts[0].ID, doc.Accounts[1].ID)
	}
	ok1, bad := doc.Accounts[0], doc.Accounts[1]
	if ok1.Error != "" || len(ok1.Batches) != 3 {
		t.Errorf("uid-1 = %+v, want a clean fold", ok1)
	}
	if bad.Error == "" || !strings.Contains(bad.Error, "upstream internal error") {
		t.Errorf("uid-2 error = %q, want the verbatim upstream reason", bad.Error)
	}
	if bad.TotalRemain != nil || bad.Nearest != nil || bad.DailyBurn != nil {
		t.Errorf("uid-2 = %+v, want nulls for a failed probe", bad)
	}
	if bad.Batches == nil {
		t.Error("uid-2 batches must stay an array on the wire, not null")
	}
	if len(b.called) != 2 {
		t.Errorf("backend called %d times, want one probe per account", len(b.called))
	}
}

func TestPackagesAggEmptyPoolStaysAnArray(t *testing.T) {
	h := newAggHost(t)
	b := &fakePackagesBackend{}
	rec := packagesDo(t, newPackagesMux(b, h), "/api/packages")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"accounts":[]`) {
		t.Fatalf("empty pool must serialize as [], got %s", body)
	}
	if len(b.called) != 0 {
		t.Fatalf("backend called %d times, want 0", len(b.called))
	}
}

func TestPackagesAggSkipsNonWorkbuddyAccounts(t *testing.T) {
	h := newAggHost(t, "uid-1")
	h.Pool.AddAccount(&pool.Account{Platform: "qoder", ID: "pat-1"})
	b := &fakePackagesBackend{batches: map[string][]pkgBatch{"uid-1": wbBatches()}}
	rec := packagesDo(t, newPackagesMux(b, h), "/api/packages")
	var doc struct {
		Accounts []pkgAccountWire `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Accounts) != 1 || doc.Accounts[0].Platform != "workbuddy" {
		t.Fatalf("accounts = %+v, want only the workbuddy account", doc.Accounts)
	}
}

// ---------- registration (real host, no network) ----------

func TestPackagesRoutesRegistered(t *testing.T) {
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	mux := http.NewServeMux()
	RegisterPanelData(mux, h, schedule.NewScheduler(), schedule.NewStatsMemory(), observability.NewRing(8))

	// Malformed segment → 400 proves the route is mounted (and never reaches
	// the backend, so no upstream contact is possible here).
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/accounts/noseparator/packages", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}

	// Unknown account → 404 through the REAL backend (store miss short-
	// circuits before any upstream call).
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/accounts/workbuddy:ghost/packages", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 (unknown account through the real backend)", rec.Code)
	}

	// The aggregate route is mounted too: an empty pool answers 200 [] with
	// zero upstream contact.
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/packages", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/packages: got %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"accounts":[]`) {
		t.Fatalf("GET /api/packages body = %s, want the empty aggregate", body)
	}
}

// TestPackagesRefreshTalliesAndErrors pins the M10 adversarial-review fix:
// POST /api/packages/refresh probes every workbuddy account live (frontend
//「检查」button), counts successes, and lists failures verbatim — the route
// was frozen on the wire but never registered (404) until this batch.
func TestPackagesRefreshTalliesAndErrors(t *testing.T) {
	b := &fakePackagesBackend{
		batches: map[string][]pkgBatch{
			"uid-1": wbBatches(),
			"uid-2": wbBatches(),
		},
		err: map[string]error{
			"uid-2": errors.New("upstream timeout"),
		},
	}
	h := newTestHost(t, "qa")
	h.Creds = host.NewCredentialStore(h.Pool)
	for _, id := range []string{"uid-1", "uid-2"} {
		h.Pool.AddAccount(&pool.Account{ID: id, Platform: "workbuddy", Status: "active"})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/packages/refresh", withAPIKey(handlePackagesRefresh(b, h)))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/packages/refresh", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d (body %s)", rec.Code, rec.Body)
	}
	var resp struct {
		Refreshed int               `json:"refreshed"`
		Errors    []pkgRefreshError `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Refreshed != 1 {
		t.Fatalf("refreshed = %d, want 1", resp.Refreshed)
	}
	if len(resp.Errors) != 1 || resp.Errors[0].ID != "uid-2" || resp.Errors[0].Error == "" {
		t.Fatalf("errors = %+v, want one uid-2 row with the reason", resp.Errors)
	}
}
