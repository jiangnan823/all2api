/**
 * 面板冻结文案层（fronted）。组件内不内联中文（UI UE §5.16 / §5.19）。
 * 逐字基准 = PM《PM-credential-wizard-copy》v0.1.3 §9（notice_version = "1"，v0.1.3 属定稿补全非正文修订，PM 已裁）。
 * 唯一加工：markdown 强调记号 ** 不入库，字符本身逐字未改；勾选标签的「{来源}」为占位符 {source}。
 * 本节任一处改动必须同步 all2api-panel-hifi-v0.2.html#consent 并递增 noticeVersion（PM 复核）。
 */

export const CONSENT = {
  TOP_NOTE:
    '此步骤每次接入一个账号出现一次（一次/账号）。滚动到文末并勾选后才能继续；不勾选也可以直接放弃接入。',

  GENERAL_TITLE: '在接入之前，请了解',
  GENERAL_ITEMS: [
    'all2api 把你本人账号的免费额度接入你自己的 AI 客户端，属于个人自托管用途。',
    '各平台对账号使用有自己的规则。使用频率过高、或多账号集中操作，可能被平台限流或处置。',
    '网关会自动控制请求节奏并保护你的账号安全，但平台规则由平台解释，账号风险最终由账号持有人承担。',
    '建议：只用自己日常也在用的账号；不要批量注册；出现「额度耗尽」或「触发限流」提示时，降低使用强度。',
  ],

  CLOSING_TITLE: '网关不会做的事',
  CLOSING_ITEMS: [
    '不改写请求设备特征。指纹类高级选项默认关闭，开启需你在设置页二次确认——这类改写可能被平台判定为规避使用控制。',
    '不支持批量注册、不绕开付费机制、不转售额度。',
  ],

  CHECK_LABEL: '我已阅读并理解上述风险，确认这是我本人的 {source} 账号，并自愿承担账号被处置的风险。',
  /** copy §9 v0.1.3（项目经理 seq=445 裁二）：留痕边界写实——网关落戳三要素、不含凭证/对话内容；
   *  「本机存储不上传」限定到凭证与对话内容。原句"仅保存在本机…不上传任何个人数据"与 G3/O-5 服务端留痕冲突，已消解（F-1/R-B 案）。 */
  CHECK_NOTE:
    '网关只记录你的确认留痕三项：来源、须知版本、确认时间（由网关落时钟），仅用于在平台争议时证明你同意过；不含你的凭证和对话内容。凭证与对话内容只存在本机，不上传；可在「设置 · 接入须知」查看历史确认。',

  REGION_LABEL: '接入须知正文，需滚动到文末',
  PROGRESS_ARIA: '阅读进度',
  VERSION: '须知版本 {noticeVersion}',
  PROGRESS: '阅读进度{percent}% · 需滚动到文末才能勾选',
  REACHED: '已读到底',
  STALE: '接入须知已更新，请重新阅读并勾选后再继续。',

  CONTINUE: '继续',
  ABORT: '放弃接入',
  ABORT_HINT: '放弃不需要任何前置条件，随时可以退出。',
} as const;

/**
 * 来源专用补充段。新增来源只在这里加一段，结构与样式不变（UI UE §5.15 / §5.19）。
 * qoder 三条为逐字基准（含条款出处 2.3 / 2.4 / 15.2 与防误读半句），禁止删弱。
 * workbuddy / trae 两段由 §9 的描述句转写为用户口吻，尚无逐字基准，待 PM 定稿后原样替换。
 */
export const CONSENT_APPENDIX: Record<string, { title: string; items: string[] }> = {
  qoder: {
    title: 'Qoder 账号需特别注意',
    items: [
      'Qoder 服务条款约定不得将账户凭证提供给第三方访问你的账户，且平台可在我们认为存在规避其使用控制的情形下限制、暂停或终止账户；账户一旦被终止将无法重新激活。（条款依据：服务条款 2.3 账户安全、2.4 处置情形、15.2 终止不可恢复）',
      '使用本接入方式意味着你自担该风险。建议仅使用你可承受失去的账号，并保持低频使用。',
      '本来源默认节奏已按保守档配置：稳态 4 次/分钟、日请求上限 300、请求间随机间隔 0.2–1.2 秒、同一时刻仅 1 个在途请求。保守频率仅降低触发风控的概率，不能消除上述条款风险。',
    ],
  },
  /** v0.1.2 起 PM copy §9 对 WorkBuddy / Trae 也给出逐字基准（PM seq=438 裁定二② 闭环）：
   *  全段原样消费、不得润色；唯一加工仍是去掉 markdown 加粗标记。 */
  workbuddy: {
    title: 'WorkBuddy 账号需特别注意',
    items: [
      '除了转发你的对话请求，网关还会代你执行例行操作：自动签到、活跃上报、凭证保活。这类操作等同于让网关代替你操作账号，平台若判定异常，可能限流或使凭证失效。',
      '出现「凭证已过期」提示时，通常是平台的处置表现，不是你设备或网关的故障；出路是回到账号页重新授权即可恢复。',
      '本来源默认节奏已按保守档配置：稳态 6 次/分钟、日请求上限 600、请求间随机间隔 0.1–0.9 秒、同一时刻最多 2 个在途请求。保守频率仅降低触发风控的概率，不能消除上述风险。',
    ],
  },
  trae: {
    title: 'Trae 账号需特别注意',
    items: [
      'Trae 的免费额度规则由平台不公开说明，平台可随时调整额度或可用模型。额度缩水、模型下线属平台行为，非网关故障；网关无法替你承诺额度长期不变。',
      '本来源默认节奏已按最保守档配置：稳态 2 次/分钟、日请求上限 100、请求间随机间隔 0.5–2 秒、同一时刻仅 1 个在途请求。保守频率仅降低触发风控的概率，不能消除上述风险。',
      '设备验证只是把你已授予的授权换取凭证保存在本机；随时可在「账号管理」删除账号并撤销留存凭证。',
    ],
  },
};

export const CONSENT_APPENDIX_FALLBACK_TITLE = '该来源需注意';

/** 来源显示名：仅用于勾选标签的 {来源} 占位替换，不新增文案。未知来源回退原始 id。 */
export const SOURCE_NAMES: Record<string, string> = {
  workbuddy: 'WorkBuddy',
  qoder: 'Qoder',
  trae: 'Trae',
};

export function sourceDisplayName(source: string): string {
  return SOURCE_NAMES[source] ?? source;
}

export function consentAppendix(source: string) {
  const hit = CONSENT_APPENDIX[source];
  if (hit) return hit;
  return { title: CONSENT_APPENDIX_FALLBACK_TITLE, items: [] };
}

export function consentCheckLabel(sourceName: string): string {
  return CONSENT.CHECK_LABEL.replace('{source}', sourceName);
}

/** 节流表单文案（UI UE §5.20）。速率单位只允许「次/分钟」，界面不出现 rps／"每秒"。 */
export const THROTTLE = {
  TITLE: '按源节流预算',
  SUBTITLE: '数值为产品建议默认值，可调整，非平台承诺。',
  FOOTER: '保守频率仅降低触发风控的概率，不能消除接入须知的条款风险。',
  SAVE: '保存并热生效',
  RESET: '还原',
  COL_FIELD: '字段',
  COL_VALUE: '值',
  COL_UNIT: '单位',
  COL_RANGE: '范围',
  COL_ERROR: '校验',
  GAP_LO: '下界',
  GAP_HI: '上界',
  HALVE: '429 后当日预算减半',
  READONLY: '只读',
  DASH: '—',
  UNIT_RPM: '次/分钟',
  UNIT_MS: '毫秒',
  UNIT_DAILY: '次/日',
  UNIT_INFLIGHT: '个',
  UNIT_SEC: '秒',
  UNIT_MIN: '分钟',
  EXHAUST_COLUMN: '触发限流后',
  EXHAUST_READ_ONLY_NOTE: '这一行由平台的重置时刻决定，面板只读，不可修改。',
  HALVE_ONLY_QODER_NOTE: '仅具备定时签到任务的来源显示。',
  ERR_RANGE: '{label}需在 {min}–{max} 之间',
  ERR_INT: '{label}必须是整数',
  ERR_GAP_ORDER: '间隔下界不得大于上界',
  ERR_BURST_LT_STEADY: '突发上限不得低于稳态值',
} as const;

/** DangerAction 通用外壳文案（UI UE §5.13）。 */
export const DANGER = {
  TITLE: '此操作会提高账户处置风险',
  TYPE_HINT_PREFIX: '逐字输入「',
  TYPE_HINT_SUFFIX: '」',
  CANCEL: '保持关闭',
  CONFIRM: '确认开启',
} as const;

/** 每源高级选项文案（G4 / F-V1，源级非全局）。 */
export const ADVANCED = {
  COLUMN: '请求特征改写',
  NOTE: '可能被平台判定为规避使用控制，风险高于普通接入。',
  SCOPE: '对你当前已接入的每个来源逐一生效：开启或关闭都只影响该来源，不波及其他来源。',
  ON: '已开启',
  OFF: '已关闭',
  CONFIRM_TITLE: '开启请求特征改写？',
  IMPACT: '仅影响 {source} 这一个来源。改写可能被平台判定为规避使用控制，导致限流或账户处置；账户一旦被终止将无法重新激活。',
  CONFIRM_WORD: '开启改写',
  CANCEL: '保持关闭',
} as const;

/**
 * 原因码 → 界面（UI UE §5.18，F3 单一枚举消费；402 文案红线逐字不可变）。
 * 只登记 PM 门禁三条 + unknown 兜底；其余键随 M1 各屏补齐。
 */
export const REASON_COPY: Partial<Record<string, { label: string; action: string }>> = {
  rate_limited: { label: '触发限流，已降频', action: '已自动降频，分钟级倒计时后恢复' },
  quota_exhausted: { label: '额度耗尽·次日04:00重置', action: '等待重置 / 改用其他来源' },
  not_found: { label: '接口或模型不存在', action: '查看模型列表' },
};

const REASON_FALLBACK = { label: '未知原因', action: '查看日志' };

export function reasonCopy(code: string, message?: string) {
  const hit = REASON_COPY[code];
  if (hit) return hit;
  return { label: message || REASON_FALLBACK.label, action: REASON_FALLBACK.action };
}

/**
 * 面板外壳文案（B1）。全部为**新增键**，不改动上方任何冻结文案：
 * §9 逐字基准与 402 唯一形态由 TC-M1-033 diff 通道守卫，本批零触碰。
 * M2 第二批：状态 pill 接真实探测（/api/time = 服务连通；401 = key 不符；其余 = 离线），
 * STATUS_UNWIRED 保留为首屏未完成首次探测时的观感键。
 * M5b：connect 空状态节退役（CONNECT_* / EMPTY_* 随节删除）；新增占位屏文案；
 * 页签标签统一迁往新 NAV 命名空间（下方）。
 */
export const PANEL = {
  TITLE: 'all2api 管理面板',
  RAIL_ARIA: '主导航',
  LOGO: 'a2',
  THEME: '切换明暗主题',
  THEME_SHORT: '明/暗',
  ACC_OVERVIEW: '总览',
  ACC_OVERVIEW_TITLE: '账号池 · 总览',
  OV_OPEN_DETAIL: '进入明细 →',
  STATUS_LABEL: '接口状态',
  STATUS_UNWIRED: '未接线',
  STATUS_ONLINE: '已连接',
  STATUS_UNAUTHORIZED: '未授权',
  STATUS_OFFLINE: '离线',
  STATUS_CHECKING: '探测中',
  STATUS_SKEW: '与服务器相差约 {sec} 秒（{tz}）',
  STATUS_REFRESH: '立即重新探测',
  SETTINGS_TITLE: '按源节流与高级选项',
  SETTINGS_SUB: '每个来源的节流预算与请求特征改写都集中在此管理。',
  RESTORE_DEFAULTS: '恢复默认设置',
  RESTORE_CONFIRM: '将把所有可调设置恢复为出厂默认（保留监听地址与 API 密钥），确定恢复吗？',
  PLACEHOLDER_TITLE: '并行开发中',
  PLACEHOLDER_BODY: '「{name}」功能屏由并行批次交付，合入后此占位将被替换。',
} as const;

/**
 * 左栏页签标签（M5b）。五项信息架构对齐参考控制台：账号池为首屏；
 * 「用量 / 积分构成」暂不做，不留死链接。TASKS / MODELS 命名空间归并行批（M5c）所有，
 * 本文件不得出现这两个命名空间。
 */
export const NAV = {
  ACCOUNTS: '账号池',
  TASKS: '任务中心',
  MODELS: '模型',
  CONFIG: '配置',
  LOGS: '运行日志',
} as const;

/**
 * 账号池屏文案（M5b）。数据端点 GET /api/accounts 由后端并行批交付，形状见
 * lib/panelApi.ts 的 TODO(contract) 注释；空池引导按运维实际路径逐字写。
 */
export const ACCOUNTS = {
  SUB: '各来源账号的池化状态、积分与健康度总览，每 30 秒自动刷新。',
  LOADING: '正在读取账号池…',
  ERROR_TITLE: '账号池读取失败',
  ERROR_HINT: '面板与网关同源部署。请确认网关进程在运行、api_key 配置一致，然后重试。',
  RETRY: '重试',
  REFRESH: '刷新',
  OV_ACCOUNTS: '账号',
  OV_CREDIT: '积分',
  OV_REQUESTS: '本次运行请求',
  OV_REQUESTS_TODO: '统计维度待接入（不展示未采集的数据）',
  OV_LAST_OK: '最近成功',
  OV_NONE: '未添加',
  ADD_OPEN: '＋ 添加账号',
  ADD_CLOSE: '收起',
  ADD_TITLE: '添加账号',
  ADD_WB_LABEL: 'WorkBuddy 凭证 JSON',
  ADD_QO_LABEL: 'Qoder PAT',
  ADD_SAVE: '保存并热生效',
  ADD_SAVING: '正在保存…',
  ADD_OK_PREFIX: '账号已添加：',
  ADD_OK_SUFFIX: '（已入池，额度正在同步）',
  CARD_TOTAL: '账号总数',
  CARD_ACTIVE: '可用',
  CARD_COOLING: '冷却中',
  CARD_DISABLED: '已禁用',
  CARD_CREDIT: '积分剩余',
  COL_PLATFORM: '平台',
  COL_ACCOUNT: '账号 ID',
  COL_STATUS: '状态',
  COL_CREDIT: '积分',
  COL_HEALTH: '健康度',
  COL_EXPIRES: '到期时间',
  COL_COOLDOWN: '冷却至',
  COPY: '点击复制账号 ID',
  COPIED: '已复制',
  DASH: '—',
  EMPTY_TITLE: '账号池为空',
  EMPTY_BODY_QO: 'Qoder 平台还没有账号。点击「＋ 添加账号」，粘贴客户端生成的 Personal Access Token 即可。',
  EMPTY_WB_HINT: 'WorkBuddy 凭证可从官方桌面端 CodeBuddyExtension 的 auth 目录复制。',
  EMPTY_TRAE: 'Trae 平台尚未接入网关，启用与添加账号将在接入后开放。',
  EMPTY_BODY: '该平台还没有账号。点击「＋ 添加账号」，按向导粘贴凭证即可，保存后立即生效。',
  /* ---- M6b 富列 + 行内操作（只追加，不改上方既有键）---- */
  COL_SUCCESS: '成功 / 失败',
  COL_INFLIGHT: '在途',
  COL_LASTOK: '最近成功',
  COL_ACTIONS: '操作',
  STATUS_PAUSED: '已暂停选号',
  STATUS_CRED_INVALID: '凭证失效',
  BTN_PAUSE: '暂停选号',
  BTN_RESUME: '恢复选号',
  BTN_REMOVE: '删除',
  CONFIRM_REMOVE: '确认删除该账号？它将从账号池移除，本地凭证文件将被删除，不可恢复。',
  ACTION_ERR_PREFIX: '操作失败：',
} as const;

/**
 * 运行日志屏文案（M5b；M8-reqlog 追加 TAB_/RESULT_/REQ_ 组——请求记录 tab，
 * 全部为新增键，不改动上方任何冻结段）。数据端点 GET /api/logs?limit=200 与
 * GET /api/requests?limit=300&result=…；级别过滤只内置 全部/info/error 三档，
 * 其他级别在「全部」下如实显示。
 */
export const LOGS = {
  SUB: '网关运行日志实时滚动，可按级别过滤，每 10 秒自动刷新。',
  LOADING: '正在读取日志…',
  ERROR_TITLE: '日志读取失败',
  ERROR_HINT: '面板与网关同源部署。请确认网关进程在运行、api_key 配置一致，然后重试。',
  RETRY: '重试',
  REFRESH: '刷新',
  LEVEL_LABEL: '级别过滤',
  FILTER_ALL: '全部',
  FILTER_INFO: 'info',
  FILTER_ERROR: 'error',
  AUTOSCROLL: '自动滚动',
  LIST_LABEL: '日志列表',
  DASH: '—',
  EMPTY_TITLE: '暂无日志',
  EMPTY_BODY: '网关产生新的运行日志后会自动出现在这里。',
  TAB_LABEL: '日志屏视图',
  TAB_RUNTIME: '运行日志',
  TAB_REQUESTS: '请求记录',
  RESULT_SUCCESS: '成功',
  RESULT_ERROR: '失败',
  RESULT_STREAM_ERROR: '流中断',
  RESULT_INTERRUPTED: '客户端取消',
  RESULT_UNKNOWN: '未知',
  REQ_SUB: '最近 /v1 请求（旧→新），结果过滤为服务端口径，每 30 秒自动刷新。',
  REQ_LOADING: '正在读取请求记录…',
  REQ_ERROR_TITLE: '请求记录读取失败',
  REQ_FILTER_LABEL: '结果过滤',
  REQ_FILTER_SUCCESS: '成功',
  REQ_FILTER_ERROR: '失败',
  REQ_EMPTY_TITLE: '暂无请求记录',
  REQ_EMPTY_BODY: '网关处理新的 /v1 请求后会自动出现在这里。',
  REQ_LIST_LABEL: '请求记录表',
  REQ_COL_TIME: '时间',
  REQ_COL_RESULT: '结果',
  REQ_COL_MODEL: '模型',
  REQ_COL_ACCOUNT: '账号',
  REQ_COL_LATENCY: '耗时',
  REQ_COL_TOKENS: 'Token（输入/输出）',
  REQ_MS: '{ms} ms',
} as const;

/**
 * 设置屏（M2 B2 起，M2 第二批补齐保存流/通用组/X-API-Key）。全部为新增键，不触碰上方任何冻结段。
 * 边界如实：保存端点后端并行开发中——运行期 404/501 原样展示错误码，不硬编码假成功；
 * X-API-Key 只存内存态（M1 红线），刷新丢失是预期，文案如实说明。
 */
export const SETTINGS = {
  RESTORE_DEFAULTS: '恢复默认设置',
  RESTORE_CONFIRM: '将把所有可调设置恢复为出厂默认（保留监听地址与 API 密钥），确定恢复吗？',
  LOADING: '正在读取网关配置…',
  ERROR_TITLE: '网关配置读取失败',
  ERROR_HINT: '面板与网关同源部署。请确认网关进程在运行、api_key 配置一致，然后重试。',
  RETRY: '重试',
  TIME_NOW: '服务器时间 {local}（{tz}），与本机相差约 {sec} 秒',
  TIME_UNAVAILABLE: '服务器时间暂不可用（不影响配置展示）',
  SOURCE_PICKER_LABEL: '来源',
  NO_SOURCES: '当前网关配置没有任何来源。',
  ADVANCED_TITLE: '高级选项（逐源）',
  ADVANCED_EMPTY: '该来源没有下发任何高级选项。',
  SOURCE_ENABLED_LABEL: '来源启用',
  SOURCE_ENABLED_ON: '已启用',
  SOURCE_ENABLED_OFF: '已停用',
  SAVE: '保存全部改动',
  SAVING: '正在保存…',
  /** 保存流主提示：dirty 非空时保存按钮激活，成功以服务端回读为准重建基线。 */
  SAVE_OK_NOTE: '已保存，展示的是网关回读后的生效配置。',
  SAVE_FAILED: '保存失败：{detail}',
  SAVE_REJECTED_TITLE: '保存被网关拒绝（逐字段）',
  SAVE_MALFORMED_RESPONSE: '网关返回了无法理解的保存响应',
  /** 无 dirty 时的基线说明（原 SAVE_DISABLED_NOTE 语义随保存流上线退役，键名保留更替）。 */
  SAVE_NOTE: '改动只保存在本页内存，提交保存后才写入网关；未保存即刷新或离开会丢失。',
  DIRTY_BADGE: '有本地改动',
  DIRTY_SUMMARY: '{count} 处本地改动未保存',
  SYNCED_NOTE: '当前展示的是网关生效中的实时配置。',
  DISCARD: '放弃本地改动',
  GENERIC_EFFECT_RESTART: '需重启网关生效',
  GENERIC_CONFIRM_NOTE: '危险字段：改动将随下一次保存一并提交。',
  APIKEY_TITLE: '网关 API Key（可选，内存态）',
  APIKEY_PLACEHOLDER: '粘贴网关 api_key（配置为空 = 开放模式，可留空）',
  APIKEY_NOTE: '只保存在当前页面内存中，不会写入浏览器存储；刷新或关闭页面后需重新粘贴。',
} as const;

/** SETTINGS 模板占位符替换（与 consentCheckLabel 同一做法，组件不内联拼串逻辑）。 */
export function settingsLine(tpl: string, vars: Record<string, string | number>): string {
  return tpl.replace(/\{(\w+)\}/g, (_, k: string) => String(vars[k] ?? ''));
}

/**
 * 后端 /api/config/fields 的 label / description 是文案层 key（UI §5.16，configfields.go 逐字下发
 * 如 'config.field.steady_rpm.label'），面板在这里换算成展示文案；未知 key 原样回显（可追溯优于装懂）。
 * 展示串复用既有冻结文案（THROTTLE / ADVANCED 的对应值），不另立第二写法。
 */
export const CONFIG_FIELD_COPY: Record<string, string> = {
  'config.field.steady_rpm.label': '稳态请求',
  'config.field.burst_rpm.label': '突发上限',
  'config.field.min_gap_ms.label': '最小间隔',
  'config.field.daily_cap.label': '每日上限',
  'config.field.inflight.label': '在途请求',
  'config.field.base_429_sec.label': '429 退避基数',
  'config.field.not_found_sec.label': '404 退避',
  'config.field.jitter_minutes.label': '签到抖动',
  'config.field.halve_budget_after_429.label': THROTTLE.HALVE,
  'config.field.fingerprint_rewrite.label': ADVANCED.COLUMN,
  'config.field.fingerprint_rewrite.desc': '该开关逐源独立、默认关闭；开启影响发往该来源的请求特征，随时可再关闭。',
  'config.field.source_enabled.label': SETTINGS.SOURCE_ENABLED_LABEL,
  // ---- 通用配置组字段（M2 第二批；key 逐字 = 后端 configfields.go 交付） ----
  'config.field.listen.label': '监听地址',
  'config.field.api_key.label': '网关 API Key',
  'config.field.external_base_url.label': '对外 Base URL',
  'config.field.external_base_url.desc': '生成接入地址时使用的对外可达地址；留空则用请求本身的地址。',
  'config.field.responses_api.label': '启用 /v1/responses 接口',
  'config.field.credit_floor.label': '额度下限',
  'config.field.max_retries.label': '最大重试',
  'config.field.pool_cooldown_sec.label': '池冷却时长',
  'config.field.signin_cron.label': '签到定时',
  'config.field.task_cron.label': '任务定时',
  'config.field.token_keepalive_cron.label': '凭证保活定时',
};

/** 通用配置组标题（组名 = 后端 configfields.go 交付的 group 字段，未识别组原样显示）。 */
export const CONFIG_GROUP_TITLES: Record<string, string> = {
  gateway: '网关',
  routing: '路由',
  pooling: '池化',
  schedule: '定时任务',
};

export function configFieldCopy(copyKey: string): string {
  return CONFIG_FIELD_COPY[copyKey] ?? copyKey;
}

/** 后端单位码 → 展示单位。'count' 在 daily_cap / inflight 两字段复用同一码，展示档按字段定形；其余码直查。 */
const FIELD_UNIT_FOR_COUNT: Record<string, string> = {
  daily_cap: THROTTLE.UNIT_DAILY,
  inflight: THROTTLE.UNIT_INFLIGHT,
};

const UNIT_CODE_COPY: Record<string, string> = {
  rpm: THROTTLE.UNIT_RPM,
  ms: THROTTLE.UNIT_MS,
  sec: THROTTLE.UNIT_SEC,
  min: THROTTLE.UNIT_MIN,
  credit: '点',
};

export function configUnitCopy(unit: string | undefined, fieldKey: string): string {
  if (!unit) return THROTTLE.DASH;
  if (unit === 'count') return FIELD_UNIT_FOR_COUNT[fieldKey] ?? '次';
  return UNIT_CODE_COPY[unit] ?? unit;
}

/**
 * 任务中心屏（M5c）。全部为**新增键**，不改动上方任何冻结段（并行批在同文件改
 * NAV/ACCOUNTS/LOGS 命名空间，本块独立追加）。边界如实：手动触发端点不存在
 * （仅账号级 recover），文案明示只读，不渲染假按钮。
 */
export const TASKS = {
  LOADING: '正在读取任务调度状态…',
  ERROR_TITLE: '任务状态读取失败',
  ERROR_HINT: '面板与网关同源部署。请确认网关进程在运行、api_key 配置一致，然后重试。',
  RETRY: '重试',
  /** 顶部诚实说明行：本屏只读 + 无手动触发端点。 */
  NOTE: '任务由网关定时自动调度，面板只读展示；手动触发接口未提供（仅账号级修复可用）。',
  COUNT: '已注册 {count} 个定时任务',
  REFRESH: '刷新',
  EMPTY: '网关没有注册任何定时任务。',
  PLATFORM_NONE: '该平台暂无定时任务（Qoder 额度为订阅制无签到；Trae 的每日签到随平台接入开放）。',
  ENABLED_ON: '已启用',
  ENABLED_OFF: '已停用',
  CRON_LABEL: '调度周期',
  LAST_RUN_TITLE: '最近一次运行',
  NEVER_RUN: '尚未运行',
  STAT_TOTAL: '总数',
  STAT_SUCCESS: '成功',
  STAT_FAILED: '失败',
  STAT_SKIPPED: '跳过',
  RAN_AT: '运行时间：{at}',
} as const;

/** 任务名 → 中文名（未知任务名回退原始标识，如实可追溯）。 */
const TASK_NAME_COPY: Record<string, string> = {
  signin: '自动签到',
  active_check: '活跃上报',
  cat_travel: '猫猫旅行',
  token_keepalive: '凭证保活',
};

/** 任务名 → 一行说明（与 CONSENT_APPENDIX.workbuddy 的例行操作描述口径一致）。 */
const TASK_DESC_COPY: Record<string, string> = {
  signin: '按计划代替你在平台完成每日签到，领取签到权益。',
  active_check: '按计划上报账号活跃状态，降低长期沉默触发的风控概率。',
  cat_travel: 'WorkBuddy 专属的宠物出行任务，按计划代替你完成猫猫旅行派遣。',
  token_keepalive: '按计划刷新平台凭证，减少「凭证已过期」导致的重新授权。',
};

export function taskDisplayName(name: string): string {
  return TASK_NAME_COPY[name] ?? name;
}

export function taskDescription(name: string): string {
  return TASK_DESC_COPY[name] ?? '';
}

/**
 * 模型屏（M5c，M8 升级）。M8 新增键：COL_CREDIT / COL_TIERS（wb2api 同款两列）、
 * SOURCE_UPSTREAM / SOURCE_MIXED（目录来源三态）；NOTE 更新为实时目录语义。
 * 边界如实：WorkBuddy 实时拉上游目录（网关侧 60s 缓存，refresh=1 强制重拉），
 * Qoder 保持静态快照；未知一律「—」，不编造。
 */
export const MODELS = {
  LOADING: '正在读取模型目录…',
  ERROR_TITLE: '模型目录读取失败',
  ERROR_HINT: '面板与网关同源部署。请确认网关进程在运行、api_key 配置一致，然后重试。',
  RETRY: '重试',
  /** 顶部诚实说明行：目录来源 + 能力字段缺失语义。 */
  NOTE: '模型目录：WorkBuddy 实时拉取上游（网关侧 60s 缓存，「刷新」强制重拉），Qoder 为静态快照；「—」表示目录未声明该能力。',
  FILTER_LABEL: '平台',
  FILTER_ALL: '全部平台',
  SOURCE_LOCAL: '目录来源：本地配置',
  SOURCE_UPSTREAM: '目录来源：上游实时',
  SOURCE_MIXED: '目录来源：上游实时 + 本地静态',
  SOURCE_PREFIX: '目录来源：',
  COUNT: '{count} 个模型',
  REFRESH: '刷新',
  EMPTY: '模型目录为空。',
  FILTER_EMPTY: '该平台没有已登记的模型。',
  COL_PLATFORM: '平台',
  COL_MODEL: '模型',
  COL_CREDIT: '积分倍率',
  COL_TIERS: '思考档位',
  COL_CONTEXT: '上下文',
  COL_MAX_OUTPUT: '最大输出',
  COL_TOOL: '工具调用',
  COL_STREAM: '流式',
  COL_FREE: '免费额度',
  UNKNOWN: '—',
} as const;

/** 平台标识 → 显示名（与 SOURCE_NAMES 同源扩列；未知平台回退原始标识）。 */
export const MODEL_SOURCE_NAMES: Record<string, string> = {
  workbuddy: 'WorkBuddy',
  qoder: 'Qoder',
  trae: 'Trae',
};

/** 能力三态格的明确值文案（null = 未声明，由 MODELS.UNKNOWN 承担）。 */
export function modelCapLabel(v: 'yes' | 'no'): string {
  return v === 'yes' ? '支持' : '不支持';
}

/** v1.2：cron 字段的 24 小时制时点选择（主人裁定——用户不面对 cron 语法）。 */
export const CRON_FIELD = {
  HOURS_LABEL: '运行时点（24 小时制，勾选即生效）',
  HOURS_HINT: '勾选每天运行的小时；保存后自动转成调度表达式。',
  FALLBACK_SHAPE: '该表达式不是"每日整点"形态，已回落为原始表达式编辑（懂 cron 可直接改）。',
  FALLBACK_EDITED: '已手动修改为非每日形态，已回落为原始表达式编辑。',
} as const;

/**
 * 成长任务弹层文案（M7b，wb2api「积分任务」同款）。**独立新增命名空间**，
 * 不改动上方任何冻结段；端点由后端并行批交付（growth-tasks / growth-accept / growth-claim-auto），
 * {error} 信封原文逐字展示，不回显凭证。
 * 状态六值为冻结闭集（claimed/claimable/completed/in_progress/accepted/not_accepted），
 * 未知值原样回显（可追溯优于装懂）。
 */
export const GROWTH = {
  BTN_OPEN: '任务',
  TITLE: '积分任务 · {name}',
  NOTE: '成长任务来自上游平台，接受与领取由网关代为执行；奖励与状态以上游实际回执为准。',
  COL_TASK: '任务',
  COL_PROGRESS: '进度',
  COL_REWARD: '奖励',
  COL_STATUS: '状态',
  ACCEPT_ALL: '全部接受',
  CLAIM_AUTO: '一键完成可自动任务',
  REFRESH: '重新查询',
  CLOSE: '关闭',
  EMPTY: '该账号暂无成长任务',
  LOADING: '正在读取成长任务…',
  ERROR_TITLE: '成长任务读取失败',
  RETRY: '重试',
  CLAIM_OK: '领取成功 +{n} 积分',
  ACCEPT_OK: '已接受 {n} 个任务',
  ACTION_ERR_PREFIX: '操作失败：',
  STATUS_CLAIMED: '已领取',
  STATUS_CLAIMABLE: '可领取',
  STATUS_COMPLETED: '已完成',
  STATUS_IN_PROGRESS: '进行中',
  STATUS_ACCEPTED: '已报名',
  STATUS_NOT_ACCEPTED: '未报名',
  REWARD_CREDIT: '+{n} 分',
  REWARD_ENERGY: '+{n} 能',
  DASH: '—',
} as const;

/**
 * 用量 KPI 文案（M8-reqlog）。数据端点 GET /api/usage（本次运行口径，
 * 进程重启清零）；全部为**新增键**，独立追加不与上方冻结段冲突。
 * 边界如实：流式请求上游当前不下发 usage，token 口径只含非流式回包，
 * 前端不做任何折算或补零。
 */
export const USAGE = {
  WINDOW_BADGE: '本次运行',
  WINDOW_NOTE: '进程重启清零',
  KPI_REQUESTS: '请求',
  KPI_FAIL: '失败',
  KPI_PROMPT: '输入 tokens',
  KPI_COMPLETION: '输出 tokens',
  ARIA_LABEL: '本次运行用量摘要',
  ERROR_NOTE: '用量数据读取失败，30 秒后自动重试。',
} as const;
