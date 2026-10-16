package api

// M10-B2 batch-op endpoint tests: the frozen wire shape, per-op write-back
// parity with the scheduled handlers, the capability skip (travel outside
// workbuddy 全 skipped), skip predicates, and the failure classification
// (cooldown vs credential-expiry disable). The fake upstream carries a mutex —
// the shared-fake race lesson from M8 — and the account source is the real
// pool-backed one, so the capability table under test is production's.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"all2api/internal/host"
	"all2api/internal/pool"
	"all2api/internal/schedule"
)

// fakeBatchUpstream records calls and hands back scripted outcomes. All
// methods lock: the handler runs in the httptest server goroutine while the
// test goroutine inspects the calls.
type fakeBatchUpstream struct {
	mu sync.Mutex

	signinDelta    int64
	travelDelta    int64
	keepaliveUntil time.Time

	errFor   map[string]error // account id → scripted error
	calls    []string         // "op:platform:id" in call order
	seenCtx  bool
	failSlow bool
}

func (f *fakeBatchUpstream) record(op string, acc *schedule.Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op+":"+acc.Platform+":"+acc.ID)
	return f.errFor[acc.ID]
}

func (f *fakeBatchUpstream) Signin(ctx context.Context, acc *schedule.Account) (schedule.SigninResult, error) {
	if err := f.record("signin", acc); err != nil {
		return schedule.SigninResult{}, err
	}
	f.mu.Lock()
	f.seenCtx = f.seenCtx || ctx != nil
	delta := f.signinDelta
	f.mu.Unlock()
	return schedule.SigninResult{CreditDelta: delta}, nil
}

func (f *fakeBatchUpstream) ActiveCheck(_ context.Context, acc *schedule.Account) error {
	return f.record("active_check", acc)
}

func (f *fakeBatchUpstream) CatTravel(ctx context.Context, acc *schedule.Account) (schedule.CatTravelResult, error) {
	if err := f.record("cat_travel", acc); err != nil {
		return schedule.CatTravelResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return schedule.CatTravelResult{CreditDelta: f.travelDelta}, nil
}

func (f *fakeBatchUpstream) TokenKeepalive(ctx context.Context, acc *schedule.Account) (schedule.KeepaliveResult, error) {
	if err := f.record("token_keepalive", acc); err != nil {
		return schedule.KeepaliveResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return schedule.KeepaliveResult{ExpiresAt: f.keepaliveUntil}, nil
}

func (f *fakeBatchUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newBatchFixture: a workbuddy-only pool (the capability truth) with one
// account per scripted state, wired through the production registration path.
func newBatchFixture(t *testing.T, up schedule.Upstream) (*host.Host, *http.ServeMux) {
	t.Helper()
	loadM5Config(t, `{"listen":":0"}`)
	h := newTestHost(t, "workbuddy")
	h.Creds = host.NewCredentialStore(h.Pool)
	h.Pool.AddAccount(&pool.Account{ID: "wb-ok", Platform: "workbuddy", Status: "active", Credit: 500})
	h.Pool.AddAccount(&pool.Account{ID: "wb-cool", Platform: "workbuddy", Status: "cooling",
		Credit: 500, CooldownEnd: time.Now().Add(time.Minute)})
	h.Pool.AddAccount(&pool.Account{ID: "wb-off", Platform: "workbuddy", Status: "disabled", Credit: 500})
	h.Pool.AddAccount(&pool.Account{ID: "qd-1", Platform: "qoder", Status: "active", Credit: 500})

	mux := http.NewServeMux()
	handle := handleBatchOp(h, host.NewScheduleSource(h.Pool), up)
	mux.HandleFunc("POST /api/accounts/{platform}/batch-op", withAPIKey(handle))
	return h, mux
}

func postBatchOp(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

type batchWire struct {
	Requested int `json:"requested"`
	Done      int `json:"done"`
	Skipped   int `json:"skipped"`
	Failed    []struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	} `json:"failed"`
}

func decodeBatch(t *testing.T, rec *httptest.ResponseRecorder) batchWire {
	t.Helper()
	var resp batchWire
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode batch response: %v (%s)", err, rec.Body)
	}
	return resp
}

func TestBatchOpCheckinSuccessWritesCreditAndStatus(t *testing.T) {
	up := &fakeBatchUpstream{signinDelta: 20}
	h, mux := newBatchFixture(t, up)

	rec := postBatchOp(t, mux, "/api/accounts/workbuddy/batch-op", `{"op":"checkin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	// Wire keys first — the frontend batch aligns against these names.
	for _, key := range []string{`"requested"`, `"done"`, `"skipped"`, `"failed"`} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Fatalf("wire key %s missing: %s", key, rec.Body)
		}
	}
	resp := decodeBatch(t, rec)
	if resp.Requested != 3 || resp.Done != 1 || resp.Skipped != 2 || len(resp.Failed) != 0 {
		t.Fatalf("counters = %+v, want requested 3 done 1 skipped 2 (cool+disabled skip)", resp)
	}
	if !strings.Contains(rec.Body.String(), `"failed":[]`) {
		t.Fatalf("failed must serialize as [] not null: %s", rec.Body)
	}
	// The reward landed in the pool and the account re-activated (handler parity).
	for _, acc := range h.Pool.SnapshotAccounts() {
		if acc.ID != "wb-ok" {
			continue
		}
		if acc.Credit != 520 || acc.Status != "active" {
			t.Fatalf("wb-ok = credit %d status %s, want 520/active", acc.Credit, acc.Status)
		}
	}
	if up.callCount() != 1 {
		t.Fatalf("upstream called %d times, want 1 (skips never burn a call)", up.callCount())
	}
}

func TestBatchOpTravelOnQoderAllSkipped(t *testing.T) {
	up := &fakeBatchUpstream{}
	_, mux := newBatchFixture(t, up)

	rec := postBatchOp(t, mux, "/api/accounts/qoder/batch-op", `{"op":"travel"}`)
	resp := decodeBatch(t, rec)
	if resp.Requested != 1 || resp.Done != 0 || resp.Skipped != 1 || len(resp.Failed) != 0 {
		t.Fatalf("qoder travel = %+v, want requested 1 skipped 1 (capability gap)", resp)
	}
	if up.callCount() != 0 {
		t.Fatalf("capability-skipped accounts must never reach the upstream, got %d calls", up.callCount())
	}
}

func TestBatchOpKeepaliveRunsThroughCooldownAndWritesExpiry(t *testing.T) {
	up := &fakeBatchUpstream{keepaliveUntil: time.Now().Add(24 * time.Hour)}
	h, mux := newBatchFixture(t, up)

	rec := postBatchOp(t, mux, "/api/accounts/workbuddy/batch-op", `{"op":"keepalive"}`)
	resp := decodeBatch(t, rec)
	// keepalive skips only disabled: ok + cooling both run.
	if resp.Requested != 3 || resp.Done != 2 || resp.Skipped != 1 || len(resp.Failed) != 0 {
		t.Fatalf("keepalive counters = %+v, want 3/2/1", resp)
	}
	for _, acc := range h.Pool.SnapshotAccounts() {
		if acc.ID == "wb-ok" && acc.ExpiresAt.IsZero() {
			t.Fatal("keepalive expiry must be written back into the pool")
		}
	}
}

func TestBatchOpFailureCoolsAccountAndReportsRow(t *testing.T) {
	up := &fakeBatchUpstream{errFor: map[string]error{
		"wb-ok": errors.New("upstream 500: exploded"),
	}}
	h, mux := newBatchFixture(t, up)

	rec := postBatchOp(t, mux, "/api/accounts/workbuddy/batch-op", `{"op":"active"}`)
	resp := decodeBatch(t, rec)
	if resp.Requested != 3 || resp.Done != 0 || resp.Skipped != 2 || len(resp.Failed) != 1 {
		t.Fatalf("failure counters = %+v, want 3/0/2 with one failed row", resp)
	}
	if resp.Failed[0].ID != "wb-ok" || resp.Failed[0].Error == "" {
		t.Fatalf("failed row = %+v, want the account id and a reason", resp.Failed[0])
	}
	// Failure write-back parity: cooldown with the task-failure note.
	var target *pool.Account
	for _, a := range h.Pool.ListAccounts("workbuddy") {
		if a.ID == "wb-ok" {
			target = a
		}
	}
	if target == nil || target.Status != "cooling" || target.StatusNote() != "task failed" {
		t.Fatalf("failed account state = %s/%q, want cooling/task failed",
			target.Status, target.StatusNote())
	}
}

func TestBatchOpCredentialExpiryDisables(t *testing.T) {
	up := &fakeBatchUpstream{errFor: map[string]error{
		"wb-ok": errors.Join(schedule.ErrCredentialExpired, errors.New("refresh chain exhausted")),
	}}
	h, mux := newBatchFixture(t, up)

	rec := postBatchOp(t, mux, "/api/accounts/workbuddy/batch-op", `{"op":"checkin"}`)
	resp := decodeBatch(t, rec)
	if len(resp.Failed) != 1 || resp.Done != 0 {
		t.Fatalf("expiry counters = %+v, want one failed row and no done", resp)
	}
	for _, a := range h.Pool.ListAccounts("workbuddy") {
		if a.ID == "wb-ok" && a.Status != "disabled" {
			t.Fatalf("credential-expiry account = %s, want disabled", a.Status)
		}
	}
}

func TestBatchOpUnsupportedTaskCountsSkipped(t *testing.T) {
	up := &fakeBatchUpstream{errFor: map[string]error{
		"wb-ok": errors.Join(schedule.ErrUnsupportedTask, errors.New("no client")),
	}}
	_, mux := newBatchFixture(t, up)

	rec := postBatchOp(t, mux, "/api/accounts/workbuddy/batch-op", `{"op":"active"}`)
	resp := decodeBatch(t, rec)
	if resp.Skipped != 3 || resp.Done != 0 || len(resp.Failed) != 0 {
		t.Fatalf("unsupported = %+v, want quiet skip for every account", resp)
	}
}

func TestBatchOpValidationAndEmptyPool(t *testing.T) {
	up := &fakeBatchUpstream{}
	h, mux := newBatchFixture(t, up)

	// Unknown op → 400, no upstream contact.
	if rec := postBatchOp(t, mux, "/api/accounts/workbuddy/batch-op", `{"op":"frobnicate"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown op = %d, want 400", rec.Code)
	}
	// Malformed body → 400.
	if rec := postBatchOp(t, mux, "/api/accounts/workbuddy/batch-op", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d, want 400", rec.Code)
	}
	// Empty platform → honest zeros, still an array for failed.
	rec := postBatchOp(t, mux, "/api/accounts/trae/batch-op", `{"op":"checkin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty platform = %d, want 200", rec.Code)
	}
	resp := decodeBatch(t, rec)
	if resp.Requested != 0 || !strings.Contains(rec.Body.String(), `"failed":[]`) {
		t.Fatalf("empty platform response = %s", rec.Body)
	}
	_ = h
}

func TestBatchOpSequentialExecution(t *testing.T) {
	up := &fakeBatchUpstream{signinDelta: 1}
	h, mux := newBatchFixture(t, up)
	// Reset the fixture pool to a uniform active set so every account runs.
	h.Pool.AddAccount(&pool.Account{ID: "wb-ok2", Platform: "workbuddy", Status: "active", Credit: 500})

	rec := postBatchOp(t, mux, "/api/accounts/workbuddy/batch-op", `{"op":"checkin"}`)
	resp := decodeBatch(t, rec)
	// 4 requested: wb-ok, wb-cool, wb-off, wb-ok2 → 2 run, 2 skip.
	if resp.Requested != 4 || resp.Done != 2 || resp.Skipped != 2 {
		t.Fatalf("sequential counters = %+v, want 4/2/2", resp)
	}
	up.mu.Lock()
	calls := append([]string(nil), up.calls...)
	up.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want exactly the two eligible accounts", calls)
	}
}
