package api

// GET /api/time — ServerTimeResponse (api-contract.ts v0.5.1, UI §5.7/§5.8):
// the panel must derive "now" from the server clock for countdowns and the
// expired check (credential expiresAt.unix < now); browser clocks are
// forbidden. This is purely additive wiring of an already-frozen contract
// type — no new keys, not a D2.

import (
	"net/http"
	"time"
)

type serverTimeResponse struct {
	Now      int64  `json:"now"`      // Unix seconds — the only value used for comparisons
	Local    string `json:"local"`    // server-local display string (same layout as TimeLocal.local)
	Timezone string `json:"timezone"` // display annotation only (IANA name when TZ is set)
}

func handleServerTime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	now := time.Now()
	tz := time.Local.String()
	if tz == "" {
		tz = "UTC"
	}
	writeJSON(w, http.StatusOK, serverTimeResponse{
		Now:      now.Unix(),
		Local:    now.Format(localLayout),
		Timezone: tz,
	})
}
