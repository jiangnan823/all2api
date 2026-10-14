//go:build workbuddy || full || (!qoder && !trae && !workbuddy)

// Package workbuddy implements the WorkBuddy/CodeBuddy platform adapter.
//
// Protocol basis: docs/upstream/workbuddy-protocol.md (public-source research,
// cross-validated across HanawaBanana/workbuddy2api, xiaofan6ya/workbuddy2api
// and HanHan666666/codebuddy2openai). Endpoint/auth facts coded here are the
// ones those sources agree on; anything single-source or disputed is left as
// a TODO and noted in the report §9 gap list.
//
// doc.go is always compiled so "go build ./..." never fails with "build
// constraints exclude all Go files" in variants where the platform is
// excluded; the Sender implementation here carries the same build tags as the
// adapter registration.
package workbuddy

import (
	"context"

	"all2api/internal/adapter"
)

// Adapter implements the platform adapter for WorkBuddy/CodeBuddy.
// It embeds senderImpl (client.go), which provides the adapter.Sender
// methods — the router type-asserts the registered adapter onto
// adapter.Sender, so the adapter itself must carry those methods.
type Adapter struct {
	adapter.BaseAdapter
	*senderImpl
}

// init auto-registers this adapter at build time.
// If excluded via build tag, this init() never runs.
func init() {
	a := &Adapter{senderImpl: newSender()}
	a.SetName("workbuddy")
	a.SetCapabilities(adapter.CapSet{
		Value: adapter.CapChatCompletion | adapter.CapResponsesStreaming,
	})
	if err := adapter.Default.Register(a); err != nil {
		panic("workbuddy: register adapter: " + err.Error())
	}
}

func (a *Adapter) ConvertRequest(req *adapter.UnifiedRequest) (*adapter.PlatformRequest, error) {
	// The /v1 surface routes through adapter.Sender; the legacy
	// UnifiedRequest conversion is not part of the Sender wire path.
	return &adapter.PlatformRequest{}, nil
}

func (a *Adapter) ParseResponse(resp *adapter.PlatformResponse) (*adapter.UnifiedResponse, error) {
	return &adapter.UnifiedResponse{}, nil
}

func (a *Adapter) HealthCheck(ctx context.Context) (*adapter.HealthStatus, error) {
	// CredentialState is fed by the integrator's credential wiring; without
	// it the adapter reports ok (the pool, not the adapter, owns health).
	if a.credentials() == nil {
		return &adapter.HealthStatus{Reason: adapter.ReasonOK}, nil
	}
	return a.credentials().HealthCheck(ctx)
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
	// CredentialState is optional wiring (see CredentialState in client.go);
	// absent it stays "supported: false" — grey "expiry managed by platform",
	// never a false "expired".
	if a.credentials() == nil {
		return &adapter.CredentialInfo{Ok: true, Supported: false}, nil
	}
	return a.credentials().Status()
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
