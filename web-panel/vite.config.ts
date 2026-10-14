import { defineConfig } from 'vite';

/**
 * B1 构建管道（M2 批次计划）。三条落点约束：
 * 1. outDir 直指网关 embed 的 `internal/web/dist`（`//go:embed all:dist`，web.go:14）。
 *    产物按仓库 .gitignore 不入库（`internal/web/dist/*` + `!.gitkeep`），
 *    因此 emptyOutDir 必须为 false —— 清空会把唯一入库的 .gitkeep 带走，
 *    embed 对空目录直接编译失败（.gitignore:35-38 已写明同一理由）。
 * 2. base 保持默认 '/'：Go 侧 NewPanelHandler 对未命中路径一律回落 index.html，
 *    相对 base 会让深层路径去请求 <path>/assets/*。此处只用绝对路径，不拼 host
 *    （G5：host 由注入的 window.__ALL2API_BASE__ / location.origin 决定）。
 * 3. 无 public 目录，避免把无关文件带进 embed 面。
 */
export default defineConfig({
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: false,
    target: 'es2022',
    assetsInlineLimit: 0,
    sourcemap: false,
  },
  publicDir: false,
});
