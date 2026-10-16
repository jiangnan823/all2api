package router

// sticky.go implements the M10-B2 sticky-session registry (wb2api parity):
// a (caller API key, model) → account binding that lets one caller's session
// keep riding the same upstream account across requests, instead of the
// weighted-random draw re-rolling an account per request. Bindings are
// in-process memory (never persisted, never carries credential material —
// the key is whatever the caller presented at /v1, the value a pool account
// id), expire by TTL, and are consultative: a binding whose account has
// meanwhile cooled, been disabled, fallen under the credit floor, been paused
// or model-locked is dropped and the ordinary Select runs — stickiness is a
// preference, never a bypass of pool health rules.
//
// TTL comes from the frozen config key sticky_session_ttl_min (minutes,
// default 30, 0 = off) and is read hot per dispatch, so a config save takes
// effect on the next request without a restart.

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"all2api/internal/pool"
)

// DefaultSticky is the process-wide registry: /v1 dispatch consults and
// binds into it, GET /api/accounts reads ActiveBindings for the
// sticky_sessions snapshot field. Package-level default follows the
// usage.Default / DefaultModelLocks precedent; tests inject isolated
// registries via Router.sticky.
var DefaultSticky = NewStickyRegistry()

// stickyKey identifies one caller session: the presented /v1 key (the
// gateway api_key in the single-tenant deployment, a caller-distinct string
// if a fronting proxy maps keys) plus the bare model name — the form the
// upstream actually receives, so "workbuddy/deepseek-v4-flash" and
// "deepseek-v4-flash" address one binding, not two.
type stickyKey struct {
	apiKey string
	model  string
}

// stickyBinding is one live binding. The platform rides along because pool
// eligibility (LeaseIfEligible) is platform-scoped and the bare model name
// alone does not always pin a platform (auto-routing).
type stickyBinding struct {
	platform  string
	accountID string
	expiresAt time.Time
}

// StickyRegistry tracks (caller, model) → account bindings with a per-entry
// TTL. All methods are nil-receiver safe, so a Router built without a
// registry degrades to plain pool selection, never a panic.
type StickyRegistry struct {
	mu       sync.Mutex
	bindings map[stickyKey]stickyBinding
	now      func() time.Time // injectable clock for tests
}

// NewStickyRegistry builds an empty registry on the wall clock.
func NewStickyRegistry() *StickyRegistry {
	return &StickyRegistry{
		bindings: make(map[stickyKey]stickyBinding),
		now:      time.Now,
	}
}

// stickyTTL converts the frozen config key onto a duration: minutes ×
// time.Minute; 0 stays 0 (sticky off). Negative configs are treated as off —
// a nonsense value must not extend stickiness, it must disable it.
func stickyTTL(ttlMin int) time.Duration {
	if ttlMin <= 0 {
		return 0
	}
	return time.Duration(ttlMin) * time.Minute
}

// Lookup returns the account bound to (apiKey, model), if the binding exists
// and its TTL still runs. A hit slides the expiry forward (a session in
// active use never expires mid-conversation); a hit on an expired entry is
// dropped on sight. Returns "" when no live binding exists.
func (s *StickyRegistry) Lookup(apiKey, model string, ttl time.Duration) (platform, accountID string) {
	if s == nil || ttl <= 0 {
		return "", ""
	}
	k := stickyKey{apiKey: apiKey, model: model}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[k]
	if !ok {
		return "", ""
	}
	now := s.now()
	if !now.Before(b.expiresAt) {
		delete(s.bindings, k) // TTL ran out: drop on sight
		return "", ""
	}
	b.expiresAt = now.Add(ttl) // sliding TTL: refresh on use
	s.bindings[k] = b
	return b.platform, b.accountID
}

// Bind records (or rebinds) the account a caller's model session rides. The
// latest binding wins: a re-selection overwrites the stale account.
func (s *StickyRegistry) Bind(apiKey, model, platform, accountID string, ttl time.Duration) {
	if s == nil || ttl <= 0 || apiKey == "" || accountID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindings == nil {
		s.bindings = make(map[stickyKey]stickyBinding)
	}
	s.bindings[stickyKey{apiKey: apiKey, model: model}] = stickyBinding{
		platform:  platform,
		accountID: accountID,
		expiresAt: s.now().Add(ttl),
	}
}

// Drop forgets one binding — the consult path calls it when the bound
// account turned out ineligible, so the next request re-selects cleanly.
func (s *StickyRegistry) Drop(apiKey, model string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.bindings, stickyKey{apiKey: apiKey, model: model})
	s.mu.Unlock()
}

// ActiveBindings reports how many bindings are currently live, pruning the
// expired ones first — this is the GET /api/accounts sticky_sessions value.
func (s *StickyRegistry) ActiveBindings() int {
	if s == nil {
		return 0
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, b := range s.bindings {
		if !now.Before(b.expiresAt) {
			delete(s.bindings, k)
		}
	}
	return len(s.bindings)
}

// presentedAPIKey extracts the key the caller presented at /v1 — the same
// two-header precedence authorize() applies (X-API-Key first, then
// Authorization: Bearer). It is the session identity for sticky binding, not
// an auth decision; with api_key unset (open LAN) the presented header (often
// empty) still separates callers that do send one.
func presentedAPIKey(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	return ""
}

// selectAccount is the sticky-aware selection seam of the dispatch loop:
// consult the registry first (a live, eligible, unlocked bound account is
// leased directly — same lease discipline as Select), otherwise fall back to
// the ordinary weighted Select and (re)bind its outcome. model must be the
// bare model name; ttl ≤ 0 (sticky off) short-circuits to plain Select. The
// registry is rt.sticky (nil-receiver safe: no registry → plain Select).
func (rt *Router) selectAccount(r *http.Request, platform, model string, ttl time.Duration) (*pool.Account, error) {
	if ttl > 0 {
		apiKey := presentedAPIKey(r)
		if boundPlatform, id := rt.sticky.Lookup(apiKey, model, ttl); id != "" && boundPlatform == platform {
			// Consultative reuse: the binding is advice — the account must
			// still pass every pool health filter right now, and must not be
			// model-locked for this session's model.
			if acc := rt.host.Pool.LeaseIfEligible(platform, id); acc != nil && !rt.locks.Locked(model, platform, id) {
				return acc, nil
			}
			rt.sticky.Drop(apiKey, model) // stale advice: re-select below
		}
	}
	acc, err := rt.host.Pool.Select(r.Context(), platform)
	if err == nil && acc != nil && ttl > 0 {
		rt.sticky.Bind(presentedAPIKey(r), model, platform, acc.ID, ttl)
	}
	return acc, err
}
