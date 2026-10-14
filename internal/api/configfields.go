package api

import (
	"net/http"
	"sort"

	"all2api/internal/config"
)

// halveBudgetSupported implements the per-source filter decided for test
// registration A (seq=482) / PM seq=486 leaning: the 「429 后当日额度减半」
// stance is a signin-posture attribute (PRD v0.2 §7.1 / config.defaultThrottle
// — currently Qoder only), so the editable FieldDef is delivered only for
// that source class. The frozen key itself stays under
// sources.<id>.throttle.halve_budget_after_429 and in the raw config
// passthrough — no contract-surface change, no D2 needed.
func halveBudgetSupported(source string) bool {
	return source == "qoder"
}

// jitterSupported is the sibling filter decided in M2 B2 (PM ruling 6):
// jitter_minutes is a signin-schedule attribute, delivered only for the
// sources that run timed signin tasks (workbuddy, qoder). Frontend renders
// "FieldDef present = render" and never guesses source classes itself —
// the display condition lives exclusively in this delivery set. The same
// function gates the PATCH side: a field the panel was never shown cannot
// arrive as a known key.
func jitterSupported(source string) bool {
	return source == "workbuddy" || source == "qoder"
}

// FieldDef metadata delivery (M1 queue ④). The panel form is backend-driven
// (UI §5.13: 前端不硬编码表单). Wire keys/paths follow the frozen surface;
// label/description are copy KEYS, never inline text (UI §5.16).

// throttleBoundSpec is the single source of truth for an editable numeric
// config field: its wire key suffix, unit, sign-off bounds and copy keys.
// FieldDef DELIVERY (this file) and POST /api/config VALIDATION
// (config_save.go) both read from these tables, so a field the panel was
// shown is exactly the field the save endpoint accepts — the two sides can
// never drift the way a second hard-coded table would.
type throttleBoundSpec struct {
	key   string // full JSON path suffix after "sources.<id>." (e.g. "throttle.steady_rpm")
	label string
	unit  string
	min   float64
	max   float64
	desc  string
}

// throttleColumnSpecs lists the per-source numeric throttle fields (§5.20
// range sign-off v0.1: 4 narrowed maxima, lower bounds adopt backend values).
// The arrays below take a pointer into this table — do not copy the values.
var throttleColumnSpecs = []throttleBoundSpec{
	{key: "throttle.steady_rpm", label: "config.field.steady_rpm.label", unit: "rpm", min: 1, max: 120,
		desc: "config.field.steady_rpm.desc"},
	{key: "throttle.burst_rpm", label: "config.field.burst_rpm.label", unit: "rpm", min: 1, max: 300,
		desc: "config.field.burst_rpm.desc"},
	// min_gap_ms is ONE array-valued field [lo,hi] (M2 B2 ruling 2). min/max
	// below bound BOTH components; the array shape itself is documented via
	// the description copy key.
	{key: "throttle.min_gap_ms", label: "config.field.min_gap_ms.label", unit: "ms", min: 0, max: 60000,
		desc: "config.field.min_gap_ms.desc"},
	{key: "throttle.daily_cap", label: "config.field.daily_cap.label", unit: "count", min: 1, max: 10000,
		desc: "config.field.daily_cap.desc"},
	{key: "throttle.inflight", label: "config.field.inflight.label", unit: "count", min: 1, max: 32,
		desc: "config.field.inflight.desc"},
	{key: "throttle.backoff.base_429_sec", label: "config.field.base_429_sec.label", unit: "sec", min: 600, max: 86400,
		desc: "config.field.base_429_sec.desc"},
	{key: "throttle.backoff.not_found_sec", label: "config.field.not_found_sec.label", unit: "sec", min: 0, max: 3600,
		desc: "config.field.not_found_sec.desc"},
}

// jitterSpec: source-filtered (jitterSupported) — delivered and patchable
// only for signin-posture sources.
var jitterSpec = throttleBoundSpec{key: "throttle.jitter_minutes", label: "config.field.jitter_minutes.label",
	unit: "min", min: 0, max: 120, desc: "config.field.jitter_minutes.desc"}

// poolSpecs: global numeric pool fields (pooling group).
var poolSpecs = []throttleBoundSpec{
	{key: "pool.credit_floor", label: "config.field.credit_floor.label", unit: "credit", min: 0, max: 1000000},
	{key: "pool.max_retries", label: "config.field.max_retries.label", unit: "count", min: 0, max: 10},
	{key: "pool.cooldown_sec", label: "config.field.pool_cooldown_sec.label", unit: "sec", min: 0, max: 86400},
}

// globalStringSpecs: global string fields (gateway group). Config-save
// validates these with stringRules below, not numeric bounds.
var globalStringSpecs = []string{
	"listen",
	"api_key",
	"external_base_url",
}

// globalBoolSpecs: global boolean fields keyed by JSON path suffix.
var globalBoolSpecs = []string{
	"router.responses_api.enabled",
}

// cronSpecs: global cron fields (schedule group) — validated as 5-field cron.
var cronSpecs = []string{
	"schedule.signin_cron",
	"schedule.task_cron",
	"schedule.token_keepalive_cron",
}

// stringRules pins the per-key textual validation (POST /api/config): empty
// means free-form. listen is exact "host:port" shape; the regex is compiled
// once. external_base_url must be http(s) scheme + authority — the value is
// injected into the panel's __ALL2API_BASE__ inline script (G5), so a
// script-breaking or javascript:-shaped value must never land on disk.
var stringRules = map[string]string{
	"listen":            `^(:\d{1,5}|[\w.\-\[\]]+:\d{1,5})$`,
	"external_base_url": `^https?://[^\s"'<>()\\]+$`,
}

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

func fnumWithDesc(key, label, unit string, min, max float64, desc, effect string) fieldDef {
	f := fnum(key, label, unit, min, max, effect)
	f.Description = desc
	return f
}

func fbool(key, label, effect string) fieldDef {
	return fieldDef{Key: key, Label: label, Type: "boolean", Effect: effect}
}

// handleConfigFields serves GET /api/config/fields → ConfigGroup[].
// Read-only anchors (backoff.exhaust_402_until, notice_version) are NOT
// editable fields (PM ruling: 不进可编辑集) and are deliberately absent.
// Every numeric bound below comes from the shared spec tables — the same
// tables POST /api/config validates against.
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
		{Group: "pooling", Fields: numericFieldDefs(poolPointers(), "", "next_request")},
		{Group: "schedule", Fields: []fieldDef{
			{Key: "schedule.signin_cron", Label: "config.field.signin_cron.label", Type: "cron", Effect: "next_request"},
			{Key: "schedule.task_cron", Label: "config.field.task_cron.label", Type: "cron", Effect: "next_request"},
			{Key: "schedule.token_keepalive_cron", Label: "config.field.token_keepalive_cron.label", Type: "cron", Effect: "next_request"},
		}},
	}

	// Deterministic delivery order (M2 B2 ruling 7): fixed global groups first,
	// then per-source throttle/danger pairs sorted by source id — Go map
	// iteration order must never leak onto the wire.
	sourceIDs := make([]string, 0, len(cfg.Sources))
	for id := range cfg.Sources {
		sourceIDs = append(sourceIDs, id)
	}
	sort.Strings(sourceIDs)

	for _, id := range sourceIDs {
		specs := make([]*throttleBoundSpec, 0, len(throttleColumnSpecs)+2)
		for i := range throttleColumnSpecs {
			specs = append(specs, &throttleColumnSpecs[i])
		}
		if jitterSupported(id) {
			specs = append(specs, &jitterSpec)
		}
		fields := numericFieldDefs(specs, "sources."+id+".", "next_request")
		if halveBudgetSupported(id) {
			fields = append(fields,
				fbool("sources."+id+".throttle.halve_budget_after_429", "config.field.halve_budget_after_429.label", "next_request"))
		}
		groups = append(groups, configGroup{Group: "sources_throttle." + id, Fields: fields})

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

// numericFieldDefs renders spec entries as number FieldDefs. prefix is the
// JSON-path prefix prepended to each spec key ("" for global fields,
// "sources.<id>." for per-source columns); delivery and validation share the
// same table entries, so the two sides cannot drift.
func numericFieldDefs(specs []*throttleBoundSpec, prefix, effect string) []fieldDef {
	fields := make([]fieldDef, 0, len(specs))
	for _, sp := range specs {
		fields = append(fields, fnumWithDesc(prefix+sp.key, sp.label, sp.unit, sp.min, sp.max, sp.desc, effect))
	}
	return fields
}

// poolPointers returns pointers into the global pool spec table.
func poolPointers() []*throttleBoundSpec {
	out := make([]*throttleBoundSpec, 0, len(poolSpecs))
	for i := range poolSpecs {
		out = append(out, &poolSpecs[i])
	}
	return out
}
