//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// packages.go implements the per-package credits view over the same billing
// balance endpoint tasks.go already drives (M9a counterpart of the summed
// GetUserResource):
//
//	ListPackages  POST {billing}/v2/billing/meter/get-user-resource
//	              → one PackageBatch per CapacityUnit=="credits" row, FEFO
//	                (first-expired-first-out) sorted.
//
// Wire facts (wb_research reference client + live evidence 2026-10-15, owner
// account with 32 packages — trial pack and fission packs each carry their own
// expiry):
//   - every Accounts[] row carries PackageName, CycleEndTime and the cycle
//     capacity trio; the balance roll-up in tasks.go consumes
//     CycleCapacityRemain/CycleCapacitySize and ListPackages reads exactly the
//     same pair per row (never CapacityRemain — the report pins the cycle form,
//     protocol report §2).
//   - CycleEndTime is a "2006-01-02 15:04:05" wall-clock string in UTC+8 (the
//     official-site display timezone, same constant the wb2api reference
//     bucketing uses); empty or missing means the package never expires.
//   - the same credits filter as GetUserResource applies: token/compute
//     wallets are not credit packages and must never leak into the expiry
//     view.
//
// The client is stdlib-only and every endpoint base is injectable, so tests
// run against httptest stubs only — the suite never contacts the real
// endpoints (task red line).
package workbuddy

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

// packageEndLayout is the upstream CycleEndTime wall-clock format; the zone is
// UTC+8 regardless of the gateway host's local timezone (the official site
// renders the same strings, so parsing in any other zone would shift every
// expiry by the host offset).
const packageEndLayout = "2006-01-02 15:04:05"

// packageEndZone is the fixed UTC+8 display zone CycleEndTime strings live in.
var packageEndZone = time.FixedZone("UTC+8", 8*60*60)

// PackageBatch is one credits package on the expiry view. Remain/Total follow
// the GetUserResource conventions (CycleCapacityRemain spendable,
// CycleCapacitySize package capacity); a row missing either number keeps the
// zero value rather than aborting the list (one malformed pack must not hide
// the rest). CycleEnd zero value = the package never expires.
type PackageBatch struct {
	PackageName string
	Remain      int64
	Total       int64
	CycleEnd    time.Time
}

// ListPackages returns the account's credits packages FEFO-sorted: packages
// with a CycleEndTime first in ascending expiry order, never-expiring packages
// appended after them (first-expired-first-out — the panel's 积分到期提醒
// renders row one as the nearest expiry). The endpoint, envelope handling and
// classification are exactly GetUserResource's (same do() round-trip); only
// the fold differs — rows stay separate instead of summing.
//
// An absent/empty Accounts array (or no credits row at all) stays an explicit
// error, mirroring GetUserResource — never an invented empty success.
func (c *TaskClient) ListPackages(ctx context.Context, cred *Credential) ([]PackageBatch, error) {
	body, err := c.do(ctx, cred, http.MethodPost, pathGetUserResource, []byte("{}"), true)
	if err != nil {
		return nil, err
	}
	resp, ok := envelopeData(body)["Response"].(map[string]any)
	if !ok {
		return nil, errors.New("workbuddy: user-resource reply missing data.Response object")
	}
	inner, ok := resp["Data"].(map[string]any)
	if !ok {
		return nil, errors.New("workbuddy: user-resource reply missing data.Response.Data object")
	}
	accounts, ok := inner["Accounts"].([]any)
	if !ok {
		return nil, errors.New("workbuddy: user-resource reply missing data.Response.Data.Accounts array")
	}
	var out []PackageBatch
	for _, row := range accounts {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		unit, _ := m["CapacityUnit"].(string)
		if !strings.EqualFold(strings.TrimSpace(unit), "credits") {
			continue // token/compute wallets are not credit packages
		}
		batch := PackageBatch{
			PackageName: firstString(m, "PackageName"),
		}
		if n, ok := numberOf(m["CycleCapacityRemain"]); ok {
			if v, ok := creditsFrom(n); ok {
				batch.Remain = v
			}
		}
		if n, ok := numberOf(m["CycleCapacitySize"]); ok {
			if v, ok := creditsFrom(n); ok {
				batch.Total = v
			}
		}
		batch.CycleEnd = parseCycleEnd(m["CycleEndTime"])
		out = append(out, batch)
	}
	if len(out) == 0 {
		return nil, errors.New("workbuddy: user-resource reply has no CapacityUnit=credits account row")
	}
	sortPackagesFEFO(out)
	return out, nil
}

// parseCycleEnd reads one CycleEndTime tolerantly: empty, missing, non-string
// or unparseable all mean "never expires" (zero time), never an invented
// date. The string is wall-clock UTC+8 (packageEndZone).
func parseCycleEnd(v any) time.Time {
	s, ok := v.(string)
	if !ok {
		return time.Time{}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.ParseInLocation(packageEndLayout, s, packageEndZone)
	if err != nil {
		return time.Time{}
	}
	return t
}

// sortPackagesFEFO orders expiring packages by ascending CycleEnd and moves
// never-expiring packages (zero CycleEnd) behind them. Stable so upstream row
// order survives among equal expiries — deterministic output for the panel.
func sortPackagesFEFO(pkgs []PackageBatch) {
	sort.SliceStable(pkgs, func(i, j int) bool {
		zi, zj := pkgs[i].CycleEnd.IsZero(), pkgs[j].CycleEnd.IsZero()
		if zi != zj {
			return zj // the expiring one goes first
		}
		if zi {
			return false // both never expire: keep upstream order
		}
		return pkgs[i].CycleEnd.Before(pkgs[j].CycleEnd)
	})
}
