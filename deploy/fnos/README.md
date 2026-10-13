# fnOS (.fpk) 打包 — M0 状态与核销清单

负责人：devops ｜ 状态：**结构已成文，字段名未在真机核销**

## 结论先说

1. **fpk 可行，且成本低**：我们已有 Docker Compose 形态，fpk 只是在外面套一层
   manifest + lifecycle script + 图标，由应用中心调 `docker compose` 起停。
2. **但我不能声称这份包现在就能装**：本机没有 fnpack，也连不上 github.com，
   真机验证未做。下面"核销清单"里 5 项没打勾之前，这个 target 在 CI 里必须是
   **非阻塞（continue-on-error）**，不能当成发布门禁。
3. **arm64 要单独出包**：manifest 的 `platform` 是单值。飞牛主流是 x86_64，
   但我们的镜像已经出 linux/arm64，fpk 侧只要换 `platform="arm"` 再 build 一次。

## 已确认（来自公开 fpk 仓库与社区实践）

| 项 | 结论 |
|---|---|
| 包内结构 | `manifest` + `config/privilege` + `config/resource` + `cmd/main` + `app/docker/docker-compose.yaml` + `app/ui/config` + `app/ui/images/` + `ICON.PNG`/`ICON_256.PNG` |
| 打包命令 | `fnpack build --directory <app_dir>`，产物 `.fpk` |
| 安装方式 | 应用中心手动安装，或 SSH `appcenter-cli install-fpk <file>.fpk` |
| 权限 | 需要 root（docker compose 调用），在 `config/privilege` 声明 |
| 资源类型 | `config/resource` → `type: docker-project` |
| 数据目录 | 落在 `TRIM_PKGVAR`（`/vol*/@appdata/<app>/`） |
| 版本格式 | `X.Y.Z` 三段，两段被拒 |
| 端口 | manifest 里 `service_port`，应用中心可能重映射（社区实测不一致）→ 见下方 Q6 结论 |

## 待真机核销（CI 转阻塞前必须全绿）

- [ ] `manifest` 的确切 key 集合与拼写（`start_on_boot` / `url` / `display_name` 三处是我按社区包推测的）
- [ ] `config/privilege`、`config/resource` 的格式是 JSON 还是 key=value（我按 JSON 写，两处已标 UNVERIFIED）
- [ ] `cmd/main` 被调用时 fnOS 传入的参数动词集合，以及 status 的期望返回字面量（我写了 `running` / `stoped` / `starting`，`stoped` 拼写需按实际解析器确认）
- [ ] `TRIM_PKGVAR` / `TRIM_PKGSRVDIR` / `TRIM_PKGDSMDIR` 三个环境变量的真实名称
- [ ] `app/ui/config` 是否需要 `portal` 字段以及子路径反代行为
- [ ] 图标尺寸要求（64/256 是社区包观察值）

核销做法（等主人有一台飞牛时）：
```bash
FPK_VERSION=0.1.0 FPK_IMAGE_TAG=v0.1.0 bash deploy/fnos/package-fpk.sh --emit-dir
# 1) 先看 staged 目录，与官方 SDK / 一个从应用中心拆出来的官方 fpk 逐字段对比
# 2) fnpack build --directory all2api_0.1.0
# 3) appcenter-cli install-fpk 后：start / stop / restart / status / 升级 / 卸载 六个动作全跑一遍
# 4) 卸载后再装，确认 auths/ 与 data/ 里的凭证和签到历史没有丢
```

## Q6 答复：fpk 场景下面板端口与反代路径（回 UI UE）

**结论：不能固定，`/connect` 的 Base URL 必须运行时计算。** 具体规则给前端：

1. **面板与 API 同源同端口**（面板是 `go:embed` 进二进制的，不是独立站点）。
   所以默认 Base URL = `location.origin + "/v1"`，这条在所有形态下都成立。
2. **端口号不可假设 7863**：
   - Docker/Compose：用户可改 `ports`
   - exe：用户改 `config.json` 的 `listen`
   - fpk：manifest 声明 `service_port=7863`，但**应用中心可能重映射**
   → 因此 UI 里任何"7863"都只能是示例值，不能是拼接逻辑的一部分。
3. **反代子路径**：fnOS 应用中心入口是以子路径方式打开面板的（`portal` 控制）。
   两种解法，我推荐 (b)：
   - (a) 前端用 `import.meta.env.BASE_URL` 推 base path —— 需要构建期知道前缀，fpk 做不到
   - (b) **后端在 index.html 注入 `window.__ALL2API_BASE__`**（来自
     `X-Forwarded-Prefix` / 配置的 `external_base_url`），前端优先读它，
     fallback `location.origin`。这样一套构建产物适配直连/反代/NAS 三种入口。
     → 需要 backend 在 config 里加 `external_base_url`（我在 `config.example.json` 已放好，默认空串）
     并在 HTTP server 上信任 `X-Forwarded-Prefix`。
4. **给客户端的 Base URL 要出两条**：很多 OpenAI 兼容客户端**不接受带路径的 Base URL**
   或对其支持不一致。`/connect` 视图应同时给：
   - 内网直连：`http://<NAS-IP>:<port>/v1`（推荐，给 CLI/客户端用）
   - 反代入口：`https://<nas-domain>/<app-prefix>/v1`（给浏览器面板用，客户端可能不支持）
   并在 UI 上标注"客户端请优先用直连地址"。
5. `?source=` 逃生通道在两条路径下都可用，路由不依赖 header，这条与议题 9 一致。

**给 UI UE 的落地约束**：接入视图的 Base URL 输入框必须是**只读推导 + 一键复制**，
不要让用户手填端口；端口来源用 `/healthz` 返回体里的 `service` 字段和
`location.origin`，两者不一致时以 `__ALL2API_BASE__` 为准并给出提示。

## 与分发链的衔接

- release.yml 里 fpk job：`continue-on-error: true`，直到上面 6 项核销完成再转阻塞。
- 图标资产目前缺失（`deploy/fnos/assets/`），需要 UI UE 出 64/256 两版 PNG，
  否则 `package-fpk.sh` 会直接失败退出（我刻意让它失败，而不是塞占位图）。
