//go:build trae || full

package trae

import (
	"context"

	"all2api/internal/adapter"
)

// Adapter implements the platform adapter for Trae (ByteDance).
// SPIKE: Trae uses internal IDE protocol, not public API.
// Initial feasibility check required before full implementation.
type Adapter struct {
	adapter.BaseAdapter
}

func init() {
	a := &Adapter{}
	a.SetName("trae")
	a.SetCapabilities(adapter.CapSet{
		Value: adapter.CapChatCompletion, // Only chat completions until protocol stability is confirmed
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
		{ID: "trae.signin", Source: "trae", Name: "签到", Type: "signin", Display: adapter.DisplayStatusPlain, CronSpec: "", Description: "Trae 免费额度管理任务", MaxDailyRun: 1, Enabled: true},
	}, nil
}

func (a *Adapter) CredentialStatus() (*adapter.CredentialInfo, error) {
	// TODO: read auth state
	return &adapter.CredentialInfo{Ok: true, Supported: false}, nil
}

func (a *Adapter) ListModels(ctx context.Context) ([]*adapter.ModelInfo, error) {
	// TODO: populate from Trae after spike confirms protocol stability
	return []*adapter.ModelInfo{}, nil
}
