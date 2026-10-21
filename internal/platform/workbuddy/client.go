//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// client.go implements adapter.Sender for the WorkBuddy/CodeBuddy upstream.
//
// Wire facts (docs/upstream/workbuddy-protocol.md, every point sourced there):
//   - POST {base}/v2/chat/completions, OpenAI chat-completions shaped, but
//     stream:true is mandatory — non-stream requests are aggregated locally.
//   - Auth: Authorization: Bearer <accessToken> plus the X-User-Id /
//     X-Enterprise-Id / X-Domain account headers (X-No-* placeholders when
//     absent). The refresh token never travels on the chat path.
//   - Refresh: on an auth-expired reply (401 / session-death envelope) or a
//     pre-send expiry, the sender POSTs /v2/plugin/auth/token/refresh with
//     X-Refresh-Token + X-Auth-Refresh-Source: plugin (no Authorization),
//     hands the rotated pair to ClientOptions.Refresher and replays the send
//     once (refresh.go: single-flight per account, 5-min quiet window).
//   - Request rewrites the upstream enforces (400 code=11101/11-128 else):
//     tool_choice must be a string, image_url must be an object, role
//     "developer" must be renamed "system".
//   - Errors: {code,msg,data} envelope decoupled from the HTTP status; the
//     6004/6008 codes are per-(account,model) daily quota, other 6xxx are
//     account-level rate limits; 403 without a business envelope is WAF.
//   - Retry hints: Retry-After (seconds), retry-after-ms (milliseconds),
//     x-ratelimit-reset (epoch seconds or milliseconds).
//
// The client is stdlib-only. The upstream base URL and the credential source
// are injectable so tests run against httptest stubs only — the test suite
// never contacts the real endpoints.
package workbuddy

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
	"sync"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/pool"
)

// wireChatPath is the chat completions path both realms share (report §2).
const wireChatPath = "/v2/chat/completions"

// wireRefreshPath is the token refresh endpoint (report §3). It lives on the
// same realm base as chat; its auth is the X-Refresh-Token header (plus
// X-Auth-Refresh-Source: plugin) — never an Authorization bearer.
const wireRefreshPath = "/v2/plugin/auth/token/refresh"

// Production chat bases (report §2). CN: copilot.tencent.com with the
// www.codebuddy.cn brand; global: www.workbuddy.ai. Both are bypassed in
// tests via ClientOptions.BaseURL (httptest stubs).
const (
	wireDefaultBaseCN     = "https://copilot.tencent.com"
	wireDefaultBaseGlobal = "https://www.workbuddy.ai"
	wireOriginCN          = "https://www.codebuddy.cn"
	wireOriginGlobal      = "https://www.workbuddy.ai"
)

// wireClientVersion is the WorkBuddy desktop version segment in the UA.
// Fingerprint discipline (report §8): the version must track the official
// distribution — it is a var so integrators can update it without a protocol
// change. Stale versions are a documented 11140 risk-control trigger; the
// 2026-10-21 restrictions hit while 5.5.4 (2026-09 captures) was two minor
// releases behind. Current values track the community-maintained reference
// (workbuddy2api-hub wb_identity.py, test-pinned) and the locally installed
// official client (5.8.3). The CLI segment is its own 2.x release stream —
// the old single-version UA was a fingerprint mismatch of its own.
var wireClientVersion = "5.8.3"
var wireCLIVersion = "2.161.0"

// Credential is the account credential snapshot one send needs.
// pool.Account deliberately carries no secrets (pool.go), so credentials are
// injected via CredentialSource — this package stays storage-free.
type Credential struct {
	AccessToken  string // Bearer token; empty → auth failure before the send
	RefreshToken string // never sent on the chat path; for integrator-side refresh
	UID          string // account id → X-User-Id
	EnterpriseID string // personal accounts: empty → X-No-Enterprise-Id
	Domain       string // empty → realm default
	Global       bool   // realm flag: base URL + Origin/UA brand segment
	ExpiresAt    time.Time
}

// CredentialSource supplies the credential for one pool account. A nil
// *Credential (with nil error) means "no usable credential" — the send fails
// with ReasonAuthExpired and the router retries on the next account.
type CredentialSource interface {
	CredentialFor(ctx context.Context, acct *pool.Account) (*Credential, error)
}

// CredentialSourceFunc adapts a function to CredentialSource.
type CredentialSourceFunc func(ctx context.Context, acct *pool.Account) (*Credential, error)

// CredentialFor implements CredentialSource.
func (f CredentialSourceFunc) CredentialFor(ctx context.Context, acct *pool.Account) (*Credential, error) {
	return f(ctx, acct)
}

// CredentialSink receives the credential pair the sender rotated through the
// refresh endpoint (refresh.go). Implemented integrator-side over the
// credential store: by the time AcceptRefreshed returns, CredentialFor must
// serve the new tokens, because the failed send replays immediately after.
type CredentialSink interface {
	AcceptRefreshed(ctx context.Context, acct *pool.Account, cred *Credential) error
}

// CredentialState is the optional health/status hook the adapter surfaces
// (panel credential aging, health checks). Production wiring implements it
// over the credential store; absent it the adapter reports the neutral
// "expiry managed by platform" shape.
type CredentialState interface {
	HealthCheck(ctx context.Context) (*adapter.HealthStatus, error)
	Status() (*adapter.CredentialInfo, error)
}

// ClientOptions configures one sender. Zero value is production-shaped:
// realm-default bases, no credential source, stdlib transport.
type ClientOptions struct {
	// BaseURL overrides the upstream chat base for both realms. Tests point
	// this at an httptest server.
	BaseURL string
	// HTTPClient overrides the transport. Nil uses a shared default client.
	HTTPClient *http.Client
	// Credentials resolves per-account credentials. Production wiring must
	// set it; nil makes every send fail with ReasonAuthExpired.
	Credentials CredentialSource
	// State optionally exposes health/credential status to the adapter.
	State CredentialState
	// Refresher receives tokens rotated by the 401/expiry refresh flow
	// (refresh.go). Production wiring sets it to the credential store; nil
	// disables refresh-and-replay — auth failures surface as-is.
	Refresher CredentialSink
	// Now overrides the clock for credential-expiry checks (tests).
	Now func() time.Time
}

// senderImpl carries the adapter.Sender implementation; the Adapter embeds
// it so the router's adapter.Sender type assertion finds the methods.
type senderImpl struct {
	mu   sync.Mutex
	opts ClientOptions
	http *http.Client

	// refreshGate serializes token rotation (refresh.go): one in-flight
	// refresh per account (single-flight) plus the last-success clock behind
	// the quiet window. Maps initialize lazily under refreshMu.
	refreshMu   sync.Mutex
	refreshBusy map[string]*refreshFlight
	refreshOKAt map[string]time.Time
}

// NewSender builds a sender with production defaults (used by init()).
func NewSender() *senderImpl { return &senderImpl{} }

// newSender is the internal constructor alias used by init().
func newSender() *senderImpl { return &senderImpl{} }

// credentials returns the optional credential-state hook (adapter.go health
// and status surfaces). Nil until Configure wires one via ClientOptions.
func (s *senderImpl) credentials() CredentialState {
	return s.options().State
}

// Configure sets the client options (integrator wiring, before serving).
func (s *senderImpl) Configure(opts ClientOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts = opts
	s.http = nil
}

func (s *senderImpl) httpClient() *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http != nil {
		return s.http
	}
	if s.opts.HTTPClient != nil {
		s.http = s.opts.HTTPClient
	} else {
		// No global timeout: SSE streams run arbitrarily long; ctx is the
		// cancellation surface (router derives it from the client request).
		s.http = &http.Client{}
	}
	return s.http
}

func (s *senderImpl) options() ClientOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts
}

func (s *senderImpl) now() time.Time {
	if fn := s.options().Now; fn != nil {
		return fn()
	}
	return time.Now()
}

// baseFor returns the chat base URL for the credential's realm.
func (s *senderImpl) baseFor(cred *Credential) string {
	if base := s.options().BaseURL; base != "" {
		return base
	}
	if cred != nil && cred.Global {
		return wireDefaultBaseGlobal
	}
	return wireDefaultBaseCN
}

// originFor returns the Origin/Referer brand domain for the realm.
func originFor(cred *Credential) string {
	if cred != nil && cred.Global {
		return wireOriginGlobal
	}
	return wireOriginCN
}

// userAgent assembles the official UA (report §8, 2026-10 shape):
// "WorkBuddy/<desktop> <product>/<desktop> CLI/<cli>". The product segment is
// the lowercase applicationName on the global realm (workbuddy-ai) and
// WorkBuddy on CN — tracked from the reference implementation's test-pinned
// values, not the older "WorkBuddy AI" spelling.
func userAgent(cred *Credential) string {
	platform := "WorkBuddy"
	if cred != nil && cred.Global {
		platform = "workbuddy-ai"
	}
	return "WorkBuddy/" + wireClientVersion + " " + platform + "/" + wireClientVersion + " CLI/" + wireCLIVersion
}

// setCommonHeaders applies the header set every upstream call shares
// (report §3/§8): JSON content, XHR marker, realm brand Origin/Referer, the
// X-CodeBuddy-Request gate header, realm Accept-Language and stream Accept.
func setCommonHeaders(req *http.Request, cred *Credential) {
	origin := originFor(cred)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", userAgent(cred))
	req.Header.Set("X-CodeBuddy-Request", "1")
	if cred != nil && cred.Global {
		req.Header.Set("Accept-Language", "en-US")
	} else {
		req.Header.Set("Accept-Language", "zh-CN")
	}
}

// setAccountHeaders applies the per-account identity headers (report §3):
// Bearer auth (or the X-No-Authorization placeholder), X-User-Id /
// X-Enterprise-Id with X-No-* fallbacks and X-Domain realm defaults.
func setAccountHeaders(req *http.Request, cred *Credential) {
	if cred == nil {
		req.Header.Set("X-No-Authorization", "1")
		req.Header.Set("X-No-User-Id", "1")
		req.Header.Set("X-No-Enterprise-Id", "1")
		return
	}
	if cred.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if cred.UID != "" {
		req.Header.Set("X-User-Id", cred.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if cred.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
		req.Header.Set("X-Tenant-Id", cred.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	domain := cred.Domain
	if domain == "" {
		if cred.Global {
			domain = "www.workbuddy.ai"
		} else {
			// The chat base host itself — 2026-10 reference parity; the old
			// www.codebuddy.cn value was an older observation.
			domain = "copilot.tencent.com"
		}
	}
	req.Header.Set("X-Domain", domain)
}

// SendChat performs one non-streaming chat completion. The upstream only
// speaks SSE, so the request always goes out with stream:true and the frames
// are aggregated into a single ChatResponse. Auth-expired replies (401 /
// session death) and pre-send expiry route through one token refresh + one
// replay (refresh.go).
func (s *senderImpl) SendChat(ctx context.Context, acct *pool.Account, req adapter.ChatRequest) (*adapter.ChatResponse, error) {
	body, err := buildUpstreamBody(req)
	if err != nil {
		return nil, &adapter.UpstreamError{Status: http.StatusBadRequest, Reason: adapter.ReasonNotFound, Message: "workbuddy: build request body: " + err.Error()}
	}
	rc, sendErr := s.postWithRotation(ctx, acct, body)
	if sendErr != nil {
		return nil, sendErr
	}
	defer rc.Close()
	resp, aggErr := aggregateStream(rc)
	if aggErr != nil {
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: aggregate stream: " + aggErr.Error()}
	}
	return resp, nil
}

// SendChatStream starts one streaming chat completion per the Sender channel
// contract: chunks close on healthy completion; errCh carries at most one
// error and is NOT closed on a healthy stream; both stop when ctx is done.
func (s *senderImpl) SendChatStream(ctx context.Context, acct *pool.Account, req adapter.ChatRequest) (<-chan adapter.StreamChunk, <-chan error, error) {
	body, err := buildUpstreamBody(req)
	if err != nil {
		return nil, nil, &adapter.UpstreamError{Status: http.StatusBadRequest, Reason: adapter.ReasonNotFound, Message: "workbuddy: build request body: " + err.Error()}
	}
	rc, sendErr := s.postWithRotation(ctx, acct, body)
	if sendErr != nil {
		return nil, nil, sendErr
	}
	chunks := make(chan adapter.StreamChunk)
	// Buffered so a mid-stream failure can be delivered and the producer can
	// exit even when the router has stopped reading. An open, idle errCh is
	// the idle (healthy) state per the Sender contract.
	errCh := make(chan error, 1)
	go func() {
		defer close(chunks)
		defer rc.Close()
		relayStream(ctx, rc, chunks, errCh)
	}()
	return chunks, errCh, nil
}

// relayStream parses the SSE body and forwards chat.completion.chunk frames.
// Mirrors the router's expectations: close(chunks) on [DONE], at most one
// errCh delivery on failure, prompt exit when ctx is done.
func relayStream(ctx context.Context, rc io.Reader, chunks chan<- adapter.StreamChunk, errCh chan<- error) {
	br := bufio.NewReaderSize(rc, 64*1024)
	for {
		if ctx.Err() != nil {
			return
		}
		line, rerr := br.ReadString('\n')
		if rerr != nil && rerr != io.EOF {
			tryDeliverErr(ctx, errCh, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: read stream: " + rerr.Error()})
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload == "[DONE]" {
				return // healthy completion; close(chunks) runs via defer
			}
			var chunk adapter.StreamChunk
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				select {
				case chunks <- chunk:
				case <-ctx.Done():
					return
				}
			}
			// Unparseable frames are dropped (upstream noise), matching the
			// reference implementations' skip-on-error behavior.
		}
		if rerr == io.EOF {
			// EOF without [DONE]: truncated, not healthy — deliver the
			// failure so the router does not mistake silence for completion.
			tryDeliverErr(ctx, errCh, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: upstream stream ended without [DONE]"})
			return
		}
	}
}

// tryDeliverErr delivers the single in-band stream error without blocking
// forever when the router already walked away.
func tryDeliverErr(ctx context.Context, errCh chan<- error, ue error) {
	select {
	case errCh <- ue:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
}

// credentialFor resolves the credential, folding missing/expired credentials
// into the canonical auth failure the router's retry loop understands.
func (s *senderImpl) credentialFor(ctx context.Context, acct *pool.Account) (*Credential, error) {
	if acct == nil {
		return nil, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "workbuddy: no account selected"}
	}
	src := s.options().Credentials
	if src == nil {
		return nil, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "workbuddy: no credential source configured"}
	}
	cred, err := src.CredentialFor(ctx, acct)
	if err != nil {
		return nil, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "workbuddy: credential lookup: " + err.Error()}
	}
	if cred == nil || cred.AccessToken == "" {
		// The upstream has an X-No-Authorization placeholder for anonymous
		// calls, but the gateway cannot attribute free-quota spend without a
		// token: treat as expired so the pool rotates to a real account.
		return nil, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "workbuddy: account " + acct.ID + " has no access token"}
	}
	if !cred.ExpiresAt.IsZero() && !s.now().Before(cred.ExpiresAt) {
		return nil, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "workbuddy: account " + acct.ID + " access token expired"}
	}
	return cred, nil
}

// postStream sends the prepared body and returns the raw SSE reader on
// success. HTTP >= 400 is classified into *adapter.UpstreamError; nothing
// was produced, so the send failed outright.
func (s *senderImpl) postStream(ctx context.Context, cred *Credential, body []byte) (io.ReadCloser, error) {
	url := strings.TrimRight(s.baseFor(cred), "/") + wireChatPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setCommonHeaders(req, cred)
	setAccountHeaders(req, cred)
	setChatIdentityHeaders(req)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &adapter.UpstreamError{Status: http.StatusGatewayTimeout, Reason: adapter.ReasonTimeout, Message: "workbuddy: " + ctx.Err().Error()}
		}
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: transport: " + err.Error()}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if rerr != nil {
			return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: read error body: " + rerr.Error()}
		}
		return nil, classifyError(resp.StatusCode, string(raw), resp.Header)
	}
	// 200 with a JSON error envelope: the upstream wraps rate limits and
	// session death inside HTTP 200 (report §6 headnote; the reference doJSON
	// treats code!=0 as an error regardless of status). Discriminate by the
	// first byte — SSE frames start with "data:" or ":", JSON bodies with
	// "{" — so error envelopes are read and classified without ever blocking
	// a live SSE stream on a full-body read.
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	first, ok := peekNonSpace(br)
	if !ok {
		resp.Body.Close()
		// Empty body on 200: nothing usable — surface as an empty stream
		// (aggregate/relay will report it) rather than inventing success.
		return io.NopCloser(strings.NewReader("")), nil
	}
	if first == '{' {
		// The peek consumed the brace; re-attach it so businessCode sees the
		// full JSON object.
		head := []byte{first}
		raw, rerr := io.ReadAll(io.LimitReader(br, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: read body: " + rerr.Error()}
		}
		full := append(head, raw...)
		if code, ok := businessCode(string(full)); ok && code != "" && code != "0" {
			return nil, classifyError(resp.StatusCode, string(full), resp.Header)
		}
		// code=0 JSON on the chat path: no SSE frames follow; hand the body
		// through so the consumers surface the empty-stream failure honestly.
		return io.NopCloser(bytes.NewReader(full)), nil
	}
	// SSE stream: the peeked byte belongs back on the reader. It is still the
	// last byte read, so a single UnreadByte rewinds it.
	if err := br.UnreadByte(); err != nil {
		resp.Body.Close()
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: rewind body: " + err.Error()}
	}
	return io.NopCloser(struct {
		io.Reader
		io.Closer
	}{br, resp.Body}), nil
}

// peekNonSpace reads bytes until a non-whitespace byte is found. The byte is
// left as "last read" (exactly one UnreadByte rewinds it); ok=false on EOF.
func peekNonSpace(br *bufio.Reader) (b byte, ok bool) {
	for {
		c, err := br.ReadByte()
		if err != nil {
			return 0, false
		}
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return c, true
		}
	}
}

// ---------- error classification ----------

// businessCode extracts the envelope code as a string, tolerating the numeric
// and string JSON forms and walking one level into an "error" object (the
// reference classifiers do the same walk).
func businessCode(body string) (string, bool) {
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return "", false
	}
	nodes := []map[string]any{root}
	if inner, ok := root["error"].(map[string]any); ok {
		nodes = append(nodes, inner)
	}
	for _, node := range nodes {
		for _, key := range []string{"code", "errCode", "error_code"} {
			if v, ok := node[key]; ok && v != nil {
				return strings.TrimSpace(fmt.Sprint(v)), true
			}
		}
	}
	return "", false
}

// hasBusinessEnvelope reports whether the body carries the {code,msg} JSON
// shape; a 403 without it is the APISIX WAF page, not a business reply.
func hasBusinessEnvelope(body string) bool {
	return strings.Contains(body, `"code":`) || strings.Contains(body, `"msg":`)
}

// envelopeMessage picks the human-readable message out of an error body:
// the {msg} envelope field, else {message}, else the raw body.
func envelopeMessage(body string) string {
	var env map[string]any
	if json.Unmarshal([]byte(body), &env) == nil {
		for _, key := range []string{"msg", "message"} {
			if s, ok := env[key].(string); ok && s != "" {
				return s
			}
		}
		if inner, ok := env["error"].(map[string]any); ok {
			for _, key := range []string{"msg", "message"} {
				if s, ok := inner[key].(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return body
}

// classifyError maps an upstream failure onto the canonical FailureReason
// vocabulary. Order is semantic: specific business codes are checked before
// the generic status buckets, because the upstream wraps rate limits inside
// 200/400 bodies (report §6 headnote) — a status-only switch would misfile
// them and the pool would cool the wrong dimension.
func classifyError(status int, body string, header http.Header) error {
	msg := truncate(envelopeMessage(body), 200)
	lower := strings.ToLower(body)
	code, hasCode := businessCode(body)

	// Session death: 401, or the 12153 marker in any status.
	if status == http.StatusUnauthorized || strings.Contains(body, "12153") || strings.Contains(body, "Offline user session not found") {
		return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonAuthExpired, Message: msg}
	}
	// Quota exhaustion: 402/412, the usage-limit codes, or the balance wording.
	if status == http.StatusPaymentRequired || status == 412 ||
		codeIn(code, "14001", "14012", "14013", "14014", "14018") ||
		containsAny(lower, "insufficient credit", "quota exceeded", "积分不足", "额度不足", "余额不足") {
		return &adapter.UpstreamError{Status: pickStatus(status, http.StatusPaymentRequired), Reason: adapter.ReasonQuotaExhausted, Message: msg}
	}
	// Per-(account,model) daily quota: 6004/6008 (report §6). Canonical
	// vocabulary has one rate-limited reason; the message carries the detail.
	if hasCode && (code == "6004" || code == "6008") {
		return rateLimitedError(status, msg, header, body)
	}
	// Account-level rate limiting: HTTP 429, the 6xxx family (minus the daily
	// pair), or rate-limit wording inside non-429 statuses (report §6: the
	// upstream embeds limits in 200/400 bodies).
	if status == http.StatusTooManyRequests ||
		(hasCode && len(code) == 4 && strings.HasPrefix(code, "600") && code != "6004" && code != "6008") ||
		containsAny(lower, "rate limit", "rate-limiting", "rate-limited", "too many requests", "usage limit", "请求过于频繁", "限流") {
		return rateLimitedError(status, msg, header, body)
	}
	// Account-level risk-control restriction: 11140 "request illegal" on
	// 403/429 (report §6 account_fault row). Ground truth 2026-10-21: the
	// official client surfaces this as 「账号存在异常行为，已被限制使用」 with an
	// appeal path — re-login does NOT lift it, so the old auth_expired
	// disposition (re-exchange + rotate) just hammered a restricted account
	// on every retry. account_restricted parks it in a long pool cooldown
	// instead; only an appeal or upstream expiry ends it.
	if (status == http.StatusForbidden || status == http.StatusTooManyRequests) &&
		(hasCode && code == "11140" || containsAny(lower, "request illegal", "auth_forbidden")) {
		return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonAccountRestricted, Message: msg}
	}
	// WAF: 403 with no business envelope (HTML block page / empty body).
	if status == http.StatusForbidden && !hasBusinessEnvelope(body) {
		return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: upstream WAF block (403 without business envelope)"}
	}
	// Request-scoped defects on 400/404/413: bad params (11101), prompt too
	// long (11115), invalid image (11135), tool pairing (11148), reasoning
	// effort (11150), model not servable (11102) — none are the account's
	// fault; invalid-request keeps clients fixing the payload instead of the
	// pool rotating accounts.
	if status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge {
		if (hasCode && codeIn(code, "11101", "11102", "11115", "11135", "11148", "11150")) ||
			containsAny(lower,
				"unmarshal chat params failed", "service info not found",
				"prompt is too long", "invalid image", "invalid_reasoning_effort") {
			return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonNotFound, Message: msg}
		}
		if containsAny(lower, "blocked by security policy", "unapproved channel", "illegal api invocation", "11-128") {
			// Content-policy false positives are account-independent; the
			// canonical vocabulary has no dedicated reason, so platform_down
			// keeps the account healthy while the message carries the verdict.
			return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonPlatformDown, Message: msg}
		}
	}
	if status >= 500 || status == http.StatusRequestTimeout || status == 425 {
		return &adapter.UpstreamError{Status: pickStatus(status, http.StatusBadGateway), Reason: adapter.ReasonPlatformDown, Message: msg}
	}
	if status == http.StatusNotFound {
		// Generic 404 without a business code: the endpoint/model does not
		// exist here — invalid_request, not an account problem.
		return &adapter.UpstreamError{Status: status, Reason: adapter.ReasonNotFound, Message: msg}
	}
	return &adapter.UpstreamError{Status: pickStatus(status, http.StatusBadGateway), Reason: adapter.ReasonPlatformDown, Message: msg}
}

// rateLimitedError builds the canonical rate-limit failure with the upstream
// retry hint folded into the message.
func rateLimitedError(status int, msg string, header http.Header, body string) error {
	full := msg
	if d, ok := parseResetAfter(header); ok {
		full += " (retry after " + d.String() + ")"
	} else if reset := bodyResetWording(body); reset != "" {
		full += " (resets " + reset + ")"
	}
	return &adapter.UpstreamError{Status: pickStatus(status, http.StatusTooManyRequests), Reason: adapter.ReasonRateLimited, Message: full}
}

// pickStatus keeps the upstream status when it is a usable 4xx/5xx, else the
// canonical fallback for the reason.
func pickStatus(status, fallback int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return fallback
}

func codeIn(code string, want ...string) bool {
	for _, w := range want {
		if code == w {
			return true
		}
	}
	return false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// parseResetAfter reads the machine-readable retry hints (report §6):
// Retry-After (integer seconds), retry-after-ms (milliseconds) and
// x-ratelimit-reset (epoch seconds or milliseconds). Non-numeric (HTTP-date)
// values are ignored on purpose, matching the reference implementations.
func parseResetAfter(h http.Header) (time.Duration, bool) {
	const maxHorizon = 2 * time.Hour
	if v := strings.TrimSpace(h.Get("Retry-After")); isDigits(v) {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && time.Duration(n)*time.Second <= maxHorizon {
			return time.Duration(n) * time.Second, true
		}
	}
	if v := strings.TrimSpace(h.Get("Retry-After-Ms")); isDigits(v) {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && time.Duration(n)*time.Millisecond <= maxHorizon {
			return time.Duration(n) * time.Millisecond, true
		}
	}
	if v := strings.TrimSpace(h.Get("X-Ratelimit-Reset")); isDigits(v) {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			var at time.Time
			switch {
			case len(v) >= 12: // millisecond epoch
				at = time.UnixMilli(n)
			case len(v) >= 9: // second epoch
				at = time.Unix(n, 0)
			default:
				return 0, false
			}
			d := time.Until(at)
			if d > 0 && d <= maxHorizon {
				return d, true
			}
		}
	}
	return 0, false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// bodyResetWording surfaces the raw reset clock embedded in 6004-family
// bodies ("将在 <UTC+8 时间> 重置") without converting it through a second
// clock — the wording is quoted verbatim.
func bodyResetWording(body string) string {
	i := strings.Index(body, "将在")
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := strings.Index(rest, "重置"); j > 0 {
		return rest[:j+len("重置")]
	}
	return ""
}

// ---------- request body preparation ----------

// buildUpstreamBody converts the gateway ChatRequest into the upstream body:
// OpenAI-shaped with the mandatory rewrites (report §4). stream is forced
// true — the upstream rejects non-streaming requests.
func buildUpstreamBody(req adapter.ChatRequest) ([]byte, error) {
	obj := map[string]any{
		"model":    req.Model,
		"messages": convertMessages(req.Messages),
		"stream":   true,
	}
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		obj["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		obj["max_tokens"] = *req.MaxTokens
	}
	if len(req.Stop) > 0 && string(req.Stop) != "null" {
		obj["stop"] = req.Stop
	}
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		obj["tools"] = req.Tools
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		if v, ok := normalizeToolChoice(req.ToolChoice); ok {
			obj["tool_choice"] = v
		} else {
			// "none" (or an unparseable shape) drops tool_choice; the sources
			// also drop tools there to avoid a dangling tools array.
			delete(obj, "tools")
		}
	}
	if req.User != "" {
		obj["user"] = req.User
	}
	// Reasoning effort → the upstream thinking switch (protocol §7): the
	// flat reasoning_effort field 400s on "off", so "off" maps to
	// thinking.disabled and every other non-empty value rides the flat
	// field verbatim (deepseek 系据此必吐 delta.reasoning_content).
	if effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort)); effort != "" {
		if effort == "off" || effort == "none" {
			obj["thinking"] = map[string]any{"type": "disabled"}
		} else {
			obj["reasoning_effort"] = req.ReasoningEffort
		}
	}
	// stream_options: the official CLI always sends include_usage so the
	// final frame carries usage (report §4 item 2).
	obj["stream_options"] = map[string]any{"include_usage": true}
	return json.Marshal(obj)
}

// convertMessages rewrites each message: developer→system rename and
// image_url objectification.
func convertMessages(msgs []adapter.ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		entry := map[string]any{"role": m.Role}
		if strings.EqualFold(strings.TrimSpace(m.Role), "developer") {
			entry["role"] = "system"
		}
		if len(m.Content) > 0 && string(m.Content) != "null" {
			entry["content"] = normalizeContent(m.Content)
		}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 && string(m.ToolCalls) != "null" {
			entry["tool_calls"] = m.ToolCalls
		}
		out = append(out, entry)
	}
	return out
}

// normalizeContent passes strings through and objectifies image_url parts in
// content arrays: the upstream only accepts {"url": ...}; the bare-string
// form answers 400 code=11101 (report §4 item 4).
func normalizeContent(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []any
	if json.Unmarshal(raw, &parts) != nil {
		return raw
	}
	for i, p := range parts {
		part, ok := p.(map[string]any)
		if !ok || part["type"] != "image_url" {
			continue
		}
		if v, ok := part["image_url"].(string); ok && v != "" {
			part["image_url"] = map[string]any{"url": v}
			parts[i] = part
		}
	}
	return parts
}

// normalizeToolChoice stringifies OpenAI tool_choice per the upstream's
// string-typed field (report §4 item 3):
//
//	"auto"/"required" (string or {"type":...}) → that string
//	{"type":"function","function":{"name":x}}  → x
//	"none" / {"type":"none"} / unknown shapes  → ("", false): drop the field
func normalizeToolChoice(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch v := strings.ToLower(strings.TrimSpace(s)); v {
		case "auto", "required":
			return v, true
		default: // "none" and unknown strings: drop
			return "", false
		}
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return "", false
	}
	switch v := strings.ToLower(strings.TrimSpace(stringOf(obj["type"]))); v {
	case "auto", "required":
		return v, true
	case "function":
		if fn, ok := obj["function"].(map[string]any); ok {
			if name := strings.TrimSpace(stringOf(fn["name"])); name != "" {
				return name, true
			}
		}
		if name := strings.TrimSpace(stringOf(obj["name"])); name != "" {
			return name, true
		}
		return "auto", true
	default:
		return "", false
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// ---------- SSE aggregation ----------

// rawChunk is a tolerant mirror of the upstream chunk frame: it keeps the
// OpenAI fields plus the provider-extension reasoning_content on delta.
type rawChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *adapter.Usage `json:"usage"`
}

// aggregated folds the stream state into the non-streaming response.
type aggregated struct {
	id, model string
	created   int64
	role      string
	content   strings.Builder
	reasoning strings.Builder // provider-extension chain-of-thought
	finish    string
	usage     *adapter.Usage
	toolCalls map[int]*toolCall
	toolOrder []int
	sawAny    bool
}

type toolCall struct {
	id   string
	name string
	args strings.Builder
}

// scanChunk applies one parsed chunk to the accumulator.
func (a *aggregated) scanChunk(chunk *rawChunk) {
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
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}
	for i := range chunk.Choices {
		c := &chunk.Choices[i]
		if c.FinishReason != nil && *c.FinishReason != "" {
			a.finish = *c.FinishReason
		}
		a.scanDelta(c.Delta.Role, c.Delta.Content, c.Delta.ToolCalls, c.Delta.ReasoningContent)
		// Some upstream frames carry a complete message instead of a delta;
		// fold it the same way (reference implementations do both). An empty
		// message frame contributes nothing, so double-scanning is inert.
		a.scanDelta(c.Message.Role, c.Message.Content, c.Message.ToolCalls, c.Message.ReasoningContent)
	}
}

// scanDelta folds one delta-or-message slice into the accumulator,
// reasoning_content included — thinking models ship their chain-of-thought
// here and the panel's clients expect it relayed (ZCode providers render it
// as the thinking trace).
func (a *aggregated) scanDelta(role string, content json.RawMessage, toolCalls json.RawMessage, reasoning string) {
	if role != "" {
		a.role = role
	}
	if len(content) > 0 {
		var txt string
		if json.Unmarshal(content, &txt) == nil {
			a.content.WriteString(txt)
		}
	}
	if len(toolCalls) > 0 {
		a.mergeToolCalls(toolCalls)
	}
	if reasoning != "" {
		a.reasoning.WriteString(reasoning)
	}
}

// mergeToolCalls accumulates streamed tool_call fragments: the first fragment
// carries id/name, later ones append arguments text; fragments lacking index
// continue the last seen call (single-call streams).
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
			call = &toolCall{}
			a.toolCalls[idx] = call
			a.toolOrder = append(a.toolOrder, idx)
		}
		if cid := stringOf(fr["id"]); cid != "" {
			call.id = cid
		}
		if fn, ok := fr["function"].(map[string]any); ok {
			if name := stringOf(fn["name"]); name != "" {
				call.name = name
			}
			call.args.WriteString(stringOf(fn["arguments"]))
		}
	}
}

// response assembles the final chat.completion object.
//
// reasoning_content note: the upstream ships chain-of-thought as the
// provider-extension delta.reasoning_content (report §5). The shared
// ChatMessage now carries the extension, so the aggregated response relays
// the accumulated reasoning verbatim; strict-OpenAI clients ignore the
// unknown field, agent providers (ZCode et al) render it as the thinking
// trace. The streaming path passes it through the same field on deltas.
func (a *aggregated) response() *adapter.ChatResponse {
	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	msg := adapter.ChatMessage{Role: "assistant"}
	msg.Content = json.RawMessage(marshalString(a.content.String()))
	if a.reasoning.Len() > 0 {
		msg.ReasoningContent = a.reasoning.String()
	}
	if len(a.toolOrder) > 0 {
		calls := make([]map[string]any, 0, len(a.toolOrder))
		for _, idx := range a.toolOrder {
			call := a.toolCalls[idx]
			calls = append(calls, map[string]any{
				"index": idx,
				"id":    call.id,
				"type":  "function",
				"function": map[string]any{
					"name":      call.name,
					"arguments": call.args.String(),
				},
			})
		}
		msg.ToolCalls = mustMarshal(calls)
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
		Usage:   a.usage,
	}
}

// aggregateStream reads a full SSE stream and folds the deltas into one
// non-streaming ChatResponse (the SendChat path).
func aggregateStream(r io.Reader) (*adapter.ChatResponse, error) {
	acc := &aggregated{toolCalls: map[int]*toolCall{}}
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload == "[DONE]" {
				break
			}
			var chunk rawChunk
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				acc.scanChunk(&chunk)
			}
			// Unparseable frames are skipped (upstream noise), matching the
			// reference implementations.
		}
		if err == io.EOF {
			break
		}
	}
	if !acc.sawAny {
		return nil, errors.New("upstream stream contained no valid data events")
	}
	return acc.response(), nil
}

func marshalString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
