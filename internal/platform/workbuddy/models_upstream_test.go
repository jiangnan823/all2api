//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package workbuddy

// Model-catalog tests (models_upstream.go) run exclusively against httptest
// local stubs — no real endpoint is ever contacted (task red line). Stub
// replies use the response shape extracted from the wb2api reference
// implementation (dynModelEntry field names: credits/maxInputTokens/
// maxOutputTokens/supportsToolCall/reasoning.supportedEfforts/...), which the
// protocol report §2 records as the verified /v2/enterprises/*/models shape.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// catalogStub counts directory hits and scripts per-path replies.
type catalogStub struct {
	mu    sync.Mutex
	calls map[string]int
	auths map[string]string // path → last Authorization header
	reply map[string]catalogReply
}

type catalogReply struct {
	status int // 0 → 200
	body   string
}

func (s *catalogStub) script(path string, r catalogReply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reply == nil {
		s.reply = map[string]catalogReply{}
	}
	s.reply[path] = r
}

func (s *catalogStub) hit(path string) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path], s.auths[path]
}

func (s *catalogStub) handler() http.Handler {
	s.mu.Lock()
	s.calls = map[string]int{}
	s.auths = map[string]string{}
	s.mu.Unlock()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls[r.URL.Path]++
		s.auths[r.URL.Path] = r.Header.Get("Authorization")
		s.mu.Unlock()
		rep, ok := s.reply[r.URL.Path]
		if !ok {
			rep = catalogReply{status: 200, body: catalogFixtureBody}
		}
		if rep.status == 0 {
			rep.status = 200
		}
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	})
}

// catalogFixtureBody mirrors the wb2api dynModelEntry field names: one full
// entry, one disabled, one id-fallback (modelId only), one credits-as-"0x"
// limited-free badge entry.
const catalogFixtureBody = `{"code":0,"msg":"ok","data":{"models":[` +
	`{"id":"deepseek-v4-flash","name":"DeepSeek V4 Flash","descriptionZh":"旗舰快思考","credits":"x0.05 credits","vendor":"deepseek","isDefault":true,` +
	`"maxInputTokens":128000,"maxOutputTokens":32768,"supportsToolCall":true,"supportsReasoning":true,` +
	`"reasoning":{"effort":"high","defaultEffort":"low","canDisableThinking":true,"supportedEfforts":["low","high"]}},` +
	`{"id":"nes-embed-1","name":"Embed","maxInputTokens":8192,"disabled":true},` +
	`{"modelId":"fallback-only","name":"Fallback","credits":"x0.29","maxInputTokens":65536},` +
	`{"id":"glm-free","name":"GLM Free","credits":"0x","tags":["badge:限时免费"],"maxInputTokens":200000,"maxOutputTokens":8192,"reasoning":{"summary":"auto"}},` +
	`{"id":"no-credits","name":"No Credits"}` +
	`]}}`

// newCatalogStub serves the fake upstream and returns a task client pointed
// at it plus a personal-realm credential (empty EnterpriseID → the personal
// path).
func newCatalogStub(t *testing.T) (*catalogStub, *TaskClient) {
	t.Helper()
	stub := &catalogStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	client := NewTaskClient(TaskClientOptions{
		ChatBaseURL: srv.URL,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
	})
	return stub, client
}

func catalogCred(enterprise string) *Credential {
	return &Credential{
		AccessToken:  "catalog-access",
		RefreshToken: "catalog-refresh",
		UID:          "uid-1",
		EnterpriseID: enterprise,
		Domain:       "www.codebuddy.cn",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
}

// ---------- TaskClient.ModelCatalog: fetch + parse ----------

func TestModelCatalogParsesUpstreamEntries(t *testing.T) {
	_, client := newCatalogStub(t)

	entries, err := client.ModelCatalog(context.Background(), catalogCred(""))
	if err != nil {
		t.Fatalf("ModelCatalog: %v", err)
	}
	// disabled entries are skipped; the modelId-only fallback stays.
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4 (5 served, 1 disabled): %+v", len(entries), entries)
	}

	first := entries[0]
	if first.Model != "deepseek-v4-flash" || first.Name != "DeepSeek V4 Flash" {
		t.Fatalf("first entry id/name = %q/%q", first.Model, first.Name)
	}
	if first.ContextSize != 128000 || first.MaxOutput != 32768 {
		t.Fatalf("first entry windows = %d/%d, want 128000/32768", first.ContextSize, first.MaxOutput)
	}
	if !first.SupportsTool || !first.SupportsStream {
		t.Fatalf("first entry tool/stream = %v/%v, want true/true", first.SupportsTool, first.SupportsStream)
	}
	if first.CreditMultiplier == nil || *first.CreditMultiplier != 0.05 {
		t.Fatalf("first entry credit multiplier = %v, want 0.05", first.CreditMultiplier)
	}
	if !first.IsDefault {
		t.Fatal("first entry carries isDefault:true — must map through")
	}
	if first.IsFree {
		t.Fatal("first entry carries no free badge — IsFree must stay false")
	}
	if first.DefaultEffort != "low" {
		t.Fatalf("defaultEffort = %q, want low (new key wins over legacy effort)", first.DefaultEffort)
	}
	if strings.Join(first.ThinkingTiers, ",") != "low,high" {
		t.Fatalf("thinking tiers = %v, want [low high]", first.ThinkingTiers)
	}
	if !first.CanDisableThinking || !first.SupportsReasoning {
		t.Fatalf("reasoning flags = %v/%v, want true/true", first.SupportsReasoning, first.CanDisableThinking)
	}
	if first.Description != "旗舰快思考" {
		t.Fatalf("descriptionZh = %q", first.Description)
	}

	// modelId-only entry: id fallback chain, no invented facts.
	fb := entries[1]
	if fb.Model != "fallback-only" {
		t.Fatalf("fallback entry id = %q, want fallback-only", fb.Model)
	}
	if fb.CreditMultiplier == nil || *fb.CreditMultiplier != 0.29 {
		t.Fatalf("fallback multiplier = %v, want 0.29", fb.CreditMultiplier)
	}
	if fb.ThinkingTiers != nil || fb.DefaultEffort != "" {
		t.Fatalf("fallback tiers/effort = %v/%q, want nil/empty", fb.ThinkingTiers, fb.DefaultEffort)
	}

	// credits "0x" → multiplier 0 (limited-free pricing), tiers absent → nil.
	free := entries[2]
	if free.CreditMultiplier == nil || *free.CreditMultiplier != 0 {
		t.Fatalf("free multiplier = %v, want 0", free.CreditMultiplier)
	}
	if !free.IsFree {
		t.Fatalf("badge:限时免费 entry must set IsFree: %+v", free)
	}
	if free.ThinkingTiers != nil {
		t.Fatalf("free tiers = %v, want nil", free.ThinkingTiers)
	}

	// no credits at all → nil multiplier (null on the wire), never 0.
	if entries[3].CreditMultiplier != nil {
		t.Fatalf("creditless multiplier = %v, want nil", entries[3].CreditMultiplier)
	}
}

func TestModelCatalogPersonalAndEnterprisePaths(t *testing.T) {
	stub, client := newCatalogStub(t)
	ctx := context.Background()

	if _, err := client.ModelCatalog(ctx, catalogCred("")); err != nil {
		t.Fatalf("personal fetch: %v", err)
	}
	if n, _ := stub.hit("/v2/enterprises/personal/models"); n != 1 {
		t.Fatalf("personal path hits = %d, want 1", n)
	}
	if _, err := client.ModelCatalog(ctx, catalogCred("ent-42")); err != nil {
		t.Fatalf("enterprise fetch: %v", err)
	}
	if n, auth := stub.hit("/v2/enterprises/ent-42/models"); n != 1 {
		t.Fatalf("enterprise path hits = %d, want 1", n)
	} else if auth != "Bearer catalog-access" {
		t.Fatalf("enterprise auth = %q, want Bearer token", auth)
	}
}

func TestModelCatalogErrorsAreExplicit(t *testing.T) {
	stub, client := newCatalogStub(t)
	ctx := context.Background()

	// non-zero business envelope → classified error (do() rejects it).
	stub.script("/v2/enterprises/personal/models", catalogReply{status: 200, body: `{"code":1102,"msg":"denied"}`})
	if _, err := client.ModelCatalog(ctx, catalogCred("")); err == nil {
		t.Fatal("code=1102 envelope must error")
	}

	// HTTP failure surfaces as an error.
	stub.script("/v2/enterprises/personal/models", catalogReply{status: 500, body: `{"msg":"boom"}`})
	if _, err := client.ModelCatalog(ctx, catalogCred("")); err == nil {
		t.Fatal("HTTP 500 must error")
	}

	// empty model table is an explicit error, not an empty success.
	stub.script("/v2/enterprises/personal/models", catalogReply{status: 200, body: `{"code":0,"msg":"ok","data":{"models":[]}}`})
	if _, err := client.ModelCatalog(ctx, catalogCred("")); err == nil {
		t.Fatal("empty models table must error")
	}

	// all entries unusable (no id anywhere in the fallback chain / disabled)
	// is an error too.
	stub.script("/v2/enterprises/personal/models", catalogReply{status: 200, body: `{"code":0,"msg":"ok","data":{"models":[{"id":"","name":""},{"id":"gone","disabled":true}]}}`})
	if _, err := client.ModelCatalog(ctx, catalogCred("")); err == nil {
		t.Fatal("all-unusable table must error")
	}

	// malformed JSON.
	stub.script("/v2/enterprises/personal/models", catalogReply{status: 200, body: `not-json`})
	if _, err := client.ModelCatalog(ctx, catalogCred("")); err == nil {
		t.Fatal("malformed body must error")
	}

	// no credential → no call at all.
	if _, err := client.ModelCatalog(ctx, nil); err == nil {
		t.Fatal("nil credential must error")
	}
}

// ---------- ModelCatalog.Get: TTL cache + failure cooldown ----------

func TestModelCatalogCacheTTLAndRefresh(t *testing.T) {
	stub, client := newCatalogStub(t)
	now := time.Unix(1800000000, 0)
	client.opts.Now = func() time.Time { return now }
	cat := NewModelCatalog(client)
	cred := catalogCred("")
	ctx := context.Background()

	// First call fetches; second within the TTL is served from cache.
	if _, ok := cat.Get(ctx, cred, false); !ok {
		t.Fatal("first Get must succeed")
	}
	if _, ok := cat.Get(ctx, cred, false); !ok {
		t.Fatal("cached Get must succeed")
	}
	if n, _ := stub.hit("/v2/enterprises/personal/models"); n != 1 {
		t.Fatalf("upstream hits within TTL = %d, want 1", n)
	}

	// Past the TTL the next Get refetches.
	now = now.Add(catalogTTL + time.Second)
	if _, ok := cat.Get(ctx, cred, false); !ok {
		t.Fatal("post-TTL Get must succeed")
	}
	if n, _ := stub.hit("/v2/enterprises/personal/models"); n != 2 {
		t.Fatalf("upstream hits after TTL = %d, want 2", n)
	}

	// refresh=1 forces a live fetch even inside the TTL window.
	if _, ok := cat.Get(ctx, cred, true); !ok {
		t.Fatal("refresh Get must succeed")
	}
	if n, _ := stub.hit("/v2/enterprises/personal/models"); n != 3 {
		t.Fatalf("upstream hits after refresh = %d, want 3", n)
	}

	// A nil credential is a cache-neutral miss (no upstream call).
	if _, ok := cat.Get(ctx, nil, false); ok {
		t.Fatal("nil credential must miss")
	}
	if n, _ := stub.hit("/v2/enterprises/personal/models"); n != 3 {
		t.Fatalf("nil-credential path must not hit upstream, got %d", n)
	}
}

func TestModelCatalogFailureFallsBackWithinCooldown(t *testing.T) {
	stub, client := newCatalogStub(t)
	now := time.Unix(1800000000, 0)
	client.opts.Now = func() time.Time { return now }
	cat := NewModelCatalog(client)
	cred := catalogCred("")
	ctx := context.Background()

	stub.script("/v2/enterprises/personal/models", catalogReply{status: 503, body: `{"msg":"down"}`})
	if _, ok := cat.Get(ctx, cred, false); ok {
		t.Fatal("upstream 503 must miss")
	}
	if _, ok := cat.Get(ctx, cred, false); ok {
		t.Fatal("cooldown miss")
	}
	if n, _ := stub.hit("/v2/enterprises/personal/models"); n != 1 {
		t.Fatalf("upstream hits within failure cooldown = %d, want 1", n)
	}

	// refresh=1 bypasses the cooldown (explicit force) — and its failure
	// refreshes the cooldown timestamp.
	now = now.Add(catalogTTL + time.Second)
	if _, ok := cat.Get(ctx, cred, true); ok {
		t.Fatal("forced refresh against a down upstream must miss")
	}

	// Recovery: upstream healthy again → success re-cached.
	stub.script("/v2/enterprises/personal/models", catalogReply{status: 200, body: catalogFixtureBody})
	now = now.Add(catalogTTL + time.Second)
	entries, ok := cat.Get(ctx, cred, true)
	if !ok || len(entries) == 0 {
		t.Fatalf("recovery Get = %d entries, %v; want entries, ok", len(entries), ok)
	}
	// And the recovered cache serves the TTL window without new calls.
	if _, ok := cat.Get(ctx, cred, false); !ok {
		t.Fatal("post-recovery cached Get must succeed")
	}
	// Hit ledger: initial 503 (1), forced refresh against the down upstream
	// (2), recovery fetch (3); the cached tail adds none.
	if n, _ := stub.hit("/v2/enterprises/personal/models"); n != 3 {
		t.Fatalf("total upstream hits = %d, want 3", n)
	}
}

// ---------- parseCreditMultiplier ----------

func TestParseCreditMultiplier(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
		ok   bool
	}{
		{"x0.05 credits", 0.05, true},
		{"x0.29", 0.29, true},
		{"0x", 0, true},
		{"x1.0 credits", 1, true},
		{"  x2.5  ", 2.5, true},
		{"", 0, false},
		{"credits", 0, false},
		{"免费", 0, false},
		{".", 0, false},
	}
	for _, c := range cases {
		got, ok := parseCreditMultiplier(c.raw)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("parseCreditMultiplier(%q) = %v, %v; want %v, %v", c.raw, got, ok, c.want, c.ok)
		}
	}
}
