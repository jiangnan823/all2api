//go:build qoder || full || (!qoder && !trae && !workbuddy)

// client.go implements adapter.Sender for the Qoder platform.
//
// Wire facts (ported from the verified reference implementation vhub/qauth +
// vhub/qbridge, 2026-10 state):
//   - Credential: a static Personal Access Token ("pt-…" PAT). No refresh
//     semantics, no expiry — renewal is a re-exchange of the same PAT.
//   - Exchange chain: POST {openapi}/api/v1/jobToken/exchange with plain JSON
//     {"personal_token": …} is the only path that still accepts PATs (the
//     legacy COSY gateway exchange answers 401 "personal token is invalid"
//     for every PAT since the 2026-10 client update); the legacy
//     machine-signed POST {auth}/algo/api/v3/user/jobToken?Encode=1 remains
//     the fallback. The REST result is enriched from GET /api/v1/userinfo
//     (Bearer jobToken) because the REST exchange carries no user id.
//   - Region: every PAT resolves to the CN deployment — auth/chat on
//     gateway.qoder.com.cn, OpenAPI on openapi.qoder.sh (the CN OpenAPI host
//     does not serve the exchange route, so the international host is used
//     even for CN accounts).
//   - Chat: POST {auth}/algo/api/v2/service/pro/sse/
//     agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&
//     Encode=1, body = Encode(JSON), stream always true upstream (non-stream
//     client requests are aggregated locally).
//   - SSE frames: each data line carries {"body":"<inner JSON string>",
//     "statusCode","statusCodeValue"}; the inner JSON is OpenAI-shaped
//     (choices[].delta / finish_reason / usage). Termination is a [DONE]
//     body inside the envelope (or a bare data:[DONE]), or "event:finish"
//     (MiniMax-style); "event:error" and error envelopes signal failure.
//   - Error semantics: HTTP 401/403 with business code 10605 is the
//     queue/admission-control rejection (rate-limited class, retry after
//     retryAfterSeconds); 401/403 otherwise is auth failure; in-stream
//     statusCodeValue >= 400 mirrors the same classification mid-stream; a
//     stream ending without [DONE] is a truncation, never a success.
//   - Admission: the gateway rejects parallel requests per account with code
//     10605, so the sender holds one upstream slot per account and queues
//     further sends locally.
//
// The client is stdlib-only. Base URLs and the credential source are
// injectable so tests run against httptest stubs only — the test suite never
// contacts the real endpoints, and the PAT never appears in logs or errors.
package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/pool"
)

// Production endpoint bases (ResolveRegion pins every PAT to the CN
// deployment; both hosts are bypassed in tests via ClientOptions).
const (
	prodAuthBase    = "https://gateway.qoder.com.cn"
	prodOpenAPIBase = "https://openapi.qoder.sh"
	// prodOpenAPICNBase serves the desktop app's device-token exchange
	// (/api/v1/me/jobToken); the .sh host rejects that route with 401.
	prodOpenAPICNBase = "https://openapi.qoder.com.cn"
)

// Wire paths. Auth-family paths carry the machine signature; the REST paths
// carry plain JSON or a Bearer jobToken.
const (
	pathChat           = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	pathJobToken       = "/algo/api/v3/user/jobToken?Encode=1" // #nosec G101 -- endpoint path, not credential material
	pathUserStatus     = "/algo/api/v3/user/status?Encode=1"
	pathRESTExchange   = "/api/v1/jobToken/exchange"
	pathDeviceJobToken = "/api/v1/me/jobToken" // #nosec G101 -- endpoint path, not credential material
	pathRESTUserinfo   = "/api/v1/userinfo"
	pathQuotaUsage     = "/api/v2/quota/usage"
)

// Session renewal bounds, mirroring the reference bridge: renew 2h before the
// reported expiry; when no expiry was reported, trust the session for 30 min.
const (
	refreshMarginMs  = int64(2 * 3600 * 1000)
	unknownExpiryTTL = 30 * time.Minute
	defaultMaxTokens = 32000
)

// errBodyCap bounds how much of an upstream error response is read for
// classification (a truncated body must stay parseable for the 10605 check).
const errBodyCap = 4 << 10

// Region bundles the resolved endpoint bases for one PAT.
type Region struct {
	Name        string
	AuthBase    string
	ChatBase    string
	OpenAPIBase string
}

// ResolveRegion maps a PAT onto its endpoint region. The reference resolver
// currently pins every PAT to the CN deployment (auth+chat on
// gateway.qoder.com.cn, OpenAPI on openapi.qoder.sh).
func ResolveRegion(_ string) *Region {
	return &Region{Name: "cn", AuthBase: prodAuthBase, ChatBase: prodAuthBase, OpenAPIBase: prodOpenAPIBase}
}

// PATCredential is the account credential snapshot one send needs: the static
// personal access token, or the desktop app's device-login credential.
type PATCredential struct {
	PAT string
	// DeviceToken/ClientID carry the Qoder desktop app's device-login
	// credential (dt- token + clientId). When DeviceToken is set the PAT
	// field stays empty and the exchange goes through the official
	// /api/v1/me/jobToken endpoint — PAT-exchanged sessions are rejected by
	// the chat gateway with 105 "Login expired" for app-login accounts.
	DeviceToken string
	ClientID    string
	// RefreshToken is the drt-… companion of a device credential (persisted
	// next to the device token once an exchange or rotation produced one).
	// It is the keepalive renewal input: the legacy jobToken needRefresh
	// mode trades it for a fresh pair. Empty = never captured.
	RefreshToken string
}

// DeviceRotation is one renewed device credential pair. Empty fields mean
// "unchanged" — the caller persists only what came back. A rotation that
// answers with a session-shaped token (jt-…) rather than a new device token
// leaves DeviceToken empty: session tokens must never be persisted as the
// long-lived device credential.
type DeviceRotation struct {
	DeviceToken  string
	RefreshToken string
	ExpiresAt    time.Time // zero = not reported
}

// CredentialSource supplies the credential for one pool account. An error (or
// a nil/empty credential) means "no usable credential" — the send fails with
// ReasonAuthExpired and the router rotates to the next account.
type CredentialSource interface {
	CredentialFor(ctx context.Context, acct *pool.Account) (*PATCredential, error)
}

// ClientOptions configures one sender. The zero value is production-shaped:
// realm-default bases, no credential source, stdlib transport.
type ClientOptions struct {
	// AuthBaseURL overrides the COSY gateway base (auth + chat). Tests point
	// this at an httptest server.
	AuthBaseURL string
	// OpenAPIBaseURL overrides the OpenAPI base (exchange, userinfo, quota).
	OpenAPIBaseURL string
	// HTTPClient overrides the transport. Nil uses a shared default client.
	HTTPClient *http.Client
	// Credentials resolves per-account credentials. Production wiring must
	// set it; nil makes every send fail with ReasonAuthExpired.
	Credentials CredentialSource
	// Now overrides the clock for session-expiry checks (tests).
	Now func() time.Time
}

// busyFallbackBackoff is used when a 10605 rejection carries no usable
// retryAfterSeconds (or an implausible one). Package var so tests can
// shorten the wait.
var busyFallbackBackoff = 2 * time.Second

// senderImpl carries the adapter.Sender implementation; the Adapter embeds it
// so the router's adapter.Sender type assertion finds the methods.
type senderImpl struct {
	mu   sync.Mutex
	opts ClientOptions
	http *http.Client

	statesMu sync.Mutex
	states   map[string]*acctState
}

// acctState is the per-account session cache: one machine identity, one live
// COSY session and one upstream admission slot (the gateway rejects parallel
// sends per account with code 10605, so further sends queue locally).
type acctState struct {
	machineID, machineToken, machineType string
	clientID                             string // device-login clientId (dt- credential only)

	refreshMu sync.Mutex // serializes renewal network calls
	cur       atomic.Pointer[sessionData]
	slots     chan struct{}
	credToken string // last resolved credential string (dt- or pt-); catalog fetch reuses it
}

// sessionData is the immutable snapshot swapped in on every (re)bootstrap.
type sessionData struct {
	sess          *session
	refreshToken  string
	expireAtMs    int64
	refreshedAtMs int64
}

// Configure sets the client options (integrator wiring). Replaces options
// wholesale and drops cached sessions — the next send re-bootstraps.
func (s *senderImpl) Configure(opts ClientOptions) {
	s.mu.Lock()
	s.opts = opts
	s.http = nil
	s.mu.Unlock()
	s.statesMu.Lock()
	s.states = nil
	s.statesMu.Unlock()
}

func (s *senderImpl) options() ClientOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts
}

func (s *senderImpl) authBase() string {
	if base := s.options().AuthBaseURL; base != "" {
		return base
	}
	return prodAuthBase
}

func (s *senderImpl) openapiBase() string {
	if base := s.options().OpenAPIBaseURL; base != "" {
		return base
	}
	return prodOpenAPIBase
}

// httpClient returns the chat client. No global timeout: SSE streams run
// arbitrarily long; ctx is the cancellation surface.
func (s *senderImpl) httpClient() *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http != nil {
		return s.http
	}
	if s.opts.HTTPClient != nil {
		s.http = s.opts.HTTPClient
	} else {
		s.http = &http.Client{}
	}
	return s.http
}

// shortClient bounds the short auth/OpenAPI calls (exchange, userinfo,
// status, quota) the way the reference's 15s clients do.
func (s *senderImpl) shortClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

func (s *senderImpl) nowMs() int64 {
	if fn := s.options().Now; fn != nil {
		return fn().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// state lazily creates the per-account state.
func (s *senderImpl) state(accountID string) *acctState {
	s.statesMu.Lock()
	defer s.statesMu.Unlock()
	if s.states == nil {
		s.states = make(map[string]*acctState)
	}
	if st, ok := s.states[accountID]; ok {
		return st
	}
	// Placeholder identity derived from the account id; patFor re-derives
	// from the resolved credential so the fingerprint tracks the credential
	// (stable across restarts), never the process.
	machineID, machineToken, machineType := deriveMachineIdentity("account:" + accountID)
	st := &acctState{
		machineID:    machineID,
		machineToken: machineToken,
		machineType:  machineType,
		slots:        make(chan struct{}, 1),
	}
	s.states[accountID] = st
	return st
}

// patFor resolves the credential, folding missing credentials into the
// canonical auth failure the router's retry loop understands.
func (s *senderImpl) patFor(ctx context.Context, acct *pool.Account) (string, error) {
	if acct == nil {
		return "", &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "qoder: no account selected"}
	}
	src := s.options().Credentials
	if src == nil {
		return "", &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "qoder: no credential source configured"}
	}
	cred, err := src.CredentialFor(ctx, acct)
	if err != nil {
		return "", &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "qoder: credential lookup: " + err.Error()}
	}
	if cred == nil {
		return "", &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "qoder: account " + acct.ID + " has no credential"}
	}
	st := s.state(acct.ID)
	st.clientID = cred.ClientID
	if cred.DeviceToken != "" {
		st.credToken = cred.DeviceToken
		// The machine fingerprint tracks the credential: stable across
		// restarts for the same credential, distinct per credential.
		st.machineID, st.machineToken, st.machineType = deriveMachineIdentity("cred:" + cred.DeviceToken)
		return cred.DeviceToken, nil
	}
	st.credToken = cred.PAT
	if cred.PAT == "" {
		return "", &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "qoder: account " + acct.ID + " has no PAT"}
	}
	st.machineID, st.machineToken, st.machineType = deriveMachineIdentity("cred:" + cred.PAT)
	return cred.PAT, nil
}

// --- Credential exchange chain ---

// jobToken is the normalized exchange result either path produces.
type jobToken struct {
	Token        string // jt-… jobToken; securityOauthToken for OpenAPI calls
	RefreshToken string // jrt-… on the REST path
	UID          string
	Name         string
	OrgID        string
	OrgName      string
	ExpireAtMs   int64 // 0 = not reported (unknownExpiryTTL applies)
}

// exchangePAT runs the credential exchange chain: REST first (the only
// PAT-accepting entry since 2026-10), legacy COSY gateway exchange as the
// fallback the reference keeps for older sessions. The account state supplies
// the machine identity the legacy signature family presents.
func (s *senderImpl) exchangePAT(ctx context.Context, st *acctState, pat string) (*jobToken, error) {
	// dt- = the desktop app's device-login credential: the chat gateway only
	// accepts sessions from the official /api/v1/me/jobToken exchange for it.
	if strings.HasPrefix(pat, "dt-") {
		return s.exchangeDevice(ctx, st, pat, st.clientID)
	}
	jt, err := s.exchangeREST(ctx, pat)
	if err == nil {
		return jt, nil
	}
	fmt.Printf("[qoder] REST PAT exchange failed (%v); falling back to legacy gateway exchange\n", err)
	return s.exchangeLegacy(ctx, st, pat)
}

// exchangeREST performs POST /api/v1/jobToken/exchange and the best-effort
// userinfo enrichment. The PAT never travels anywhere but the request body
// and is never echoed into an error message.
func (s *senderImpl) exchangeREST(ctx context.Context, pat string) (*jobToken, error) {
	payload, _ := marshalNoEscape(map[string]string{"personal_token": pat})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.openapiBase()+pathRESTExchange, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.shortClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &adapter.UpstreamError{Status: http.StatusGatewayTimeout, Reason: adapter.ReasonTimeout, Message: "qoder: exchange: " + ctx.Err().Error()}
		}
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "qoder: exchange transport: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyCap))
	drainBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// 401/403 on the exchange means the PAT itself is dead: no body
			// echo (it may carry identity data), status says everything.
			return nil, &adapter.UpstreamError{Status: resp.StatusCode, Reason: adapter.ReasonAuthExpired,
				Message: fmt.Sprintf("qoder: PAT rejected by exchange (HTTP %d)", resp.StatusCode)}
		}
		return nil, classifyUpstream(resp.StatusCode, raw, resp.Header)
	}
	var er struct {
		Token        string `json:"token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &er); err != nil || er.Token == "" {
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown,
			Message: "qoder: jobToken/exchange response shape unexpected"}
	}
	jt := &jobToken{Token: er.Token, RefreshToken: er.RefreshToken}
	if er.ExpiresIn > 0 {
		jt.ExpireAtMs = s.nowMs() + er.ExpiresIn
	} else if t, perr := time.Parse(time.RFC3339, er.ExpiresAt); perr == nil {
		jt.ExpireAtMs = t.UnixMilli()
	}
	s.enrichUserinfo(ctx, s.openapiBase(), jt)
	return jt, nil
}

// exchangeDevice trades the desktop app's device-login token for a chat
// session on POST {openapi}/api/v1/me/jobToken (Bearer dt-, JSON clientId).
// This is the exchange the official client uses; its jt- jobToken is the only
// kind the chat gateway accepts for app-login accounts — the PAT exchange
// planes yield sessions the gateway rejects with 105 "Login expired".
func (s *senderImpl) exchangeDevice(ctx context.Context, st *acctState, deviceToken, clientID string) (*jobToken, error) {
	if clientID == "" {
		return nil, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired,
			Message: "qoder: device credential carries no clientId"}
	}
	body, err := json.Marshal(map[string]string{"clientId": clientID})
	if err != nil {
		return nil, &adapter.UpstreamError{Status: http.StatusBadRequest, Reason: adapter.ReasonNotFound,
			Message: "qoder: build device exchange body: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, prodOpenAPICNBase+pathDeviceJobToken, bytes.NewReader(body))
	if err != nil {
		return nil, &adapter.UpstreamError{Status: http.StatusBadRequest, Reason: adapter.ReasonNotFound,
			Message: "qoder: build device exchange request: " + err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	resp, err := s.shortClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &adapter.UpstreamError{Status: http.StatusGatewayTimeout, Reason: adapter.ReasonTimeout, Message: "qoder: device exchange: " + ctx.Err().Error()}
		}
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown,
			Message: "qoder: device exchange transport: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyCap))
	drainBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, classifyUpstream(resp.StatusCode, raw, resp.Header)
	}
	var er struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"` // milliseconds
		ExpiresAt    string `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &er); err != nil || er.Token == "" {
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown,
			Message: "qoder: device exchange response shape unexpected"}
	}
	jt := &jobToken{Token: er.Token, RefreshToken: er.RefreshToken}
	if er.ExpiresIn > 0 {
		jt.ExpireAtMs = s.nowMs() + er.ExpiresIn
	} else if t, perr := time.Parse(time.RFC3339, er.ExpiresAt); perr == nil {
		jt.ExpireAtMs = t.UnixMilli()
	}
	// com.cn plane: device jt- tokens are only valid here (.sh userinfo
	// rejects them with TOKEN_EXPIRE, which used to leave the session uid
	// empty and the chat gateway rejecting every signed request with 101).
	s.enrichUserinfo(ctx, prodOpenAPICNBase, jt)
	return jt, nil
}

// RefreshDeviceCredential runs one keepalive rotation of the stored device
// pair. The preferred path is the legacy jobToken needRefresh mode — a drt-
// refresh token trades for a fresh device/refresh pair (community research:
// device credentials expire on a ~15-day cycle, and once both halves die the
// account silently fails until a manual re-login). Without a stored refresh
// token — or when the refresh path rejects — it falls back to a plain device
// exchange, whose response's refresh_token bootstraps the stored pair. Only
// the returned pair is new: the caller persists it through the credential
// store; session state here stays untouched.
func (s *senderImpl) RefreshDeviceCredential(ctx context.Context, acct *pool.Account) (DeviceRotation, error) {
	if acct == nil {
		return DeviceRotation{}, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "qoder: no account selected"}
	}
	src := s.options().Credentials
	if src == nil {
		return DeviceRotation{}, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "qoder: no credential source configured"}
	}
	cred, err := src.CredentialFor(ctx, acct)
	if err != nil || cred == nil {
		return DeviceRotation{}, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "qoder: credential lookup failed"}
	}
	if cred.DeviceToken == "" {
		return DeviceRotation{}, &adapter.UpstreamError{Status: http.StatusUnauthorized, Reason: adapter.ReasonAuthExpired, Message: "qoder: account " + acct.ID + " holds no device credential"}
	}
	st := s.state(acct.ID)
	st.clientID = cred.ClientID

	if rt := strings.TrimSpace(cred.RefreshToken); rt != "" {
		rot, rerr := s.refreshDevicePair(ctx, st, rt)
		if rerr == nil {
			return rot, nil
		}
		fmt.Printf("[qoder] device pair refresh rejected (%v); falling back to device exchange\n", rerr)
	}
	jt, err := s.exchangeDevice(ctx, st, cred.DeviceToken, cred.ClientID)
	if err != nil {
		return DeviceRotation{}, err
	}
	var exp time.Time
	if jt.ExpireAtMs > 0 {
		exp = time.UnixMilli(jt.ExpireAtMs)
	}
	return DeviceRotation{RefreshToken: jt.RefreshToken, ExpiresAt: exp}, nil
}

// refreshDevicePair trades a drt- refresh token for a fresh pair through the
// legacy machine-signed jobToken endpoint, needRefresh mode (personalToken
// empty — the drt- token is the sole credential, per the community
// reference's wire shape).
func (s *senderImpl) refreshDevicePair(ctx context.Context, st *acctState, refreshToken string) (DeviceRotation, error) {
	inner, err := marshalNoEscape(struct {
		PersonalToken      string `json:"personalToken"`
		SecurityOauthToken string `json:"securityOauthToken"`
		RefreshToken       string `json:"refreshToken"`
		NeedRefresh        bool   `json:"needRefresh"`
		AuthInfo           string `json:"authInfo"`
	}{RefreshToken: refreshToken, NeedRefresh: true, AuthInfo: "{}"})
	if err != nil {
		return DeviceRotation{}, err
	}
	raw, err := s.postEncoded(ctx, st, pathJobToken, inner)
	if err != nil {
		return DeviceRotation{}, err
	}
	dt, rt, expMs := normalizeDevicePair(raw, refreshToken)
	if dt == "" && rt == refreshToken {
		return DeviceRotation{}, fmt.Errorf("qoder: refresh reply carried no new token (keys: %d)", len(raw))
	}
	rot := DeviceRotation{DeviceToken: dt, RefreshToken: rt}
	if expMs > 0 {
		rot.ExpiresAt = time.UnixMilli(expMs)
	}
	return rot, nil
}

// normalizeDevicePair pulls the rotated pair out of a jobToken reply. Field
// spellings vary across planes and versions (device_token/deviceToken/
// access_token/accessToken, optionally nested under "data"); a session-shaped
// "token" (jt-…) is never taken as the device credential. A missing refresh
// token keeps the old one.
func normalizeDevicePair(raw map[string]any, oldRefresh string) (deviceToken, refreshToken string, expireAtMs int64) {
	pick := func(m map[string]any, keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok {
				if s := strings.TrimSpace(v); s != "" {
					return s
				}
			}
		}
		return ""
	}
	data, _ := raw["data"].(map[string]any)
	dt := pick(raw, "device_token", "deviceToken", "access_token", "accessToken", "token")
	rt := pick(raw, "refresh_token", "refreshToken")
	if data != nil {
		if dt == "" {
			dt = pick(data, "device_token", "deviceToken", "access_token", "accessToken", "token")
		}
		if rt == "" {
			rt = pick(data, "refresh_token", "refreshToken")
		}
		if expireAtMs == 0 {
			if v, ok := data["expireTime"].(float64); ok {
				expireAtMs = int64(v)
			}
		}
	}
	// Only a genuine device token may replace the stored one; anything else
	// (session jt-…) stays out of the long-lived credential.
	if !strings.HasPrefix(dt, "dt-") {
		dt = ""
	}
	if rt == "" {
		rt = oldRefresh
	}
	return dt, rt, expireAtMs
}

// enrichUserinfo fills uid/name/org from GET /api/v1/userinfo (Bearer
// jobToken). Best-effort: the legacy exchange carried these fields, the REST
// one does not, and an empty uid degrades the session (chat still signs,
// /user/status resolution is skipped).
func (s *senderImpl) enrichUserinfo(ctx context.Context, base string, jt *jobToken) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+pathRESTUserinfo, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+jt.Token)
	resp, err := s.shortClient().Do(req)
	if err != nil {
		fmt.Printf("[qoder] WARN userinfo enrichment failed: %v\n", err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyCap))
	drainBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("[qoder] WARN userinfo returned HTTP %d\n", resp.StatusCode)
		return
	}
	var info struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		OrganizationID   string `json:"organization_id"`
		OrganizationName string `json:"organization_name"`
	}
	if json.Unmarshal(raw, &info) != nil {
		return
	}
	jt.UID = info.ID
	jt.Name = info.Name
	jt.OrgID = info.OrganizationID
	jt.OrgName = info.OrganizationName
}

// exchangeLegacy is the COSY gateway jobToken exchange (machine signature,
// Encode-wrapped payload). Kept as the fallback path; field order of the
// inner payload matches the official client.
func (s *senderImpl) exchangeLegacy(ctx context.Context, st *acctState, pat string) (*jobToken, error) {
	inner, err := marshalNoEscape(struct {
		PersonalToken      string `json:"personalToken"`
		SecurityOauthToken string `json:"securityOauthToken"`
		RefreshToken       string `json:"refreshToken"`
		NeedRefresh        bool   `json:"needRefresh"`
		AuthInfo           string `json:"authInfo"`
	}{PersonalToken: pat, AuthInfo: "{}"})
	if err != nil {
		return nil, err
	}
	raw, err := s.postEncoded(ctx, st, pathJobToken, inner)
	if err != nil {
		return nil, err
	}
	return jobTokenFromMap(raw), nil
}

// refreshLegacy renews a legacy (non-REST) session through the gateway.
func (s *senderImpl) refreshLegacy(ctx context.Context, st *acctState, pat, refreshToken string) (*jobToken, error) {
	inner, err := marshalNoEscape(struct {
		PersonalToken      string `json:"personalToken"`
		SecurityOauthToken string `json:"securityOauthToken"`
		RefreshToken       string `json:"refreshToken"`
		NeedRefresh        bool   `json:"needRefresh"`
		AuthInfo           string `json:"authInfo"`
	}{PersonalToken: pat, RefreshToken: refreshToken, NeedRefresh: true, AuthInfo: "{}"})
	if err != nil {
		return nil, err
	}
	raw, err := s.postEncoded(ctx, st, pathJobToken, inner)
	if err != nil {
		return nil, err
	}
	return jobTokenFromMap(raw), nil
}

// jobTokenFromMap normalizes the legacy gateway's map-shaped reply.
func jobTokenFromMap(raw map[string]any) *jobToken {
	jt := &jobToken{
		Token:        stringOf(raw["token"]),
		RefreshToken: stringOf(raw["refreshToken"]),
		UID:          stringOf(raw["id"]),
		Name:         stringOf(raw["name"]),
		OrgID:        stringOf(raw["organizationId"]),
		OrgName:      stringOf(raw["organizationName"]),
		ExpireAtMs:   int64Of(raw["expireTime"]),
	}
	if jt.Token == "" {
		jt.Token = stringOf(raw["securityOauthToken"])
	}
	return jt
}

// postEncoded performs one machine-signed, Encode-wrapped POST on the auth
// endpoint family and decodes the JSON object reply.
func (s *senderImpl) postEncoded(ctx context.Context, st *acctState, pathAndQuery string, innerJSON []byte) (map[string]any, error) {
	outer := struct {
		Payload       string `json:"payload"`
		EncodeVersion string `json:"encodeVersion"`
	}{Payload: string(innerJSON), EncodeVersion: "1"}
	plain, err := marshalNoEscape(outer)
	if err != nil {
		return nil, err
	}
	body, err := customEncode(plain)
	if err != nil {
		return nil, err
	}
	date := httpDate()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.authBase()+pathAndQuery, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = authHeaders(st.machineID, st.machineToken, st.machineType, date, cosySign(date))
	resp, err := s.shortClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &adapter.UpstreamError{Status: http.StatusGatewayTimeout, Reason: adapter.ReasonTimeout, Message: "qoder: auth call: " + ctx.Err().Error()}
		}
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "qoder: auth transport: " + err.Error()}
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyCap))
	drainBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if busy := busyFromBody(resp.StatusCode, detail); busy != nil {
			return nil, busy
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &adapter.UpstreamError{Status: resp.StatusCode, Reason: adapter.ReasonAuthExpired,
				Message: fmt.Sprintf("qoder: auth call rejected (HTTP %d)", resp.StatusCode)}
		}
		return nil, classifyUpstream(resp.StatusCode, detail, resp.Header)
	}
	var result map[string]any
	if err := json.NewDecoder(bytes.NewReader(detail)).Decode(&result); err != nil {
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown,
			Message: "qoder: auth reply decode: " + err.Error()}
	}
	return result, nil
}

// resolveUserType fetches the authoritative subscription tier from
// /user/status (machine-signed, no bearer). Best-effort with a bounded
// deadline: it runs before the session is built because the tier is sealed
// into the bearer payload. "" means "keep the fallback".
func (s *senderImpl) resolveUserType(ctx context.Context, st *acctState, uid string) string {
	if uid == "" {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	inner, err := marshalNoEscape(struct {
		UserID      string `json:"userId"`
		NeedRefresh bool   `json:"needRefresh"`
		AuthInfo    string `json:"authInfo"`
	}{UserID: uid, AuthInfo: "{}"})
	if err != nil {
		return ""
	}
	raw, err := s.postEncoded(lookupCtx, st, pathUserStatus, inner)
	if err != nil {
		fmt.Printf("[qoder] WARN user/status failed (%v); using default tier\n", err)
		return ""
	}
	return stringOf(raw["userType"])
}

// --- Session lifecycle ---

// ensureSession returns a live session, bootstrapping or renewing on demand.
// Fast path reads the atomic snapshot; renewals serialize on refreshMu.
func (s *senderImpl) ensureSession(ctx context.Context, pat string, st *acctState) (*session, error) {
	if d := st.cur.Load(); d != nil && !sessionStale(d, s.nowMs()) {
		return d.sess, nil
	}
	st.refreshMu.Lock()
	defer st.refreshMu.Unlock()
	if d := st.cur.Load(); d != nil && !sessionStale(d, s.nowMs()) {
		return d.sess, nil
	}
	if err := s.bootstrapOrRenew(ctx, pat, st, false); err != nil {
		return nil, err
	}
	d := st.cur.Load()
	if d == nil {
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "qoder: session not bootstrapped"}
	}
	return d.sess, nil
}

// forceRenew re-exchanges the PAT from scratch (reactive 401 path).
func (s *senderImpl) forceRenew(ctx context.Context, pat string, st *acctState) error {
	st.refreshMu.Lock()
	defer st.refreshMu.Unlock()
	return s.bootstrapOrRenew(ctx, pat, st, true)
}

// bootstrapOrRenew runs the renewal policy: jrt-/absent refresh tokens are
// REST-shaped and simply re-exchange; a legacy refresh token renews through
// the gateway first and falls back to a full exchange; force skips straight
// to the full exchange.
func (s *senderImpl) bootstrapOrRenew(ctx context.Context, pat string, st *acctState, force bool) error {
	if !force {
		d := st.cur.Load()
		rt := ""
		if d != nil {
			rt = d.refreshToken
		}
		if rt != "" && !strings.HasPrefix(rt, "jrt-") {
			jt, err := s.refreshLegacy(ctx, st, pat, rt)
			if err == nil {
				s.applyJobToken(ctx, st, jt)
				return nil
			}
			fmt.Printf("[qoder] legacy refresh rejected (%v); falling back to PAT exchange\n", err)
		}
	}
	jt, err := s.exchangePAT(ctx, st, pat)
	if err != nil {
		return err
	}
	s.applyJobToken(ctx, st, jt)
	return nil
}

// applyJobToken builds the signed session from an exchange result. The
// subscription tier is resolved first because it is sealed into the bearer
// payload; the fallback ladder is /user/status → jobToken → personal_standard.
func (s *senderImpl) applyJobToken(ctx context.Context, st *acctState, jt *jobToken) {
	userType := s.resolveUserType(ctx, st, jt.UID)
	if userType == "" {
		userType = "personal_standard"
	}
	sess, err := newSession(authIdentity{
		Name:               jt.Name,
		Aid:                jt.UID,
		UID:                jt.UID,
		OrganizationID:     jt.OrgID,
		OrganizationName:   jt.OrgName,
		UserType:           userType,
		SecurityOauthToken: jt.Token,
		RefreshToken:       jt.RefreshToken,
	}, st.machineID, st.machineToken, st.machineType)
	if err != nil {
		fmt.Printf("[qoder] WARN session build failed (%v); keeping previous session\n", err)
		return
	}
	st.cur.Store(&sessionData{
		sess:          sess,
		refreshToken:  jt.RefreshToken,
		expireAtMs:    jt.ExpireAtMs,
		refreshedAtMs: s.nowMs(),
	})
}

// sessionStale reports whether a session needs renewal: 2h before a reported
// expiry, or 30 min after renewal when none was reported.
func sessionStale(d *sessionData, nowMs int64) bool {
	if d.expireAtMs == 0 {
		return nowMs-d.refreshedAtMs > unknownExpiryTTL.Milliseconds()
	}
	return nowMs > d.expireAtMs-refreshMarginMs
}

// --- Quota probe (startup credit seeding) ---

// QuotaRemaining bootstraps the account session and reads the authoritative
// allowance from GET /api/v2/quota/usage (the official client's /usage view).
// Returns the rounded remaining credits of the user quota pool.
func (s *senderImpl) QuotaRemaining(ctx context.Context, acct *pool.Account) (int64, error) {
	pat, err := s.patFor(ctx, acct)
	if err != nil {
		return 0, err
	}
	st := s.state(acct.ID)
	sess, err := s.ensureSession(ctx, pat, st)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.openapiBase()+pathQuotaUsage, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+sess.securityOauth)
	resp, err := s.shortClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, &adapter.UpstreamError{Status: http.StatusGatewayTimeout, Reason: adapter.ReasonTimeout, Message: "qoder: quota: " + ctx.Err().Error()}
		}
		return 0, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "qoder: quota transport: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	drainBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, classifyUpstream(resp.StatusCode, raw, resp.Header)
	}
	var usage struct {
		UserQuota *struct {
			Remaining float64 `json:"remaining"`
		} `json:"userQuota"`
	}
	if err := json.Unmarshal(raw, &usage); err != nil || usage.UserQuota == nil {
		return 0, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown,
			Message: "qoder: quota usage response shape unexpected"}
	}
	return int64(math.Round(usage.UserQuota.Remaining)), nil
}

// --- Shared helpers ---

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func int64Of(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	if s, ok := v.(string); ok {
		var n int64
		_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
		if err == nil {
			return n
		}
	}
	return 0
}

// drainBody reads and discards the remaining response body (bounded) so the
// connection can return to the pool before Close on error paths.
func drainBody(body io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
}
