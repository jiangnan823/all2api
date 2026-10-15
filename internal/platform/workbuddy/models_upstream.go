//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// models_upstream.go implements the M8 live upstream model catalog: the panel
// model page pulls the real directory (credit multiplier / thinking tiers /
// context / max output) instead of serving only the static name list.
//
// Endpoint (report §2, wb2api-verified):
//
//	GET {chat}/v2/enterprises/personal/models          personal accounts
//	GET {chat}/v2/enterprises/{enterpriseId}/models    enterprise accounts
//
// The reply is the {code,msg,data} envelope; data carries the model table.
// Response field names follow the wb2api reference implementation
// (workbuddy2api-panel internal/upstream/client.go dynModelEntry — the CN and
// global directories are the same shape), extracted verbatim:
//
//	id / name / modelId / model / descriptionZh / credits / tags / vendor /
//	isDefault / maxInputTokens / maxOutputTokens / maxAllowedSize / disabled /
//	supportsImages / supportsReasoning / supportsToolCall / onlyReasoning /
//	reasoning{effort,summary,defaultEffort,canDisableThinking,supportedEfforts}
//
// credits is a raw string ("x0.05 credits" / "x0.29" / "0x") — the numeric
// multiplier is extracted for the panel's credit_multiplier wire field.
// Everything the upstream omits stays a zero value; the wire layer maps those
// to null (never a fabricated default).
//
// The catalog is a display surface, not the chat-usable list: entries are
// only filtered on empty id and disabled=true (wb2api's /v2 parse applies the
// same two skips; its agents[cli]/nonChatModel narrowing is a chat-path
// concern the panel does not inherit).
//
// Caching: success is cached for 60s (catalogTTL); a failure starts the same
// 60s cooldown so a stale panel cannot hammer a downed upstream. refresh
// bypasses both — the panel's refresh=1 is an explicit force. The client is
// stdlib-only; bases are injectable via TaskClientOptions, so tests run
// against httptest stubs only — the suite never contacts the real endpoints.
package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// catalogModelsPathPersonal is the personal-account directory path (report
// §2). Enterprise accounts swap "personal" for the enterprise id.
const catalogModelsPathPersonal = "/v2/enterprises/personal/models"

// freeBadgeTag marks a limited-time-free model in the upstream tags (wb2api
// client.go ModelInfo.Tags: "含 badge:限时免费 等"). It is the only sourced
// free indicator the directory carries.
const freeBadgeTag = "badge:限时免费"

// catalogTTL is the success cache lifetime AND the failure cooldown: one
// duration, one mental model — the catalog is at most a minute stale either way.
const catalogTTL = 60 * time.Second

// CatalogModel is one upstream directory entry mapped onto the panel's needs.
// Zero values mean "upstream omitted" — the wire layer nulls them (never 0 /
// false as a fake fact).
type CatalogModel struct {
	Model        string // bare upstream id ("deepseek-v4-flash")
	Name         string // upstream display name (may be empty)
	Vendor       string // vendor marker
	Description  string // descriptionZh
	ContextSize  int64  // maxInputTokens; 0 = omitted
	MaxOutput    int64  // maxOutputTokens; 0 = omitted
	SupportsTool bool   // supportsToolCall as declared (absent = false)

	// SupportsStream is a protocol fact, not a directory field: the chat path
	// is stream-mandatory (report §4), so every directory entry streams.
	SupportsStream bool

	// IsFree is badge-derived (freeBadgeTag); false = no badge = unknown, and
	// the wire layer sends null rather than a bare false.
	IsFree bool

	// CreditMultiplier is parsed from the credits string ("x0.05 credits" →
	// 0.05); nil = absent or unparsable.
	CreditMultiplier *float64

	IsDefault bool

	// Thinking tiers: reasoning.supportedEfforts verbatim; nil = none declared.
	ThinkingTiers []string

	SupportsReasoning  bool
	CanDisableThinking bool
	DefaultEffort      string // defaultEffort, falling back to the legacy effort key
}

// catalogEntry is the wire-decode form of one directory object (wb2api
// dynModelEntry field names verbatim).
type catalogEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	ModelID string `json:"modelId"` // loose fallback keys (global variants)
	Model   string `json:"model"`

	Credits      string   `json:"credits"`
	Tags         []string `json:"tags"`
	Vendor       string   `json:"vendor"`
	Description  string   `json:"descriptionZh"`
	IsDefault    bool     `json:"isDefault"`
	Disabled     bool     `json:"disabled"`
	MaxInputTok  int64    `json:"maxInputTokens"`
	MaxOutputTok int64    `json:"maxOutputTokens"`

	SupportsReason bool `json:"supportsReasoning"`
	SupportsTool   bool `json:"supportsToolCall"`

	Reasoning struct {
		Effort             string   `json:"effort"`
		Summary            string   `json:"summary"`
		DefaultEffort      string   `json:"defaultEffort"`
		CanDisableThinking bool     `json:"canDisableThinking"`
		SupportedEfforts   []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// catalogModels decodes the {code,msg,data} envelope's model table. The
// non-zero business-code rejection already happened in do() (taskReplyText);
// an empty table here is an error — the caller falls back to the static list.
func catalogModels(text string) ([]CatalogModel, error) {
	var env struct {
		Data struct {
			Models []catalogEntry `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return nil, fmt.Errorf("workbuddy: models parse: %w", err)
	}
	if len(env.Data.Models) == 0 {
		return nil, errors.New("workbuddy: models api returned empty list")
	}
	out := make([]CatalogModel, 0, len(env.Data.Models))
	for _, m := range env.Data.Models {
		id := m.ID
		for _, k := range []string{m.ModelID, m.Model, m.Name} {
			if id != "" {
				break
			}
			id = k
		}
		if id == "" || m.Disabled {
			continue // wb2api's two skips: no id, or explicitly disabled
		}
		cm := CatalogModel{
			Model:              id,
			Name:               m.Name,
			Vendor:             m.Vendor,
			ContextSize:        m.MaxInputTok,
			MaxOutput:          m.MaxOutputTok,
			SupportsTool:       m.SupportsTool,
			SupportsStream:     true, // stream-mandatory chat path (report §4)
			IsDefault:          m.IsDefault,
			SupportsReasoning:  m.SupportsReason,
			CanDisableThinking: m.Reasoning.CanDisableThinking,
			DefaultEffort:      m.Reasoning.DefaultEffort,
			ThinkingTiers:      m.Reasoning.SupportedEfforts,
		}
		if cm.DefaultEffort == "" {
			cm.DefaultEffort = m.Reasoning.Effort // legacy single-effort key
		}
		cm.Description = m.Description
		if x, ok := parseCreditMultiplier(m.Credits); ok {
			cm.CreditMultiplier = &x
		}
		for _, t := range m.Tags {
			if t == freeBadgeTag {
				cm.IsFree = true
				break
			}
		}
		out = append(out, cm)
	}
	if len(out) == 0 {
		return nil, errors.New("workbuddy: models api returned no usable entries")
	}
	return out, nil
}

// parseCreditMultiplier extracts the numeric multiplier from the raw credits
// string ("x0.05 credits" → 0.05, "0x" → 0, "x0.29" → 0.29). ok=false when
// the string carries no parsable number — the wire sends null, never a guess.
func parseCreditMultiplier(raw string) (float64, bool) {
	var b strings.Builder
	for _, r := range raw {
		if (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" || s == "." {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// ModelCatalog serves the upstream directory behind a 60s TTL cache (success
// and failure alike). Safe for concurrent use; one instance per panel backend.
type ModelCatalog struct {
	client *TaskClient

	mu        sync.Mutex
	entries   []CatalogModel
	fetchedAt time.Time
	failedAt  time.Time
}

// NewModelCatalog binds the catalog cache to a task client (whose options
// carry the credential source and the test clock override).
func NewModelCatalog(c *TaskClient) *ModelCatalog {
	return &ModelCatalog{client: c}
}

// Get returns the directory, consulting the cache unless refresh forces a
// live fetch. ok=false means "upstream unavailable right now" — the caller
// falls back to the static list and labels the response local.
func (m *ModelCatalog) Get(ctx context.Context, cred *Credential, refresh bool) ([]CatalogModel, bool) {
	if m == nil || cred == nil || cred.AccessToken == "" {
		return nil, false
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !refresh {
		if len(m.entries) > 0 && now.Sub(m.fetchedAt) < catalogTTL {
			return m.entries, true
		}
		if !m.failedAt.IsZero() && now.Sub(m.failedAt) < catalogTTL {
			return nil, false // failure cooldown: do not re-hammer a downed upstream
		}
	}
	entries, err := m.client.ModelCatalog(ctx, cred)
	if err != nil {
		m.failedAt = now
		return nil, false
	}
	m.entries = entries
	m.fetchedAt = now
	m.failedAt = time.Time{}
	return entries, true
}

// now is the clock, honoring the task client's test override.
func (m *ModelCatalog) now() time.Time {
	if fn := m.client.opts.Now; fn != nil {
		return fn()
	}
	return time.Now()
}

// ModelCatalog fetches the live directory once (no cache — ModelCatalog.Get
// is the cached entry point). Personal vs enterprise path follows the
// credential's EnterpriseID (report §2).
func (c *TaskClient) ModelCatalog(ctx context.Context, cred *Credential) ([]CatalogModel, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, errors.New("workbuddy: model catalog needs a credential with an access token")
	}
	path := catalogModelsPathPersonal
	if cred.EnterpriseID != "" {
		path = "/v2/enterprises/" + url.PathEscape(cred.EnterpriseID) + "/models"
	}
	text, err := c.do(ctx, cred, http.MethodGet, path, nil, false)
	if err != nil {
		return nil, err
	}
	return catalogModels(text)
}
