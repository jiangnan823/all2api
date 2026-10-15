package api

// POST /api/accounts/{id}/recover — PM seq=445 ruling 1, bound to TC-M1-034.
//
// User-initiated exit for tripped (noAutoRecover) source circuits. The
// semantics are strictly ordered: validate credentials via the adapter's
// HealthCheck FIRST; only a healthy credential may reset the source circuit
// (which clears failures/rateLimitHits/noAutoRecover) and lift the account's
// pool cooldown. There is no naked Reset endpoint — Reset() is reachable only
// behind credential verification.
//
// Account id is source-namespaced: "{source}:{accountID}" (UI §task-ID
// convention — bare ids would collide across platforms).

import (
	"net/http"
	"strings"

	"all2api/internal/adapter"
	"all2api/internal/circuit"
	"all2api/internal/host"
	"all2api/internal/observability"
)

type recoverCircuitView struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type recoverResponse struct {
	OK            bool                  `json:"ok"`
	Source        string                `json:"source"`
	AccountID     string                `json:"account_id"`
	Circuit       recoverCircuitView    `json:"circuit"`
	PoolRecovered bool                  `json:"pool_recovered"`
	ReasonCode    adapter.FailureReason `json:"reason_code,omitempty"`
}

func circuitView(b *circuit.Breaker) recoverCircuitView {
	var state string
	switch b.State() {
	case circuit.StateClosed:
		state = "closed"
	case circuit.StateOpen:
		state = "open"
	case circuit.StateHalfOpen:
		state = "half_open"
	default:
		state = "unknown"
	}
	return recoverCircuitView{State: state, Reason: string(b.Reason())}
}

func handleRecover(h *host.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		source, accountID, found := strings.Cut(id, ":")
		if !found || strings.TrimSpace(source) == "" || strings.TrimSpace(accountID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": `account id must be "{source}:{accountID}"`,
			})
			return
		}

		br, ok := h.Breaker(source)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown source"})
			return
		}
		a := adapter.Default.Get(source)
		if a == nil {
			// Source configured but not compiled into this binary variant
			// (build tag): recovery genuinely cannot be performed here.
			writeJSON(w, http.StatusNotImplemented, map[string]string{
				"error": "recovery unavailable for this source",
			})
			return
		}

		hs, err := a.HealthCheck(r.Context())
		if err != nil || hs == nil {
			hs = &adapter.HealthStatus{Reason: adapter.ReasonPlatformDown}
		}
		if hs.Reason != adapter.ReasonOK {
			// Credential not valid: refuse the recovery and leave the circuit
			// exactly as it was — resetting here would mask the real problem.
			// M5a: the refusal is one of the recover wire points in the panel
			// ring (GET /api/logs); stdout keeps its silence from before.
			observability.Warn("recover refused", "source", source, "account", accountID, "reason", string(hs.Reason))
			writeJSON(w, http.StatusConflict, recoverResponse{
				OK:         false,
				Source:     source,
				AccountID:  accountID,
				Circuit:    circuitView(br),
				ReasonCode: hs.Reason,
			})
			return
		}

		// Verification passed: Reset clears failures, rateLimitHits and the
		// noAutoRecover latch (the tripped-circuit exit the PM ruling asked
		// for). The pool side only lifts cooldowns — a manually disabled
		// account stays disabled (pool.RecoverAccount never overrides that).
		br.Reset()
		poolRecovered := h.Pool.RecoverAccount(source, accountID)
		observability.Info("recover ok", "source", source, "account", accountID, "pool_recovered", poolRecovered)

		writeJSON(w, http.StatusOK, recoverResponse{
			OK:            true,
			Source:        source,
			AccountID:     accountID,
			Circuit:       circuitView(br),
			PoolRecovered: poolRecovered,
		})
	}
}
