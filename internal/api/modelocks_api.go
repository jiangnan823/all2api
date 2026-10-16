package api

// GET /api/model-locks — M9b 模型锁池（wire 契约冻结，前端并行开发逐字对齐）：
//
//	200 {"locks":[{"model":"deepseek-v4-flash","platform":"workbuddy",
//	      "locked_accounts":2,"total_accounts":4,
//	      "earliest_unlock":{"unix":…,"local":"…"}|null,"reason":"429 rate limit",
//	      "scope":"workbuddy","available":2,"total":4}]}
//
// 数据源不是上游，而是网关自身路由层的 429 观测（router.DefaultModelLocks，
// dispatch 的 recordSendOutcome 记账）；total_accounts 是该平台池内账号总数，
// 由本 handler 与 pool 联结——注册表只管锁，不抄池子状态。无锁时返回
// {"locks":[]}（非 null），前端据此显示空态。
//
// M10-B1 追加三个键（既有键名一律不动）：scope = 平台域（与 platform 同值，
// wire 冻结名）；available = 该模型当前可选账号数（池内可选且未被该模型锁定）；
// total = available + locked_accounts。可用度联结由注册表完成（BindPool 注入
// 池视图），本 handler 只转写。
//
// POST /api/model-locks/unlock — M10-B1 解锁：
//
//	body {"model":"x"} → 解锁该模型全部锁；body {} / {"model":""} / 空请求体
//	→ 全池解锁。200 {"unlocked":n}（n = 实际移除的存活锁数）。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"all2api/internal/adapter"
	"all2api/internal/host"
	"all2api/internal/router"
)

// modelLockWire is one /api/model-locks row (snake_case wire keys, contract
// freeze above). EarliestUnlock stays nil when a group somehow holds no
// recoverable unlock time — the field must be able to encode null per contract.
type modelLockWire struct {
	Model          string             `json:"model"`
	Scope          string             `json:"scope"`    // M10-B1: 平台域，与 platform 同值
	Platform       string             `json:"platform"` // M9b frozen key
	LockedAccounts int                `json:"locked_accounts"`
	TotalAccounts  int                `json:"total_accounts"`
	Available      int                `json:"available"` // M10-B1: 该模型当前可选账号数
	Total          int                `json:"total"`     // M10-B1: 可选 + 锁定总数
	EarliestUnlock *adapter.TimeLocal `json:"earliest_unlock"`
	Reason         string             `json:"reason"`
}

type modelLocksResponse struct {
	Locks []modelLockWire `json:"locks"`
}

// handleModelLocks serves the aggregated model-lock view. The registry is
// injected (production passes router.DefaultModelLocks) so tests can run
// against an isolated registry instead of process-global state; construction
// also binds the host pool as the availability-join source (M10-B1) — the one
// place registry and pool meet outside the router.
func handleModelLocks(h *host.Host, locks *router.ModelLockRegistry) http.HandlerFunc {
	if h != nil {
		locks.BindPool(h.Pool)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		resp := modelLocksResponse{Locks: make([]modelLockWire, 0)}
		for _, a := range locks.Snapshot() {
			row := modelLockWire{
				Model:          a.Model,
				Scope:          a.Platform, // M10-B1 scope = 平台域
				Platform:       a.Platform,
				LockedAccounts: a.LockedAccounts,
				TotalAccounts:  pooledAccountCount(h, a.Platform),
				Available:      a.Available,
				Total:          a.Total,
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

// modelLockUnlockRequest is the POST /api/model-locks/unlock body. Empty model
// (missing key, empty string, or no body at all) means the whole pool, scoped
// to platform when that key carries a non-empty platform name — the per-
// platform pages'「全部解锁」must not reach into other platforms' locks
// (M10 review scope-leak fix).
type modelLockUnlockRequest struct {
	Model    string `json:"model"`
	Platform string `json:"platform"`
}

// modelLockUnlockResponse is the frozen M10-B1 reply shape: {"unlocked":n}.
type modelLockUnlockResponse struct {
	Unlocked int `json:"unlocked"`
}

// handleModelLockUnlock serves POST /api/model-locks/unlock: {"model":"x"}
// unlocks that one model everywhere; {} / {"model":""} / empty body unlock the
// whole pool — narrowed to one platform when the body carries platform.
// The reply is {"unlocked":n} with n the live locks actually removed —
// expired ghosts never inflate it.
func handleModelLockUnlock(locks *router.ModelLockRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		var req modelLockUnlockRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			// io.EOF = empty body: still a whole-pool unlock request.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		model := strings.TrimSpace(req.Model)
		n := locks.Unlock(model)
		if model == "" {
			// 空 model（{} / {"model":""} / 空请求体）= 全池解锁；platform
			// 非空时收窄到该平台域（平台页「全部解锁」的作用域）。
			n = locks.UnlockAll(strings.TrimSpace(req.Platform))
		}
		writeJSON(w, http.StatusOK, modelLockUnlockResponse{Unlocked: n})
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
