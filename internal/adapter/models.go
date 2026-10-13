package adapter

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
	HealthCheck(ctx interface{}) (*HealthStatus, error)

	// GetTasks fetches platform-specific task list (signin, streak, cat_travel, etc.).
	GetTasks(ctx interface{}) ([]*TaskState, error)

	// ExecuteTask executes a specific task by ID (signin, active_check, token_keepalive...).
	ExecuteTask(ctx interface{}, taskID string) error

	// Capabilities returns the set of capabilities this adapter supports.
	// Used by router to decide which source can satisfy a request.
	Capabilities() CapSet

	// GetTaskSchema returns task metadata for UI component selection (declaration-based rendering).
	GetTaskSchema(ctx interface{}) ([]*TaskSchema, error)

	// CredentialStatus returns token expiration info for credential aging progress bar.
	// ok=true means valid; expiresAt shows when it will expire.
	CredentialStatus() (*CredentialInfo, error)

	// ListModels returns platform-available models for P4 panel view.
	ListModels(ctx interface{}) ([]*ModelInfo, error)
}

// CapType represents adapter capability bits.
type CapType uint32

const (
	CapChatCompletion CapType = 1 << iota // /v1/chat/completions
	CapResponsesStreaming                  // /v1/responses streaming
	CapResponsesToolChain                  // tool_calls chain invocation
	CapResponsesStructuredOutput           // structured output / JSON schema
	CapResponsesReasoning                  // reasoning_content semantic
	CapSSERebuild                           // SSE frame normalization rebuild
)

// CapSet holds the bitmask of supported capabilities.
type CapSet struct {
	Value CapType
}

// Has returns true if the given cap is included.
func (c CapSet) Has(cap CapType) bool {
	return c.Value&cap != 0
}

// AllCapabilities returns a CapSet with every bit set.
var AllCapabilities = CapSet{Value: CapChatCompletion | CapResponsesStreaming | CapResponsesToolChain | CapResponsesStructuredOutput | CapResponsesReasoning | CapSSERebuild}

// FailureReason classifies health check failure cause.
type FailureReason string

const (
	ReasonOK             FailureReason = "ok"
	ReasonAuthExpired    FailureReason = "auth_expired"
	ReasonPlatformDown   FailureReason = "platform_down"
	ReasonRateLimited    FailureReason = "rate_limited"
	ReasonProtocolMismatch FailureReason = "protocol_mismatch"
	ReasonTimeout        FailureReason = "timeout"
)

// HealthStatus carries health check result with detailed failure classification.
type HealthStatus struct {
	Reason   FailureReason
	Message  string
	Latency  int64  // milliseconds
	ExpiresAt int64 // Unix timestamp of credential expiry, 0 if not applicable
}

// TaskState runtime state of a scheduled task.
type TaskState struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"` // pending, running, success, failed, skipped
	LastRunAt   int64  `json:"last_run_at"`
	NextRunAt   int64  `json:"next_run_at"`
	SkipReason  string `json:"skip_reason,omitempty"` // why skipped: cooldown, credit_floor, disabled
	DailyRuns   int    `json:"daily_runs"`            // runs today
	CronSpec    string `json:"cron_spec"`
}

// TaskSchema metadata for UI declaration-based rendering.
// Two fields for display: Type (business semantic for filtering/logs)
// and Display (closed-set presentation primitive for component selection).
// This separation enables "zero frontend changes when adding new platforms" —
// unknown Display values fall back to DisplayStatusPlain with a console warning.
type TaskSchema struct {
	ID          string `json:"id"`           // unique task identifier
	Name        string `json:"name"`         // display name
	Type        string `json:"type"`         // business semantic: signin / active_check / cat_travel / ...
	Display     string `json:"display"`      // closed-set display primitive for component selection
	CronSpec    string `json:"cron_spec"`    // cron expression
	Description string `json:"description"`  // human-readable description
	MaxDailyRun int    `json:"max_daily_run"` // max runs per day
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
type CredentialInfo struct {
	ExpiresAt int64  `json:"expires_at"` // Unix timestamp
	Ok        bool   `json:"ok"`         // true if valid
	DaysLeft  int    `json:"days_left"`  // days remaining, negative if already expired
}

// ModelInfo represents a model available on a platform. Used by P4 panel view.
type ModelInfo struct {
	ID             string `json:"id"`              // full name: "platform/model_name" (e.g. "workbuddy/deepseek-v4-flash")
	Name           string `json:"name"`            // display name (e.g. "DeepSeek V4 Flash")
	ContextSize    int    `json:"context_size"`    // max context window tokens
	SupportsTool   bool   `json:"supports_tool"`   // supports tool_calls
	SupportsStream bool   `json:"supports_stream"` // supports streaming responses
	IsFree         bool   `json:"is_free"`         // available via free quota
}

// UnifiedRequest the canonical request shape passed through pool → router → adapter.
type UnifiedRequest struct {
	Model    string                 `json:"model"`
	Messages []map[string]interface{} `json:"messages"`
	Stream   bool                   `json:"stream"`
	Source   string                 `json:"source"` // override: ?source= or X-Upstream header
}

// UnifiedResponse the canonical response shape returned to client.
type UnifiedResponse struct {
	ID      string                 `json:"id"`
	Model   string                 `json:"model"`
	Choices []map[string]interface{} `json:"choices"`
	Usage   map[string]interface{} `json:"usage"`
	Error   string                 `json:"error,omitempty"`
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
