//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// login_test.go pins the M8 device-authorization client against httptest
// stubs only — the suite never contacts the real endpoints (task red line).
// Coverage: Start wire shape (method/path/query/body/headers), the poll state
// machine (pending on 11217 "login ing" and every other not-finished reply,
// done on token+account), the identity-failure error class (account endpoint
// down, uid missing, uid illegal, refreshToken missing), expiry stamping and
// the realm brand switch.
package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// loginStub spins an httptest server recording every request and answering
// handler; the client returned points at it (BaseURL override).
type loginRequest struct {
	Method, Path, RawQuery, Body, Auth, Origin, Referer, UA string
}

func newLoginStub(t *testing.T, handler func(req loginRequest) (int, string)) (*LoginClient, *[]loginRequest) {
	t.Helper()
	requests := &[]loginRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := loginRequest{
			Method:   r.Method,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Body:     string(raw),
			Auth:     r.Header.Get("Authorization"),
			Origin:   r.Header.Get("Origin"),
			Referer:  r.Header.Get("Referer"),
			UA:       r.Header.Get("User-Agent"),
		}
		*requests = append(*requests, req)
		code, body := handler(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	client := NewLoginClient(LoginClientOptions{BaseURL: srv.URL})
	return client, requests
}

func loginOK(data string) string {
	return `{"code":0,"msg":"","data":` + data + `}`
}

func TestLoginStartWire(t *testing.T) {
	client, requests := newLoginStub(t, func(req loginRequest) (int, string) {
		return http.StatusOK, loginOK(`{"state":"st-1","authUrl":"https://auth.example/wb?state=st-1"}`)
	})
	res, err := client.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.State != "st-1" || res.LoginURL != "https://auth.example/wb?state=st-1" {
		t.Fatalf("Start result = %+v", res)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(*requests))
	}
	req := (*requests)[0]
	if req.Method != http.MethodPost || req.Path != "/v2/plugin/auth/state" || req.RawQuery != "platform=CLI" {
		t.Fatalf("wire shape = %s %s?%s", req.Method, req.Path, req.RawQuery)
	}
	if req.Body != "{}" {
		t.Fatalf("body = %q, want {}", req.Body)
	}
	// wb2api login header family: CLI UA + realm-brand Origin/Referer.
	if req.UA != loginClientUA {
		t.Fatalf("UA = %q, want %q", req.UA, loginClientUA)
	}
	if req.Origin != wireOriginCN || req.Referer != wireOriginCN+"/" {
		t.Fatalf("origin/referer = %q / %q", req.Origin, req.Referer)
	}
}

func TestLoginStartMissingFields(t *testing.T) {
	for name, data := range map[string]string{
		"no state":   `{"authUrl":"https://x"}`,
		"no authUrl": `{"state":"s"}`,
		"null data":  `null`,
	} {
		client, _ := newLoginStub(t, func(req loginRequest) (int, string) {
			return http.StatusOK, envelope(data)
		})
		if _, err := client.Start(context.Background()); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

func TestLoginStartUpstreamFailure(t *testing.T) {
	client, _ := newLoginStub(t, func(req loginRequest) (int, string) {
		return http.StatusInternalServerError, `{"code":500,"msg":"boom"}`
	})
	if _, err := client.Start(context.Background()); err == nil {
		t.Fatalf("5xx: want error")
	}
}

func TestLoginPollPendingThenDone(t *testing.T) {
	// Full three-step chain: token endpoint answers the documented 11217
	// "login ing" first, then the token bundle; the account endpoint then
	// completes the identity.
	tokenCalls := 0
	fixedNow := time.Unix(1800000000, 0)
	client, requests := newLoginStub(t, func(req loginRequest) (int, string) {
		switch req.Path {
		case "/v2/plugin/auth/token":
			tokenCalls++
			if tokenCalls == 1 {
				return http.StatusOK, `{"code":11217,"msg":"login ing","data":null}`
			}
			return http.StatusOK, loginOK(`{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":3600,"domain":"example.com"}`)
		case "/v2/plugin/login/account":
			if req.Auth != "Bearer at-1" {
				t.Errorf("account call auth = %q", req.Auth)
			}
			return http.StatusOK, loginOK(`{"uid":"uid-1","enterpriseId":"ent-1","nickname":"昵称"}`)
		default:
			t.Errorf("unexpected path %q", req.Path)
			return http.StatusNotFound, "{}"
		}
	})
	client.opts.Now = func() time.Time { return fixedNow }

	// Round 1: pending (11217 branch).
	if out := client.Poll(context.Background(), "st-1"); out.Status != LoginPending {
		t.Fatalf("poll 1 status = %v, want pending", out.Status)
	}
	// Round 2: done with the full credential.
	out := client.Poll(context.Background(), "st-1")
	if out.Status != LoginDone {
		t.Fatalf("poll 2 status = %v (%s), want done", out.Status, out.Message)
	}
	cred := out.Credential
	if cred == nil {
		t.Fatal("done outcome without credential")
	}
	if cred.AccessToken != "at-1" || cred.RefreshToken != "rt-1" || cred.UID != "uid-1" ||
		cred.EnterpriseID != "ent-1" || cred.Domain != "example.com" || cred.Global {
		t.Fatalf("credential = %+v", cred)
	}
	if !cred.ExpiresAt.Equal(fixedNow.Add(3600 * time.Second)) {
		t.Fatalf("expiresAt = %v", cred.ExpiresAt)
	}
	if out.Nickname != "昵称" {
		t.Fatalf("nickname = %q", out.Nickname)
	}
	// State travels on every upstream call.
	for _, req := range *requests {
		if !strings.Contains(req.RawQuery, "state=st-1") {
			t.Fatalf("call without state: %s?%s", req.Path, req.RawQuery)
		}
	}
}

func TestLoginPollPendingOnNotFinishedReplies(t *testing.T) {
	// The proven reference semantics: every token-endpoint failure that is
	// not a finished login maps to pending — transport-style HTTP failures,
	// undocumented business codes, unparseable bodies and tokenless data.
	cases := map[string]func(req loginRequest) (int, string){
		"http 500": func(req loginRequest) (int, string) {
			return http.StatusInternalServerError, `{"code":500,"msg":"boom"}`
		},
		"business 99999": func(req loginRequest) (int, string) { return http.StatusOK, `{"code":99999,"msg":"???","data":null}` },
		"garbage body":   func(req loginRequest) (int, string) { return http.StatusOK, `<html>blocked</html>` },
		"tokenless data": func(req loginRequest) (int, string) {
			return http.StatusOK, loginOK(`{"accessToken":"","refreshToken":""}`)
		},
		"empty data": func(req loginRequest) (int, string) { return http.StatusOK, loginOK(`null`) },
	}
	for name, handler := range cases {
		client, _ := newLoginStub(t, handler)
		if out := client.Poll(context.Background(), "st-1"); out.Status != LoginPending {
			t.Fatalf("%s: status = %v, want pending", name, out.Status)
		}
	}
}

func TestLoginPollErrorClass(t *testing.T) {
	cases := map[string]struct {
		acct     string
		acctCode int
		wantMsg  string
	}{
		"account endpoint down": {acctCode: http.StatusBadGateway, acct: "", wantMsg: "获取账号信息失败"},
		"uid missing":           {acctCode: http.StatusOK, acct: loginOK(`{"nickname":"x"}`), wantMsg: "uid"},
		"uid path traversal":    {acctCode: http.StatusOK, acct: loginOK(`{"uid":"../evil","nickname":"x"}`), wantMsg: "路径穿越"},
		"uid illegal chars":     {acctCode: http.StatusOK, acct: loginOK(`{"uid":"a b/c","nickname":"x"}`), wantMsg: "路径穿越"},
	}
	for name, tc := range cases {
		client, _ := newLoginStub(t, func(req loginRequest) (int, string) {
			if req.Path == "/v2/plugin/auth/token" {
				return http.StatusOK, loginOK(`{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":60}`)
			}
			return tc.acctCode, tc.acct
		})
		out := client.Poll(context.Background(), "st-1")
		if out.Status != LoginError {
			t.Fatalf("%s: status = %v, want error", name, out.Status)
		}
		if !strings.Contains(out.Message, tc.wantMsg) {
			t.Fatalf("%s: message = %q, want it to contain %q", name, out.Message, tc.wantMsg)
		}
	}
}

func TestLoginPollMissingRefreshToken(t *testing.T) {
	// The persistence contract requires both tokens (AddWorkbuddyCredential
	// validates the pair); fail the flow up front with the reason.
	client, _ := newLoginStub(t, func(req loginRequest) (int, string) {
		return http.StatusOK, loginOK(`{"accessToken":"at-1","expiresIn":60}`)
	})
	out := client.Poll(context.Background(), "st-1")
	if out.Status != LoginError || !strings.Contains(out.Message, "refreshToken") {
		t.Fatalf("status = %v message = %q", out.Status, out.Message)
	}
}

func TestLoginPollGlobalRealm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			_, _ = w.Write([]byte(loginOK(`{"accessToken":"at-g","refreshToken":"rt-g","expiresIn":60,"domain":"gb.example"}`)))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(loginOK(`{"uid":"uid-g","nickname":"global"}`)))
		}
	}))
	t.Cleanup(srv.Close)
	client := NewLoginClient(LoginClientOptions{BaseURL: srv.URL, Realm: "global"})
	if client.origin() != wireOriginGlobal {
		t.Fatalf("origin = %q, want %q", client.origin(), wireOriginGlobal)
	}
	out := client.Poll(context.Background(), "st-g")
	if out.Status != LoginDone {
		t.Fatalf("status = %v (%s)", out.Status, out.Message)
	}
	if out.Credential == nil || !out.Credential.Global {
		t.Fatalf("credential = %+v, want Global", out.Credential)
	}
}

func TestLoginPollRequestBodyShape(t *testing.T) {
	// The token and account calls are GETs without a body.
	var methods []string
	client, _ := newLoginStub(t, func(req loginRequest) (int, string) {
		methods = append(methods, req.Method)
		if req.Path == "/v2/plugin/auth/token" {
			return http.StatusOK, loginOK(`{"accessToken":"a","refreshToken":"r","expiresIn":60}`)
		}
		return http.StatusOK, loginOK(`{"uid":"u"}`)
	})
	client.Poll(context.Background(), "st-1")
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodGet {
		t.Fatalf("methods = %v", methods)
	}
}

func TestValidLoginUID(t *testing.T) {
	cases := map[string]bool{
		"uid-1":                 true,
		"UUID_4f2b-9c":          true,
		"":                      false,
		"../evil":               false,
		"a/b":                   false,
		"a\\b":                  false,
		"a b":                   false,
		"a.b":                   false,
		strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false,
	}
	for uid, want := range cases {
		if got := validLoginUID(uid); got != want {
			t.Fatalf("validLoginUID(%q) = %v, want %v", uid, got, want)
		}
	}
}

// loginCodePending: the documented "login ing" marker rides the same
// {code,msg,data} envelope decoder as every other reply (exercised through
// the pending branch in TestLoginPollPendingThenDone); pin the constant so a
// protocol renumber cannot slip through silently.
func TestLoginPendingCodeConstant(t *testing.T) {
	raw := `{"code":11217,"msg":"login ing","data":null}`
	var env loginEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Code != loginCodePending {
		t.Fatalf("envelope code = %d, want %d", env.Code, loginCodePending)
	}
}
