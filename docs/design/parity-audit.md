# all2api 面板 vs wb2api 逐项验收差距报告（parity-audit）

> 生成：2026-10-15 · 验收 agent 产出 · 依据运行实例 + 双方源码实测，非截图转述。
> 对比对象：
> - **参考（标杆）**：WorkBuddy2API 面板，`http://127.0.0.1:7863/panel/`（72KB SSR HTML + 147KB `app.js`，已抓全量解析）。
>   **勘误**：任务书称其源码在 `D:\CODE\vhub\panel.go`——实测该文件是 **vhub** 项目（多供应商聚合器，`vhub 控制台`，panel.go:167 起是另一套路由）。wb2api 真源码在 `D:\CODE\workbuddy2api-panel\internal\panel\`（panel.go / app.js / index.html），本报告全部以其为准。
> - **验收对象**：`http://localhost:7800/`（React 面板），源码 `D:\CODE\all2api\web-panel\src\`，后端 `D:\CODE\all2api\internal\`。所有 `/api/*` 已 curl 实测 200。
>
> 判定基线：主人已裁定事项优先——v1.1/v1.2 分平台信息架构、添加账号向导为一等操作（"文件即账号"废止）、不做假按钮、不发明上游协议、M1 红线（面板不落 storage）。
> 状态枚举：**已有 / 部分 / 缺失 / 明确不做**。批次：P0（本批立即）/ P1（下批）/ P2（缓）/ 不做（必须给理由）。

---

## 0. 全局与顶栏

| 功能点 | wb2api 行为 | 我们现状 | 状态 | 批次建议 |
| --- | --- | --- | --- | --- |
| 导航结构 | 顶栏 7 项：账号池/用量/积分构成/任务中心/模型与档位/配置/运行日志（app.js 7863 HTML nav 区） | 左栏 5 项：账号池（二级菜单：总览+WB/Qoder/Trae）/任务/模型/配置/日志（PanelApp.tsx:33-39,157-178） | 部分（架构差异属 v1.2 已裁定） | 维持；缺「用量」「连接」两页见下 |
| 状态指示 | 顶部 pills：连接中/版本 v-·-/redis/ttl（HTML `navPulse,navState,navVer,navRedis,ttl`） | 单 pill：在线(+时钟偏差)/未授权/离线，`GET /api/time` 探测+周期轮询+手动重探（PanelApp.tsx:57-96） | 部分（我们更聚焦，版本/redis 展示无对应后端面） | 维持；redis/版本不做（网关无该组件，做了是假指标） |
| 全局轮询 | `refreshVisible` 5s 轮询当前可见视图（app.js:1220）；登录轮询 3s（app.js:1138）；队列轮询（app.js:1751） | pill 探测轮询（PanelApp.tsx:72）；账号 30s（poolApi.ts:53）；日志 10s（poolApi.ts:54）；任务/模型仅手动刷新 | 部分 | P1：任务/模型挂低频轮询（纯前端 S），对齐"10 秒内回答健不健康" |
| 全局刷新按钮 | 顶栏常驻「刷新」（btnRefresh） | 无全局按钮，各屏自带刷新 | 部分 | P1 顶栏补全局刷新（纯前端 S） |
| 主题切换 | auto/light/dark 三态，**localStorage 持久化**（app.js:4,36） | light/dark 内存态（PanelApp.tsx:42-50） | 已有（差异是裁定内） | 维持：localStorage 持久化 **不做**（M1 红线，矩阵 §7） |
| 访问密钥门 | 401 时弹「需要访问密钥」模态框，key 存 localStorage（app.js:44,322,1099） | 设置屏 ApiKeySection 内存态输入（SettingsScreen.tsx:472-495）；401 只在 pill 显示"未授权" | 部分 | P0-：按矩阵 C6 补"数据屏密钥不匹配引导去配置屏"的空态/横幅（纯前端 S）。弹窗形态不做（体验取向不同） |
| 确认弹窗 | 原生 `confirm()`：移除/禁用/执行队列/一键完成（app.js:522-523,1295,1668） | DangerAction 组件用于指纹开关（components/DangerAction.tsx）；账号级删除确认未落 | 部分 | P0：账号删除/覆盖用 DangerAction（C14/C15 已规格化，矩阵 §1/§8） |
| 添加账号入口 | 顶栏常驻「添加账号」（btnAdd） | 仅分平台明细页工具栏（AccountsScreen.tsx:193-197）；总览页无（矩阵 §0 裁定） | 已有（裁定内） | 维持 |

## 1. view-accounts（账号池）

| 功能点 | wb2api 行为 | 我们现状 | 状态 | 批次建议 |
| --- | --- | --- | --- | --- |
| 状态卡行 | `账号池 n/m` pills（HTML subMeta） | 5 张卡：总数/可用/冷却/禁用/积分合计（AccountsScreen.tsx:213-234） | 已有 | — |
| FEFO 到期提醒横幅 | h3 常驻："按最近到期批次估算 · 日均需耗…（FEFO）" | 无 | 缺失 | P2（依赖积分批次数据面=packages，见 §5） |
| 昵称主显+uid 短码 | 昵称+uid 短码，行 title 全量 uid（app.js renderAccounts） | 昵称+id 全量+点击复制（AccountsScreen.tsx:96-114；wire nickname：panel_data.go:66） | 已有 | — |
| 状态徽标细分 | 可用/已禁用/已暂停选号/熔断·倒计时/连败降权/积分冷却/限流冷却 + reason 副标 + 限流模型子行（app.js:21-30 区段） | 三态徽标 active/cooling/disabled（AccountsScreen.tsx:32-40）；有 cooldown_end 时间戳列但**无冷却类型、无倒计时、无原因** | 部分 | P0：冷却列显示倒计时（`/api/time` 已有，纯前端 S）；类型/reason 副标 P1（后端 wire 增补 cool_kind/reason，M） |
| 积分「剩余/总额」+进度条 | 双值+进度条；企业版 `不限`；tooltip 含 per-model 实测单价（app.js:32-53 区段） | 单值 credit（wire 无 credits_total，panel_data.go:68） | 缺失 | P0：后端补 credits_total（workbuddy 余额配额/qoder Quota remaining，gap-vs-wb2api §1、§3 已裁定采纳）+ 前端双值条（合计 M） |
| 成功/失败 计数 | 表格列（2568/0） | 无（wire 无字段，curl 实测；gap 册 §1 裁定采纳 P0） | 缺失 | **P0**：路由结果回灌按账号累计，wire 加 success/fail，"本次运行"口径（后端 S-M） |
| 在途并发 | 表格列 | 无（同上裁定 P0） | 缺失 | **P0**：dispatch 前后原子计数（后端 S） |
| 最近成功 | "29 秒前" | 只有 `last_selected`（选号时刻≠成功时刻，panel_data.go:72） | 部分 | **P0**：UpdateHealth success 记 last_success 并上 wire（gap 册 §1 已裁定，后端 S） |
| 用量徽章组（次数/tok/延迟/吐字速） | 行内徽章+title | 无 | 缺失 | P1（依赖 usage 记账面 §6；本版不做假数——gap 冲 §1 裁定"裁剪→P1"） |
| 行内：暂停选号/恢复选号 | pause/resume 按钮+paused 状态徽（app.js:539-542） | 无（pool.Account 无 paused 态，internal/pool/pool.go 全文无） | 缺失 | **P0**（gap 册 §1 裁定"暂停选号 P0"）：后端 paused 字段+选号跳过+端点（M）、前端开关（S） |
| 行内：禁用/解冻 | disable/revive 按钮+confirm 文案（app.js:523,533,536） | revive 无 UI；disable 无（我们的 disabled=凭证死亡语义，不同于 wb2api 手动禁用） | 部分 | P0：行内 recover（=解冻）按钮——**后端已有** `POST /api/accounts/{id}/recover`（api.go:29-31，recover.go），前端从未渲染（全 src 无调用，grep 证实）。手动禁用 **不做**（我们语义里禁用=凭证失效，是观测结果非操作；需要时暂停选号已覆盖） |
| 行内：删除 | remove 按钮+强 confirm（删凭证文件，app.js:522,547） | 无 UI；后端无删除端点（api.go 全路由仅 recover/add） | 缺失 | **P0**（矩阵 §1 已裁定）：新端点 `POST /api/accounts/{id}/remove`（删凭证文件+池移除+热生效，M）+ 行内 DangerAction + C15 唯一账号确认文案 |
| 行内：签到/余额/任务 | 三按钮，是单账号详情弹层入口 | 无 | 明确不做 | gap 册 §1 已裁定裁剪：任务屏+行内数据承载，详情弹层 P1 再议 |
| 模型锁池表 | 第二张表：哪些模型不可用/锁几号/最早解锁/原因（renderModelLocks，app.js:456） | 无 | 缺失 | P1（需后端模型级限流追踪数据面；wb2api 有实证，但我们网关未采集——先落数据面再谈 UI） |
| 顶部批量按钮组 | 检查/全部签到/旅行巡检/活跃上报/全部保活（btnExp 等 5 键→`*_all` 端点） | 无 | 明确不做 | **维持不做**（story-map §6 已裁定：调度器自动跑；手动批量触发=P1 待办队列故事，需后端任务队列支撑） |
| 空态 | "账号池是空的→点右上角添加账号，用浏览器登录"（app.js:10-12） | 空态只有刷新按钮；文案过时："把 workbuddy-*.json 放入 auths\ 后重启网关"（AccountsScreen.tsx:176-186 提前 return 无添加按钮；zh.ts:259） | **部分（违背裁定）** | **P0**：空态加「添加账号」按钮（C1）+ 改写文案为向导指引（纯前端 S）。现文案直接违背 v1.1"文件即账号废止"裁定 |
| trae 明细页 | —（wb2api 单平台） | trae 无账号时同样落 workbuddy 文件指引文案 | 部分 | P0：与 C1b 合并修——未接入/未启用平台显示"启用/接入引导"而非文件指引（纯前端 S） |

## 2. 添加账号（wb2api 的添加弹层 vs 我们向导）

| 功能点 | wb2api 行为 | 我们现状 | 状态 | 批次建议 |
| --- | --- | --- | --- | --- |
| 浏览器登录流 | 「浏览器登录」tab：`login/start {realm}`→3s 轮询 `login/poll?state=`（app.js:1138 区段；端点 panel.go:157-159） | 无 | 缺失 | P1（story-map §4 已裁定：凭证粘贴 P0、设备授权流 P1；协议已实证） |
| 凭证导入 | 「导入 JSON」tab：文件上传 `POST /panel/api/import/cockpit`（app.js fetch 区；panel.go:160） | 粘贴框：workbuddy 凭证 JSON / qoder PAT（AccountsScreen.tsx:265-341；poolApi.ts:185-192；端点 api.go+accounts_add.go:21-44） | 已有（形态差异=粘贴 vs 文件，裁定内"用户不必碰文件"） | 维持；文件上传不做（同裁定） |
| 区域/realm 选择 | `login/regions` 下拉（panel.go:159） | 无（粘贴流不需要） | 不做（属登录流，随 P1 设备授权一起评估） | — |
| trae 向导 | — | **缺陷**：AddAccountPanel 对 trae 不渲染任何输入框但仍显示标题+保存键，提交会误走 workbuddy 端点（AccountsScreen.tsx:276 `platform === 'qoder' ? addQoder : addWorkbuddy`） | **部分（违背矩阵 §1a）** | **P0**：trae 时显示"该平台尚未接入网关"引导，不渲染表单（纯前端 S） |
| 重复 uid 覆盖提示 | — | 未实现覆盖确认（C14） | 缺失 | P0：与删除确认同批（后端 add 端点返回已存在信号，S） |

## 3. view-taskscenter（成长任务队列）

| 功能点 | wb2api 行为 | 我们现状 | 状态 | 批次建议 |
| --- | --- | --- | --- | --- |
| growth 任务清单+队列 | 每账号 automation/chat 任务、进度、奖励；扫描待办/执行全部待办+并发 1/2/3（`tasks/scan_all`、`tasks/run_queue`、`tasks/queue`，panel.go:174-176） | 无 | 明确不做（暂） | **需协议消歧**：gap 册 §2 已登记——growth 进度由 chat body `extra_vars.growthEvent` 驱动，与 `/v2/report` 口径冲突未消歧，做出来是假状态。挂协议调研专项，不进批次 |
| 开学季券码 | `school/vouchers` 弹层（panel.go:177） | 无 | 明确不做 | 活动向功能，all2api 上游无实证；主人提供抓包再议 |
| 任务 cron 展示 | （wb2api 无此面；cron 在配置页） | 四任务卡：中文名/说明/cron/enabled/最近运行统计（TasksScreen.tsx:178-223；wire 实测 last_run 恒 null） | 已有 | — |
| 平台页签 | — | workbuddy/qoder/trae 三页签；qoder/trae 显示"该平台暂无定时任务"（TasksScreen.tsx:149-165） | 已有（裁定内，不渲染假卡片） | — |
| 手动执行按钮 | 执行全部待办（confirm） | 无（TasksScreen.tsx:11 注释明示端点不存在不渲染假按钮） | 明确不做 | **维持不做**（矩阵 §2 已裁定；等 P1 待办队列故事） |

## 4. view-models（模型与档位）

| 功能点 | wb2api 行为 | 我们现状 | 状态 | 批次建议 |
| --- | --- | --- | --- | --- |
| 模型能力表 | 模型/积分倍率/默认档/思考档位/上下文/最大输出（HTML th 区） | 平台/模型/上下文/最大输出/工具/流/免费（ModelsScreen.tsx:165-196） | 已有（积分倍率/思考档位如实裁剪——本地目录无字段，源码注释明示） | 维持 |
| 上游实时"重新获取" | btnModels 拉实时目录（`api('models')` 是上游实时面） | 仅本地目录刷新 | 明确不做（本批） | P2：上游实时目录协议未在我方网关实证；矩阵 §3 已标注"上游实时查询未接入" |
| 搜索+5 筛选+排序 | 搜索框+域/能力/档位/价格/排序五下拉（HTML mdQ/mdRealm/...） | 仅平台过滤下拉（ModelsScreen.tsx:137-148） | 部分 | P1：搜索+排序纯前端（S，模型数>50 才有价值） |
| 未知值处理 | — | **缺陷**：qoder 模型 `context_size:0`（curl 实测）会显示"0"而非"未知"（ModelsScreen.tsx:202-204 只判 null） | 部分 | P0-：0/负值视为未知（纯前端 XS） |

## 5. view-packages（积分构成）

| 功能点 | wb2api 行为 | 我们现状 | 状态 | 批次建议 |
| --- | --- | --- | --- | --- |
| 积分到期分布 | `GET /panel/api/packages`+FEFO 分布图+排序（按到期/面额）+折叠分组+localStorage 记忆（app.js:2521-2777） | 无此屏 | 明确不做（本批） | P2（story-map ④ 已裁定 P2）：需 per-account 积分批次数据，我们的余额面只有单值 credit。等后端批次数据面立项 |
| 账号对比 | pkSummary/pkDetail 表 | 无（总览页平台摘要卡承载了最简对比） | 部分 | P2 同上 |

## 6. view-usage（用量）

| 功能点 | wb2api 行为 | 我们现状 | 状态 | 批次建议 |
| --- | --- | --- | --- | --- |
| KPI 六卡 | 请求数/总 token/prompt/completion/失败/平均延迟（renderUsage，app.js:2003-2058） | 无；总览页"请求"行如实显示"统计维度待接入"（OverviewScreen.tsx:111；zh.ts:231） | 缺失 | P1（story-map ④：用量统计=P1）：后端 per-request 记账+`/api/usage` 端点（M）+前端 KPI（S） |
| Token 时序堆叠图 | renderUsageChart（app.js:2115）+时间窗切换 | 无 | 缺失 | P1 后半（随记账面；不做小时级假图表——story-map §6 裁定上游无历史接口不做时序假数据，窗口统计需自记账） |
| 明细维度表 | 按账号/模型/域 三 tab+排序（usDimTabs/usSort） | 无 | 缺失 | P1（同上） |
| 积分扣除历史 | usCreditTabs（按账号/模型） | 无 | 明确不做 | 需上游扣分流水协议（未实证）；P2 |
| 用量持久化 | `POST /panel/api/usage/save`（panel.go:185） | 无 | 不做（暂） | 随 P1 记账面一并设计落盘 |

## 7. view-config（配置）

| 功能点 | wb2api 行为 | 我们现状 | 状态 | 批次建议 |
| --- | --- | --- | --- | --- |
| 配置分组 | 5 组 ~50 字段手写表单：服务/定时任务/账号池与流量治理/上游与高级/日志（HTML label 区全量提取） | fields 元数据驱动：gateway(3)/routing(1)/pooling(动态)/schedule(3)/sources_throttle.*/danger.*（configfields.go:162-203；~11 个显式 Key） | 已有（架构不同：我们元数据驱动+保存流+field_errors+restart_required，wb2api 是整页 POST） | 维持 |
| API 密钥"显示"眼睛按钮 | btnEye 切换明文 | 无（password 框固定） | 部分 | P1 小 UX（纯前端 XS） |
| 放弃修改/保存双键 | btnCfgReload/btnCfgSave | 保存+放弃双键，dirty 激活（SettingsScreen.tsx:329-353） | 已有 | — |
| 流量治理参数（熔断/退避/降权/闲置补偿/成本探索/积分保底/快过期窗口） | config 页 12+ 字段 | pooling 组有数值字段（poolPointers 映射）；网关侧语义与 wb2api 不一一对应 | 部分 | 逐项对齐需产品裁定：**不做整表复制**（两网关调度语义不同，抄字段=假开关）；按我方 pool 实际消费的键暴露 |
| 超时三元组（短请求/首字节/流空闲）+出站 UA | config 页，需重启 | 无对应键 | 明确不做（暂） | 我方网关无该实现面；等上游协议/网关能力实证，不发明配置项 |
| 系统提示词模式（custom/append/passthrough 下拉） | config 页 | 无 | 明确不做 | 我方网关无提示词注入面，协议未消歧 |
| 会话粘性 TTL | config 页 | feature_flags.session_stickiness 已有开关（panelApi.ts:161），无 TTL 数值 | 部分 | P2（粘性已实现，TTL 调节待网关暴露） |
| 日志组配置 | config 页"日志"组 | 无 | 不做（暂） | 环形缓冲固定 500 行（矩阵 §4），无运行时可调面 |

## 8. view-logs（运行日志+请求记录）

| 功能点 | wb2api 行为 | 我们现状 | 状态 | 批次建议 |
| --- | --- | --- | --- | --- |
| 运行日志列表 | 级别 chips：全部/任务/对话/系统 + 自动滚动开关（btnLogPin） | 全部/info/error radio + 自动滚动 + 刷新 + 10s 轮询（LogsScreen.tsx:53-161） | 已有（分类维度不同） | P1：来源分类 chips 随日志面增强评估 |
| 请求记录表 | 10 列：时间/结果/模型/账号/来源 IP/UA/耗时/Token/积分/请求 ID + 搜索 + 结果筛选（成功/HTTP 错误/流错误/中断）+ 100/300/1000 条 + 汇总 chips（`request_logs`+`request_metrics`，panel.go:154-155） | **无**（/api/logs 只有运行日志 ring；trail.go 仅 consent 留痕，无请求级记录） | 缺失 | **P1（盲区 top1）**：后端 reqlog 面（M-L）+前端表（M）。这是"503 为什么"故事（story-map ⑤-2）的最强支撑，也是 wb2api 最常用的排障面 |
| 积分任务弹层 | 全部接受/一键完成/重新查询（growth 面） | 无 | 明确不做 | 需协议消歧（同 §3） |
| 券码弹层 | `school/vouchers` | 无 | 明确不做 | 活动向（同 §3） |
| 日志平台过滤 | —（wb2api 单平台无此需求） | 矩阵 §4 承诺 ✅，但 wire `{ts,level,line}` 无 platform 字段（curl 实测；poolApi.ts:136-151 也未解析）——**规格与 wire 脱节** | 部分 | P1：ring 条目加 platform 标注（后端 S）+前端下拉（S） |

## 9. 盲区清单（wb2api 有、我们连入口都没有）

按运维价值排序：

1. **请求记录整面**（10 列请求表+结果筛选+汇总）——排障核心面，我们只有运行日志。
2. **用量统计整面**（KPI/时序/按账号模型域）——story-map 排 P1 但完全未开工；总览页该行只能写"待接入"。
3. **账号行内操作整列**——wb2api 每行 6 键；我们连**已裁定 P0 的 recover/删除**都没有 UI（recover 后端已就绪却无人调用）。
4. **积分构成整面**（到期分布/账号对比/FEFO 横幅）——裁定 P2，登记在案。
5. **模型锁池表**——模型级限流可见性。
6. **冷却原因副标+类型细分**（熔断/降权/限流/积分冷却）——我们有冷却时间戳但看不出"为什么冷却"。
7. **growth 任务队列/券码**——需协议消歧，不做假状态。
8. **手动批量保活按钮组**——裁定不做（调度器自动）。

## 10. 我们的定位偏差（对照主人裁定，需要纠正或再裁定）

1. **空态文案仍教用户"放文件+重启"**（zh.ts:259）——直接违背 v1.1"文件即账号废止"裁定，且空态无添加按钮（C1 违背）。最刺眼的偏差。
2. **trae 添加残表单**：渲染不出输入框的假表单且提交误路由 workbuddy 端点（AccountsScreen.tsx:276）——违背矩阵 §1a"不渲染假表单"。
3. **规格已裁定的 P0 行内操作（recover/删除/暂停选号）未落 UI**——矩阵 §1 与 gap 册 §5.1 写了，代码没接。规格与实现脱节。
4. **连接屏消失**：story-map §2/⑤ 把连接屏列为 P0（"Base URL/key/模型清单+复制"，§7 明言"集成批补建真屏"），M5b 删除空壳后未重建（zh.ts:178"connect 空状态节退役"），现导航无连接入口。规格内部矛盾——**需主人裁定**：要么 P0 补真屏（纯前端 S：读 /api/config+/api/models 即可），要么正式从 story-map 移除。
5. **日志平台过滤承诺了但 wire 无 platform**（矩阵 §4 vs poolApi.ts:136）——规格超前于契约。
6. **总览页启用状态用"是否出现账号"演示口径**（OverviewScreen.tsx:20 注释自认）——C1b"未启用平台引导"未实现。
7. **模型 context_size=0 显示为"0"**而非未知——诚实性小破口。

## 11. 本批实现顺序建议

**第一波：纯前端 P0（合计约 1 天，S×4）**
| # | 事项 | 工作量 | 证据 |
| --- | --- | --- | --- |
| 1 | 空态修文案+补添加按钮（C1）+trae/qoder 分平台文案（C1b） | S | zh.ts:259；AccountsScreen.tsx:176-186 |
| 2 | trae 添加向导改"未接入"引导，删除误路由 | S | AccountsScreen.tsx:265-341 |
| 3 | 行内 recover 按钮（后端端点已存在，纯前端接线）+DangerAction 确认 | S | api.go:29-31 |
| 4 | 冷却倒计时（用 /api/time 对齐 now）+模型 0 值未知 | XS | ModelsScreen.tsx:202 |

**第二波：需后端小增补的 P0（合计约 2-3 天）**
| # | 事项 | 工作量 | 说明 |
| --- | --- | --- | --- |
| 5 | accounts wire 增补 success/fail/in_flight/last_success/credits_total + 前端双值条与新列 | M | gap 册 §5.1 已裁定；"本次运行"口径 |
| 6 | 删除账号端点+UI（C15 唯一账号确认；重复 uid 覆盖确认 C14 顺手） | M | pool.RemoveAccount 已有池内能力（pool.go:88-89），缺 HTTP 面+凭证文件删除 |
| 7 | 冷却原因/类型上 wire（reason/cool_kind 副标） | S-M | 与 5 同一 PR 顺手 |
| 8 | pause/resume 选号开关（后端 paused 态+端点+前端开关） | M | gap 册 §1 裁定 P0；pool.Account 无此态需加 |

**第三波：P1（下批，按价值排序）**
| # | 事项 | 工作量 | 说明 |
| --- | --- | --- | --- |
| 9 | 请求记录面（后端 reqlog+前端表） | L | 盲区 top1，"为什么 503"的最短路径 |
| 10 | 用量记账+KPI 卡 | M-L | 先 KPI 后时序图 |
| 11 | 日志 platform 标注+平台过滤 | S+S | 兑现矩阵 §4 |
| 12 | workbuddy 设备授权登录流 | M | 协议已实证（story-map §4） |
| 13 | 连接屏（若主人裁定保留） | S | 纯前端 |
| 14 | 模型搜索/排序、任务/模型低频轮询、密钥显示按钮 | S×3 | 体验增强 |

**不做清单（含理由，防回归）**
- 批量签到/旅行/保活/活跃按钮：调度器自动跑（story-map §6 裁定）。
- growth 任务队列、券码：上游协议未消歧（gap 册 §2），做=假状态。
- 积分扣除历史/积分到期分布：上游无流水/批次接口实证，P2 等数据面。
- 手动禁用账号：与我们"disabled=凭证死亡"语义冲突，暂停选号已覆盖诉求。
- localStorage 持久化（主题/key/排序偏好）：M1 红线。
- 上游实时模型目录"重新获取"：协议未在我方网关实证，P2。
- wb2api 配置页的超时三元组/出站 UA/系统提示词模式：我方网关无对应实现，不发明配置项。

---

### 附：主要证据索引

- wb2api 路由总表：`D:\CODE\workbuddy2api-panel\internal\panel\panel.go:150-188`（38 端点）
- wb2api 行内按钮/状态细分/积分双值：同目录 `app.js` renderAccounts（约 :368 起）与动作分发（:527-547）
- wb2api 轮询/确认/localStorage：app.js:4,36,522-523,1138,1220,1668,1751
- all2api 后端现状：`internal/api/api.go:23-31`（全部 panel 路由）；`internal/api/panel_data.go:60-101`（accounts wire 无 success/fail/in_flight/last_success/credits_total）；`internal/api/accounts_add.go:21-44`（添加账号）；`internal/pool/pool.go:88-89`（池内 Remove，无 paused）
- all2api 前端现状：`web-panel/src/app/`（AccountsScreen/OverviewScreen/TasksScreen/ModelsScreen/SettingsScreen/LogsScreen/PanelApp）、`web-panel/src/lib/poolApi.ts`、`web-panel/src/i18n/zh.ts:231,259`
- 实测：`http://localhost:7800/api/{time,accounts,logs,tasks,models,config,config/fields}` 全 200；`/api/accounts` 无本报告所指字段；`/api/tasks` last_run 恒 null；qoder 模型 context_size=0
- 勘误：`D:\CODE\vhub\panel.go|panel_page.go` 属 vhub 项目，非 wb2api 源码（panel_page.go:4 标题"vhub 控制台"）
