//go:build qoder || full || (!qoder && !trae && !workbuddy)

package host

// Qoder wire tests over the full seam: PAT file in a t.TempDir →
// CredentialStore → qoder sender (CredentialSource + quota probe) → httptest
// stub upstream. No real endpoint is contacted (task red line).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/platform/qoder"
	"all2api/internal/pool"
)

// qoderStubUpstream answers the exchange chain, the quota endpoint and one
// healthy chat stream (minimal shape — protocol detail lives in the qoder
// package's own tests).
type qoderStubUpstream struct {
	mu    sync.Mutex
	calls map[string]int
}

func newQoderStub() *qoderStubUpstream {
	return &qoderStubUpstream{calls: map[string]int{}}
}

func (u *qoderStubUpstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobToken/exchange", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"jt-fake","refresh_token":"jrt-fake","expires_in":86400000}`)
	})
	mux.HandleFunc("/api/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"user-1","name":"tester"}`)
	})
	mux.HandleFunc("/algo/api/v3/user/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"userType":"personal_standard"}`)
	})
	mux.HandleFunc("/api/v2/quota/usage", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"userId":"user-1","userQuota":{"total":100,"used":10,"remaining":1234.6}}`)
	})
	mux.HandleFunc("/algo/api/v2/service/pro/sse/agent_chat_generation", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls["chat"]++
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		env := func(body string) string {
			return "data: " + qoderStubJSON(map[string]any{"body": body, "statusCode": "OK", "statusCodeValue": 200}) + "\n\n"
		}
		fmt.Fprint(w,
			env(`{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)+
				env(`{"choices":[{"index":0,"delta":{"content":"wired"},"finish_reason":null}]}`)+
				env(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+
				env("[DONE]"))
	})
	return mux
}

func (u *qoderStubUpstream) count(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[name]
}

func qoderStubJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// qoderTestSource adapts the store onto qoder.CredentialSource for the stub
// wiring (the wire file's qoderCredSource is unexported and asserts the same
// contract; this mirror keeps the test independent of its internals).
type qoderTestSource struct{ store *CredentialStore }

func (q qoderTestSource) CredentialFor(_ context.Context, acct *pool.Account) (*qoder.PATCredential, error) {
	cred, ok := q.store.lookup(credKey(acct.Platform, acct.ID))
	if !ok {
		return nil, fmt.Errorf("credential store: no credential for %s/%s", acct.Platform, acct.ID)
	}
	return &qoder.PATCredential{PAT: cred.AccessToken}, nil
}

// qoderWireUp serves the stub, loads the PAT into a fresh store+pool and
// points the registered adapter's sender at the stub (the same Configure seam
// WireQoderSender drives in production). Production neutrality is restored
// on cleanup.
func qoderWireUp(t *testing.T, dir string) (*CredentialStore, *pool.WeightedRandomSelector, *qoderStubUpstream) {
	t.Helper()
	u := newQoderStub()
	srv := httptest.NewServer(u.handler())
	t.Cleanup(srv.Close)
	p := pool.NewWeightedRandomSelector(0, 3, 60)
	store := NewCredentialStore(p)
	if loaded, err := store.LoadAuthDir(dir); err != nil || loaded != 1 {
		t.Fatalf("LoadAuthDir = %d, %v", loaded, err)
	}

	a, ok := adapter.Default.Get(QoderPlatform).(interface{ Configure(qoder.ClientOptions) })
	if !ok {
		t.Fatal("qoder adapter lacks the Configure seam in this build")
	}
	a.Configure(qoder.ClientOptions{
		AuthBaseURL:    srv.URL,
		OpenAPIBaseURL: srv.URL,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		Credentials:    qoderTestSource{store: store},
	})
	t.Cleanup(func() { a.Configure(qoder.ClientOptions{}) })
	return store, p, u
}

// qoderTestChatRequest is the minimal gateway-shaped chat request.
func qoderTestChatRequest() adapter.ChatRequest {
	return adapter.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []adapter.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
}

func TestWireQoderSenderInjectsStore(t *testing.T) {
	store := NewCredentialStore(pool.NewWeightedRandomSelector(0, 3, 60))
	if n := store.WireQoderSender(); n != 1 {
		t.Fatalf("WireQoderSender = %d, want 1 (qoder adapter registers via init)", n)
	}
	t.Cleanup(func() {
		if a, ok := adapter.Default.Get(QoderPlatform).(interface{ Configure(qoder.ClientOptions) }); ok {
			a.Configure(qoder.ClientOptions{})
		}
	})

	sender, ok := adapter.Default.Get(QoderPlatform).(adapter.Sender)
	if !ok {
		t.Fatal("qoder adapter does not implement adapter.Sender")
	}
	// Unknown account: fails with auth_expired BEFORE any network I/O —
	// zero risk of touching a real endpoint.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	_, err := sender.SendChat(req.Context(), &pool.Account{ID: "nope", Platform: QoderPlatform, Status: "active"}, qoderTestChatRequest())
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) || ue.Reason != adapter.ReasonAuthExpired {
		t.Fatalf("err = %v, want auth_expired from the wired store", err)
	}
	if !strings.Contains(err.Error(), "no credential for qoder/nope") {
		t.Errorf("err = %v, want the store's explicit lookup error", err)
	}
}

func TestSeedQoderCreditsSeedsPool(t *testing.T) {
	dir := t.TempDir()
	writeQoderPAT(t, dir, fakePAT("ACCT1FAKE"))
	store, p, u := qoderWireUp(t, dir)

	seeded, failed := store.SeedQoderCredits()
	if seeded != 1 || failed != 0 {
		t.Fatalf("SeedQoderCredits = %d/%d, want 1/0", seeded, failed)
	}
	if u.count("chat") != 0 {
		t.Error("credit seeding must not send chat requests")
	}
	snap := p.SnapshotAccounts()
	if len(snap) != 1 || snap[0].Credit != 1235 {
		t.Errorf("pool credit = %+v, want 1235 (rounded remaining)", snap)
	}
}

// TestSeedQoderCreditsCountsFailures: an account whose credential cannot
// authenticate (empty PAT → auth_expired) counts as failed and leaves the
// pool credit untouched.
func TestSeedQoderCreditsCountsFailures(t *testing.T) {
	dir := t.TempDir()
	writeQoderPAT(t, dir, fakePAT("ACCT2FAKE"))
	store, p, _ := qoderWireUp(t, dir)

	// Replace the store entry with a credential-less account.
	store.entries = map[string]fileCred{
		credKey(QoderPlatform, "ghost"): {AccessToken: "", UID: "ghost"},
	}
	p.AddAccount(&pool.Account{ID: "ghost", Platform: QoderPlatform, Status: "active"})

	seeded, failed := store.SeedQoderCredits()
	if seeded != 0 || failed != 1 {
		t.Fatalf("SeedQoderCredits = %d/%d, want 0/1 (empty PAT is auth_expired)", seeded, failed)
	}
	snap := p.SnapshotAccounts()
	for _, a := range snap {
		if a.Credit != 0 {
			t.Errorf("pool credit touched on failure: %+v", a)
		}
	}
}

// TestQoderSendThroughWiredSender: the full wire — store PAT → sender →
// stub — produces an aggregated completion.
func TestQoderSendThroughWiredSender(t *testing.T) {
	dir := t.TempDir()
	writeQoderPAT(t, dir, fakePAT("ACCT3FAKE"))
	store, p, u := qoderWireUp(t, dir)

	sender, ok := adapter.Default.Get(QoderPlatform).(adapter.Sender)
	if !ok {
		t.Fatal("qoder adapter does not implement adapter.Sender")
	}
	acct, err := p.Select(context.Background(), QoderPlatform)
	if err != nil || acct == nil {
		t.Fatalf("Select(qoder) = %v, %v", acct, err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := sender.SendChat(req.Context(), acct, qoderTestChatRequest())
	if err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	if resp == nil || len(resp.Choices) == 0 {
		t.Fatal("no usable response")
	}
	var content string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &content); err != nil || content != "wired" {
		t.Errorf("content = %s (%v), want \"wired\"", resp.Choices[0].Message.Content, err)
	}
	if u.count("chat") != 1 {
		t.Errorf("chat calls = %d, want 1", u.count("chat"))
	}
	_ = store // the store served the credential through the wired source
}

// TestQoderDeviceRotationRoundTrip pins the keepalive persistence: loading a
// device file without a refresh token, then accepting a rotation, rewrites
// the file with the new pair under a STABLE account id (client-id
// fingerprint — the device token itself rotates).
func TestQoderDeviceRotationRoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := `{"device_token":"dt-old","client_id":"cid-1234"}`
	path := filepath.Join(dir, "qoder-device.json")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewCredentialStore(nil)
	if n := s.loadQoderDevice(dir); n != 1 {
		t.Fatalf("load = %d, want 1", n)
	}

	// The account id derives from the client id — stable across rotations.
	cred, ok := s.lookup(credKey(QoderPlatform, patFingerprint("cid-1234")))
	if !ok || cred.AccessToken != "dt-old" || cred.RefreshToken != "" {
		t.Fatalf("initial cred = %+v", cred)
	}

	if err := s.AcceptQoderDeviceRotation(patFingerprint("cid-1234"), "dt-new", "drt-first"); err != nil {
		t.Fatalf("rotation: %v", err)
	}

	cred, ok = s.lookup(credKey(QoderPlatform, patFingerprint("cid-1234")))
	if !ok {
		t.Fatal("account id must stay stable across the rotation")
	}
	if cred.AccessToken != "dt-new" || cred.RefreshToken != "drt-first" {
		t.Fatalf("post-rotation cred = %+v", cred)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		DeviceToken  string `json:"device_token"`
		ClientID     string `json:"client_id"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.DeviceToken != "dt-new" || doc.RefreshToken != "drt-first" || doc.ClientID != "cid-1234" {
		t.Fatalf("file doc = %+v", doc)
	}
}
