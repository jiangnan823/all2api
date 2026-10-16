package adapter

import (
	"context"
	"encoding/json"
	"fmt"

	"all2api/internal/pool"
)

// Upstream send contract (M2 router): OpenAI-wire-shaped chat types plus the
// Sender interface a platform adapter implements once its native HTTP client
// lands. The router consumes only this contract — it never speaks a platform
// protocol itself, and adapters that do not (yet) implement Sender are simply
// unroutable and answer 503 at the /v1 surface.

// ChatMessage is one conversation turn in the OpenAI chat.completions shape.
// Content stays RawMessage because OpenAI allows either a plain string or an
// array of content parts — re-typing it would distort passthrough traffic.
type ChatMessage struct {
	Role       string          `json:"role"`                   // system / user / assistant / tool
	Content    json.RawMessage `json:"content,omitempty"`      // string or content-part array; absent on tool-call-only turns
	Name       string          `json:"name,omitempty"`         // optional participant name
	ToolCallID string          `json:"tool_call_id,omitempty"` // tool role: which call this answers
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`   // assistant role: requested tool calls
	// ReasoningContent carries the provider-extension chain-of-thought
	// (delta.reasoning_content) that thinking models ship alongside content.
	// Strict-OpenAI clients ignore unknown fields; omitempty keeps the wire
	// clean for platforms without the concept.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ChatRequest is a chat completion request as received on /v1/chat/completions.
// Model carries the bare model name — the router strips any "platform/" prefix
// before dispatch, since the prefix is a gateway routing concern only.
type ChatRequest struct {
	Model       string          `json:"model"`
	Messages    []ChatMessage   `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`        // string or array
	Tools       json.RawMessage `json:"tools,omitempty"`       // tool definitions
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"` // "none"|"auto"|{...}
	User        string          `json:"user,omitempty"`
	// ReasoningEffort is the OpenAI-style reasoning-effort hint ("low" /
	// "medium" / "high", "off" to disable). Platforms without the concept
	// ignore it; workbuddy maps it per protocol §7 (off → thinking.disabled,
	// the flat field itself 400s on "off").
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// Usage reports token accounting for a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatChoice is one completion alternative of a ChatResponse.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason *string     `json:"finish_reason"` // "stop"/"length"/"tool_calls"; null while streaming
}

// ChatResponse is the non-streaming OpenAI chat.completion object.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"` // "chat.completion"
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *Usage       `json:"usage,omitempty"`
}

// StreamChoice is one choice of a StreamChunk: Delta carries only the newly
// generated fragment, FinishReason stays null until the terminal chunk.
type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        ChatMessage `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// StreamChunk is one OpenAI chat.completion.chunk frame of a streamed reply.
type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"` // "chat.completion.chunk"
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
}

// UpstreamError is the classified failure a Sender reports for a send attempt.
// Status is the HTTP status the gateway should surface (clamped to 502 when
// unset or out of range); Reason feeds the pool health vocabulary and the
// OpenAI error envelope's type/code fields.
type UpstreamError struct {
	Status  int           `json:"status"`
	Reason  FailureReason `json:"reason"`
	Message string        `json:"message"`
}

// Error implements the error interface.
func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %s: %s", e.Reason, e.Message)
}

// Sender is the upstream-send capability: it performs one chat completion
// against the platform using the pool-selected account. Implemented by
// platform adapters; the router type-asserts adapters onto it.
//
// Channel contract for SendChatStream:
//   - chunks carries the reply frames in order and MUST be closed by the
//     sender when the stream completes normally;
//   - errCh carries at most one error for a failed stream and must NOT be
//     closed alongside a healthy stream (an open, idle errCh is the idle state);
//   - both goroutines and channels must terminate when ctx is done — the
//     router cancels ctx as soon as the client disconnects.
type Sender interface {
	// SendChat performs one non-streaming chat completion.
	SendChat(ctx context.Context, acct *pool.Account, req ChatRequest) (*ChatResponse, error)

	// SendChatStream starts one streaming chat completion; a non-nil error
	// return means no bytes were produced and the attempt failed outright.
	SendChatStream(ctx context.Context, acct *pool.Account, req ChatRequest) (<-chan StreamChunk, <-chan error, error)
}
