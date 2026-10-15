package api

// auth_login.go implements the M8 browser device-authorization login
// endpoints (frozen wire for the parallel frontend batch):
//
//	POST /api/accounts/workbuddy/login-start  body {}
//	     → 200 {"state":"…","login_url":"https://…授权链接"}
//	     → 502 {"error":"…"} when the upstream state call fails
//	GET  /api/accounts/workbuddy/login-poll?state=…
//	     → 200 {"status":"pending"}                        登录未完成，继续轮询
//	     → 200 {"status":"done","uid":"…","nickname":"…"}  凭证已落盘入池
//	     → 200 {"status":"expired"}                        state 未知/已过期/已消费
//	     → 200 {"status":"error","error":"…"}              登录完成但身份解析或落盘失败
//
// Both are withAPIKey-guarded and registered beside the other account routes
// (registerLogin from RegisterPanelData) behind the same nil-host/nil-store
// guard as the add-account routes: no credential store → no routes → 404.
//
// State machine (the "expired" class is ours alone — no upstream expiry code
// is documented, the platform client maps every not-finished token reply to
// pending):
//
//	login-start  → state stored in memory (TTL loginSessionTTL, wb2api
//	               panel loginTTL 口径), expired sessions swept.
//	login-poll   → unknown / TTL-expired / already-consumed state → expired
//	               (never forwarded upstream). Known state → one upstream
//	               poll round: pending | done | error. done persists the
//	               credential through host.Creds.AddWorkbuddyCredential
//	               (the same validate+persist+register face as the paste
//	               path) and consumes the session; error keeps the session
//	               so a transient account-endpoint failure can recover on
//	               the next poll — the TTL is the ultimate bound.
//
// The upstream client lives behind the loginBackend seam (growthBackend
// pattern): workbuddy builds wire the real LoginClient (auth_login_wire.go);
// qoder/trae-only builds get nil (auth_login_stub.go) and the handlers answer
// an honest build-limitation error instead of pretending.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"all2api/internal/host"
)

// loginSessionTTL bounds how long a login-start state stays pollable
// (wb2api panel loginTTL 口径: 15 分钟).
const loginSessionTTL = 15 * time.Minute

// loginNow is the session clock (tests stub it for the TTL branch).
var loginNow = time.Now

// loginSession is one in-flight browser login. Only the creation clock is
// kept: the realm and all credential material travel with the poll calls, so
// the map itself stays secret-free.
type loginSession struct {
	created time.Time
}

// loginSessionMap is the process-memory state store. Panel-scale by
// construction (a human logs in one account at a time); login-start sweeps
// expired entries so an abandoned flow cannot accumulate.
type loginSessionMap struct {
	mu       sync.Mutex
	sessions map[string]loginSession
}

var loginSessionStore = &loginSessionMap{sessions: map[string]loginSession{}}

func (m *loginSessionMap) put(state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := loginNow()
	for s, sess := range m.sessions {
		if now.Sub(sess.created) > loginSessionTTL {
			delete(m.sessions, s)
		}
	}
	m.sessions[state] = loginSession{created: now}
}

// get returns the session only while it is inside the TTL; an expired entry
// is deleted on sight (the next poll answers expired, not pending).
func (m *loginSessionMap) get(state string) (loginSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[state]
	if !ok {
		return loginSession{}, false
	}
	if loginNow().Sub(sess.created) > loginSessionTTL {
		delete(m.sessions, state)
		return loginSession{}, false
	}
	return sess, true
}

func (m *loginSessionMap) delete(state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, state)
}

// loginCredential is the tag-neutral credential snapshot the backend hands
// over on a completed login — field-for-field the auth-file shape the paste
// path persists (AddWorkbuddyCredential validates the same uid+token pair).
type loginCredential struct {
	AccessToken  string
	RefreshToken string
	UID          string
	Nickname     string
	EnterpriseID string
	Domain       string
	Realm        string // "cn" | "global" — stamped into the auth file
	ExpiresAt    int64  // unix seconds; 0 = no expiry documented
}

// loginPollResult is one backend poll round. done carries Cred; error
// carries a user-facing message that never echoes credential material.
type loginPollResult struct {
	Status  string // "pending" | "done" | "error"
	Message string
	Cred    *loginCredential
}

// loginBackend is the seam the login handlers serve. Implemented over the
// real workbuddy LoginClient in auth_login_wire.go (workbuddy builds).
type loginBackend interface {
	// Start creates one upstream authorization state.
	Start(ctx context.Context) (state, loginURL string, err error)
	// Poll runs one upstream poll round for that state.
	Poll(ctx context.Context, state string) loginPollResult
}

// registerLogin wires the two endpoints beside the other account routes
// (same nil-host/nil-store guard as registerAddAccount: without a credential
// store there is nothing to persist into — the routes stay absent, 404).
func registerLogin(mux *http.ServeMux, h *host.Host) {
	if h == nil || h.Creds == nil {
		return // store not wired — no credential sink, no login surface
	}
	b := newLoginBackend(h)
	mux.HandleFunc("POST /api/accounts/workbuddy/login-start", withAPIKey(handleLoginStart(b)))
	mux.HandleFunc("GET /api/accounts/workbuddy/login-poll", withAPIKey(handleLoginPoll(b, h)))
}

// loginUnavailable answers for builds without the workbuddy platform
// (qoder/trae-only): honest limitation, never a fabricated success.
func loginUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "当前构建未包含 workbuddy 平台，浏览器登录不可用"})
}

func handleLoginStart(b loginBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b == nil {
			loginUnavailable(w)
			return
		}
		state, loginURL, err := b.Start(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "生成登录链接失败: " + err.Error()})
			return
		}
		loginSessionStore.put(state)
		writeJSON(w, http.StatusOK, map[string]string{"state": state, "login_url": loginURL})
	}
}

func handleLoginPoll(b loginBackend, h *host.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b == nil {
			loginUnavailable(w)
			return
		}
		state := r.URL.Query().Get("state")
		if state == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing state"})
			return
		}
		if _, known := loginSessionStore.get(state); !known {
			// Unknown / TTL-expired / already-consumed state: the frozen
			// wire's expired class. Never forwarded upstream.
			writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
			return
		}
		res := b.Poll(r.Context(), state)
		switch res.Status {
		case "pending":
			writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		case "error":
			// Session kept: a transient identity-resolution failure can
			// recover on the next poll; the TTL is the ultimate bound.
			writeJSON(w, http.StatusOK, map[string]string{"status": "error", "error": res.Message})
		case "done":
			cred := res.Cred
			if cred == nil || cred.UID == "" {
				writeJSON(w, http.StatusOK, map[string]string{"status": "error", "error": "登录完成但缺少账号标识"})
				return
			}
			if err := persistLoginCredential(h, cred); err != nil {
				writeJSON(w, http.StatusOK, map[string]string{"status": "error", "error": "凭证落盘失败: " + err.Error()})
				return
			}
			loginSessionStore.delete(state) // consumed — any further poll expires
			// Immediate balance seeding, same as the paste path: a fresh
			// account with credit 0 reads as broken.
			go h.Creds.SeedCredits()
			writeJSON(w, http.StatusOK, map[string]any{"status": "done", "uid": cred.UID, "nickname": cred.Nickname})
		default:
			writeJSON(w, http.StatusOK, map[string]string{"status": "error", "error": "未知登录状态"})
		}
	}
}

// persistLoginCredential folds a completed browser login into the same
// AddWorkbuddyCredential face the paste path uses: build the auth-file JSON
// shape (account/auth blocks, realm key included), then validate + persist +
// register in one call. No credential material is logged or echoed back.
func persistLoginCredential(h *host.Host, cred *loginCredential) error {
	auth := map[string]any{
		"accessToken":  cred.AccessToken,
		"refreshToken": cred.RefreshToken,
		"realm":        cred.Realm,
	}
	if cred.Domain != "" {
		auth["domain"] = cred.Domain
	}
	if cred.ExpiresAt > 0 {
		auth["expiresAt"] = cred.ExpiresAt
	}
	doc := map[string]any{
		"account": map[string]any{
			"uid":          cred.UID,
			"nickname":     cred.Nickname,
			"enterpriseId": cred.EnterpriseID,
		},
		"auth": auth,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = h.Creds.AddWorkbuddyCredential(string(raw))
	return err
}
