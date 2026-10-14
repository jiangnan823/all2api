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
 */
export const PANEL = {
  TITLE: 'all2api 管理面板',
  RAIL_ARIA: '主导航',
  LOGO: 'a2',
  NAV_SETTINGS: '配置',
  NAV_CONNECT: '接入',
  THEME: '切换明暗主题',
  STATUS_LABEL: '接口状态',
  STATUS_UNWIRED: '未接线',
  SETTINGS_TITLE: '按源节流与高级选项',
  SETTINGS_SUB: '每个来源的节流预算与请求特征改写都集中在此管理。',
  CONNECT_TITLE: '接入与凭证',
  CONNECT_SUB: '新增来源、校验凭证，以及接入须知的确认记录。',
  EMPTY_TITLE: '本节尚未接入网关接口',
  EMPTY_BODY: '这里显示的是网关的实时配置与状态。本次构建交付的是界面骨架，接线完成后内容自动出现。',
} as const;
