//go:build qoder || full || (!qoder && !trae && !workbuddy)

// adapter.go registers the Qoder platform adapter and its sender surfaces.
// The build tag includes the default build (no tags), the qoder-only and full
// variants, and excludes the trae-only and workbuddy-only thin builds — the
// same matrix the workbuddy platform uses, mirrored for qoder.
package qoder

import (
	"context"

	"all2api/internal/adapter"
)

// Adapter implements the platform adapter for Qoder; the embedded senderImpl
// carries the adapter.Sender methods (SendChat / SendChatStream) the router
// type-asserts on, plus the Configure seam the host credential wire uses.
type Adapter struct {
	adapter.BaseAdapter
	senderImpl
}

func init() {
	a := &Adapter{}
	a.SetName("qoder")
	a.SetCapabilities(adapter.CapSet{
		Value: adapter.CapChatCompletion | adapter.CapResponsesStreaming,
	})
	if err := adapter.Default.Register(a); err != nil {
		panic("qoder: register adapter: " + err.Error())
	}
}

func (a *Adapter) ConvertRequest(req *adapter.UnifiedRequest) (*adapter.PlatformRequest, error) {
	return &adapter.PlatformRequest{}, nil
}

func (a *Adapter) ParseResponse(resp *adapter.PlatformResponse) (*adapter.UnifiedResponse, error) {
	return &adapter.UnifiedResponse{}, nil
}

func (a *Adapter) HealthCheck(ctx context.Context) (*adapter.HealthStatus, error) {
	// The router's per-send outcome classification (UpstreamError reasons)
	// feeds the pool and the breaker; a synthetic probe would only duplicate
	// that signal. Honest neutral reply until a dedicated probe is specced.
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
	// Qoder PATs are static tokens with no refresh/expiry semantics: the
	// expiry is platform-managed in practice, so the panel renders the
	// neutral "expiry managed by platform" shape (Supported=false is a
	// contract requirement — never Ok:false).
	return &adapter.CredentialInfo{Ok: true, Supported: false}, nil
}

// ListModels returns the static fallback catalog (display-name → qoder key
// snapshot). A dynamic model/list fetch is not wired yet.
func (a *Adapter) ListModels(ctx context.Context) ([]*adapter.ModelInfo, error) {
	out := make([]*adapter.ModelInfo, 0, len(defaultModelMap))
	for _, name := range modelDisplayNames() {
		out = append(out, &adapter.ModelInfo{
			ID:             "qoder/" + name,
			Name:           name,
			SupportsStream: true,
		})
	}
	return out, nil
}
