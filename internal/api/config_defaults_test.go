package api

// 恢复默认设置端点测试：重置可调字段到出厂默认，保留连接/数据关键四项
// （listen/api_key/auth_dir/state_file）；401 守卫生效。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"all2api/internal/config"
)

func newDefaultsTestEnv(t *testing.T) (mux *http.ServeMux, path string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "config.json")
	body := `{"listen":":9999","api_key":"sk-test","auth_dir":"./auths","state_file":"./data/state.json","pool":{"credit_floor":7,"max_retries":9,"cooldown_sec":120},"schedule":{"signin_cron":"0 5 * * *"},"router":{"responses_api":{"enabled":true}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := config.Load(path); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(dir, "open.json"), []byte(`{"listen":":0"}`), 0o600)
		_ = config.Load(filepath.Join(dir, "open.json"))
	})
	mux = http.NewServeMux()
	Register(mux, nil)
	return mux, path
}

func TestRestoreDefaultsResetsTunablesKeepsConnection(t *testing.T) {
	mux, path := newDefaultsTestEnv(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/config/restore-defaults", nil)
	req.Header.Set("X-API-Key", "sk-test")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: got %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"pool"`) {
		t.Fatalf("restore response must carry the applied panel config")
	}

	// 磁盘文件已写：可调字段回默认，连接四项保留
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	text := string(raw)
	for _, keep := range []string{`":9999"`, `"sk-test"`, `"./auths"`, `"./data/state.json"`} {
		if !strings.Contains(text, keep) {
			t.Fatalf("connection-critical value %s must be preserved", keep)
		}
	}
	for _, gone := range []string{`"credit_floor": 7`, `"max_retries": 9`, `"0 5 * * *"`} {
		if strings.Contains(text, gone) {
			t.Fatalf("default reset failed: %s still present", gone)
		}
	}
	// 热副本同步：后续 GET 看到默认值
	if config.Shared.Get().Pool.MaxRetries != 3 {
		t.Fatalf("hot config max_retries = %d, want default 3", config.Shared.Get().Pool.MaxRetries)
	}
}

func TestRestoreDefaultsRequiresAuth(t *testing.T) {
	mux, _ := newDefaultsTestEnv(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/config/restore-defaults", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: got %d, want 401", rec.Code)
	}
}
