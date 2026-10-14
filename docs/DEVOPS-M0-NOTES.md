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



### 9.7 二跑实测与根因二次修正（16:13Z，run 38428692560 / 38428694889，head=8d327ec）

远端 `refs/heads/main == 8d327ec18d47d9306ae7c8076fcf6f94e6a2a2b9`（API 实测，pushed_at 16:13:12Z），
本次 push **确实自动触发**两条 run，两条仍 `completed/failure`、**jobs = 0**、`run_started_at == updated_at`、
`billable` 为空、`spendable_runner_ms` 无、仓库 artifacts total = 0。
即：**消耗的 runner 分钟 = 0**，代价只是两条 run 记录（校验层拒绝，作业从未创建）。

根因修正（推翻我在 §9.6 末段的判断）：官方 context 可用性表里
`jobs.<job_id>.if` 的 **可用函数白名单只有 `always / cancelled / success / failure`**，
`hashFiles` 只在 `steps.<step_id>.if` 可用。
我上一次的"修复"只是把违规的 `env` context 换成违规的 `hashFiles` 函数，
所以 8d327ec 里的 ci.yml/release.yml 依旧整体被拒 —— 这是**同一个缺陷的第二种形态，仍归 devops 工件线**，
与 backend / fronted 代码无关。§9.6 那句"`hashFiles` 属可用于 key，保留"是错的，正式作废。

另记：GitHub 的校验错误文案**只在 Web UI 出现**，REST（runs / jobs / timing / check-suites / check-runs）
全部拿不到 reason（check-runs total = 0）。所以静态复扫必须自证充分，不能指望"跑一次看报错"。

修复（本轮落盘，未 push）：panel job **取消 job 级 `if`**，改为 checkout 后的 `Detect panel project` 步骤
（纯 shell 判 `-f ${PANEL_DIR}/package-lock.json`，不依赖任何 context 白名单）写 `present` 输出，
npm 各步骤统一 `if: steps.detect.outputs.present == 'true'`；job 输出 `dist` 仍取 `steps.build.outputs.built`，
无面板时为空 ⇒ release 的 binaries 走占位嵌入分支。复扫口径：job 级 `if` 只剩 `needs`/`github` + `always()`。

同时修掉两处只在"真实构建"才暴露的工件缺陷（本机静态判定，非猜测）：
1. `Dockerfile` 的 `COPY go.mod go.sum ./` —— 仓库**没有 go.sum**（`go.mod` 零第三方依赖，30 个 .go 文件 import
   全部 stdlib 或模块内路径，已脚本核验），该 COPY 会让 `docker build` 直接 "file not found" 失败，
   image / smoke / release 全红。已改为 `COPY go.mod ./` 并写明"backend 引入第一个依赖时必须同处加回 go.sum"。
2. `.dockerignore` 未排除 `.worktrees/`（仓库内并存的嵌套 worktree，含完整源码 + node_modules），
   `COPY . .` 会把它们带进上下文；已排除，并显式 `!internal/web/dist/**` 保住 `go:embed all:dist` 的目录。
   `.gitignore` 同步加 `.worktrees/`，让 `git status` 仍是可信的干净度信号、`git add -A` 误操作失效。

### 9.8 本地等效门禁（云端 Actions 由主人冻结后的替代口径）

本机工具链实测：`node v24.18.0` / `npm` / `git` 可用；`go`、`docker`、`make`、`gh`、`gitleaks`、`gosec`、
`govulncheck`、`golangci-lint`、`podman` **全部缺失**（`C:\Users\User\go` 只是 GOPATH，不是工具链）。
所以按 PM 改道令：能真跑的真跑，跑不了的标 `skipped-with-reason`，不判红、不假 Pass。
详见群内「本地等效门禁报告」。要让 Go 线与镜像线拿到真实证据，只有两条路：
便携版 Go 解压到临时目录（不装系统、不改 PATH、用完删）或主人侧跑一次 `make build-all && docker build`。
两者都需要主人点头，我不擅自装工具链。

### 9.9 本地 Go 补证（临时便携 Go，零 Actions 消耗）+ 首次真实 run 事实

**首次真实 run（主人 push 9963226 后自动触发）**：run 38433219562 / workflow `ci`（**名字已能解析 ⇒ 校验层通过，
§9.7 的修复有效**）/ head `9963226` / 16:46:17Z→16:47:05Z（墙钟 48s）/ 7 个 job 真实创建。
`billable.UBUNTU.total_ms = 0`（7 个 job 各 0ms）：**公共仓库 GitHub-hosted standard runner 不计额度**，
主人实测消耗 = 0 分钟。panel job **绿**（detect→npm ci→typecheck+test→build 步骤 built=false，断言与上传按预期 skip）。
lint（`go vet` 红）与 test（`go test -race` 红）失败 ⇒ build / image / smoke / security 四个下游 job 全部 skip。
release.yml 未触发（tag-only，符合设计）。

**本地补证工具链**：`go1.24.13.windows-amd64.zip` 取自官方 dl.google.com，SHA256
`40b16bc8f00540a2cb02dff4de72b73e966fdd8d65f95e33d8e4080b48a2459a` **实测 MATCH**；解压在
`%LOCALAPPDATA%\Temp\devops-go\go`，GOROOT/GOCACHE 全在临时目录，**未装系统、未改全局 PATH、未写入仓库**。
`golangci-lint 1.64.8 windows-amd64` 取自官方 release，SHA256 `54c2ed3a…a8f` MATCH；
`built with go1.24.1` ⇒ §7 挂账的"golangci 与 Go 1.24 toolchain 兼容性"**判掉**（与 ci.yml pin 版本一致，可跑通）。

**编译面真因（5 条硬错误，3 个包，全在 Go 线 = backend）**：
- `internal/schedule/schedule.go:70` 调 `s.initNextRuns()`，全仓**无该方法定义**（grep 只命中调用）。
- `internal/schedule/schedule.go:283` 调 `splitByComma(field)`，**无该函数**。
- `internal/schedule/schedule.go:276` `(24 + firstH) * time.Hour`：`firstH` 是 int ⇒ 需 `time.Duration(...)`。
- `internal/sessions/stickiness.go:154` `for platform, platformMap := range` 中 `platform` **declared and not used**。
- `cmd/gateway/main.go:108` `ctx, cancel := context.WithCancel(...)` 中 **ctx 未使用**（全文件只有 `:108` 声明与 `:113` cancel）。

**由第 5 条带出的一条实质问题（要更正我自己的旧结论）**：ctx 从未传给任何组件 ⇒ SIGTERM 只走
`server.Shutdown(10s ctx)`，**调度器 tickLoop 没有 ctx 可取消**。因此 §9.1 里我把"backend 必做第 4 项
（SIGTERM 优雅退出 ≤10s，签到调度器/在途请求挂 ctx）"判为 ✅ **收回到"部分满足"**：HTTP 面满足，
调度面未接。smoke 的 `docker stop ≤10s` 断言在真实 run 里能过（进程随 SIGTERM 退出），但**"10s 内停止派发"没有实现**——
这不是 CI 红，是语义缺口，归 backend 线，M1 内应补。

**其余门禁实测结果**：四目标 `go build`（linux/amd64、linux/arm64、windows/amd64、darwin/arm64）**全部同点失败**
（`main.go:108`），失败原因与平台无关 ⇒ 交叉编译能力本身未证伪；逐包 linux/amd64 编译矩阵：**12/15 包 OK**，
FAIL = `cmd/gateway` / `internal/schedule` / `internal/sessions`。
`go test ./...`：`internal/api` **ok 0.544s**（recover 5 个用例全 PASS，含 402/凭证失效/未知 source/需要 APIKey 四条语义），
其余包因编译失败 build failed。`go test -race` **维持挂账**，两层实测原因：
`go: -race requires cgo; enable cgo by setting CGO_ENABLED=1` ⇒ `cgo: C compiler "gcc" not found`（本机无 gcc/cc/clang）。
`golangci-lint run ./...`：**2 条 errcheck**（`internal/platform/workbuddy/adapter.go:24` Register 未查错、
`internal/web/panel.go:89` `w.Write` 未查错）+ 3 条 typechecking error；诚实边界——3 个包因编译不过**未被完整分析**，
修完后 lint 结果需重跑。`gofmt`：以 HEAD blob（LF 真源，已排除 autocrlf 假阳性）口径 **10/30 文件未格式化**，
差异是常量块/结构体字段的对齐（如 `internal/circuit/circuit.go` 的 Reason 注释列），CI 的 gofmt 步骤会红。
docker / 镜像 / smoke / gitleaks / gosec / govulncheck 本机仍无工具 ⇒ skipped-with-reason 不变。

### 9.10 R1 分支复验（tip `b308646`，Go 线由「已验证=红」转「已验证=绿」）

复验对象：backend R1 分支栈顶 `b308646` = `b308646d08d6a84b741c74d77aa9a129aa4e7615`，
`git merge-base 9963226 b308646` = `9963226e…`（确系 main 直接后代，非分叉），
`git log --oneline 9963226..b308646` = 5 个 commit（`152138e` A1 / `6dde5cb` A2 / `a375993` A3 / `ce83f7d` A4 / `b308646` B），
`git diff --stat 9963226..b308646` = **13 files, +231 / −161**，与其自报一致。
权威 blob 坐标全部核对命中：`main.go 3bdb79e`、`schedule.go e158f03`、`stickiness.go e402ed1`、
`panel.go d65388a`、`pool.go 03a3d05`、`workbuddy/adapter.go c37fadc`。

**冻结面扫描**：`git diff --name-only 9963226..b308646 -- web-panel/ .github/ docs/ Dockerfile Makefile
docker-compose.yml deploy/ go.mod config.example.json .gitignore .dockerignore .golangci.yml` ⇒ **零命中**。
R1 没碰任何契约冻结面，也未侵入我的工件线（除 `.golangci.yml` 的 noctx 豁免归我，见下）。

**门禁实测（同一便携工具链 go1.24.13 + golangci-lint 1.64.8，`git archive` 导出，不碰 main 检出与 backend worktree）**：

| 门禁 | 结果 | 证据 |
| --- | --- | --- |
| `go vet ./...` | **0 错** | exit=0 |
| `go build ./...` | **通过** | exit=0 |
| 四目标交叉编译 | **4/4 出真产物** | linux/amd64 6,529,208 B；linux/arm64 6,160,568 B；windows/amd64 6,718,464 B；darwin/arm64 6,291,426 B（`CGO_ENABLED=0`、`-trimpath`、`-s -w`、ldflags 注入 version/commit/buildDate） |
| 逐包编译矩阵 | **15/15 OK** | §9.9 的 3 个 FAIL 包（`cmd/gateway`/`internal/schedule`/`internal/sessions`）全部转绿 |
| `go test ./...` | **0 FAIL** | `internal/api ok 0.554s`，其余包 `[no test files]` |
| `go test -v ./internal/api` | **5/5 PASS** | recover 402 / 凭证失效 / 未知 source / 需要 APIKey + halve-budget source 过滤 |
| `gofmt -l` | **0/30**（§9.9 为 10/30） | 以 `git cat-file blob` 镜像判定（30 文件 0 个 CR）；`git archive`+MSYS tar 会转 CRLF ⇒ 该法在 Windows 上是假阳性源，弃用 |
| `golangci-lint run ./...` | 加豁免前 **1 条**，加豁免后 **0 条 / exit=0** | 唯一命中 = `cmd/gateway/main.go:181` `noctx`（裁定三.4 判免）；§9.9 的 2 条 errcheck（`workbuddy/adapter.go:24`、`panel.go:89`）与新增可见的 `stickiness.go:173/:175` 全部消失 |
| `golangci-lint config verify` | **exit=0** | 豁免配置 schema 合法，且 `noctx` 仍在启用 linter 列表中（不是整条 linter 被关） |

**裁定二（buildDate 必须被 healthz 引用）已验**：`cmd/gateway/main.go:29` 声明 `buildDate = "unknown"`，
`:74` 的 `/healthz` 响应体 `fmt.Fprintf` 直接引用 `version, buildDate, uptime`——**无分支逻辑**，缺省即输出
`unknown`，满足「容忍空/unknown」的要求。PM 原句的行号映射是 `:28 → :73`，实际偏移一行，语义成立。

**新增：本机原生运行时冒烟（不是 docker，但首次在真进程上验）**：用同点构建的 `windows-amd64` 产物起真服务，
- `GET /healthz` → HTTP 200，body `{"status":"ok","service":"all2api","version":"r1-verify","buildDate":"2026-10-13T17:17:49Z","uptime":"1.61s"}`
  ⇒ ci.yml smoke job 的 `"service"` 身份字段断言在真实 payload 上成立，且 ldflags 注入的 buildDate 确实透到 HTTP 面。
- `all2api.exe healthcheck` → 打印 `healthcheck OK`，**exit=0** ⇒ Docker `HEALTHCHECK` 与 fpk `cmd/main status` 所依赖的子命令路径可用。
- 启动日志含 open-mode 警告（未配 api_key 时明示面板与 `/api/*` 无鉴权、不得暴露给不可信网络）⇒ 安全提示面存在，非静默裸跑。
- 收尾确认：进程已退出、`:7863` 无 LISTEN 残留（仅 TIME_WAIT）。

**仍挂账（本机环境硬限制，非代码问题）**：`go test -race`（`-race requires cgo` ⇒ `C compiler "gcc" not found`，
gcc/cc/clang 全缺）；docker 镜像构建 + `docker stop ≤10s` 的 SIGTERM 优雅窗口断言（`command -v docker` 无）；
gitleaks / gosec / govulncheck 无二进制。Windows 原生无法等价验证 Linux 的 SIGTERM 语义，这一条不因本机冒烟而改口。

**非阻塞观察（M2 处理，不卡 R1）**：`runHealthcheck`（`:180-195`）把 `http://localhost:7863/healthz` 写死，
不读 `cfg.Listen`；若部署改端口，容器 HEALTHCHECK 与 fpk status 会误判失败。裁定三.4 只免 `noctx` 这一条 lint，
未授权改行为，故本轮登记不动。裁定一（调度器 ctx 未接线）维持 M2 前置条件登记。

### 9.11 merged tip 收尾确认（main = `05a0d41`，R1 合流后）

合流结构独立复核：`git rev-list --parents -n1 05a0d41` = `05a0d41c275e… d1db62b28aaa… b308646d08d6…`
⇒ 确为双亲 merge commit（--no-ff），本地 `main` = `05a0d41`，工作树干净，**未 push、未触 Actions**。

内容一致性：
- `git diff d1db62b..05a0d41` = 恰 13 个 `.go` 文件，非 Go 路径数 **0** ⇒ 合流未夹带代码外变化。
- `git diff b308646..05a0d41` = 仅 `.golangci.yml` + `docs/DEVOPS-M0-NOTES.md`，二者均来自我的 `d1db62b` ⇒ **merge commit 自身代码增量 = 0**。
- 13 枚 `.go` blob 在 merged tip 与分支栈顶 **逐一同值**（main.go `3bdb79e8`、base.go `0c066666`、models.go `a546f047`、
  registry.go `71b966db`、api.go `c2b2d980`、recover_test.go `c9979ad8`、circuit.go `78ec42e5`、config.go `8c7e624b`、
  workbuddy adapter.go `c37fadce`、pool.go `03a3d05a`、schedule.go `e158f037`、stickiness.go `e402ed1d`、panel.go `d65388a6`）。
- 冻结面扫描的口径修正：`9963226..05a0d41` 对冻结清单**有 2 个命中** = `.golangci.yml` 与 `docs/DEVOPS-M0-NOTES.md`，
  二者都来自 devops 工件线（§9.10 + PM seq=692 追认），**代码线 `9963226..b308646` 对该清单仍为零命中**。
  台账若写"merged tip 冻结面零命中"不准确，须按"代码线零命中 + 工件线两项已追认"表述。

门禁复跑（同一便携工具链、`git archive` 导出 `r3src`、gofmt 用 `r3mirror` 的 LF blob 镜像）：

| 门禁 | @b308646 | @05a0d41（merged） |
| --- | --- | --- |
| `go vet ./...` | exit=0 | **exit=0** |
| `go build ./...` | exit=0 | **exit=0** |
| 四目标交叉编译 | 4/4 | **4/4，产物字节数逐一相同**（6,529,208 / 6,160,568 / 6,718,464 / 6,291,426） |
| 逐包矩阵 | 15/15 | **15/15** |
| `go test ./...` | 0 FAIL | **0 FAIL**，`internal/api ok 0.056s`，`-v` **5/5 PASS** |
| `gofmt -l` | 0/30 | **0/30** |
| `golangci-lint run ./...` | 0 条 / exit=0 | **0 条 / exit=0**（用的是树内 `.golangci.yml`，证明豁免随 main 生效，不依赖本机额外配置） |

运行时冒烟（merged 产物）：`/healthz` → 200
`{"status":"ok","service":"all2api","version":"merged-05a0d41","buildDate":"2026-10-13T17:31:53Z","uptime":"1.63s"}`；
`healthcheck` 子命令 exit=0；启动 banner `All2API gateway vmerged-05a0d41 (05a0d41)` ⇒ version / commit / buildDate
三项在真实进程上同时可见，"运行版本 == 打标版本"的核对链路自此成立（M0 挂账的发布证据项补齐）。进程退出后 `:7863` 无残留监听。

挂账不变：`-race`（无 CGO/gcc）、docker 镜像与 `docker stop ≤10s` 的 Linux SIGTERM 窗口、gitleaks / gosec / govulncheck。

### 9.12 云端首跑全绿核验（主人 push `14bdaa3`，run 38494575451）

**远端事实**：`git ls-remote origin refs/heads/main` = `14bdaa331660f99a014fa52a8e4be621d39b2c65` = 本地 `main`
⇒ 领先的那 9 枚全部上仓，M1 工件线首次完整进远端。远端只有 `main` 一个分支，无多余 ref。

**run 事实**（只读 REST，未重跑、未取消、未改 Settings）：`ci` / push / `main` / head `14bdaa331`，
id `38494575451`，`created 01:13:56Z → updated 01:20:11Z`，`run_duration_ms = 375000`（6 分 15 秒），
**`conclusion = failure`**，但 **`billable.UBUNTU.total_ms = 0`（10 个 job 全部 duration_ms=0）** ⇒ 公共仓库 0 计额再次实测成立。
`release.yml` 未被触发（tag-only，符合设计）。

**逐 job（10 个）**：

| job | 结论 | 关键步 |
| --- | --- | --- |
| panel / build | ✅ | `Detect panel project` → `present=true`，install/typecheck/build 三步真跑；产物非空断言与 artifact 上传 = `skipped`（面板尚无 bundle，M1 内 fronted 项，口径不变） |
| lint / vet | ✅ | `go vet` / `gofmt check` / `golangci-lint` 三步全 success ⇒ §9.11 本地判定在 Linux 上复现，noctx 豁免随树生效 |
| **test / race** | ✅ | **`Unit tests with race detector` success** ⇒ 本项目**首次 Linux 面 `-race` 证据**，§9.9/§9.11 的 `-race` 挂账正式收掉 |
| build ×4 | ✅ | linux/amd64、linux/arm64、windows/amd64、darwin/arm64 全绿 + Hash + 60MB 体积护栏通过 |
| docker / multi-arch | ✅ | qemu + buildx，`linux/amd64,linux/arm64` 双架构真构建（provenance max + sbom）⇒ §9.7 的 `COPY go.mod` 与 `.dockerignore` 修复被验证 |
| smoke / healthz | ✅ | `Wait for healthy` / `Assert healthz payload identifies the service` / **`Assert graceful shutdown within stop grace period`** 全 success ⇒ M0 验收项云端兑现，且 A2（信号→Shutdown）首次拿到执行面证据 |
| security / scans | ❌ | `Secret scan (gitleaks)` ✅；**`gosec` 步失败**；`Dependency vulnerability scan (OSV)` = **skipped** |

**security 红的真因（我的工件缺陷，本地零额度复现，非代码安全问题）**：
ci.yml 写的是 `args: -exit-code 1 -severity HIGH ./...`，而当前 gosec CLI **已无 `-exit-code` 这个 flag**。
本机用 `v2.22.2` 与 `v2.29.0` 两个版本跑同一串参数，均在参数解析阶段中止：
`flag provided but not defined: -exit-code`，**exit=2，一个文件都没分析**。⇒ 这一红是工件线（devops），不是代码线。
附带缺陷：该步一失败，后面的 OSV 步被标 `skipped`，所以 **govulncheck 至今从未真正执行过**（云端 + 本地都是零证据）。

**修好参数之后的真实安全面**（`gosec -severity high -fmt=text ./...` @merged tip，23 文件 / 2773 行）：exit=1，**2 条 HIGH**：
1. `internal/pool/pool.go:73` **G404**（CWE-338，Confidence MEDIUM）`rand.New(rand.NewSource(time.Now().UnixNano()))`
   —— 账号池加权随机选择的 rng，非密钥用途，但时间种子可预测轮换顺序。归 backend 线，待裁定（改 crypto/rand 种子 或 `#nosec` 附理由）。
2. `internal/config/config.go:146-150` **G101**（CWE-798，**Confidence LOW**）—— 命中的是
   `TokenKeepalive: "0 22 * * *"`：**字段名含 `Token` + 字符串字面量**触发的名字启发式，值是 cron 表达式，不是凭据（gitleaks 亦判 0 秘密）。
   键名属冻结契约面，不能改名 ⇒ 出路是就地 `#nosec[G101]` 附理由（backend 线），**不建议**用全局 `-exclude=G101` 放松门禁。

MEDIUM 4 条（**不作为门禁**，但两条有审查价值，已转 backend/QA）：
`internal/web/panel.go:129` **G705** XSS via taint（A1 已清洗+编码，gosec 仍看到请求头 → html 的污点流，属残留告警）；
`internal/config/config.go:162` 与 `internal/api/trail.go:35` **G304** file inclusion via variable（配置路径/留痕路径，按设计）；
`internal/api/api.go:90` **G117** `json.Marshal(cfg)` 含 `APIKey` ⇒ **机器印证了既有挂账「api_key 遮蔽」**（面板 raw config 返回体）。

**工件修复（本地提交，未 push）**：ci.yml 的 security job 改为 —— gosec **版本 pin `v2.29.0`（不再 `@master`）**、
参数改 `"-severity high -fmt=text ./..."`、三个扫描各自 `continue-on-error: true`、新增 `Aggregate scan verdict`
（`if: always()`）按 `steps.*.outcome` 汇总判红 ⇒ 任一扫描失败不再掩盖另外两个的证据。HIGH 仍是阻断阈值，未放松。
Makefile：`scan` 的 `-severity` 大小写对齐；新增 `workflows` 目标（actionlint 离线校验）。

**新增离线门禁 `actionlint v1.7.12`**：对现行 `ci.yml` + `release.yml` **零告警**；用故意写坏探针验证它能抓住
§9.7 那类云端拒绝——探针里 job 级 `hashFiles` 直接报
`calling function "hashFiles" is not allowed here … only available in jobs.<job_id>.steps.*`，正是前两次 run
`jobs=0 / conclusion=failure` 的根因。⇒ 这类错误今后在本地 `make workflows` 就该被拦，不必再靠 push 试。
**未把 actionlint 放进 CI**：那需要「下载并执行 release 二进制」，属面扩大，待 PM/主人裁定后再落。

**仍挂账（本轮后）**：govulncheck（从未执行，参数已就绪，等下一次 owner push 顺带首跑）；
fnOS fpk 真机六项；M2 接线批（Scheduler 实例化 + `Start(ctx)` 消费 shutdown + `TaskHandler` ctx 转正 +
`runHealthcheck` 硬编码 `:7863` 改读 `cfg.Listen`）；api_key 遮蔽（本轮获 G117 机器印证）。
**已收**：`-race`（Linux 首跑绿）、docker 多架构构建、`/healthz` smoke 含 `stop ≤10s`、gitleaks、build×4 云端、`go test` 云端。

## 9.13 主人本机新装 Go 原生复跑对照（Goal 判据 ①b）+ R2-sec 双签复核

### 9.13-a 环境探测（实测，非推断）

| 探针 | 实值 |
| --- | --- |
| `go version` | **go1.27.2 windows/amd64**（`C:\Program Files\Go\bin\go.exe`） |
| `go env GOROOT / GOPATH / GOCACHE` | `C:\Program Files\Go` / `C:\Users\User\go`（**该目录不存在** ⇒ 主人只装了 Go 本体，无任何 `go install` 工具） / `AppData\Local\go-build`（已有 201 MB 编译缓存） |
| `go env CGO_ENABLED / CC / GOTOOLCHAIN` | **0** / `gcc`（仅是默认命令名，**磁盘与 PATH 上都没有 gcc**） / `auto` |
| C 编译器 | `gcc`、`cc`、`clang`、`tcc`、`x86_64-w64-mingw32-gcc` 全部 `NOT on PATH`；`Program Files` 两层内无 gcc/clang 可执行 |
| gosec / golangci-lint / actionlint | 主机**均无**（`C:\Users\User\go\bin` 不存在）⇒ 本机唯一可用副本仍是便携链 |

**`-race` 本机可行性（用最小探针坐实，不猜）**：两条独立证据同一结论——
1. 真包：`CGO_ENABLED=1 go test -race ./internal/api` → `# runtime/cgo` / `cgo: C compiler "gcc" not found: exec: "gcc": executable file not found in %PATH%`，**exit=1，build failed**；
2. 最小探针（5 行 `import "C"` 程序，独立临时 module，不碰仓库）：`CGO_ENABLED=1 go build` → **同一条 gcc not found 错误**，exit=1。
⇒ **Windows 面 `-race` 在主人本机装了 Go 之后依旧不可行**，阻塞点是 CGO 依赖的 C 编译器，与 Go 版本无关。
（附注：`CGO_ENABLED=1 go build ./...` 会 exit=0——本项目全包纯 Go，不触发 cgo 编译；所以**必须用上面两种强制探针**，否则会得到"CGO 可用"的假结论。这是本轮方法论上一个值得记的坑。）
⇒ `-race` 的证据来源仍然只有云端 Linux job（§9.12 已收），本机不补。

### 9.13-b 版本 skew：新装 Go 跑不动 CI 钉版的分析工具（本轮最重要的意外发现）

CI 钉的是 `GO_VERSION: "1.24"` + `golangci-lint-action@v6 / version: v1.64.8`；主机 Go 是 **1.27.2**。同一源码树、同一命令交叉实测：

| 工具（CI 钉版） | 便携 go1.24.13 下 | 系统 go1.27.2 下 |
| --- | --- | --- |
| `golangci-lint v1.64.8 run ./...` | **exit=0，0 条** | **exit=3，无法运行**：`cannot decode "internal/goarch", export data version 5 is greater than maximum supported version 2`；并假报 typechecking error（`workbuddy/adapter.go:20 a.SetName undefined` —— 嵌入方法因依赖包 export data 载入失败而"消失"） |
| `gosec v2.29.0 -severity high ./...` | 干净：0 处载入错误 | **63 处** `could not import …（cannot decode …, export data version 5 > maximum supported version 4）`，**但汇总表照样打印 `Issues : 0`** |

⇒ 两条硬结论：
1. **系统 go1.27.2 不能作为 lint / scan 门禁的执行链**；gosec 在降级（packages 载入失败）状态下仍输出 `Issues : 0`，是**假绿**，绝不能据此判安全面干净。
2. 便携链 `go1.24.13 + golangci 1.64.8 + gosec 2.29.0 + actionlint 1.7.12` 与 CI 钉版同构，是**本机唯一与云端等价的链** ⇒ 删留口径应据此定：**保留**。
3. 登记一条真实风险（M2 待落 CI 注释）：**若 backend 抬 `go.mod` 的 Go 版本，必须在同一 commit 抬 golangci-lint / gosec 版本**，否则门禁不是红，而是**静默假绿**——比红危险。

### 9.13-c 原生链 vs 便携链逐项对照（源码 = `main@bc60417`，导出到临时目录，不碰检出）

| 门禁 | 便携 go1.24.13 | 系统 go1.27.2 | 判定 |
| --- | --- | --- | --- |
| `go vet ./...` | exit=0 | exit=0 | 一致 |
| `go build ./...` | exit=0 | exit=0 | 一致 |
| `go test ./...` | `ok internal/api` | `ok internal/api 0.737s` | 一致 |
| `go test -v ./internal/api` | 5/5 PASS | **5/5 PASS**（recover 402 / 凭证失效 / 未知 source / 需 APIKey / halve-budget source 过滤） | 一致 |
| `gofmt -l`（`git cat-file blob` LF 镜像，30 文件 0 CR） | **0/30** | **0/30**，且两版 `gofmt -d` 输出**逐字节相同（均 0 字节）** | 一致，跨版本无格式化分歧 |
| `golangci-lint v1.64.8` | **0 条 / exit=0** | 无法运行（见 9.13-b） | **不一致：仅便携链可用** |
| `go test -race` | 不可行（无 gcc） | **同样不可行（无 gcc）** | 一致（都是环境限制） |

四目标交叉编译产物字节数（**同一棵树、同一 flags**：`CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version/commit/buildDate"`）：

| 目标 | 便携 go1.24.13 | 系统 go1.27.2 | delta |
| --- | --- | --- | --- |
| linux/amd64 | 6,529,208 | 7,348,384 | **+819,176（+12.5%）** |
| linux/arm64 | 6,160,568 | 6,815,904 | +655,336（+10.6%） |
| windows/amd64 | 6,718,464 | 7,565,824 | +847,360（+12.6%） |
| darwin/arm64 | 6,291,426 | 6,968,466 | +677,040（+10.8%） |

⇒ 差异**纯属 Go 1.24 → 1.27 的编译器/运行时体积变化，不是代码膨胀**（源码逐字节相同）；CI 的 60 MB size budget 远未触及。
⇒ 另一条回归价值：便携链在 `bc60417` 上重建出的四个字节数与 §9.11 在 `05a0d41` 记录的**逐一完全相同** ⇒ 便携链口径跨轮稳定，可当体积回归基线用。

**原生链运行时冒烟（go1.27.2 产物）**：`GET /healthz` → **200**，body `{"status":"ok","service":"all2api","version":"v0.1.0-m1-nativecmp","buildDate":"2026-10-14T01:00:00Z","uptime":"3.51s"}`；
`gw127.exe healthcheck` → `healthcheck OK`，**exit=0**；banner 保留 open-mode 警告。
收尾：进程已停（Stop-Process 后 `:7863` 探测 `http_code=000` = 无监听）、临时 config 只写空 api_key、scratch 目录已删。

### 9.13-d R2-sec 双签复核（分支 tip `4109d6c`，gosec HIGH=0 独立执行方）

**独立取证（未采信自报）**：`git rev-list --parents -n1 4109d6c` = 单亲 **bc60417** ✅；`git diff bc60417 4109d6c` = **恰 2 文件 / 2 增 2 删**，全部是行尾 `// #nosec …` 追加；非 Go 路径数 **0**；权威 blob `config.go ac63914` / `pool.go ee4ddc8`，基线 `8c7e624` / `03a3d05` 与 R1 链条吻合 ✅；`cryptoSeed` 试验**无残码**（diff 面即全部改动）✅。

**合入门禁实测**（便携 go1.24.13 + gosec v2.29.0，参数与 CI 修复后完全一致 `-severity high -fmt=text ./...`）：
```
Summary:
  Files  : 23
  Lines  : 2773
  Nosec  : 2
  Issues : 0
```
⇒ **HIGH=0 达成**，exit=0。`Nosec : 2` 恰等于两处豁免点数 ⇒ 证明是**按行生效**的定点抑制，不是 `-exclude` 关规则、不是降阈值（与裁定一约束一致）。
⚠️ 同一命令在系统 go1.27.2 下也印 `Issues : 0`，但带 63 处包载入错误 ⇒ **不计入独立证据**，以便携链那次为准（见 9.13-b 的假绿警告）。

**快子集 @4109d6c**：`go vet` / `go build ./...` / `go test ./...`（`ok internal/api`）在**两条链下均 exit=0**；`gofmt -l` **0/30**（LF 镜像，两链一致）⇒ 新增的行内注释未破坏结构体字段对齐，lint/gofmt 门禁不受影响。

**devops 侧判定**：R2-sec 双签中「gosec 实测 HIGH=0」这一签**成立**，条件是把工具链钉在便携链（go1.24.13 + gosec v2.29.0）。

## 9.14 merged tip `22409df` 收尾复验（R2-sec 合流后，安全线收口）

**合流结构独立复核**（未采信自报）：
- `git rev-list --parents -n1 22409df` = `22409df / 8de54360 / 4109d6cf` ⇒ **确为 `--no-ff` 双亲 merge commit**，base 取 `main@8de5436`，与 PM seq=739 指令一致 ✅
- `git diff 8de5436..22409df` = 恰 2 文件 / +2 −2（`config.go`、`pool.go` 行尾 nosec），**非 Go 路径 0 个** ⇒ 合流未夹带任何其他变化 ✅
- `git diff 4109d6c..22409df` = 仅 `.github/workflows/ci.yml` + `docs/DEVOPS-M0-NOTES.md`（我的 `8de5436` 工件本体）⇒ **merge commit 代码增量 = 0** ✅
- merged tip 两文件 blob 与分支 tip **逐一同值**（`ac63914` / `ee4ddc8`）⇒ 同一 tree 内容合入 ✅；工作树干净，未 push。

**门禁实测 @22409df**（便携链 go1.24.13 + gosec v2.29.0 + golangci-lint v1.64.8，与 CI 钉版同构；`git archive` 导出，检出与 worktree 未碰）：

| 门禁 | 结果 |
| --- | --- |
| `go vet ./...` | exit=0 |
| `go build ./...` | exit=0 |
| `go test ./...` | exit=0，`ok internal/api` |
| **`gosec -severity high -fmt=text ./...`** | **exit=0 / Files 23 / Lines 2773 / Nosec 2 / Issues 0**；载入错误 **0** 处、`Severity: HIGH` 标记行 **0** 行 ⇒ **安全线合入门禁达成** |
| `golangci-lint run ./...`（仓库内 `.golangci.yml`） | **exit=0，0 条**（noctx 豁免仍只钉 `cmd/gateway/main.go:181` 一行） |
| `gofmt -l`（LF blob 镜像 30 文件） | **0/30**，便携链与系统链（1.24.13 / 1.27.2）结论一致 |
| 系统 go1.27.2 快子集 | `vet` / `build` / `test` 均 exit=0（lint 与 gosec **不在本机 1.27 上出结论**，理由见 §9.13-b） |

**冻结面台账口径（供 PM 落 PRD v0.2.5）**：
- `14bdaa3..22409df` = 5 个路径：`internal/config/config.go`、`internal/pool/pool.go`（backend 代码线，各 1 行 nosec）+ `.github/workflows/ci.yml`、`Makefile`、`docs/DEVOPS-M0-NOTES.md`（devops 工件线，`bc60417` / `8de5436`，均已逐例追认）。
- `9963226..22409df` 对冻结清单命中 = **4 项，全部来自 devops 工件线**（`.github/workflows/ci.yml`、`.golangci.yml`、`Makefile`、`docs/DEVOPS-M0-NOTES.md`），代码线对冻结清单命中 = **0**。
⇒ 台账仍按既定口径落字：「**代码线冻结面零命中 + 工件线四项已逐例追认**」，不写"merged tip 冻结面零命中"。

**devops 侧判定 @22409df**：R2-sec 在 CI/门禁面**已验证 = 绿**，gosec HIGH 由 2 → 0，且经 §9.12/§9.13 的云端与安全线事实串联后，M1 安全线本地侧收口。
**唯一仍未闭合的依赖（不是缺陷，是顺序）**：`bc60417`（gosec 参数修复 + 聚合判定）与 `8de5436`（注释/台账）随 `22409df` 一起仍在本地 ⇒ security job 的云端首绿与 **govulncheck 首跑**必须等主人自主 push 后才拿得到；团队不提 push 请求。



