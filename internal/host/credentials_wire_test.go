//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package host

// Integration tests over the full M3 seam: auth files in a t.TempDir →
// CredentialStore → workbuddy sender (CredentialSource + refresh sink) →
// httptest stub upstream. No real endpoint is contacted and no real auth file
// is read or written (task red lines). All synthesized requests use
// httptest.NewRequestWithContext (noctx forbids the context-free variant).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
)

// chatSender is the slice of the workbuddy sender the tests drive.
type chatSender interface {
	SendChat(ctx context.Context, acct *pool.Account, req adapter.ChatRequest) (*adapter.ChatResponse, error)
}

// stubAuthUpstream is the local fake upstream: chat + refresh endpoints with
// scriptable behavior, recording every auth-bearing header the gateway sends.
type stubAuthUpstream struct {
	mu sync.Mutex

	// script
	acceptAccess  string // chat 200 only for "Bearer <acceptAccess>"; else 401
	rotAccess     string // access token handed out on refresh success
	rotRefresh    string // refresh token handed out on refresh success
	refreshStatus int    // 0 → 200
	chatStatus    int    // non-zero overrides acceptAccess-based routing

	// observations
	chatCalls    int
	refreshCalls int
	chatAuth     []string // Authorization per chat call
	chatUID      []string // X-User-Id per chat call
	chatDomain   []string // X-Domain per chat call
	chatRefTok   []string // X-Refresh-Token per chat call (must stay empty)
	refreshAuth  []string // Authorization per refresh call (must stay empty)
	refreshTok   []string // X-Refresh-Token per refresh call
}

const stubSSE = "data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

func (u *stubAuthUpstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.chatCalls++
		u.chatAuth = append(u.chatAuth, r.Header.Get("Authorization"))
		u.chatUID = append(u.chatUID, r.Header.Get("X-User-Id"))
		u.chatDomain = append(u.chatDomain, r.Header.Get("X-Domain"))
		u.chatRefTok = append(u.chatRefTok, r.Header.Get("X-Refresh-Token"))
		status, forced, accept := u.chatStatus, u.chatStatus != 0, u.acceptAccess
		u.mu.Unlock()
		if !forced {
			status = http.StatusUnauthorized
			if accept != "" && r.Header.Get("Authorization") == "Bearer "+accept {
				status = http.StatusOK
			}
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"code":12153,"msg":"Offline user session not found"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stubSSE)
	})
	mux.HandleFunc("/v2/plugin/auth/token/refresh", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.refreshCalls++
		u.refreshAuth = append(u.refreshAuth, r.Header.Get("Authorization"))
		u.refreshTok = append(u.refreshTok, r.Header.Get("X-Refresh-Token"))
		status, rotA, rotR := u.refreshStatus, u.rotAccess, u.rotRefresh
		u.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"code":11140,"msg":"refresh rejected by stub"}`)
			return
		}
		fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"accessToken":%q,"refreshToken":%q,"expiresIn":5184000,"domain":"www.codebuddy.cn"}}`, rotA, rotR)
	})
	return mux
}

// stubObs is a copy of the stub's observations (never the stub itself — the
// sync.Mutex inside must not be copied).
type stubObs struct {
	chatCalls    int
	refreshCalls int
	chatAuth     []string
	chatUID      []string
	chatDomain   []string
	chatRefTok   []string
	refreshAuth  []string
	refreshTok   []string
}

// obs copies the observations under the lock.
func (u *stubAuthUpstream) obs() stubObs {
	u.mu.Lock()
	defer u.mu.Unlock()
	return stubObs{
		chatCalls:    u.chatCalls,
		refreshCalls: u.refreshCalls,
		chatAuth:     u.chatAuth,
		chatUID:      u.chatUID,
		chatDomain:   u.chatDomain,
		chatRefTok:   u.chatRefTok,
		refreshAuth:  u.refreshAuth,
		refreshTok:   u.refreshTok,
	}
}

// wireUp serves the stub upstream, loads dir into a fresh store+pool, and
// wires the store into a fresh sender as credential source + refresh sink.
func wireUp(t *testing.T, dir string, u *stubAuthUpstream, now func() time.Time) (*CredentialStore, *pool.WeightedRandomSelector, chatSender) {
	t.Helper()
	srv := httptest.NewServer(u.handler())
	t.Cleanup(srv.Close)
	p := pool.NewWeightedRandomSelector(0, 3, 60)
	store := NewCredentialStore(p)
	loaded, err := store.LoadAuthDir(dir)
	if err != nil {
		t.Fatalf("LoadAuthDir: %v", err)
	}
	if loaded == 0 {
		t.Fatalf("no credentials loaded from %s", dir)
	}
	sender := workbuddy.NewSender()
	sender.Configure(workbuddy.ClientOptions{
		BaseURL:     srv.URL,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
		Credentials: store,
		Refresher:   store,
		Now:         now,
	})
	return store, p, sender
}

// chatRequest is the minimal gateway-shaped chat request.
func chatRequest() adapter.ChatRequest {
	return adapter.ChatRequest{
		Model: "glm-5.3-flash",
		Messages: []adapter.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
}

// sendChat runs one send over a context derived from httptest.NewRequestWithContext.
func sendChat(t *testing.T, sender chatSender, acct *pool.Account) (*adapter.ChatResponse, error) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	return sender.SendChat(req.Context(), acct, chatRequest())
}

func TestAuth401TriggersRefreshReplayAndFileWriteBack(t *testing.T) {
	dir := t.TempDir()
	staleAccess, staleRefresh := fakeAccessToken("ACCT1FAKE"), fakeRefreshToken("REF1FAKE")
	rotAccess, rotRefresh := fakeAccessToken("ACCT1NEW"), fakeRefreshToken("REF1NEW")
	path := writeAuthFile(t, dir, "uid-a", staleAccess, staleRefresh, time.Now().Add(time.Hour).Unix())

	u := &stubAuthUpstream{acceptAccess: rotAccess, rotAccess: rotAccess, rotRefresh: rotRefresh}
	store, p, sender := wireUp(t, dir, u, nil)

	resp, err := sendChat(t, sender, &pool.Account{ID: "uid-a", Platform: WorkbuddyPlatform, Status: "active"})
	if err != nil {
		t.Fatalf("SendChat after refresh replay: %v", err)
	}
	if resp == nil || len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		t.Fatal("no usable response after replay")
	}

	obs := u.obs()
	if obs.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", obs.refreshCalls)
	}
	// Refresh endpoint contract (report §3): X-Refresh-Token carries the
	// stored refresh token; no Authorization bearer rides the refresh path.
	if len(obs.refreshTok) != 1 || obs.refreshTok[0] != staleRefresh {
		t.Errorf("refresh endpoint saw refresh tokens %d entries, want exactly the stored one", len(obs.refreshTok))
	}
	if anyNonEmpty(obs.refreshAuth) {
		t.Errorf("refresh request carried Authorization: %d non-empty values", countNonEmpty(obs.refreshAuth))
	}
	// Chat path: never the refresh token; stale first, rotated on replay.
	if anyNonEmpty(obs.chatRefTok) {
		t.Errorf("chat request carried X-Refresh-Token: %d non-empty values", countNonEmpty(obs.chatRefTok))
	}
	if len(obs.chatAuth) < 2 || obs.chatAuth[0] != "Bearer "+staleAccess || obs.chatAuth[len(obs.chatAuth)-1] != "Bearer "+rotAccess {
		t.Errorf("chat Authorization sequence = %d entries, want stale→rotated", len(obs.chatAuth))
	}
	// Account header family rides every chat call.
	for i, uid := range obs.chatUID {
		if uid != "uid-a" {
			t.Errorf("chat call %d X-User-Id = %q, want uid-a", i, uid)
		}
		if obs.chatDomain[i] != "www.codebuddy.cn" {
			t.Errorf("chat call %d X-Domain = %q, want www.codebuddy.cn", i, obs.chatDomain[i])
		}
	}

	// File write-back: same name, structure kept, tokens replaced.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, staleAccess) || strings.Contains(text, staleRefresh) {
		t.Error("stale token material survived the rotation write-back")
	}
	if !strings.Contains(text, rotAccess) || !strings.Contains(text, rotRefresh) {
		t.Error("rotated tokens missing from the written file")
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("written file is not JSON: %v", err)
	}
	auth := doc["auth"].(map[string]any)
	if auth["accessToken"] != rotAccess || auth["refreshToken"] != rotRefresh {
		t.Error("auth block not updated with the rotated pair")
	}
	if auth["realm"] != "cn" || auth["domain"] != "www.codebuddy.cn" {
		t.Errorf("untouched auth fields lost: realm=%v domain=%v", auth["realm"], auth["domain"])
	}
	account := doc["account"].(map[string]any)
	if account["uid"] != "uid-a" || account["nickname"] != "worker-uid-a" {
		t.Errorf("account block not preserved: %v", account)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	// Store now serves the rotated pair.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/probe", nil)
	cred, err := store.CredentialFor(req.Context(), &pool.Account{ID: "uid-a", Platform: WorkbuddyPlatform})
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != rotAccess || cred.RefreshToken != rotRefresh {
		t.Error("store does not serve the rotated pair after AcceptRefreshed")
	}
	// Pool expiry synced forward by the 60-day expiresIn.
	snap := p.SnapshotAccounts()
	if len(snap) != 1 || !snap[0].ExpiresAt.After(time.Now().Add(24*time.Hour)) {
		t.Errorf("pool expiry not synced forward: %v", snap[0].ExpiresAt)
	}
}

func TestAuthExpiredTokenRefreshesBeforeSend(t *testing.T) {
	dir := t.TempDir()
	staleAccess, staleRefresh := fakeAccessToken("ACCT2FAKE"), fakeRefreshToken("REF2FAKE")
	rotAccess, rotRefresh := fakeAccessToken("ACCT2NEW"), fakeRefreshToken("REF2NEW")
	path := writeAuthFile(t, dir, "uid-b", staleAccess, staleRefresh, 1000) // long expired

	frozen := time.Unix(2000, 0)
	u := &stubAuthUpstream{acceptAccess: rotAccess, rotAccess: rotAccess, rotRefresh: rotRefresh}
	_, _, sender := wireUp(t, dir, u, func() time.Time { return frozen })

	// Pre-send expiry must rotate BEFORE the first chat call: no request ever
	// carries the stale access token.
	resp, err := sendChat(t, sender, &pool.Account{ID: "uid-b", Platform: WorkbuddyPlatform, Status: "active"})
	if err != nil {
		t.Fatalf("SendChat with expired stored token: %v", err)
	}
	if resp == nil || len(resp.Choices) == 0 {
		t.Fatal("no usable response")
	}

	obs := u.obs()
	if obs.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", obs.refreshCalls)
	}
	if obs.refreshTok[0] != staleRefresh {
		t.Error("refresh endpoint did not receive the stored refresh token")
	}
	for i, a := range obs.chatAuth {
		if a != "Bearer "+rotAccess {
			t.Errorf("chat call %d carried an unexpected token (want only the rotated one)", i)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), staleAccess) || !strings.Contains(string(raw), rotAccess) {
		t.Error("file not rewritten with the rotated token after pre-send refresh")
	}
}

func TestRefreshFailureSurfacesAuthExpiredAndCoolsPoolAccount(t *testing.T) {
	dir := t.TempDir()
	staleAccess, staleRefresh := fakeAccessToken("ACCT3FAKE"), fakeRefreshToken("REF3FAKE")
	path := writeAuthFile(t, dir, "uid-c", staleAccess, staleRefresh, time.Now().Add(time.Hour).Unix())

	u := &stubAuthUpstream{refreshStatus: http.StatusInternalServerError}
	_, p, sender := wireUp(t, dir, u, nil)

	_, err := sendChat(t, sender, &pool.Account{ID: "uid-c", Platform: WorkbuddyPlatform, Status: "active"})
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
		t.Fatalf("err = %v, want auth_expired after failed refresh", err)
	}
	if obs := u.obs(); obs.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", obs.refreshCalls)
	}

	// No write-back on failure: the auth file still holds the original pair.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), staleAccess) || !strings.Contains(string(raw), staleRefresh) {
		t.Error("failed refresh must not touch the auth file")
	}

	// Auth-rejection semantics: the router feeds "auth_error" into the pool
	// for an auth_expired failure; the account parks in the bounded auth
	// cooldown at zero health (2026-10-21: a transient upstream 403 storm
	// must not permanently disable healthy accounts).
	p.UpdateHealth(WorkbuddyPlatform, "uid-c", "auth_error", 0)
	snap := p.SnapshotAccounts()
	if len(snap) != 1 {
		t.Fatalf("pool accounts = %d, want 1", len(snap))
	}
	if snap[0].Status != "cooling" || snap[0].Health != 0 {
		t.Errorf("after auth_error: status = %q health = %v, want cooling/0", snap[0].Status, snap[0].Health)
	}
}

func TestRefreshSingleFlightAndQuietWindow(t *testing.T) {
	dir := t.TempDir()
	staleAccess, staleRefresh := fakeAccessToken("ACCT4FAKE"), fakeRefreshToken("REF4FAKE")
	rotAccess, rotRefresh := fakeAccessToken("ACCT4NEW"), fakeRefreshToken("REF4NEW")
	writeAuthFile(t, dir, "uid-d", staleAccess, staleRefresh, time.Now().Add(time.Hour).Unix())

	// Chat rejects everything: initial send and every replay answer 401. The
	// refresh endpoint succeeds — so the only way refreshCalls stays at 1 is
	// the single-flight gate plus the quiet window.
	u := &stubAuthUpstream{acceptAccess: "rejected-by-stub", rotAccess: rotAccess, rotRefresh: rotRefresh}
	_, _, sender := wireUp(t, dir, u, nil)

	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acct := &pool.Account{ID: "uid-d", Platform: WorkbuddyPlatform, Status: "active"}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			_, err := sender.SendChat(req.Context(), acct, chatRequest())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		var ue *adapter.UpstreamError
		if !errors.As(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
			t.Errorf("concurrent send err = %v, want auth_expired", err)
		}
	}
	if obs := u.obs(); obs.refreshCalls != 1 {
		t.Fatalf("refresh calls after %d concurrent sends = %d, want 1 (single-flight)", n, obs.refreshCalls)
	}

	// Quiet window: a further send retries nothing for 5 minutes.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	if _, err := sender.SendChat(req.Context(), &pool.Account{ID: "uid-d", Platform: WorkbuddyPlatform, Status: "active"}, chatRequest()); err == nil {
		t.Fatal("stub rejects everything; send must fail")
	}
	if obs := u.obs(); obs.refreshCalls != 1 {
		t.Fatalf("refresh calls after quiet-window send = %d, want still 1", obs.refreshCalls)
	}
}

func TestCredentialSourceUnknownAccountErrors(t *testing.T) {
	store := NewCredentialStore(nil)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/probe", nil)

	if _, err := store.CredentialFor(req.Context(), nil); err == nil {
		t.Error("nil account must error, not panic")
	}
	_, err := store.CredentialFor(req.Context(), &pool.Account{ID: "ghost", Platform: WorkbuddyPlatform})
	if err == nil || !strings.Contains(err.Error(), "no credential for workbuddy/ghost") {
		t.Fatalf("err = %v, want the explicit no-credential error", err)
	}
}

func TestWireWorkbuddySenderInjectsStore(t *testing.T) {
	store := NewCredentialStore(pool.NewWeightedRandomSelector(0, 3, 60))
	if n := store.WireWorkbuddySender(); n != 1 {
		t.Fatalf("WireWorkbuddySender = %d, want 1 (workbuddy adapter registers via init)", n)
	}
	t.Cleanup(func() {
		// Restore the production-neutral state for any later test: the
		// Configure contract replaces options wholesale.
		if a, ok := adapter.Default.Get(WorkbuddyPlatform).(interface{ Configure(workbuddy.ClientOptions) }); ok {
			a.Configure(workbuddy.ClientOptions{})
		}
	})

	sender, ok := adapter.Default.Get(WorkbuddyPlatform).(adapter.Sender)
	if !ok {
		t.Fatal("workbuddy adapter does not implement adapter.Sender")
	}
	// Unknown account: fails with auth_expired BEFORE any network I/O (no
	// credential in the store, nothing to refresh with) — zero risk of
	// touching a real endpoint.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	_, err := sender.SendChat(req.Context(), &pool.Account{ID: "nope", Platform: WorkbuddyPlatform, Status: "active"}, chatRequest())
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
		t.Fatalf("err = %v, want auth_expired from the wired store", err)
	}
	if !strings.Contains(err.Error(), "no credential for workbuddy/nope") {
		t.Errorf("err = %v, want the store's explicit lookup error", err)
	}
}

func anyNonEmpty(xs []string) bool {
	for _, x := range xs {
		if x != "" {
			return true
		}
	}
	return false
}

func countNonEmpty(xs []string) int {
	n := 0
	for _, x := range xs {
		if x != "" {
			n++
		}
	}
	return n
}
