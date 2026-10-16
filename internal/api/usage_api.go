package api

// usage_api.go implements the M8 request-observation endpoints (frozen wire,
// parallel frontend batch consumes verbatim):
//
//	GET /api/requests?limit=300&result=all|success|error
//	  → 200 {"requests":[{ts:{unix,local}, result, model, platform, account_id,
//	    latency_ms, prompt_tokens, completion_tokens, request_id}]}  (old→new)
//	GET /api/usage
//	  → 200 {"window":"process","requests":N,"fail":N,"prompt_tokens":N,
//	    "completion_tokens":N,"by_platform":{...},"by_model":{...},
//	    "accounts":{"<shortId>":{"requests":N,"tokens":N,"duration_sec":f,
//	    "tok_per_sec":f}}}   (accounts is the M10-B2 addition; the frozen M8
//	    keys above keep their order and semantics)
//
// Both are withAPIKey-guarded and read-only over the in-process ledger
// (internal/usage): no persistence, counters reset on restart — the usage
// wire says so with "window":"process". result=success keeps only completed
// replies; result=error is "everything that did not succeed" (error,
// stream_error, interrupted — the troubleshooting view). limit caps to the
// newest entries; the ledger itself never holds more than 1000.

import (
	"net/http"
	"strconv"

	"all2api/internal/adapter"
	"all2api/internal/usage"
)

// defaultRequestsLimit is the frozen default (?limit=300) and
// maxRequestsLimit the hard ceiling (the ring capacity — asking for more
// than exists is legal, not an error).
const (
	defaultRequestsLimit = 300
	maxRequestsLimit     = usage.RingCapacity
)

// resultFilter is the frozen ?result= vocabulary.
const (
	resultFilterAll     = "all"
	resultFilterSuccess = "success"
	resultFilterError   = "error"
)

// requestRow is one ledger entry on the wire. PromptTokens/CompletionTokens
// and RequestID are nullable: an upstream that shipped no usage block (all
// streaming replies today) and a stream that died before any completion id
// are unknowns, never fabricated zeros.
type requestRow struct {
	TS               adapter.TimeLocal `json:"ts"`
	Result           string            `json:"result"`
	Model            string            `json:"model"`
	Platform         string            `json:"platform"`
	AccountID        string            `json:"account_id"`
	LatencyMS        int64             `json:"latency_ms"`
	PromptTokens     *int              `json:"prompt_tokens"`
	CompletionTokens *int              `json:"completion_tokens"`
	RequestID        *string           `json:"request_id"`
}

type requestsResponse struct {
	Requests []requestRow `json:"requests"`
}

// handleRequests serves GET /api/requests.
func handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	limit := defaultRequestsLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
			return
		}
		limit = min(n, maxRequestsLimit)
	}
	result := r.URL.Query().Get("result")
	switch result {
	case "", resultFilterAll, resultFilterSuccess, resultFilterError:
		// ok
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "result must be all, success or error"})
		return
	}

	snap := usage.Default.Snapshot()
	// Oldest→newest scan; keep the newest `limit` matching rows. Snapshots
	// are short (≤1000), so a backward-walk cap avoids an extra allocation
	// dance while preserving order for the client.
	rows := make([]requestRow, 0, min(limit, len(snap)))
	for i := len(snap) - 1; i >= 0 && len(rows) < limit; i-- {
		e := snap[i]
		if !matchesResultFilter(e.Result, result) {
			continue
		}
		rows = append(rows, rowWire(e))
	}
	// rows were collected newest-first; restore the frozen old→new order.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	writeJSON(w, http.StatusOK, requestsResponse{Requests: rows})
}

// matchesResultFilter applies the frozen filter vocabulary: success keeps
// only successes; error is "not a success" (error, stream_error, interrupted
// all belong to the troubleshooting bucket); all/"" keeps everything.
func matchesResultFilter(result, filter string) bool {
	switch filter {
	case resultFilterSuccess:
		return result == usage.ResultSuccess
	case resultFilterError:
		return result != usage.ResultSuccess
	default:
		return true
	}
}

// intPtr maps the ledger's -1-unknown onto the wire null.
// nonNegativePtr: 未知（-1/负数哨兵）序列化为 null，非负保留。与 panel_data.go
// 的 intPtr（直通取址）语义不同，故独立命名。
func nonNegativePtr(v int) *int {
	if v < 0 {
		return nil
	}
	return &v
}

// strPtr maps the empty-string-unknown onto the wire null.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func rowWire(e usage.RequestEntry) requestRow {
	return requestRow{
		TS:               timeLocalVal(e.TS),
		Result:           e.Result,
		Model:            e.Model,
		Platform:         e.Platform,
		AccountID:        e.AccountID,
		LatencyMS:        e.Latency.Milliseconds(),
		PromptTokens:     nonNegativePtr(e.PromptTokens),
		CompletionTokens: nonNegativePtr(e.CompletionTokens),
		RequestID:        strPtr(e.RequestID),
	}
}

// handleUsage serves GET /api/usage.
func handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, usageResponse{
		Aggregate: usage.Default.Aggregate(),
		Accounts:  usageAccounts(usage.Default.Snapshot()),
	})
}

// usageAccountWire is the M10-B2 per-account aggregation cell on the
// /api/usage wire: {"requests","tokens","duration_sec","tok_per_sec"}. Tokens
// sum the prompt+completion pairs that actually carried a usage block (-1
// unknowns contribute nothing, matching the window counters' semantics);
// duration sums every dispatch attributed to the account; tok_per_sec is the
// derived throughput (0 when no measurable duration accumulated).
type usageAccountWire struct {
	Requests    int64   `json:"requests"`
	Tokens      int64   `json:"tokens"`
	DurationSec float64 `json:"duration_sec"`
	TokPerSec   float64 `json:"tok_per_sec"`
}

// usageResponse is the frozen M8 usage wire extended with the M10-B2
// "accounts" map. The embedded Aggregate keeps every frozen key exactly where
// it was; accounts aggregates the request ring (same window semantics as the
// table viewport: entries fall out at RingCapacity, so per-account rows can
// under-count the window counters on a busy process).
type usageResponse struct {
	usage.Aggregate
	Accounts map[string]usageAccountWire `json:"accounts"`
}

// usageAccounts folds the request ring into per-account cells keyed by the
// short account id the ledger stores (entries predating account attribution —
// failed before any account was selected — carry no id and are skipped).
func usageAccounts(entries []usage.RequestEntry) map[string]usageAccountWire {
	out := make(map[string]usageAccountWire)
	for _, e := range entries {
		if e.AccountID == "" {
			continue
		}
		cell := out[e.AccountID]
		cell.Requests++
		cell.DurationSec += e.Latency.Seconds()
		if e.PromptTokens >= 0 {
			cell.Tokens += int64(e.PromptTokens)
		}
		if e.CompletionTokens >= 0 {
			cell.Tokens += int64(e.CompletionTokens)
		}
		if cell.DurationSec > 0 {
			cell.TokPerSec = float64(cell.Tokens) / cell.DurationSec
		}
		out[e.AccountID] = cell
	}
	return out
}
