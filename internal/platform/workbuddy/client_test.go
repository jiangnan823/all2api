//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package workbuddy

// Tests run exclusively against httptest local stubs — no real endpoint is
// ever contacted (task hard constraint). Stub behavior is derived from the
// recorded wire facts in docs/upstream/workbuddy-protocol.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/pool"
)

// newStubSender wires a sender onto an httptest server with a static
// credential and returns (sender, server, baseURL).
func newStubSender(t *testing.T, handler http.Handler) (*senderImpl, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s := NewSender()
	s.Configure(ClientOptions{
		BaseURL: srv.URL,
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second, // guards against hangs on stub bugs
		},
		Credentials: CredentialSourceFunc(func(_ context.Context, acct *pool.Account) (*Credential, error) {
			return &Credential{
				AccessToken: "test-access-token",
				UID:         "uid-1",
				Global:      false,
				ExpiresAt:   time.Now().Add(time.Hour),
			}, nil
		}),
	})
	return s, srv
}

func testAccount() *pool.Account {
	return &pool.Account{ID: "acct-1", Platform: "workbuddy", Status: "active"}
}

func testRequest() adapter.ChatRequest {
	return adapter.ChatRequest{
		Model: "glm-5.3-flash",
		Messages: []adapter.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
	}
}

// sseStub builds a handler answering any /v2/chat/completions POST with the
// given SSE frames and status.
func sseStub(t *testing.T, status int, frames ...string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("stub: unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprint(w, f)
		}
	})
}

// ---------- wire-shape tests: headers + body rewrites ----------

func TestSendChatHeadersAndBody(t *testing.T) {
	var gotHeaders http.Header
	var gotBody []byte
	var gotQueryPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQueryPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	s := NewSender()
	s.Configure(ClientOptions{BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Credentials: CredentialSourceFunc(func(_ context.Context, _ *pool.Account) (*Credential, error) {
			return &Credential{AccessToken: "tok", UID: "u1", EnterpriseID: "ent9", Domain: "www.codebuddy.cn"}, nil
		})})

	req := testRequest()
	req.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`)
	req.Messages = append(req.Messages,
		adapter.ChatMessage{Role: "developer", Content: json.RawMessage(`"dev rules"`)},
		adapter.ChatMessage{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":"data:image/png;base64,AAA"}]`)},
	)
	if _, err := s.SendChat(context.Background(), testAccount(), req); err != nil {
		t.Fatalf("SendChat: %v", err)
	}

	// Endpoint: base + /v2/chat/completions.
	if gotQueryPath != "/v2/chat/completions" {
		t.Errorf("path = %q, want /v2/chat/completions", gotQueryPath)
	}
	// Auth + account headers (report §3).
	if got := gotHeaders.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	if got := gotHeaders.Get("X-User-Id"); got != "u1" {
		t.Errorf("X-User-Id = %q", got)
	}
	if got := gotHeaders.Get("X-Enterprise-Id"); got != "ent9" {
		t.Errorf("X-Enterprise-Id = %q", got)
	}
	if got := gotHeaders.Get("X-Tenant-Id"); got != "ent9" {
		t.Errorf("X-Tenant-Id = %q", got)
	}
	if got := gotHeaders.Get("X-Domain"); got != "www.codebuddy.cn" {
		t.Errorf("X-Domain = %q", got)
	}
	// Gate + brand headers (report §8).
	if got := gotHeaders.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("X-CodeBuddy-Request = %q", got)
	}
	if got := gotHeaders.Get("Origin"); got != wireOriginCN {
		t.Errorf("Origin = %q", got)
	}
	ua := gotHeaders.Get("User-Agent")
	if !strings.HasPrefix(ua, "WorkBuddy/"+wireClientVersion+" WorkBuddy/") || !strings.Contains(ua, " CLI/") {
		t.Errorf("User-Agent = %q, want three-segment WorkBuddy UA", ua)
	}
	if got := gotHeaders.Get("Accept-Language"); got != "zh-CN" {
		t.Errorf("Accept-Language = %q", got)
	}

	// Body rewrites (report §4).
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, gotBody)
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want forced true", body["stream"])
	}
	if body["tool_choice"] != "get_weather" {
		t.Errorf("tool_choice = %v, want function name string", body["tool_choice"])
	}
	if _, ok := body["stream_options"]; !ok {
		t.Errorf("stream_options missing, want include_usage")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	first := msgs[1].(map[string]any) // developer message
	if first["role"] != "system" {
		t.Errorf("developer role not renamed: %v", first["role"])
	}
	third := msgs[2].(map[string]any)
	parts := third["content"].([]any)
	img := parts[0].(map[string]any)["image_url"].(map[string]any)
	if img["url"] == nil || img["detail"] != nil {
		t.Errorf("image_url not objectified: %v", img)
	}
}

func TestGlobalRealmHeaders(t *testing.T) {
	var gotHeaders http.Header
	s, _ := newStubSender(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"g1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	// Reconfigure the credential source for a global account.
	s.Configure(ClientOptions{
		BaseURL:    s.options().BaseURL,
		HTTPClient: s.httpClient(),
		Credentials: CredentialSourceFunc(func(_ context.Context, _ *pool.Account) (*Credential, error) {
			return &Credential{AccessToken: "tok", Global: true}, nil
		}),
	})
	if _, err := s.SendChat(context.Background(), testAccount(), testRequest()); err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	if got := gotHeaders.Get("Origin"); got != wireOriginGlobal {
		t.Errorf("Origin = %q, want global brand", got)
	}
	ua := gotHeaders.Get("User-Agent")
	if !strings.Contains(ua, " WorkBuddy AI/"+wireClientVersion+" ") {
		t.Errorf("User-Agent = %q, want WorkBuddy AI platform segment", ua)
	}
	if got := gotHeaders.Get("Accept-Language"); got != "en-US" {
		t.Errorf("Accept-Language = %q", got)
	}
	if got := gotHeaders.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("X-Enterprise-Id = %q, want absent for personal account", got)
	}
	if got := gotHeaders.Get("X-No-Enterprise-Id"); got != "1" {
		t.Errorf("X-No-Enterprise-Id = %q, want placeholder", got)
	}
}

// ---------- aggregation tests ----------

func TestSendChatAggregatesStream(t *testing.T) {
	frames := []string{
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"he\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"},\"finish_reason\":null}]}\n\n",
		// reasoning_content extension frame
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think\"},\"finish_reason\":null}]}\n\n",
		// tool call fragments
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"ci\"}}]},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ty\\\":\\\"SF\\\"}\"}}]},\"finish_reason\":null}]}\n\n",
		// terminal usage frame (stream_options include_usage)
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n",
		"data: [DONE]\n\n",
	}
	s, _ := newStubSender(t, sseStub(t, http.StatusOK, frames...))
	resp, err := s.SendChat(context.Background(), testAccount(), testRequest())
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	if resp.ID != "chatcmpl-1" || resp.Model != "glm-5.3-flash" || resp.Created != 1700000000 {
		t.Errorf("header fields = %s/%s/%d", resp.ID, resp.Model, resp.Created)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q", resp.Object)
	}
	msg := resp.Choices[0].Message
	if msg.Role != "assistant" {
		t.Errorf("role = %q", msg.Role)
	}
	var content string
	if err := json.Unmarshal(msg.Content, &content); err != nil || content != "hello" {
		t.Errorf("content = %q (err %v), want hello", msg.Content, err)
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", resp.Choices[0].FinishReason)
	}
	if msg.ToolCalls == nil {
		t.Fatal("tool_calls missing")
	}
	var calls []map[string]any
	if err := json.Unmarshal(msg.ToolCalls, &calls); err != nil {
		t.Fatalf("tool_calls not JSON: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d entries, want 1 (fragments merged by index)", len(calls))
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool name = %v", fn["name"])
	}
	if fn["arguments"] != `{"city":"SF"}` {
		t.Errorf("arguments = %v, want merged fragments", fn["arguments"])
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 18 {
		t.Errorf("usage = %+v, want total 18 from terminal frame", resp.Usage)
	}
}

func TestSendChatEmptyStreamFails(t *testing.T) {
	s, _ := newStubSender(t, sseStub(t, http.StatusOK, "data: [DONE]\n\n"))
	_, err := s.SendChat(context.Background(), testAccount(), testRequest())
	if err == nil {
		t.Fatal("empty stream should fail, got nil")
	}
	var ue *adapter.UpstreamError
	if !asUpstream(err, &ue) || ue.Status != http.StatusBadGateway {
		t.Fatalf("err = %v, want 502 UpstreamError", err)
	}
}

// ---------- error classification tests (report §6) ----------

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		header map[string]string
		want   adapter.FailureReason
	}{
		{"401 session dead", 401, `{"code":12153,"msg":"Offline user session not found"}`, nil, adapter.ReasonAuthExpired},
		{"12153 in 200", 200, `{"code":12153,"msg":"Offline user session not found"}`, nil, adapter.ReasonAuthExpired},
		{"quota 402", 402, `{"code":14018,"msg":"UsageLimitUserExhausted"}`, nil, adapter.ReasonQuotaExhausted},
		{"quota wording in 400", 400, `{"code":1,"msg":"积分不足"}`, nil, adapter.ReasonQuotaExhausted},
		{"6004 daily model quota wrapped in 200", 200, `{"code":6004,"msg":"model usage limit exceeded, 将在 2026-10-15 00:00:00 重置"}`, nil, adapter.ReasonRateLimited},
		{"6004 with retry-after header", 429, `{"code":6004,"msg":"daily token quota"}`, map[string]string{"Retry-After": "30"}, adapter.ReasonRateLimited},
		{"6001 account-level in 400", 400, `{"code":6001,"msg":"CraftRateTPSLimit"}`, nil, adapter.ReasonRateLimited},
		{"plain 429", 429, `{"code":1,"msg":"too many requests"}`, nil, adapter.ReasonRateLimited},
		{"11140 account fault as 429", 429, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, nil, adapter.ReasonRateLimited},
		{"WAF 403 no envelope", 403, "<html>blocked</html>", nil, adapter.ReasonPlatformDown},
		// 11140 "request illegal" is the account-level authorization fault
		// (report §6 account_fault): the gateway vocabulary has no dedicated
		// reason, so it maps to auth_expired — the pool rotates/cools the
		// account, matching the sources' "re-login required" disposition.
		{"business 403 not WAF", 403, `{"code":11140,"msg":"request illegal"}`, nil, adapter.ReasonAuthExpired},
		{"11102 model block", 404, `{"code":11102,"msg":"service info not found"}`, nil, adapter.ReasonNotFound},
		{"11115 prompt too long", 400, `{"code":11115,"msg":"prompt is too long"}`, nil, adapter.ReasonNotFound},
		{"11101 bad params", 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, nil, adapter.ReasonNotFound},
		{"11-128 content block", 400, `{"code":"11-128","msg":"Illegal API invocation from an unapproved channel"}`, nil, adapter.ReasonPlatformDown},
		{"500 server", 500, `{"code":500,"msg":"boom"}`, nil, adapter.ReasonPlatformDown},
		{"404 generic", 404, "not found", nil, adapter.ReasonNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newStubSender(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			_, err := s.SendChat(context.Background(), testAccount(), testRequest())
			var ue *adapter.UpstreamError
			if !asUpstream(err, &ue) {
				t.Fatalf("err = %v, want UpstreamError", err)
			}
			if ue.Reason != tc.want {
				t.Errorf("reason = %q, want %q (msg %q)", ue.Reason, tc.want, ue.Message)
			}
		})
	}
}

func TestRateLimitedMessageCarriesRetryHint(t *testing.T) {
	s, _ := newStubSender(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"code":6001,"msg":"rate limited"}`)
	}))
	_, err := s.SendChat(context.Background(), testAccount(), testRequest())
	var ue *adapter.UpstreamError
	if !asUpstream(err, &ue) || ue.Reason != adapter.ReasonRateLimited {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ue.Message, "retry after 45s") {
		t.Errorf("message = %q, want embedded retry hint", ue.Message)
	}
}

func TestParseResetAfter(t *testing.T) {
	h := http.Header{}
	if _, ok := parseResetAfter(h); ok {
		t.Error("empty headers should yield no hint")
	}
	h.Set("Retry-After", "90")
	if d, ok := parseResetAfter(h); !ok || d != 90*time.Second {
		t.Errorf("Retry-After = %v ok=%v", d, ok)
	}
	h.Del("Retry-After")
	h.Set("Retry-After-Ms", "1500")
	if d, ok := parseResetAfter(h); !ok || d != 1500*time.Millisecond {
		t.Errorf("Retry-After-Ms = %v ok=%v", d, ok)
	}
	h.Del("Retry-After-Ms")
	h.Set("X-Ratelimit-Reset", fmt.Sprint(time.Now().Add(2*time.Minute).Unix()))
	if d, ok := parseResetAfter(h); !ok || d <= 0 || d > 2*time.Minute+10*time.Second {
		t.Errorf("x-ratelimit-reset epoch seconds = %v ok=%v", d, ok)
	}
	h.Set("X-Ratelimit-Reset", "12345") // too short to be an epoch
	if _, ok := parseResetAfter(h); ok {
		t.Error("short numeric x-ratelimit-reset should be ignored")
	}
	h.Set("Retry-After", "Fri, 15 Oct 2026 00:00:00 GMT") // HTTP-date: ignored
	if _, ok := parseResetAfter(h); ok {
		t.Error("HTTP-date Retry-After should be ignored")
	}
}

// ---------- streaming tests ----------

func TestSendChatStreamRelaysFrames(t *testing.T) {
	frames := []string{
		"data: {\"id\":\"c1\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"},\"finish_reason\":null}]}\n\n",
		"data: [DONE]\n\n",
		"data: {\"after_done\":true}\n\n", // ignored per the reference behavior
	}
	s, _ := newStubSender(t, sseStub(t, http.StatusOK, frames...))
	chunks, errCh, err := s.SendChatStream(context.Background(), testAccount(), testRequest())
	if err != nil {
		t.Fatalf("SendChatStream: %v", err)
	}
	var got []adapter.StreamChunk
	for ch := range chunks {
		got = append(got, ch)
	}
	if len(got) != 2 {
		t.Fatalf("chunks = %d, want 2", len(got))
	}
	var a, b string
	json.Unmarshal(got[0].Choices[0].Delta.Content, &a)
	json.Unmarshal(got[1].Choices[0].Delta.Content, &b)
	if a+b != "ab" {
		t.Errorf("relay content = %q+%q", a, b)
	}
	select {
	case e := <-errCh:
		t.Errorf("errCh should stay open on healthy stream, got %v", e)
	default:
	}
}

func TestSendChatStreamMidStreamError(t *testing.T) {
	s, _ := newStubSender(t, sseStub(t, http.StatusOK,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n",
		// connection cut without [DONE] — the stub hijacks and closes
	))
	// Wrap: sever mid-stream by closing after first frame is unreachable with
	// a static stub; instead use a handler that writes one frame then closes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// no [DONE]: connection closes → EOF without DONE
	}))
	defer srv.Close()
	s.Configure(ClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(),
		Credentials: s.options().Credentials})

	chunks, errCh, err := s.SendChatStream(context.Background(), testAccount(), testRequest())
	if err != nil {
		t.Fatalf("SendChatStream: %v", err)
	}
	n := 0
	for range chunks {
		n++
	}
	if n != 1 {
		t.Errorf("chunks = %d, want 1", n)
	}
	select {
	case e := <-errCh:
		var ue *adapter.UpstreamError
		if !asUpstream(e, &ue) || ue.Reason != adapter.ReasonPlatformDown {
			t.Errorf("mid-stream err = %v, want platform_down", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for mid-stream error")
	}
}

func TestSendChatStreamHonorsContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // hold the stream open until the test cancels
	}))
	defer srv.Close()
	defer close(release)
	s := NewSender()
	s.Configure(ClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(),
		Credentials: CredentialSourceFunc(func(_ context.Context, _ *pool.Account) (*Credential, error) {
			return &Credential{AccessToken: "tok"}, nil
		})})

	ctx, cancel := context.WithCancel(context.Background())
	chunks, _, err := s.SendChatStream(ctx, testAccount(), testRequest())
	if err != nil {
		t.Fatalf("SendChatStream: %v", err)
	}
	<-chunks // first frame arrives
	cancel()
	for range chunks {
	} // producer must exit promptly after cancel
	// Reaching here without the 5s client timeout firing is the assertion.
}

// ---------- credential / auth tests ----------

func TestSendFailsAuthWithoutCredential(t *testing.T) {
	s := NewSender()
	s.Configure(ClientOptions{BaseURL: "http://127.0.0.1:1"}) // credentials nil
	_, err := s.SendChat(context.Background(), testAccount(), testRequest())
	var ue *adapter.UpstreamError
	if !asUpstream(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
		t.Fatalf("err = %v, want auth_expired", err)
	}
}

func TestSendFailsAuthOnExpiredToken(t *testing.T) {
	s := NewSender()
	s.Configure(ClientOptions{
		BaseURL: "http://127.0.0.1:1",
		Now:     func() time.Time { return time.Unix(1000, 0) },
		Credentials: CredentialSourceFunc(func(_ context.Context, _ *pool.Account) (*Credential, error) {
			return &Credential{AccessToken: "tok", ExpiresAt: time.Unix(500, 0)}, nil
		}),
	})
	_, err := s.SendChat(context.Background(), testAccount(), testRequest())
	var ue *adapter.UpstreamError
	if !asUpstream(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
		t.Fatalf("err = %v, want auth_expired", err)
	}
}

func TestSendNeverCarriesRefreshToken(t *testing.T) {
	var gotHeaders http.Header
	s, _ := newStubSender(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		fmt.Fprint(w, "data: {\"id\":\"r1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	s.Configure(ClientOptions{
		BaseURL:    s.options().BaseURL,
		HTTPClient: s.httpClient(),
		Credentials: CredentialSourceFunc(func(_ context.Context, _ *pool.Account) (*Credential, error) {
			return &Credential{AccessToken: "at", RefreshToken: "rt-SECRET"}, nil
		}),
	})
	if _, err := s.SendChat(context.Background(), testAccount(), testRequest()); err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	for k, vs := range gotHeaders {
		for _, v := range vs {
			if strings.Contains(v, "rt-SECRET") {
				t.Errorf("refresh token leaked via header %s", k)
			}
		}
	}
	if got := gotHeaders.Get("X-Refresh-Token"); got != "" {
		t.Errorf("X-Refresh-Token = %q, must never ride the chat path", got)
	}
}

// ---------- tool_choice "none" drops the field ----------

func TestToolChoiceNoneDropped(t *testing.T) {
	body, err := buildUpstreamBody(adapter.ChatRequest{
		Model:      "m",
		Messages:   []adapter.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:      json.RawMessage(`[{"type":"function","function":{"name":"f"}}]`),
		ToolChoice: json.RawMessage(`"none"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	if _, has := obj["tool_choice"]; has {
		t.Error("tool_choice=none should drop the field")
	}
	if _, has := obj["tools"]; has {
		t.Error("tool_choice=none should drop tools (upstream dangling-array 11101)")
	}
}

// ---------- adapter registration ----------

func TestAdapterImplementsSender(t *testing.T) {
	var a adapter.Adapter = &Adapter{senderImpl: NewSender()}
	if _, ok := a.(adapter.Sender); !ok {
		t.Fatal("registered adapter does not satisfy adapter.Sender — router would answer no_upstream_sender")
	}
	if a.Name() != "" {
		// Name is set by init(); a manually-built adapter has none. Registered
		// instance is exercised via the registry below.
		t.Log("manual adapter has empty name (expected; registration sets it)")
	}
}

func TestRegisteredAdapterSendsSender(t *testing.T) {
	a := adapter.Default.Get("workbuddy")
	if a == nil {
		t.Skip("workbuddy adapter not registered in this build variant")
	}
	if _, ok := a.(adapter.Sender); !ok {
		t.Fatal("registered workbuddy adapter lacks adapter.Sender")
	}
}

// asUpstream is errors.As specialized for *adapter.UpstreamError.
func asUpstream(err error, target **adapter.UpstreamError) bool {
	return errors.As(err, target)
}
