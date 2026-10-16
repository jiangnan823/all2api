package schedule

// deps.go is the dependency-inversion seam for the scheduled task handlers:
// every collaborator — the account batch source, the per-account upstream
// operation, the pool write-back — is a package-local interface, so this
// package never imports internal/adapter, internal/platform/* or
// internal/pool. The real upstream protocols do not exist in this repo and
// must stay uninvented; the integrator (cmd wiring) implements these
// interfaces over adapter.Adapter / pool.WeightedRandomSelector, and
// handlers_test.go implements them over deterministic fakes.

import (
	"context"
	"errors"
	"time"
)

// Account is the handler-side snapshot of a pooled account: the identity and
// lifecycle fields of pool.Account, mirrored locally to keep this package
// pool-free. Handlers only read it; every mutation goes through AccountStore.
type Account struct {
	ID          string    `json:"id"`
	Platform    string    `json:"platform"` // source namespace: "workbuddy", "qoder", "trae"
	Status      string    `json:"status"`   // AccountActive / AccountCooling / AccountDisabled
	CooldownEnd time.Time `json:"cooldown_end,omitempty"`
}

// Account lifecycle statuses — the raw string values of pool.Account.Status,
// mirrored locally (same values, no import).
const (
	AccountActive   = "active"
	AccountCooling  = "cooling"
	AccountDisabled = "disabled"
)

// IsCooling reports whether the account is still inside its cooldown window
// (pool.Account.IsCooling parity).
func (a *Account) IsCooling() bool {
	return !a.CooldownEnd.IsZero() && time.Now().Before(a.CooldownEnd)
}

// AccountSource lists the account batch for one task run. taskType is one of
// the Task* name constants ("signin", ...), letting the implementation filter
// by platform capability (e.g. cat_travel is a workbuddy-only task).
type AccountSource interface {
	Accounts(ctx context.Context, taskType string) ([]*Account, error)
}

// Upstream performs the platform operation for exactly one account. Handlers
// add no retries of their own — one call, one outcome. Wrap the sentinel with
// %w / fmt.Errorf to classify; errors.Is is what the handlers use.
type Upstream interface {
	// Signin executes the daily sign-in (calendar streak + credit reward).
	Signin(ctx context.Context, acc *Account) (SigninResult, error)
	// ActiveCheck executes the active-report + streak self-check.
	ActiveCheck(ctx context.Context, acc *Account) error
	// CatTravel executes the twice-daily cat_travel quota task (喂猫/猫猫旅行).
	CatTravel(ctx context.Context, acc *Account) (CatTravelResult, error)
	// TokenKeepalive refreshes the account credential (token 保活).
	TokenKeepalive(ctx context.Context, acc *Account) (KeepaliveResult, error)
}

// SigninResult carries what a successful sign-in awards.
type SigninResult struct {
	CreditDelta int64 // awarded points (>= 0)
}

// CatTravelResult carries the awarded quota of one cat_travel run.
type CatTravelResult struct {
	CreditDelta int64 // awarded points (>= 0)
}

// KeepaliveResult carries the refreshed credential expiry.
type KeepaliveResult struct {
	ExpiresAt time.Time // zero = platform-managed expiry, nothing to write back
}

// AccountStore persists task outcomes back into the account pool (status /
// credit / cooldown / expiry). Implementations map onto
// pool.WeightedRandomSelector operations.
type AccountStore interface {
	// AddCredit applies a signed credit delta to the account.
	AddCredit(ctx context.Context, platform, accountID string, delta int64) error
	// SetStatus writes a lifecycle status (the Account* constants).
	SetStatus(ctx context.Context, platform, accountID, status string) error
	// SetCooldown puts the account into cooldown until the given time and
	// sets its status to cooling (pool.TriggerCooldown parity).
	SetCooldown(ctx context.Context, platform, accountID string, until time.Time) error
	// SetExpiry records a refreshed credential expiry (token_keepalive).
	SetExpiry(ctx context.Context, platform, accountID string, at time.Time) error
}

// Sentinel causes Upstream implementations signal. errors.Is-compatible
// wrapping is fine.
var (
	// ErrCredentialExpired: the account's credential is dead — the handler
	// disables the account instead of cooling it (pool auth_error parity).
	ErrCredentialExpired = errors.New("credential expired")

	// ErrUnsupportedTask: the account's platform has no such task (e.g.
	// cat_travel on qoder) — the account is counted skipped and nothing is
	// written back.
	ErrUnsupportedTask = errors.New("task unsupported for platform")
)

// Deps carries every collaborator the task handlers need. Filled by the
// integrator (cmd wiring); this package only consumes it.
type Deps struct {
	// Source lists the per-run account batch.
	Source AccountSource

	// Upstream performs the per-account platform operation.
	Upstream Upstream

	// Store persists outcomes back into the pool.
	Store AccountStore

	// FailureCooldown is how long a failed account cools down before its next
	// scheduled attempt (config pool.cooldown_sec). Non-positive values fall
	// back to DefaultFailureCooldown.
	FailureCooldown time.Duration

	// Crons optionally overrides the code-truth cron specs — filled from
	// config.Schedule by the integrator. See Crons for the cat_travel gap.
	Crons Crons

	// ReportStats, when non-nil, receives the per-run counters after every
	// batch, including error-aborted ones. Called inline — must not block.
	ReportStats func(RunStats)
}

// Crons maps each configurable task to its cron spec, as filled from
// config.Schedule. Only three keys exist there — signin_cron → Signin,
// task_cron → ActiveCheck (the "0 10 * * *" daily slot), token_keepalive_cron
// → TokenKeepalive — and that mapping is recorded here as-is. (M10-B2 update:
// per-platform cron keys now exist — schedule.signin_cron_<platform> et al and
// the workbuddy-only cat_travel_cron; they ride PlatformCrons and
// BuildPlatformTasks below, while this struct stays the legacy global path
// BuildDefaultTasks serves. cat_travel gained its first config key there.)
type Crons struct {
	Signin         string // config.Schedule.SigninCron
	ActiveCheck    string // config.Schedule.TaskCron
	TokenKeepalive string // config.Schedule.TokenKeepalive
}

// PlatformCrons carries one platform's resolved cron specs for the M10-B2
// per-platform task registration: four tasks (cat_travel only where the
// platform actually offers it — workbuddy). Values are the config-resolved
// specs (empty platform key already fell back to the global key upstream, in
// host.SchedulePlatformTasks); "" lets BuildPlatformTasks apply the code-truth
// default, which is what cat_travel does when its key is unset.
type PlatformCrons struct {
	Platform       string
	Signin         string
	ActiveCheck    string
	TokenKeepalive string
	CatTravel      string
}
