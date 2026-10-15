// Package router implements the OpenAI-compatible /v1 surface: request
// authentication, model→platform routing, account-pool orchestration and SSE
// stream relaying. Platform protocols stay behind adapter.Sender — this
// package never speaks a native platform protocol itself.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/pool"
	"all2api/internal/usage"
)

// OpenAI error envelope types (subset the gateway actually emits).
const (
	errTypeInvalidRequest = "invalid_request_error"
	errTypeServer         = "server_error"
)

// maxBodyBytes caps /v1 request bodies: chat contexts run far past the 1 MiB
// the panel /api surface allows, but an unbounded decode is a DoS door.
const maxBodyBytes = 10 << 20

// AdapterLookup is the registry surface the router consumes;
// adapter.Registry satisfies it. Injectable so tests can route against
// isolated fake registries instead of the process-wide adapter.Default.
type AdapterLookup interface {
	Get(name string) adapter.Adapter
	ByCap(capability adapter.CapType) []string
}

// Router owns the /v1 request pipeline: auth → model routing → account
// selection → upstream send (via adapter.Sender) → OpenAI wire response.
type Router struct {
	host   *host.Host
	lookup AdapterLookup
}

// Register wires POST /v1/chat/completions and POST /v1/responses onto mux.
// Non-POST methods are answered 405 by the ServeMux method patterns.
func Register(mux *http.ServeMux, h *host.Host, lookup AdapterLookup) {
	rt := &Router{host: h, lookup: lookup}
	mux.HandleFunc("POST /v1/chat/completions", rt.handleChatCompletions)
	mux.HandleFunc("POST /v1/responses", rt.handleResponses)
}

// apiError mirrors the OpenAI error object (wire keys snake_case).
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func writeError(w http.ResponseWriter, status int, e apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: e})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// authorize enforces the gateway api_key when one is configured — semantics
// match api.withAPIKey (X-API-Key first, then Authorization: Bearer); with
// api_key unset the gateway is in open-LAN mode and /v1 stays usable. 401
// bodies use the OpenAI error envelope because /v1 clients speak that dialect.
func authorize(w http.ResponseWriter, r *http.Request) bool {
	key := strings.TrimSpace(config.Shared.Get().APIKey)
	if key == "" {
		return true
	}
	got := r.Header.Get("X-API-Key")
	if got == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			got = strings.TrimSpace(auth[len("Bearer "):])
		}
	}
	if got != key {
		writeError(w, http.StatusUnauthorized, apiError{
			Message: "Incorrect API key provided.",
			Type:    errTypeInvalidRequest,
			Code:    "invalid_api_key",
		})
		return false
	}
	return true
}

// ---------- POST /v1/chat/completions ----------

func (rt *Router) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !authorize(w, r) {
		return
	}
	var req adapter.ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "Invalid request body: " + err.Error(),
			Type:    errTypeInvalidRequest,
		})
		return
	}
	rt.dispatch(w, r, req)
}

// ---------- POST /v1/responses ----------

// responsesAPIRequest is the subset of the Responses API request the
// interface-first router understands: model addressing and the stream flag.
type responsesAPIRequest struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Input  json.RawMessage `json:"input"`
}

func (rt *Router) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !authorize(w, r) {
		return
	}
	if !config.Shared.Get().Router.Responses.Enabled {
		// Honest stub: the flag is off, so the endpoint reports itself
		// unimplemented rather than faking a Responses surface.
		writeError(w, http.StatusNotImplemented, apiError{
			Message: "The responses API is disabled on this gateway (router.responses_api.enabled).",
			Type:    errTypeInvalidRequest,
			Code:    "responses_api_disabled",
		})
		return
	}
	var raw responsesAPIRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "Invalid request body: " + err.Error(),
			Type:    errTypeInvalidRequest,
		})
		return
	}
	msgs, err := responsesInputToMessages(raw.Input)
	if err != nil {
		writeError(w, http.StatusBadRequest, apiError{
			Message: err.Error(),
			Type:    errTypeInvalidRequest,
		})
		return
	}
	rt.dispatch(w, r, adapter.ChatRequest{
		Model:    raw.Model,
		Messages: msgs,
		Stream:   raw.Stream,
	})
}

// responsesInputToMessages performs the minimal Responses→chat mapping the
// interface-first router supports: a plain string input becomes one user
// message; an array of chat-shaped message objects passes through verbatim.
// Native Responses envelope conversion lands with the platform protocol batch.
func responsesInputToMessages(input json.RawMessage) ([]adapter.ChatMessage, error) {
	if len(input) == 0 || string(input) == "null" {
		return nil, errors.New("input is required")
	}
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		return []adapter.ChatMessage{{Role: "user", Content: input}}, nil
	}
	var msgs []adapter.ChatMessage
	if err := json.Unmarshal(input, &msgs); err == nil {
		return msgs, nil
	}
	return nil, errors.New("unsupported input shape: expected a string or a message array")
}

// ---------- shared orchestration ----------

// dispatch runs auth-passed requests through the full pipeline. Send failures
// retry through the pool's own selection semantics: Select leases each chosen
// account for its anti-thundering TTL, so a failed account drops out of the
// candidate set and the next loop iteration naturally advances to the next
// account — no router-side retry ring. The loop ends when Select finds no
// eligible account left.
func (rt *Router) dispatch(w http.ResponseWriter, r *http.Request, req adapter.ChatRequest) {
	// M8 request ledger: exactly one entry per client request, emitted by the
	// deferred record below whatever the exit path. Result defaults to
	// "error" (the validation/pool-exhaustion exits); success-shaped paths
	// overwrite it before returning. Platform/account land here as the
	// pipeline resolves them, so a retried request shows its last attempt.
	start := time.Now()
	ledger := usage.RequestEntry{
		TS:               start,
		Result:           usage.ResultError,
		Model:            bareModelName(req.Model),
		PromptTokens:     -1,
		CompletionTokens: -1,
	}
	defer func() {
		ledger.Latency = time.Since(start)
		usage.Default.Record(ledger)
	}()

	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "you must provide a model parameter",
			Type:    errTypeInvalidRequest,
		})
		return
	}

	a, platform, ok := rt.route(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, apiError{
			Message: "The model '" + req.Model + "' does not exist or no platform provides chat completions.",
			Type:    errTypeInvalidRequest,
			Code:    "model_not_found",
		})
		return
	}
	ledger.Platform = platform
	sender, ok := a.(adapter.Sender)
	if !ok {
		// Interface-first reality: adapters ship without an upstream HTTP
		// client until their protocol batch lands — report it, never fake it.
		writeError(w, http.StatusServiceUnavailable, apiError{
			Message: "No upstream sender registered for platform '" + platform + "'.",
			Type:    errTypeServer,
			Code:    "no_upstream_sender",
		})
		return
	}
	// The upstream sees the bare model name; "platform/" is routing only.
	if i := strings.Index(req.Model, "/"); i > 0 {
		req.Model = req.Model[i+1:]
	}
	// Platform circuit breaker gate (§9.21-e.2): the per-source breaker owns
	// open/half-open recovery semantics (circuit.Allow) — when it refuses, the
	// whole platform is cooling down (429 backoff, 404 short cooldown, 402
	// hard cooldown or consecutive-failure trip), so probing Select would only
	// burn accounts for a guaranteed-failing send. Checked per attempt, not
	// once at entry: a breaker that trips mid-request must stop the retry loop
	// instead of letting one caller hammer a just-broken platform. No breaker
	// entry (platform outside the configured sources map) means no flow
	// control exists for it: pass through and let the pool be the only guard.
	gate := func() bool {
		br, hasBr := rt.host.Breaker(platform)
		return !hasBr || br.Allow()
	}

	// Lease discipline: Select takes an anti-thundering lease on each account
	// it hands out. While this request keeps retrying, the leases stay put —
	// that is what bounds the loop to "each account at most once" (Select
	// returns nil once every account is leased or cooled). They are all
	// released when the request is done, whatever the outcome: holding them
	// for the full 30s TTL after a request would count a healthy account as
	// unavailable and 503 the client's next request (live regression
	// 2026-10-15: a single funded account answered no_available_account for
	// every request within 30s of the previous one).
	var leased []string
	defer func() {
		for _, id := range leased {
			rt.host.Pool.ReleaseLease(id)
		}
	}()

	var lastErr error
	for {
		if err := r.Context().Err(); err != nil {
			ledger.Result = usage.ResultInterrupted
			return // client went away — nothing left to report or bill
		}
		if !gate() {
			writeError(w, http.StatusServiceUnavailable, apiError{
				Message: "Platform '" + platform + "' is circuit-broken; requests are suspended while it cools down.",
				Type:    errTypeServer,
				Code:    "platform_breaker_open",
			})
			return
		}
		acct, err := rt.host.Pool.Select(r.Context(), platform)
		if err != nil {
			writeError(w, http.StatusInternalServerError, apiError{
				Message: "account selection failed: " + err.Error(),
				Type:    errTypeServer,
			})
			return
		}
		if acct == nil {
			if lastErr != nil {
				// Every candidate was tried and failed: surface the last
				// upstream failure, not a misleading "overloaded".
				writeSendError(w, lastErr)
			} else {
				writeError(w, http.StatusServiceUnavailable, apiError{
					Message: "No available upstream account for platform '" + platform + "'; please retry later.",
					Type:    errTypeServer,
					Code:    "no_available_account",
				})
			}
			return
		}
		leased = append(leased, acct.ID)
		ledger.AccountID = usage.ShortID(acct.ID)
		// Observation plane (M6a): the attempt is in flight from here
		// until its completion; every outcome path below pairs one
		// finishAttempt decrement with the existing UpdateHealth call.
		rt.host.Pool.InFlightAdd(platform, acct.ID, +1)

		if req.Stream {
			committed, reqID, streamErr := rt.relayStream(w, r.Context(), sender, acct, req)
			if reqID != "" {
				ledger.RequestID = reqID
			}
			if !committed {
				// Outright stream-start failure: same retry path as SendChat.
				lastErr = streamErr
				rt.recordSendOutcome(platform, acct.ID, 0, streamErr)
				rt.finishAttempt(platform, acct.ID, healthStatus(streamErr))
				continue
			}
			switch {
			case streamErr == nil:
				ledger.Result = usage.ResultSuccess
				rt.recordSendOutcome(platform, acct.ID, http.StatusOK, nil)
				rt.finishAttempt(platform, acct.ID, "success")
			case errors.Is(streamErr, context.Canceled):
				// Client disconnect mid-stream: the upstream outcome stays
				// unobserved — recorded as interrupted, not as a failure.
				ledger.Result = usage.ResultInterrupted
				rt.recordSendOutcome(platform, acct.ID, upstreamStatus(streamErr), streamErr)
				rt.finishAttempt(platform, acct.ID, healthStatus(streamErr))
			default:
				// Mid-stream failure: the classification of the upstream error
				// (429 vs 5xx vs timeout) still feeds the breaker — Record is
				// keyed on the upstream status, not on the wire status that
				// stays committed at 200.
				ledger.Result = usage.ResultStreamError
				rt.recordSendOutcome(platform, acct.ID, upstreamStatus(streamErr), streamErr)
				rt.finishAttempt(platform, acct.ID, healthStatus(streamErr))
			}
			return
		}

		resp, err := sender.SendChat(r.Context(), acct, req)
		if err == nil {
			ledger.Result = usage.ResultSuccess
			ledger.RequestID = resp.ID
			ledger.PromptTokens, ledger.CompletionTokens = usage.TokensFromAdapterUsage(resp.Usage)
			rt.recordSendOutcome(platform, acct.ID, http.StatusOK, nil)
			rt.finishAttempt(platform, acct.ID, "success")
			writeJSON(w, http.StatusOK, resp)
			return
		}
		lastErr = err
		rt.recordSendOutcome(platform, acct.ID, upstreamStatus(err), err)
		rt.finishAttempt(platform, acct.ID, healthStatus(err))
	}
}

// bareModelName strips a "platform/" routing prefix for the ledger, matching
// what the upstream actually received (the dispatch strip happens only after
// a successful route).
func bareModelName(model string) string {
	if i := strings.Index(model, "/"); i > 0 {
		return model[i+1:]
	}
	return model
}

// finishAttempt closes one dispatch attempt: the in-flight counter booked at
// Select time (+1) drops back, and the outcome feeds the pool's health. A
// "success" also stamps the account's LastSuccess — that bookkeeping lives
// inside pool.UpdateHealth, the router stays unaware of it.
func (rt *Router) finishAttempt(platform, accountID, status string) {
	rt.host.Pool.InFlightAdd(platform, accountID, -1)
	rt.host.Pool.UpdateHealth(platform, accountID, status, 0)
}

// recordSendOutcome feeds one send result into the platform's circuit breaker.
// The breaker is authoritative for open/half-open thresholds (429 exponential
// backoff, 404 short cooldown, 402 hard cooldown, 5xx/0 consecutive-failure
// trip, 2xx–3xx success closes a half-open probe) — the router translates, it
// never re-derives thresholds. statusCode 0 is reserved for "no upstream
// exchange happened" (transport error); a cancelled client context reports
// nothing at all, since the upstream outcome was never observed.
func (rt *Router) recordSendOutcome(platform string, accountID string, statusCode int, err error) {
	if err != nil && errors.Is(err, context.Canceled) {
		return
	}
	br, ok := rt.host.Breaker(platform)
	if !ok {
		return
	}
	br.Record(statusCode, accountID)
}

// upstreamStatus extracts the upstream HTTP status from a send error for
// breaker bookkeeping: classified UpstreamError keeps its status; anything
// else is an opaque transport failure, which circuit.Record treats as
// statusCode 0 (the same bucket as a connection error).
func upstreamStatus(err error) int {
	var ue *adapter.UpstreamError
	if errors.As(err, &ue) {
		return ue.Status
	}
	return 0
}

// route resolves the requested model onto a registered adapter that is also
// enabled in the live config (sources.<id>.enabled, hot-read per request):
// a "platform/model" prefix addresses the platform by name — a platform whose
// source stanza says enabled:false answers model_not_found, the same envelope
// a removed adapter would produce, so one-click source removal is
// indistinguishable from "this model never existed" for prefix-addressing
// clients (503 would invite retries against an address that stays gone); a
// bare model name auto-routes to the first *enabled* adapter carrying
// CapChatCompletion (registry registration order — deterministic per binary)
// that has at least one pooled account, skipping disabled platforms and
// platforms with an empty pool so traffic fails over to any other enabled
// platform that can actually serve the request (a platform with no loaded
// credentials must not shadow one that has them just because it registered
// first). When every enabled candidate's pool is empty the first enabled
// candidate is used anyway, so the dispatch path answers no_available_account
// instead of a misleading model_not_found. A platform with no sources.<id>
// stanza at all is never treated as disabled: only the explicit
// enabled:false switch (the 一键摘源 path) removes a source from /v1.
func (rt *Router) route(model string) (adapter.Adapter, string, bool) {
	disabled := rt.disabledPlatforms()
	var platform string
	if i := strings.Index(model, "/"); i > 0 {
		platform = model[:i]
		if disabled[platform] {
			return nil, "", false
		}
	} else {
		// fallback keeps the first enabled candidate even when its pool is
		// empty; poolAvailable reports whether a candidate has accounts.
		var fallback string
		poolAvailable := func(name string) bool {
			if rt.host == nil || rt.host.Pool == nil {
				return true // no pool to consult: preserve registration-order pick
			}
			return len(rt.host.Pool.ListAccounts(name)) > 0
		}
		for _, name := range rt.lookup.ByCap(adapter.CapChatCompletion) {
			if disabled[name] {
				continue
			}
			if fallback == "" {
				fallback = name
			}
			if poolAvailable(name) {
				platform = name
				break
			}
		}
		if platform == "" {
			platform = fallback
		}
		if platform == "" {
			return nil, "", false
		}
	}
	a := rt.lookup.Get(platform)
	if a == nil {
		return nil, "", false
	}
	return a, platform, true
}

// disabledPlatforms snapshots the live config's per-source enabled flags,
// keeping only the explicitly disabled ids: an absent stanza means the
// platform is not under source management yet and stays routable.
func (rt *Router) disabledPlatforms() map[string]bool {
	cfg := config.Shared.Get()
	out := make(map[string]bool, len(cfg.Sources))
	for id, src := range cfg.Sources {
		if !src.Enabled {
			out[id] = true
		}
	}
	return out
}

// relayStream forwards SendChatStream chunks to the client as
// text/event-stream frames ("data: {chunk}\n\n") and terminates with
// "data: [DONE]". committed=false means nothing was written and the caller
// may retry on the next account; once committed, the status line is out and
// a mid-stream failure can only travel as an in-band error frame (followed
// by stream close without [DONE], so clients must not treat it as complete).
// requestID is the completion id observed on the first chunk carrying one,
// for the request ledger ("" when the stream died before any id showed up).
func (rt *Router) relayStream(w http.ResponseWriter, ctx context.Context, sender adapter.Sender, acct *pool.Account, req adapter.ChatRequest) (committed bool, requestID string, streamErr error) {
	chunks, errCh, err := sender.SendChatStream(ctx, acct, req)
	if err != nil {
		return false, "", err
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	writeFrame := func(payload string) bool {
		if _, wErr := fmt.Fprintf(w, "data: %s\n\n", payload); wErr != nil {
			return false // client went away — no in-band signal possible
		}
		if canFlush {
			flusher.Flush()
		}
		return true
	}

	for {
		select {
		case <-ctx.Done():
			// Client disconnected: stop relaying; the cancelled ctx also
			// tells the sender goroutines to stand down.
			return true, requestID, ctx.Err()
		case chunk, ok := <-chunks:
			if !ok {
				writeFrame("[DONE]")
				return true, requestID, nil
			}
			if requestID == "" && chunk.ID != "" {
				requestID = chunk.ID
			}
			payload, mErr := json.Marshal(chunk)
			if mErr != nil {
				return true, requestID, mErr
			}
			if !writeFrame(string(payload)) {
				return true, requestID, nil
			}
		case upErr, ok := <-errCh:
			if !ok {
				errCh = nil // closed error channel: park this case, keep relaying
				continue
			}
			_, e := apiErrorForSend(upErr)
			if payload, mErr := json.Marshal(errorEnvelope{Error: e}); mErr == nil {
				writeFrame(string(payload))
			}
			return true, requestID, upErr
		}
	}
}

// healthStatus maps a send error onto the pool's UpdateHealth status
// vocabulary so real traffic feeds cooldown/disable semantics.
func healthStatus(err error) string {
	var ue *adapter.UpstreamError
	if errors.As(err, &ue) {
		switch ue.Reason {
		case adapter.ReasonRateLimited:
			return "rate_limited"
		case adapter.ReasonAuthExpired:
			return "auth_error"
		case adapter.ReasonTimeout:
			return "timeout"
		}
	}
	return "error"
}

// apiErrorForSend classifies a failed upstream send into an HTTP status
// (informational for in-band stream frames) and an OpenAI error object.
// UpstreamError keeps its classified status and reason; anything else is a
// plain 502.
func apiErrorForSend(err error) (int, apiError) {
	status := http.StatusBadGateway
	e := apiError{Message: err.Error(), Type: errTypeServer}
	var ue *adapter.UpstreamError
	if errors.As(err, &ue) {
		if ue.Status >= 400 && ue.Status <= 599 {
			status = ue.Status
		}
		e.Type = upstreamErrorType(ue.Reason)
		e.Code = string(ue.Reason)
		e.Message = ue.Message
	}
	return status, e
}

// writeSendError surfaces a failed upstream send as an OpenAI-style error.
func writeSendError(w http.ResponseWriter, err error) {
	status, e := apiErrorForSend(err)
	writeError(w, status, e)
}

// upstreamErrorType maps the canonical FailureReason onto the OpenAI error
// type vocabulary clients branch on.
func upstreamErrorType(reason adapter.FailureReason) string {
	switch reason {
	case adapter.ReasonRateLimited:
		return "rate_limit_error"
	case adapter.ReasonAuthExpired:
		return "authentication_error"
	case adapter.ReasonNotFound:
		return errTypeInvalidRequest
	case adapter.ReasonQuotaExhausted:
		return "insufficient_quota"
	default:
		return errTypeServer
	}
}
