package qoder

import (
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

func (a *Adapter) HealthCheck(ctx interface{}) (*adapter.HealthStatus, error) {
	return &adapter.HealthStatus{Reason: adapter.ReasonOK}, nil
}

func (a *Adapter) GetTasks(ctx interface{}) ([]*adapter.TaskState, error) {
	return nil, nil
}

func (a *Adapter) ExecuteTask(ctx interface{}, taskID string) error {
	return nil
}

func (a *Adapter) GetTaskSchema(ctx interface{}) ([]*adapter.TaskSchema, error) {
	return []*adapter.TaskSchema{
		{ID: "signin", Name: "签到", DisplayType: adapter.DisplayCalendar, CronSpec: "0 9 * * *", Description: "每日签到获取积分", MaxDailyRun: 1},
	}, nil
}

func (a *Adapter) CredentialStatus() (*adapter.CredentialInfo, error) {
	return &adapter.CredentialInfo{Ok: false, ExpiresAt: 0, DaysLeft: 0}, nil
}

func (a *Adapter) ListModels(ctx interface{}) ([]*adapter.ModelInfo, error) {
	// TODO: populate from Qoder's actual model list after spike
	return []*adapter.ModelInfo{}, nil
}
