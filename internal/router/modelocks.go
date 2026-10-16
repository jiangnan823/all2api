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
	"all2api/internal/pool"
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
	// carries: locks are only ever created from 429 observations. Aliases
	// pool.CooldownReasonRateLimit so both panel surfaces spell the phrase
	// identically from one source of truth.
	ModelLockReason = pool.CooldownReasonRateLimit
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
// consumer questions: "may this pair send?" (Locked), "what does the panel
// show?" (Snapshot, with the M10 availability columns joined from the bound
// pool view), and "clear the pool" (Unlock / UnlockAll). All methods are
// nil-receiver safe so a Router built without a registry degrades to "no
// model locks", never a panic.
type ModelLockRegistry struct {
	mu         sync.Mutex
	locks      map[modelLockKey]modelLock
	defaultTTL time.Duration
	now        func() time.Time // injectable clock for tests

	// pool is the optional read-only account-state view behind the M10
	// available/total columns (BindPool). nil = no join source: the
	// aggregates keep zeroed availability columns instead of guessing.
	pool poolView
}

// poolView is the read-only pool face the registry needs for the M10
// available/total columns: which accounts exist per platform and whether they
// look selectable right now. *pool.WeightedRandomSelector satisfies it via
// SnapshotAccounts; router tests inject fakes through the unexported field.
type poolView interface {
	SnapshotAccounts() []pool.Account
}

// NewModelLockRegistry builds an empty registry on the wall clock.
func NewModelLockRegistry() *ModelLockRegistry {
	return &ModelLockRegistry{
		locks:      make(map[modelLockKey]modelLock),
		defaultTTL: DefaultModelLockTTL,
		now:        time.Now,
	}
}

// BindPool attaches the availability-join source for the M10 available/total
// columns. The concrete parameter keeps a typed-nil *pool.WeightedRandomSelector
// out of the interface (the models_wire.go constructor guards the same trap):
// a nil selector binds nothing and the aggregates report zeroed availability
// rather than panicking. Called at panel-handler construction; safe to re-call.
func (m *ModelLockRegistry) BindPool(p *pool.WeightedRandomSelector) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p == nil {
		m.pool = nil
		return
	}
	m.pool = p
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
// first of them unlocks. M10-B1 adds the availability columns: Available counts
// the platform accounts the pool would select right now (active, not paused,
// not cooling) minus the ones this model itself has locked, and Total is
// Available + LockedAccounts — the model's usable breadth, not the raw pool
// size. Both stay 0 when no pool view is bound (BindPool); the legacy wire
// total_accounts keeps being joined by the wire layer (api.handleModelLocks).
type ModelLockAggregate struct {
	Model          string
	Platform       string
	LockedAccounts int
	EarliestUnlock time.Time // zero only in the impossible empty-group case
	Reason         string

	Available int // selectable now and not locked for this model (needs a bound pool view)
	Total     int // Available + LockedAccounts
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
	lockedBy := make(map[aggKey]map[string]bool)
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
		if lockedBy[ak] == nil {
			lockedBy[ak] = make(map[string]bool)
		}
		lockedBy[ak][k.account] = true
		if a.EarliestUnlock.IsZero() || l.unlockAt.Before(a.EarliestUnlock) {
			a.EarliestUnlock = l.unlockAt
		}
	}
	pv := m.pool
	m.mu.Unlock()

	// Availability join: one pool snapshot serves every group. selectable
	// remembers, per platform, which account ids the pool would hand out right
	// now — Select's eligibility (active, not paused, not cooling) minus the
	// anti-thundering lease, which lasts one in-flight send and must not make
	// the panel flicker. The pool's credit floor stays pool-private (the
	// snapshot carries no floor); availability here is the lock layer's view.
	if pv != nil {
		selectable := make(map[string]map[string]bool)
		for _, acc := range pv.SnapshotAccounts() {
			if acc.Status != "active" || acc.Paused {
				continue
			}
			if !acc.CooldownEnd.IsZero() && now.Before(acc.CooldownEnd) {
				continue
			}
			if selectable[acc.Platform] == nil {
				selectable[acc.Platform] = make(map[string]bool)
			}
			selectable[acc.Platform][acc.ID] = true
		}
		for ak, a := range aggs {
			for id := range selectable[a.Platform] {
				if !lockedBy[ak][id] {
					a.Available++
				}
			}
			a.Total = a.Available + a.LockedAccounts
		}
	}

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

// Unlock drops every lock booked for the model (all platforms, all accounts)
// and returns how many live locks it removed. Expired entries in the same
// family are pruned silently — the TTL already released them, so counting
// them would overstate the unlock.
func (m *ModelLockRegistry) Unlock(model string) int {
	if m == nil {
		return 0
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, l := range m.locks {
		if k.model != model {
			continue
		}
		delete(m.locks, k)
		if now.Before(l.unlockAt) {
			n++
		}
	}
	return n
}

// UnlockAll is Unlock across every model. platform non-empty scopes the clear
// to one platform's locks — the panel's per-platform「全部解锁」must not reach
// into other platforms' locks (M10 review scope-leak fix). Empty platform
// clears everything (whole-pool action).
func (m *ModelLockRegistry) UnlockAll(platform string) int {
	if m == nil {
		return 0
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, l := range m.locks {
		if platform != "" && k.platform != platform {
			continue
		}
		delete(m.locks, k)
		if now.Before(l.unlockAt) {
			n++
		}
	}
	return n
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
