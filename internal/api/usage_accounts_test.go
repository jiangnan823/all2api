package api

// M10-B2 /api/usage "accounts" aggregation tests: per-account cell math over
// the request ring (tokens = prompt+completion, tok_per_sec = tokens/duration),
// unknown-token and unattributed-entry semantics.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"all2api/internal/usage"
)

func TestUsageAccountsAggregation(t *testing.T) {
	isolateLedger(t)
	base := time.Unix(1_800_000_000, 0)
	// Two accounts plus one unattributed failure (no account reached yet).
	usage.Default.Record(usage.RequestEntry{
		TS: base, Result: usage.ResultSuccess, Model: "m", Platform: "qa",
		AccountID: "aaaaaaaa", Latency: 2 * time.Second,
		PromptTokens: 100, CompletionTokens: 50,
	})
	usage.Default.Record(usage.RequestEntry{
		TS: base, Result: usage.ResultSuccess, Model: "m", Platform: "qa",
		AccountID: "aaaaaaaa", Latency: 1 * time.Second,
		PromptTokens: 30, CompletionTokens: -1, // streaming reply: completion unknown
	})
	usage.Default.Record(usage.RequestEntry{
		TS: base, Result: usage.ResultError, Model: "m", Platform: "qa",
		AccountID: "bbbbbbbb", Latency: 500 * time.Millisecond,
		PromptTokens: -1, CompletionTokens: -1, // nothing measured
	})
	usage.Default.Record(usage.RequestEntry{
		TS: base, Result: usage.ResultError, Model: "m", Platform: "qa",
		AccountID: "", Latency: time.Second, // failed before account selection
	})

	rec := usageGet(t, newUsageMux(t), "/api/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, key := range []string{`"accounts"`, `"requests"`, `"tokens"`, `"duration_sec"`, `"tok_per_sec"`} {
		if !strings.Contains(body, key) {
			t.Fatalf("wire key %s missing from usage body: %s", key, body)
		}
	}

	var resp struct {
		Accounts map[string]struct {
			Requests    int64   `json:"requests"`
			Tokens      int64   `json:"tokens"`
			DurationSec float64 `json:"duration_sec"`
			TokPerSec   float64 `json:"tok_per_sec"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The unattributed entry must not fabricate an account row.
	if got := len(resp.Accounts); got != 2 {
		t.Fatalf("accounts rows = %d, want 2 (empty account_id skipped)", got)
	}
	a := resp.Accounts["aaaaaaaa"]
	if a.Requests != 2 || a.Tokens != 180 {
		t.Fatalf("aaaaaaaa = %+v, want 2 requests, 180 tokens (unknown completion adds nothing)", a)
	}
	if a.DurationSec != 3.0 {
		t.Fatalf("duration_sec = %v, want 3", a.DurationSec)
	}
	if a.TokPerSec != 60.0 {
		t.Fatalf("tok_per_sec = %v, want 180/3", a.TokPerSec)
	}
	b := resp.Accounts["bbbbbbbb"]
	if b.Requests != 1 || b.Tokens != 0 || b.TokPerSec != 0 {
		t.Fatalf("bbbbbbbb = %+v, want 1 request, zero tokens, zero throughput", b)
	}
}

func TestUsageAccountsEmptyRing(t *testing.T) {
	isolateLedger(t)
	rec := usageGet(t, newUsageMux(t), "/api/usage")
	// No traffic: the accounts map must still be a JSON object, never null.
	if !strings.Contains(rec.Body.String(), `"accounts":{}`) {
		t.Fatalf("empty ring must deliver an empty object: %s", rec.Body)
	}
}
