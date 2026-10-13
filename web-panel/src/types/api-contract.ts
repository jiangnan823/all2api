/**
 * [仓内镜像 v0.5.1 · 2026-10-13 正式冻结令（seq=405）]
 * 本文件是冻结契约的逐字镜像，类型与 JSON tag 映射不得在本仓另立第二名；
 * 键名权威 = 仓内 config.example.json（af208a8）+ fronted contract-json-tags-v0.5.md §1。
 * 冻结后改动需走 G 增量单 + 测试增量审 + PM 复核。以下为镜像原文。
 *
 * all2api Web 面板 API 契约草案 v0.5.1（fronted）
 * 基准：《接口契约增补 v2》（seq=197/200 终审）+ UI UE props 契约表（shared/all2api-panel-tokens-props-v0.2.md）
 *       + 项目经理裁定 seq=340（G1/G2/G3 入冻结清单）、seq=349（G4 定名）、seq=360（O-2/O-5/O-6）、
 *       seq=376（裁定一：键名权威=仓内 af208a8；裁定二：F-V1 源级定形）。
 * 本文件与 backend models.go 为同一契约的两侧表达；类型名是 UI props 表的唯一引用源。
 *
 * v0.5 变更（全部来自 seq=333 缺口报告 → seq=340/349 裁定；无一处视觉/文案改动）：
 * - G1【冻结硬门】频率预算单位收敛为单一口径 `_rpm`（整数·每分钟），**废弃 rps 语义**：
 *   删除 `PanelConfig.rateBudgets:{requestsPerMin}` 与 backend `max_rps`/`burst_size` 的双单位并存。
 *   原写法下把面板值 6 写进 `max_rps` = 360 rpm = 拍板值的 60 倍，属安全事故级。
 * - G1 节流落点合一：只保留 `sources.<id>.throttle.*`，废弃顶层 `Throttle` map（双落点无优先级即事故温床）。
 * - G2 八组节流字段定名（实现排 M1，键名/单位/JSON path 本次冻结）：新增 `ThrottleBudget`。
 * - G3 知情同意服务端面（M1 验收第 6 条双端校验的可实现前提）：新增 `ConsentAck`；
 *   `POST /api/auth/verify` 请求体追加唯一顶层键 `consent`；`PanelConfig` 下发 `noticeVersion`；
 *   拒绝复用逐字段 `fieldErrors`，**不扩 FailureReason 枚举**（保护 UI §5.18 REASON_COPY 展示词表）。
 * - G4 指纹改写开关定名 `request_fingerprint_rewrite`（seq=349），默认 false。
 * - v0.5.1 F-V1（测试 seq=363 提出 + PM seq=360 O-6 裁定）：该开关由**全局 boolean 改为逐源**
 *   `PanelConfig.advanced: Record<source, SourceAdvanced>`，JSON path `sources.<id>.advanced.request_fingerprint_rewrite`，
 *   与节流同属源属性、不开第二处落点；键缺失一律按 false 渲染（安全默认）。`featureFlags` 恢复为纯全局语义。
 * - `FieldError` 提为具名类型，`ConfigSaveResponse` 与 `AuthVerifyResponse` 共用同一结构。
 * - 附：Base URL 运行时注入约定见 `contract-json-tags-v0.5.md`（G5，embed 声明已在 af208a8 落仓）。
 *
 * v0.5.1 变更（裁定一／二，均为对齐仓内 af208a8，无新增名）：
 * - 裁定二 F-V1 定形：指纹开关为**源级** `PanelConfig.advanced: Record<source, SourceAdvanced>`
 *   → JSON path `sources.<id>.advanced.request_fingerprint_rewrite`；键缺失一律按 `false` 渲染（安全默认）；
 *   `featureFlags` 恢复纯全局语义，全仓单点、无第三落点。
 * - 裁定一：backoff 内层 JSON 串按仓内逐字采 `base_429_sec` / `not_found_sec` / `exhaust_402_until`
 *   （TS 属性名不变，映射见 tags 表 §1）；`exhaust_402_until` 明确为**只读锚点、不进可编辑集**。
 * - UI v0.2.3 的 6 个节流键名不成立，改名清单见 tags 表 §5 第 4 条。
 *
 * v0.4 变更（对齐 UI v0.2 props 表）：
 * - 补齐 UI §5/§5.12/§5.13 引用但本文件缺失的类型：RequestLogRow、RequestMeta、FieldDef、ConfigPatch、ServerTimeResponse
 * - LogRow 更名 RequestLogRow（UI 引用名），OverviewResponse 引用同步
 * - 新增 WizardMethod（UI 层）与 AuthMode（adapter 能力层）显式映射，两套枚举不再混用
 * - 新增 DevicePollState 作为 AuthPollResponse.status 的 UI 投影（含 UI 缺的 failed 分支）
 * - F4：CredentialStatus.daysLeftCache 显式声明为"仅展示缓存"，UI §5.8 daysLeftCache 有类型来源
 * - §5.7/§5.8 组件需要 now：新增 GET /api/time 契约（服务端时钟基准，禁用浏览器时钟）
 *
 * v0.3 遗留（仍有效）：F1–F4、C2 去 bool、Q9 TimeLocal、Q10 urgencyRank、C1/C3/C4 backlog
 */

// ---------- 展示原语（闭集，前端只对这几个值编码） ----------

export type DisplayPrimitive =
  | 'calendar_streak'   // 需要 state.calendar，nil → 回退 status_plain
  | 'tier_progress'     // 需要 state.progress，nil → 回退 status_plain
  | 'quota_countdown'   // 需要 state.quota，nil → 回退 status_plain
  | 'validity_gauge'    // 需要 state.validity，nil → 回退 status_plain
  | 'status_plain';     // 仅需 lastRun/running/nextRunAt；未知 Display 值同样回退此项 + console 告警

// ---------- canonical 失败原因枚举（F3：adapter ∪ circuit 合并，单一词表） ----------

export type FailureReason =
  | 'ok'
  | 'auth_expired'        // 人工处置：主操作=重新授权；不计入熔断展示
  | 'platform_down'
  | 'rate_limited'        // 文案钉死：「触发限流·已降频」+ 分钟级倒计时
  | 'not_found'           // 文案钉死：「接口或模型不存在」→ 点击跳模型视图
  | 'quota_exhausted'     // 文案钉死：「额度耗尽·次日04:00重置」；严禁渲染成"冷却中"
  | 'protocol_mismatch'   // 人工处置：需发版/热修；不计入熔断展示
  | 'timeout'
  | 'disabled'            // 人工处置（源级停用小灯）；不进源级熔断展示
  | 'consecutive'         // 连续失败熔断
  | 'unknown';            // 未识别新值兜底渲染，前端不崩

// ---------- 时间双表示（Q9） ----------

export interface TimeLocal {
  unix: number;          // Unix 秒（绝对时刻，排序/倒计时用）
  local: string;         // 服务器时区展示串；日历日用 YYYY-MM-DD（服务器本地"某一天"边界）
}

// ---------- 凭证方式：adapter 能力层枚举 vs UI 向导枚举（两套不混用，显式映射） ----------

export type AuthMode = 'device_code' | 'paste_token' | 'script_import';   // adapter.Capabilities 上报

export type WizardMethod = 'device_flow' | 'paste' | 'auto';              // UI 向导入参（UI §5.11）

export const AUTH_MODE_TO_WIZARD_METHOD: Record<AuthMode, WizardMethod> = {
  device_code: 'device_flow',
  paste_token: 'paste',
  script_import: 'auto',
};

// ---------- 服务端时钟基准（UI §5.7/§5.8 组件需要 now；禁用浏览器时钟） ----------

export interface ServerTimeResponse {
  now: number;             // Unix 秒
  local: string;           // 服务器本地展示串
  timezone: string;        // 如 Asia/Shanghai；面板显示"服务器时区"角注
}

// ---------- 能力（adapter.Capabilities → CapSet 三态，C2：布尔能力位全面废弃） ----------

export type TriState = 'yes' | 'partial' | 'no';

export interface CapSet {
  chat: TriState;
  responses: TriState;
  streaming: TriState;
  toolChain: TriState;
  structured: TriState;
  reasoning: TriState;
  degradationNote?: string;   // 'partial' 必须写明降级代价
}

export interface SourceCapabilities {
  capSet: CapSet;             // 唯一能力数据源；backend 基类默认零值，禁止桩复用全 bit 谎报
  authModes: AuthMode[];
  taskTypes: string[];
}

// ---------- 来源 / 平台 ----------

export interface SourceInfo {
  id: string;              // 'workbuddy' | 'qoder' | 'trae' | ...
  displayName: string;
  capabilities: SourceCapabilities;
  disabled: boolean;       // 源级降级开关（面板一键停源）
  health: FailureReason;
  healthDetail?: string;
}

// ---------- 账号 / 凭证（F4） ----------

export interface CredentialStatus {
  supported: boolean;      // 唯一判别位：false = 该源不支持检测有效期 → 中性空态"有效期由平台管理"
  valid: boolean;
  expiresAt?: TimeLocal | null; // 仅 supported=true 有意义；剩余天数由前端用 unix 自算
  daysLeftCache?: number;  // F4：后端可下发的展示缓存；绝不参与判定，与计算值不一致时以计算值为准
  // 已过期 = supported && expiresAt && expiresAt.unix < now（now 取 /api/time，不用浏览器时钟）
  // 严禁把 supported=false 渲染成红色"已过期"；严禁用 daysLeftCache 正负/-1 判状态。
}

export interface Account {
  id: string;
  source: string;
  label: string;
  quota: QuotaInfo;
  credential: CredentialStatus;
  circuitState: 'closed' | 'open' | 'cooldown';
  cooldownUntil?: TimeLocal | null;
  addedAt: TimeLocal;
  urgencyRank: number;     // Q10：后端计算（失效>将到期>冷却>可用），前端不复制业务排序规则
}

// C1 backlog：面板只消费 Account.credential 这一处有效期；HealthStatus 若另有 ExpiresAt，前端忽略。

export interface QuotaInfo {
  kind: 'credit' | 'tokens' | 'requests';  // canonical 归一类型
  used: number;
  total?: number;
  unit: string;
  floor?: number;
}

// ---------- 任务（F1/F2：schema 静态缓存 / state SSE 运行时，分两端点） ----------

export interface TaskSchema {
  id: string;              // F2：强制源命名空间 'workbuddy.signin_09'，跨源 join/去重键
  source: string;
  name: string;
  type: string;            // 业务语义：仅用于筛选/日志/追溯，不参与组件选择
  display: DisplayPrimitive;
  description?: string;
  cron: string;
  timezone: string;        // 面板按浏览器本地时区展示 + 服务器时区标注
  maxDailyRun?: number;
  // C3 backlog：卡片启用开关的读取源待定稿（schema.enabled vs config 字段），定稿前前端按 config 渲染
  capabilities: {
    runNow: boolean;       // false → 立即执行按钮禁用并给出原因
    history: boolean;
    configurable: boolean;
  };
}

export interface TaskState {
  taskId: string;          // 与 schema.id 全等（含源前缀）
  lastRun?: {
    at: TimeLocal;
    status: 'success' | 'failed' | 'skipped';  // skipped ≠ failed：中性色
    reasonCode?: FailureReason;
    skipReason?: string;   // UI §5.4：skipped 时的人话原因（冷却/credit_floor/开关关闭）
    message?: string;
  };
  nextRunAt?: TimeLocal | null;
  running?: boolean;
  // ---- F1：四原语数据字段（与已冻结方法签名无冲突；Go 侧为指针） ----
  calendar?: {
    windowDays: number;
    days: Array<{ date: string; status: string; award?: number }>; // date=服务器本地 YYYY-MM-DD（Q9）
  } | null;
  progress?: { current: number; target: number; unit: string } | null;
  quota?: { used: number; max?: number; resetAt?: TimeLocal } | null;
  validity?: { expiresAt: TimeLocal; warningDays: number; hardDeadline?: TimeLocal } | null;
  // 回退契约（与 UI UE props 表同步定稿）：schema.display 声明的原语所需字段为 nil
  // → 前端回退渲染 status_plain + console 告警；不画空组件、不崩。
}

// ---------- 模型（C2：无 bool 能力位） ----------

export interface ModelInfo {
  qualifiedName: string;   // 'workbuddy/deepseek-v4-flash'（model 命名空间路由）
  source: string;
  bareName?: string;       // 全局唯一才暴露裸名（O3 裁决）
  contextLength: number;
  caps: CapSet;            // 三态能力矩阵唯一数据源
  // C4 backlog：tier/credit_cost 是否进 M1 由 PM 在 PRD 标范围；进则此处加字段，前端档位筛列同步
}

// ---------- 面板 API 端点（/api 前缀，go:embed 同端口；对齐 UI UE §2.1） ----------

export interface SourceOverview {
  source: SourceInfo;
  accountCount: number;
  availableAccounts: number;
  quotaSummary: string;
  circuitOpenAccounts: number;
  requestsToday: number;
  taskHealth: { idle: number; running: number; success: number; failed: number; skipped: number };
}

export interface OverviewResponse {
  sources: SourceOverview[];
  recentFailures: RequestLogRow[];    // top 5
  alerts: PanelAlert[];
}

export interface PanelAlert {
  id: string;
  level: 'warn' | 'danger';
  message: string;
  actionLabel?: string;
  actionHref?: string;
}

export interface LogsQuery {
  from?: number;             // Unix 秒
  to?: number;
  source?: string;
  accountId?: string;
  status?: 'failed' | 'all';
  model?: string;
  ip?: string;
  page?: number;
  pageSize?: number;
}

// UI §5.12 引用名 RequestLogRow（v0.3 前叫 LogRow，v0.4 起统一为 UI 引用名）
export interface RequestLogRow {
  ts: TimeLocal;
  source: string;
  accountId: string;
  model: string;               // qualifiedName
  endpoint: 'chat.completions' | 'responses';
  status: number;
  ttfbMs: number | null;
  tokensPerSec: number | null;
  creditCost?: number;         // 额度消耗列（UI §5.12 要求）
  callerIp?: string;
  callerUa?: string;
  uid?: string;
  reasonCode?: FailureReason;  // 失败行行内直显文案，不靠用户猜（UI §5.12）
  requestId: string;
}

// 详情抽屉数据：默认仅元数据，请求体需 debugBodySampling 显式开启
export interface RequestMeta {
  requestId: string;
  row: RequestLogRow;
  attempts?: Array<{ account: string; reasonCode: FailureReason; at: TimeLocal }>; // 重试/回退轨迹
  bodySample?: string;         // 仅 debugBodySampling=true 时下发；抽屉须给黄色敏感提示
}

// ---------- 认证流程（AddAccountWizard 依赖；文案绑 PM 术语表，界面词=「接入你的免费额度账号」） ----------

export interface AuthBeginResponse {
  mode: 'device_code';
  userCode: string;
  verificationUrl: string;
  qrPayload: string;           // 二维码必须提供（NAS 手机扫码场景）
  intervalSec: number;
  expiresAt: TimeLocal;
  flowId: string;
}

export interface AuthPollResponse {
  status: 'pending' | 'authorized' | 'expired' | 'denied' | 'failed' | 'slow_down';
  accountId?: string;
  error?: string;              // denied → "你取消了授权"；expired → "代码已过期，请重新获取"
}

// DeviceCodePanel.pollState 是 AuthPollResponse.status 的 UI 投影（UI §5.11 缺 failed，v0.4 补）
export type DevicePollState =
  | 'pending' | 'polling' | 'slow_down' | 'expired' | 'denied' | 'ok' | 'failed';

export const POLL_STATUS_TO_DEVICE_STATE: Record<AuthPollResponse['status'], DevicePollState> = {
  pending: 'pending',
  authorized: 'ok',
  expired: 'expired',
  denied: 'denied',
  failed: 'failed',
  slow_down: 'slow_down',
};

// ---------- 知情同意门（Q11 · M1 验收第 6 条的双端校验面；UI §5.19 ConsentGate 的服务端对应） ----------

/**
 * 留痕三要素，与 UI §5.19「留痕」行逐字对齐：只记 source + 须知版本 + 时间戳。
 * 绝不携带账号标识、凭证内容或须知正文；面板不上传除这三项以外的任何数据。
 */
export interface ConsentAck {
  source: string;             // JSON: source
  noticeVersion: string;      // JSON: notice_version；版本比对由后端持有，前端只透传
  ackAt: TimeLocal;           // JSON: ack_at —— ★ 由服务端落戳（建议项 O-5，见 tags 表）：
                              // 请求体只携带 source + notice_version；时间戳以服务端接收时刻为准，
                              // 避免客户端时钟偏差污染留痕。前端 onAck 的 ackAt 仅作即时回显/本地兜底。
}

// POST /api/auth/verify 请求体本次冻结仅追加一个顶层键：consent（内含 source + notice_version）。
// 该源要求知情同意（Qoder 恒为要求）而 consent 缺失、或 noticeVersion 落后于
// 后端当前值 → 后端拒绝保存该源凭证，并在 AuthVerifyResponse.fieldErrors 给出落点。
// 前端禁用「保存」按钮只是必要非充分：直接调 API 绕过向导同样被服务端拒绝（PRD §6⑥）。

export interface AuthVerifyResponse {
  ok: boolean;
  accountLabel?: string;       // paste 校验回显"读到了什么"
  quota?: QuotaInfo;
  credential?: CredentialStatus;
  reasonCode?: FailureReason;
  fieldErrors?: FieldError[];  // G3：consent 类拒绝走此通道，不占用 reasonCode 展示词表
}

// ---------- 节流预算（G1/G2 · Q12；UI §5.20 ThrottleForm 的线上数据源） ----------

/**
 * 单位纪律（seq=340 冻结硬门）：本文件内所有频率字段一律为 **每分钟整数（`_rpm`）**。
 * rps/每秒语义字段（backend 原 `max_rps`、`burst_size`）自 v0.5 起废弃，全仓不得再有每秒语义节流字段；
 * 稳态与突发用同一单位，是为了让面板的按源同列比较（§5.20「一张按源表」）不会因单位不同而失真。
 */
export interface ThrottleBackoff {
  base429Sec: number;                  // JSON: backoff.base_429_sec，429 指数退避基数（PRD §7.1 定 ≥600）
  notFoundSec: number;                 // JSON: backoff.not_found_sec，见 tags 表待 backend 确认
  exhaust402Until: 'next-04:00';       // JSON: backoff.exhaust_402_until，服务器本地次日 04:00 重置锚点
}

export interface ThrottleBudget {
  steadyRpm: number;                   // JSON: steady_rpm  ★ 唯一稳态口径
  burstRpm: number;                    // JSON: burst_rpm
  minGapMs: [number, number];          // JSON: min_gap_ms [lo,hi]，随机间隔闭区间（消除机器节拍）
  dailyCap: number;                    // JSON: daily_cap
  inflight: number;                    // JSON: inflight，同一时刻在途上限
  backoff: ThrottleBackoff;            // JSON: backoff{...}
  jitterMinutes?: number;              // JSON: jitter_minutes，签到抖动，缺省 15
  halveBudgetAfter429?: boolean;       // JSON: halve_budget_after_429，Qoder 特有当日预算减半
}

/**
 * 源级高级选项（F-V1 · seq=360 O-6 裁定）：落点跟随 G1 的源属性唯一路径
 * `sources.<id>.advanced.*`，**不开第二处**——全局布尔 + 源级覆盖的双落点正是 v0.4 单位事故的同类结构，
 * 且改写请求设备特征的风险本就逐源独立（每平台条款不同），全局单值既不真实也不安全。
 */
export interface SourceAdvanced {
  fingerprintRewrite: boolean;   // JSON: request_fingerprint_rewrite
                                 // ★ 逐源默认 false：改写请求设备特征会把"灰色使用"升格为"风控规避"（PM 调研 §4.1）
                                 // UI 措辞「请求特征改写」；开启必须走 DangerAction 二次确认，弹窗默认焦点「保持关闭」
                                 // 回退：`advanced[source]` 缺失或该键缺失 → 一律按 false 渲染（安全默认，见 UI §7 回退矩阵）
}

// ---------- 配置 ----------

export interface PanelConfig {
  raw: Record<string, unknown>;      // config.json 热更新透传
  featureFlags: {
    responsesApi: boolean;
    sessionStickiness: boolean;
    redisMirror: boolean;
    debugBodySampling: boolean;      // 开启后日志抽屉含请求体，页面须给黄色敏感提示
                                     // F-V1：指纹改写已移出本对象——它是源级属性，不是全局开关
  };
  /**
   * 按源节流预算。落点唯一（G1）：JSON path = `sources.<id>.throttle.*`，
   * 顶层 `Throttle` map 废弃——同一旋钮两处可写且无优先级声明正是 60 倍事故的来源。
   * 缺省值来源 PRD §7.1 / PM 调研 §4：WorkBuddy 6/20·日600·0.1–0.9s·在途2；
   * Qoder 4/12·日300·0.2–1.2s·在途1·429后减半；Trae 2/6·日100·0.5–2.0s·在途1；抖动 ±15min。
   * 键名与单位本次冻结，字段实现与 FieldDef 元数据下发列 M1（前端不硬编码表单，UI §5.13）。
   */
  throttle: Record<string, ThrottleBudget>;   // key = source id（'workbuddy' | 'qoder' | 'trae'）
  advanced: Record<string, SourceAdvanced>;   // 同上 key；JSON path = sources.<id>.advanced.*
  noticeVersion: string;               // G3：JSON: notice_version，随 /api/config 下发
                                       // ConsentGate 用它判定"须知是否已变更需重确认"；比对在后端，前端只透传
}

// ---------- 配置表单（UI §5.13 ConfigGroupForm 依赖；后端下发字段元数据，前端不硬编码表单） ----------

export type FieldEffect = 'immediate' | 'next_request' | 'restart_required';  // UI：生效范围标注

export interface FieldDef {
  key: string;                 // JSON path，如 'pool.credit_floor'
  label: string;               // 文案层 key（不内联中文，UI §5.16）
  type: 'number' | 'string' | 'boolean' | 'cron' | 'select';
  unit?: string;
  min?: number;
  max?: number;
  options?: string[];          // type=select
  description?: string;        // 一行说明 + 建议值（UI：运维旋钮误配代价高）
  effect: FieldEffect;
  requiresConfirm?: boolean;   // 危险区字段
}

export interface ConfigGroup {
  group: string;               // 网关/路由/来源与节流/池化/熔断/凭证/定时任务/危险区
  fields: FieldDef[];
}

export type ConfigPatch = Array<{ key: string; value: unknown }>;

/**
 * 逐字段错误（v0.5 提为具名类型，`ConfigSaveResponse` 与 `AuthVerifyResponse` 共用）。
 * `code` 为机器可读稳定标识，供前端区分处置动作而**不必匹配中文 message**；
 * message 仍逐字红字直显（不是一句"保存失败"）。
 */
export interface FieldError {
  key: string;                              // JSON path，如 'sources.qoder.throttle.steady_rpm'
  message: string;                          // 红字文案 key，UI §5.16 不内联中文
  code?: 'range'                            // 数值越界（§5.20 每列带范围）
       | 'consent_missing'                  // 未勾选知情同意即调 API → 拒绝保存凭证
       | 'consent_stale';                   // noticeVersion 落后 → 须知已更新，须重新确认
}

export interface ConfigSaveResponse {
  ok: boolean;
  applied?: PanelConfig;                    // saved：前端以服务端回读为准，不信本地乐观值
  fieldErrors?: FieldError[];               // rejected：逐字段红字，不是一句"保存失败"
}

// ---------- SSE 事件（/api/stream/tasks、/api/stream/logs；轮询为降级） ----------

export type PanelStreamEvent =
  | { type: 'task.state'; state: TaskState }
  | { type: 'task.run.result'; taskId: string; result: TaskState['lastRun'] }
  | { type: 'log.new'; row: RequestLogRow }
  | { type: 'source.health'; sourceId: string; health: FailureReason }
  | { type: 'overview.tick'; overview: OverviewResponse };
