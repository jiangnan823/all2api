package router

// Streaming and /v1/responses tests: SSE frame sequence with terminal
// [DONE], mid-stream upstream error frames, the responses_api feature-flag
// two states, and client-disconnect ctx cancellation propagating into the
// sender.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"all2api/internal/adapter"
)

func cannedChunks() []adapter.StreamChunk {
	stop := "stop"
	role := "assistant"
	return []adapter.StreamChunk{
		{
			ID: "cc", Object: "chat.completion.chunk", Created: 1770000000, Model: "m",
			Choices: []adapter.StreamChoice{{Index: 0, Delta: adapter.ChatMessage{Role: role}}},
		},
		{
			ID: "cc", Object: "chat.completion.chunk", Created: 1770000000, Model: "m",
			Choices: []adapter.StreamChoice{{Index: 0, Delta: adapter.ChatMessage{Content: json.RawMessage(`"hi"`)}}},
		},
		{
			ID: "cc", Object: "chat.completion.chunk", Created: 1770000000, Model: "m",
			Choices: []adapter.StreamChoice{{Index: 0, FinishReason: &stop}},
		},
	}
}

// sseFrames splits an SSE body into its "data: ..." payloads.
func sseFrames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var frames []string
	for _, block := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n") {
		if block == "" {
			continue
		}
		if !strings.HasPrefix(block, "data: ") {
			t.Fatalf("SSE block must start with 'data: ', got %q", block)
		}
		frames = append(frames, strings.TrimPrefix(block, "data: "))
	}
	return frames
}

func TestV1StreamSSEFrames(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rstr")
	fs.chunks = cannedChunks()
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rstr")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"rstr/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream request: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content-type: got %q, want text/event-stream", ct)
	}

	frames := sseFrames(t, rec)
	if len(frames) != 4 { // 3 chunks + [DONE]
		t.Fatalf("frame count: got %d frames (%v), want 4", len(frames), frames)
	}
	for i := 0; i < 3; i++ {
		var chunk adapter.StreamChunk
		if err := json.Unmarshal([]byte(frames[i]), &chunk); err != nil {
			t.Fatalf("frame %d is not a JSON chunk: %v (%s)", i, err, frames[i])
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("frame %d object: got %q, want chat.completion.chunk", i, chunk.Object)
		}
	}
	if role := jsonGet(t, frames[0], "choices.0.delta.role"); role != "assistant" {
		t.Fatalf("first frame must carry the role delta, got %q", role)
	}
	if frames[3] != "[DONE]" {
		t.Fatalf("terminal frame: got %q, want [DONE]", frames[3])
	}
}

// jsonGet pulls one nested string out of a JSON payload (small test helper
// for the two stream-frame lookups).
func jsonGet(t *testing.T, payload, path string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	cur := v
	for _, seg := range strings.Split(path, ".") {
		if idx, aErr := strconv.Atoi(seg); aErr == nil {
			arr, ok := cur.([]any)
			if !ok || idx >= len(arr) {
				t.Fatalf("path %q: segment %q is not a valid array index in %s", path, seg, payload)
			}
			cur = arr[idx]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %q: segment %q does not sit on an object in %s", path, seg, payload)
		}
		cur = m[seg]
	}
	s, ok := cur.(string)
	if !ok {
		t.Fatalf("path %q does not resolve to a string in %s", path, payload)
	}
	return s
}

func TestV1StreamUpstreamErrorFrame(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rserr")
	fs.chunks = cannedChunks()
	fs.streamErr = &adapter.UpstreamError{Status: 500, Reason: adapter.ReasonPlatformDown, Message: "died mid-stream"}
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rserr")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/chat/completions", `{"model":"rserr/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mid-stream failure keeps the committed 200: got %d (body %s)", rec.Code, rec.Body)
	}
	frames := sseFrames(t, rec)
	if len(frames) != 4 {
		t.Fatalf("frame count: got %d (%v), want 3 chunks + error frame", len(frames), frames)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(frames[3]), &env); err != nil {
		t.Fatalf("terminal frame must be an error object: %v (%s)", err, frames[3])
	}
	if env.Error.Message != "died mid-stream" || env.Error.Type != "server_error" {
		t.Fatalf("error frame: got %+v", env.Error)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatal("a failed stream must NOT terminate with [DONE]")
	}
}

func TestV1ResponsesFeatureFlag(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0","router":{"responses_api":{"enabled":false}}}`)
	fs := newSender("rresp")
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rresp")
	mux := newMux(h, reg)

	rec := postJSON(t, mux, "/v1/responses", `{"model":"rresp/m","input":"ping"}`, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("responses disabled: got %d, want 501 (body %s)", rec.Code, rec.Body)
	}
	if _, errType, code := decodeErrEnvelope(t, rec); errType != "invalid_request_error" || code != "responses_api_disabled" {
		t.Fatalf("501 envelope: got type %q code %q", errType, code)
	}

	loadRouterConfig(t, `{"listen":":0","router":{"responses_api":{"enabled":true}}}`)
	rec = postJSON(t, mux, "/v1/responses", `{"model":"rresp/m","input":"ping"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("responses enabled: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	_, reqs := fs.callLog()
	if len(reqs) != 1 || reqs[0].Model != "m" {
		t.Fatalf("responses must route like chat (bare model), got %+v", reqs)
	}
	if len(reqs[0].Messages) != 1 || reqs[0].Messages[0].Role != "user" || string(reqs[0].Messages[0].Content) != `"ping"` {
		t.Fatalf("string input must map to one user message, got %+v", reqs[0].Messages)
	}

	rec = postJSON(t, mux, "/v1/responses", `{"model":"rresp/m","input":{"weird":true}}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported input shape: got %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	rec = postJSON(t, mux, "/v1/responses", `{"model":"rresp/m"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing input: got %d, want 400 (body %s)", rec.Code, rec.Body)
	}
}

func TestV1ClientDisconnectCancelsStream(t *testing.T) {
	loadRouterConfig(t, `{"listen":":0"}`)
	fs := newSender("rgone")
	fs.chunks = cannedChunks()[:1]
	fs.holdStream = true
	fs.consumed = make(chan struct{})
	fs.cancelSeen = make(chan struct{})
	reg := newRegistry(t, fs)
	h := newTestHost(t)
	addAccount(t, h, "a1", "rgone")
	mux := newMux(h, reg)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"rgone/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(handlerDone)
	}()

	select {
	case <-fs.consumed: // handler pulled chunk 0 — stream is live
	case <-time.After(5 * time.Second):
		t.Fatal("handler never pulled the first chunk")
	}
	cancel()

	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler must return once the client context cancels")
	}
	select {
	case <-fs.cancelSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation must propagate into the sender's send context")
	}
	if body := rec.Body.String(); !strings.Contains(body, "data: ") || strings.Contains(body, "[DONE]") {
		t.Fatalf("aborted stream must keep the partial frames without [DONE], got %q", body)
	}
}
