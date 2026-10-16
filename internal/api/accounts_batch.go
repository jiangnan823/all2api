package api

// accounts_batch.go implements the M10-B2 per-platform batch operation
// endpoint (wb2api parity: the account table's column-level action buttons):
//
//	POST /api/accounts/{platform}/batch-op   body {"op":"checkin"|"travel"|
//	  "active"|"keepalive"}
//	→ 200 {"requested":n,"done":n,"skipped":n,
//	       "failed":[{"id":"x","error":"..."}]}
//
// The execution plane reuses the scheduled-task machinery verbatim: the four
// ops are the schedule.Upstream operations (Signin / ActiveCheck / CatTravel /
// TokenKeepalive, deps.go) over the production assembly (host/sched.go +
// sched_upstream_wire.go), and the account batch comes from
// AccountSource.Accounts(ctx, taskType) — so platform capability is decided
// by the same table the cron tasks use (travel is workbuddy-only; ops on
// platforms the wired upstream cannot serve count every requested account as
// skipped, never as a fabricated success).
//
// Semantics per account, mirroring the scheduled handlers one-to-one:
//   - disabled accounts are skipped by every op; cooling accounts by every op
//     except keepalive (credential refresh must continue through a cooldown);
//   - success writes back exactly what the handler would (credit reward into
//     the pool, status re-activation, refreshed expiry);
//   - a failure cools the account for pool.cooldown_sec — a credential-expiry
//     classification disables it instead (pool auth_error parity);
//   - ErrUnsupportedTask counts skipped with no write-back.
//
// Accounts run strictly sequentially (the local model engine is single-
// request serial — no concurrent batch), each with its own timeout budget so
// one hung upstream call cannot pin the panel request forever.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/observability"
	"all2api/internal/pool"
	"all2api/internal/schedule"
)

// batchOpPerAccountTimeout is the single-account execution budget. The whole
// request is bounded by requested × this — 100 accounts answer within at
// most ~50 minutes worst case, and the handler never blocks the gateway's
// other work (each upstream call runs in the request's goroutine, the pool
// write-backs are lock-scoped).
const batchOpPerAccountTimeout = 30 * time.Second

// batchOpFailedWire is one per-account failure row on the frozen wire.
type batchOpFailedWire struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// batchOpResponse is the frozen wire shape of POST .../batch-op.
type batchOpResponse struct {
	Requested int                 `json:"requested"`
	Done      int                 `json:"done"`
	Skipped   int                 `json:"skipped"`
	Failed    []batchOpFailedWire `json:"failed"` // always an array, [] when none
}

// registerAccountBatch wires the batch-op endpoint behind the same
// nil-host/nil-store guard as the growth routes (the upstream assembly
// resolves credentials, so without a store there is nothing to execute).
func registerAccountBatch(mux *http.ServeMux, h *host.Host) {
	if h == nil || h.Creds == nil || h.Pool == nil {
		return
	}
	mux.HandleFunc("POST /api/accounts/{platform}/batch-op",
		withAPIKey(handleBatchOp(h, host.NewScheduleSource(h.Pool), host.NewScheduleUpstream(h.Creds))))
}

// handleBatchOp serves one platform-wide batch. src and up are injectable for
// tests (deterministic fakes over a real pool); production passes the same
// constructors main.go wires the cron tasks with.
func handleBatchOp(h *host.Host, src schedule.AccountSource, up schedule.Upstream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform := r.PathValue("platform")
		if platform == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "platform is required"})
			return
		}
		var req struct {
			Op string `json:"op"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		taskType, ok := batchOpTaskType(req.Op)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "op must be one of checkin, travel, active, keepalive"})
			return
		}

		// requested: every pooled account of the platform, regardless of
		// eligibility — the wire answers "how many did you aim at". The
		// snapshot is taken once: value copies, safe to walk lock-free while
		// write-backs mutate the live pool underneath.
		var platformAccounts []pool.Account
		for _, acc := range h.Pool.SnapshotAccounts() {
			if acc.Platform == platform {
				platformAccounts = append(platformAccounts, acc)
			}
		}

		resp := batchOpResponse{Requested: len(platformAccounts), Failed: []batchOpFailedWire{}}
		if resp.Requested == 0 {
			writeJSON(w, http.StatusOK, resp)
			return
		}

		// The execution batch comes from the same AccountSource the cron
		// tasks run on — its capability table decides which of the requested
		// accounts the wired upstream can actually serve. Everything else is
		// a skip (e.g. travel on qoder: 全 skipped, nothing executed).
		batch, err := src.Accounts(r.Context(), taskType)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		executable := make(map[string]*schedule.Account, len(batch))
		for _, acc := range batch {
			if acc.Platform == platform {
				executable[acc.ID] = acc
			}
		}

		for i := range platformAccounts {
			acc := platformAccounts[i]
			exec, inBatch := executable[acc.ID]
			if !inBatch || batchOpSkip(taskType, exec) {
				resp.Skipped++
				continue
			}
			if execErr := runBatchOp(r.Context(), h.Pool, taskType, exec, up); execErr != nil {
				if errors.Is(execErr, schedule.ErrUnsupportedTask) {
					resp.Skipped++
					continue
				}
				resp.Failed = append(resp.Failed, batchOpFailedWire{ID: acc.ID, Error: execErr.Error()})
				continue
			}
			resp.Done++
		}
		observability.Info("batch op", "platform", platform, "op", req.Op,
			"requested", resp.Requested, "done", resp.Done, "skipped", resp.Skipped, "failed", len(resp.Failed))
		writeJSON(w, http.StatusOK, resp)
	}
}

// batchOpTaskType maps the frozen op vocabulary onto the schedule task names
// (the AccountSource taskType and RunStats key). ok=false for unknown ops.
func batchOpTaskType(op string) (taskType string, ok bool) {
	switch op {
	case "checkin":
		return schedule.TaskSignin, true
	case "travel":
		return schedule.TaskCatTravel, true
	case "active":
		return schedule.TaskActiveCheck, true
	case "keepalive":
		return schedule.TaskTokenKeepalive, true
	default:
		return "", false
	}
}

// batchOpSkip mirrors the scheduled handlers' skip predicates: keepalive
// deliberately runs through a rate-limit cooldown (a cooling account's token
// still needs refreshing — handlers.go skipDisabled), everything else skips
// both disabled and cooling accounts (skipDisabledOrCooling).
func batchOpSkip(taskType string, acc *schedule.Account) bool {
	if acc == nil {
		return true
	}
	if taskType == schedule.TaskTokenKeepalive {
		return acc.Status == schedule.AccountDisabled
	}
	return acc.Status == schedule.AccountDisabled || acc.IsCooling()
}

// runBatchOp executes one account's op under its own timeout budget and
// writes the outcome back into the pool exactly like the scheduled handlers
// (handlers.go): success → reward credit / status re-activation / expiry
// write-back; credential expiry → disable; anything else → cooldown for
// pool.cooldown_sec. The returned error classifies the account into the
// failed[] rows (ErrUnsupportedTask must pass through untouched — skip path).
func runBatchOp(ctx context.Context, p *pool.WeightedRandomSelector, taskType string, acc *schedule.Account, up schedule.Upstream) error {
	runCtx, cancel := context.WithTimeout(ctx, batchOpPerAccountTimeout)
	defer cancel()

	var opErr error
	switch taskType {
	case schedule.TaskSignin:
		var res schedule.SigninResult
		res, opErr = up.Signin(runCtx, acc)
		if opErr == nil && res.CreditDelta != 0 {
			p.ApplyAccountCredit(acc.Platform, acc.ID, res.CreditDelta)
		}
	case schedule.TaskCatTravel:
		var res schedule.CatTravelResult
		res, opErr = up.CatTravel(runCtx, acc)
		if opErr == nil && res.CreditDelta != 0 {
			p.ApplyAccountCredit(acc.Platform, acc.ID, res.CreditDelta)
		}
	case schedule.TaskActiveCheck:
		opErr = up.ActiveCheck(runCtx, acc)
	case schedule.TaskTokenKeepalive:
		var res schedule.KeepaliveResult
		res, opErr = up.TokenKeepalive(runCtx, acc)
		if opErr == nil && !res.ExpiresAt.IsZero() {
			p.SetAccountExpiry(acc.Platform, acc.ID, res.ExpiresAt)
		}
	default:
		return schedule.ErrUnsupportedTask
	}
	if opErr != nil {
		return writeBatchOpFailure(p, taskType, acc, opErr)
	}
	// Success parity with the handlers: the credit-bearing ops landed their
	// reward above; every op except keepalive re-activates the account.
	if taskType != schedule.TaskTokenKeepalive {
		p.SetAccountStatus(acc.Platform, acc.ID, schedule.AccountActive)
	}
	return nil
}

// writeBatchOpFailure applies the failure policy and always returns non-nil
// so the caller keeps counting the account out of done (handlers.go
// writeFailure parity, pool-addressed directly).
func writeBatchOpFailure(p *pool.WeightedRandomSelector, taskType string, acc *schedule.Account, cause error) error {
	switch {
	case errors.Is(cause, schedule.ErrUnsupportedTask):
		return cause // skip path: no write-back
	case errors.Is(cause, schedule.ErrCredentialExpired):
		if !p.SetAccountStatus(acc.Platform, acc.ID, schedule.AccountDisabled) {
			return errors.Join(cause, errors.New("account vanished from pool"))
		}
		return cause
	default:
		cooldown := time.Duration(config.Shared.Get().Pool.CooldownSec) * time.Second
		if cooldown <= 0 {
			cooldown = schedule.DefaultFailureCooldown
		}
		if !p.SetAccountCooldown(acc.Platform, acc.ID, time.Now().Add(cooldown)) {
			return errors.Join(cause, errors.New("account vanished from pool"))
		}
		return cause
	}
}
