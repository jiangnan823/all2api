//go:build qoder || full

package qoder

import (
	"context"

	"all2api/internal/adapter"
)

// Adapter implements the platform adapter for Qoder.
type Adapter struct {
	adapter.BaseAdapter
}

func init() {
	a := &Adapter{}
	a.SetName("qoder")
	a.SetCapabilities(adapter.CapSet{
		Value: adapter.CapChatCompletion | adapter.CapResponsesStreaming,
	})
	adapter.Default.Register(a)
}

func (a *Adapter) ConvertRequest(req *adapter.UnifiedRequest) (*adapter.PlatformRequest, error) {
	return &adapter.PlatformRequest{}, nil
}

func (a *Adapter) ParseResponse(resp *adapter.PlatformResponse) (*adapter.UnifiedResponse, error) {
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
		{ID: "qoder.signin", Source: "qoder", Name: "签到", Type: "signin", Display: adapter.DisplayCalendarStreak, CronSpec: "0 9 * * *", Description: "每日签到获取积分", MaxDailyRun: 1, Enabled: true},
	}, nil
}

func (a *Adapter) CredentialStatus() (*adapter.CredentialInfo, error) {
	// TODO: read auth state
	return &adapter.CredentialInfo{Ok: true, Supported: false}, nil
}

func (a *Adapter) ListModels(ctx context.Context) ([]*adapter.ModelInfo, error) {
	// TODO: populate from Qoder's actual model list after spike
	return []*adapter.ModelInfo{}, nil
}
