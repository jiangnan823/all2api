package api

// usage_api.go implements the M8 request-observation endpoints (frozen wire,
// parallel frontend batch consumes verbatim):
//
//	GET /api/requests?limit=300&result=all|success|error
//	  → 200 {"requests":[{ts:{unix,local}, result, model, platform, account_id,
//	    latency_ms, prompt_tokens, completion_tokens, request_id}]}  (old→new)
//	GET /api/usage
//	  → 200 {"window":"process","requests":N,"fail":N,"prompt_tokens":N,
//	    "completion_tokens":N,"by_platform":{...},"by_model":{...}}
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
func intPtr(v int) *int {
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
		PromptTokens:     intPtr(e.PromptTokens),
		CompletionTokens: intPtr(e.CompletionTokens),
		RequestID:        strPtr(e.RequestID),
	}
}

// handleUsage serves GET /api/usage.
func handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, usage.Default.Aggregate())
}
