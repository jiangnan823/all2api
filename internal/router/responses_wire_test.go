package router

import (
	"encoding/json"
	"strings"
	"testing"

	"all2api/internal/adapter"
)

// TestResponsesObjectOfShape pins the native non-stream envelope: output
// items in reasoning → message → function_call order, Responses usage keys.
func TestResponsesObjectOfShape(t *testing.T) {
	calls := []map[string]any{{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": "list_files", "arguments": `{"path":"."}`},
	}}
	raw, err := json.Marshal(calls)
	if err != nil {
		t.Fatal(err)
	}
	resp := &adapter.ChatResponse{
		ID: "cmb-1", Object: "chat.completion", Created: 1700000000, Model: "glm-5.3-flash",
		Choices: []adapter.ChatChoice{{
			Index: 0,
			Message: adapter.ChatMessage{
				Role: "assistant", Content: json.RawMessage(`"hello"`),
				ReasoningContent: "thinking…",
				ToolCalls:        raw,
			},
			FinishReason: nil,
		}},
		Usage: &adapter.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
	}
	obj := responsesObjectOf(resp)
	if obj["object"] != "response" || obj["status"] != "completed" {
		t.Fatalf("object/status = %v/%v", obj["object"], obj["status"])
	}
	if obj["id"] != "resp_cmb-1" {
		t.Fatalf("id = %v, want resp_cmb-1", obj["id"])
	}
	out, ok := obj["output"].([]map[string]any)
	if !ok || len(out) != 3 {
		t.Fatalf("output = %v, want 3 items", obj["output"])
	}
	if out[0]["type"] != "reasoning" || out[1]["type"] != "message" || out[2]["type"] != "function_call" {
		t.Fatalf("item order = %v/%v/%v, want reasoning/message/function_call", out[0]["type"], out[1]["type"], out[2]["type"])
	}
	msgJSON, _ := json.Marshal(out[1])
	if !strings.Contains(string(msgJSON), `"output_text"`) || !strings.Contains(string(msgJSON), "hello") {
		t.Fatalf("message item = %s", msgJSON)
	}
	usage, _ := obj["usage"].(map[string]any)
	if usage == nil || usage["total_tokens"] != 12 {
		t.Fatalf("usage = %v, want total 12", obj["usage"])
	}
}

// TestResponsesStreamEventSequence drives the state machine over a mixed
// chunk stream (reasoning → text → tool call → finish) and pins the event
// order and the terminal completed frame.
func TestResponsesStreamEventSequence(t *testing.T) {
	acc := newResponsesStream()
	chunks := []adapter.StreamChunk{
		{ID: "cmb-9", Model: "m", Created: 1, Choices: []adapter.StreamChoice{{
			Index: 0,
			Delta: adapter.ChatMessage{Role: "assistant", ReasoningContent: "think"},
		}}},
		{ID: "cmb-9", Model: "m", Choices: []adapter.StreamChoice{{
			Index: 0,
			Delta: adapter.ChatMessage{Content: json.RawMessage(`"he"`), ReasoningContent: " more"},
		}}},
		{ID: "cmb-9", Model: "m", Choices: []adapter.StreamChoice{{
			Index: 0,
			Delta: adapter.ChatMessage{Content: json.RawMessage(`"llo"`)},
		}}},
		{ID: "cmb-9", Model: "m", Choices: []adapter.StreamChoice{{
			Index: 0,
			Delta: adapter.ChatMessage{ToolCalls: json.RawMessage(`[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"x\""}}]`)},
		}}},
		{ID: "cmb-9", Model: "m", Choices: []adapter.StreamChoice{{
			Index:        0,
			Delta:        adapter.ChatMessage{ToolCalls: json.RawMessage(`[{"index":0,"function":{"arguments":":1}"}}]`)},
			FinishReason: strPtr("tool_calls"),
		}}},
	}
	var types []string
	for _, c := range chunks {
		for _, ev := range acc.feed(c) {
			types = append(types, ev["type"].(string))
		}
	}
	for _, ev := range acc.finish() {
		types = append(types, ev["type"].(string))
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added",            // reasoning
		"response.reasoning_summary_text.delta", // "think"
		"response.reasoning_summary_text.delta", // " more"
		"response.output_item.added",            // message
		"response.content_part.added",
		"response.output_text.delta",                              // "he"
		"response.output_text.delta",                              // "llo"
		"response.output_item.added",                              // function_call
		"response.function_call_arguments.delta",                  // {"x"
		"response.function_call_arguments.delta",                  // :1}
		"response.output_item.done",                               // reasoning close
		"response.content_part.done", "response.output_item.done", // message close
		"response.function_call_arguments.done", "response.output_item.done", // fc close
		"response.completed",
	}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event[%d] = %s, want %s\nall: %v", i, types[i], want[i], types)
		}
	}
	// The completed frame carries the fully assembled items.
	completed := acc.finish() // idempotent: closed items produce only the completed frame? — feed already closed
	_ = completed
	// Rebuild from scratch to inspect the completed payload deterministically.
	acc2 := newResponsesStream()
	var last map[string]any
	for _, c := range chunks {
		for _, ev := range acc2.feed(c) {
			last = ev
		}
	}
	for _, ev := range acc2.finish() {
		last = ev
	}
	if last["type"] != "response.completed" {
		t.Fatalf("terminal = %v", last["type"])
	}
	respObj := last["response"].(map[string]any)
	items := respObj["output"].([]map[string]any)
	if len(items) != 3 {
		t.Fatalf("completed output = %d items, want 3", len(items))
	}
	fc := items[2]
	if fc["arguments"] != `{"x":1}` {
		t.Fatalf("fc arguments = %v, want merged {\"x\":1}", fc["arguments"])
	}
	if items[0]["summary"] == nil {
		t.Fatalf("reasoning summary missing: %v", items[0])
	}
}

// TestResponsesFailedEventShape pins the mid-stream failure frame.
func TestResponsesFailedEventShape(t *testing.T) {
	ev := responsesFailedEvent("cmb-1", "m", "upstream_error", "boom")
	if ev["type"] != "response.failed" {
		t.Fatalf("type = %v", ev["type"])
	}
	b, _ := json.Marshal(ev)
	if !strings.Contains(string(b), `"status":"failed"`) || !strings.Contains(string(b), "boom") {
		t.Fatalf("failed frame = %s", b)
	}
}

func strPtr(s string) *string { return &s }
