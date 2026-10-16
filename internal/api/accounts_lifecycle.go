package api

// accounts_lifecycle.go implements the M10-B2 row-inline lifecycle endpoints
// (v1.2 parity: the panel's account table gains a direct disable/enable
// switch next to the existing checkin/balance quick actions):
//
//	POST /api/accounts/{platform}:{id}/disable → SetAccountStatus("disabled")
//	POST /api/accounts/{platform}:{id}/enable  → SetAccountStatus("active")
//
// The {id} path segment follows the recover/accounts_write
// "{platform}:{accountID}" convention, split by splitAccountID (400 on a
// malformed segment, 404 on an account the pool does not know). disable
// freezes the account in place — counters, credit and cooldown state are
// kept, Select just stops offering it (pool Account.SetStatus semantics);
// enable writes the plain "active" status, deliberately WITHOUT clearing a
// running cooldown: that is recover's job (POST /api/accounts/{id}/recover
// verifies the credential first), so enable cannot be used to bypass a 429
// backoff.
//
// Registered from RegisterPanelData behind a nil-host/nil-pool guard (this is
// a pure pool-plane operation — no credential store involved).

import (
	"net/http"

	"all2api/internal/host"
	"all2api/internal/observability"
)

// registerAccountLifecycle wires the two lifecycle endpoints beside the other
// account write routes.
func registerAccountLifecycle(mux *http.ServeMux, h *host.Host) {
	if h == nil || h.Pool == nil {
		return // no pool — nothing to enable/disable
	}
	mux.HandleFunc("POST /api/accounts/{id}/disable", withAPIKey(handleAccountLifecycle(h, "disabled")))
	mux.HandleFunc("POST /api/accounts/{id}/enable", withAPIKey(handleAccountLifecycle(h, "active")))
}

// handleAccountLifecycle serves both directions: want is the target lifecycle
// status ("disabled" / "active").
func handleAccountLifecycle(h *host.Host, want string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, accountID, ok := splitAccountID(w, r.PathValue("id"))
		if !ok {
			return
		}
		drainBody(r) // frozen wire carries {} and no fields
		if !h.Pool.SetAccountStatus(platform, accountID, want) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "account not found"})
			return
		}
		// Lifecycle flips are audit-worthy panel actions: one ring line each.
		observability.Info("account lifecycle", "platform", platform, "account", accountID, "status", want)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": want})
	}
}
