package api

// packages_api.go implements the M9a credits-expiry panel endpoints (the
// wb2api「积分到期提醒」card backend, frozen wire for the parallel frontend
// batch):
//
//	GET /api/accounts/{platform}:{id}/packages
//	     → 200 {"packages":[{"package_name":"CodeBuddy个人体验版","remain":500,
//	          "total":500,"cycle_end":{"unix":…,"local":"2026-10-31 23:59:59"}|null}]}
//	     (one row per upstream credits package, FEFO ascending — nearest expiry
//	     first, never-expiring packs last)
//	GET /api/packages
//	     → 200 {"accounts":[{"platform":"workbuddy","id":"…","nickname":"…",
//	          "total_remain":N,"nearest":{"unix":…,"local":"…"}|null,
//	          "daily_burn":45|null,
//	          "batches":[{"remain":306,"cycle_end":{"unix":…,"local":"…"}}]}]}
//
// daily_burn is the wb2api「到期前日均需耗」: total_remain ÷ ceil(days to the
// nearest expiry), null when the account has no dated batch or the nearest
// expiry is already in the past (the burn rate would be meaningless).
//
// Both endpoints are withAPIKey-guarded and registered beside the growth
// routes (registerPackages from RegisterPanelData). The {id} segment follows
// the recover/growth "{platform}:{accountID}" convention, split in code
// exactly like recover.go; a non-workbuddy platform answers 400 (this
// upstream is the only one with a credits-package surface). An account
// without a stored credential answers 404; upstream failures answer 502 with
// the reason shown verbatim and no credential material echoed (the shared
// growthFail frame).
//
// The aggregate probes every pooled workbuddy account concurrently (four at a
// time — the same politeness bound as the startup credit seed); one account's
// failure never hides the others: its entry stays in place with the batches
// empty and the upstream reason annotated verbatim in "error".
//
// The upstream client lives behind the packagesBackend seam: workbuddy builds
// wire it to the real TaskClient.ListPackages over host.CredentialStore
// (packages_api_wire.go); qoder/trae-only builds get nil (packages_api_stub.go)
// and the handlers answer an honest build-limitation error instead of
// pretending.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/host"
	"all2api/internal/pool"
)

// packagesProbeConcurrency bounds the aggregate fan-out: four probes at a
// time, matching the startup credit seed's politeness against the upstream's
// serial per-account engine.
const packagesProbeConcurrency = 4

// pkgBatch is the platform-neutral package row the seam hands over. CycleEnd
// zero value = the package never expires (serialized as null).
type pkgBatch struct {
	PackageName string
	Remain      int64
	Total       int64
	CycleEnd    time.Time
}

// pkgBatchWire is one frozen single-account wire row.
type pkgBatchWire struct {
	PackageName string             `json:"package_name"`
	Remain      int64              `json:"remain"`
	Total       int64              `json:"total"`
	CycleEnd    *adapter.TimeLocal `json:"cycle_end"`
}

type accountPackagesResponse struct {
	Packages []pkgBatchWire `json:"packages"`
}

// aggBatchWire is the aggregate's trimmed batch row (remain + cycle_end only —
// the frozen aggregate shape carries no package_name/total).
type aggBatchWire struct {
	Remain   int64              `json:"remain"`
	CycleEnd *adapter.TimeLocal `json:"cycle_end"`
}

// pkgAccountWire is one account on the aggregate wire. The pointer fields
// encode absence honestly: total_remain/nearest/daily_burn serialize as null
// when the probe failed or the account has no dated batch; error carries the
// per-account failure reason verbatim and is omitted on success.
type pkgAccountWire struct {
	Platform    string             `json:"platform"`
	ID          string             `json:"id"`
	Nickname    string             `json:"nickname"`
	TotalRemain *int64             `json:"total_remain"`
	Nearest     *adapter.TimeLocal `json:"nearest"`
	DailyBurn   *int64             `json:"daily_burn"`
	Batches     []aggBatchWire     `json:"batches"`
	Error       string             `json:"error,omitempty"`
}

type packagesAggResponse struct {
	Accounts []pkgAccountWire `json:"accounts"`
}

// packagesBackend is the seam the handlers serve. Implemented over the real
// workbuddy client in packages_api_wire.go (workbuddy builds).
type packagesBackend interface {
	// Packages lists one account's credits batches, FEFO-sorted.
	Packages(ctx context.Context, platform, accountID string) ([]pkgBatch, error)
}

// registerPackages wires the packages endpoints beside the other account
// routes (same nil-host/nil-store guard as registerGrowth).
func registerPackages(mux *http.ServeMux, h *host.Host) {
	if h == nil || h.Creds == nil {
		return // store not wired — no credential source, no packages surface
	}
	b := newPackagesBackend(h)
	mux.HandleFunc("GET /api/accounts/{id}/packages", withAPIKey(handleAccountPackages(b)))
	mux.HandleFunc("GET /api/packages", withAPIKey(handlePackagesAgg(b, h)))
	mux.HandleFunc("POST /api/packages/refresh", withAPIKey(handlePackagesRefresh(b, h)))
}

// packagesRefreshWire is the frozen M10 wire for POST /api/packages/refresh:
// the panel's「检查」button fires it, then re-runs GET /api/packages to
// rebuild the card. refreshed counts the accounts whose live probe succeeded;
// errors carries failures verbatim (id + reason, no credential material —
// pkgAggErrorText already strips to the classified reason).
type packagesRefreshWire struct {
	Refreshed int               `json:"refreshed"`
	Errors    []pkgRefreshError `json:"errors"`
}

type pkgRefreshError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// handlePackagesRefresh re-probes every pooled workbuddy account's packages
// live (same politeness bound as the GET aggregate) and reports the tally.
// The probe results themselves are discarded — the client refetches the
// aggregate right after — so this endpoint is a connectivity/latency check
// with a fresh-cache side effect, not a second read path to keep in sync.
func handlePackagesRefresh(b packagesBackend, h *host.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b == nil {
			packagesUnavailable(w)
			return
		}
		targets := workbuddyTargets(h)
		errs := make([]pkgRefreshError, 0)
		var mu sync.Mutex
		var refreshed int64
		sem := make(chan struct{}, packagesProbeConcurrency)
		var wg sync.WaitGroup
		for _, a := range targets {
			wg.Add(1)
			sem <- struct{}{}
			go func(platform, id string) {
				defer wg.Done()
				defer func() { <-sem }()
				if _, err := b.Packages(r.Context(), platform, id); err != nil {
					mu.Lock()
					errs = append(errs, pkgRefreshError{ID: id, Error: pkgAggErrorText(err)})
					mu.Unlock()
					return
				}
				mu.Lock()
				refreshed++
				mu.Unlock()
			}(a.Platform, a.ID)
		}
		wg.Wait()
		writeJSON(w, http.StatusOK, packagesRefreshWire{Refreshed: int(refreshed), Errors: errs})
	}
}

// packagesTarget parses and validates the account path segment. A malformed
// segment answers 400 (splitAccountID); a non-workbuddy platform answers 400
// — the credits-package surface exists only on this upstream.
func packagesTarget(w http.ResponseWriter, r *http.Request) (platform, accountID string, ok bool) {
	platform, accountID, ok = splitAccountID(w, r.PathValue("id"))
	if !ok {
		return "", "", false
	}
	if platform != "workbuddy" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "该平台无积分套餐面"})
		return "", "", false
	}
	return platform, accountID, true
}

// packagesUnavailable answers for builds without the workbuddy platform
// (qoder/trae-only): honest limitation, never a fabricated empty success.
func packagesUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "当前构建未包含 workbuddy 平台，积分套餐面不可用"})
}

// pkgWireBatch folds one seam row onto the single-account wire shape.
func pkgWireBatch(b pkgBatch) pkgBatchWire {
	return pkgBatchWire{
		PackageName: b.PackageName,
		Remain:      b.Remain,
		Total:       b.Total,
		CycleEnd:    timeLocalPtr(b.CycleEnd),
	}
}

func handleAccountPackages(b packagesBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, accountID, ok := packagesTarget(w, r)
		if !ok {
			return
		}
		if b == nil {
			packagesUnavailable(w)
			return
		}
		batches, err := b.Packages(r.Context(), platform, accountID)
		if err != nil {
			growthFail(w, "查询积分批次", err)
			return
		}
		out := make([]pkgBatchWire, 0, len(batches))
		for _, x := range batches {
			out = append(out, pkgWireBatch(x))
		}
		writeJSON(w, http.StatusOK, accountPackagesResponse{Packages: out})
	}
}

// workbuddyTargets enumerates the pooled workbuddy accounts the aggregate
// probes, deterministically ordered (SnapshotAccounts is unordered). A missing
// pool (or store) yields no targets — the honest empty aggregate.
func workbuddyTargets(h *host.Host) []pool.Account {
	if h == nil || h.Pool == nil || h.Creds == nil {
		return nil
	}
	var out []pool.Account
	for _, acc := range h.Pool.SnapshotAccounts() {
		if acc.Platform == "workbuddy" {
			out = append(out, acc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// dailyBurnFor folds the「到期前日均需耗」: total remain ÷ ceil(days to the
// nearest expiry). days < 1 (already expired, or expiring within the same
// day-boundary arithmetic's reach of zero) yields false — the wire null.
func dailyBurnFor(totalRemain int64, nearest, now time.Time) (int64, bool) {
	days := int64(math.Ceil(nearest.Sub(now).Hours() / 24))
	if days < 1 {
		return 0, false
	}
	return int64(math.Round(float64(totalRemain) / float64(days))), true
}

// foldPkgAccount folds one account's FEFO batches onto the aggregate wire:
// total_remain sums every batch, nearest is the earliest dated expiry,
// daily_burn the burn-rate fold above, batches the trimmed rows in the same
// FEFO order the single-account endpoint serves.
func foldPkgAccount(platform, id, nickname string, batches []pkgBatch, now time.Time) pkgAccountWire {
	entry := pkgAccountWire{
		Platform: platform,
		ID:       id,
		Nickname: nickname,
		Batches:  make([]aggBatchWire, 0, len(batches)),
	}
	var total int64
	var nearest time.Time
	for _, x := range batches {
		total += x.Remain
		entry.Batches = append(entry.Batches, aggBatchWire{Remain: x.Remain, CycleEnd: timeLocalPtr(x.CycleEnd)})
		if !x.CycleEnd.IsZero() && (nearest.IsZero() || x.CycleEnd.Before(nearest)) {
			nearest = x.CycleEnd
		}
	}
	totalRemain := total
	entry.TotalRemain = &totalRemain
	if !nearest.IsZero() {
		entry.Nearest = timeLocalPtr(nearest)
		if burn, ok := dailyBurnFor(total, nearest, now); ok {
			entry.DailyBurn = &burn
		}
	}
	return entry
}

// pkgAggErrorText frames one per-account failure for the aggregate's error
// annotation: the same mapping growthFail uses for the endpoint-level 502
// (auth death, rate limit), with everything else shown verbatim — credential
// material never echoes because the backend errors never carry it.
func pkgAggErrorText(err error) string {
	var ue *adapter.UpstreamError
	if errors.As(err, &ue) {
		switch ue.Reason {
		case adapter.ReasonAuthExpired:
			return "该账号凭证已失效，请重新登录后重试"
		case adapter.ReasonRateLimited:
			return fmt.Sprintf("上游限流（%s）", ue.Message)
		}
	}
	return err.Error()
}

// handlePackagesAgg serves GET /api/packages: every pooled workbuddy account
// probed concurrently (packagesProbeConcurrency at a time), one failure
// annotated in place instead of failing the aggregate. Delivery order is
// deterministic (platform, then id) regardless of probe completion order.
func handlePackagesAgg(b packagesBackend, h *host.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b == nil {
			packagesUnavailable(w)
			return
		}
		targets := workbuddyTargets(h)
		entries := make([]pkgAccountWire, len(targets))
		sem := make(chan struct{}, packagesProbeConcurrency)
		var wg sync.WaitGroup
		for i, a := range targets {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, platform, id string) {
				defer wg.Done()
				defer func() { <-sem }()
				nickname := ""
				if h != nil && h.Creds != nil {
					nickname = h.Creds.NicknameFor(platform, id)
				}
				batches, err := b.Packages(r.Context(), platform, id)
				if err != nil {
					failed := pkgAccountWire{
						Platform: platform,
						ID:       id,
						Nickname: nickname,
						Batches:  []aggBatchWire{},
						Error:    pkgAggErrorText(err),
					}
					entries[i] = failed
					return
				}
				entries[i] = foldPkgAccount(platform, id, nickname, batches, time.Now())
			}(i, a.Platform, a.ID)
		}
		wg.Wait()
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Platform != entries[j].Platform {
				return entries[i].Platform < entries[j].Platform
			}
			return entries[i].ID < entries[j].ID
		})
		writeJSON(w, http.StatusOK, packagesAggResponse{Accounts: entries})
	}
}
