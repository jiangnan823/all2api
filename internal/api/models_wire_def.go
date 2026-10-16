package api

import "context"

// models_wire_def.go holds the build-tag-free M10 model catalog wire faces,
// extracted from panel_data.go so the models branch and the accounts branch
// own disjoint files. The route registration and the handler stay in
// panel_data.go; the per-platform catalog adapters live in models_wire.go
// (workbuddy variants) and models_wire_stub.go (qoder/trae-only builds).

// ---------- GET /api/models ----------

// modelWire is one catalog entry. The keys follow the frozen M8 wire:
// the M5a base (platform/provider/id/name/context_size/supports_tool/
// supports_stream/is_free) plus model/max_output and the three M8 additions
// (credit_multiplier/is_default/thinking_tiers). Unknown facts are null —
// never a fabricated 0/false — so nullable facts are pointer fields.
// M10-B1 appends supports_vision (additive; existing keys untouched).
type modelWire struct {
	// Platform mirrors Provider under the name the panel reads; both are sent
	// so the M5a adapter-wire tags and the M5c panel contract stay aligned.
	Platform string `json:"platform"`
	Provider string `json:"provider"`
	ID       string `json:"id"`   // namespaced: "workbuddy/deepseek-v4-flash"
	Name     string `json:"name"` // display name
	Model    string `json:"model"`

	ContextSize      *int     `json:"context_size"` // tokens; null = unknown
	MaxOutput        *int     `json:"max_output"`   // tokens; null = unknown
	SupportsTool     *bool    `json:"supports_tool"`
	SupportsStream   *bool    `json:"supports_stream"`
	IsFree           *bool    `json:"is_free"`
	CreditMultiplier *float64 `json:"credit_multiplier"` // 积分倍率; null = unknown
	IsDefault        *bool    `json:"is_default"`        // null = unknown
	ThinkingTiers    []string `json:"thinking_tiers"`    // 思考档位; null = none declared

	// SupportsVision is the image-input capability (M10-B1, additive key):
	// null = the catalog did not declare it — never a fabricated false.
	SupportsVision *bool `json:"supports_vision"`
}

type modelsResponse struct {
	// Source labels the catalog origin: "upstream" = live workbuddy directory,
	// "local" = static tables only, "mixed" = live workbuddy + static qoder.
	Source string      `json:"source"`
	Models []modelWire `json:"models"`
}

// upstreamCatalog is the optional live-catalog seam handleModels consults.
// Compiled per build tag: the workbuddy variants wire the real upstream
// directory (models_wire.go); qoder/trae-only builds get a nil constructor
// (models_wire_stub.go) and the endpoint keeps serving the static tables.
type upstreamCatalog interface {
	// fetch returns the live catalog rows; ok=false means the upstream is
	// unavailable (no credential, transport failure, empty table) and the
	// caller falls back to the static list with source "local".
	Fetch(ctx context.Context, refresh bool) (rows []modelWire, ok bool)
}
