//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// refresh.go implements the 401/expiry-driven token refresh flow (report §3):
// POST {base}/v2/plugin/auth/token/refresh carrying X-Refresh-Token plus
// X-Auth-Refresh-Source: plugin — never an Authorization bearer, and the
// refresh token never rides the chat path. The rotated pair always rotates
// together and is handed to ClientOptions.Refresher (the credential store)
// before the failed send replays once with the new access token.
//
// Gating: at most one refresh per account at a time (single-flight —
// concurrent senders wait on the in-flight call and inherit its result), and
// a successful refresh silences further refresh attempts for
// refreshQuietWindow, so a still-failing account cannot hammer the refresh
// endpoint.
package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/pool"
)

// refreshQuietWindow suppresses repeated refreshes after one success: a
// freshly rotated pair is the freshest the upstream will hand out, so any
// auth failure inside the window is final for this account until the store
// changes again.
const refreshQuietWindow = 5 * time.Minute

// errRefreshSuppressed marks "a refresh for this account already succeeded
// inside refreshQuietWindow" — the caller falls back to the freshest stored
// credential instead of hitting the network again.
var errRefreshSuppressed = errors.New("workbuddy: refresh suppressed (already refreshed within quiet window)")

// refreshFlight is one in-flight token rotation. Concurrent senders for the
// same account wait on done and inherit the result (single-flight).
type refreshFlight struct {
	done chan struct{}
	cred *Credential
	err  error
}

// postWithRotation resolves the credential, posts the SSE request, and on an
// auth-expired reply (HTTP 401 or the session-death envelope) rotates the
// token pair once and replays. Pre-send expiry goes through the same gate: an
// expired stored token is rotated before the first send instead of failing.
func (s *senderImpl) postWithRotation(ctx context.Context, acct *pool.Account, body []byte) (io.ReadCloser, error) {
	cred, err := s.credentialFor(ctx, acct)
	if err != nil {
		var ue *adapter.UpstreamError
		if !errors.As(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
			return nil, err
		}
		// The stored credential is expired (or absent): rotate first, then
		// send. On rotation failure the original expiry verdict carries the
		// account id and reason the router's retry loop understands.
		refreshed, rerr := s.rotateCredential(ctx, acct, nil)
		if rerr != nil {
			return nil, err
		}
		cred = refreshed
	}
	rc, sendErr := s.postStream(ctx, cred, body)
	if sendErr == nil || ctx.Err() != nil {
		return rc, sendErr
	}
	var ue *adapter.UpstreamError
	if !errors.As(sendErr, &ue) || ue.Reason != adapter.ReasonAuthExpired {
		return rc, sendErr
	}
	// Session death after send: rotate and replay exactly once — never a
	// second refresh inside one send.
	refreshed, rerr := s.rotateCredential(ctx, acct, cred)
	if rerr != nil {
		return nil, sendErr
	}
	return s.postStream(ctx, refreshed, body)
}

// rotateCredential runs the gated refresh for acct. A nil stale credential
// means "the stored token is already expired": the refresh token then comes
// from the credential source. A suppressed rotation (a refresh already
// succeeded inside the quiet window) falls back to the freshest stored
// credential when it is usable.
func (s *senderImpl) rotateCredential(ctx context.Context, acct *pool.Account, stale *Credential) (*Credential, error) {
	refreshed, rerr := s.refreshFor(ctx, acct, stale)
	if rerr == nil {
		return refreshed, nil
	}
	if errors.Is(rerr, errRefreshSuppressed) {
		if cur, ok := s.currentCredential(ctx, acct); ok {
			return cur, nil
		}
	}
	return nil, rerr
}

// refreshFor performs at most one concurrent refresh per account, and none at
// all within refreshQuietWindow of the last success.
func (s *senderImpl) refreshFor(ctx context.Context, acct *pool.Account, stale *Credential) (*Credential, error) {
	if acct == nil {
		return nil, errors.New("workbuddy: refresh: no account given")
	}
	key := acct.Platform + "/" + acct.ID
	now := s.now()

	s.refreshMu.Lock()
	if s.refreshBusy == nil {
		s.refreshBusy = make(map[string]*refreshFlight)
		s.refreshOKAt = make(map[string]time.Time)
	}
	if at, ok := s.refreshOKAt[key]; ok && now.Sub(at) < refreshQuietWindow {
		s.refreshMu.Unlock()
		return nil, errRefreshSuppressed
	}
	if f, ok := s.refreshBusy[key]; ok {
		// Someone else is rotating for this account right now: wait and
		// inherit the result instead of issuing a second wire call.
		s.refreshMu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return f.cred, f.err
	}
	f := &refreshFlight{done: make(chan struct{})}
	s.refreshBusy[key] = f
	s.refreshMu.Unlock()

	cred, err := s.doRefresh(ctx, acct, stale)

	s.refreshMu.Lock()
	delete(s.refreshBusy, key)
	if err == nil {
		s.refreshOKAt[key] = s.now()
	}
	s.refreshMu.Unlock()

	f.cred, f.err = cred, err
	close(f.done)
	return cred, err
}

// doRefresh performs one refresh wire call and hands the rotated pair to the
// configured sink. The refresh token comes from stale (the credential the
// failed send used), else from the credential source — the token that holds
// the current session is the one the upstream expects to rotate.
func (s *senderImpl) doRefresh(ctx context.Context, acct *pool.Account, stale *Credential) (*Credential, error) {
	opts := s.options()
	if opts.Refresher == nil {
		return nil, errors.New("workbuddy: no credential sink wired; refresh disabled")
	}
	base := stale
	if base == nil {
		base = s.storedCredential(ctx, acct)
	}
	refreshToken := ""
	if base != nil {
		refreshToken = base.RefreshToken
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("workbuddy: account %s has no refresh token", acct.ID)
	}
	rotated, err := s.callRefreshEndpoint(ctx, base, refreshToken)
	if err != nil {
		return nil, err
	}
	if err := opts.Refresher.AcceptRefreshed(ctx, acct, rotated); err != nil {
		return nil, fmt.Errorf("workbuddy: persist refreshed credential: %w", err)
	}
	return rotated, nil
}

// callRefreshEndpoint POSTs the refresh endpoint (report §3): X-Refresh-Token
// + X-Auth-Refresh-Source: plugin, no Authorization bearer. The rotated pair
// comes back in the envelope's data object; both tokens always rotate
// together and expiresIn is seconds (observed constant 5184000 = 60d).
func (s *senderImpl) callRefreshEndpoint(ctx context.Context, base *Credential, refreshToken string) (*Credential, error) {
	url := strings.TrimRight(s.baseFor(base), "/") + wireRefreshPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	setCommonHeaders(req, base)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Refresh-Token", refreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")

	resp, err := s.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &adapter.UpstreamError{Status: http.StatusGatewayTimeout, Reason: adapter.ReasonTimeout, Message: "workbuddy: refresh: " + ctx.Err().Error()}
		}
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: refresh transport: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: read refresh body: " + err.Error()}
	}
	body := string(raw)
	if resp.StatusCode >= 400 {
		return nil, classifyError(resp.StatusCode, body, resp.Header)
	}
	access, refresh, expiresIn, domain := parseRefreshEnvelope(body)
	if code, ok := businessCode(body); ok && code != "" && code != "0" {
		return nil, &adapter.UpstreamError{
			Status:  pickStatus(resp.StatusCode, http.StatusUnauthorized),
			Reason:  adapter.ReasonAuthExpired,
			Message: "workbuddy: refresh rejected: " + truncate(envelopeMessage(body), 200),
		}
	}
	if access == "" {
		return nil, &adapter.UpstreamError{Status: http.StatusBadGateway, Reason: adapter.ReasonPlatformDown, Message: "workbuddy: refresh reply carries no access token"}
	}
	rotated := &Credential{
		AccessToken:  access,
		RefreshToken: refresh,
		Domain:       domain,
	}
	if base != nil {
		if rotated.RefreshToken == "" {
			// The protocol rotates both tokens together; a reply that omits
			// the refresh token keeps the old one so the account stays
			// refreshable instead of silently going stale.
			rotated.RefreshToken = base.RefreshToken
		}
		rotated.UID = base.UID
		rotated.EnterpriseID = base.EnterpriseID
		rotated.Global = base.Global
		if rotated.Domain == "" {
			rotated.Domain = base.Domain
		}
	}
	if expiresIn > 0 {
		rotated.ExpiresAt = s.now().Add(time.Duration(expiresIn) * time.Second)
	}
	return rotated, nil
}

// parseRefreshEnvelope tolerantly extracts the rotated pair from a refresh
// reply. The documented shape is {code,msg,data:{accessToken,refreshToken,
// expiresIn,domain}}; a flat (data-less) reply is read the same way.
func parseRefreshEnvelope(body string) (access, refresh string, expiresIn int64, domain string) {
	var root map[string]any
	if json.Unmarshal([]byte(body), &root) != nil {
		return "", "", 0, ""
	}
	data, ok := root["data"].(map[string]any)
	if !ok {
		data = root
	}
	access = stringOf(data["accessToken"])
	refresh = stringOf(data["refreshToken"])
	domain = stringOf(data["domain"])
	if v, ok := data["expiresIn"].(float64); ok && v > 0 {
		expiresIn = int64(v)
	}
	return access, refresh, expiresIn, domain
}

// storedCredential returns whatever the source currently holds for acct —
// expired or not: the store is the persistence layer, expiry policy lives in
// this package.
func (s *senderImpl) storedCredential(ctx context.Context, acct *pool.Account) *Credential {
	src := s.options().Credentials
	if src == nil || acct == nil {
		return nil
	}
	c, err := src.CredentialFor(ctx, acct)
	if err != nil || c == nil {
		return nil
	}
	return c
}

// currentCredential returns the stored credential only when it is usable:
// non-empty access token and not past ExpiresAt.
func (s *senderImpl) currentCredential(ctx context.Context, acct *pool.Account) (*Credential, bool) {
	c := s.storedCredential(ctx, acct)
	if c == nil || c.AccessToken == "" {
		return nil, false
	}
	if !c.ExpiresAt.IsZero() && !s.now().Before(c.ExpiresAt) {
		return nil, false
	}
	return c, true
}
