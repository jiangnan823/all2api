package api

// panel_data.go implements the M5a read-only panel data exits:
//
//   GET /api/accounts — pool snapshot + top-level aggregation
//   GET /api/tasks    — scheduler task list + per-task last RunStats
//   GET /api/models   — per-platform model catalogs (workbuddy live upstream
//                       directory behind a 60s cache, static tables otherwise)
//   GET /api/logs     — in-process ring buffer of operational log lines
//
// All four are guarded by withAPIKey like the rest of /api/*. Wire keys are
// snake_case (contract-json-tags-v0.5.md §1). api-contract.ts has no frozen
// type for these four endpoints yet, so the shapes below are the new
// reference: the keys are listed in the M5a delivery report for the frontend
// batch to align against.
//
// Honesty markers: /api/models labels its origin (source:"upstream" for the
// live workbuddy directory, "local" for the static tables, "mixed" when both
// contribute — unknown metadata is null, never invented); /api/logs serves
// only what was explicitly wired into the ring this round (credit seeding,
// schedule RunStats, recover outcomes, startup banner) — it is not a
// request-access log.
//
// Routes are registered with method-specific patterns, so a non-GET request
// answers 405 from ServeMux itself; the handlers stay check-free.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"all2api/internal/adapter"
	"all2api/internal/config"
	"all2api/internal/host"
	"all2api/internal/observability"
	"all2api/internal/pool"
	"all2api/internal/schedule"
)

// RegisterPanelData wires the four read-only endpoints onto mux. Split from
// Register so the existing registration path stays untouched (the M5a batch
// adds routes, changes none; M6a appends the account write routes
// pause/resume/remove behind the same nil-host/nil-store guard as the
// add-account routes; M7a appends the workbuddy growth task endpoints behind
// the same guard). All parameters are nullable: nil host/empty
// pool, nil scheduler, nil stats memory and nil ring each degrade to the
// honest empty response, never a panic.
func RegisterPanelData(mux *http.ServeMux, h *host.Host, sched TaskSource, stats *schedule.StatsMemory, ring *observability.Ring) {
	mux.HandleFunc("GET /api/accounts", withAPIKey(handleAccounts(h)))
	mux.HandleFunc("GET /api/tasks", withAPIKey(handleTasks(sched, stats)))
	mux.HandleFunc("GET /api/models", withAPIKey(handleModels(h)))
	mux.HandleFunc("GET /api/logs", withAPIKey(handleLogs(ring)))
	registerAddAccount(mux, h)
	registerAccountWrite(mux, h)
	registerGrowth(mux, h)
	registerLogin(mux, h) // M8: browser device-authorization login endpoints
}

// TaskSource is the read-only scheduler view panel_data needs (schedule
// *Scheduler satisfies it; tests use a stub). Kept as an interface so this
// package stays decoupled from the scheduler's lifecycle methods.
type TaskSource interface {
	GetTasks() []*schedule.Task
}

// ---------- GET /api/accounts ----------

// accountWire is one pooled account on the wire. Field values come from
// pool.Account value snapshots (SnapshotAccounts); times use the Q9 dual
// TimeLocal representation (unix for comparisons, local for display) and a
// zero time serializes as null via the pointer form.
//
// M6a appended the observation/pause block after last_selected (frozen key
// order: the pre-M6a keys stay first and untouched — the frontend table was
// built against this exact shape):
//
//	success     — successful send count this process (UpdateHealth "success")
//	fail        — failed send count this process (429/5xx/timeout/auth)
//	in_flight   — requests currently mid-send for this account
//	paused      — selection pause switch (Select skips paused accounts)
//	last_success — most recent successful send, null until the first one
//	credit_total — upstream quota capacity; 0 = unknown/not yet seeded
type accountWire struct {
	Platform     string             `json:"platform"`
	ID           string             `json:"id"`
	Nickname     string             `json:"nickname"` // display name from the auth file; empty → client falls back to id
	Status       string             `json:"status"`
	Credit       int64              `json:"credit"`
	Health       float64            `json:"health"`
	ExpiresAt    *adapter.TimeLocal `json:"expires_at"`
	CooldownEnd  *adapter.TimeLocal `json:"cooldown_end"`
	LastSelected *adapter.TimeLocal `json:"last_selected"`
	Success      int64              `json:"success"`
	Fail         int64              `json:"fail"`
	InFlight     int64              `json:"in_flight"`
	Paused       bool               `json:"paused"`
	LastSuccess  *adapter.TimeLocal `json:"last_success"`
	CreditTotal  int64              `json:"credit_total"`
}

// accountsResponse: the account array plus the aggregation flattened at the
// top level (M5a spec: "JSON 数组 + 顶层聚合 {total, active, cooling,
// disabled, credit_total}").
type accountsResponse struct {
	Accounts    []accountWire `json:"accounts"`
	Total       int           `json:"total"`
	Active      int           `json:"active"`
	Cooling     int           `json:"cooling"`
	Disabled    int           `json:"disabled"`
	CreditTotal int64         `json:"credit_total"`
}

func handleAccounts(h *host.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := accountsResponse{Accounts: []accountWire{}}
		if h != nil && h.Pool != nil && h.Creds != nil {
			snapshot := h.Pool.SnapshotAccounts()
			for _, acc := range snapshot {
				w := accountFromPool(acc)
				w.Nickname = h.Creds.NicknameFor(acc.Platform, acc.ID)
				resp.Accounts = append(resp.Accounts, w)
				switch acc.Status {
				case "cooling":
					resp.Cooling++
				case "disabled":
					resp.Disabled++
				case "active":
					resp.Active++
				}
				resp.CreditTotal += acc.Credit
			}
			resp.Total = len(snapshot)
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// accountFromPool maps a pool.Account value copy onto the wire shape.
func accountFromPool(acc pool.Account) accountWire {
	return accountWire{
		Platform:     acc.Platform,
		ID:           acc.ID,
		Status:       acc.Status,
		Credit:       acc.Credit,
		Health:       acc.Health,
		ExpiresAt:    timeLocalPtr(acc.ExpiresAt),
		CooldownEnd:  timeLocalPtr(acc.CooldownEnd),
		LastSelected: timeLocalPtr(acc.LastSelected),
		Success:      acc.SuccessCount,
		Fail:         acc.FailCount,
		InFlight:     acc.InFlight,
		Paused:       acc.Paused,
		LastSuccess:  timeLocalPtr(acc.LastSuccess),
		CreditTotal:  acc.CreditTotal,
	}
}

// ---------- GET /api/tasks ----------

// lastRunWire is one task's most recent batch counters (schedule.RunStats)
// plus the wall-clock time the batch finished. null on the wire when the
// task has never finished a run.
type lastRunWire struct {
	Name    string            `json:"name"`
	Total   int               `json:"total"`
	Success int               `json:"success"`
	Failed  int               `json:"failed"`
	Skipped int               `json:"skipped"`
	At      adapter.TimeLocal `json:"at"`
}

type taskWire struct {
	Name    string       `json:"name"`
	Cron    string       `json:"cron"`
	Enabled bool         `json:"enabled"`
	LastRun *lastRunWire `json:"last_run"`
}

type tasksResponse struct {
	Tasks []taskWire `json:"tasks"`
}

func handleTasks(sched TaskSource, stats *schedule.StatsMemory) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := tasksResponse{Tasks: []taskWire{}}
		if sched != nil {
			tasks := sched.GetTasks()
			// Deterministic delivery order: GetTasks ranges a map.
			sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
			for _, t := range tasks {
				tw := taskWire{Name: t.Name, Cron: t.CronSpec, Enabled: t.Enabled}
				if stats != nil {
					if s, at, ok := stats.LastRun(t.Name); ok {
						tw.LastRun = &lastRunWire{
							Name:    s.Task,
							Total:   s.Total,
							Success: s.Success,
							Failed:  s.Failed,
							Skipped: s.Skipped,
							At:      timeLocalVal(at),
						}
					}
				}
				resp.Tasks = append(resp.Tasks, tw)
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// ---------- GET /api/models ----------

// modelWire is one catalog entry. The keys follow the frozen M8 wire:
// the M5a base (platform/provider/id/name/context_size/supports_tool/
// supports_stream/is_free) plus model/max_output and the three M8 additions
// (credit_multiplier/is_default/thinking_tiers). Unknown facts are null —
// never a fabricated 0/false — so nullable facts are pointer fields.
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

// workbuddyDefaultModels is the known-good default model name list for the
// workbuddy source (README quickstart: deepseek-v4-flash et al). The config
// model_map is a contract-frozen field and normally empty, so without this
// constant the workbuddy column would render empty although the names are
// real and request-path usable.
var workbuddyDefaultModels = []string{
	"deepseek-v4-flash",
	"glm-5.3-flash",
	"kimi-k3",
	"minimax-m3",
	"hy3",
	"hunyuan",
}

// handleModels serves GET /api/models over the production catalog seam
// (nil without a credential store — the static tables answer, source local).
func handleModels(h *host.Host) http.HandlerFunc {
	var cat upstreamCatalog
	if h != nil {
		cat = newModelsCatalog(h)
	}
	return modelsEndpointHandler(cat)
}

// modelsEndpointHandler is the seam-carrying form tests mount over a stubbed
// catalog (same split as the growth handlers): qoder stays static, the
// workbuddy half prefers the live upstream directory (?refresh=1 forces a
// fetch past the 60s cache) and falls back to the static name list on any
// failure — honestly labeled local.
func modelsEndpointHandler(cat upstreamCatalog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		refresh := r.URL.Query().Get("refresh") == "1"
		resp := modelsResponse{Source: "local", Models: []modelWire{}}

		// qoder: static fallback catalog (display-name → qoder key snapshot,
		// chat.go defaultModelMap) via the adapter's ListModels. Static by
		// construction — it never flips the source to upstream.
		if a := adapter.Default.Get("qoder"); a != nil {
			if infos, err := a.ListModels(r.Context()); err == nil {
				for _, m := range infos {
					if m == nil {
						continue
					}
					// The static table only names models and marks streaming;
					// every other fact stays null (undeclared, not false).
					stream := m.SupportsStream
					resp.Models = append(resp.Models, modelWire{
						Platform:       "qoder",
						Provider:       "qoder",
						ID:             m.ID,
						Name:           m.Name,
						Model:          strings.TrimPrefix(m.ID, "qoder/"),
						SupportsStream: &stream,
					})
				}
			}
		}

		// workbuddy: live upstream directory first (M8), static name list on
		// any failure (no credential / transport / parse / empty table).
		upstreamOK := false
		if cat != nil {
			if rows, ok := cat.Fetch(r.Context(), refresh); ok {
				resp.Models = append(resp.Models, rows...)
				upstreamOK = true
			}
		}
		if !upstreamOK {
			// config model_map (frozen surface, usually empty) first, then the
			// known default names not already covered by a map entry. The
			// static list carries names only — metadata stays null.
			known := map[string]bool{}
			for name := range config.Shared.Get().ModelMap {
				resp.Models = append(resp.Models, localWorkbuddyRow(name))
				known[name] = true
			}
			for _, name := range workbuddyDefaultModels {
				if known[name] {
					continue
				}
				resp.Models = append(resp.Models, localWorkbuddyRow(name))
			}
		}

		// Source honesty: live workbuddy + static qoder = mixed; live only =
		// upstream; anything that touched a static table only = local.
		if upstreamOK {
			resp.Source = "upstream"
			for _, m := range resp.Models {
				if m.Platform == "qoder" {
					resp.Source = "mixed"
					break
				}
			}
		}

		// Deterministic delivery order: by provider, then id.
		sort.Slice(resp.Models, func(i, j int) bool {
			if resp.Models[i].Provider != resp.Models[j].Provider {
				return resp.Models[i].Provider < resp.Models[j].Provider
			}
			return resp.Models[i].ID < resp.Models[j].ID
		})
		writeJSON(w, http.StatusOK, resp)
	}
}

// localWorkbuddyRow is one static workbuddy name-list entry: namespaced id,
// the bare name everywhere else, metadata null (the static list has none).
func localWorkbuddyRow(name string) modelWire {
	return modelWire{
		Platform: "workbuddy",
		Provider: "workbuddy",
		ID:       "workbuddy/" + name,
		Name:     name,
		Model:    name,
	}
}

// boolPtr / intPtr / float64Ptr are the nullable-wire helpers: they give the
// pointer fields a concrete value so the zero value can keep meaning null.
func boolPtr(v bool) *bool          { return &v }
func intPtr(v int) *int             { return &v }
func float64Ptr(v float64) *float64 { return &v }

// ---------- GET /api/logs ----------

// logWire is one ring entry on the panel wire. level is the two-value panel
// vocabulary ("info"/"error"); ts is the Q9 dual time representation.
type logWire struct {
	Level string             `json:"level"`
	Msg   string             `json:"msg"`
	TS    *adapter.TimeLocal `json:"ts"`
}

type logsResponse struct {
	Logs []logWire `json:"logs"`
}

func handleLogs(ring *observability.Ring) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := logsResponse{Logs: []logWire{}}
		if ring != nil {
			for _, e := range ring.Snapshot() {
				resp.Logs = append(resp.Logs, logWire{
					Level: observability.WireLevel(e.Level),
					Msg:   e.Msg,
					TS:    timeLocalPtr(e.TS),
				})
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// ---------- shared helpers ----------

// timeLocalPtr converts a time.Time into the Q9 dual wire representation;
// the zero time serializes as null (never "0001-01-01 00:00:00").
func timeLocalPtr(t time.Time) *adapter.TimeLocal {
	if t.IsZero() {
		return nil
	}
	tl := timeLocalVal(t)
	return &tl
}

// timeLocalVal is the non-pointer form of timeLocalPtr (non-null fields).
func timeLocalVal(t time.Time) adapter.TimeLocal {
	return adapter.TimeLocal{Unix: t.Unix(), Local: t.Format(localLayout)}
}
