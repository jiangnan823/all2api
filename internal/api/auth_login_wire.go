//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// auth_login_wire.go binds the M8 browser-login endpoints to the real
// workbuddy login client (the platform device-authorization flow in
// platform/workbuddy/login.go). Compiled under the same build tags as the
// workbuddy platform; qoder/trae-only builds get the nil backend from
// auth_login_stub.go and the handlers answer an honest build-limitation
// error instead.
package api

import (
	"context"

	"all2api/internal/host"
	"all2api/internal/platform/workbuddy"
)

// newLoginBackend is the production constructor: the login client over the
// realm-default endpoint bases (CN; the login flow is the CLI device shape).
func newLoginBackend(_ *host.Host) loginBackend {
	return newWBLoginBackend(workbuddy.LoginClientOptions{})
}

// newWBLoginBackend is the options-carrying form tests use to point the login
// client at httptest stub servers.
func newWBLoginBackend(opts workbuddy.LoginClientOptions) loginBackend {
	return &wbLoginBackend{client: workbuddy.NewLoginClient(opts)}
}

// wbLoginBackend adapts the platform client onto the tag-neutral seam.
// Safe for concurrent requests: the client is stateless per call.
type wbLoginBackend struct {
	client *workbuddy.LoginClient
}

func (b *wbLoginBackend) Start(ctx context.Context) (string, string, error) {
	res, err := b.client.Start(ctx)
	if err != nil {
		return "", "", err
	}
	return res.State, res.LoginURL, nil
}

func (b *wbLoginBackend) Poll(ctx context.Context, state string) loginPollResult {
	out := b.client.Poll(ctx, state)
	switch out.Status {
	case workbuddy.LoginPending:
		return loginPollResult{Status: "pending"}
	case workbuddy.LoginError:
		return loginPollResult{Status: "error", Message: out.Message}
	default:
		cred := out.Credential
		if cred == nil {
			return loginPollResult{Status: "error", Message: "登录完成但缺少凭证"}
		}
		realm := "cn"
		if cred.Global {
			realm = "global"
		}
		var expiresAt int64
		if !cred.ExpiresAt.IsZero() {
			expiresAt = cred.ExpiresAt.Unix()
		}
		return loginPollResult{
			Status: "done",
			Cred: &loginCredential{
				AccessToken:  cred.AccessToken,
				RefreshToken: cred.RefreshToken,
				UID:          cred.UID,
				Nickname:     out.Nickname,
				EnterpriseID: cred.EnterpriseID,
				Domain:       cred.Domain,
				Realm:        realm,
				ExpiresAt:    expiresAt,
			},
		}
	}
}
