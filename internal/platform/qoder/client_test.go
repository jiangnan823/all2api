//go:build qoder || full || (!qoder && !trae && !workbuddy)

package qoder

// Protocol-shape tests over an httptest fake upstream. Everything asserted
// here is distilled from the verified reference implementation (vhub/qauth +
// vhub/qbridge): the Encode layer, the COSY bearer signature, both header
// families, the enveloped SSE stream and the error classes. The fake never
// proxies: the test suite never contacts gateway.qoder.com.cn or
// openapi.qoder.sh (task red line). All PAT values are fake "pt-" strings.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/pool"
)

const fakePAT = "pt-" + "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// chatSigPath is the path part the bearer signature covers (host and query
// stripped, leading /algo removed).
const chatSigPath = "/api/v2/service/pro/sse/agent_chat_generation"

// envFrame wraps an inner OpenAI-shaped payload into the gateway's envelope.
func envFrame(inner string) string {
	b, _ := json.Marshal(map[string]any{"body": inner, "statusCode": "OK", "statusCodeValue": 200})
	return "data: " + string(b) + "\n\n"
}

// happySSE is a complete healthy stream: role, two content deltas (usage
// riding the last content frame, GLM-style), finish, enveloped [DONE].
func happySSE() string {
	return envFrame(`{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`) +
		envFrame(`{"choices":[{"index":0,"delta":{"content":"hel"},"finish_reason":null}]}`) +
		envFrame(`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`) +
		envFrame(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		envFrame("[DONE]")
}

type chatReply struct {
	status int // 0 → 200 with sse body
	sse    string
}

// stubUpstream is the fake gateway: REST exchange, userinfo, machine-signed
// user/status and legacy jobToken, quota usage, and the enveloped chat SSE.
type stubUpstream struct {
	mu sync.Mutex

	jt    string
	jrt   string
	exch  int         // exchangeStatus override (0 → 200)
	chat  []chatReply // per-call script; last entry repeats
	quota struct{ remaining float64 }

	// observations
	exchangeBodies []string
	userinfoAuth   []string
	statusCalls    int
	statusUserIDs  []string
	legacyCalls    int
	quotaAuth      []string
	chatCalls      int
	chatAuth       []string
	chatHeaders    []http.Header
	chatInner      []map[string]any
	chatEncoded    []string
}

func (u *stubUpstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobToken/exchange", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		u.exchangeBodies = append(u.exchangeBodies, string(raw))
		if u.exch != 0 {
			w.WriteHeader(u.exch)
			fmt.Fprint(w, `{"message":"nope"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":%q,"refresh_token":%q,"expires_in":86400000,"expires_at":"2026-10-16T00:00:00Z"}`, u.jt, u.jrt)
	})
	mux.HandleFunc("/api/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.userinfoAuth = append(u.userinfoAuth, r.Header.Get("Authorization"))
		u.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+u.jt {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"id":"user-1","name":"tester","organization_id":"org-1","organization_name":"Fake Org"}`)
	})
	mux.HandleFunc("/api/v2/quota/usage", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.quotaAuth = append(u.quotaAuth, r.Header.Get("Authorization"))
		rem := u.quota.remaining
		u.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+u.jt {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"userId":"user-1","userQuota":{"total":100,"used":10,"remaining":%v}}`, rem)
	})
	mux.HandleFunc("/algo/api/v3/user/status", func(w http.ResponseWriter, r *http.Request) {
		inner := u.readEncodedPayload(r)
		u.mu.Lock()
		u.statusCalls++
		u.statusUserIDs = append(u.statusUserIDs, stringOf(inner["userId"]))
		u.mu.Unlock()
		// Machine-signature family: date + signature, no bearer.
		if date := r.Header.Get("date"); date == "" || r.Header.Get("signature") != cosySign(date) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("appcode") != cosyAppCode || r.Header.Get("login-version") != "v2" || r.Header.Get("authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"userType":"personal_standard","plan":"PLAN_TIER_FREE","userTag":"Free"}`)
	})
	mux.HandleFunc("/algo/api/v3/user/jobToken", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.legacyCalls++
		u.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":"401","message":"personal token is invalid"}`)
	})
	mux.HandleFunc("/algo/api/v2/service/pro/sse/agent_chat_generation", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		encoded := string(raw)
		inner := u.decodeChatBody(r, encoded)

		u.mu.Lock()
		idx := u.chatCalls
		u.chatCalls++
		u.chatAuth = append(u.chatAuth, r.Header.Get("authorization"))
		u.chatHeaders = append(u.chatHeaders, r.Header.Clone())
		u.chatInner = append(u.chatInner, inner)
		u.chatEncoded = append(u.chatEncoded, encoded)
		var reply chatReply
		switch {
		case len(u.chat) == 0:
			reply = chatReply{status: http.StatusInternalServerError}
		case idx < len(u.chat):
			reply = u.chat[idx]
		default:
			reply = u.chat[len(u.chat)-1]
		}
		u.mu.Unlock()

		// Bearer family assertions: COSY bearer whose MD5 signature covers
		// payloadB64 \n cosyKey \n date \n body \n sigPath.
		auth := r.Header.Get("authorization")
		date := r.Header.Get("cosy-date")
		if !strings.HasPrefix(auth, "Bearer COSY.") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rest := strings.TrimPrefix(auth, "Bearer COSY.")
		dot := strings.LastIndexByte(rest, '.')
		if dot < 0 || date == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		payloadB64, sig := rest[:dot], rest[dot+1:]
		cosyKey := r.Header.Get("cosy-key")
		expected := md5Hex(payloadB64 + "\n" + cosyKey + "\n" + date + "\n" + encoded + "\n" + chatSigPath)
		if sig != expected || cosyKey == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if reply.status != 0 && reply.status != http.StatusOK {
			w.WriteHeader(reply.status)
			fmt.Fprint(w, reply.sse)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, reply.sse)
	})
	return mux
}

// readEncodedPayload decodes an auth-family Encode-wrapped body into the
// inner payload object.
func (u *stubUpstream) readEncodedPayload(r *http.Request) map[string]any {
	raw, _ := io.ReadAll(r.Body)
	decoded, err := customDecode(string(raw))
	if err != nil {
		return nil
	}
	var outer struct {
		Payload string `json:"payload"`
	}
	if json.Unmarshal(decoded, &outer) != nil {
		return nil
	}
	var inner map[string]any
	_ = json.Unmarshal([]byte(outer.Payload), &inner)
	return inner
}

// decodeChatBody decodes a chat request body back to its inner JSON object.
func (u *stubUpstream) decodeChatBody(_ *http.Request, encoded string) map[string]any {
	decoded, err := customDecode(encoded)
	if err != nil {
		return nil
	}
	var inner map[string]any
	_ = json.Unmarshal(decoded, &inner)
	return inner
}

type obs struct {
	exchangeBodies []string
	userinfoAuth   []string
	statusCalls    int
	statusUserIDs  []string
	legacyCalls    int
	quotaAuth      []string
	chatCalls      int
	chatAuth       []string
	chatHeaders    []http.Header
	chatInner      []map[string]any
	chatEncoded    []string
}

func (u *stubUpstream) snapshot() obs {
	u.mu.Lock()
	defer u.mu.Unlock()
	return obs{
		exchangeBodies: append([]string(nil), u.exchangeBodies...),
		userinfoAuth:   append([]string(nil), u.userinfoAuth...),
		statusCalls:    u.statusCalls,
		statusUserIDs:  append([]string(nil), u.statusUserIDs...),
		legacyCalls:    u.legacyCalls,
		quotaAuth:      append([]string(nil), u.quotaAuth...),
		chatCalls:      u.chatCalls,
		chatAuth:       append([]string(nil), u.chatAuth...),
		chatHeaders:    u.chatHeaders,
		chatInner:      u.chatInner,
		chatEncoded:    u.chatEncoded,
	}
}

// fakeSource is the credential source the tests wire in.
type fakeSource struct{ pat string }

func (f fakeSource) CredentialFor(_ context.Context, acct *pool.Account) (*PATCredential, error) {
	if acct == nil || acct.ID == "ghost" {
		return nil, fmt.Errorf("credential store: no credential for %s/%s", "qoder", acctIDOf(acct))
	}
	return &PATCredential{PAT: f.pat}, nil
}

func acctIDOf(acct *pool.Account) string {
	if acct == nil {
		return "<nil>"
	}
	return acct.ID
}

// newSender builds a sender wired to the stub.
func newSender(t *testing.T, u *stubUpstream) *senderImpl {
	t.Helper()
	srv := httptest.NewServer(u.handler())
	t.Cleanup(srv.Close)
	s := &senderImpl{}
	s.Configure(ClientOptions{
		AuthBaseURL:    srv.URL,
		OpenAPIBaseURL: srv.URL,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		Credentials:    fakeSource{pat: fakePAT},
	})
	return s
}

func chatReq(model string) adapter.ChatRequest {
	return adapter.ChatRequest{
		Model:    model,
		Messages: []adapter.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
}

func testAccount() *pool.Account {
	return &pool.Account{ID: "fp-1", Platform: "qoder", Status: "active"}
}

// happyStub is a stub with a healthy one-shot chat script.
func happyStub() *stubUpstream {
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.quota.remaining = 1234.6
	u.chat = []chatReply{{sse: happySSE()}}
	return u
}

func TestCustomEncodeRoundTrip(t *testing.T) {
	for _, in := range []string{"", "hi", "hello world", strings.Repeat("x", 100), "{\"a\":1,\"b\":[2,3]}"} {
		enc, err := customEncode([]byte(in))
		if err != nil {
			t.Fatalf("encode %q: %v", in, err)
		}
		for i := 0; i < len(enc); i++ {
			if enc[i] >= 128 || !c2sOK[enc[i]] {
				t.Fatalf("encode output byte %q outside custom alphabet", enc[i])
			}
		}
		dec, err := customDecode(enc)
		if err != nil || string(dec) != in {
			t.Fatalf("round trip %q → %q (err %v)", in, dec, err)
		}
	}
	// Padding maps '=' → '$': a 2-byte input has one '=' in std base64, a
	// 3-byte input none.
	enc, err := customEncode([]byte("hi"))
	if err != nil || !strings.ContainsRune(enc, '$') {
		t.Fatalf("padding '=' not mapped to '$': %q %v", enc, err)
	}
	enc, err = customEncode([]byte("hi!"))
	if err != nil || strings.ContainsRune(enc, '$') {
		t.Fatalf("unexpected padding: %q %v", enc, err)
	}
	if _, err := customDecode("£€"); err == nil {
		t.Fatal("decode must reject out-of-alphabet characters")
	}
}

func TestResolveRegion(t *testing.T) {
	r := ResolveRegion(fakePAT)
	if r.Name != "cn" || r.AuthBase != "https://gateway.qoder.com.cn" || r.ChatBase != "https://gateway.qoder.com.cn" {
		t.Errorf("region = %+v, want CN with gateway.qoder.com.cn auth/chat", r)
	}
	if r.OpenAPIBase != "https://openapi.qoder.sh" {
		t.Errorf("openapi base = %q, want https://openapi.qoder.sh (international host serves the exchange even for CN)", r.OpenAPIBase)
	}
}

func TestSigPathStripsAlgoAndQuery(t *testing.T) {
	got := sigPath("https://gateway.qoder.com.cn" + pathChat)
	if got != chatSigPath {
		t.Errorf("sigPath = %q, want %q", got, chatSigPath)
	}
}

func TestModelKeyResolution(t *testing.T) {
	cases := map[string]string{
		"GLM-5.3":     "gmodel",
		"Qwen3.8-Max": "qmodel_38max",
		"gmodel":      "gmodel",      // known key passes through
		"weird-model": "weird-model", // unknown passes through (gateway judges)
		"":            "qmodel_latest",
		" qmodel ":    "qmodel",
	}
	for in, want := range cases {
		if got := resolveModelKey(in); got != want {
			t.Errorf("resolveModelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSendChatAggregatesProtocolShape(t *testing.T) {
	u := happyStub()
	s := newSender(t, u)

	resp, err := s.SendChat(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 {
		t.Fatalf("response shape: %+v", resp)
	}
	var content string
	if json.Unmarshal(resp.Choices[0].Message.Content, &content) != nil || content != "hello" {
		t.Errorf("content = %s, want \"hello\"", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", resp.Choices[0].FinishReason)
	}

	o := u.snapshot()
	// Exchange chain: REST exchange → userinfo → user/status → chat.
	if len(o.exchangeBodies) != 1 || o.exchangeBodies[0] != fmt.Sprintf(`{"personal_token":%q}`, fakePAT) {
		t.Errorf("exchange body = %q, want the personal_token payload", o.exchangeBodies)
	}
	if len(o.userinfoAuth) != 1 || o.userinfoAuth[0] != "Bearer jt-fake-1" {
		t.Errorf("userinfo auth = %v, want the Bearer jobToken", o.userinfoAuth)
	}
	if o.statusCalls != 1 || o.statusUserIDs[0] != "user-1" {
		t.Errorf("user/status calls = %d ids = %v, want 1 × user-1", o.statusCalls, o.statusUserIDs)
	}
	if o.chatCalls != 1 {
		t.Fatalf("chat calls = %d, want 1", o.chatCalls)
	}

	h := o.chatHeaders[0]
	for key, want := range map[string]string{
		"Authorization":    "Bearer COSY.",
		"Cosy-Data-Policy": "AGREE",
		"Cosy-Version":     cosyVersion,
		"Cosy-Clienttype":  "5",
		"Cosy-User":        "user-1",
		"X-Model-Key":      "gmodel",
		"X-Model-Source":   "system",
		"Accept":           "text/event-stream",
		"Accept-Encoding":  "identity",
		"Cache-Control":    "no-cache",
		"Login-Version":    "v2",
		"User-Agent":       "Go-http-client/2.0",
		"Content-Type":     "application/json",
	} {
		if !strings.HasPrefix(h.Get(key), want) {
			t.Errorf("chat header %s = %q, want prefix %q", key, h.Get(key), want)
		}
	}
	if _, err := parseUnix(h.Get("Cosy-Date")); err != nil {
		t.Errorf("cosy-date = %q, want unix seconds", h.Get("Cosy-Date"))
	}

	// Inner chat request body shape (distilled from vhub HandleChat).
	inner := o.chatInner[0]
	if inner["stream"] != true || inner["chat_task"] != "FREE_INPUT" || inner["session_type"] != "qodercli" ||
		inner["agent_id"] != "agent_common" || inner["task_id"] != "common" || inner["version"] != "3" || inner["source"] != float64(1) {
		t.Errorf("chat envelope constants wrong: %v", inner)
	}
	if inner["aliyun_user_type"] != "personal_standard" {
		t.Errorf("aliyun_user_type = %v, want the resolved tier", inner["aliyun_user_type"])
	}
	mc, _ := inner["model_config"].(map[string]any)
	if mc == nil || mc["key"] != "gmodel" || mc["source"] != "system" || mc["is_reasoning"] != true {
		t.Errorf("model_config = %v, want gmodel/system/reasoning", mc)
	}
	ctxMap, _ := inner["chat_context"].(map[string]any)
	if ctxMap == nil {
		t.Fatal("chat_context missing")
	}
	text, _ := ctxMap["text"].(map[string]any)
	if text == nil || text["type"] != "text" || text["text"] != "hi" {
		t.Errorf("chat_context.text = %v, want the latest user prompt", text)
	}
	msgs, _ := inner["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want one user turn", inner["messages"])
	}
	m0, _ := msgs[0].(map[string]any)
	contents, _ := m0["contents"].([]any)
	c0, _ := contents[0].(map[string]any)
	if m0["role"] != "user" || c0["type"] != "text" || c0["text"] != "hi" {
		t.Errorf("user message shape = %v / %v", m0, c0)
	}
	params, _ := inner["parameters"].(map[string]any)
	if params == nil || params["max_tokens"] != float64(defaultMaxTokens) {
		t.Errorf("parameters = %v, want default max_tokens %d", params, defaultMaxTokens)
	}
	for _, id := range []string{"request_id", "request_set_id", "chat_record_id", "session_id"} {
		if s, _ := inner[id].(string); s == "" {
			t.Errorf("%s missing/empty", id)
		}
	}
	biz, _ := inner["business"].(map[string]any)
	if biz == nil || biz["id"] == "" || biz["name"] != "hi" {
		t.Errorf("business = %v, want id + truncated prompt name", biz)
	}
}

func parseUnix(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func TestSendChatStreamRelaysChunks(t *testing.T) {
	u := happyStub()
	s := newSender(t, u)

	chunks, errCh, err := s.SendChatStream(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	if err != nil {
		t.Fatalf("SendChatStream: %v", err)
	}
	var contents []string
	var finish *string
	for chunk := range chunks {
		if len(chunk.Choices) == 0 {
			t.Fatal("chunk without choices")
		}
		c := chunk.Choices[0]
		if len(c.Delta.Content) > 0 {
			var s string
			if json.Unmarshal(c.Delta.Content, &s) == nil {
				contents = append(contents, s)
			}
		}
		if c.FinishReason != nil {
			finish = c.FinishReason
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("chunk object = %q", chunk.Object)
		}
	}
	if strings.Join(contents, "") != "hello" || finish == nil || *finish != "stop" {
		t.Errorf("stream = %v finish %v, want hello/stop", contents, finish)
	}
	select {
	case err := <-errCh:
		t.Fatalf("healthy stream delivered an error: %v", err)
	default:
	}
}

func TestMaxTokensAndParamsPassThrough(t *testing.T) {
	u := happyStub()
	s := newSender(t, u)
	max := 128
	temp := 0.5
	req := chatReq("GLM-5.3")
	req.MaxTokens = &max
	req.Temperature = &temp
	req.Stop = json.RawMessage(`"\n\n"`)
	req.ToolChoice = json.RawMessage(`"auto"`)
	req.Tools = json.RawMessage(`[{"type":"function","function":{"name":"f"}}]`)
	if _, err := s.SendChat(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(), testAccount(), req); err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	inner := u.snapshot().chatInner[0]
	params := inner["parameters"].(map[string]any)
	if params["max_tokens"] != float64(128) || params["temperature"] != float64(0.5) || params["tool_choice"] != "auto" {
		t.Errorf("parameters = %v, want passthrough values", params)
	}
	if params["stop"] != "\n\n" {
		t.Errorf("stop = %v", params["stop"])
	}
	if inner["tools"] == nil {
		t.Error("tools not forwarded")
	}
}

func TestAuth401ReexchangeAndReplay(t *testing.T) {
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{
		{status: 401, sse: `{"code":"x","message":"Login expired"}`},
		{sse: happySSE()},
	}
	s := newSender(t, u)

	resp, err := s.SendChat(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	if err != nil {
		t.Fatalf("SendChat after replay: %v", err)
	}
	if resp == nil || len(resp.Choices) == 0 {
		t.Fatal("no usable response after replay")
	}
	o := u.snapshot()
	if o.chatCalls != 2 {
		t.Errorf("chat calls = %d, want 2 (initial + replay)", o.chatCalls)
	}
	if len(o.exchangeBodies) != 2 {
		t.Errorf("exchange calls = %d, want 2 (bootstrap + forced re-exchange)", len(o.exchangeBodies))
	}
}

func TestBusy10605BacksOffThenSucceeds(t *testing.T) {
	oldBackoff := busyFallbackBackoff
	busyFallbackBackoff = 10 * time.Millisecond
	t.Cleanup(func() { busyFallbackBackoff = oldBackoff })

	// No retryAfterSeconds in the busy body: the retry falls back to the
	// shortened test backoff instead of sleeping the server-suggested delay.
	busy := `{"code":"10605","message":"{\"isQueued\":false}"}`
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{
		{status: 401, sse: busy},
		{sse: happySSE()},
	}
	s := newSender(t, u)

	resp, err := s.SendChat(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	if err != nil {
		t.Fatalf("SendChat after busy backoff: %v", err)
	}
	if resp == nil {
		t.Fatal("no response")
	}
	o := u.snapshot()
	if o.chatCalls != 2 {
		t.Errorf("chat calls = %d, want 2 (busy + retry)", o.chatCalls)
	}
	// A busy rejection must NOT trigger a token re-exchange (the session is
	// valid — that is the whole point of the 10605 distinction).
	if len(o.exchangeBodies) != 1 {
		t.Errorf("exchange calls = %d, want 1 (no pointless re-exchange)", len(o.exchangeBodies))
	}
}

func TestBusyExhaustedClassifiesRateLimited(t *testing.T) {
	oldBackoff := busyFallbackBackoff
	busyFallbackBackoff = 5 * time.Millisecond
	t.Cleanup(func() { busyFallbackBackoff = oldBackoff })

	busy := `{"code":"10605","message":"{\"retryAfterSeconds\":2}"}`
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{{status: 403, sse: busy}}
	s := newSender(t, u)

	_, err := s.SendChat(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Reason != adapter.ReasonRateLimited {
		t.Fatalf("err = %v, want rate_limited", err)
	}
	if !strings.Contains(ue.Message, "10605") {
		t.Errorf("message = %q, want the busy code", ue.Message)
	}
}

func TestPersistent401ClassifiesAuthExpired(t *testing.T) {
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{{status: 401, sse: `{"message":"Login expired"}`}}
	s := newSender(t, u)

	_, err := s.SendChat(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
		t.Fatalf("err = %v, want auth_expired", err)
	}
	if u.snapshot().chatCalls != 2 {
		t.Error("expected exactly one refresh-and-retry round")
	}
}

func Test429CarriesRateLimited(t *testing.T) {
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{{status: http.StatusTooManyRequests, sse: `{"message":"slow down"}`}}
	s := newSender(t, u)

	_, err := s.SendChat(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Reason != adapter.ReasonRateLimited || ue.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want 429 rate_limited", err)
	}
}

func TestDeadPATSurfacesAuthExpiredWithoutLeak(t *testing.T) {
	u := &stubUpstream{jt: "unused", exch: http.StatusUnauthorized}
	s := newSender(t, u)

	_, err := s.SendChat(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
		t.Fatalf("err = %v, want auth_expired when both exchange paths reject", err)
	}
	if u.snapshot().legacyCalls != 1 {
		t.Errorf("legacy exchange calls = %d, want the fallback attempt", u.snapshot().legacyCalls)
	}
	// Red line: the PAT itself never appears in an error message.
	if strings.Contains(err.Error(), fakePAT) {
		t.Error("PAT material leaked into the error message")
	}
}

func TestInStreamAuthFailureClassifies(t *testing.T) {
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{{sse: envFrame(`{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`) +
		envFrame(`{"code":"105","message":"Login expired"}`)}}
	// Wrap the auth frame in a 401-status envelope instead: build manually.
	u.chat[0].sse = envFrame(`{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`) +
		"data: " + mustJSON(map[string]any{"body": `{"code":"105","message":"Login expired"}`, "statusCode": "UNAUTHORIZED", "statusCodeValue": 401}) + "\n\n"
	s := newSender(t, u)

	chunks, errCh, err := s.SendChatStream(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	if err != nil {
		t.Fatalf("SendChatStream: %v", err)
	}
	n := 0
	for range chunks {
		n++
	}
	if n != 1 {
		t.Errorf("chunks before failure = %d, want the single partial delta", n)
	}
	select {
	case err := <-errCh:
		var ue *adapter.UpstreamError
		if !errors.As(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
			t.Fatalf("in-stream err = %v, want auth_expired", err)
		}
	default:
		t.Fatal("no in-stream error delivered")
	}
}

func TestInStreamBusyClassifiesRateLimited(t *testing.T) {
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{{sse: "data: " + mustJSON(map[string]any{
		"body":            `{"code":"10605","message":"{\"retryAfterSeconds\":3}"}`,
		"statusCode":      "FORBIDDEN",
		"statusCodeValue": 403,
	}) + "\n\n"}}
	s := newSender(t, u)

	chunks, errCh, err := s.SendChatStream(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	if err != nil {
		t.Fatalf("SendChatStream: %v", err)
	}
	for range chunks {
	}
	select {
	case err := <-errCh:
		var ue *adapter.UpstreamError
		if !errors.As(err, &ue) || ue.Reason != adapter.ReasonRateLimited {
			t.Fatalf("in-stream err = %v, want rate_limited", err)
		}
		if !strings.Contains(ue.Message, "retry after") {
			t.Errorf("message = %q, want the retry hint", ue.Message)
		}
	default:
		t.Fatal("no in-stream error delivered")
	}
}

// TestInStreamWrappedBusyClassifiesRateLimited pins the double-wrapped shape
// seen in production: a 403 frame whose body is a {"code":"403",...} envelope
// with the real 10605 rejection stringified inside its message field. Before
// busyFromBody learned to unwrap, this classified as auth_expired and burned
// a valid credential.
func TestInStreamWrappedBusyClassifiesRateLimited(t *testing.T) {
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{{sse: "data: " + mustJSON(map[string]any{
		"body":            `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"dfmodel\\\",\\\"retryAfterSeconds\\\":30,\\\"serviceAvailable\\\":true,\\\"waitTime\\\":30}\"}"}`,
		"statusCode":      "FORBIDDEN",
		"statusCodeValue": 403,
	}) + "\n\n"}}
	s := newSender(t, u)

	chunks, errCh, err := s.SendChatStream(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	if err != nil {
		t.Fatalf("SendChatStream: %v", err)
	}
	for range chunks {
	}
	select {
	case err := <-errCh:
		var ue *adapter.UpstreamError
		if !errors.As(err, &ue) || ue.Reason != adapter.ReasonRateLimited {
			t.Fatalf("in-stream err = %v, want rate_limited", err)
		}
		if !strings.Contains(ue.Message, "gateway busy (10605)") || !strings.Contains(ue.Message, "retry after 30s") {
			t.Errorf("message = %q, want busy 10605 with the 30s retry hint", ue.Message)
		}
	default:
		t.Fatal("no in-stream error delivered")
	}
}

func TestTruncatedStreamFails(t *testing.T) {
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{{sse: envFrame(`{"choices":[{"index":0,"delta":{"content":"cut"},"finish_reason":null}]}`)}}
	s := newSender(t, u)

	chunks, errCh, err := s.SendChatStream(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	if err != nil {
		t.Fatalf("SendChatStream: %v", err)
	}
	for range chunks {
	}
	select {
	case err := <-errCh:
		var ue *adapter.UpstreamError
		if !errors.As(err, &ue) || ue.Reason != adapter.ReasonPlatformDown {
			t.Fatalf("err = %v, want platform_down for a stream without [DONE]", err)
		}
	default:
		t.Fatal("truncated stream must fail, not look complete")
	}
}

func TestQuotaRemainingParsesOpenAPIShape(t *testing.T) {
	u := happyStub()
	s := newSender(t, u)

	rem, err := s.QuotaRemaining(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/probe", nil).Context(), testAccount())
	if err != nil {
		t.Fatalf("QuotaRemaining: %v", err)
	}
	if rem != 1235 {
		t.Errorf("remaining = %d, want 1235 (rounded)", rem)
	}
	o := u.snapshot()
	if len(o.quotaAuth) != 1 || o.quotaAuth[0] != "Bearer jt-fake-1" {
		t.Errorf("quota auth = %v, want the Bearer jobToken", o.quotaAuth)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestResourceLimitWordingClassifiesBusy pins the queue family's second
// wording (community issue #38/#51): "RESOURCE_LIMIT" on a 403 envelope is
// the same admission-control rejection as 10605 and must ride the busy
// backoff path, not trip the auth/5xx buckets.
func TestResourceLimitWordingClassifiesBusy(t *testing.T) {
	u := &stubUpstream{jt: "jt-fake-1", jrt: "jrt-fake-1"}
	u.chat = []chatReply{{status: http.StatusForbidden, sse: `{"code":"500504","message":"RESOURCE_LIMIT: model queue is full"}`}}
	s := newSender(t, u)

	_, err := s.SendChat(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).Context(),
		testAccount(), chatReq("GLM-5.3"))
	// Exhausted-busy semantics (TestBusyExhaustedClassifiesRateLimited): the
	// signal retries once, then surfaces as the canonical rate-limited error.
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Reason != adapter.ReasonRateLimited {
		t.Fatalf("err = %v, want rate_limited for the RESOURCE_LIMIT wording", err)
	}
	if !strings.Contains(ue.Message, "RESOURCE_LIMIT") {
		t.Fatalf("message = %q, want the upstream wording preserved", ue.Message)
	}
}
