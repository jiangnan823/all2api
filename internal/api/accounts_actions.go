package api

// 行内快捷操作端点（v1.2 parity：对齐 wb2api 行内「已签/余额」按钮）：
//
//	POST /api/accounts/{platform}:{id}/checkin → 每日签到（上游幂等，重复签
//	    返回 already=true 零奖励）。{"signed":true,"already":b,"credit_gained":n}
//	POST /api/accounts/{platform}:{id}/balance → 立即重查该账号余额并回写池。
//	    {"remain":n,"total":n}
//
// 仅 workbuddy 有此二面（矩阵 §1a）；其他平台 400。上游实时调用失败 502
// {error} 透传；凭证缺失 404（growthAccountError）。余额成功后池内 credit /
// credit_total 同步刷新——面板余额列立即看到新值。

import (
	"net/http"

	"all2api/internal/host"
)

// registerAccountActions wires the two quick-action endpoints beside the
// growth routes (same nil-host/nil-store guard as registerGrowth).
func registerAccountActions(mux *http.ServeMux, h *host.Host) {
	if h == nil || h.Creds == nil {
		return // no credential source — the action surface doesn't exist
	}
	b := newGrowthBackend(h)
	mux.HandleFunc("POST /api/accounts/{id}/checkin", withAPIKey(handleAccountCheckin(b)))
	mux.HandleFunc("POST /api/accounts/{id}/balance", withAPIKey(handleAccountBalance(b)))
}

func handleAccountCheckin(b growthBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, accountID, ok := growthTarget(w, r)
		if !ok {
			return
		}
		gained, already, err := b.Checkin(r.Context(), platform, accountID)
		if err != nil {
			growthFail(w, "签到", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"signed":        true,
			"already":       already,
			"credit_gained": gained,
		})
	}
}

func handleAccountBalance(b growthBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, accountID, ok := growthTarget(w, r)
		if !ok {
			return
		}
		remain, total, err := b.RefreshBalance(r.Context(), platform, accountID)
		if err != nil {
			growthFail(w, "余额查询", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"remain": remain, "total": total})
	}
}
