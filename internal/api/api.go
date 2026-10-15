// Package api implements the panel HTTP surface (/api/*) served on the same
// port as the embedded panel (go:embed dist). Contracts: api-contract.ts
// v0.5.1 (wire keys snake_case per contract-json-tags-v0.5.md §1) and the
// frozen consent types in internal/adapter/consent.go.
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"all2api/internal/adapter"
	"all2api/internal/config"
	"all2api/internal/host"
)

// Register wires the M1 queue-③/④ routes (plus the wiring-batch recover
// endpoint and the M2 config-save endpoint) onto mux. The host carries the
// circuit↔pool wiring; recover is registered only when a host exists so the
// endpoint can never hang empty. The method-specific "POST /api/config"
// pattern wins over the catch-all "/api/config" read view for POST requests.
func Register(mux *http.ServeMux, h *host.Host) {
	mux.HandleFunc("/api/config", withAPIKey(handleConfig))
	mux.HandleFunc("POST /api/config", withAPIKey(handleConfigSave))
	mux.HandleFunc("POST /api/config/restore-defaults", withAPIKey(handleRestoreDefaults))
	mux.HandleFunc("/api/config/fields", withAPIKey(handleConfigFields))
	mux.HandleFunc("/api/time", withAPIKey(handleServerTime))
	mux.HandleFunc("/api/auth/verify", withAPIKey(handleVerify))
	mux.HandleFunc("/api/requests", withAPIKey(handleRequests))
	mux.HandleFunc("/api/usage", withAPIKey(handleUsage))
	registerAccountActions(mux, h)
	if h != nil {
		mux.HandleFunc("POST /api/accounts/{id}/recover", withAPIKey(handleRecover(h)))
	}
}

// withAPIKey guards the panel surface with the gateway api_key when one is
// configured. The raw config passthrough carries config.json contents, so an
// unguarded /api/* would leak it; with api_key unset the gateway is by
// definition in open-LAN mode and the panel stays usable.
func withAPIKey(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(config.Shared.Get().APIKey)
		if key != "" {
			got := r.Header.Get("X-API-Key")
			if got == "" {
				auth := r.Header.Get("Authorization")
				if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
					got = strings.TrimSpace(auth[len("Bearer "):])
				}
			}
			if got != key {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------- GET /api/config ----------

type featureFlagsResponse struct {
	ResponsesAPI      bool `json:"responses_api"`
	SessionStickiness bool `json:"session_stickiness"`
	RedisMirror       bool `json:"redis_mirror"`
	DebugBodySampling bool `json:"debug_body_sampling"`
}

type sourceAdvancedResponse struct {
	FingerprintRewrite bool `json:"request_fingerprint_rewrite"` // frozen key, per-source (F-V1 / O-6)
}

type panelConfigResponse struct {
	Raw           json.RawMessage                   `json:"raw"`
	FeatureFlags  featureFlagsResponse              `json:"feature_flags"`
	Throttle      map[string]config.ThrottleConfig  `json:"throttle"`
	Advanced      map[string]sourceAdvancedResponse `json:"advanced"`
	NoticeVersion string                            `json:"notice_version"`
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	cfg := config.Shared.Get()

	panel, err := buildPanelConfig(cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, panel)
}

// buildPanelConfig renders the frozen PanelConfig response shape from a
// config snapshot. Shared by GET /api/config and the applied payload of
// POST /api/config (ConfigSaveResponse.applied — the panel trusts the
// server-side readback, never its optimistic local values).
func buildPanelConfig(cfg config.Config) (panelConfigResponse, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return panelConfigResponse{}, err
	}

	throttle := make(map[string]config.ThrottleConfig, len(cfg.Sources))
	advanced := make(map[string]sourceAdvancedResponse, len(cfg.Sources))
	for id, src := range cfg.Sources {
		throttle[id] = src.Throttle
		advanced[id] = sourceAdvancedResponse{FingerprintRewrite: src.Advanced.RequestFingerprintRewrite}
	}

	flags := featureFlagsResponse{ResponsesAPI: cfg.Router.Responses.Enabled}
	// Purely additive optional global flags: read from the raw passthrough so
	// unset keys default false (safe default) without touching the frozen
	// config schema (C4 backlog mode — not a freeze break).
	var extras map[string]json.RawMessage
	_ = json.Unmarshal(raw, &extras)
	flags.SessionStickiness = boolAt(extras, "session_stickiness")
	flags.RedisMirror = boolAt(extras, "redis_mirror")
	flags.DebugBodySampling = boolAt(extras, "debug_body_sampling")

	return panelConfigResponse{
		Raw:           raw,
		FeatureFlags:  flags,
		Throttle:      throttle,
		Advanced:      advanced,
		NoticeVersion: cfg.NoticeVersion,
	}, nil
}

func boolAt(m map[string]json.RawMessage, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	var b bool
	_ = json.Unmarshal(v, &b)
	return b
}

// ---------- POST /api/auth/verify ----------

type authVerifyRequest struct {
	Source  string              `json:"source"`
	Token   string              `json:"token,omitempty"`
	Consent *adapter.ConsentAck `json:"consent,omitempty"`
}

type quotaResponse struct {
	Kind  string `json:"kind"`
	Used  int64  `json:"used"`
	Total int64  `json:"total,omitempty"`
	Unit  string `json:"unit"`
}

type credentialStatusResponse struct {
	Supported     bool               `json:"supported"`
	Valid         bool               `json:"valid"`
	ExpiresAt     *adapter.TimeLocal `json:"expires_at"`
	DaysLeftCache int                `json:"days_left_cache,omitempty"`
}

type authVerifyResponse struct {
	OK           bool                      `json:"ok"`
	AccountLabel string                    `json:"account_label,omitempty"`
	Quota        *quotaResponse            `json:"quota,omitempty"`
	Credential   *credentialStatusResponse `json:"credential,omitempty"`
	ReasonCode   adapter.FailureReason     `json:"reason_code,omitempty"`
	FieldErrors  []adapter.FieldError      `json:"field_errors,omitempty"`
}

// requiresConsent encodes the product rule (Q11 / PRD §6⑥): Qoder always
// requires informed consent before its credentials may be saved. Adding
// consent-required sources would go through D2, not silently here.
func requiresConsent(source string) bool {
	return source == "qoder"
}

func handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req authVerifyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Source) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source is required"})
		return
	}

	cfg := config.Shared.Get()

	if requiresConsent(req.Source) {
		if fe := adapter.CheckConsent(req.Consent, req.Source, cfg.NoticeVersion); fe != nil {
			// Consent rejections travel exclusively through field_errors with
			// machine codes; they never extend the FailureReason display table
			// (G3 hard rule — REASON_COPY / 402 display stays unpolluted).
			writeJSON(w, http.StatusOK, authVerifyResponse{
				OK:          false,
				FieldErrors: []adapter.FieldError{*fe},
			})
			return
		}
		if err := appendConsentTrail(cfg.StateFile, adapter.ConsentTrail{
			Source:        req.Source,
			NoticeVersion: cfg.NoticeVersion,
			AckAt:         nowLocal(),
		}); err != nil {
			// The trail is the acceptance evidence itself — refusing to lose it
			// is safer than accepting a consent save we cannot attest.
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "consent trail write failed"})
			return
		}
	}

	a := adapter.Default.Get(req.Source)
	if a == nil {
		// Source configured but not compiled into this binary variant (build
		// tag) or unknown: verification is genuinely unimplemented here.
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "verification unavailable for this source"})
		return
	}

	hs, err := a.HealthCheck(r.Context())
	resp := authVerifyResponse{}
	switch {
	case err != nil || hs == nil:
		resp.ReasonCode = adapter.ReasonPlatformDown
	case hs.Reason != adapter.ReasonOK:
		resp.ReasonCode = hs.Reason
	default:
		resp.OK = true
		if ci, ciErr := a.CredentialStatus(); ciErr == nil && ci != nil {
			cred := &credentialStatusResponse{
				Supported:     ci.Supported,
				Valid:         ci.Ok,
				DaysLeftCache: ci.DaysLeft,
			}
			if ci.Supported && ci.ExpiresAt > 0 {
				cred.ExpiresAt = unixLocal(ci.ExpiresAt)
			}
			resp.Credential = cred
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
