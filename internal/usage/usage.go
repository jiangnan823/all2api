// Package usage implements the M8 request ledger: an in-process ring buffer
// of completed /v1 dispatches (the panel's 请求记录 table) plus the
// process-lifetime aggregates behind GET /api/usage (请求数/失败/按平台/按模型).
//
// Everything here is per-process diagnostic memory — never persisted, cleared
// on restart (the wire says so: "window":"process"). One Record call per
// client request (a dispatch that internally retried across accounts emits a
// single entry describing its terminal outcome; the account shown is the last
// one attempted). The ring holds the newest RingCapacity entries for the
// table; the aggregate counters are window-wide and never decrease on ring
// eviction.
//
// Wire keys on this package's exported response shapes are frozen
// (M8-reqlog contract): {"window","requests","fail","prompt_tokens",
// "completion_tokens","by_platform","by_model"} with per-bucket
// {"requests","fail"}.
package usage

import (
	"sync"
	"time"

	"all2api/internal/adapter"
)

// RingCapacity is the request-log table depth: the latest 1000 dispatches.
// A named constant like observability.DefaultRingCapacity — growing it is a
// D2 decision, not a silent default.
const RingCapacity = 1000

// WindowProcess is the GET /api/usage "window" wire value: all counters are
// process-lifetime and reset on restart.
const WindowProcess = "process"

// Result vocabulary of one ledger entry (frozen wire values).
const (
	ResultSuccess     = "success"      // completed reply relayed (stream ended with [DONE] or full JSON written)
	ResultError       = "error"        // failed before/outside a committed stream (send failure, pool exhausted, unroutable, breaker open)
	ResultStreamError = "stream_error" // committed stream ended with an upstream error frame (client already got a 200)
	ResultInterrupted = "interrupted"  // client went away mid-dispatch; upstream outcome unobserved
)

// RequestEntry is one completed /v1 dispatch as stored in the ring.
// PromptTokens/CompletionTokens use -1 as "upstream supplied no usage field"
// (streaming chunks carry none today); they marshal to null on the wire.
// AccountID is already in display form (ShortID) — the ledger stores exactly
// what the panel table shows.
type RequestEntry struct {
	TS        time.Time
	Result    string
	Model     string
	Platform  string
	AccountID string
	Latency   time.Duration
	// PromptTokens/CompletionTokens: -1 = unknown (no upstream usage data).
	PromptTokens     int
	CompletionTokens int
	RequestID        string // upstream completion id; "" = none observed → wire null
}

// ShortID renders an account id as its 8-rune display prefix (frozen wire:
// "uid 短码前8"). Shorter ids pass through unchanged.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// TokensFromAdapterUsage maps an upstream usage block onto the ledger's
// token pair; a missing usage block reports the -1-unknown pair (wire null),
// never a fabricated zero.
func TokensFromAdapterUsage(u *adapter.Usage) (promptTok, completionTok int) {
	if u == nil {
		return -1, -1
	}
	return u.PromptTokens, u.CompletionTokens
}

// Bucket is one aggregation cell ("requests"/"fail" wire keys). Fail counts
// gateway-visible failures: ResultError and ResultStreamError. A
// ResultInterrupted request counts in Requests only — the client hung up, so
// calling the upstream "failed" would be fabrication.
type Bucket struct {
	Requests int64 `json:"requests"`
	Fail     int64 `json:"fail"`
}

// Aggregate is the GET /api/usage response body. Field names are the frozen
// wire keys (snake_case).
type Aggregate struct {
	Window           string            `json:"window"`
	Requests         int64             `json:"requests"`
	Fail             int64             `json:"fail"`
	PromptTokens     int64             `json:"prompt_tokens"`
	CompletionTokens int64             `json:"completion_tokens"`
	ByPlatform       map[string]Bucket `json:"by_platform"`
	ByModel          map[string]Bucket `json:"by_model"`
}

// Recorder is the in-process ledger: a fixed-capacity ring of RequestEntry
// plus window-lifetime aggregate counters, all under one mutex so a Snapshot
// and its Aggregate can never disagree. All methods are safe for concurrent
// use.
type Recorder struct {
	mu       sync.Mutex
	capacity int
	buf      []RequestEntry
	head     int // index of the oldest entry once buf is full

	requests int64
	fail     int64
	// Token sums fold in only entries that carried a usage block; unknown
	// (-1) contributes nothing rather than poisoning the sum with a fake 0.
	promptSum     int64
	completionSum int64
	byPlatform    map[string]Bucket
	byModel       map[string]Bucket
}

// New builds a Recorder holding at most RingCapacity entries.
func New() *Recorder {
	return &Recorder{
		capacity:   RingCapacity,
		byPlatform: make(map[string]Bucket),
		byModel:    make(map[string]Bucket),
	}
}

// Default is the process-wide ledger the router records into (same
// package-singleton pattern as adapter.Default / observability.Default).
var Default = New()

// Record stores one completed dispatch and folds it into the aggregates.
// A zero TS is stamped with the arrival time. Aggregates survive ring
// eviction: the ring is the table's viewport, the counters are the window.
func (r *Recorder) Record(e RequestEntry) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.capacity <= 0 {
		return
	}
	if len(r.buf) < r.capacity {
		r.buf = append(r.buf, e)
	} else {
		r.buf[r.head] = e
		r.head = (r.head + 1) % r.capacity
	}

	r.requests++
	fail := e.Result == ResultError || e.Result == ResultStreamError
	if fail {
		r.fail++
	}
	if e.PromptTokens >= 0 {
		r.promptSum += int64(e.PromptTokens)
	}
	if e.CompletionTokens >= 0 {
		r.completionSum += int64(e.CompletionTokens)
	}
	r.byPlatform[e.Platform] = Bucket{
		Requests: r.byPlatform[e.Platform].Requests + 1,
		Fail:     r.byPlatform[e.Platform].Fail + b2i(fail),
	}
	r.byModel[e.Model] = Bucket{
		Requests: r.byModel[e.Model].Requests + 1,
		Fail:     r.byModel[e.Model].Fail + b2i(fail),
	}
}

// Snapshot returns a copy of the buffered entries, oldest first. Mutating the
// returned slice never affects the recorder.
func (r *Recorder) Snapshot() []RequestEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RequestEntry, len(r.buf))
	for i := range r.buf {
		out[i] = r.buf[(r.head+i)%r.capacity]
	}
	return out
}

// Aggregate returns the process-lifetime totals with fresh bucket maps
// (callers may mutate them without touching the recorder).
func (r *Recorder) Aggregate() Aggregate {
	r.mu.Lock()
	defer r.mu.Unlock()
	agg := Aggregate{
		Window:           WindowProcess,
		Requests:         r.requests,
		Fail:             r.fail,
		PromptTokens:     r.promptSum,
		CompletionTokens: r.completionSum,
		ByPlatform:       make(map[string]Bucket, len(r.byPlatform)),
		ByModel:          make(map[string]Bucket, len(r.byModel)),
	}
	for k, v := range r.byPlatform {
		agg.ByPlatform[k] = v
	}
	for k, v := range r.byModel {
		agg.ByModel[k] = v
	}
	return agg
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
