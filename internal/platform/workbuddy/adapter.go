//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

package workbuddy

import (
	"context"

	"all2api/internal/adapter"
)

// Adapter implements the platform adapter for WorkBuddy/CodeBuddy.
type Adapter struct {
	adapter.BaseAdapter
}

// init auto-registers this adapter at build time.
// If excluded via build tag, this init() never runs.
func init() {
	a := &Adapter{}
	a.SetName("workbuddy")
	a.SetCapabilities(adapter.CapSet{
		Value: adapter.CapChatCompletion | adapter.CapResponsesStreaming,
	})
	adapter.Default.Register(a)
}

func (a *Adapter) ConvertRequest(req *adapter.UnifiedRequest) (*adapter.PlatformRequest, error) {
	// TODO: implement WorkBuddy-specific request conversion
	return &adapter.PlatformRequest{}, nil
}

func (a *Adapter) ParseResponse(resp *adapter.PlatformResponse) (*adapter.UnifiedResponse, error) {
	// TODO: implement WorkBuddy-specific response parsing
	return &adapter.UnifiedResponse{}, nil
}

func (a *Adapter) HealthCheck(ctx context.Context) (*adapter.HealthStatus, error) {
	return &adapter.HealthStatus{Reason: adapter.ReasonOK}, nil
}

func (a *Adapter) GetTasks(ctx context.Context) ([]*adapter.TaskState, error) {
	return nil, nil
}

func (a *Adapter) ExecuteTask(ctx context.Context, taskID string) error {
	return nil
}

func (a *Adapter) GetTaskSchema(ctx context.Context) ([]*adapter.TaskSchema, error) {
	return []*adapter.TaskSchema{
		{ID: "workbuddy.signin_09", Source: "workbuddy", Name: "签到 (09:00)", Type: "signin", Display: adapter.DisplayCalendarStreak, CronSpec: "0 9 * * *", Description: "每日 09:00 自动签到", MaxDailyRun: 1, Enabled: true},
		{ID: "workbuddy.signin_21", Source: "workbuddy", Name: "签到 (21:00)", Type: "signin", Display: adapter.DisplayCalendarStreak, CronSpec: "0 21 * * *", Description: "每日 21:00 自动签到 + 连登管家", MaxDailyRun: 1, Enabled: true},
		{ID: "workbuddy.cat_travel", Source: "workbuddy", Name: "猫猫旅行", Type: "cat_travel", Display: adapter.DisplayQuotaCountdown, CronSpec: "0 9,21 * * *", Description: "每日两次猫猫旅行任务", MaxDailyRun: 2, Enabled: true},
		{ID: "workbuddy.token_keepalive", Source: "workbuddy", Name: "Token 保活", Type: "token_keepalive", Display: adapter.DisplayValidityGauge, CronSpec: "0 22 * * *", Description: "每日 22:00 token 刷新", MaxDailyRun: 1, Enabled: true},
		{ID: "workbuddy.active_check", Source: "workbuddy", Name: "活跃上报", Type: "active_check", Display: adapter.DisplayValidityGauge, CronSpec: "0 10 * * *", Description: "每日 10:00 活跃上报 + streak 自检", MaxDailyRun: 1, Enabled: true},
	}, nil
}

func (a *Adapter) CredentialStatus() (*adapter.CredentialInfo, error) {
	// TODO: read auth state file and return expiry info
	return &adapter.CredentialInfo{Ok: true, Supported: false}, nil
}

func (a *Adapter) ListModels(ctx context.Context) ([]*adapter.ModelInfo, error) {
	return []*adapter.ModelInfo{
		{ID: "workbuddy/deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextSize: 1048576, SupportsTool: true, SupportsStream: true, IsFree: true},
		{ID: "workbuddy/glm-5.3-flash", Name: "GLM-5.3 Flash", ContextSize: 1048576, SupportsTool: true, SupportsStream: true, IsFree: true},
		{ID: "workbuddy/kimi-k3", Name: "Kimi K3", ContextSize: 200000, SupportsTool: true, SupportsStream: true, IsFree: true},
		{ID: "workbuddy/minimax-m3", Name: "MiniMax M3", ContextSize: 200000, SupportsTool: true, SupportsStream: true, IsFree: true},
		{ID: "workbuddy/hy3", Name: "Hunyuan Hy3", ContextSize: 200000, SupportsTool: true, SupportsStream: true, IsFree: true},
		{ID: "workbuddy/hunyuan", Name: "Hunyuan", ContextSize: 1048576, SupportsTool: true, SupportsStream: true, IsFree: true},
	}, nil
}
