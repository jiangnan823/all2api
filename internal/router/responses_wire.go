package router

import (
	"encoding/json"
	"strconv"
	"strings"

	"all2api/internal/adapter"
)

// responses_wire.go shapes the native Responses API surface — the protocol
// ZCode providers speak when "API 格式 = Responses": the response object for
// non-streaming, and the typed SSE event stream (response.created →
// output_item.added → output_text.delta → … → response.completed) for
// streaming. The chat.completion pipeline stays untouched; the two write
// points in dispatch branch on the protocol tag.

// ---------- item builders (shared by the object and the stream) ----------

const (
	prefixResponse = "resp_"
	prefixMessage  = "msg_"
	prefixReason   = "rs_"
	prefixFunc     = "fc_"
)

// responsesID derives a typed item id from the upstream completion id so ids
// stay stable across the stream and the final object.
func responsesID(prefix, upstreamID, suffix string) string {
	base := strings.TrimPrefix(upstreamID, prefixResponse)
	if base == "" {
		base = "local"
	}
	return prefix + base + suffix
}

func reasoningItem(id, text string) map[string]any {
	return map[string]any{
		"type": "reasoning",
		"id":   id,
		"summary": []map[string]any{
			{"type": "summary_text", "text": text},
		},
	}
}

func messageItem(id, text string) map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     id,
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{
			{"type": "output_text", "text": text, "annotations": []any{}},
		},
	}
}

func funcCallItem(id, callID, name, args string) map[string]any {
	return map[string]any{
		"type":      "function_call",
		"id":        id,
		"call_id":   callID,
		"name":      name,
		"arguments": args,
		"status":    "completed",
	}
}

// rawText unwraps a chat content RawMessage that carries a plain string.
func rawText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// toolCallFragments is the tolerant mirror of a chat tool_calls delta.
type toolCallFragments []struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// responsesUsageOf maps the chat usage onto the Responses usage object.
func responsesUsageOf(u *adapter.Usage) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"input_tokens":  u.PromptTokens,
		"output_tokens": u.CompletionTokens,
		"total_tokens":  u.TotalTokens,
	}
}

// ---------- non-streaming envelope ----------

// responsesObjectOf converts the aggregated chat response onto the native
// response object: reasoning item, message item, then function_call items.
func responsesObjectOf(resp *adapter.ChatResponse) map[string]any {
	var out []map[string]any
	// Single-choice pipeline: the gateway relays one choice per request.
	if len(resp.Choices) > 0 {
		msg := resp.Choices[0].Message
		if msg.ReasoningContent != "" {
			out = append(out, reasoningItem(responsesID(prefixReason, resp.ID, "_0"), msg.ReasoningContent))
		}
		if text := rawText(msg.Content); text != "" {
			out = append(out, messageItem(responsesID(prefixMessage, resp.ID, "_1"), text))
		}
		if len(msg.ToolCalls) > 0 {
			var frags toolCallFragments
			if json.Unmarshal(msg.ToolCalls, &frags) == nil {
				for _, f := range frags {
					out = append(out, funcCallItem(
						responsesID(prefixFunc, resp.ID, "_"+strconv.Itoa(f.Index)),
						f.ID, f.Function.Name, f.Function.Arguments))
				}
			}
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	return map[string]any{
		"id":         responsesID(prefixResponse, resp.ID, ""),
		"object":     "response",
		"created_at": resp.Created,
		"status":     "completed",
		"model":      resp.Model,
		"output":     out,
		"usage":      responsesUsageOf(resp.Usage),
		"error":      nil,
	}
}

// itoa is a tiny index formatter avoiding strconv imports in hot paths.

// ---------- streaming state machine ----------

// responsesStreamCall tracks one open function_call item.
type responsesStreamCall struct {
	outputIdx int
	callID    string
	name      string
	args      strings.Builder
}

// responsesStream folds chat chunks into native Responses events. Feed each
// relayed chunk; Take returns the events to write (possibly none); Completed
// closes the stream, Failed replaces it on upstream error.
type responsesStream struct {
	respID  string
	model   string
	created int64
	seq     int64
	nextIdx int

	rsOpen, rsDone bool
	rsID           string
	rsIdx          int
	rsText         strings.Builder

	msgOpen, msgDone bool
	msgID            string
	msgIdx           int
	msgText          strings.Builder

	fcOpen  map[int]bool
	fcDone  map[int]bool
	fcs     map[int]*responsesStreamCall
	fcOrder []int
}

func newResponsesStream() *responsesStream {
	return &responsesStream{
		fcOpen: map[int]bool{},
		fcDone: map[int]bool{},
		fcs:    map[int]*responsesStreamCall{},
	}
}

// ev builds one event frame with the sequence number stamped.
func (s *responsesStream) ev(typ string, kv ...any) map[string]any {
	m := map[string]any{"type": typ, "sequence_number": s.seq}
	s.seq++
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

// responseSnapshot assembles the response object as it stands (used by
// created/in_progress/completed events).
func (s *responsesStream) responseSnapshot(status string) map[string]any {
	var out []map[string]any
	if s.rsOpen {
		out = append(out, reasoningItem(s.rsID, s.rsText.String()))
	}
	if s.msgOpen {
		out = append(out, messageItem(s.msgID, s.msgText.String()))
	}
	for _, idx := range s.fcOrder {
		c := s.fcs[idx]
		out = append(out, funcCallItem(responsesID(prefixFunc, s.respID, "_fc"+strconv.Itoa(idx)), c.callID, c.name, c.args.String()))
	}
	if out == nil {
		out = []map[string]any{}
	}
	return map[string]any{
		"id":         responsesID(prefixResponse, s.respID, ""),
		"object":     "response",
		"created_at": s.created,
		"status":     status,
		"model":      s.model,
		"output":     out,
		"error":      nil,
	}
}

// openReasoning emits the reasoning item header once.
func (s *responsesStream) openReasoning() []map[string]any {
	if s.rsOpen {
		return nil
	}
	s.rsOpen = true
	s.rsID = responsesID(prefixReason, s.respID, "_rs0")
	s.rsIdx = s.nextIdx
	s.nextIdx++
	return []map[string]any{s.ev("response.output_item.added",
		"output_index", s.rsIdx,
		"item", reasoningItem(s.rsID, ""))}
}

// openMessage emits the message item + content part headers once.
func (s *responsesStream) openMessage() []map[string]any {
	if s.msgOpen {
		return nil
	}
	s.msgOpen = true
	s.msgID = responsesID(prefixMessage, s.respID, "_msg0")
	s.msgIdx = s.nextIdx
	s.nextIdx++
	return []map[string]any{
		s.ev("response.output_item.added",
			"output_index", s.msgIdx,
			"item", map[string]any{
				"type": "message", "id": s.msgID, "role": "assistant", "status": "in_progress",
				"content": []any{},
			}),
		s.ev("response.content_part.added",
			"item_id", s.msgID, "output_index", s.msgIdx, "content_index", 0,
			"part", map[string]any{"type": "output_text", "text": "", "annotations": []any{}}),
	}
}

// feed translates one upstream chunk into the native events it implies.
func (s *responsesStream) feed(chunk adapter.StreamChunk) []map[string]any {
	var events []map[string]any
	if s.respID == "" {
		s.respID = chunk.ID
		s.model = chunk.Model
		s.created = chunk.Created
		snapshot := s.responseSnapshot("in_progress")
		events = append(events,
			s.ev("response.created", "response", snapshot),
			s.ev("response.in_progress", "response", snapshot))
	}
	if len(chunk.Choices) == 0 {
		return events
	}
	delta := chunk.Choices[0].Delta

	if delta.ReasoningContent != "" {
		events = append(events, s.openReasoning()...)
		s.rsText.WriteString(delta.ReasoningContent)
		events = append(events, s.ev("response.reasoning_summary_text.delta",
			"item_id", s.rsID, "output_index", s.rsIdx, "summary_index", 0,
			"delta", delta.ReasoningContent))
	}

	if text := rawText(delta.Content); text != "" {
		events = append(events, s.openMessage()...)
		s.msgText.WriteString(text)
		events = append(events, s.ev("response.output_text.delta",
			"item_id", s.msgID, "output_index", s.msgIdx, "content_index", 0,
			"delta", text))
	}

	if len(delta.ToolCalls) > 0 {
		var frags toolCallFragments
		if json.Unmarshal(delta.ToolCalls, &frags) == nil {
			for _, f := range frags {
				st, seen := s.fcs[f.Index]
				if !seen {
					st = &responsesStreamCall{outputIdx: s.nextIdx, callID: f.ID, name: f.Function.Name}
					s.nextIdx++
					s.fcs[f.Index] = st
					s.fcOrder = append(s.fcOrder, f.Index)
					events = append(events, s.ev("response.output_item.added",
						"output_index", st.outputIdx,
						"item", map[string]any{
							"type": "function_call", "id": responsesID(prefixFunc, s.respID, "_fc"+strconv.Itoa(f.Index)),
							"call_id": st.callID, "name": st.name, "arguments": "",
							"status": "in_progress",
						}))
				} else if f.ID != "" && st.callID == "" {
					st.callID = f.ID
				}
				if f.Function.Name != "" && st.name == "" {
					st.name = f.Function.Name
				}
				if f.Function.Arguments != "" {
					st.args.WriteString(f.Function.Arguments)
					events = append(events, s.ev("response.function_call_arguments.delta",
						"item_id", responsesID(prefixFunc, s.respID, "_fc"+strconv.Itoa(f.Index)),
						"output_index", st.outputIdx,
						"delta", f.Function.Arguments))
				}
			}
		}
	}

	if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
		events = append(events, s.closeItems()...)
	}
	return events
}

// closeItems emits the terminal part/item events for every open output item
// (idempotent — finish_reason may arrive more than once across retries).
func (s *responsesStream) closeItems() []map[string]any {
	var events []map[string]any
	if s.rsOpen && !s.rsDone {
		s.rsDone = true
		events = append(events, s.ev("response.output_item.done",
			"output_index", s.rsIdx, "item", reasoningItem(s.rsID, s.rsText.String())))
	}
	if s.msgOpen && !s.msgDone {
		s.msgDone = true
		events = append(events,
			s.ev("response.content_part.done",
				"item_id", s.msgID, "output_index", s.msgIdx, "content_index", 0,
				"part", map[string]any{"type": "output_text", "text": s.msgText.String(), "annotations": []any{}}),
			s.ev("response.output_item.done",
				"output_index", s.msgIdx, "item", messageItem(s.msgID, s.msgText.String())))
	}
	for _, idx := range s.fcOrder {
		if s.fcDone[idx] {
			continue
		}
		s.fcDone[idx] = true
		c := s.fcs[idx]
		itemID := responsesID(prefixFunc, s.respID, "_fc"+strconv.Itoa(idx))
		events = append(events,
			s.ev("response.function_call_arguments.done",
				"item_id", itemID, "output_index", c.outputIdx,
				"arguments", c.args.String()),
			s.ev("response.output_item.done",
				"output_index", c.outputIdx,
				"item", funcCallItem(itemID, c.callID, c.name, c.args.String())))
	}
	return events
}

// finish emits the closing events: item closes (idempotent after
// finish_reason) then response.completed. The native stream does NOT send a
// data:[DONE] terminator — response.completed is the end.
func (s *responsesStream) finish() []map[string]any {
	events := s.closeItems()
	events = append(events, s.ev("response.completed", "response", s.responseSnapshot("completed")))
	return events
}

// responsesFailedEvent builds the response.failed frame for a mid-stream
// upstream error.
func responsesFailedEvent(respID, model string, code, message string) map[string]any {
	return map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id":     responsesID(prefixResponse, respID, ""),
			"object": "response",
			"status": "failed",
			"model":  model,
			"error": map[string]any{
				"code":    code,
				"message": message,
			},
		},
	}
}
