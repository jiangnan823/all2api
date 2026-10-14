// sched.go bridges the scheduled-task handlers (internal/schedule deps.go
// seam) onto the host's shared account pool, plus the honest no-protocol
// upstream stub. This is the M2 batch-② assembly surface: cmd/gateway/main.go
// builds schedule.Deps exclusively through the three constructors here, so
// swapping in the real platform HTTP client later is a one-line change at the
// NewScheduleUpstream call site.
package host

import (
	"context"
	"fmt"
	"time"

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
// An unknown taskType conservatively matches no platform (empty batch, not a
// cross-platform misfire).
func taskPlatforms(taskType string) map[string]bool {
	switch taskType {
	case schedule.TaskCatTravel:
		return map[string]bool{"workbuddy": true}
	case schedule.TaskSignin, schedule.TaskActiveCheck, schedule.TaskTokenKeepalive:
		return map[string]bool{"workbuddy": true, "qoder": true, "trae": true}
	default:
		return nil
	}
}

// Accounts lists the run's account batch (deps.go AccountSource contract).
func (s *poolSource) Accounts(_ context.Context, taskType string) ([]*schedule.Account, error) {
	platforms := taskPlatforms(taskType)
	snapshot := s.pool.SnapshotAccounts()

	result := make([]*schedule.Account, 0, len(snapshot))
	for i := range snapshot {
		acc := snapshot[i]
		// Unknown taskType: taskPlatforms returned nil — conservatively match
		// no platform (empty batch, never a cross-platform misfire).
		if platforms == nil || !platforms[acc.Platform] {
			continue
		}
		result = append(result, &schedule.Account{
			ID:          acc.ID,
			Platform:    acc.Platform,
			Status:      acc.Status,
			CooldownEnd: acc.CooldownEnd,
		})
	}
	return result, nil
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
// by the scheduler. The platform HTTP protocols do not exist in this repo yet
// (protocol-research batch in flight), so today this is the honest stub:
// every operation fails with an error wrapping schedule.ErrUnsupportedTask,
// which the handler batch loop classifies as Skipped — counted quietly per
// account, no write-back, batch status stays success. No scheduled traffic is
// faked and no account state is invented.
//
// REPLACEMENT POINT: once internal/platforms gains a real per-account client,
// change the body to construct it (one line) — every signature is already the
// schedule.Upstream interface, e.g.
//
//	return realclient.New(cfg)
func NewScheduleUpstream() schedule.Upstream {
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
