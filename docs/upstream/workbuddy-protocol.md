# WorkBuddy / CodeBuddy 上游协议调研（M2 第二批）

- 日期：2026-10-14
- 调研方式：**仅公开资料**（GitHub 开源实现 + 官方站点页面），未登录、未注册、未向任何真实端点发请求（本机对 `copilot.tencent.com` 的连通性探测也只到 DNS/TCP 层失败即止，未构造业务请求）。
- 结论先行：**证据等级 = 充分（可指导实现）**，但实现前置条件是持有真实账号凭证（access/refresh token），凭证获取路径本身不属于网关代码范围。详见 §9 判定。

---

## 0. 产品甄别：WorkBuddy 与 CodeBuddy 的关系

- **CodeBuddy** 是腾讯云代码助手（`www.codebuddy.cn`，页面标题「腾讯云代码助手 CodeBuddy - AI 时代的智能编程伙伴」）。其文档中心在 `www.codebuddy.cn/docs`，但官网页面为 SPA，公开可抓取的正文极少，**未见任何公开 API 协议文档**（来源：codebuddy.cn、codebuddy.cn/docs 页面抓取，2026-10-14）。
- **WorkBuddy** 是同一后端体系的国际/关联品牌：`www.workbuddy.ai` 页面自述「WorkBuddy - AI Agent for Everyday Office Work」（来源：workbuddy.ai 页面抓取，2026-10-14）。开源实现的交叉证据显示两者共享同一套 `/v2/*` 协议：
  - HanawaBanana/workbuddy2api 的 `internal/upstream/headers.go` 中 `originRefererCN = "https://www.codebuddy.cn"`、`originRefererGlobal = "https://www.workbuddy.ai"`，按账号 `realm`（cn/global）切换 Origin/Referer/UA 品牌段，chat base 统一为 `https://copilot.tencent.com`（文件头注释「规则来自 docs/api-reference.md §0/§4/§6」）。
  - xiaofan6ya/workbuddy2api README 自述「把 WorkBuddy / CodeBuddy（腾讯代码助手）的桌面端登录态，转成 OpenAI / Anthropic 兼容 API」，架构图明确「腾讯后端 `https://copilot.tencent.com` 本身走标准 OpenAI chat/completions 协议」。
- 判定：**WorkBuddy ≈ CodeBuddy 的桌面端/国际版品牌**，二者后端协议同源（同一 `/v2/chat/completions`），网关默认平台名 `workbuddy` 可覆盖两域，靠账号 `realm`（cn/global）区分路由。

## 1. 来源与可信度分级

| 来源 | 类型 | 可信度 | 说明 |
|---|---|---|---|
| [HanawaBanana/workbuddy2api](https://github.com/HanawaBanana/workbuddy2api) @ `b262660` (2026-10-11) | Go 开源实现（原 Sliverkiss/workbuddy2api 删库后的延续仓，MIT） | 高 | 大量「实测」注释（端点、业务码、响应形状均标注实测日期）；本仓 internal/platform/workbuddy 壳的任务清单（签到/猫猫旅行/token 保活）与其调度器一一对应 |
| [xiaofan6ya/workbuddy2api](https://github.com/xiaofan6ya/workbuddy2api) @ `93f4872` (master) | Python 开源实现 + 桌面端逆向报告 | 高 | 独立实现的同一协议，头族/错误码表标注「从官方桌面端 app.asar 的 CLI bundle 提取的权威枚举 ServerErrorCode」 |
| [HanHan666666/codebuddy2openai](https://github.com/HanHan666666/codebuddy2openai) | Python 单文件实现 | 中高 | 最早期项目（85 stars），佐证核心链路：`copilot.tencent.com/v2/chat/completions` 原生 OpenAI 协议 + SSE |
| codebuddy.cn / workbuddy.ai / copilot.tencent.com 官方页面 | 官方站点 | — | 仅产品名与定位，**无任何公开 API 协议文档** |
| wb_research/wb2api_tree.json（上轮遗留） | GitHub API tree 快照 | 高 | 已核对 sha `b262660` = HanawaBanana master head（2026-10-14 现值），非陈旧快照 |

两套独立实现（Go/Python）对同一端点、同一头族、同一业务码给出一致描述，且 Python 侧标注了从官方客户端二进制提取的一手证据——这构成交叉验证。

## 2. 端点拓扑（base + path）

双域架构：**chat 域**与 **billing 域分离**，global realm 复用同一路径族但换 base，且部分路径少一层 `/v2` 前缀。

| 用途 | CN base | Global base | 路径 | 方法 |
|---|---|---|---|---|
| chat 补全 | `https://copilot.tencent.com` | `https://www.workbuddy.ai` | `/v2/chat/completions` | POST |
| token 刷新 | 同 chat base | 同 chat base | `/v2/plugin/auth/token/refresh` | POST |
| 登录 state（设备授权第一步） | 同 chat base | 同 chat base | `/v2/plugin/auth/state?platform=CLI`（CLI 形态）/ `?platform=workbuddy`（Web 形态） | POST |
| 登录轮询 | 同 chat base | 同 chat base | `/v2/plugin/auth/token?state=` | GET |
| 登录账号信息 | 同 chat base | 同 chat base | `/v2/plugin/login/account?state=` | GET |
| 积分余额 | `https://www.codebuddy.cn` | `https://www.workbuddy.ai` | `/v2/billing/meter/get-user-resource`（global 首选无 `/v2` 前缀：`/billing/meter/get-user-resource`，404 回落 `/v2`） | POST（空 body `{}`） |
| 每日签到 | 同 billing base | 同 billing base | `/v2/billing/meter/daily-checkin`（global 同样先试 `/billing/meter/daily-checkin`） | POST（空 body `{}`） |
| 签到活动状态 | 同 billing base | 同 billing base | `/v2/billing/meter/checkin-activity-status` | POST（**必须 POST**，GET 404） |
| 活跃上报 | 同 billing base | 同 billing base | `/v2/report` | POST（事件数组） |
| 模型目录 | 同 chat base | 同 chat base | `/v2/enterprises/personal/models`（个人）；企业用 `/v2/enterprises/{enterpriseId}/models`；global 另有 `/v3/config` 探测 | GET |
| 成长任务（猫猫旅行等） | 同 chat base | 同 chat base | `/activity/growth/buddy/travel/status|depart|claim`、`/activity/growth/buddy/info|first|agreement`、`/activity/growth/streak|heatmap|redeem|makeup-cards/use|lottery/chances|lottery/draw` | GET/POST |

来源（逐条）：
- chat/refresh 路径与 base：HanawaBanana `internal/upstream/client.go`（`chatCompletionsPath = "/v2/chat/completions"`、`ChatBaseCN = "https://copilot.tencent.com"`、`defaultGlobalBase = "https://www.workbuddy.ai"`、`RefreshToken` 拼 `/v2/plugin/auth/token/refresh`），https://github.com/HanawaBanana/workbuddy2api/blob/master/internal/upstream/client.go
- xiaofan6ya `converter.py`：`BACKEND = "https://copilot.tencent.com"`、`/v2/plugin/auth/token/refresh`、`/v2/chat/completions`、`/v2/billing/meter/get-user-resource`，https://github.com/xiaofan6ya/workbuddy2api/blob/master/converter.py
- 登录流：HanawaBanana `cmd/login/main.go` 文件头（state/poll/account 三端点、`platform=CLI`）；xiaofan6ya `admin/wb_login.py`（`HOST = "https://www.workbuddy.cn"` Web 形态 11 步短信登录链、Keycloak realm `copilot`、OneID `oauth2.account.tencent.com`）
- billing/checkin/growth：HanawaBanana `internal/upstream/client.go`（billing 常量块）、`checkin_activity.go`、`travel.go`、`growth_bonus.go`、`growth_reward.go`、`report.go`
- 模型目录：HanawaBanana `client.go`（`cnModelsPath`/`globalModelsPath`/`v3ConfigPath`）；xiaofan6ya `converter.py` `fetch_models()`（`/v2/enterprises/{personal|eid}/models`）
- 余额响应形状：xiaofan6ya `converter.py` `fetch_balance()`——`data.Response.Data.Accounts[]`，按 `CapacityUnit=="credits"` 过滤、取 `CycleCapacityRemain`（而非 CapacityRemain）

## 3. 鉴权

- **形态**：HTTP 头 `Authorization: Bearer <accessToken>`（chat 与 billing 均）；accessToken 为 JWT（xiaofan6ya 凭证校验要求「结构完整的 JWT 且能解出 payload」）。
- **凭证来源**：桌面端登录态文件（Windows `%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\*.info`，macOS `~/Library/Application Support/CodeBuddyExtension/Data/Public/auth/*.info`），内含 `accessToken / refreshToken / domain / uid / enterpriseId` 等字段（HanHan666666 README「前置条件」；HanawaBanana `internal/auth/auth.go` 解析嵌套形/扁平形双形态）。
- **账号头族**（chat 必带，缺省用 `X-No-*` 约定）：
  - `X-User-Id: <uid>`（缺则 `X-No-User-Id: 1`）
  - `X-Enterprise-Id: <eid>`（个人号缺则 `X-No-Enterprise-Id: 1`）；billing 域另发 `X-Tenant-Id`
  - `X-Domain: www.codebuddy.cn`（CN）或 `www.workbuddy.ai`（global）；CN 无 domain 时发 `X-No-Department-Info: 1`
- **刷新机制**：`POST /v2/plugin/auth/token/refresh`，鉴权头**不放 Authorization**，而是 `X-Refresh-Token: <refreshToken>` + `X-Auth-Refresh-Source: plugin`；响应信封 `data` 内 `{accessToken, refreshToken, expiresIn, domain}`，**双 token 总是一起轮换**，实测 `expiresIn` 恒 5184000（60 天）（HanawaBanana `client.go` RefreshToken 注释：实测 R-D/R-E/R-F）。安全红线：refreshToken 只允许出现在 refresh 端点，绝不进 chat 头（`headers.go`：「绝不在 chat 请求里携带 X-Refresh-Token」）。
- **无 token 降级形态**：chat 头允许 `X-No-Authorization: 1`（无凭证占位），说明上游接受无鉴权请求（走匿名/试用额度），但生产以 Bearer 为主。
- **设备风控头（可选但重要）**：`X-Device-Token` 由桌面端内嵌腾讯 T-Sec TuringShield 原生 SDK 生成（DLL 采集硬件指纹向 `https://tdid.m.qq.com/tmf` 换取的不透明串，channelId=109144）；**纯服务端无法自行生成**，官方客户端缺失时降级为不发（或发 `X-Device-Token-Error`），网关可安全省略但更易被风控识别（xiaofan6ya README §1.3/§10.9.1，逆向 `main/tar.js` `buildHeadersWithTuringToken`）。
- 来源：HanawaBanana `internal/upstream/headers.go`（ChatHeaders/RefreshHeaders/BillingHeaders 全量头组）；xiaofan6ya `converter.py` `_build_headers_from`/`_refresh`；README §10.9.1。

## 4. 请求形状（/v2/chat/completions）

后端**就是 OpenAI Chat Completions 协议**（含原生 `tools`/`tool_calls`/SSE），但有若干强约束——三个独立实现均需做请求改写：

1. **强制 `stream:true`**——上游拒绝非流式（HanawaBanana `payload.go`「强制 stream:true（上游拒绝非流式）」；xiaofan6ya `upstream_compat.py` `prepare_upstream_body` 第 1 条；HanHan666666「后端只支持流式，非流式由本地聚合」）。
2. **`stream_options` 补 `{include_usage:true}`**——官方 CLI 流式必发，上游据此在末帧回 usage（两实现一致）。
3. **`tool_choice` 必须是字符串**——对象形态 400 `code=11101`（`"none"`→删 tool_choice+tools；`{"type":"function",...}`→函数名字符串）。
4. **`image_url` 必须对象形态** `{"url":...}`——字符串形态 400 `code=11101`。
5. **`role` 白名单**：`developer` 不在其中，须改写为 `system`，否则 400 `code=11-128`（"Illegal API invocation from an unapproved channel"）。
6. **`max_tokens`**：只认 `max_tokens`，`max_completion_tokens` 别名会被忽略后回落默认输出上限（实测 32000）。
7. **tool 配对**：assistant.tool_calls 与 role:tool 消息必须一一配对，坏历史会让该会话之后每条消息都 400，网关需先行剔除孤儿。
8. **thinking/reasoning**：DeepSeek 系开关 `thinking={"type":"enabled"|"disabled"}`；档位扁平字段 `reasoning_effort`（`off` 会 400，需删字段+`thinking.disabled`），非法档位 400 `code=11150`；模型目录下发 `reasoning.supportedEfforts/defaultEffort` 供降级。
9. **会话键**：body 可带 `conversation_id`（snake）/`conversationId`（camel）/`metadata.conversation_id`；`prompt_cache_key` 可注入做前缀缓存（HanawaBanana 称费用降 ~17×）。
10. **growth 任务驱动**：任务进度由**请求体** `extra_vars.growthEvent`（JSON 字符串形态，如 `"[{\"eventCode\":\"chat_request_send\",\"id\":\"<会话ID>\"}]"`）驱动，`POST /v2/report` 独立上报返回 200 但**不驱动任务进度**（xiaofan6ya README §6.1 逆向实测；注意与 HanawaBanana 的 `/v2/report` 上报实现形成两种口径——xiaofan6ya 明确指出 report 通道不记账）。

来源：HanawaBanana `internal/upstream/payload.go`（全文件）、`thinking.go`（引用见 client.go/payload.go 注释）、`session/ids.go`；xiaofan6ya `upstream_compat.py` 文件头 1–5 条 + `apply_thinking_intent`；README §6.1/§10.11。

## 5. 响应与 SSE 流式形态

- **流式**：标准 `text/event-stream`，帧格式 `data: <JSON>\n\n`，终止帧 `data: [DONE]`。帧内为 OpenAI `chat.completion.chunk` 形状：`id/model/created/choices[].delta{role,content,reasoning_content,tool_calls}/finish_reason`，末帧带 `usage`。思维链走 `delta.reasoning_content`（deepseek 系带 reasoning_effort 时必吐）。
- **非流式**：上游不支持；网关必须本地把 SSE 聚合成单个 `chat.completion`（聚合要点：delta 按 index 合并 tool_calls 分片、`message` 整条帧兼容、`finish_reason:"length"` 或 EOF 无 [DONE] 时剔除残缺 arguments）。
- **信封外错误**：上游会把内部故障塞进 HTTP 200 的错误信封（JSON-RPC `-32603`/`error` 字段），只看状态码会误判成功。
- 来源：HanawaBanana `internal/upstream/sse.go`（Aggregate/StreamHint 全文件注释）；xiaofan6ya README §10.4.1、§10.11；converter.py `_collect_stream`。

## 6. 错误分类与限流（429）特征

业务信封 `{code, msg, data}`（`code!=0` 即业务错误，**与 HTTP 状态码解耦**——限流可能包在 200/400 里返回，这是两个实现共同强调的头号坑）。

**错误码表**（xiaofan6ya 标注来源为官方桌面端 CLI bundle 的 `ServerErrorCode` 枚举，HanawaBanana 独立佐证其中大半）：

| code | 含义 | 处置建议 |
|---|---|---|
| 6000–6003, 6005–6007 | 限流族（TPS/TPM/TPH/RPS/RPM/RPH），账号级 | soft_rate（短冷却/换号） |
| **6004** CraftRateTPDLimit | 该账号×该模型**日**token 额度 | 只冷 (账号,模型) 对，换号或换模型 |
| **6008** CraftRateRPDLimit | 该账号×该模型**日**请求额度 | 同上 |
| 6020/6021/6022 | 模型排队（响应带 retry_after） | 读队列 retry_after |
| 14001/14012/14013/14014/14018 | 用量/额度耗尽（个人/企业/腾讯侧） | hard_credit（长冷却） |
| 14015/14016/14017 | 授权到期/企业未开通/试用未激活 | account_fault（账号级故障，需重登） |
| 11140 "request illegal" | 账号级授权风控（常带 403/429） | account_fault |
| 11101 | 请求体 Unmarshal 失败（tool_choice/image_url 形状错） | 请求级，不罚号 |
| 11102 "service info not found" | 该后端无此模型（400/404 确定性） | (账号,模型) 负缓存 |
| 11115 "prompt is too long" | 上下文超限（400/404/413） | 透传客户端 |
| 11133/11135 | 模型不支持图片 / 图片数据无效 | 透传客户端 |
| 11148 | tool_calls/tool 结果历史错配 | 透传客户端 |
| 11150 | invalid_reasoning_effort | 请求级 |
| 11-128 | "Illegal API invocation from an unapproved channel"（内容审核误杀 system 模板句/缺风控头） | 换中性 system 重试 |
| 12153 "Offline user session not found" | 401 + session 失效（账号禁用级；会瞬时抖动，连续 3 次才禁用） | session_dead |
| 11217 "login ing" | 登录未完成（轮询中） | 继续轮询 |
| 10001/14001(签到域) "已签到" | 重复签到的幂等拒绝 | 视为成功 |

**429/限流响应头**（机器契约，优先于文案解析）：
- `Retry-After`（整数秒，RFC 7231；日期形态按官方行为忽略）
- `retry-after-ms`（毫秒；HanawaBanana 口径）
- `x-ratelimit-reset` / `anthropic-ratelimit-unified-reset`（epoch 秒或毫秒，或 HTTP 日期）
- body 文案兜底：6004 文案含「将在 <UTC+8 时间> 重置」/ "reset at YYYY-MM-DD HH:MM:SS"（时区固定 UTC+8）
- **WAF 层**：403 且 body **无** `{code,msg}` 业务信封（HTML/空体）= APISIX WAF 拦截页（global 曾对 `/console` 路径挂内容规则，反引号/命令特征必 403——故 global chat 固定走 `/v2`），需 IP 级 fail-fast 而非罚号。

来源：HanawaBanana `client.go`（errorRule 词表、`ParseRetryAfter`、`IsModelRateLimit`、`IsWafBlocked`、`IsSessionDead`、`softRateResetLoc` 注释）；xiaofan6ya `admin/routers/proxy.py`（`_classify_error` 分层、`_ACCOUNT_FAULT_CODES`、`_MODEL_RATE_CODES`、`_QUOTA_EXHAUSTED_CODES`、`_SESSION_DEAD_MARKERS`）、`admin/model_limits.py`（`reset_from_headers`/`reset_from_body`）、README §10.4/§10.4.0。

## 7. 签到 / 任务类接口（对齐本仓 adapter.go 的任务清单）

本仓 `internal/platform/workbuddy/adapter.go` 预置 5 个任务（签到 09/21、猫猫旅行 09/21、token 保活 22、活跃上报 10），与上游端点对应关系全部有实证：

| 本仓任务 | 上游端点 | 要点 |
|---|---|---|
| 签到 | `POST /v2/billing/meter/daily-checkin`（空 body `{}`） | 重复签返回 code 10001/14001 "已签到"（幂等拒绝）；走 billing base + BillingHeaders（UA 单段 `WorkBuddy/<ver>`，不带 CLI 段） |
| 签到活动状态 | `POST /v2/billing/meter/checkin-activity-status` | **必须 POST**（GET 404）；响应 `data.{active, today_checked_in, streak_days, daily_credit, start_time, end_time, theme_name, ...}`；时间为 "2006-01-02 15:04:05" 无时区后缀、按 CST 解释（实测 2026-09-28） |
| 活跃上报 | `POST /v2/report`（billing base，事件数组） | 事件形状 `chat_request_send`（28 字段，`userId` 必填=uid，缺则 200 但静默丢弃）；每号每天 1 次即可点亮连登/解锁领养前置。⚠️ xiaofan6ya 逆向结论：真正驱动 growth 任务进度的是 chat body 的 `extra_vars.growthEvent`，`/v2/report` 不记账（两实现口径冲突，落码时需实测消歧） |
| 猫猫旅行 | `GET/POST /activity/growth/buddy/travel/status|depart|claim`（chat base） | depart body `{"location_id":1..4}`；claim body `{"record_id":<int64>}`→`reward_credit`；状态机 idle/traveling/arrived；`daily_limit_reached` 自然日 00:00 CST 重置 |
| token 保活 | `POST /v2/plugin/auth/token/refresh` | 见 §3；实测响应恒带 `expiresIn=5184000`（60d），双 token 同轮换 |

连登/奖励族（本仓未列但同域）：`GET /activity/growth/streak`（连登天数+补签卡+兑换状态）、`POST /activity/growth/redeem`（`{"tier":"7d","client_token":"<uuid>"}`）、`GET /activity/growth/lottery/chances`、`POST /activity/growth/lottery/draw`、`GET /activity/growth/heatmap`、`POST /activity/growth/makeup-cards/use`。

来源：HanawaBanana `internal/upstream/{client,checkin_activity,travel,growth_bonus,growth_reward,report}.go`（各文件头「实测」注释带日期）、`cmd/signin/main.go`；xiaofan6ya README §5（签到）、§6（growth 状态机 not_accepted→accepted→in_progress→completed→claimed，先 accept 才计数、completed≠claimed）。

## 8. 客户端指纹（风控头族，公开实现共识）

两个实现独立收敛到同一组「拟真头」，反证上游按头形状做客户端识别：

- UA 三段式：`WorkBuddy/<clientVer> WorkBuddy[ AI]/<clientVer> CLI/<cliVer>`（CN 段 `WorkBuddy`、global 段 `WorkBuddy AI`；报错品牌段可能 403 11140；版本须随官方发版更新——5.5.4/2.137.1 为 2026-09 观测值，xiaofan6ya 观测 5.5.6）
- `X-CodeBuddy-Request: 1`（官方所有 API 请求必带的闸门头）
- `X-Requested-With: XMLHttpRequest`、`Accept-Language`（CN zh-CN / global en-US）
- 用量归属：`X-Agent-Purpose: conversation` + `X-IDE-Name/Type: WorkBuddy` + `X-IDE-Version: <ver>` + `X-Product: WorkBuddy`
- 会话头族：`X-Conversation-ID`（透传）、`X-Conversation-Request-ID`/`X-Root-Request-ID`（**对话轮级聚合主键**，换号重试必须复用同值）、`X-Conversation-Message-ID`/`X-Request-ID`（消息级 32hex）、`X-Trace-ID`、`X-B3-TraceId/X-B3-SpanId/X-B3-Sampled`（16/32hex 规范）；xiaofan6ya 另发 W3C `traceparent`+单头 `b3`（与 B3 同值）
- 设备标识：`X-Machine-ID`/`X-Session-ID`（按 uid 稳定派生，「每账号一台固定虚拟设备」；xiaofan6ya 用 md5(salt:uid) 截断，HanawaBanana 用 sha256 截 36hex——形态自由，稳定性是关键）
- 反向证据（**不能发**的头）：`X-Moderation-Type`/`X-Expert-Id`/`X-Expert-Team-Task` 在 5.5.6 CLI bundle 命中数为 0；`X-B3-ParentSpanId` 仅父 span 存在时官方才带；`X-Machine-Id`（官方仅此拼写）不在模型请求上——多发比不发更易识别
- 内容审核：system 模板句（Claude Code/Codex 注入的合规声明）会被腾讯审核误杀（400 11-128），社区做法是对 system 合规词插零宽空格降敏或换中性提示词重试

来源：HanawaBanana `internal/upstream/headers.go`（全文件，含「官方无任何 UA 随机化」「step1 指纹文档 §1.2/1.3/4」引用）；xiaofan6ya README §10.9/§10.9.1（含正向/反向头清单与 Turing 结论）。

## 9. 实现判定

**判定：充分（可落码实现 `adapter.Sender`），但带两项边界说明。**

理由：
1. **协议形状完全实证**：端点、鉴权头、请求改写规则、SSE 形状、错误码表、签到/任务端点均有两套独立开源实现交叉验证，且 Python 侧标注了从官方桌面端二进制提取的一手枚举；核心链路（`copilot.tencent.com/v2/chat/completions` = 标准 OpenAI 协议 + 强制流式）三个实现零分歧。
2. **与网关契约对齐无障碍**：上游 wire 形状就是 OpenAI Chat Completions，本仓 `adapter.Sender` 的 ChatRequest/ChatResponse/StreamChunk 即透传映射；base URL 天然可注入（CN/global 两套常量在参考实现里就是 config 字段），httptest 桩全测可行——参考实现自身就是靠 httptest 等价物（fake transport 回放）完成全测试的。
3. **边界一（凭证）**：网关实现不解决凭证获取（桌面端 OAuth/短信登录链复杂且依赖真实账号），`pool.Account` 现有字段（ID/Credit/Health/...）也不携带 access/refresh token——落码需同时设计凭证注入面（如 account 侧扩展或 adapter 级 credential provider），这是接口设计决策而非协议盲区。
4. **边界二（风控头不可复刻部分）**：`X-Device-Token` 依赖桌面端 Turing SDK，纯服务端不可生成（官方自身有缺省降级路径，可省略）；`X-Machine-ID/X-Session-ID/会话头族` 等可按 §8 自行派生。省略风控头的请求可用但更易被识别/限流，属于运营风险而非功能缺口。

**公开资料未见/存在口径分歧的维度**（诚实清单）：
- 官方文档侧：codebuddy.cn/workbuddy.ai/copilot.tencent.com 均无公开 API 协议文档，全部知识来自社区逆向（官方随发版可能变更头校验/路径）。
- `/v2/report` 与 `extra_vars.growthEvent` 两种活跃记账口径冲突（§7），无第三方仲裁，需持真实账号实测消歧。
- `prompt_cache_key` 仅 HanawaBanana 单源提及（收益 ~17× 声明无法独立复核）。
- global 域 `/v3/config` 模型探测、`/v2/chat/busy` 繁忙度枚举（idle/normal/busy/congested/unavailable）为单源（xiaofan6ya），未交叉验证。
- 签到业务码 10001 与 14001 并存（两实现各引其一），精确值需实测。

## 10. Qoder 平台公开资料可得性（顺带，简）

Qoder（qoder.com，自称 Agentic Coding Platform，页脚主体 BRIGHT ZENITH PRIVATE LIMITED，有 qoder.cn 中国站）**官方提供 Agent SDK**（`@qoder-ai/qoder-agent-sdk`）与 Cloud Agents（SSE 事件流），文档站 docs.qoder.com，计费 credits 制——官方侧公开资料比 WorkBuddy 充分。社区逆向侧存在活跃生态：GitHub 搜索 `qoder2api` 命中 22 个仓库，代表项目 shuishuipingan/qoder2api-hub（161 stars，「COSY 签名推理、稳定设备指纹防风控、OAuth 设备授权」）、cubk1/qoder2api（90 stars，逆向 demo）。即：协议可从社区项目取证，且签名（COSY）/设备指纹类门槛比 WorkBuddy 更重。来源：qoder.com 页面、api.github.com 搜索（2026-10-14）。

## 11. Trae 平台公开资料可得性（顺带，简）

Trae（trae.ai，字节跳动 AI IDE，另有国内版 trae.com.cn 与网页版 work.trae.ai）官方**无任何公开 API/协议文档**，模型调用封闭在 IDE/订阅体系内。社区侧有反代项目：GitHub 搜索 `trae2api` 命中 15 个仓库，代表项目 autumnsentiment/Trae2api-cn（53 stars，「OpenAI-compatible Chat/Responses API with tool calling, reasoning effort, multi-account pool」）、connectedGraph/trae2api-web（41 stars，SOLO 反代+多账号池+保活）。取证难度与 WorkBuddy 相当（靠逆向+真实账号），但项目活跃度与文档化程度低于 workbuddy2api 生态。来源：trae.ai 页面、api.github.com 搜索（2026-10-14）。

## 12. 下一步建议（补齐什么）

1. **抓包样本**：一台已登录的 WorkBuddy/CodeBuddy 桌面端 + mitmproxy 抓 `/v2/chat/completions` 全头全帧（尤其确认 §8 头族在本版本的精确形状与 `X-Device-Token` 是否强制），这是唯一能固化协议时效性的手段。
2. **账号侧配合**：一只测试账号做 `/v2/report` vs `extra_vars.growthEvent` 记账口径消歧 + 签到业务码（10001/14001）实测 + global 域 `/v3/config` 探测复核。
3. **版本跟踪**：UA/版本段（client 5.5.x / CLI 2.137.x）随官方发版漂移，实现时应做成可配置（参考两实现均提供 config/env 覆盖；本仓 client.go 的 `wireClientVersion` 为包级 var，可由集成层覆写）。
4. **凭证注入设计**：先定 `pool.Account` 与 Sender 之间的凭证面（token 存放、刷新回调、realm 路由），再落 client——协议本身已无未知项阻塞这一步。

## 13. 已落码范围与剩余缺口（与 §9 判定对应）

按 §9 判定=充分，本批已实现 `internal/platform/workbuddy/` 的 `adapter.Sender`（client.go，stdlib only；全部测试走 httptest 本地桩，不触任何真实端点）：

**已实现**：
- 端点/双域路由：`{base}/v2/chat/completions`，CN/global 双 base（可经 `ClientOptions.BaseURL` 注入）。
- 鉴权头族：Bearer + `X-User-Id`/`X-Enterprise-Id`/`X-Tenant-Id`/`X-Domain`（含 `X-No-*` 占位）；`X-Refresh-Token` 永不上 chat 路径（有测试断言）。
- 指纹头：三段式 UA（global 段 `WorkBuddy AI`）、`X-CodeBuddy-Request: 1`、Origin/Referer、`Accept-Language` 按 realm。
- 请求改写（§4 的 1/3/4/5 条）：强制 `stream:true` + `stream_options.include_usage`、`tool_choice` 字符串化（none 连带删 tools）、`image_url` 对象化、`developer→system`。未实现 `max_completion_tokens→max_tokens` 翻译（Sender 契约的 ChatRequest 只有 `max_tokens` 字段，别名在 /v1 入口层面就不存在）与 `thinking` 注入（网关侧无该语义，接入属产品决策）。
- SSE：流式帧转发（含 `reasoning_content` 帧的透传丢弃——见下方缺口清单）、非流式聚合（content/tool_calls 按 index 合并/usage 末帧）。
- 错误分类（§6）：12153/401→auth_expired；6004/6008 与 6xxx/429/限流文案→rate_limited（含 Retry-After/retry-after-ms/x-ratelimit-reset 头解析与「将在…重置」文案透出）；402/412/1400x/余额文案→quota_exhausted；11102/11101/11115/11135/11148/11150/通用 404→not_found；11140 "request illegal"→auth_expired（account_fault 在网关词表无专名，按「需重登/轮换」语义归入）；WAF（403 无信封）/5xx/408/425→platform_down；**HTTP 200 包业务信封（code!=0）也分类**（按首字节 `{'` 区分 JSON 信封与 SSE 流，参考实现的头号坑）。
- Sender 通道契约：chunks 正常收尾关闭；errCh 至多一条错误且健康流不关闭；EOF 无 [DONE] 视为截断并投递 platform_down；ctx 取消即时退出（有专项测试）。

**实现侧明确缺口（诚实清单，接入生产前需补）**：
- **凭证面**：`CredentialSource` 接口已定义（token/uid/enterpriseId/domain/realm/过期时间），但没有实现——token 从哪来（auth 文件目录扫描、面板录入、刷新回写）是集成层决策，本批不代作。
- **token 自动刷新**：`POST /v2/plugin/auth/token/refresh` 协议已实证（§3）但未实现循环——刷新时机（过期前预刷/401 触发）与刷新凭证的存放属于凭证面设计。
- **`X-Device-Token`/`X-Machine-ID`/`X-Session-ID` 与会话头族（§8）**：未发送。缺省时上游可按「非真实客户端」加权识别（官方自身有缺省降级路径，功能不受阻）；这些头的派生策略（设备指纹口径）属风控决策面，且 `X-Device-Token` 纯服务端不可生成（§8 Turing 结论）。
- ~~**聚合响应的 `reasoning_content`**~~ **已闭合（2026-10-16）**：`adapter.ChatMessage` 扩展了 `ReasoningContent`（`json:"reasoning_content,omitempty"`），聚合（非流式）路径在 message 上透出累加的思维链，流式路径 delta 原样中继；严格 OpenAI 客户端忽略未知字段，agent 供应商（ZCode 等）渲染为思考轨迹。
- **模型目录/签到/任务端点（§2/§7）**：仅协议记录，未实现为 adapter 任务执行器——对应 `schedule.Upstream` 接口的落地属 M2 调度批次的后续工作。

**验证状态**：gofmt（新文件）/ `go vet ./...` / `go build ./...`（default、`-tags workbuddy`、`-tags full` 三变体）/ `go test -count=1 ./...` 全绿；golangci-lint 0 issues；gosec（severity high）0 issues。参考实现快照存于 `wb_research/src/*.txt`（改后缀防止其干扰模块级 `./...` 构建）。
