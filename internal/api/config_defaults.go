package api

// 恢复默认设置（v1.2 主人裁定「就没有个默认的设置吗」）：一键把可调字段重置为
// 出厂默认，监听地址 / API 密钥 / 凭证目录 / 状态文件四个连接与数据关键项保留——
// 改坏端口或密钥会让面板自身失联，不属于"可恢复默认"的范畴。原子写盘 + 热更新
// 与保存流同一套机制；DangerAction 式二次确认在前端。

import (
	"net/http"

	"all2api/internal/config"
)

// handleRestoreDefaults resets every tunable field to the factory defaults
// while preserving the connection/data-critical quartet (listen, api_key,
// auth_dir, state_file) — losing those strands the operator from their own
// gateway. The result goes through the same atomic-write + hot-swap path as
// the save flow, so a disk failure rolls the hot copy back.
func handleRestoreDefaults(w http.ResponseWriter, r *http.Request) {
	before := config.Shared.Clone()
	next := config.Shared.Clone()

	keep := func(c config.Config) (listen, key, authDir, stateFile string) {
		return c.Listen, c.APIKey, c.AuthDir, c.StateFile
	}
	listen, key, authDir, stateFile := keep(before)

	defaults := config.Defaults()
	next.Pool = defaults.Pool
	next.Schedule = defaults.Schedule
	next.Router = defaults.Router
	next.Sources = defaults.Sources
	next.ModelMap = defaults.ModelMap
	next.ExternalBaseURL = defaults.ExternalBaseURL
	next.NoticeVersion = defaults.NoticeVersion
	next.StickySessionTTLMin = defaults.StickySessionTTLMin // M10-B2: sticky back to 30min (0 stays user-choosable via save)
	next.Listen, next.APIKey = listen, key
	next.AuthDir, next.StateFile = authDir, stateFile

	config.Shared.Update(next)
	if err := writeConfigFileAtomic(savePath(), next); err != nil {
		config.Shared.Update(before)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config write failed"})
		return
	}
	panel, err := buildPanelConfig(next)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "config unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, panel)
}
