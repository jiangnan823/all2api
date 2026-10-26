//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// sched_upstream_wire.go replaces the M2 honest stub with the real per-account
// schedule upstream (M3b): a workbuddy.TaskClient over the shared credential
// store. Every operation resolves the account credential from the store,
// drives the documented endpoint family (docs/upstream/workbuddy-protocol.md
// §7) and folds adapter failure reasons onto the handler sentinels:
//
//   - auth_expired (after the refresh chain already tried) wraps
//     schedule.ErrCredentialExpired — the handler disables the account;
//   - everything else passes through — the handler cools the account;
//   - a missing credential is an explicit per-account error (counted Failed,
//     never a fabricated credential and never a quiet skip);
//   - a non-workbuddy platform wraps schedule.ErrUnsupportedTask (quiet skip)
//     — this upstream cannot serve it.
//
// Compiled under the same build tags as the workbuddy platform; qoder/trae-only
// builds keep the stub via sched_upstream_wire_stub.go.
package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
	"all2api/internal/schedule"
)

// Compile-time proof the real upstream satisfies the handler seam.
var _ schedule.Upstream = (*wbScheduleUpstream)(nil)

// newScheduleUpstream is the production constructor: the task client over the
// store with realm-default endpoint bases. The store itself is the qoder
// device-refresher seam (RotateQoderDevice), so qoder's token_keepalive
// routes through the same upstream without a second construction path.
func newScheduleUpstream(creds *CredentialStore) schedule.Upstream {
	return newWBScheduleUpstream(creds, workbuddy.TaskClientOptions{
		Credentials: creds,
		Refresher:   creds,
	})
}

// newWBScheduleUpstream is the options-carrying form tests use to point the
// task client at httptest stub servers.
func newWBScheduleUpstream(creds *CredentialStore, opts workbuddy.TaskClientOptions) schedule.Upstream {
	return &wbScheduleUpstream{store: creds, tasks: workbuddy.NewTaskClient(opts)}
}

// wbScheduleUpstream implements schedule.Upstream over the workbuddy task
// client. Safe for concurrent handlers: the task client is stateless per call
// and the refresh chain gates itself per account.
type wbScheduleUpstream struct {
	store *CredentialStore
	tasks *workbuddy.TaskClient
}

// qoderDeviceRefresher is the qoder keepalive seam: the credential store's
// rotation entry point (which drives the registered qoder sender and lands
// the rotated pair back in the store + device file). Structural, so the
// qoder platform needs no import here.
type qoderDeviceRefresher interface {
	RotateQoderDevice(ctx context.Context, acct *pool.Account) (time.Time, error)
}

// resolve maps a schedule.Account onto a stored credential. Lookup goes
// through the store's CredentialSource face, so an unknown account surfaces
// as the store's explicit "no credential for platform/id" error.
func (u *wbScheduleUpstream) resolve(ctx context.Context, acc *schedule.Account) (*workbuddy.Credential, error) {
	if acc == nil {
		return nil, errors.New("schedule upstream: no account given")
	}
	if u.store == nil {
		return nil, fmt.Errorf("schedule upstream: no credential store wired (account %s/%s)", acc.Platform, acc.ID)
	}
	cred, err := u.store.CredentialFor(ctx, &pool.Account{ID: acc.ID, Platform: acc.Platform})
	if err != nil {
		return nil, fmt.Errorf("schedule upstream: %w", err)
	}
	return cred, nil
}

// unsupportedPlatform keeps the stub-era skip semantics for platforms this
// upstream cannot serve: the handler counts the account Skipped and writes
// nothing back. With today's workbuddy-only taskPlatforms table the batch
// never contains such accounts — this is the defensive backstop.
func unsupportedPlatform(op string, acc *schedule.Account) error {
	return fmt.Errorf("schedule upstream: %s on %s: no task client for platform: %w",
		op, acc.Platform, schedule.ErrUnsupportedTask)
}

// translateTaskError maps adapter-level failure reasons onto the handler
// sentinels. auth_expired means the M3a refresh chain already ran and failed,
// so the credential is dead — ErrCredentialExpired makes the handler disable
// the account instead of cooling it in a loop.
func translateTaskError(op string, err error) error {
	var ue *adapter.UpstreamError
	if errors.As(err, &ue) && ue.Reason == adapter.ReasonAuthExpired {
		return fmt.Errorf("schedule upstream: %s: %s: %w", op, ue.Message, schedule.ErrCredentialExpired)
	}
	return fmt.Errorf("schedule upstream: %s: %w", op, err)
}

// Signin executes the daily check-in and reports the awarded credit; the
// handler writes the delta back and re-activates the account. The idempotent
// "already checked in" reply arrives here as delta 0 (nothing new earned by
// this run), so a second same-day run cannot double-count.
func (u *wbScheduleUpstream) Signin(ctx context.Context, acc *schedule.Account) (schedule.SigninResult, error) {
	if acc == nil {
		return schedule.SigninResult{}, errors.New("schedule upstream: no account given")
	}
	if acc.Platform != WorkbuddyPlatform {
		return schedule.SigninResult{}, unsupportedPlatform("signin", acc)
	}
	cred, err := u.resolve(ctx, acc)
	if err != nil {
		return schedule.SigninResult{}, err
	}
	delta, err := u.tasks.Signin(ctx, cred)
	if err != nil {
		return schedule.SigninResult{}, translateTaskError("signin", err)
	}
	return schedule.SigninResult{CreditDelta: delta}, nil
}

// ActiveCheck posts the daily active report (presence signal, no credit).
func (u *wbScheduleUpstream) ActiveCheck(ctx context.Context, acc *schedule.Account) error {
	if acc == nil {
		return errors.New("schedule upstream: no account given")
	}
	if acc.Platform != WorkbuddyPlatform {
		return unsupportedPlatform("active_check", acc)
	}
	cred, err := u.resolve(ctx, acc)
	if err != nil {
		return err
	}
	if err := u.tasks.ReportActive(ctx, cred); err != nil {
		return translateTaskError("active_check", err)
	}
	return nil
}

// CatTravel advances the cat-travel state machine one step and reports the
// claimed reward (0 on depart/traveling legs).
func (u *wbScheduleUpstream) CatTravel(ctx context.Context, acc *schedule.Account) (schedule.CatTravelResult, error) {
	if acc == nil {
		return schedule.CatTravelResult{}, errors.New("schedule upstream: no account given")
	}
	if acc.Platform != WorkbuddyPlatform {
		return schedule.CatTravelResult{}, unsupportedPlatform("cat_travel", acc)
	}
	cred, err := u.resolve(ctx, acc)
	if err != nil {
		return schedule.CatTravelResult{}, err
	}
	delta, err := u.tasks.TravelCycle(ctx, cred)
	if err != nil {
		return schedule.CatTravelResult{}, translateTaskError("cat_travel", err)
	}
	return schedule.CatTravelResult{CreditDelta: delta}, nil
}

// TokenKeepalive rotates the credential through the M3a refresh chain — the
// store's AcceptRefreshed lands the rotated pair in memory, the pool and the
// auth file before this returns. The honest ExpiresAt rides the result; zero
// (upstream gave no expiresIn) writes nothing back, per the handler contract.
// qoder accounts route through the device-pair rotation instead: the store's
// RotateQoderDevice drives the sender's needRefresh exchange (age-gated) and
// persists the rotated pair to qoder-device.json — without it the ~15-day
// device credential dies silently and takes the only qoder account with it.
func (u *wbScheduleUpstream) TokenKeepalive(ctx context.Context, acc *schedule.Account) (schedule.KeepaliveResult, error) {
	if acc == nil {
		return schedule.KeepaliveResult{}, errors.New("schedule upstream: no account given")
	}
	if acc.Platform == QoderPlatform {
		if u.store == nil {
			return schedule.KeepaliveResult{}, fmt.Errorf("schedule upstream: no credential store wired (account %s/%s)", acc.Platform, acc.ID)
		}
		var refresher qoderDeviceRefresher = u.store
		exp, err := refresher.RotateQoderDevice(ctx, &pool.Account{ID: acc.ID, Platform: acc.Platform})
		if err != nil {
			return schedule.KeepaliveResult{}, translateTaskError("token_keepalive", err)
		}
		return schedule.KeepaliveResult{ExpiresAt: exp}, nil
	}
	if acc.Platform != WorkbuddyPlatform {
		return schedule.KeepaliveResult{}, unsupportedPlatform("token_keepalive", acc)
	}
	if u.store == nil {
		return schedule.KeepaliveResult{}, fmt.Errorf("schedule upstream: no credential store wired (account %s/%s)", acc.Platform, acc.ID)
	}
	cred, err := u.tasks.Refresh(ctx, &pool.Account{ID: acc.ID, Platform: acc.Platform})
	if err != nil {
		return schedule.KeepaliveResult{}, translateTaskError("token_keepalive", err)
	}
	return schedule.KeepaliveResult{ExpiresAt: cred.ExpiresAt}, nil
}
