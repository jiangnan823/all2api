// sched.go bridges the scheduled-task handlers (internal/schedule deps.go
// seam) onto the host's shared account pool, plus the honest no-protocol
// upstream stub. cmd/gateway/main.go builds schedule.Deps exclusively through
// the constructors here; since M3b the real upstream is the workbuddy task
// client assembled in sched_upstream_wire.go (build-tag selected, same split
// as credentials_wire.go), so the main.go call site stays one line.
package host

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"all2api/internal/config"
	"all2api/internal/pool"
	"all2api/internal/schedule"
)

// NewScheduleSource returns the AccountSource over the shared pool: every
// pooled account, as value snapshots, filtered by platform capability for the
// asking taskType (deps.go contract: the source, not the handler, decides
// platform filtering). Today's capability table: cat_travel is a workbuddy-only
// quota task; signin / active_check / token_keepalive run on all platforms.
func NewScheduleSource(p *pool.WeightedRandomSelector) schedule.AccountSource {
	return &poolSource{pool: p}
}

// poolSource implements schedule.AccountSource.
type poolSource struct {
	pool *pool.WeightedRandomSelector
}

// taskPlatforms maps a schedule.Task* name onto the platforms that offer it.
// Today every task is workbuddy-only: the one wired schedule upstream is the
// workbuddy task client (sched_upstream_wire.go), and offering an account to
// an upstream that cannot serve it would turn a quiet skip into a per-tick
// failure+cooldown. qoder/trae join the table when a platform client lands
// for them. An unknown taskType conservatively matches no platform (empty
// batch, not a cross-platform misfire).
func taskPlatforms(taskType string) map[string]bool {
	switch taskType {
	case schedule.TaskSignin, schedule.TaskActiveCheck, schedule.TaskCatTravel, schedule.TaskTokenKeepalive:
		return map[string]bool{WorkbuddyPlatform: true}
	default:
		return nil
	}
}

// taskPlatformScope resolves the platform a task run is scoped to. The M10-B2
// per-platform tasks carry the schedule.PlatformTaskName suffix
// ("<task>@<platform>"): the run then covers exactly that platform — the
// registration (SchedulePlatformTasks) already decided capability when it
// created the task, so the source trusts the name. Bare legacy task names
// keep the global capability table above.
func taskPlatformScope(taskType string) (platform string, scoped bool) {
	base, platform, found := strings.Cut(taskType, "@")
	if !found || base == "" || platform == "" {
		return "", false
	}
	return platform, true
}

// Accounts lists the run's account batch (deps.go AccountSource contract).
func (s *poolSource) Accounts(_ context.Context, taskType string) ([]*schedule.Account, error) {
	platforms := taskPlatforms(taskType)
	scope, scoped := taskPlatformScope(taskType)
	snapshot := s.pool.SnapshotAccounts()

	result := make([]*schedule.Account, 0, len(snapshot))
	for i := range snapshot {
		acc := snapshot[i]
		// Unknown taskType: taskPlatforms returned nil — conservatively match
		// no platform (empty batch, never a cross-platform misfire).
		if !scoped && (platforms == nil || !platforms[acc.Platform]) {
			continue
		}
		// Platform-scoped run ("<task>@<platform>"): only that platform's
		// accounts belong in the batch, whatever the legacy table says.
		if scoped && acc.Platform != scope {
			continue
		}
		result = append(result, &schedule.Account{
			ID:             acc.ID,
			Platform:       acc.Platform,
			Status:         acc.Status,
			CooldownEnd:    acc.CooldownEnd,
			CooldownReason: acc.CooldownReason,
		})
	}
	return result, nil
}

// SchedulePlatformTasks resolves the M10-B2 per-platform cron configuration
// onto schedule.PlatformCrons for every configured source: an empty platform
// key falls back to the frozen global key (signin_cron / task_cron /
// token_keepalive_cron), and cat_travel falls back to its code-truth default
// when cat_travel_cron is unset. Sources are delivered in sorted-id order so
// the resulting task registration order is deterministic.
func SchedulePlatformTasks(cfg config.Config) []schedule.PlatformCrons {
	ids := make([]string, 0, len(cfg.Sources))
	for id := range cfg.Sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	specs := make([]schedule.PlatformCrons, 0, len(ids))
	for _, id := range ids {
		specs = append(specs, schedule.PlatformCrons{
			Platform:       id,
			Signin:         pickPlatformCron(cfg.Schedule.SigninCronWorkbuddy, cfg.Schedule.SigninCronQoder, cfg.Schedule.SigninCronTrae, id, cfg.Schedule.SigninCron),
			ActiveCheck:    pickPlatformCron(cfg.Schedule.TaskCronWorkbuddy, cfg.Schedule.TaskCronQoder, cfg.Schedule.TaskCronTrae, id, cfg.Schedule.TaskCron),
			TokenKeepalive: pickPlatformCron(cfg.Schedule.TokenKeepaliveWorkbuddy, cfg.Schedule.TokenKeepaliveQoder, cfg.Schedule.TokenKeepaliveTrae, id, cfg.Schedule.TokenKeepalive),
		})
		if id == WorkbuddyPlatform {
			specs[len(specs)-1].CatTravel = cfg.Schedule.CatTravelCron
		}
	}
	return specs
}

// pickPlatformCron returns the platform key's value when non-empty, else the
// frozen global fallback (旧键冻结不改名：平台键只是叠加层).
func pickPlatformCron(wb, qoder, trae, id, global string) string {
	switch id {
	case WorkbuddyPlatform:
		if wb != "" {
			return wb
		}
	case QoderPlatform:
		if qoder != "" {
			return qoder
		}
	case "trae": // no credentials plane yet, so no platform constant exists
		if trae != "" {
			return trae
		}
	}
	return global
}

// NewScheduleStore returns the AccountStore writing task outcomes back into
// the shared pool through the minimal scheduler-facing pool methods
// (ApplyAccountCredit / SetAccountStatus / SetAccountCooldown /
// SetAccountExpiry). A missing account is an error, so a vanished account
// surfaces in RunStats.Failed instead of vanishing silently.
func NewScheduleStore(p *pool.WeightedRandomSelector) schedule.AccountStore {
	return &poolStore{pool: p}
}

// poolStore implements schedule.AccountStore.
type poolStore struct {
	pool *pool.WeightedRandomSelector
}

// AddCredit applies a signed credit delta to the account.
func (s *poolStore) AddCredit(_ context.Context, platform, accountID string, delta int64) error {
	if !s.pool.ApplyAccountCredit(platform, accountID, delta) {
		return fmt.Errorf("schedule store: account %s/%s not in pool", platform, accountID)
	}
	return nil
}

// SetStatus writes a lifecycle status (schedule.Account* constants).
func (s *poolStore) SetStatus(_ context.Context, platform, accountID, status string) error {
	if !s.pool.SetAccountStatus(platform, accountID, status) {
		return fmt.Errorf("schedule store: account %s/%s not in pool", platform, accountID)
	}
	return nil
}

// SetCooldown cools the account until the given time (status → cooling).
func (s *poolStore) SetCooldown(_ context.Context, platform, accountID string, until time.Time) error {
	if !s.pool.SetAccountCooldown(platform, accountID, until) {
		return fmt.Errorf("schedule store: account %s/%s not in pool", platform, accountID)
	}
	return nil
}

// SetExpiry records a refreshed credential expiry.
func (s *poolStore) SetExpiry(_ context.Context, platform, accountID string, at time.Time) error {
	if !s.pool.SetAccountExpiry(platform, accountID, at) {
		return fmt.Errorf("schedule store: account %s/%s not in pool", platform, accountID)
	}
	return nil
}

// NewScheduleUpstream returns the per-account platform-operation client used
// by the scheduler. Under the workbuddy-capable builds this is the real task
// client (newScheduleUpstream in sched_upstream_wire.go): it resolves each
// account's credential from the store and drives the four documented endpoint
// families (check-in / active report / cat travel / token refresh), folding
// adapter failure reasons onto the handler sentinels. qoder/trae-only builds
// get the honest stub. A nil store keeps the constructor usable — the ops
// then fail with explicit "no credential store wired" errors instead of
// panicking.
func NewScheduleUpstream(creds *CredentialStore) schedule.Upstream {
	return newScheduleUpstream(creds)
}

// NewScheduleStubUpstream returns the honest no-protocol upstream: every
// operation fails with an error wrapping schedule.ErrUnsupportedTask, which
// the handler batch loop classifies as Skipped — counted quietly per account,
// no write-back, batch status stays success. This is the qoder/trae-only
// build's production upstream and the tests' skip-semantics fixture.
func NewScheduleStubUpstream() schedule.Upstream {
	return stubUpstream{}
}

// stubUpstream implements schedule.Upstream with "not wired yet" outcomes.
type stubUpstream struct{}

func (stubUpstream) Signin(_ context.Context, acc *schedule.Account) (schedule.SigninResult, error) {
	return schedule.SigninResult{}, unsupported("signin", acc)
}

func (stubUpstream) ActiveCheck(_ context.Context, acc *schedule.Account) error {
	return unsupported("active_check", acc)
}

func (stubUpstream) CatTravel(_ context.Context, acc *schedule.Account) (schedule.CatTravelResult, error) {
	return schedule.CatTravelResult{}, unsupported("cat_travel", acc)
}

func (stubUpstream) TokenKeepalive(_ context.Context, acc *schedule.Account) (schedule.KeepaliveResult, error) {
	return schedule.KeepaliveResult{}, unsupported("token_keepalive", acc)
}

// unsupported builds the sentinel-wrapping error shared by all stub ops. The
// ErrUnsupportedTask wrap is what keeps the batch loop quiet (Skipped, no
// write-back) instead of cooling every account on every cron tick.
func unsupported(op string, acc *schedule.Account) error {
	return fmt.Errorf("schedule upstream stub: %s on %s: no platform client wired yet: %w",
		op, acc.Platform, schedule.ErrUnsupportedTask)
}
