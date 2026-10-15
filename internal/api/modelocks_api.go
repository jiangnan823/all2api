package api

// GET /api/model-locks — M9b 模型锁池（wire 契约冻结，前端并行开发逐字对齐）：
//
//	200 {"locks":[{"model":"deepseek-v4-flash","platform":"workbuddy",
//	      "locked_accounts":2,"total_accounts":4,
//	      "earliest_unlock":{"unix":…,"local":"…"}|null,"reason":"429 rate limit"}]}
//
// 数据源不是上游，而是网关自身路由层的 429 观测（router.DefaultModelLocks，
// dispatch 的 recordSendOutcome 记账）；total_accounts 是该平台池内账号总数，
// 由本 handler 与 pool 联结——注册表只管锁，不抄池子状态。无锁时返回
// {"locks":[]}（非 null），前端据此显示空态。

import (
	"net/http"

	"all2api/internal/adapter"
	"all2api/internal/host"
	"all2api/internal/router"
)

// modelLockWire is one /api/model-locks row (snake_case wire keys, contract
// freeze above). EarliestUnlock stays nil when a group somehow holds no
// recoverable unlock time — the field must be able to encode null per contract.
type modelLockWire struct {
	Model          string             `json:"model"`
	Platform       string             `json:"platform"`
	LockedAccounts int                `json:"locked_accounts"`
	TotalAccounts  int                `json:"total_accounts"`
	EarliestUnlock *adapter.TimeLocal `json:"earliest_unlock"`
	Reason         string             `json:"reason"`
}

type modelLocksResponse struct {
	Locks []modelLockWire `json:"locks"`
}

// handleModelLocks serves the aggregated model-lock view. The registry is
// injected (production passes router.DefaultModelLocks) so tests can run
// against an isolated registry instead of process-global state.
func handleModelLocks(h *host.Host, locks *router.ModelLockRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		resp := modelLocksResponse{Locks: make([]modelLockWire, 0)}
		for _, a := range locks.Snapshot() {
			row := modelLockWire{
				Model:          a.Model,
				Platform:       a.Platform,
				LockedAccounts: a.LockedAccounts,
				TotalAccounts:  pooledAccountCount(h, a.Platform),
				Reason:         a.Reason,
			}
			if !a.EarliestUnlock.IsZero() {
				t := a.EarliestUnlock
				row.EarliestUnlock = &adapter.TimeLocal{Unix: t.Unix(), Local: t.Format(localLayout)}
			}
			resp.Locks = append(resp.Locks, row)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// pooledAccountCount is the total_accounts join: the platform's pool size,
// regardless of lock state. A nil host/pool (handler built without wiring)
// reports 0 rather than guessing.
func pooledAccountCount(h *host.Host, platform string) int {
	if h == nil || h.Pool == nil {
		return 0
	}
	return len(h.Pool.ListAccounts(platform))
}
