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

	"all2api/internal/adapter"
	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/pool"
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

	var lastErr error
	for {
		if err := r.Context().Err(); err != nil {
			return // client went away — nothing left to report or bill
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

		if req.Stream {
			committed, streamErr := rt.relayStream(w, r.Context(), sender, acct, req)
			if !committed {
				// Outright stream-start failure: same retry path as SendChat.
				lastErr = streamErr
				rt.host.Pool.UpdateHealth(platform, acct.ID, healthStatus(streamErr), 0)
				continue
			}
			if streamErr == nil {
				rt.host.Pool.UpdateHealth(platform, acct.ID, "success", 0)
			} else {
				rt.host.Pool.UpdateHealth(platform, acct.ID, healthStatus(streamErr), 0)
			}
			return
		}

		resp, err := sender.SendChat(r.Context(), acct, req)
		if err == nil {
			rt.host.Pool.UpdateHealth(platform, acct.ID, "success", 0)
			writeJSON(w, http.StatusOK, resp)
			return
		}
		lastErr = err
		rt.host.Pool.UpdateHealth(platform, acct.ID, healthStatus(err), 0)
	}
}

// route resolves the requested model onto a registered adapter: a
// "platform/model" prefix addresses the platform by name; a bare model name
// auto-routes to the first adapter carrying CapChatCompletion (registry
// registration order — deterministic per binary).
func (rt *Router) route(model string) (adapter.Adapter, string, bool) {
	var platform string
	if i := strings.Index(model, "/"); i > 0 {
		platform = model[:i]
	} else {
		names := rt.lookup.ByCap(adapter.CapChatCompletion)
		if len(names) == 0 {
			return nil, "", false
		}
		platform = names[0]
	}
	a := rt.lookup.Get(platform)
	if a == nil {
		return nil, "", false
	}
	return a, platform, true
}

// relayStream forwards SendChatStream chunks to the client as
// text/event-stream frames ("data: {chunk}\n\n") and terminates with
// "data: [DONE]". committed=false means nothing was written and the caller
// may retry on the next account; once committed, the status line is out and
// a mid-stream failure can only travel as an in-band error frame (followed
// by stream close without [DONE], so clients must not treat it as complete).
func (rt *Router) relayStream(w http.ResponseWriter, ctx context.Context, sender adapter.Sender, acct *pool.Account, req adapter.ChatRequest) (committed bool, streamErr error) {
	chunks, errCh, err := sender.SendChatStream(ctx, acct, req)
	if err != nil {
		return false, err
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
			return true, ctx.Err()
		case chunk, ok := <-chunks:
			if !ok {
				writeFrame("[DONE]")
				return true, nil
			}
			payload, mErr := json.Marshal(chunk)
			if mErr != nil {
				return true, mErr
			}
			if !writeFrame(string(payload)) {
				return true, nil
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
			return true, upErr
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
