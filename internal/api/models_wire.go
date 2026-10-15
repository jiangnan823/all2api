//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// models_wire.go binds the M8 /api/models live-catalog seam to the real
// workbuddy upstream directory: the host credential store resolves the
// account credential (the same CredentialFor face the chat sender and the
// M3b schedule upstream use) and the workbuddy ModelCatalog drives the
// endpoint behind its 60s TTL cache. Compiled under the same build tags as
// the workbuddy platform; qoder/trae-only builds get the nil constructor
// from models_wire_stub.go and the endpoint serves the static tables only.
package api

import (
	"context"

	"all2api/internal/host"
	"all2api/internal/platform/workbuddy"
	"all2api/internal/pool"
)

// wbModelCatalog implements upstreamCatalog over the credential store + the
// cached workbuddy directory client. Safe for concurrent requests: the cache
// inside workbuddy.ModelCatalog is mutex-guarded.
type wbModelCatalog struct {
	creds *host.CredentialStore
	pool  accountSource
	cat   *workbuddy.ModelCatalog
}

// accountSource is the read-only pool face the catalog needs (pick one
// workbuddy account to fetch the directory with). *pool.Pool satisfies it.
type accountSource interface {
	SnapshotAccounts() []pool.Account
}

// newModelsCatalog is the production constructor: the live-catalog seam over
// the host credential store with realm-default endpoint bases. nil when the
// host carries no credential store — the endpoint then stays on the static
// tables (source "local").
func newModelsCatalog(h *host.Host) upstreamCatalog {
	if h == nil || h.Creds == nil {
		return nil
	}
	// Keep a typed-nil *pool.Pool out of the interface (pickAccount nil-checks
	// the interface; nil pool = no account to fetch with = static fallback).
	var p accountSource
	if h.Pool != nil {
		p = h.Pool
	}
	return newWBModelCatalog(h.Creds, p, workbuddy.TaskClientOptions{})
}

// newWBModelCatalog is the options-carrying form tests use to point the
// directory client at httptest stub servers (mirrors newWBGrowthBackend).
func newWBModelCatalog(creds *host.CredentialStore, p accountSource, opts workbuddy.TaskClientOptions) upstreamCatalog {
	opts.Credentials = creds
	opts.Refresher = creds
	client := workbuddy.NewTaskClient(opts)
	return &wbModelCatalog{creds: creds, pool: p, cat: workbuddy.NewModelCatalog(client)}
}

// Fetch resolves one stored workbuddy credential and pulls the live
// directory. ok=false on any miss (no account in the pool, no credential,
// upstream unavailable) — the caller falls back to the static tables.
func (b *wbModelCatalog) Fetch(ctx context.Context, refresh bool) ([]modelWire, bool) {
	acct := b.pickAccount()
	if acct == nil {
		return nil, false
	}
	cred, err := b.creds.CredentialFor(ctx, acct)
	if err != nil {
		return nil, false
	}
	entries, ok := b.cat.Get(ctx, cred, refresh)
	if !ok {
		return nil, false
	}
	out := make([]modelWire, 0, len(entries))
	for _, m := range entries {
		out = append(out, upstreamWireRow(m))
	}
	return out, true
}

// pickAccount chooses the workbuddy account the directory is fetched with:
// the pool's first workbuddy account (pool order is the panel's own listing
// order; any live credential serves the same shared directory). nil when the
// pool holds none — the static fallback then answers instead.
func (b *wbModelCatalog) pickAccount() *pool.Account {
	if b.pool == nil {
		return nil
	}
	for _, acc := range b.pool.SnapshotAccounts() {
		if acc.Platform == "workbuddy" {
			a := acc // snapshot copy outlives the range
			return &a
		}
	}
	return nil
}

// upstreamWireRow folds one upstream directory entry onto the frozen wire.
// Facts the upstream omitted go out as null (pointer nil / nil slice), never
// as a fabricated 0/false.
func upstreamWireRow(m workbuddy.CatalogModel) modelWire {
	row := modelWire{
		Platform:       "workbuddy",
		Provider:       "workbuddy",
		ID:             "workbuddy/" + m.Model,
		Name:           m.Name,
		Model:          m.Model,
		SupportsStream: boolPtr(m.SupportsStream), // protocol fact: stream-mandatory chat path
		IsDefault:      boolPtr(m.IsDefault),
		ThinkingTiers:  m.ThinkingTiers,
	}
	if row.Name == "" {
		row.Name = m.Model // bare id when the upstream ships no display name
	}
	if m.ContextSize > 0 {
		row.ContextSize = intPtr(int(m.ContextSize))
	}
	if m.MaxOutput > 0 {
		row.MaxOutput = intPtr(int(m.MaxOutput))
	}
	row.SupportsTool = boolPtr(m.SupportsTool) // as declared by the directory
	if m.IsFree {
		row.IsFree = boolPtr(true) // badge-derived; no badge → null (unknown)
	}
	row.CreditMultiplier = m.CreditMultiplier
	return row
}
