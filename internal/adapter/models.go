package adapter

import (
	"context"
)

// Adapter defines the contract for all platform adapters.
// Each platform (WorkBuddy, Qoder, Trae, Copilot, Windsurf...)
// must implement this interface.
type Adapter interface {
	// Name returns the platform identifier, e.g. "workbuddy", "qoder", "trae".
	Name() string

	// AuthRequired returns true if the platform requires credentials.
	AuthRequired() bool

	// ConvertRequest converts a UnifiedRequest to the platform's native request format.
	ConvertRequest(req *UnifiedRequest) (*PlatformRequest, error)

	// ParseResponse converts the platform's native response to a UnifiedResponse.
	ParseResponse(resp *PlatformResponse) (*UnifiedResponse, error)

	// HealthCheck checks platform availability and returns detailed failure reason.
	// Returns four failure categories:
	//  - auth_expired: credential expired, needs user re-login
	//  - platform_down: platform is down, wait and degrade
	//  - rate_limited: rate limited, reduce frequency + cooldown
	//  - protocol_mismatch: upstream API version changed, needs patch release
	HealthCheck(ctx context.Context) (*HealthStatus, error)

	// GetTasks fetches platform-specific task list (signin, streak, cat_travel, etc.).
	GetTasks(ctx context.Context) ([]*TaskState, error)

	// ExecuteTask executes a specific task by ID (signin, active_check, token_keepalive...).
	ExecuteTask(ctx context.Context, taskID string) error

	// Capabilities returns the set of capabilities this adapter supports.
	// Used by router to decide which source can satisfy a request.
	Capabilities() CapSet

	// GetTaskSchema returns task metadata for UI component selection (declaration-based rendering).
	GetTaskSchema(ctx context.Context) ([]*TaskSchema, error)

	// CredentialStatus returns token expiration info for credential aging progress bar.
	// ok=true means valid; expiresAt shows when it will expire.
	CredentialStatus() (*CredentialInfo, error)

	// ListModels returns platform-available models for P4 panel view.
	ListModels(ctx context.Context) ([]*ModelInfo, error)
}

// Platform CapType represents adapter capability bits for router择优.
// These are the FIRST 5 bits — used by CapSet and byCap index lookups.
type CapType uint32

const (
	CapChatCompletion            CapType = 1 << iota // /v1/chat/completions
	CapResponsesStreaming                            // /v1/responses streaming
	CapResponsesToolChain                            // tool_calls chain invocation
	CapResponsesStructuredOutput                     // structured output / JSON schema
	CapResponsesReasoning                            // reasoning_content semantic
)

// _lastPlatformCap is the highest platform bit index (used by registry.byCap loop).
const _lastPlatformCap = CapResponsesReasoning

// Gateway-level capabilities (not part of platform CapSet).
// These are implementation concerns of the all2api gateway itself, not platform attributes.
// They start at a high bit position to avoid collision with platform bits.
const (
	CapSSERebuild CapType = 1 << 16 // SSE frame normalization rebuild (gateway internal)
	// Add more gateway-only caps here as needed
)

// CapSet holds the bitmask of supported capabilities.
type CapSet struct {
	Value CapType
}

// Has returns true if the given cap is included.
func (c CapSet) Has(cap CapType) bool {
	return c.Value&cap != 0
}

// AllPlatformCapabilities returns a CapSet with every platform bit set.
// Gateway-only capabilities (CapSSERebuild) are NOT included — they belong
// to the gateway implementation, not the platform contract.
var AllPlatformCapabilities = CapSet{Value: CapChatCompletion | CapResponsesStreaming | CapResponsesToolChain | CapResponsesStructuredOutput | CapResponsesReasoning}

// FailureReason classifies health check failure cause.
// Unified canonical enum across adapter level (per-account disposition) and
// circuit level (per-source flow control). When recording to UI/panel,
// ReasonConsecutive maps to protocol_mismatch equivalent, ReasonDisabled
// shows as manual override — neither enters source-level circuit display.
type FailureReason string

const (
	ReasonOK               FailureReason = "ok"                // healthy
	ReasonAuthExpired      FailureReason = "auth_expired"      // per-account removed from pool; does NOT enter source-level circuit
	ReasonPlatformDown     FailureReason = "platform_down"     // platform unreachable; wait and degrade
	ReasonRateLimited      FailureReason = "rate_limited"      // 429 — "触发限流·已降频" minute countdown
	ReasonNotFound         FailureReason = "not_found"         // 404 — "接口或模型不存在", jump to model view
	ReasonQuotaExhausted   FailureReason = "quota_exhausted"   // 402 — "额度耗尽·次日04:00重置" NEVER show as "cooling"
	ReasonProtocolMismatch FailureReason = "protocol_mismatch" // upstream API version changed; needs patch release
	ReasonTimeout          FailureReason = "timeout"           // request timeout exceeded
	ReasonDisabled         FailureReason = "disabled"          // manual disable switch (account/source); no circuit display
	ReasonConsecutive      FailureReason = "consecutive"       // consecutive failures threshold reached; maps to protocol_mismatch display
)

// HealthStatus carries health check result with detailed failure classification.
type HealthStatus struct {
	Reason    FailureReason
	Message   string
	Latency   int64 // milliseconds
	ExpiresAt int64 // Unix timestamp of credential expiry, 0 if not applicable
}

// CalendarRenderData calendar heatmap data for calendar_streak display primitive.
type CalendarRenderData struct {
	Heatmap []string `json:"heatmap"` // "YYYY-MM-DD" format dates with color class
	Streak  int      `json:"streak"`  // current streak count
}

// ProgressRenderData tier progress bar data for tier_progress display primitive.
type ProgressRenderData struct {
	Current int    `json:"current"` // current value (e.g. daily login count)
	Target  int    `json:"target"`  // target value
	Unit    string `json:"unit"`    // unit label (e.g. "次", "天")
}

// QuotaRenderData countdown data for quota_countdown display primitive.
type QuotaRenderData struct {
	RestartAt    int64  `json:"restart_at"`    // Unix timestamp of next reset
	HumanRestart string `json:"human_restart"` // server-local date string, e.g. "2026-10-13 21:00"
}

// ValidityRenderData validity gauge data for validity_gauge display primitive.
type ValidityRenderData struct {
	ExpiresAt int64  `json:"expires_at"` // Unix timestamp, 0 = platform-managed (no expiry info available)
	DaysLeft  int    `json:"days_left"`  // days remaining; negative if expired
	HumanDate string `json:"human_date"` // server-local date string
	Supported bool   `json:"supported"`  // true iff credential expiry can be detected
}

// TaskState runtime state of a scheduled task.
// Optional render fields (nil = fallback to status_plain): each corresponds
// to exactly one Display primitive variant in TaskSchema.Display.
type TaskState struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Status      string              `json:"status"` // pending, running, success, failed, skipped
	Source      string              `json:"source"` // platform source namespace for cross-source dedup
	LastRunAt   int64               `json:"last_run_at"`
	NextRunAt   int64               `json:"next_run_at"`
	SkipReason  string              `json:"skip_reason,omitempty"`
	DailyRuns   int                 `json:"daily_runs"`
	CronSpec    string              `json:"cron_spec"`
	UrgencyRank *int                `json:"urgency_rank,omitempty"` // backend-assigned priority (lower = more urgent)
	Calendar    *CalendarRenderData `json:"calendar,omitempty"`     // nil → not calendar_streak display
	Progress    *ProgressRenderData `json:"progress,omitempty"`     // nil → not tier_progress display
	Quota       *QuotaRenderData    `json:"quota,omitempty"`        // nil → not quota_countdown display
	Validity    *ValidityRenderData `json:"validity,omitempty"`     // nil → not validity_gauge display
}

// TaskSchema metadata for UI declaration-based rendering.
// ID MUST include source namespace prefix: "workbuddy.signin_09" (not bare "signin_09").
// This enables cross-source join/dedup and log traceability.
type TaskSchema struct {
	ID          string `json:"id"`            // namespaced: "{source}.{task_id}"
	Source      string `json:"source"`        // platform source: workbuddy/qoder/trae (deduplication key)
	Name        string `json:"name"`          // display name
	Type        string `json:"type"`          // business semantic: signin / active_check / cat_travel / ...
	Display     string `json:"display"`       // closed-set display primitive for component selection
	CronSpec    string `json:"cron_spec"`     // cron expression
	Description string `json:"description"`   // human-readable description
	MaxDailyRun int    `json:"max_daily_run"` // max runs per day
	Enabled     bool   `json:"enabled"`       // whether this task is currently enabled (from config)
}

// DisplayPrimitives are closed-set presentation primitives maintained by the UI team.
// Frontend maps each primitive to exactly one React component. Unknown values fall
// back to DisplayStatusPlain + console warning, ensuring new platforms require zero frontend changes.
const (
	// calendar_streak: calendar heatmap with streak count (签到场景)
	DisplayCalendarStreak = "calendar_streak"
	// tier_progress: tier progress bar (档位进度，如连登管家已解锁 x/y)
	DisplayTierProgress = "tier_progress"
	// quota_countdown: remaining time/usage countdown (猫猫旅行等一次性任务)
	DisplayQuotaCountdown = "quota_countdown"
	// validity_gauge: credential/pool validity indicator (token 保活、活跃上报)
	DisplayValidityGauge = "validity_gauge"
	// status_plain: generic plain-text fallback (所有无法分类的场景)
	DisplayStatusPlain = "status_plain"
)

// CredentialInfo credential status for frontend aging progress bar.
// Supported=true means this adapter can detect/read credential expiry;
// false → grey "expiry managed by platform", NEVER Ok:false (that renders as "expired").
type CredentialInfo struct {
	ExpiresAt int64 `json:"expires_at"` // Unix timestamp; 0 if unsupported or unknown
	Ok        bool  `json:"ok"`         // true if valid (when supported); meaningless when not supported
	Supported bool  `json:"supported"`  // true iff credential expiry is detectable
	DaysLeft  int   `json:"days_left"`  // calculated only when supported=true; negative if expired
}

// ModelInfo represents a model available on a platform. Used by P4 panel view.
type ModelInfo struct {
	ID              string   `json:"id"`               // full name: "platform/model_name" (e.g. "workbuddy/deepseek-v4-flash")
	Name            string   `json:"name"`             // display name (e.g. "DeepSeek V4 Flash")
	ContextSize     int      `json:"context_size"`     // max context window tokens
	SupportsTool    bool     `json:"supports_tool"`    // supports tool_calls
	SupportsStream  bool     `json:"supports_stream"`  // supports streaming responses
	IsFree          bool     `json:"is_free"`          // available via free quota
	MaxOutputTokens int      `json:"max_output_tokens"` // completion cap; 0 = catalog did not publish
	SupportsVision  bool     `json:"supports_vision"`   // image input; false = not published (panel falls back to its static table)
	ThinkingTiers   []string `json:"thinking_tiers"`    // reasoning tiers when the catalog publishes them
}

// UnifiedRequest the canonical request shape passed through pool → router → adapter.
type UnifiedRequest struct {
	Model    string                   `json:"model"`
	Messages []map[string]interface{} `json:"messages"`
	Stream   bool                     `json:"stream"`
	Source   string                   `json:"source"` // override: ?source= or X-Upstream header
}

// UnifiedResponse the canonical response shape returned to client.
type UnifiedResponse struct {
	ID      string                   `json:"id"`
	Model   string                   `json:"model"`
	Choices []map[string]interface{} `json:"choices"`
	Usage   map[string]interface{}   `json:"usage"`
	Error   string                   `json:"error,omitempty"`
}

// PlatformRequest abstracts the platform-specific request format.
type PlatformRequest struct {
	Raw   interface{}
	Extra map[string]string
}

// PlatformResponse abstracts the platform-specific response format.
type PlatformResponse struct {
	Raw   interface{}
	Extra map[string]string
}
