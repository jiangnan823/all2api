package api

// Add-account endpoints (v1.2 §1a，主人裁定：添加账号是面板内一等操作):
//
//	POST /api/accounts/workbuddy  body = 官方桌面端凭证 JSON 原文
//	POST /api/accounts/qoder      body = {"pat": "pt-…"}
//
// Both are withAPIKey-guarded, validate before persisting and return
// {ok:true, platform, id} on success / {error} on failure. Wire errors carry
// user-facing Chinese messages (the panel shows them verbatim); nothing
// echoes credential material back.

import (
	"encoding/json"
	"io"
	"net/http"

	"all2api/internal/host"
)

func registerAddAccount(mux *http.ServeMux, h *host.Host) {
	if h == nil || h.Creds == nil {
		return // store not wired (main keeps this impossible today, but stay safe)
	}
	mux.HandleFunc("POST /api/accounts/workbuddy", withAPIKey(handleAddWorkbuddy(h)))
	mux.HandleFunc("POST /api/accounts/qoder", withAPIKey(handleAddQoder(h)))
}

func handleAddWorkbuddy(h *host.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := decodeRawBody(http.MaxBytesReader(w, r.Body, 1<<20))
		uid, err := h.Creds.AddWorkbuddyCredential(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		// Immediate balance seeding: a fresh account with credit 0 reads as
		// broken; one round of the billing probe gives the real number.
		go h.Creds.SeedCredits()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "platform": "workbuddy", "id": uid})
	}
}

func handleAddQoder(h *host.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PAT string `json:"pat"`
		}
		if err := decodeJSONInto(http.MaxBytesReader(w, r.Body, 64<<10), &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体必须为 {\"pat\": \"…\"} JSON"})
			return
		}
		uid, err := h.Creds.AddQoderPAT(body.PAT)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		go h.Creds.SeedCredits()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "platform": "qoder", "id": uid})
	}
}

// decodeRawBody returns the raw request body bytes as a string (bounded by
// the MaxBytesReader already wrapped around r.Body).
func decodeRawBody(r io.ReadCloser) string {
	buf, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	return string(buf)
}

// decodeJSONInto is a strict JSON body decoder with a size guard.
func decodeJSONInto(r io.ReadCloser, v any) error {
	return json.NewDecoder(r).Decode(v)
}
