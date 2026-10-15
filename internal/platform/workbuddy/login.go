//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// login.go implements the M8 browser device-authorization login flow
// (docs/upstream/workbuddy-protocol.md §7), the login counterpart of the chat
// sender (client.go) and the task client (tasks.go):
//
//	Start  POST {chat}/v2/plugin/auth/state?platform=CLI  body {}
//	       → data {state, authUrl} — the panel hands authUrl to the user's
//	       browser and keeps state as the poll handle.
//	Poll   GET {chat}/v2/plugin/auth/token?state=<state>
//	       → not finished: business code 11217 "login ing" (report §6), or
//	         any other token-endpoint failure (see the Poll note) → pending.
//	       → finished: data {accessToken, refreshToken, expiresIn, domain}.
//	then   GET {chat}/v2/plugin/login/account?state=<state> (Bearer)
//	       → data {uid, enterpriseId, nickname} — folded into the outcome.
//
// Header family: the wb2api login surface's commonHeaders (JSON content,
// XHR marker, realm-brand Origin/Referer, the CLI-shaped UA) — NOT the chat
// header family. The login endpoints predate the desktop fingerprint and the
// CLI shape is what both proven reference implementations ship
// (wb_research cmd__login__main.go.txt / workbuddy2api-panel panel/login.go).
//
// The client is stdlib-only and the endpoint base is injectable, so tests run
// against httptest stubs only — the suite never contacts the real endpoints.
// No cookie jar: every request carries the state; there is no session.
package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Login endpoint paths (report §7). All three live on the chat base. Named
// state/poll/account after their roles — deliberately not "token" (gosec G101
// keys on the identifier, and this is a URL path, not a credential).
const (
	wireLoginStatePath = "/v2/plugin/auth/state?platform=CLI"
	wireLoginPollPath  = "/v2/plugin/auth/token?state="
	wireLoginAcctPath  = "/v2/plugin/login/account?state="
)

// loginClientUA is the CLI-shaped User-Agent the login endpoints expect
// (wb2api cmd/login + panel/login.go — the empirically proven shape).
const loginClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// loginCodePending is the upstream business code for "login ing" (report §6):
// the authoritative not-finished-yet marker — the poll must continue.
const loginCodePending = 11217

// LoginStatus is the poll outcome class.
type LoginStatus string

const (
	LoginPending LoginStatus = "pending" // keep polling
	LoginDone    LoginStatus = "done"    // Credential + Nickname populated
	LoginError   LoginStatus = "error"   // Message carries the reason
)

// LoginOutcome is one Poll round's result. pending carries nothing; done
// carries the full credential (realm stamped, expiry computed) plus the
// account display name; error carries a user-facing message that never
// echoes credential material.
type LoginOutcome struct {
	Status     LoginStatus
	Credential *Credential
	Nickname   string
	Message    string
}

// LoginStartResult is the Start result: the upstream state plus the
// authorization URL the user's browser opens.
type LoginStartResult struct {
	State    string
	LoginURL string
}

// LoginClientOptions configures one login client. Zero value is
// production-shaped: CN realm defaults, a 30s-bounded transport (login calls
// are short JSON round-trips).
type LoginClientOptions struct {
	// BaseURL overrides the chat base for all three endpoints. Tests point
	// this at an httptest server.
	BaseURL string
	// Realm selects the production base + Origin brand ("cn" default,
	// "global" for www.workbuddy.ai). Ignored when BaseURL is set.
	Realm string
	// HTTPClient overrides the transport. Nil uses a 30s-timeout client.
	HTTPClient *http.Client
	// Now overrides the clock for credential-expiry stamps (tests).
	Now func() time.Time
}

// LoginClient performs the device-authorization login. Stateless per call
// (every request carries the state), safe for concurrent use.
type LoginClient struct {
	opts LoginClientOptions
	http *http.Client
}

// NewLoginClient builds a login client with production defaults.
func NewLoginClient(opts LoginClientOptions) *LoginClient {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &LoginClient{opts: opts, http: hc}
}

// base returns the endpoint base (test override, else the realm default).
func (c *LoginClient) base() string {
	if c.opts.BaseURL != "" {
		return c.opts.BaseURL
	}
	if c.realm() == "global" {
		return wireDefaultBaseGlobal
	}
	return wireDefaultBaseCN
}

// origin returns the Origin/Referer brand domain for the realm.
func (c *LoginClient) origin() string {
	if c.realm() == "global" {
		return wireOriginGlobal
	}
	return wireOriginCN
}

func (c *LoginClient) realm() string {
	if c.opts.Realm == "global" {
		return "global"
	}
	return "cn"
}

func (c *LoginClient) now() time.Time {
	if c.opts.Now != nil {
		return c.opts.Now()
	}
	return time.Now()
}

// loginEnvelope is the upstream {code,msg,data} business envelope, decoupled
// from the HTTP status (report §6 headnote).
type loginEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// loginBusinessError marks a non-zero envelope code; Poll inspects the code
// (11217 = keep polling).
type loginBusinessError struct {
	Code int
	Msg  string
}

func (e *loginBusinessError) Error() string {
	return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
}

// loginDo performs one login round-trip with the wb2api login header family.
// Transport failures, HTTP >= 300 and unparseable bodies are plain errors; a
// non-zero envelope code is a *loginBusinessError carrying the code.
func (c *LoginClient) loginDo(ctx context.Context, method, fullURL, bearer string, body []byte) (json.RawMessage, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, payload)
	if err != nil {
		return nil, err
	}
	origin := c.origin()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", loginClientUA)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env loginEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, &loginBusinessError{Code: env.Code, Msg: env.Msg}
	}
	return env.Data, nil
}

// Start creates one device-authorization state (report §7 step 1). The state
// is the poll handle; LoginURL is what the user's browser opens.
func (c *LoginClient) Start(ctx context.Context) (*LoginStartResult, error) {
	url := strings.TrimRight(c.base(), "/") + wireLoginStatePath
	data, err := c.loginDo(ctx, http.MethodPost, url, "", []byte("{}"))
	if err != nil {
		return nil, fmt.Errorf("auth state: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return nil, errors.New("auth state: missing state or authUrl")
	}
	return &LoginStartResult{State: st.State, LoginURL: st.AuthURL}, nil
}

// loginToken is the token-endpoint data payload on a finished login.
type loginToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// Poll runs one poll round (report §7 steps 2–3; exactly one upstream
// attempt per call — the panel frontend re-invokes every few seconds):
//
//   - pending: the login is not finished. Business code 11217 "login ing" is
//     the documented shape; every other token-endpoint failure (transport,
//     HTTP status, unparseable body, other business codes) and a tokenless
//     data payload map here too — the proven reference implementations keep
//     polling on all of them, and no upstream expiry code is documented.
//     Session expiry is therefore the caller's TTL, never ours.
//   - done: token bundle + account fetched; Credential is complete (UID
//     validated, expiry stamped from expiresIn) and Nickname set.
//   - error: the login finished upstream but the account identity could not
//     be resolved (account endpoint failed, uid missing, refreshToken
//     missing, or an illegal uid).
func (c *LoginClient) Poll(ctx context.Context, state string) LoginOutcome {
	tokenURL := strings.TrimRight(c.base(), "/") + wireLoginPollPath + state
	data, err := c.loginDo(ctx, http.MethodGet, tokenURL, "", nil)
	if err != nil {
		return LoginOutcome{Status: LoginPending}
	}
	var tok loginToken
	if uerr := json.Unmarshal(data, &tok); uerr != nil || tok.AccessToken == "" {
		return LoginOutcome{Status: LoginPending}
	}
	if tok.RefreshToken == "" {
		// The gateway's credential persistence requires both tokens (the paste
		// path validates the same pair); a token bundle without the refresh
		// half could never complete the flow, so fail it here with the reason.
		return LoginOutcome{Status: LoginError, Message: "上游未签发 refreshToken，无法落盘凭证"}
	}
	acctURL := strings.TrimRight(c.base(), "/") + wireLoginAcctPath + state
	acctData, err := c.loginDo(ctx, http.MethodGet, acctURL, tok.AccessToken, nil)
	if err != nil {
		return LoginOutcome{Status: LoginError, Message: "获取账号信息失败: " + err.Error()}
	}
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(acctData, &acct); err != nil || acct.UID == "" {
		return LoginOutcome{Status: LoginError, Message: "登录已完成但上游未返回 uid（请重新发起登录）"}
	}
	if !validLoginUID(acct.UID) {
		// The uid becomes the auth file name downstream; path characters in it
		// would be a path traversal (wb2api panel/login.go guards the same way).
		return LoginOutcome{Status: LoginError, Message: "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）"}
	}
	cred := &Credential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Domain:       tok.Domain,
		Global:       c.realm() == "global",
	}
	if tok.ExpiresIn > 0 {
		cred.ExpiresAt = c.now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return LoginOutcome{Status: LoginDone, Credential: cred, Nickname: acct.Nickname}
}

// validLoginUID reports whether the upstream uid is safe to use as an auth
// file name: 1–64 chars of [A-Za-z0-9_-] (the Tencent-side uid is a UUID
// shape). Empty strings and path characters (. / \ …) are rejected.
func validLoginUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for i := 0; i < len(uid); i++ {
		ch := uid[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}
