package api

import (
	"net/http"

	"all2api/internal/config"
)

// FieldDef metadata delivery (M1 queue ④). The panel form is backend-driven
// (UI §5.13: 前端不硬编码表单). Wire keys/paths follow the frozen surface;
// label/description are copy KEYS, never inline text (UI §5.16).

type fieldDef struct {
	Key             string   `json:"key"`
	Label           string   `json:"label"`
	Type            string   `json:"type"` // number|string|boolean|cron|select
	Unit            string   `json:"unit,omitempty"`
	Min             *float64 `json:"min,omitempty"`
	Max             *float64 `json:"max,omitempty"`
	Options         []string `json:"options,omitempty"`
	Description     string   `json:"description,omitempty"`
	Effect          string   `json:"effect"` // immediate|next_request|restart_required
	RequiresConfirm bool     `json:"requires_confirm,omitempty"`
}

type configGroup struct {
	Group  string     `json:"group"`
	Fields []fieldDef `json:"fields"`
}

func fnum(key, label, unit string, min, max float64, effect string) fieldDef {
	lo, hi := min, max
	return fieldDef{Key: key, Label: label, Type: "number", Unit: unit, Min: &lo, Max: &hi, Effect: effect}
}

func fbool(key, label, effect string) fieldDef {
	return fieldDef{Key: key, Label: label, Type: "boolean", Effect: effect}
}

// handleConfigFields serves GET /api/config/fields → ConfigGroup[].
// Read-only anchors (backoff.exhaust_402_until, notice_version) are NOT
// editable fields (PM ruling: 不进可编辑集) and are deliberately absent.
// Ranges: PRD §7.1 pins base_429_sec ≥ 600; all other bounds are engineering
// guardrails pending UI §5.20 column-range sign-off — they never rename keys.
func handleConfigFields(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	cfg := config.Shared.Get()

	groups := []configGroup{
		{Group: "gateway", Fields: []fieldDef{
			{Key: "listen", Label: "config.field.listen.label", Type: "string", Effect: "restart_required"},
			{Key: "api_key", Label: "config.field.api_key.label", Type: "string", Effect: "restart_required"},
			{Key: "external_base_url", Label: "config.field.external_base_url.label", Type: "string",
				Description: "config.field.external_base_url.desc", Effect: "next_request"},
		}},
		{Group: "routing", Fields: []fieldDef{
			{Key: "router.responses_api.enabled", Label: "config.field.responses_api.label", Type: "boolean", Effect: "next_request"},
		}},
		{Group: "pooling", Fields: []fieldDef{
			fnum("pool.credit_floor", "config.field.credit_floor.label", "credit", 0, 1000000, "next_request"),
			fnum("pool.max_retries", "config.field.max_retries.label", "count", 0, 10, "next_request"),
			fnum("pool.cooldown_sec", "config.field.pool_cooldown_sec.label", "sec", 0, 86400, "next_request"),
		}},
		{Group: "schedule", Fields: []fieldDef{
			{Key: "schedule.signin_cron", Label: "config.field.signin_cron.label", Type: "cron", Effect: "next_request"},
			{Key: "schedule.task_cron", Label: "config.field.task_cron.label", Type: "cron", Effect: "next_request"},
			{Key: "schedule.token_keepalive_cron", Label: "config.field.token_keepalive_cron.label", Type: "cron", Effect: "next_request"},
		}},
	}

	// Per-source groups generated from configured sources — frozen paths only.
	for id := range cfg.Sources {
		p := "sources." + id + ".throttle."
		th := configGroup{Group: "sources_throttle." + id, Fields: []fieldDef{
			fnum(p+"steady_rpm", "config.field.steady_rpm.label", "rpm", 1, 10000, "next_request"),
			fnum(p+"burst_rpm", "config.field.burst_rpm.label", "rpm", 1, 10000, "next_request"),
			fnum(p+"min_gap_ms.lo", "config.field.min_gap_ms_lo.label", "ms", 0, 60000, "next_request"),
			fnum(p+"min_gap_ms.hi", "config.field.min_gap_ms_hi.label", "ms", 0, 60000, "next_request"),
			fnum(p+"daily_cap", "config.field.daily_cap.label", "count", 1, 1000000, "next_request"),
			fnum(p+"inflight", "config.field.inflight.label", "count", 1, 64, "next_request"),
			fnum(p+"backoff.base_429_sec", "config.field.base_429_sec.label", "sec", 600, 86400, "next_request"),
			fnum(p+"backoff.not_found_sec", "config.field.not_found_sec.label", "sec", 0, 3600, "next_request"),
			fnum(p+"jitter_minutes", "config.field.jitter_minutes.label", "min", 0, 120, "next_request"),
			fbool(p+"halve_budget_after_429", "config.field.halve_budget_after_429.label", "next_request"),
		}}
		groups = append(groups, th)

		adv := configGroup{Group: "danger." + id, Fields: []fieldDef{
			{Key: "sources." + id + ".advanced.request_fingerprint_rewrite",
				Label:           "config.field.fingerprint_rewrite.label",
				Type:            "boolean",
				Description:     "config.field.fingerprint_rewrite.desc",
				Effect:          "next_request",
				RequiresConfirm: true}, // DangerAction 二次确认, default focus 保持关闭 (UI §7)
			{Key: "sources." + id + ".enabled",
				Label: "config.field.source_enabled.label", Type: "boolean", Effect: "next_request"},
		}}
		groups = append(groups, adv)
	}

	writeJSON(w, http.StatusOK, groups)
}
