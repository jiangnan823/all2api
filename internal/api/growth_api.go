package api

// growth_api.go implements the M7a growth credit-task panel endpoints (the
// WorkBuddy「积分任务」pop-up backend, frozen wire for the parallel frontend
// batch):
//
//	GET  /api/accounts/{platform}:{id}/growth-tasks
//	     → 200 {"source":"upstream","tasks":[{code,title,progress_done,
//	       progress_total,reward_credit,reward_energy,status}]}
//	       with status ∈ claimed|claimable|completed|in_progress|accepted|
//	       not_accepted; upstream failure → 502 {error}, as-is.
//	POST /api/accounts/{platform}:{id}/growth-accept   body {} → {"accepted":N}
//	     (sign up every not-yet-accepted task; idempotent skips don't count)
//	POST /api/accounts/{platform}:{id}/growth-claim-auto body {}
//	     → {"claimed":N,"credit_gained":N}
//	     (claim every completed-unclaimed task, summing the landed credit)
//
// All three are withAPIKey-guarded and registered beside the account write
// routes (registerGrowth from RegisterPanelData). The {id} segment follows
// the recover/accounts_write "{platform}:{accountID}" convention, split in
// code exactly like recover.go. platform must be workbuddy (this upstream is
// the only one with a growth surface): anything else answers 400
// 「该平台无成长任务面」; an account without a stored credential answers 404;
// upstream failures answer 502 with a Chinese-framed reason that shows the
// upstream message verbatim and never echoes credential material.
//
// The upstream client lives behind the growthBackend seam: workbuddy builds
// wire it to the real client over host.CredentialStore.CredentialFor
// (growth_api_wire.go); qoder/trae-only builds get nil (growth_api_stub.go)
// and the handlers answer an honest build-limitation error instead of
// pretending.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"all2api/internal/adapter"
	"all2api/internal/host"
	"all2api/internal/observability"
)

// growthTaskWire is one task on the frozen wire. Keys are exactly the frozen
// ones; the frontend passes title through verbatim (Chinese titles are
// resolved Go-side in the workbuddy package).
type growthTaskWire struct {
	Code          string `json:"code"`
	Title         string `json:"title"`
	ProgressDone  int64  `json:"progress_done"`
	ProgressTotal int64  `json:"progress_total"`
	RewardCredit  int64  `json:"reward_credit"`
	RewardEnergy  int64  `json:"reward_energy"`
	Status        string `json:"status"`
}

type growthTasksResponse struct {
	Source string           `json:"source"` // always "upstream": the data is live
	Tasks  []growthTaskWire `json:"tasks"`
}

// growthBackend is the seam the handlers serve. Implemented over the real
// workbuddy client in growth_api_wire.go (workbuddy builds).
type growthBackend interface {
	// Tasks lists the frozen-wire task view for one account.
	Tasks(ctx context.Context, platform, accountID string) ([]growthTaskWire, error)
	// AcceptAll signs up every pending task and returns the count.
	AcceptAll(ctx context.Context, platform, accountID string) (int, error)
	// ClaimAuto sweeps every completed-unclaimed task and returns the claim
	// count plus the credit that landed.
	ClaimAuto(ctx context.Context, platform, accountID string) (claimed int, creditGained int64, err error)
}

// growthAccountError marks "no usable credential for this account here" —
// the panel reads it as 404 (unknown/not-loaded account), never 502.
type growthAccountError struct{ msg string }

func (e *growthAccountError) Error() string { return e.msg }

// registerGrowth wires the three endpoints beside the other account routes
// (same nil-host/nil-store guard as registerAccountWrite).
func registerGrowth(mux *http.ServeMux, h *host.Host) {
	if h == nil || h.Creds == nil {
		return // store not wired — no credential source, no growth surface
	}
	b := newGrowthBackend(h)
	mux.HandleFunc("GET /api/accounts/{id}/growth-tasks", withAPIKey(handleGrowthTasks(b)))
	mux.HandleFunc("POST /api/accounts/{id}/growth-accept", withAPIKey(handleGrowthAccept(b)))
	mux.HandleFunc("POST /api/accounts/{id}/growth-claim-auto", withAPIKey(handleGrowthClaim(b)))
}

// growthTarget parses and validates the account path segment. A malformed
// segment answers 400 (splitAccountID); a non-workbuddy platform answers
// 400 「该平台无成长任务面」 — the growth surface exists only on this upstream.
func growthTarget(w http.ResponseWriter, r *http.Request) (platform, accountID string, ok bool) {
	platform, accountID, ok = splitAccountID(w, r.PathValue("id"))
	if !ok {
		return "", "", false
	}
	if platform != "workbuddy" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "该平台无成长任务面"})
		return "", "", false
	}
	return platform, accountID, true
}

// growthUnavailable answers for builds without the workbuddy platform
// (qoder/trae-only): honest limitation, never a fabricated empty success.
func growthUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "当前构建未包含 workbuddy 平台，成长任务面不可用"})
}

// growthFail maps a backend failure onto the frozen error wire: no credential
// → 404, everything else → 502 with a Chinese frame around the upstream
// message (shown verbatim, credential material never echoed).
func growthFail(w http.ResponseWriter, what string, err error) {
	var acct *growthAccountError
	if errors.As(err, &acct) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在或未加载凭证"})
		return
	}
	var ue *adapter.UpstreamError
	if errors.As(err, &ue) {
		switch ue.Reason {
		case adapter.ReasonAuthExpired:
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "该账号凭证已失效，请重新登录后重试"})
			return
		case adapter.ReasonRateLimited:
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("%s失败：上游限流（%s），请稍后重试", what, ue.Message)})
			return
		}
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("%s失败：%s", what, err.Error())})
}

// drainBody discards the request body: the frozen wire carries `{}` and no
// fields, so any body (or none) is accepted and never inspected.
func drainBody(r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
}

func handleGrowthTasks(b growthBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, accountID, ok := growthTarget(w, r)
		if !ok {
			return
		}
		if b == nil {
			growthUnavailable(w)
			return
		}
		tasks, err := b.Tasks(r.Context(), platform, accountID)
		if err != nil {
			growthFail(w, "查询成长任务", err)
			return
		}
		if tasks == nil {
			tasks = []growthTaskWire{} // empty stays an array on the wire
		}
		writeJSON(w, http.StatusOK, growthTasksResponse{Source: "upstream", Tasks: tasks})
	}
}

func handleGrowthAccept(b growthBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, accountID, ok := growthTarget(w, r)
		if !ok {
			return
		}
		drainBody(r)
		if b == nil {
			growthUnavailable(w)
			return
		}
		n, err := b.AcceptAll(r.Context(), platform, accountID)
		if err != nil {
			growthFail(w, "接受任务", err)
			return
		}
		observability.Info("growth accept all", "platform", platform, "account", accountID, "accepted", n)
		writeJSON(w, http.StatusOK, map[string]any{"accepted": n})
	}
}

func handleGrowthClaim(b growthBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, accountID, ok := growthTarget(w, r)
		if !ok {
			return
		}
		drainBody(r)
		if b == nil {
			growthUnavailable(w)
			return
		}
		n, credit, err := b.ClaimAuto(r.Context(), platform, accountID)
		if err != nil {
			growthFail(w, "领取奖励", err)
			return
		}
		observability.Info("growth claim auto", "platform", platform, "account", accountID, "claimed", n, "credit", credit)
		writeJSON(w, http.StatusOK, map[string]any{"claimed": n, "credit_gained": credit})
	}
}
