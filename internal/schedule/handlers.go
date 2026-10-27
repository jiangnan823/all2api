package schedule

// handlers.go implements the four scheduled task handlers declared by the
// schedule.go package header — signin(09/21), active_check(10),
// cat_travel(09/21), token_keepalive(22) — over the deps.go seam. Every
// handler walks the account batch once, isolates per-account failures (one
// broken account never aborts the batch), writes outcomes back through
// AccountStore, and reports counters through Deps.ReportStats.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"all2api/internal/adapter"
)

// Task names — the stable registration keys shared by Scheduler.AddTask and
// Scheduler.RegisterHandler (and what AccountSource receives as taskType).
const (
	TaskSignin         = "signin"
	TaskActiveCheck    = "active_check"
	TaskCatTravel      = "cat_travel"
	TaskTokenKeepalive = "token_keepalive"
)

// Code-truth cron specs, from the schedule.go package header:
// signin(09/21), active_check(10), cat_travel(09/21), token_keepalive(22).
// The first, second and fourth can be overridden via Deps.Crons
// (config.Schedule); cat_travel has no config key (see Crons).
const (
	DefaultSigninCron         = "0 9,21 * * *"
	DefaultActiveCheckCron    = "0 10 * * *"
	DefaultCatTravelCron      = "0 9,21 * * *"
	DefaultTokenKeepaliveCron = "0 22 * * *" // #nosec G101 -- 值为 cron 表达式，键名属契约冻结面不可改名（同 config.go TokenKeepalive 先例）
)

// DefaultFailureCooldown mirrors the config pool.cooldown_sec default (60s).
const DefaultFailureCooldown = 60 * time.Second

// RunStats summarizes one task run across its account batch. Per-account
// failures never abort the batch — they are counted here for the upper layer
// (logging / panel), while the Task-level status only turns failed on
// batch-level errors (nil deps, source listing failure, ctx cancellation).
type RunStats struct {
	Task    string `json:"task"`
	Total   int    `json:"total"`
	Success int    `json:"success"`
	Failed  int    `json:"failed"`
	Skipped int    `json:"skipped"`
}

// resolveCtx picks the per-run context from the scheduler's untyped handler
// argument: a real context wins (forward-compatible with a scheduler revision
// that forwards one), anything else — schedule.go today invokes handlers as
// handler(nil) — falls back to context.Background. Shutdown cancellation is
// injected on top of that by RegisterDefaultHandlers/injectContext.
func resolveCtx(v interface{}) context.Context {
	if c, ok := v.(context.Context); ok && c != nil {
		return c
	}
	return context.Background()
}

// accountOp is the per-task, per-account behavior plug. skip decides whether
// an account participates in this run; run performs the upstream operation
// plus every write-back for that one account and returns non-nil on failure
// of either.
type accountOp struct {
	skip func(acc *Account) bool
	run  func(ctx context.Context, acc *Account) error
}

// handlerFor shapes one task handler out of the shared batch loop.
func handlerFor(taskName string, d *Deps, op accountOp) TaskHandler {
	return func(v interface{}) error {
		ctx := resolveCtx(v)
		stats := RunStats{Task: taskName}

		err := runAccounts(ctx, taskName, d, op, &stats)
		if d != nil && d.ReportStats != nil {
			d.ReportStats(stats)
		}
		return err
	}
}

// runAccounts is the shared batch loop: list accounts, then per account —
// ctx gate, skip predicate, upstream op + write-back. Error contract:
//   - nil deps / nil dependency field / source listing error → returned
//     (Task.Status turns failed, remaining accounts untouched);
//   - per-account errors → counted Failed, batch continues;
//   - ErrUnsupportedTask from the upstream op → counted Skipped, no
//     write-back, batch continues;
//   - ctx cancelled between accounts → ctx.Err() returned immediately.
func runAccounts(ctx context.Context, taskName string, d *Deps, op accountOp, stats *RunStats) error {
	if err := validateDeps(taskName, d); err != nil {
		return err
	}

	accounts, err := d.Source.Accounts(ctx, taskName)
	if err != nil {
		return fmt.Errorf("schedule %s: list accounts: %w", taskName, err)
	}
	stats.Total = len(accounts)

	for _, acc := range accounts {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if op.skip != nil && op.skip(acc) {
			stats.Skipped++
			continue
		}
		if runErr := op.run(ctx, acc); runErr != nil {
			if errors.Is(runErr, ErrUnsupportedTask) {
				stats.Skipped++
				continue
			}
			stats.Failed++
			continue
		}
		stats.Success++
	}
	return nil
}

// validateDeps names the first nil dependency so a wiring hole surfaces as
// Task.Status=failed with an actionable message instead of a panic.
func validateDeps(taskName string, d *Deps) error {
	if d == nil {
		return fmt.Errorf("schedule %s: deps is nil", taskName)
	}
	var missing []string
	if d.Source == nil {
		missing = append(missing, "Source")
	}
	if d.Upstream == nil {
		missing = append(missing, "Upstream")
	}
	if d.Store == nil {
		missing = append(missing, "Store")
	}
	if len(missing) > 0 {
		return fmt.Errorf("schedule %s: nil dep(s): %s", taskName, strings.Join(missing, ", "))
	}
	return nil
}

// skipDisabledOrCooling excludes accounts that are manually disabled or still
// in a rate-limit cooldown — neither should receive scheduled task traffic.
func skipDisabledOrCooling(acc *Account) bool {
	return acc.Status == AccountDisabled || acc.IsCooling()
}

// restrictedParkWindow is how long a 11140-flagged account stays out of the
// task batches after one contact: the restriction persists for days and
// every touch reads as more abnormal behavior (2026-10-21 incident: four
// accounts flagged within a day of retries).
const restrictedParkWindow = 72 * time.Hour

// skipDisabled excludes manually disabled accounts AND risk-control-
// restricted ones. token_keepalive uses this looser predicate on purpose:
// credential refresh must continue during a rate-limit cooldown, or the
// token dies while the account merely cools — but a 11140-restricted
// account must be left completely alone (the community red line: touching a
// restricted account reads as more abnormal behavior and can extend the
// restriction).
func skipDisabled(acc *Account) bool {
	if acc.Status == AccountDisabled {
		return true
	}
	return acc.IsCooling() && acc.CooldownReason == CooldownReasonRestricted
}

// writeFailure applies the per-account failure policy and always returns
// non-nil so the caller keeps counting the account out of Success: it returns
// the original cause when the write-back lands, or the write-back error when
// that fails too. ErrUnsupportedTask is passed through untouched — the batch
// loop classifies it as Skipped and nothing may be written back for it.
// Credential expiry disables the account (pool.UpdateHealth auth_error
// parity) — except a risk-control restriction, which long-parks instead;
// any other error cools it for FailureCooldown.
func (d *Deps) writeFailure(ctx context.Context, acc *Account, cause error) error {
	if errors.Is(cause, ErrUnsupportedTask) {
		return cause // skip path: no write-back
	}
	if errors.Is(cause, ErrCredentialExpired) {
		// A risk-control restriction surfacing through the auth chain is not
		// a dead credential: disabling here would brick the account until a
		// human noticed, while the restriction is upstream-side and
		// temporary. Long-park instead (the auth chain already tried).
		var ue *adapter.UpstreamError
		if errors.As(cause, &ue) && ue.Reason == adapter.ReasonAccountRestricted {
			until := time.Now().Add(restrictedParkWindow)
			if err := d.Store.SetCooldown(ctx, acc.Platform, acc.ID, until); err != nil {
				return fmt.Errorf("schedule: park %s/%s after restriction: %w", acc.Platform, acc.ID, err)
			}
			return fmt.Errorf("schedule: %s/%s restricted (parked %s): %w", acc.Platform, acc.ID, restrictedParkWindow, cause)
		}
		if err := d.Store.SetStatus(ctx, acc.Platform, acc.ID, AccountDisabled); err != nil {
			return fmt.Errorf("schedule: disable %s/%s after credential expiry: %w", acc.Platform, acc.ID, err)
		}
		return cause
	}
	until := time.Now().Add(d.failureCooldown())
	if err := d.Store.SetCooldown(ctx, acc.Platform, acc.ID, until); err != nil {
		return fmt.Errorf("schedule: cooldown %s/%s: %w", acc.Platform, acc.ID, err)
	}
	return cause
}

// failureCooldown returns the configured failure cooldown, falling back to
// the pool.cooldown_sec default when unset or negative.
func (d *Deps) failureCooldown() time.Duration {
	if d.FailureCooldown > 0 {
		return d.FailureCooldown
	}
	return DefaultFailureCooldown
}

// NewSigninHandler builds the signin handler (DefaultSigninCron): daily
// sign-in with calendar streak keep-alive. Success credits the awarded points
// and (re)activates the account; failure cools it down, credential expiry
// disables it.
func NewSigninHandler(d *Deps) TaskHandler {
	return handlerFor(TaskSignin, d, accountOp{
		skip: skipDisabledOrCooling,
		run: func(ctx context.Context, acc *Account) error {
			res, err := d.Upstream.Signin(ctx, acc)
			if err != nil {
				return d.writeFailure(ctx, acc, err)
			}
			creditErr := d.Store.AddCredit(ctx, acc.Platform, acc.ID, res.CreditDelta)
			if creditErr != nil {
				return fmt.Errorf("schedule signin: credit %s/%s: %w", acc.Platform, acc.ID, creditErr)
			}
			return d.Store.SetStatus(ctx, acc.Platform, acc.ID, AccountActive)
		},
	})
}

// NewActiveCheckHandler builds the active_check handler
// (DefaultActiveCheckCron): daily active-report + streak self-check. Success
// re-activates the account; failure cools it, credential expiry disables it.
// No credit is involved.
func NewActiveCheckHandler(d *Deps) TaskHandler {
	return handlerFor(TaskActiveCheck, d, accountOp{
		skip: skipDisabledOrCooling,
		run: func(ctx context.Context, acc *Account) error {
			if err := d.Upstream.ActiveCheck(ctx, acc); err != nil {
				return d.writeFailure(ctx, acc, err)
			}
			return d.Store.SetStatus(ctx, acc.Platform, acc.ID, AccountActive)
		},
	})
}

// NewCatTravelHandler builds the cat_travel handler (DefaultCatTravelCron):
// the twice-daily one-shot quota task. Success writes the awarded quota as a
// credit delta and re-activates the account; failure handling matches signin.
func NewCatTravelHandler(d *Deps) TaskHandler {
	return handlerFor(TaskCatTravel, d, accountOp{
		skip: skipDisabledOrCooling,
		run: func(ctx context.Context, acc *Account) error {
			res, err := d.Upstream.CatTravel(ctx, acc)
			if err != nil {
				return d.writeFailure(ctx, acc, err)
			}
			creditErr := d.Store.AddCredit(ctx, acc.Platform, acc.ID, res.CreditDelta)
			if creditErr != nil {
				return fmt.Errorf("schedule cat_travel: credit %s/%s: %w", acc.Platform, acc.ID, creditErr)
			}
			return d.Store.SetStatus(ctx, acc.Platform, acc.ID, AccountActive)
		},
	})
}

// NewTokenKeepaliveHandler builds the token_keepalive handler
// (DefaultTokenKeepaliveCron): nightly credential refresh. It runs even on
// cooling accounts (only a manual disable skips — see skipDisabled), because
// credential expiry is orthogonal to a rate-limit cooldown. A zero ExpiresAt
// means platform-managed expiry and writes nothing back.
func NewTokenKeepaliveHandler(d *Deps) TaskHandler {
	return handlerFor(TaskTokenKeepalive, d, accountOp{
		skip: skipDisabled,
		run: func(ctx context.Context, acc *Account) error {
			res, err := d.Upstream.TokenKeepalive(ctx, acc)
			if err != nil {
				return d.writeFailure(ctx, acc, err)
			}
			if res.ExpiresAt.IsZero() {
				return nil
			}
			return d.Store.SetExpiry(ctx, acc.Platform, acc.ID, res.ExpiresAt)
		},
	})
}

// BuildDefaultTasks assembles the four framework Task entries with their
// cron specs and default-on switches. Deps.Crons overrides win when set —
// that is how config.Schedule lands without inventing a fourth cron key:
// cat_travel has no config entry and always uses DefaultCatTravelCron (see
// Crons). Handlers are attached separately via RegisterDefaultHandlers, so
// this function stays a pure data assembly.
func BuildDefaultTasks(d *Deps) []*Task {
	var c Crons
	if d != nil {
		c = d.Crons
	}
	return []*Task{
		{Name: TaskSignin, CronSpec: pickCron(c.Signin, DefaultSigninCron), Enabled: true},
		{Name: TaskActiveCheck, CronSpec: pickCron(c.ActiveCheck, DefaultActiveCheckCron), Enabled: true},
		{Name: TaskCatTravel, CronSpec: DefaultCatTravelCron, Enabled: true},
		{Name: TaskTokenKeepalive, CronSpec: pickCron(c.TokenKeepalive, DefaultTokenKeepaliveCron), Enabled: true},
	}
}

// RegisterDefaultHandlers binds all four handlers to a Scheduler under their
// stable task names. base is the integrator's lifecycle context, cancelled at
// shutdown: the scheduler itself passes nil to handlers (execute() calls
// handler(nil)), so injectContext's fallback to base is what lets an in-flight
// batch exit when the gateway goes down.
func RegisterDefaultHandlers(base context.Context, s *Scheduler, d *Deps) {
	s.RegisterHandler(TaskSignin, injectContext(base, NewSigninHandler(d)))
	s.RegisterHandler(TaskActiveCheck, injectContext(base, NewActiveCheckHandler(d)))
	s.RegisterHandler(TaskCatTravel, injectContext(base, NewCatTravelHandler(d)))
	s.RegisterHandler(TaskTokenKeepalive, injectContext(base, NewTokenKeepaliveHandler(d)))
}

// PlatformTaskName is the M10-B2 per-platform task naming convention:
// "<task>@<platform>" (e.g. "signin@workbuddy"). Scheduler task names are the
// map key and must be unique, so per-platform tasks are distinguished by this
// suffix; GET /api/tasks carries the plain platform in its own wire field,
// split from the name by the wire layer.
func PlatformTaskName(task, platform string) string {
	return task + "@" + platform
}

// platformOfTaskName splits PlatformTaskName back into its parts. ok=false
// for bare legacy names ("signin") — those never carried a platform suffix.
func platformOfTaskName(name string) (task, platform string, ok bool) {
	task, platform, found := strings.Cut(name, "@")
	if !found || task == "" || platform == "" {
		return name, "", false
	}
	return task, platform, true
}

// BuildPlatformTasks assembles the per-platform M10-B2 task entries: one
// signin / active_check / token_keepalive per given platform plus cat_travel
// only for workbuddy (the capability table in host.taskPlatforms is the
// product truth — other platforms get no such task rather than a forever-
// skipped one). Names follow PlatformTaskName, Source carries the platform
// (GET /api/tasks wire field "platform"), and an empty spec value falls back
// to the code-truth default exactly like Deps.Crons did.
func BuildPlatformTasks(specs []PlatformCrons) []*Task {
	tasks := make([]*Task, 0, len(specs)*4)
	for _, sp := range specs {
		if sp.Platform == "" {
			continue // unnamed platform stanza: nothing to register it under
		}
		tasks = append(tasks,
			&Task{Name: PlatformTaskName(TaskSignin, sp.Platform), Source: sp.Platform,
				CronSpec: pickCron(sp.Signin, DefaultSigninCron), Enabled: true},
			&Task{Name: PlatformTaskName(TaskActiveCheck, sp.Platform), Source: sp.Platform,
				CronSpec: pickCron(sp.ActiveCheck, DefaultActiveCheckCron), Enabled: true},
			&Task{Name: PlatformTaskName(TaskTokenKeepalive, sp.Platform), Source: sp.Platform,
				CronSpec: pickCron(sp.TokenKeepalive, DefaultTokenKeepaliveCron), Enabled: true},
		)
		if sp.Platform == "workbuddy" {
			tasks = append(tasks, &Task{Name: PlatformTaskName(TaskCatTravel, sp.Platform), Source: sp.Platform,
				CronSpec: pickCron(sp.CatTravel, DefaultCatTravelCron), Enabled: true})
		}
	}
	return tasks
}

// RegisterPlatformHandlers binds the per-platform handler for every task in
// tasks: the task's name suffix selects the platform, the task's base name
// selects which of the four handlers runs. Tasks built by BuildPlatformTasks
// (BuildDefaultTasks' siblings) share the same Deps — the account source
// resolves the platform from the task name it is handed at run time.
func RegisterPlatformHandlers(base context.Context, s *Scheduler, d *Deps, tasks []*Task) {
	builders := map[string]func(*Deps) TaskHandler{
		TaskSignin:         NewSigninHandler,
		TaskActiveCheck:    NewActiveCheckHandler,
		TaskCatTravel:      NewCatTravelHandler,
		TaskTokenKeepalive: NewTokenKeepaliveHandler,
	}
	for _, t := range tasks {
		taskName, _, ok := platformOfTaskName(t.Name)
		if !ok {
			continue // legacy bare name: RegisterDefaultHandlers' territory
		}
		build, ok := builders[taskName]
		if !ok {
			continue // unknown base task: never bind a guess
		}
		s.RegisterHandler(t.Name, injectContext(base, build(d)))
	}
}

// injectContext returns a TaskHandler that runs h with base when the
// scheduler handed it nil (today's execute(): handler(nil)); a real context
// from a future scheduler revision still wins.
func injectContext(base context.Context, h TaskHandler) TaskHandler {
	return func(v interface{}) error {
		if v == nil {
			return h(base)
		}
		return h(v)
	}
}

// pickCron returns the override when set, else the code-truth default.
func pickCron(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}
