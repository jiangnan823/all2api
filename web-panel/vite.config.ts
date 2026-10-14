import { defineConfig } from 'vite';

/**
 * B1 构建管道（M2 批次计划）。四条落点约束：
 * 1. outDir 用 `web-panel/dist` —— 这是仓库既有约定，不是我的选择：
 *    `Dockerfile:15` 写明「panel source web-panel/ → npm run build -> web-panel/dist」，
 *    `Dockerfile:39` 判空、`:57` 把 `/src/dist` 搬进 `internal/web/dist`；
 *    CI 两个 panel job 同样在 `working-directory: web-panel` 下 `find dist` 断言并把
 *    `web-panel/dist` 作为 upload-artifact 的 path。产物落这里=搬运方零改动。
 *    产物本身不入库（`.gitignore:36 web-panel/dist/*`）， embed 目录
 *    `internal/web/dist/.gitkeep`（`:39`）由 `//go:embed all:dist`（web.go:14）保底，
 *    CI 另有「Ensure embed dir is buildable」步骤兜底，不由本配置负责。
 * 2. emptyOutDir 保持 false：`.gitignore:37` 已为 `web-panel/dist/.gitkeep` 预留反向规则，
 *    一旦该占位文件入库，清空目录会把它带走并使工作树变脏；构建不该依赖「可删目录」的副作用。
 * 3. base 保持默认 '/'：Go 侧 NewPanelHandler 对未命中路径一律回落 index.html，
 *    相对 base 会让深层路径去请求 <path>/assets/*。此处只用绝对路径，不拼 host
 *    （G5：host 由注入的 window.__ALL2API_BASE__ / location.origin 决定）。
 * 4. 无 public 目录，避免把无关文件带进 embed 面。
 */
export default defineConfig({
  build: {
    outDir: 'dist',
    emptyOutDir: false,
    target: 'es2022',
    assetsInlineLimit: 0,
    sourcemap: false,
  },
  publicDir: false,
});
