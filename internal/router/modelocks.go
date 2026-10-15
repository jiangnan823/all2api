package router

// modelocks.go implements the M9b model-level rate-limit lock registry: the
// gateway's own routing-layer observation of upstream 429s, keyed per
// (model, account) pair. This is deliberately NOT upstream quota data — the
// only writer is the dispatch path (recordSendOutcome) when a classified
// UpstreamError reports 429. Locks live in memory, expire by TTL (upstream
// Retry-After hint when recoverable from the error message, else a default
// cooldown matching the pool's own rate_limited account cooldown), and are
// skipped by Select so a locked pair never burns a send attempt.
//
// Relationship to the platform circuit breaker (§9.21-e.2): the breaker stays
// authoritative for per-source open/half-open recovery and is untouched; the
// registry adds a finer grain (one model × one account) on top. A platform
// with no breaker stanza still books model locks.

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"all2api/internal/adapter"
)

// DefaultModelLocks is the process-wide registry: /v1 dispatch books into it
// and the panel surface (GET /api/model-locks) reads its aggregate view.
// Package-level default follows the usage.Default precedent; tests inject
// isolated registries via the Router.locks field instead.
var DefaultModelLocks = NewModelLockRegistry()

const (
	// DefaultModelLockTTL is the cooldown applied to a 429 lock whose upstream
	// error carried no recoverable retry hint. Matches the pool's own
	// rate_limited account cooldown (UpdateHealth → TriggerCooldown 2m), so
	// both layers speak the same "one rate-limit hit = two quiet minutes".
	DefaultModelLockTTL = 2 * time.Minute

	// maxModelLockTTL clamps upstream retry hints to the same 2h horizon the
	// platform clients apply when parsing Retry-After headers — a bogus or
	// pathological hint must not lock a model away for good.
	maxModelLockTTL = 2 * time.Hour

	// ModelLockReason is the frozen wire reason every /api/model-locks row
	// carries: locks are only ever created from 429 observations.
	ModelLockReason = "429 rate limit"
)

// modelLockKey identifies one locked (model, platform, account) triple. The
// account id is platform-scoped in the pool, so the platform belongs in the
// key even though ids happen to be globally unique today.
type modelLockKey struct {
	model    string
	platform string
	account  string
}

// modelLock is one live observation. reason is kept per entry (not per model)
// so a future non-429 lock source can coexist without a schema change.
type modelLock struct {
	unlockAt time.Time
	reason   string
}

// ModelLockRegistry tracks per-(model, account) 429 locks and answers the
// two consumer questions: "may this pair send?" (Locked) and "what does the
// panel show?" (Snapshot). All methods are nil-receiver safe so a Router
// built without a registry degrades to "no model locks", never a panic.
type ModelLockRegistry struct {
	mu         sync.Mutex
	locks      map[modelLockKey]modelLock
	defaultTTL time.Duration
	now        func() time.Time // injectable clock for tests
}

// NewModelLockRegistry builds an empty registry on the wall clock.
func NewModelLockRegistry() *ModelLockRegistry {
	return &ModelLockRegistry{
		locks:      make(map[modelLockKey]modelLock),
		defaultTTL: DefaultModelLockTTL,
		now:        time.Now,
	}
}

// NoteRateLimited books (or re-books) a 429 lock for the pair. retryAfter is
// the upstream hint recovered from the error (retryHintFromError); values
// ≤ 0 fall back to DefaultModelLockTTL and anything beyond maxModelLockTTL is
// clamped. The latest observation wins: re-booking overwrites the previous
// unlock time, so a fresh 429 with a shorter hint can shorten an older lock.
func (m *ModelLockRegistry) NoteRateLimited(model, platform, account string, retryAfter time.Duration) {
	if m == nil {
		return
	}
	ttl := m.defaultTTL
	if retryAfter > 0 {
		ttl = retryAfter
		if ttl > maxModelLockTTL {
			ttl = maxModelLockTTL
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = make(map[modelLockKey]modelLock)
	}
	m.locks[modelLockKey{model: model, platform: platform, account: account}] = modelLock{
		unlockAt: m.now().Add(ttl),
		reason:   ModelLockReason,
	}
}

// Locked reports whether the pair is currently locked, expiring (auto-
// unlocking) an entry whose TTL has run out on sight. The dispatch loop calls
// this per selected account; a true answer means "skip without a send".
func (m *ModelLockRegistry) Locked(model, platform, account string) bool {
	if m == nil {
		return false
	}
	k := modelLockKey{model: model, platform: platform, account: account}
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[k]
	if !ok {
		return false
	}
	if !m.now().Before(l.unlockAt) {
		delete(m.locks, k) // TTL ran out: auto-unlock
		return false
	}
	return true
}

// ModelLockAggregate is the panel-facing view of one (model, platform) group:
// how many of the platform's accounts are locked for this model, and when the
// first of them unlocks. TotalAccounts is intentionally absent — it is pool
// state, joined by the wire layer (api.handleModelLocks), not registry state.
type ModelLockAggregate struct {
	Model          string
	Platform       string
	LockedAccounts int
	EarliestUnlock time.Time // zero only in the impossible empty-group case
	Reason         string
}

// Snapshot returns the aggregate per-(model, platform) view, pruning expired
// locks and sorting deterministically by model then platform, so the panel
// table never reshuffles between polls. The returned slice shares nothing
// with the registry.
func (m *ModelLockRegistry) Snapshot() []ModelLockAggregate {
	if m == nil {
		return nil
	}
	now := m.now()
	type aggKey struct{ model, platform string }
	m.mu.Lock()
	aggs := make(map[aggKey]*ModelLockAggregate)
	for k, l := range m.locks {
		if !now.Before(l.unlockAt) {
			delete(m.locks, k) // prune expired so the panel never lists ghosts
			continue
		}
		ak := aggKey{model: k.model, platform: k.platform}
		a := aggs[ak]
		if a == nil {
			a = &ModelLockAggregate{Model: k.model, Platform: k.platform, Reason: l.reason}
			aggs[ak] = a
		}
		a.LockedAccounts++
		if a.EarliestUnlock.IsZero() || l.unlockAt.Before(a.EarliestUnlock) {
			a.EarliestUnlock = l.unlockAt
		}
	}
	m.mu.Unlock()

	out := make([]ModelLockAggregate, 0, len(aggs))
	for _, a := range aggs {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Platform < out[j].Platform
	})
	return out
}

// retryHintFromError best-effort recovers the upstream retry hint from a
// classified send error. The adapter.UpstreamError wire type carries only
// Status/Reason/Message, and the platform clients fold their parsed
// Retry-After into the message as " (retry after <go-duration>)" — both the
// workbuddy (rateLimitedError) and qoder (busyError) builders use exactly
// that phrasing, so parsing it here stays inside the frozen adapter contract.
// Anything else (no hint, unparseable, non-429, non-UpstreamError) returns 0
// and the caller falls back to the default cooldown.
func retryHintFromError(err error) time.Duration {
	if err == nil {
		return 0
	}
	var ue *adapter.UpstreamError
	if !errors.As(err, &ue) {
		return 0
	}
	if ue.Reason != adapter.ReasonRateLimited && ue.Status != http.StatusTooManyRequests {
		return 0
	}
	const marker = "retry after "
	i := strings.Index(ue.Message, marker)
	if i < 0 {
		return 0
	}
	rest := ue.Message[i+len(marker):]
	if j := strings.IndexByte(rest, ')'); j >= 0 {
		rest = rest[:j]
	}
	d, perr := time.ParseDuration(strings.TrimSpace(rest))
	if perr != nil || d <= 0 {
		return 0
	}
	return d
}
