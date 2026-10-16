//go:build qoder || full || (!qoder && !trae && !workbuddy)

// chat.go maps the gateway ChatRequest onto one Qoder chat exchange: request
// body construction (COSY chat envelope + Qoder message shapes), the upstream
// SSE open/retry loop, frame relay/aggregation and error classification onto
// the canonical adapter.UpstreamError vocabulary.
package qoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/pool"
)

// busyCode is the gateway's business code for queue/admission-control
// rejections. It rides HTTP 401/403 envelopes (and in-stream frames with a
// matching statusCodeValue) even though the session is perfectly valid:
//
//	{"code":"10605","message":"{\"isQueued\":false,...,\"retryAfterSeconds\":2,...}"}
//
// Treating it as an auth failure would cause a pointless PAT re-exchange
// followed by an immediate retry that slams into the same window.
const busyCode = "10605"

// busySignal marks a queue/admission-control rejection for the retry loop;
// it converts to a ReasonRateLimited failure when given to the caller.
type busySignal struct {
	StatusCode int
	Message    string
	RetryAfter time.Duration
}

func (e *busySignal) Error() string {
	s := fmt.Sprintf("qoder: gateway busy (%s)", busyCode)
	if e.RetryAfter > 0 {
		s += " (retry after " + e.RetryAfter.String() + ")"
	}
	return s
}

// --- Request body (COSY chat envelope) ---

type chatContextText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type extraModelConfig struct {
	IsReasoning bool   `json:"is_reasoning"`
	Key         string `json:"key"`
	IsVL        bool   `json:"is_vl,omitempty"`
}

type originalContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type chatContextExtra struct {
	ModelConfig     extraModelConfig `json:"modelConfig"`
	OriginalContent originalContent  `json:"originalContent"`
}

type chatContext struct {
	Extra chatContextExtra `json:"extra"`
	Text  chatContextText  `json:"text"`
}

type modelConfig struct {
	Key         string `json:"key"`
	IsVL        bool   `json:"is_vl"`
	IsReasoning bool   `json:"is_reasoning"`
	Source      string `json:"source"`
}

type business struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BeginAt int64  `json:"begin_at"`
}

// chatRequestBody is the Qoder chat request body. Field order mirrors the
// official client's serialization (signature consistency).
type chatRequestBody struct {
	RequestID         string          `json:"request_id"`
	RequestSetID      string          `json:"request_set_id"`
	ChatRecordID      string          `json:"chat_record_id"`
	Stream            bool            `json:"stream"`
	ChatTask          string          `json:"chat_task"`
	ChatContext       chatContext     `json:"chat_context"`
	SessionID         string          `json:"session_id"`
	Source            int             `json:"source"`
	Version           string          `json:"version"`
	AliyunUserType    string          `json:"aliyun_user_type"`
	SessionType       string          `json:"session_type"`
	AgentID           string          `json:"agent_id"`
	TaskID            string          `json:"task_id"`
	ModelConfig       modelConfig     `json:"model_config"`
	Messages          []qoderMessage  `json:"messages"`
	Business          business        `json:"business"`
	Parameters        json.RawMessage `json:"parameters"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	ParallelToolCalls json.RawMessage `json:"parallel_tool_calls,omitempty"`
}

// --- Qoder message shapes ---

// metaUsage keeps the official client's field order (details objects last,
// reasoning before cached).
type metaUsage struct {
	PromptTokens            int                        `json:"prompt_tokens"`
	CompletionTokens        int                        `json:"completion_tokens"`
	TotalTokens             int                        `json:"total_tokens"`
	CompletionTokensDetails metaUsageCompletionDetails `json:"completion_tokens_details"`
	PromptTokensDetails     metaUsagePromptDetails     `json:"prompt_tokens_details"`
}

type responseMeta struct {
	ID    string    `json:"id"`
	Usage metaUsage `json:"usage"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

// metaUsage keeps the official client's field order: the two token counters,
// then the per-side detail objects (reasoning tokens on completion, cached
// tokens on prompt).
type metaUsageCompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type metaUsagePromptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// contentPart is one entry of a user message's contents array.
type contentPart struct {
	Type     string        `json:"type"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
	Text     string        `json:"text,omitempty"`
}

type normalizedToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type normalizedToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function normalizedToolFunction `json:"function"`
}

// qoderMessage is one conversation turn in the Qoder chat body. Field order
// matches the official client (response_meta / reasoning_content_signature
// are always present, contents/tool_calls/name/tool_call_id when non-empty).
type qoderMessage struct {
	Role                string               `json:"role"`
	Content             string               `json:"content"`
	ResponseMeta        responseMeta         `json:"response_meta"`
	ReasoningContentSig string               `json:"reasoning_content_signature"`
	Contents            []contentPart        `json:"contents,omitempty"`
	ToolCalls           []normalizedToolCall `json:"tool_calls,omitempty"`
	Name                string               `json:"name,omitempty"`
	ToolCallID          string               `json:"tool_call_id,omitempty"`
}

func blankMeta() responseMeta { return responseMeta{} }

func buildUserMessage(text string, images []string) qoderMessage {
	parts := make([]contentPart, 0, len(images)+1)
	for _, u := range images {
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURLPart{URL: u}})
	}
	if strings.TrimSpace(text) != "" || len(parts) == 0 {
		parts = append(parts, contentPart{Type: "text", Text: text})
	}
	return qoderMessage{
		Role:         "user",
		ResponseMeta: blankMeta(),
		Contents:     parts,
	}
}

func buildStructuredMessage(role, text string) qoderMessage {
	return qoderMessage{Role: role, Content: text, ResponseMeta: blankMeta()}
}

// --- Message conversion ---

// buildMessages converts the gateway's OpenAI-shaped messages onto the Qoder
// wire shapes. Ported from the reference transform: developer renames to
// system, user turns carry a contents array (text + image_url parts), tool
// round-trips ride structured tool_calls / tool messages when tools are in
// play and degrade to rendered text otherwise.
func buildMessages(msgs []adapter.ChatMessage, toolsEnabled bool) []qoderMessage {
	out := make([]qoderMessage, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		role := m.Role
		if role == "" {
			role = "user"
		}
		if role == "developer" {
			// The upstream accepts system but rejects the OpenAI-only
			// developer role.
			role = "system"
		}
		text := messageText(m.Content)
		images := messageImages(m.Content)
		var toolCalls []normalizedToolCall
		if toolsEnabled {
			toolCalls = normalizeToolCalls(m.ToolCalls)
		}
		if len(images) > 0 {
			text = stripImageMarkers(text)
		}

		switch role {
		case "assistant":
			if !toolsEnabled {
				// Tool calls without a tools declaration degrade to rendered
				// text the model can still reason about (reference behavior).
				if calls := normalizeToolCalls(m.ToolCalls); len(calls) > 0 {
					text = joinSections(text, renderToolCallsText(calls))
				}
				out = append(out, buildStructuredMessage("assistant", text))
				continue
			}
			if len(toolCalls) > 0 && hasResolvedToolResponse(msgs, i) {
				msg := buildStructuredMessage("assistant", text)
				msg.ToolCalls = toolCalls
				out = append(out, msg)
				continue
			}
			if toolsEnabled && len(toolCalls) > 0 {
				// Unresolved tool calls degrade to a text summary the model
				// can reason about (reference behavior).
				text = joinSections(text, summarizeUnresolvedToolCalls(toolCalls))
			}
			out = append(out, buildStructuredMessage("assistant", text))
		case "tool":
			if toolsEnabled {
				msg := buildStructuredMessage("tool", text)
				msg.Name = m.Name
				msg.ToolCallID = m.ToolCallID
				out = append(out, msg)
				continue
			}
			out = append(out, buildUserMessage(renderToolResult(m.Name, m.ToolCallID, text), nil))
		case "user":
			if strings.TrimSpace(text) == "" && len(images) == 0 {
				continue // empty turn contributes nothing upstream
			}
			out = append(out, buildUserMessage(text, images))
		default: // system and anything unrecognized
			out = append(out, buildStructuredMessage(role, text))
		}
	}
	return out
}

// hasResolvedToolResponse reports whether the assistant message at index i is
// followed by a tool answer before the next non-tool turn.
func hasResolvedToolResponse(msgs []adapter.ChatMessage, assistantIndex int) bool {
	if len(msgs[assistantIndex].ToolCalls) == 0 {
		return false
	}
	for i := assistantIndex + 1; i < len(msgs); i++ {
		switch msgs[i].Role {
		case "tool":
			return true
		case "assistant", "user", "system", "developer":
			return false
		}
	}
	return false
}

// messageText flattens an OpenAI content field (string or parts array) to the
// textual form; image parts become "[image] <url>" markers that are stripped
// again when the parts are carried structurally.
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	sections := make([]string, 0, len(parts))
	for _, p := range parts {
		var part struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(p, &part) != nil {
			continue
		}
		switch part.Type {
		case "text":
			if part.Text != "" {
				sections = append(sections, part.Text)
			}
		case "image_url":
			if part.ImageURL.URL != "" {
				sections = append(sections, "[image] "+part.ImageURL.URL)
			}
		}
	}
	return strings.Join(sections, "\n\n")
}

// messageImages extracts the image URLs of a content array.
func messageImages(raw json.RawMessage) []string {
	var parts []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var urls []string
	for _, p := range parts {
		var part struct {
			Type     string `json:"type"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(p, &part) == nil && part.Type == "image_url" && part.ImageURL.URL != "" {
			urls = append(urls, part.ImageURL.URL)
		}
	}
	return urls
}

// stripImageMarkers removes the "[image] <url>" textual fallback once the
// images ride structurally.
func stripImageMarkers(text string) string {
	segments := strings.Split(text, "\n\n")
	filtered := make([]string, 0, len(segments))
	for _, seg := range segments {
		s := strings.TrimSpace(seg)
		if s != "" && !strings.HasPrefix(s, "[image] ") {
			filtered = append(filtered, s)
		}
	}
	return strings.Join(filtered, "\n\n")
}

// normalizeToolCalls converts the OpenAI tool_calls array onto the Qoder
// normalized shape; unparseable input yields nil.
func normalizeToolCalls(raw json.RawMessage) []normalizedToolCall {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fragments []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &fragments) != nil || len(fragments) == 0 {
		return nil
	}
	out := make([]normalizedToolCall, 0, len(fragments))
	for _, f := range fragments {
		typ := f.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, normalizedToolCall{ID: f.ID, Type: typ, Function: normalizedToolFunction{Name: f.Function.Name, Arguments: f.Function.Arguments}})
	}
	return out
}

func renderToolCallsText(calls []normalizedToolCall) string {
	maps := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		maps = append(maps, map[string]any{
			"id":   c.ID,
			"type": c.Type,
			"function": map[string]string{
				"name":      c.Function.Name,
				"arguments": c.Function.Arguments,
			},
		})
	}
	b, _ := marshalNoEscape(maps)
	return "Tool calls:\n" + string(b)
}

func summarizeUnresolvedToolCalls(calls []normalizedToolCall) string {
	limit := len(calls)
	if limit > 6 {
		limit = 6
	}
	names := make([]string, 0, limit)
	for _, c := range calls[:limit] {
		if c.Function.Name != "" {
			names = append(names, c.Function.Name)
		} else {
			names = append(names, "unknown")
		}
	}
	sb := &strings.Builder{}
	sb.WriteString("Previously planned but unexecuted tool calls")
	if len(names) > 0 {
		sb.WriteString(": ")
		sb.WriteString(strings.Join(names, ", "))
	}
	if len(calls) > limit {
		fmt.Fprintf(sb, " and %d more", len(calls)-limit)
	}
	sb.WriteString(".")
	return sb.String()
}

func renderToolResult(name, toolCallID, text string) string {
	parts := []string{"Tool result"}
	if name != "" {
		parts = append(parts, fmt.Sprintf(" (%s)", name))
	}
	if toolCallID != "" {
		parts = append(parts, fmt.Sprintf(" [%s]", toolCallID))
	}
	if strings.TrimSpace(text) != "" {
		parts = append(parts, fmt.Sprintf(":\n%s", text))
	}
	return strings.Join(parts, "")
}

func joinSections(first, second string) string {
	if strings.TrimSpace(first) == "" {
		return second
	}
	if strings.TrimSpace(second) == "" {
		return first
	}
	return first + "\n\n" + second
}

// latestUserPrompt extracts the latest non-empty user text (chat_context +
// business.name inputs).
func latestUserPrompt(msgs []adapter.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			if text := strings.TrimSpace(messageText(msgs[i].Content)); text != "" {
				return text
			}
		}
	}
	return ""
}

// truncateRunes caps s to at most n runes without splitting multi-byte UTF-8.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// --- Model resolution ---

// defaultModelMap is the fallback display-name → qoder-key mapping (static
// catalog snapshot, 2026-09-22 chat scene). A dynamic model/list fetch is not
// wired yet; unknown names pass through verbatim — the gateway judges.
var defaultModelMap = map[string]string{
	"Qwen3.8-Max":       "qmodel_38max",
	"Qwen3.8-Flash":     "qfmodel",
	"Qwen3.7-Max":       "qmodel_latest",
	"Qwen3.7-Plus":      "qmodel",
	"Qwen3.7-Flash":     "q37fmodel",
	"DeepSeek-V4-Pro":   "dmodel",
	"DeepSeek-Flash":    "dfmodel",
	"GLM-5.3":           "gmodel",
	"GLM-5.3-Flash":     "gfmodel",
	"GLM-5.2":           "gm51model",
	"Kimi-K3":           "kmodel_latest",
	"Kimi-K2.8-Preview": "kmodel",
	"MiniMax-M2.7":      "mmodel",
}

const defaultModelKey = "qmodel_latest"

// modelDisplayNames returns the static catalog's display names sorted.
func modelDisplayNames() []string {
	names := make([]string, 0, len(defaultModelMap))
	for name := range defaultModelMap {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// resolveModelKey maps the requested model onto the qoder internal key:
// exact display-name match first, known keys as-is, anything else passes
// through verbatim (the gateway is the judge and its rejection comes back
// classified). Empty resolves to the catalog default.
func resolveModelKey(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return defaultModelKey
	}
	if key, ok := defaultModelMap[m]; ok {
		return key
	}
	return m
}

// --- Parameters ---

// orderedObject assembles a JSON object with insertion-ordered keys (Go maps
// marshal alphabetically, which would scramble the order the official client
// emits).
type orderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func newOrderedObject() *orderedObject {
	return &orderedObject{vals: make(map[string]json.RawMessage)}
}

func (o *orderedObject) set(key string, raw json.RawMessage) {
	if _, exists := o.vals[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = raw
}

func (o *orderedObject) setInt(key string, n int) {
	o.set(key, json.RawMessage(strconv.Itoa(n)))
}

func (o *orderedObject) setFloat(key string, f float64) {
	b, err := marshalNoEscape(f)
	if err != nil {
		return
	}
	o.set(key, b)
}

func (o *orderedObject) marshal() (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoEscape(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// buildParameters assembles the gateway "parameters" object: the
// gateway-defined max_tokens and tool_choice first, then the verbatim
// passthrough keys in the official client's order. Only an absent max_tokens
// gets the catalog default (the official client's own fallback).
func buildParameters(req adapter.ChatRequest) (json.RawMessage, error) {
	p := newOrderedObject()
	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	p.setInt("max_tokens", maxTokens)
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		p.set("tool_choice", req.ToolChoice)
	}
	// Passthrough (alphabetical, matching the reference's sorted list):
	// stop, temperature, top_p.
	if len(req.Stop) > 0 && string(req.Stop) != "null" {
		p.set("stop", req.Stop)
	}
	if req.Temperature != nil {
		p.setFloat("temperature", *req.Temperature)
	}
	if req.TopP != nil {
		p.setFloat("top_p", *req.TopP)
	}
	return p.marshal()
}

// buildChatBody converts the gateway request into the encoded upstream body.
// stream is always true upstream; non-stream client requests aggregate.
func buildChatBody(req adapter.ChatRequest, userType string) ([]byte, error) {
	nid := newUUIDString()
	prompt := latestUserPrompt(req.Messages)
	modelKey := resolveModelKey(req.Model)
	params, err := buildParameters(req)
	if err != nil {
		return nil, err
	}
	body := &chatRequestBody{
		RequestID:    nid,
		RequestSetID: newUUIDString(),
		ChatRecordID: nid,
		Stream:       true,
		ChatTask:     "FREE_INPUT",
		ChatContext: chatContext{
			Extra: chatContextExtra{
				ModelConfig:     extraModelConfig{IsReasoning: true, Key: modelKey},
				OriginalContent: originalContent{Type: "text", Text: prompt},
			},
			Text: chatContextText{Type: "text", Text: prompt},
		},
		SessionID:      newUUIDString(),
		Source:         1,
		Version:        "3",
		AliyunUserType: userType,
		SessionType:    "qodercli",
		AgentID:        "agent_common",
		TaskID:         "common",
		ModelConfig:    modelConfig{Key: modelKey, IsReasoning: true, Source: "system"},
		Messages:       buildMessages(req.Messages, len(req.Tools) > 0),
		Business:       business{ID: newUUIDString(), Name: truncateRunes(prompt, 30), BeginAt: time.Now().UnixMilli()},
		Parameters:     params,
	}
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		body.Tools = req.Tools
	}
	return marshalNoEscape(body)
}

// --- Upstream open + retry ---

// slotReadCloser holds one admission slot until the stream is closed.
type slotReadCloser struct {
	io.ReadCloser
	release func()
}

func (s *slotReadCloser) Close() error {
	err := s.ReadCloser.Close()
	s.release()
	return err
}

// openUpstream opens the chat SSE stream with one refresh/backoff retry
// before any content and returns the raw body reader. Each attempt acquires
// the account's admission slot fresh: a failed attempt releases it, a
// succeeded one hands it to the returned reader (released on Close), so the
// slot is held for the whole upstream exchange — queuing parallel sends
// locally instead of tripping the gateway's 10605 admission window.
func (s *senderImpl) openUpstream(ctx context.Context, pat string, st *acctState, body []byte, modelKey string) (io.ReadCloser, error) {
	for attempt := 1; ; attempt++ {
		select {
		case st.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		sess, err := s.ensureSession(ctx, pat, st)
		if err != nil {
			<-st.slots
			return nil, err
		}
		rc, err := s.postChat(ctx, sess, body, modelKey)
		if err == nil {
			return &slotReadCloser{ReadCloser: rc, release: func() { <-st.slots }}, nil
		}
		<-st.slots

		var busy *busySignal
		if errors.As(err, &busy) && attempt < 2 {
			wait := busy.RetryAfter
			if wait <= 0 || wait > 10*time.Second {
				wait = busyFallbackBackoff
			}
			fmt.Printf("[qoder] gateway busy; backing off %s then retrying once\n", wait)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		var ue *adapter.UpstreamError
		if errors.As(err, &ue) && ue.Reason == adapter.ReasonAuthExpired && attempt < 2 {
			// Nothing was produced (status-check failure): re-exchange once
			// and replay.
			fmt.Printf("[qoder] auth error before any content (%v); re-exchanging and retrying once\n", err)
			if renewErr := s.forceRenew(ctx, pat, st); renewErr != nil {
				return nil, err
			}
			continue
		}
		// A busy rejection that survived its retry converts to the canonical
		// rate-limited failure here; everything else is already classified.
		if errors.As(err, &busy) {
			return nil, busyToUpstream(busy)
		}
		return nil, err
	}
}

// busyToUpstream converts the retry-loop signal into the canonical
// rate-limited failure.
func busyToUpstream(busy *busySignal) *adapter.UpstreamError {
	full := "qoder: gateway busy (10605): " + busy.Message
	if busy.RetryAfter > 0 {
		full += fmt.Sprintf(" (retry after %s)", busy.RetryAfter)
	}
	return &adapter.UpstreamError{Status: pickStatus(busy.StatusCode, http.StatusTooManyRequests), Reason: adapter.ReasonRateLimited, Message: full}
}

// postChat sends the encoded chat body and returns the raw SSE reader on
// HTTP 200. HTTP >= 400 is classified; a 10605 envelope comes back as a
// busySignal for the retry loop.
func (s *senderImpl) postChat(ctx context.Context, sess *session, body []byte, modelKey string) (io.ReadCloser, error) {
	url := s.authBase() + pathChat
	encoded, err := customEncode(body)
	if err != nil {
		return nil, err
	}
	date := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header = chatHeaders(sess, date, sess.buildBearer(date, encoded, sigPath(url)), "text/event-stream")
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("x-model-key", modelKey)
	req.Header.Set("x-model-source", "system")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &adapter.UpstreamError{Status: http.StatusGatewayTimeout, Reason: adapter.ReasonTimeout, Message: "qoder: " + ctx.Err().Error()}
		}
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "qoder: transport: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyCap))
		drainBody(resp.Body)
		if busy := busyFromBody(resp.StatusCode, raw); busy != nil {
			return nil, busy
		}
		return nil, classifyUpstream(resp.StatusCode, raw, resp.Header)
	}
	return resp.Body, nil
}

// --- SSE frame parsing ---

// sseEnvelope is the gateway's wire frame: the OpenAI-shaped inner payload
// rides as a JSON string in "body".
type sseEnvelope struct {
	Body        string `json:"body"`
	StatusCode  string `json:"statusCode"`
	StatusValue int    `json:"statusCodeValue"`
}

// innerChunk is the OpenAI-shaped payload inside the envelope body.
type innerChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// isDonePayload reports whether an SSE data payload terminates the stream.
// The gateway wraps [DONE] inside its envelope ({"body":"[DONE]",...}); a
// bare "data: [DONE]" is accepted too so test fakes terminate cleanly.
func isDonePayload(payload string) bool {
	if payload == "[DONE]" {
		return true
	}
	var env sseEnvelope
	if json.Unmarshal([]byte(payload), &env) == nil && env.Body == "[DONE]" {
		return true
	}
	return false
}

// busyFromBody detects the 10605 admission-control rejection inside an HTTP
// error body (bodies seen in the wild are prefixed with an upstream status,
// so decoding starts at the first '{').
func busyFromBody(statusCode int, body []byte) *busySignal {
	s := string(body)
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	var outer struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if json.Unmarshal([]byte(s), &outer) != nil || rawCode(outer.Code) != busyCode {
		return nil //nolint:nilerr // not a busy envelope: fall through to the regular classification
	}
	busy := &busySignal{StatusCode: statusCode}
	if outer.Message != "" {
		busy.Message = truncateRunes(outer.Message, 512)
		busy.RetryAfter = busyRetryAfter(outer.Message)
	}
	return busy
}

// rawCode normalizes a JSON "code" field arriving as string or number.
func rawCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n == float64(int64(n)) {
		return strconv.FormatInt(int64(n), 10)
	}
	return ""
}

// busyRetryAfter extracts retryAfterSeconds from the nested message JSON.
func busyRetryAfter(msg string) time.Duration {
	var meta struct {
		RetryAfterSeconds int `json:"retryAfterSeconds"`
	}
	if strings.HasPrefix(strings.TrimSpace(msg), "{") && json.Unmarshal([]byte(msg), &meta) == nil && meta.RetryAfterSeconds > 0 {
		return time.Duration(meta.RetryAfterSeconds) * time.Second
	}
	return 0
}

// errorMessage extracts a client-safe message from an upstream error body:
// the JSON "message" field when the body parses as an object, else a generic
// status phrase. The raw body is never echoed — it carries userId/trace ids.
func errorMessage(status int, body []byte) string {
	var env struct {
		Message string `json:"message"`
		MsgInfo string `json:"msgInfo"`
	}
	if json.Unmarshal(body, &env) == nil {
		if env.Message != "" {
			return truncateRunes(env.Message, 200)
		}
		if env.MsgInfo != "" {
			return truncateRunes(env.MsgInfo, 200)
		}
	}
	return "HTTP " + strconv.Itoa(status)
}

// authCodeSuffix renders the business envelope's code, when present, as a
// bracketed suffix for diagnosability.
func authCodeSuffix(body []byte) string {
	var probe struct {
		Code json.RawMessage `json:"code"`
	}
	if json.Unmarshal(body, &probe) != nil || len(probe.Code) == 0 {
		return ""
	}
	return " (code " + string(probe.Code) + ")"
}

// retryAfterHeader parses a numeric Retry-After header into a duration; 0
// means absent/unparseable.
func retryAfterHeader(h http.Header) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// classifyUpstream maps a non-200 upstream reply onto the canonical failure
// vocabulary. Order is semantic: the 10605 admission check runs before the
// auth bucket because the busy rejection shares the 401/403 envelope.
func classifyUpstream(status int, body []byte, header http.Header) *adapter.UpstreamError {
	if busy := busyFromBody(status, body); busy != nil {
		return busyToUpstream(busy)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonAuthExpired, Message: "qoder: " + errorMessage(status, body)}
	case status == http.StatusTooManyRequests:
		full := "qoder: " + errorMessage(status, body)
		if d := retryAfterHeader(header); d > 0 {
			full += fmt.Sprintf(" (retry after %s)", d)
		}
		return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonRateLimited, Message: full}
	case status == http.StatusPaymentRequired:
		return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonQuotaExhausted, Message: "qoder: " + errorMessage(status, body)}
	case status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge:
		return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonNotFound, Message: "qoder: " + errorMessage(status, body)}
	case status >= 500:
		return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonPlatformDown, Message: "qoder: " + errorMessage(status, body)}
	default:
		return &adapter.UpstreamError{Status: pickStatus(status, http.StatusBadGateway), Reason: adapter.ReasonPlatformDown, Message: "qoder: " + errorMessage(status, body)}
	}
}

// pickStatus keeps the upstream status when it is a usable 4xx/5xx, else the
// canonical fallback for the reason.
func pickStatus(status, fallback int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return fallback
}

// classifyInStream maps a terminal in-stream failure (statusCodeValue >= 400
// under HTTP 200) through the same classification as a real HTTP status.
func classifyInStream(statusValue int, bodyJSON string) error {
	if statusValue == http.StatusUnauthorized || statusValue == http.StatusForbidden {
		if busy := busyFromBody(statusValue, []byte(bodyJSON)); busy != nil {
			return busyToUpstream(busy)
		}
		// The body code discriminates rejection families sharing one human
		// text ("Login expired" covers 105 and plain 401s) — carry it verbatim
		// so panel-side diagnosis reads facts, not guesses.
		return &adapter.UpstreamError{Status: statusValue, Reason: adapter.ReasonAuthExpired,
			Message: "qoder: in-stream auth failure: " + errorMessage(statusValue, []byte(bodyJSON)) + authCodeSuffix([]byte(bodyJSON))}
	}
	return classifyUpstream(statusValue, []byte(bodyJSON), nil)
}

// --- Stream relay / aggregation ---

// relayQoderStream parses the enveloped SSE frames and forwards OpenAI-shaped
// chat.completion.chunk frames. Healthy termination (enveloped [DONE], bare
// data:[DONE] or event:finish) closes chunks; any failure delivers at most
// one error on errCh without closing it (the Sender contract's idle state).
// The provider-extension reasoning_content cannot ride the shared ChatMessage
// type and is dropped (same disposition as the workbuddy sender).
func relayQoderStream(ctx context.Context, rc io.Reader, model string, chunks chan<- adapter.StreamChunk, errCh chan<- error) {
	br := bufio.NewReaderSize(rc, 64*1024)
	reqID := "chatcmpl-" + randHex(24)
	created := time.Now().Unix()
	sawDone := false
	var streamErr error

	emit := func(delta adapter.ChatMessage, finish *string) bool {
		chunk := adapter.StreamChunk{
			ID: reqID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []adapter.StreamChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
		select {
		case chunks <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		if ctx.Err() != nil {
			return
		}
		line, rerr := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed != "" {
			payload, isData := strings.CutPrefix(trimmed, "data:")
			switch {
			case isData:
				p := strings.TrimSpace(payload)
				if isDonePayload(p) {
					sawDone = true
				} else if err := handleFrame(ctx, p, model, emit); err != nil {
					streamErr = err
					sawDone = false // a terminal error frame ends the stream
					goto drain
				}
			case strings.HasPrefix(trimmed, "event:"):
				switch strings.TrimSpace(trimmed[len("event:"):]) {
				case "finish":
					// MiniMax-style termination: no [DONE] frame follows.
					sawDone = true
				case "error":
					if streamErr == nil {
						streamErr = &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown,
							Message: "qoder: upstream stream failed (event:error)"}
					}
					goto drain
				}
			}
		}
	drain:
		if rerr != nil {
			if rerr != io.EOF {
				tryDeliverErr(ctx, errCh, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown,
					Message: "qoder: read stream: " + rerr.Error()})
				return
			}
			break
		}
		if streamErr != nil {
			break
		}
	}

	if !sawDone {
		if streamErr == nil {
			streamErr = &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown,
				Message: "qoder: stream ended without [DONE] marker (connection truncated?)"}
		}
		tryDeliverErr(ctx, errCh, streamErr)
		return
	}
	if streamErr != nil {
		tryDeliverErr(ctx, errCh, streamErr)
	}
	// Healthy completion: close(chunks) runs via the caller's defer; errCh
	// stays open (idle state per the Sender contract).
}

// handleFrame processes one non-terminal data payload: in-stream gateway
// errors become classified failures; normal frames emit OpenAI-shaped chunks.
func handleFrame(ctx context.Context, payload, model string, emit func(adapter.ChatMessage, *string) bool) error {
	var env sseEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return nil //nolint:nilerr // unparseable frame: upstream noise, skipped on purpose
	}
	if env.StatusValue >= 400 {
		return classifyInStream(env.StatusValue, env.Body)
	}
	if env.Body != "" {
		// The reference also treats a body code of "105" as login expiry even
		// under a 2xx envelope status.
		var probe struct {
			Code json.RawMessage `json:"code"`
		}
		if json.Unmarshal([]byte(env.Body), &probe) == nil && rawCode(probe.Code) == "105" {
			return &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired,
				Message: "qoder: in-stream auth failure: login expired (code 105)"}
		}
	}
	if env.Body == "" && env.StatusCode == "" {
		// Not an envelope frame: check the bare success:false gateway error
		// shape before treating it as noise.
		return handleBareFrame(payload)
	}
	var inner innerChunk
	if json.Unmarshal([]byte(env.Body), &inner) != nil {
		return nil //nolint:nilerr // unparseable inner payload: upstream noise, skipped on purpose
	}
	for i := range inner.Choices {
		c := &inner.Choices[i]
		delta := adapter.ChatMessage{}
		if c.Delta.Role != "" {
			delta.Role = c.Delta.Role
		}
		if len(c.Delta.Content) > 0 {
			var txt string
			if json.Unmarshal(c.Delta.Content, &txt) == nil && txt != "" {
				delta.Content = mustMarshalJSON(txt)
			}
		}
		if len(c.Delta.ToolCalls) > 0 && string(c.Delta.ToolCalls) != "null" {
			delta.ToolCalls = c.Delta.ToolCalls
		}
		hasDelta := delta.Role != "" || len(delta.Content) > 0 || len(delta.ToolCalls) > 0
		if hasDelta {
			if !emit(delta, nil) {
				return ctx.Err()
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			finish := *c.FinishReason
			if !emit(adapter.ChatMessage{}, &finish) {
				return ctx.Err()
			}
		}
	}
	return nil
}

// handleBareFrame recognizes the bare {"success":false,"msgCode":…} gateway
// error shape; anything else is noise.
func handleBareFrame(payload string) error {
	var bare struct {
		Success bool   `json:"success"`
		MsgCode int    `json:"msgCode"`
		MsgInfo string `json:"msgInfo"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(payload), &bare) != nil || bare.Success {
		return nil //nolint:nilerr // healthy or unparseable bare frame: nothing to report
	}
	if bare.MsgCode >= 400 || bare.Message != "" || bare.MsgInfo != "" {
		detail := bare.Message
		if detail == "" {
			detail = bare.MsgInfo
		}
		if detail == "" {
			detail = fmt.Sprintf("msgCode %d", bare.MsgCode)
		}
		return &adapter.UpstreamError{Status: pickStatus(bare.MsgCode, http.StatusBadGateway), Reason: adapter.ReasonPlatformDown,
			Message: "qoder: in-stream gateway error: " + truncateRunes(detail, 200)}
	}
	return nil
}

// tryDeliverErr delivers the single in-band stream error without blocking
// forever when the router already walked away.
func tryDeliverErr(ctx context.Context, errCh chan<- error, err error) {
	select {
	case errCh <- err:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
}

// --- Aggregation (SendChat) ---

// aggregated folds the relayed chunks into one non-streaming ChatResponse.
type aggregated struct {
	id, model string
	created   int64
	role      string
	content   strings.Builder
	finish    string
	toolCalls map[int]*aggTool
	toolOrder []int
	sawAny    bool
}

type aggTool struct {
	id   string
	name string
	args strings.Builder
}

func (a *aggregated) accept(chunk adapter.StreamChunk) {
	a.sawAny = true
	if chunk.ID != "" && a.id == "" {
		a.id = chunk.ID
	}
	if chunk.Model != "" && a.model == "" {
		a.model = chunk.Model
	}
	if chunk.Created != 0 && a.created == 0 {
		a.created = chunk.Created
	}
	for i := range chunk.Choices {
		c := &chunk.Choices[i]
		if c.FinishReason != nil && *c.FinishReason != "" {
			a.finish = *c.FinishReason
		}
		if c.Delta.Role != "" {
			a.role = c.Delta.Role
		}
		if len(c.Delta.Content) > 0 {
			var txt string
			if json.Unmarshal(c.Delta.Content, &txt) == nil {
				a.content.WriteString(txt)
			}
		}
		if len(c.Delta.ToolCalls) > 0 {
			a.mergeToolCalls(c.Delta.ToolCalls)
		}
	}
}

// mergeToolCalls accumulates streamed tool_call fragments: the first fragment
// carries id/name, later ones append arguments text; fragments lacking index
// continue the last seen call.
func (a *aggregated) mergeToolCalls(raw json.RawMessage) {
	var fragments []map[string]any
	if json.Unmarshal(raw, &fragments) != nil {
		return
	}
	for _, fr := range fragments {
		idx := -1
		if v, ok := fr["index"].(float64); ok {
			idx = int(v)
		}
		if idx < 0 {
			if len(a.toolOrder) > 0 {
				idx = a.toolOrder[len(a.toolOrder)-1]
			} else {
				idx = 0
			}
		}
		call, seen := a.toolCalls[idx]
		if !seen {
			call = &aggTool{}
			a.toolCalls[idx] = call
			a.toolOrder = append(a.toolOrder, idx)
		}
		if s, ok := fr["id"].(string); ok && s != "" {
			call.id = s
		}
		if fn, ok := fr["function"].(map[string]any); ok {
			if s, ok := fn["name"].(string); ok && s != "" {
				call.name = s
			}
			if s, ok := fn["arguments"].(string); ok {
				call.args.WriteString(s)
			}
		}
	}
}

// response assembles the final chat.completion object. Usage is nil: the
// shared StreamChunk type carries no usage field (same disposition as the
// workbuddy sender; the upstream usage frame is consumed by classification).
func (a *aggregated) response() *adapter.ChatResponse {
	if a.role == "" {
		a.role = "assistant"
	}
	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	msg := adapter.ChatMessage{Role: a.role}
	msg.Content = mustMarshalJSON(a.content.String())
	if len(a.toolOrder) > 0 {
		calls := make([]map[string]any, 0, len(a.toolOrder))
		for _, idx := range a.toolOrder {
			call := a.toolCalls[idx]
			calls = append(calls, map[string]any{
				"index": idx,
				"id":    call.id,
				"type":  "function",
				"function": map[string]string{
					"name":      call.name,
					"arguments": call.args.String(),
				},
			})
		}
		msg.ToolCalls = mustMarshalJSON(calls)
		if finish == "stop" {
			finish = "tool_calls"
		}
	}
	return &adapter.ChatResponse{
		ID:      a.id,
		Object:  "chat.completion",
		Created: a.created,
		Model:   a.model,
		Choices: []adapter.ChatChoice{{Index: 0, Message: msg, FinishReason: &finish}},
	}
}

func mustMarshalJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// --- Sender entry points ---

// SendChat performs one non-streaming chat completion. The upstream only
// speaks SSE (stream:true is mandatory in the chat envelope), so the frames
// are aggregated into a single ChatResponse.
func (s *senderImpl) SendChat(ctx context.Context, acct *pool.Account, req adapter.ChatRequest) (*adapter.ChatResponse, error) {
	chunks, errCh, err := s.SendChatStream(ctx, acct, req)
	if err != nil {
		return nil, err
	}
	acc := &aggregated{toolCalls: map[int]*aggTool{}}
	for chunk := range chunks {
		acc.accept(chunk)
	}
	// A mid-stream failure is delivered on errCh before chunks closes, so a
	// non-blocking receive after the range sees every failure.
	select {
	case err := <-errCh:
		return nil, err
	default:
	}
	if !acc.sawAny {
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown,
			Message: "qoder: upstream stream contained no valid data events"}
	}
	return acc.response(), nil
}

// SendChatStream starts one streaming chat completion per the Sender channel
// contract: chunks close on healthy completion; errCh carries at most one
// error and is NOT closed on a healthy stream; both stop when ctx is done.
func (s *senderImpl) SendChatStream(ctx context.Context, acct *pool.Account, req adapter.ChatRequest) (<-chan adapter.StreamChunk, <-chan error, error) {
	pat, err := s.patFor(ctx, acct)
	if err != nil {
		return nil, nil, err
	}
	st := s.state(acct.ID)
	// Bootstrap first: the subscription tier is sealed into the chat body
	// (aliyun_user_type), so the session must exist before the body is built.
	sess, err := s.ensureSession(ctx, pat, st)
	if err != nil {
		return nil, nil, err
	}
	modelKey := resolveModelKey(req.Model)
	body, err := buildChatBody(req, sess.userType)
	if err != nil {
		return nil, nil, &adapter.UpstreamError{Status: http.StatusBadRequest, Reason: adapter.ReasonNotFound, Message: "qoder: build request body: " + err.Error()}
	}
	rc, err := s.openUpstream(ctx, pat, st, body, modelKey)
	if err != nil {
		return nil, nil, err
	}
	chunks := make(chan adapter.StreamChunk)
	// Buffered so a mid-stream failure can be delivered and the producer can
	// exit even when the router has stopped reading.
	errCh := make(chan error, 1)
	go func() {
		defer close(chunks)
		defer rc.Close()
		relayQoderStream(ctx, rc, req.Model, chunks, errCh)
	}()
	return chunks, errCh, nil
}
