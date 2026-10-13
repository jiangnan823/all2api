# DEVOPS M0 交付说明 — CI / 镜像 / 分发 / fnOS

交付人：devops ｜ 位置：群共享目录 `shared/devops/` ｜ 目标仓库落位由 backend 执行

## 1. 文件清单与落位

| 共享目录路径 | 仓库目标路径 | 作用 |
|---|---|---|
| `.github/workflows/ci.yml` | 同名 | PR/main 门禁：panel build、vet+lint、race 单测、四目标交叉编译、多架构镜像构建、**healthz 冒烟**、gosec/govulncheck/gitleaks |
| `.github/workflows/release.yml` | 同名 | tag 触发：GHCR 多架构推送、四平台二进制 + SHA256、SBOM(SPDX)、Trivy 门禁、cosign 签名、GitHub Release、fpk 暂存（非阻塞） |
| `Dockerfile` | 同名 | 多阶段、CGO 关闭、distroless nonroot、面板 `go:embed`、镜像内 HEALTHCHECK |
| `.dockerignore` | 同名 | 关键：把 `auths/`、`data/`、`config.json`、`.env` 挡在构建上下文外 |
| `docker-compose.yml` | 同名 | 服务器/NAS 长跑：read_only + cap_drop ALL + 健康检查 + 日志轮转 + 内存上限 |
| `config.example.json` | 同名 | 配置基线：按源熔断/节流、四类任务 cron、`features.responses_api`、`external_base_url` |
| `Makefile` | 同名 | 本地与 CI 同构：`make check` = CI 全链，避免 backend 靠 push 试错 |
| `.golangci.yml` | 同名 | lint 策略（只保正确性风险，不卡风格） |
| `.gitignore` | 合并现有 | 凭证/状态/构建产物；含 `internal/web/dist` 占位保留规则 |
| `deploy/fnos/**` | 同名 | fpk 模板 + 打包脚本 + 核销清单 |

## 2. 我按这些假设写的（backend 若不同，改一处即可）

| 约定 | 假设值 | 不匹配时改哪里 |
|---|---|---|
| Go 版本 | `1.24`（读自现有 go.mod） | ci/release 的 `env.GO_VERSION`；golangci-lint 版本需 ≥ 该 Go 的构建工具链 |
| module 路径 | `all2api` | 不影响 CI（CI 不用 import path） |
| 入口包 | `./cmd/gateway` | `env.ENTRYPOINT` |
| 二进制名 | `all2api` | `env.BINARY` |
| 面板源码目录 | `web-panel/` | `env.PANEL_DIR` |
| 面板 embed 目录 | `internal/web/dist` | `env.EMBED_DIR` + Dockerfile 的 `COPY --from=web` |
| 监听端口 | `7863` | `env.LISTEN_PORT` + compose + manifest |
| 健康端点 | `/healthz`，返回体含 `"service"` | `env.HEALTH_PATH` + 冒烟断言 |
| 版本注入 | `main.version/commit/buildDate` | ldflags，需要 backend 在 main 包声明这三个 var |

## 3. backend 必须做的 5 件事（否则 CI 直接红）

1. **`all2api healthcheck` 子命令**：GET 自身 `/healthz`，200 → exit 0，否则非 0。
   distroless 镜像里没有 shell 也没有 wget，HEALTHCHECK 只能由程序自己实现。
   这同时是 fpk 的 `cmd/main status` 依据。**M0 验收"`/healthz` 通"就是靠它。**
2. **`//go:embed all:dist`**（不是 `//go:embed dist`）：不带 `all:` 前缀时，
   `.`/`_` 开头的文件被排除，占位用的 `.gitkeep` 会匹配不到 → 编译期报
   `no matching files found`。这是空面板阶段最容易踩的坑。
3. **提交 `internal/web/dist/.gitkeep` 和 `web-panel/.gitkeep`**：git 不跟踪空目录，
   一旦 clone 后目录不存在，`COPY web-panel/ ./` 与 `go:embed` 都会失败。
4. **SIGTERM 优雅退出 ≤10s**：CI 冒烟里有一步 `docker stop --time 10` 并计时断言。
   签到调度器、在途请求、Redis 镜像都要挂 ctx。
5. **`config.json` 的 `listen` 与 compose 端口映射一致性**：端口对外变化时
   （fpk 场景尤其）`external_base_url` 要能表达真实入口，供面板生成正确接入地址。

## 4. 我对 backend M0 代码的审查意见（互审范围内，非阻塞）

看 `internal/adapter/models.go`：

- 我这 4 条接口要求（HealthCheck 原因码、CredentialStatus、按源节流、能力矩阵）都已采纳，`Capabilities() CapSet` 的实现比布尔好，同意现方案。
- **`ctx interface{}` 建议改 `context.Context`**：`interface{}` 让 ctx 无法真正传递
  取消与超时。我这边最关心的正是"上游卡住时能在 5s 内放弃并冷却该账号"，
  以及 `docker stop` 的 10s 窗口。签名不收 `context.Context`，这两点就落不了地。
  这是我会拦的一项，请在接口冻结前改掉。
- `CapSSERebuild` 作为能力位可以，但它不是"平台能力"而是"网关自身实现质量"，
  放在同一位图里会让路由择优语义混淆。建议挪到运行时信息，不进 CapSet。
- `HealthStatus.ExpiresAt int64` 与 `CredentialInfo` 的过期时间重复上报，
  两处可能不一致。以 `CredentialStatus()` 为唯一来源，`HealthStatus` 里去掉。

## 5. 关于 module path 的一条建议（给 backend + 项目经理）

现在 `go.mod` 是 `module all2api`。本地能编，但对外发布有两个代价：
`go install github.com/jiangnan823/all2api@latest` 不可用；工具链（govulncheck、
golangci、依赖解析）在解析自身 import 时行为不一致。建议现在趁 M0 就定成
`github.com/jiangnan823/all2api`，越晚改越贵。改一次 `go.mod` + 全量 import。

## 6. Q6 答复摘要（fpk 端口/反代，给 UI UE / fronted）

**不能固定，Base URL 必须运行时推导。** 完整规则在 `deploy/fnos/README.md` 的
"Q6 答复"一节，要点：面板与 API 同源（embed），默认 `location.origin + /v1`；
端口不可假设 7863（三种形态都可能变）；反代子路径由后端注入
`window.__ALL2API_BASE__`（配置项 `external_base_url` 已放在 config.example.json）；
`/connect` 视图需同时给"内网直连"和"反代入口"两条地址，客户端优先直连。

## 7. 真实验证状态（不夸大）

| 项 | 状态 |
|---|---|
| workflow YAML/JSON/compose 语法 | 已本地校验通过 |
| shell 脚本语法（`bash -n`） | 已校验通过 |
| Makefile tab 结构 | 已检查（无空格缩进 recipe） |
| Go 代码编译 | **未验证** — 本机无 go 工具链，且 backend 骨架尚未完成 |
| CI 真实跑通 | **未验证** — 本环境连不上 github.com，需 backend 落位 + 主人 push 后由我远程核验第一次运行<br>*（10-13 更新：API 已可达，push 已确认，但 CI 定义未落位 → 0 workflows；见 §9.3）* |
| Docker 镜像构建 | **未验证** — 本机无 docker，多架构 buildx 与 distroless 层需首次 push 时确认 |
| fnOS fpk 字段名 | **未验证** — 见 README 6 项核销清单，CI 里暂设 `continue-on-error: true` |
| golangci-lint 版本与 Go 1.24 兼容性 | **未验证** — 首次 CI 若报 toolchain 不匹配，按注释升版 |

第一次 actions 跑通后我会把失败项逐条回给 backend，并把 fpk job 转成阻塞门禁。

## 8. 我需要的项目经理支持

- 分支约定：建议 `main` + `feat/*`，PR 走 CI 门禁；release 只由 tag 触发。
  需要确认是否启用分支保护（要求 CI 绿 + 1 个互审 approve），这与 SOP 的方案 A 互审一致。
- 主人 push 之后请把首个 run 的 URL 给我，我做远程核验并出"首次门禁报告"。
- 提醒：`deploy/fnos/assets/` 需要 UI UE 出 64/256 两版 PNG 图标，
  打包脚本对缺失图标是**硬失败**（我没让它塞占位图，避免出一个装不上的包还以为成功）。

## 9. M1 CI 落位预检（2026-10-13，tip 7c29a27）

### 9.1 §3 的五件事：现状全部核销（我逐条看码确认，不采信自述）

| # | 项 | 证据 | 状态 |
|---|---|---|---|
| 1 | `healthcheck` 子命令 | `cmd/gateway/main.go:32` 分发，`:170-185` GET `http://localhost:7863/healthz`，非 200 → 非 0 退出 | ✅ |
| 2 | `//go:embed all:dist` | `internal/web/web.go:14` | ✅ |
| 3 | 占位文件入库 | `git ls-files`：`internal/web/dist/.gitkeep`、`web-panel/.gitkeep` 均在 | ✅ |
| 4 | SIGTERM ≤10s | `cmd/gateway/main.go:111` `signal.Notify(SIGTERM/SIGINT)`，`:121-126` 10s ctx `server.Shutdown` | ✅ |
| 5 | `listen` / 端口一致 | `config.example.json` `listen=":7863"`；Dockerfile `EXPOSE 7863` + `-config /app/config.json`；`/healthz` 返回含 `"service":"all2api"`（`main.go:73`）→ 冒烟断言可对现码通过 | ✅ |

补充：`config.Load`（`internal/config/config.go:159`）是「defaults + Unmarshal」，无严格校验 ⇒ 我冒烟作业里自造的最小
`config.json`（仅 listen/api_key/auth_dir/state_file）能启动，`notice_version`/`sources` 由 defaults 兜住。

### 9.2 本轮查出的红点（是我自己的 CI 缺陷，已修，非 backend 问题）

`web-panel/package.json` 只有 `typecheck` / `test` / `test:watch`，**没有 `build` 脚本**，也没有 bundler 依赖与
`index.html`（其 `_note` 自陈「构建、静态服务与 /api/* 接线列 M1」）。而我的四处产物硬跑 `npm run build` ⇒ 首跑
panel job 必红，且极易被误记成 Go 线故障。修订：

- `ci.yml` panel：`npm run --if-present build` + `built` 输出门控 dist 断言与产物上传；typecheck/test 保持硬门禁。
- `ci.yml` build（三变体）：**摘掉对 panel 产物的依赖**，只用仓内 `.gitkeep` 保证 embed 可编。Go 门禁与前端进度解耦。
- `release.yml` panel：同上 + 新增 job output `dist`；`binaries` 的产物恢复/占位改按 `needs.panel.outputs.dist` 判定，
  并加 `if` 允许 panel 被 skip 时仍出二进制（否则整条 release 链会被 skipped needs 带走）。
- `Dockerfile` web 阶段：`--if-present` + dist 为空时落占位 `index.html`，embed 恒有内容。
- `Makefile panel`：`--if-present` + 空 dist 早退，**不再先 `rm -rf internal/web/dist` 再 cp 失败**（原写法会留下空 embed 目录）。

`config.example.json`（shared 草稿）文件头加 `_status` 作废声明，与 PM 裁定三口径对齐：仓根版本是唯一权威，草稿不入仓、勿拷贝。

### 9.3 归位缺口（首跑被这一步卡住，不是被代码卡住）

远程已 push（`origin/main = 7c29a27`，与本地 tip 一致），但 `GET /actions/workflows` 与 `/actions/runs` 均 `total_count: 0`，
`GET /contents/.github/workflows` 404；本地 `git ls-files` 无 `.github/`、`Dockerfile`、`docker-compose.yml`、`Makefile`、
`.golangci.yml`，`deploy/fnos/` 只进了 assets。⇒ 需一枚 chore 提交把下表落位并再次 push，才会触发首次 actions。

| 落位路径 | 源（shared/devops/） |
|---|---|
| `.github/workflows/ci.yml`、`.github/workflows/release.yml` | 同名 |
| `Dockerfile`、`.dockerignore`、`docker-compose.yml`、`Makefile`、`.golangci.yml` | 同名 |
| `deploy/fnos/template/**`、`deploy/fnos/package-fpk.sh`、`deploy/fnos/README.md` | 同名（assets 两枚图标 + 生成脚本已在仓） |

**不要落位**：`config.example.json`（仓根权威版已在）、`DEVOPS-M0-NOTES.md`（可落 `deploy/` 或 `docs/`，非门禁件）。

### 9.4 CI 首跑判定口径（预登记，PM seq=511 二 + 测试 seq=514 一致）

1. `cmd/gateway` 自 `c49601c` 起不可编译，**先于接线批存在**；接线批 `7c29a27` 已恢复。
   ⇒ 三变体 `go build` 若对旧 tip 记录过红，属既有断点，**不计为 M1 回归**；首跑只对 tip `7c29a27` 断言，期望绿。
2. 首跑成功判据（逐项出结论，缺一不判 Pass）：三变体 `go build`（linux/amd64、linux/arm64、windows/amd64、darwin/arm64）、
   `go vet ./...` + golangci-lint、`go test -race`（含 `recover_test.go` 反例）、image 构建、`/healthz` 冒烟
   （healthy + HTTP 200 + 含 `"service"` + `docker stop --time 10` 计时 ≤10s）、gitleaks/gosec/govulncheck。
3. `fnos-fpk` job 首跑仍 `continue-on-error: true`，前置条件是 `deploy/fnos/README.md` 六项字段真机核销；未核销不转阻塞。
4. panel job 在 bundler 落地前允许 `built=false`，该状态记为「前端 M1 项未完成」，不是 CI 失败。
5. **`jobs = 0` 且 run 瞬间 `completed/failure` ⇒ 判为 workflow 校验层失败（devops 工件缺陷），不计入 backend/fronted 任何回归**，
   由 devops 直接修 workflow。只有 jobs 数 ≥1 且出现具体 step 红，才进入代码线归因。

### 9.5 本机仍无法验证的项（诚实边界）

本轮 GitHub API 已可达（此前不通），所以远程状态是实测；但本机无 `go`/`docker`/`fnpack`，因此：三变体实际编译、镜像构建与
多架构、golangci-lint 与 Go 1.24 的 toolchain 兼容、Trivy/SBOM 产物、fpk 字段名，仍只能由首个真实 run 给结论。
`npm ci` 能否通过（lockfile 与 package.json 一致性）同理只能由 CI 判。

### 9.6 首跑实测记录（14:26Z，run 38413356374 / 38413358569）

远端 `refs/heads/main` 已到 `6733337`，两个 workflow 均注册为 `active`，但两个 run 都在 `run_started_at == updated_at`
瞬间 `completed/failure` 且 **jobs = 0** —— 这是 workflow **校验层**失败（作业从未建立），不是任何一步跑挂。

根因（官方 context 可用性表实证）：`jobs.<job_id>.if` 只允许 `github / needs / vars / inputs`，
**不允许 `env`**。我的 panel job 写了 `if: ${{ hashFiles(format('{0}/package-lock.json', env.PANEL_DIR)) != '' }}`
⇒ ci.yml 与 release.yml 各中一处，两个文件同时被拒，与"两条 run 同时瞬间失败"完全吻合。
本机 PyYAML 只查语法、不查 context 作用域，所以 §7 的"YAML 已校验通过"覆盖不到这一层——这条要记进口径：
**workflow 的 YAML 合法 ≠ GitHub 接受**，只有真实 run 能判。

修复：job 级 `if` 改为字面路径 `hashFiles('web-panel/package-lock.json')`（加注释标明这是 PANEL_DIR 之外唯一要同改的一处）；
另把 release.yml `binaries` 的 `if: >-` 折行表达式压成单行，避免表达式跨行的未定义行为。
全量复扫（context 作用域 / 重复键 / 括号配平 / 函数白名单 / needs 图与环 / steps·needs 引用声明 / uses 格式）已零命中。
`hashFiles` 在 job 级 `if` 属"可用于 key"，保留。


