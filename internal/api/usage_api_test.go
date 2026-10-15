package api

// M8 request-log / usage KPI endpoint tests: frozen wire keys verified
// verbatim (exact key-set comparison, not field-by-field hope), nullable
// token/request_id semantics, limit + result filtering, the withAPIKey 401
// style, 405 on non-GET, and the noctx red line — every request goes through
// httptest.NewRequestWithContext.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"all2api/internal/usage"
)

// isolateLedger swaps in a fresh ledger for the test and restores Default on
// cleanup, so endpoint tests never see other tests' dispatches.
func isolateLedger(t *testing.T) {
	t.Helper()
	old := usage.Default
	usage.Default = usage.New()
	t.Cleanup(func() { usage.Default = old })
}

func newUsageMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/requests", withAPIKey(handleRequests))
	mux.HandleFunc("/api/usage", withAPIKey(handleUsage))
	return mux
}

func usageGet(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// seedLedger records two well-known dispatches: a full-fidelity success and
// a bare error (unknown tokens, no request id).
func seedLedger(t *testing.T) {
	t.Helper()
	base := time.Unix(1770000000, 0)
	usage.Default.Record(usage.RequestEntry{
		TS: base, Result: usage.ResultSuccess, Model: "deepseek-v4-flash",
		Platform: "workbuddy", AccountID: "abcd1234", Latency: 739 * time.Millisecond,
		PromptTokens: 123, CompletionTokens: 456, RequestID: "cmb-1",
	})
	usage.Default.Record(usage.RequestEntry{
		TS: base.Add(time.Second), Result: usage.ResultError, Model: "m2",
		Platform: "trae", AccountID: "effe5678", Latency: 12 * time.Millisecond,
		PromptTokens: -1, CompletionTokens: -1,
	})
}

// wireKeys pulls the exact JSON key set of every object in a decoded body.
func wireKeys(t *testing.T, body []byte, path ...string) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode body: %v (%s)", err, body)
	}
	for _, k := range path {
		switch node := v.(type) {
		case map[string]any:
			v = node[k]
		case []any:
			idx := -1
			if _, err := fmt.Sscanf(k, "%d", &idx); err != nil || idx < 0 || idx >= len(node) {
				t.Fatalf("wire path %v: bad list index %q", path, k)
			}
			v = node[idx]
		default:
			t.Fatalf("wire path %v: %v is not an object or list", path, v)
		}
	}
	switch tv := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(tv))
		for k := range tv {
			keys = append(keys, k)
		}
		return keys
	case []any:
		if len(tv) == 0 {
			t.Fatalf("wire path %v: empty list, cannot inspect keys", path)
		}
		m, ok := tv[0].(map[string]any)
		if !ok {
			t.Fatalf("wire path %v: list element is not an object", path)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		return keys
	default:
		t.Fatalf("wire path %v: unsupported node %v", path, v)
		return nil
	}
}

func keysMatch(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	set := make(map[string]bool, len(want))
	for _, w := range want {
		set[w] = true
	}
	for _, g := range got {
		if !set[g] {
			return false
		}
	}
	return true
}

func TestRequestsWireKeysVerbatim(t *testing.T) {
	isolateLedger(t)
	seedLedger(t)
	rec := usageGet(t, newUsageMux(t), "/api/requests?limit=300&result=all")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	top := wireKeys(t, rec.Body.Bytes())
	if !keysMatch(top, "requests") {
		t.Fatalf("top-level keys = %v, want [requests]", top)
	}
	row := wireKeys(t, rec.Body.Bytes(), "requests")
	if !keysMatch(row,
		"ts", "result", "model", "platform", "account_id", "latency_ms",
		"prompt_tokens", "completion_tokens", "request_id") {
		t.Fatalf("row keys = %v (frozen wire drift)", row)
	}
	bucket := wireKeys(t, rec.Body.Bytes(), "requests", "0", "ts")
	if !keysMatch(bucket, "unix", "local") {
		t.Fatalf("ts keys = %v, want [unix local]", bucket)
	}
}

func TestRequestsOrderNullabilityAndValues(t *testing.T) {
	isolateLedger(t)
	seedLedger(t)
	rec := usageGet(t, newUsageMux(t), "/api/requests")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Requests []struct {
			TS struct {
				Unix  int64  `json:"unix"`
				Local string `json:"local"`
			} `json:"ts"`
			Result           string  `json:"result"`
			Model            string  `json:"model"`
			Platform         string  `json:"platform"`
			AccountID        string  `json:"account_id"`
			LatencyMS        int64   `json:"latency_ms"`
			PromptTokens     *int    `json:"prompt_tokens"`
			CompletionTokens *int    `json:"completion_tokens"`
			RequestID        *string `json:"request_id"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Requests) != 2 {
		t.Fatalf("rows = %d, want 2", len(resp.Requests))
	}
	first, second := resp.Requests[0], resp.Requests[1]
	// The local layout must match the panel-wide "2006-01-02 15:04:05" stamp
	// (TZ-independent: derived from the same instant the row was seeded with).
	firstLocal := time.Unix(1770000000, 0).Format("2006-01-02 15:04:05")
	// Old→new order (frozen contract).
	if first.Result != usage.ResultSuccess || second.Result != usage.ResultError {
		t.Fatalf("order broken: first=%s second=%s", first.Result, second.Result)
	}
	if first.TS.Unix != 1770000000 || first.TS.Local != firstLocal {
		t.Fatalf("ts wire = %+v (unix 1770000000, server-local layout %q)", first.TS, firstLocal)
	}
	if first.LatencyMS != 739 || first.PromptTokens == nil || *first.PromptTokens != 123 ||
		first.CompletionTokens == nil || *first.CompletionTokens != 456 || first.RequestID == nil || *first.RequestID != "cmb-1" {
		t.Fatalf("success row drifted: %+v", first)
	}
	if second.PromptTokens != nil || second.CompletionTokens != nil || second.RequestID != nil {
		t.Fatalf("unknown tokens/request id must be null, got %+v", second)
	}
}

func TestRequestsLimitAndResultFilter(t *testing.T) {
	isolateLedger(t)
	seedLedger(t)
	usage.Default.Record(usage.RequestEntry{
		TS: time.Unix(1770000010, 0), Result: usage.ResultStreamError, Model: "m3",
		Platform: "qoder", AccountID: "99990000", Latency: time.Millisecond,
		PromptTokens: -1, CompletionTokens: -1,
	})
	usage.Default.Record(usage.RequestEntry{
		TS: time.Unix(1770000020, 0), Result: usage.ResultInterrupted, Model: "m3",
		Platform: "qoder", AccountID: "99990000", Latency: time.Millisecond,
		PromptTokens: -1, CompletionTokens: -1,
	})
	mux := newUsageMux(t)

	decodeResults := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		var resp struct {
			Requests []struct {
				Result string `json:"result"`
			} `json:"requests"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body)
		}
		out := make([]string, 0, len(resp.Requests))
		for _, r := range resp.Requests {
			out = append(out, r.Result)
		}
		return out
	}

	// limit keeps the newest N (old→new order preserved).
	if got := decodeResults(usageGet(t, mux, "/api/requests?limit=2")); len(got) != 2 || got[0] != "stream_error" || got[1] != "interrupted" {
		t.Fatalf("limit=2 → %v, want [stream_error interrupted]", got)
	}
	// result=success keeps only successes.
	if got := decodeResults(usageGet(t, mux, "/api/requests?result=success")); len(got) != 1 || got[0] != "success" {
		t.Fatalf("result=success → %v, want [success]", got)
	}
	// result=error is the troubleshooting bucket: everything not a success.
	got := decodeResults(usageGet(t, mux, "/api/requests?result=error"))
	if len(got) != 3 {
		t.Fatalf("result=error → %v, want error+stream_error+interrupted", got)
	}

	if rec := usageGet(t, mux, "/api/requests?limit=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 → %d, want 400", rec.Code)
	}
	if rec := usageGet(t, mux, "/api/requests?result=bogus"); rec.Code != http.StatusBadRequest {
		t.Fatalf("result=bogus → %d, want 400", rec.Code)
	}
	if rec := usageGet(t, mux, "/api/requests?limit=99999"); rec.Code != http.StatusOK {
		t.Fatalf("limit above ring capacity is legal, got %d", rec.Code)
	}
}

func TestUsageWireShapeAndAggregates(t *testing.T) {
	isolateLedger(t)
	seedLedger(t)
	rec := usageGet(t, newUsageMux(t), "/api/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !keysMatch(wireKeys(t, rec.Body.Bytes()),
		"window", "requests", "fail", "prompt_tokens", "completion_tokens", "by_platform", "by_model") {
		t.Fatalf("usage keys = %v (frozen wire drift)", wireKeys(t, rec.Body.Bytes()))
	}
	if !keysMatch(wireKeys(t, rec.Body.Bytes(), "by_platform", "workbuddy"), "requests", "fail") {
		t.Fatalf("bucket keys = %v, want [requests fail]", wireKeys(t, rec.Body.Bytes(), "by_platform", "workbuddy"))
	}
	var agg struct {
		Window           string `json:"window"`
		Requests         int64  `json:"requests"`
		Fail             int64  `json:"fail"`
		PromptTokens     int64  `json:"prompt_tokens"`
		CompletionTokens int64  `json:"completion_tokens"`
		ByPlatform       map[string]struct {
			Requests int64 `json:"requests"`
			Fail     int64 `json:"fail"`
		} `json:"by_platform"`
		ByModel map[string]struct {
			Requests int64 `json:"requests"`
			Fail     int64 `json:"fail"`
		} `json:"by_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if agg.Window != "process" {
		t.Fatalf(`window = %q, want "process"`, agg.Window)
	}
	if agg.Requests != 2 || agg.Fail != 1 {
		t.Fatalf("requests/fail = %d/%d, want 2/1", agg.Requests, agg.Fail)
	}
	if agg.PromptTokens != 123 || agg.CompletionTokens != 456 {
		t.Fatalf("token sums = %d/%d, want 123/456 (unknown entries add nothing)", agg.PromptTokens, agg.CompletionTokens)
	}
	if agg.ByPlatform["workbuddy"].Requests != 1 || agg.ByPlatform["workbuddy"].Fail != 0 {
		t.Fatalf("workbuddy bucket = %+v, want {1 0}", agg.ByPlatform["workbuddy"])
	}
	if agg.ByPlatform["trae"].Fail != 1 {
		t.Fatalf("trae bucket = %+v, want fail=1", agg.ByPlatform["trae"])
	}
	if agg.ByModel["deepseek-v4-flash"].Requests != 1 || agg.ByModel["m2"].Fail != 1 {
		t.Fatalf("by_model drifted: %+v", agg.ByModel)
	}
}

func TestUsageEndpointsMethodAndAuth(t *testing.T) {
	isolateLedger(t)
	mux := newUsageMux(t)

	post := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/api/requests"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/requests → %d, want 405", rec.Code)
	}
	if rec := post("/api/usage"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/usage → %d, want 405", rec.Code)
	}

	// api_key enforced exactly like the rest of the /api surface (shared
	// loadM5Config helper: temp config file + open-mode cleanup).
	loadM5Config(t, `{"listen":":0","api_key":"sekrit"}`)
	if rec := usageGet(t, mux, "/api/requests"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("keyed /api/requests without header → %d, want 401", rec.Code)
	}
	if rec := usageGet(t, mux, "/api/usage"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("keyed /api/usage without header → %d, want 401", rec.Code)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/usage", nil)
	req.Header.Set("X-API-Key", "sekrit")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyed /api/usage with header → %d, want 200", rec.Code)
	}
}
