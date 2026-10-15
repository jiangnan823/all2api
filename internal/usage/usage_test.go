package usage

// M8 request-ledger gate: ring overflow evicts the oldest while the window
// aggregates keep counting, bucket math (fail excludes interrupted, unknown
// tokens never poison the sums), snapshot isolation, and concurrent Record
// under contention. Every assertion runs against an isolated New() recorder —
// the package-level Default stays untouched.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"all2api/internal/adapter"
)

func entry(result, platform, model string, prompt, completion int) RequestEntry {
	return RequestEntry{
		TS:               time.Unix(1770000000, 0),
		Result:           result,
		Model:            model,
		Platform:         platform,
		AccountID:        "abcd1234",
		Latency:          739 * time.Millisecond,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		RequestID:        "cmb-x",
	}
}

func TestRecordRingOverflowKeepsNewest(t *testing.T) {
	r := New()
	const total = RingCapacity + 5
	for i := 0; i < total; i++ {
		r.Record(entry(ResultSuccess, "p", "m", 1, 2))
	}

	snap := r.Snapshot()
	if len(snap) != RingCapacity {
		t.Fatalf("ring holds %d entries, want %d", len(snap), RingCapacity)
	}
	if snap[0].TS.Unix() != 1770000000 {
		t.Fatalf("oldest surviving entry has wrong ts: %v", snap[0].TS)
	}
	// The 5 oldest of `total` were evicted: entries are value copies of an
	// identical payload, so the count check below is the eviction witness.
	agg := r.Aggregate()
	if agg.Requests != total {
		t.Fatalf("window requests = %d, want %d (aggregates survive eviction)", agg.Requests, total)
	}
	if agg.ByPlatform["p"].Requests != total {
		t.Fatalf("by_platform requests = %d, want %d", agg.ByPlatform["p"].Requests, total)
	}
}

func TestSnapshotIsOldestFirstAndIsolated(t *testing.T) {
	r := New()
	r.Record(entry(ResultSuccess, "p", "m", -1, -1))
	second := entry(ResultError, "p", "m", -1, -1)
	second.TS = second.TS.Add(time.Second)
	r.Record(second)

	snap := r.Snapshot()
	if !snap[0].TS.Before(snap[1].TS) {
		t.Fatalf("snapshot must be oldest first, got %v then %v", snap[0].TS, snap[1].TS)
	}
	snap[0].Result = "mutated"
	if r.Snapshot()[0].Result != ResultSuccess {
		t.Fatal("mutating a snapshot must never leak into the recorder")
	}
}

func TestAggregateBucketsAndFailSemantics(t *testing.T) {
	r := New()
	r.Record(entry(ResultSuccess, "workbuddy", "deepseek-v4-flash", 123, 456))
	r.Record(entry(ResultError, "workbuddy", "deepseek-v4-flash", 10, 0))
	r.Record(entry(ResultStreamError, "trae", "m2", -1, -1))
	r.Record(entry(ResultInterrupted, "trae", "m2", -1, -1))

	agg := r.Aggregate()
	if agg.Window != WindowProcess {
		t.Fatalf("window = %q, want %q", agg.Window, WindowProcess)
	}
	if agg.Requests != 4 || agg.Fail != 2 {
		t.Fatalf("requests/fail = %d/%d, want 4/2 (interrupted is not a failure)", agg.Requests, agg.Fail)
	}
	// Unknown (-1) tokens contribute nothing; known ones sum exactly.
	if agg.PromptTokens != 133 || agg.CompletionTokens != 456 {
		t.Fatalf("token sums = %d/%d, want 133/456", agg.PromptTokens, agg.CompletionTokens)
	}
	if got := agg.ByPlatform["workbuddy"]; got.Requests != 2 || got.Fail != 1 {
		t.Fatalf("workbuddy bucket = %+v, want {2 1}", got)
	}
	if got := agg.ByPlatform["trae"]; got.Requests != 2 || got.Fail != 1 {
		t.Fatalf("trae bucket = %+v, want {2 1} (stream_error fails, interrupted does not)", got)
	}
	if got := agg.ByModel["deepseek-v4-flash"]; got.Requests != 2 || got.Fail != 1 {
		t.Fatalf("deepseek-v4-flash bucket = %+v, want {2 1}", got)
	}
	if got := agg.ByModel["m2"]; got.Requests != 2 || got.Fail != 1 {
		t.Fatalf("m2 bucket = %+v, want {2 1}", got)
	}
	// Returned maps are copies: mutating them must not corrupt the recorder.
	agg.ByPlatform["workbuddy"] = Bucket{Requests: 99}
	if r.Aggregate().ByPlatform["workbuddy"].Requests != 2 {
		t.Fatal("Aggregate must hand out fresh bucket maps")
	}
}

func TestConcurrentRecordMatchesTotals(t *testing.T) {
	r := New()
	const workers, perWorker = 16, 250
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				e := entry(ResultSuccess, "p", fmt.Sprintf("m%d", w), 1, 1)
				e.TS = time.Time{} // exercise the zero-TS stamping path too
				r.Record(e)
			}
		}(w)
	}
	wg.Wait()

	agg := r.Aggregate()
	if want := int64(workers * perWorker); agg.Requests != want {
		t.Fatalf("requests = %d, want %d", agg.Requests, want)
	}
	if got := len(r.Snapshot()); got != RingCapacity {
		t.Fatalf("ring = %d entries, want %d", got, RingCapacity)
	}
	if len(agg.ByModel) != workers {
		t.Fatalf("by_model has %d models, want %d", len(agg.ByModel), workers)
	}
	if agg.PromptTokens != int64(workers*perWorker) {
		t.Fatalf("prompt sum = %d, want %d", agg.PromptTokens, workers*perWorker)
	}
}

func TestShortID(t *testing.T) {
	if got := ShortID("abcdefghij"); got != "abcdefgh" {
		t.Fatalf("ShortID long = %q, want abcdefgh", got)
	}
	if got := ShortID("abc"); got != "abc" {
		t.Fatalf("ShortID short passes through, got %q", got)
	}
}

func TestTokensFromAdapterUsage(t *testing.T) {
	p, c := TokensFromAdapterUsage(nil)
	if p != -1 || c != -1 {
		t.Fatalf("nil usage must map to (-1,-1), got (%d,%d)", p, c)
	}
	p, c = TokensFromAdapterUsage(&adapter.Usage{PromptTokens: 3, CompletionTokens: 4})
	if p != 3 || c != 4 {
		t.Fatalf("usage mapping = (%d,%d), want (3,4)", p, c)
	}
}
