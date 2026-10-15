//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// packages_api_wire.go binds the M9a packages panel endpoints to the real
// workbuddy client: the host credential store resolves the account credential
// (the same CredentialFor face the growth backend uses) and the TaskClient
// drives the upstream get-user-resource endpoint through ListPackages.
// Compiled under the same build tags as the workbuddy platform; qoder/trae-
// only builds get the nil backend from packages_api_stub.go.
package api

import (
	"context"

	"all2api/internal/host"
	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
)

// newPackagesBackend is the production constructor: the packages backend over
// the store with realm-default endpoint bases.
func newPackagesBackend(h *host.Host) packagesBackend {
	if h == nil || h.Creds == nil {
		return nil
	}
	return newWBPackagesBackend(h.Creds, workbuddy.TaskClientOptions{
		Credentials: h.Creds,
		Refresher:   h.Creds,
	})
}

// newWBPackagesBackend is the options-carrying form tests use to point the
// task client at httptest stub servers.
func newWBPackagesBackend(creds *host.CredentialStore, opts workbuddy.TaskClientOptions) packagesBackend {
	opts.Credentials = creds
	opts.Refresher = creds
	return &wbPackagesBackend{creds: creds, client: workbuddy.NewTaskClient(opts)}
}

// wbPackagesBackend implements packagesBackend over the credential store +
// task client. Safe for concurrent requests: the client is stateless per call
// (the aggregate fan-out relies on that).
type wbPackagesBackend struct {
	creds  *host.CredentialStore
	client *workbuddy.TaskClient
}

// resolve maps the path identity onto a stored credential; a lookup miss is
// the panel's 404 (growthAccountError — the same marker growthFail maps),
// anything else passes through.
func (b *wbPackagesBackend) resolve(ctx context.Context, platform, accountID string) (*workbuddy.Credential, error) {
	cred, err := b.creds.CredentialFor(ctx, &pool.Account{Platform: platform, ID: accountID})
	if err != nil {
		return nil, &growthAccountError{msg: err.Error()}
	}
	return cred, nil
}

func (b *wbPackagesBackend) Packages(ctx context.Context, platform, accountID string) ([]pkgBatch, error) {
	if platform != "workbuddy" {
		return nil, &growthAccountError{msg: "该平台无积分套餐面"}
	}
	cred, err := b.resolve(ctx, platform, accountID)
	if err != nil {
		return nil, err
	}
	rows, err := b.client.ListPackages(ctx, cred)
	if err != nil {
		return nil, err
	}
	out := make([]pkgBatch, 0, len(rows))
	for _, x := range rows {
		out = append(out, pkgBatch{
			PackageName: x.PackageName,
			Remain:      x.Remain,
			Total:       x.Total,
			CycleEnd:    x.CycleEnd,
		})
	}
	return out, nil
}
