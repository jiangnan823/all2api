package router

import (
	"encoding/json"
	"strings"
	"testing"

	"all2api/internal/adapter"
)

// decodeContent re-types a converted message's content so assertions can look
// inside it without repeating RawMessage footwork at every call site.
func decodeContent(t *testing.T, m adapter.ChatMessage) (role string, content any) {
	t.Helper()
	if err := json.Unmarshal(m.Content, &content); err != nil {
		t.Fatalf("content is not valid JSON: %v", err)
	}
	return m.Role, content
}

func TestResponsesInputStringBecomesOneUserMessage(t *testing.T) {
	msgs, err := responsesInputToMessages(json.RawMessage(`"hi"`))
	if err != nil {
		t.Fatalf("string input: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("want one user message, got %+v", msgs)
	}
	if string(msgs[0].Content) != `"hi"` {
		t.Fatalf("content should pass through verbatim, got %s", msgs[0].Content)
	}
}

func TestResponsesInputNativeMessageItemMapsInputText(t *testing.T) {
	// The exact shape native Responses clients send — the one that reached
	// upstream verbatim and failed with "unsupported content type input_text".
	input := json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`)
	msgs, err := responsesInputToMessages(input)
	if err != nil {
		t.Fatalf("native message item: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want one message, got %d", len(msgs))
	}
	role, content := decodeContent(t, msgs[0])
	if role != "user" {
		t.Fatalf("role = %q, want user", role)
	}
	if s, ok := content.(string); !ok || s != "hi" {
		t.Fatalf("input_text should flatten to plain string content, got %v", content)
	}
}

func TestResponsesInputTextPartsJoinAndImagesMapToChatParts(t *testing.T) {
	input := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"look"},
			{"type":"input_image","image_url":{"url":"https://x/y.png"}},
			{"type":"input_text","text":"closely"}
		]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
	]`)
	msgs, err := responsesInputToMessages(input)
	if err != nil {
		t.Fatalf("mixed parts: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want two messages, got %d", len(msgs))
	}
	_, content := decodeContent(t, msgs[0])
	parts, ok := content.([]any)
	if !ok {
		t.Fatalf("image-bearing content should be a part array, got %v", content)
	}
	if len(parts) != 3 {
		t.Fatalf("want text+image+text parts, got %v", parts)
	}
	first, _ := parts[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "look" {
		t.Fatalf("first part = %v, want text 'look'", parts[0])
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("second part = %v, want image_url", parts[1])
	}
	// The assistant turn has no images: it must flatten back to a string.
	_, aContent := decodeContent(t, msgs[1])
	if s, ok := aContent.(string); !ok || s != "ok" {
		t.Fatalf("output_text should flatten to string, got %v", aContent)
	}
}

func TestResponsesInputBareChatShapeStillPasses(t *testing.T) {
	input := json.RawMessage(`[{"role":"user","content":"plain"}]`)
	msgs, err := responsesInputToMessages(input)
	if err != nil {
		t.Fatalf("bare chat shape: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("want one user message, got %+v", msgs)
	}
	if string(msgs[0].Content) != `"plain"` {
		t.Fatalf("string content should pass through, got %s", msgs[0].Content)
	}
}

func TestResponsesInputRejectsUntypedPartsAndForeignItems(t *testing.T) {
	if _, err := responsesInputToMessages(json.RawMessage(
		`[{"type":"function_call_output","call_id":"c","output":"{}"}]`)); err == nil ||
		!strings.Contains(err.Error(), "unsupported item type") {
		t.Fatalf("function_call_output should be a clear item-type error, got %v", err)
	}
	if _, err := responsesInputToMessages(json.RawMessage(
		`[{"type":"message","role":"user","content":[{"type":"audio","input_audio":{"data":"..."}}]}]`)); err == nil ||
		!strings.Contains(err.Error(), `unsupported content type "audio"`) {
		t.Fatalf("audio part should be a clear content-type error, got %v", err)
	}
	if _, err := responsesInputToMessages(json.RawMessage(
		`[{"type":"message","role":"user"}]`)); err == nil ||
		!strings.Contains(err.Error(), "content is required") {
		t.Fatalf("contentless message should error, got %v", err)
	}
	if _, err := responsesInputToMessages(nil); err == nil {
		t.Fatalf("nil input should error")
	}
}
