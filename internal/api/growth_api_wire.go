//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// growth_api_wire.go binds the M7a growth panel endpoints to the real
// workbuddy growth client: the host credential store resolves the account
// credential (the same CredentialFor face the chat sender and the M3b
// schedule upstream use) and the TaskClient drives the upstream endpoints.
// Compiled under the same build tags as the workbuddy platform; qoder/trae-
// only builds get the nil backend from growth_api_stub.go.
package api

import (
	"context"

	"all2api/internal/host"
	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
)

// newGrowthBackend is the production constructor: the growth backend over the
// store with realm-default endpoint bases.
func newGrowthBackend(h *host.Host) growthBackend {
	if h == nil || h.Creds == nil {
		return nil
	}
	return newWBGrowthBackend(h.Creds, workbuddy.TaskClientOptions{
		Credentials: h.Creds,
		Refresher:   h.Creds,
	})
}

// newWBGrowthBackend is the options-carrying form tests use to point the task
// client at httptest stub servers.
func newWBGrowthBackend(creds *host.CredentialStore, opts workbuddy.TaskClientOptions) growthBackend {
	opts.Credentials = creds
	opts.Refresher = creds
	return &wbGrowthBackend{creds: creds, client: workbuddy.NewTaskClient(opts)}
}

// wbGrowthBackend implements growthBackend over the credential store + task
// client. Safe for concurrent requests: the client is stateless per call.
type wbGrowthBackend struct {
	creds  *host.CredentialStore
	client *workbuddy.TaskClient
	pool   *pool.WeightedRandomSelector
}

// Checkin runs the daily check-in and lands the reward credit in the pool.
// The upstream endpoint is idempotent (repeat → already=true, zero gain).
func (b *wbGrowthBackend) Checkin(ctx context.Context, platform, accountID string) (creditGained int64, already bool, err error) {
	if platform != "workbuddy" {
		return 0, false, &growthAccountError{msg: "该平台无成长任务面"}
	}
	cred, err := b.resolve(ctx, platform, accountID)
	if err != nil {
		return 0, false, err
	}
	gained, err := b.client.Signin(ctx, cred)
	if err != nil {
		return 0, false, err
	}
	if gained > 0 && b.pool != nil {
		b.pool.ApplyAccountCredit(platform, accountID, gained)
	}
	return gained, already, nil
}

// RefreshBalance re-probes the upstream wallet for one account and writes the
// fresh remain/total back into the pool (the panel's 余额 button).
func (b *wbGrowthBackend) RefreshBalance(ctx context.Context, platform, accountID string) (remain, total int64, err error) {
	cred, err := b.resolve(ctx, platform, accountID)
	if err != nil {
		return 0, 0, err
	}
	remain, total, err = b.client.GetUserResource(ctx, cred)
	if err != nil {
		return 0, 0, err
	}
	if b.pool != nil {
		b.pool.SetAccountCredit(platform, accountID, remain)
		b.pool.SetAccountCreditTotal(platform, accountID, total)
	}
	return remain, total, nil
}

// resolve maps the path identity onto a stored credential; a lookup miss is
// the panel's 404 (growthAccountError), anything else passes through.
func (b *wbGrowthBackend) resolve(ctx context.Context, platform, accountID string) (*workbuddy.Credential, error) {
	cred, err := b.creds.CredentialFor(ctx, &pool.Account{Platform: platform, ID: accountID})
	if err != nil {
		return nil, &growthAccountError{msg: err.Error()}
	}
	return cred, nil
}

// growthWireTask folds the client's task view onto the frozen wire shape.
func growthWireTask(t workbuddy.GrowthTask) growthTaskWire {
	return growthTaskWire{
		Code:          t.Code,
		Title:         t.Title,
		ProgressDone:  t.Current,
		ProgressTotal: t.Target,
		RewardCredit:  t.RewardCredit,
		RewardEnergy:  t.RewardEnergy,
		Status:        t.Status,
	}
}

func (b *wbGrowthBackend) Tasks(ctx context.Context, platform, accountID string) ([]growthTaskWire, error) {
	cred, err := b.resolve(ctx, platform, accountID)
	if err != nil {
		return nil, err
	}
	tasks, err := b.client.GrowthTasks(ctx, cred)
	if err != nil {
		return nil, err
	}
	out := make([]growthTaskWire, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, growthWireTask(t))
	}
	return out, nil
}

func (b *wbGrowthBackend) AcceptAll(ctx context.Context, platform, accountID string) (int, error) {
	cred, err := b.resolve(ctx, platform, accountID)
	if err != nil {
		return 0, err
	}
	return b.client.GrowthAcceptAll(ctx, cred)
}

func (b *wbGrowthBackend) ClaimAuto(ctx context.Context, platform, accountID string) (int, int64, error) {
	cred, err := b.resolve(ctx, platform, accountID)
	if err != nil {
		return 0, 0, err
	}
	return b.client.GrowthClaimAuto(ctx, cred)
}
