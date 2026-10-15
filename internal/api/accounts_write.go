package api

// accounts_write.go implements the M6a account write endpoints (主人验收裁定:
// the pool table drives pause/resume/remove straight from the panel):
//
//	POST /api/accounts/{platform}:{id}/pause   → {ok:true,paused:true}
//	POST /api/accounts/{platform}:{id}/resume  → {ok:true,paused:false}
//	POST /api/accounts/{platform}:{id}/remove  → {ok:true}
//
// All three are withAPIKey-guarded and registered next to the add-account
// routes (same nil-store guard). The {id} path segment follows the recover
// endpoint's "{platform}:{accountID}" convention and is split in code exactly
// like recover.go. remove deletes the credential file, clears the store entry
// and removes the pool account — config.json is never touched. Error bodies
// reuse the {error} shape of the add-account endpoints; an unknown account is
// a 404, a malformed path segment a 400.

import (
	"errors"
	"net/http"
	"strings"

	"all2api/internal/host"
	"all2api/internal/observability"
)

func registerAccountWrite(mux *http.ServeMux, h *host.Host) {
	if h == nil || h.Creds == nil {
		return // store not wired (same guard as registerAddAccount)
	}
	mux.HandleFunc("POST /api/accounts/{id}/pause", withAPIKey(handleAccountPause(h, true)))
	mux.HandleFunc("POST /api/accounts/{id}/resume", withAPIKey(handleAccountPause(h, false)))
	mux.HandleFunc("POST /api/accounts/{id}/remove", withAPIKey(handleAccountRemove(h)))
}

// splitAccountID parses the "{platform}:{accountID}" path segment (the
// recover.go parsing shape). A malformed segment answers 400 itself.
func splitAccountID(w http.ResponseWriter, id string) (platform, accountID string, ok bool) {
	platform, accountID, found := strings.Cut(id, ":")
	if !found || strings.TrimSpace(platform) == "" || strings.TrimSpace(accountID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `account id must be "{platform}:{accountID}"`,
		})
		return "", "", false
	}
	return platform, accountID, true
}

// handleAccountPause serves both pause (want=true) and resume (want=false).
func handleAccountPause(h *host.Host, want bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, accountID, ok := splitAccountID(w, r.PathValue("id"))
		if !ok {
			return
		}
		if !h.Creds.SetPaused(platform, accountID, want) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "account not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": want})
	}
}

func handleAccountRemove(h *host.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform, accountID, ok := splitAccountID(w, r.PathValue("id"))
		if !ok {
			return
		}
		if err := h.Creds.RemoveAccount(platform, accountID); err != nil {
			if errors.Is(err, host.ErrAccountUnknown) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "account not found"})
				return
			}
			// File delete failed: the destructive step did not happen, so the
			// panel gets the reason and can retry — memory stays untouched.
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		// Destructive operation: one panel-ring line for the audit trail.
		observability.Info("account removed", "platform", platform, "account", accountID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
