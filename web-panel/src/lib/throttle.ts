import type { ConfigPatch, FieldDef, FieldError, ThrottleBudget } from '../types/api-contract';
import { THROTTLE } from '../i18n/zh';

/** JSON path 前缀冻结：速率落点唯一 = sources.<id>.throttle.*（G1，顶层 Throttle map 已废弃）。 */
export function throttlePath(source: string, jsonKey: string): string {
  return `sources.${source}.throttle.${jsonKey}`;
}

/**
 * 可编辑字段元数据。M1 由后端 /api/config 下发 FieldDef 取代本表（UI §5.13「前端不硬编码表单」），
 * 替换时只改数据来源，props 与视觉不变。
 * 单位纪律：速率字段只有 `_rpm`（整数·每分钟）；秒只出现在退避时长（`*_sec`）。前端不做任何 ×/÷60 换算。
 */
export const THROTTLE_FIELDS: FieldDef[] = [
  { key: 'steady_rpm', label: '稳态请求', type: 'number', unit: THROTTLE.UNIT_RPM, min: 1, max: 60, effect: 'immediate', description: 'PRD §7.1 / PM 调研 §4 建议默认值，可调整' },
  { key: 'burst_rpm', label: '突发上限', type: 'number', unit: THROTTLE.UNIT_RPM, min: 1, max: 120, effect: 'immediate' },
  { key: 'min_gap_ms', label: '最小间隔', type: 'number', unit: THROTTLE.UNIT_MS, min: 0, max: 60000, effect: 'immediate', description: '[下界, 上界] 随机间隔，消除机器节拍' },
  { key: 'daily_cap', label: '每日上限', type: 'number', unit: THROTTLE.UNIT_DAILY, min: 1, max: 10000, effect: 'immediate' },
  { key: 'inflight', label: '在途请求', type: 'number', unit: THROTTLE.UNIT_INFLIGHT, min: 1, max: 32, effect: 'immediate' },
  { key: 'backoff.base_429_sec', label: '429 退避基数', type: 'number', unit: THROTTLE.UNIT_SEC, min: 600, max: 3600, effect: 'immediate', description: 'PRD §7.1 定 ≥600 秒' },
  { key: 'backoff.not_found_sec', label: '404 退避', type: 'number', unit: THROTTLE.UNIT_SEC, min: 60, max: 3600, effect: 'immediate', description: 'PM O-2 冻结默认 300 秒（工程建议值，非平台条款）' },
  { key: 'jitter_minutes', label: '签到抖动', type: 'number', unit: THROTTLE.UNIT_MIN, min: 0, max: 120, effect: 'immediate', description: THROTTLE.HALVE_ONLY_QODER_NOTE },
];

/** 只读锚点：契约内保留、界面上不给输入框（删这行 = 断 402 文案红线的契约链）。 */
export const THROTTLE_READONLY = {
  key: 'backoff.exhaust_402_until',
  label: THROTTLE.EXHAUST_COLUMN,
  value: 'next-04:00' as const,
};

/** 是否具备定时签到任务（抖动 / 当日减半两列的显示条件，UI §5.20 第 7、8 条）。 */
export function isSigninSource(source: string): boolean {
  return source === 'workbuddy' || source === 'qoder';
}

/**
 * 后端 /api/config/fields 下发的按源字段注册表（UI §5.13「前端不硬编码表单」的数据源侧）。
 * 注入后 editableThrottleFields / validateThrottleBudget 的 label、单位与边界均以交付集为准
 * （后端 configfields.go 是可编辑边界的唯一权威表）；注册表为空时回退上方仓内表（单测/离线场景）。
 * 显示条件（jitter / halve 是否交付）完全由交付集决定，前端不自行猜测来源类别。
 */
let deliveredThrottleFields: Readonly<Record<string, readonly FieldDef[]>> | null = null;

/** 由设置屏在 GET /api/config/fields 成功后注入解析好的展示字段；传 null 清空（测试隔离用）。 */
export function setDeliveredThrottleFields(bySource: Record<string, FieldDef[]> | null): void {
  deliveredThrottleFields = bySource;
}

export function editableThrottleFields(source: string): FieldDef[] {
  const delivered = deliveredThrottleFields?.[source];
  if (delivered) return delivered.map((f) => ({ ...f }));
  return THROTTLE_FIELDS.filter((f) => {
    if (f.key === 'jitter_minutes') return isSigninSource(source);
    return true;
  });
}

function fmt(tpl: string, vars: Record<string, string | number>): string {
  return tpl.replace(/\{(\w+)\}/g, (_, k: string) => String(vars[k] ?? ''));
}

function checkInt(
  out: FieldError[],
  path: string,
  label: string,
  value: number | undefined,
  min: number,
  max: number,
): void {
  if (value === undefined) return;
  // F-5（测试 seq=482 欠账③）：空输入是 NaN，不再静默转 0 放行写回。
  // code 仍复用冻结三值里的 "range"——空值不新增第四个 code。
  if (Number.isNaN(value) || !Number.isInteger(value)) {
    out.push({ key: path, message: fmt(THROTTLE.ERR_INT, { label }), code: 'range' });
    return;
  }
  if (value < min || value > max) {
    out.push({ key: path, message: fmt(THROTTLE.ERR_RANGE, { label, min, max }), code: 'range' });
  }
}

/** 提交前的结构校验；服务端仍是权威，返回的 key 一律是冻结 JSON path。
 *  边界与报错 label 优先取交付字段集（后端 configfields.go 为唯一权威表），无交付时回退仓内默认。 */
export function validateThrottleBudget(source: string, budget: ThrottleBudget): FieldError[] {
  const out: FieldError[] = [];
  const p = (k: string) => throttlePath(source, k);
  const fields = editableThrottleFields(source);
  const ruleOf = (key: string, lo: number, hi: number, label: string) => {
    const hit = fields.find((f) => f.key === key);
    return { lo: hit?.min ?? lo, hi: hit?.max ?? hi, label: hit?.label ?? label };
  };

  const steady = ruleOf('steady_rpm', 1, 60, '稳态请求');
  const burst = ruleOf('burst_rpm', 1, 120, '突发上限');
  const daily = ruleOf('daily_cap', 1, 10000, '每日上限');
  const inflightRule = ruleOf('inflight', 1, 32, '在途请求');
  const base429 = ruleOf('backoff.base_429_sec', 600, 3600, '429 退避基数');
  const notFound = ruleOf('backoff.not_found_sec', 60, 3600, '404 退避');
  const jitter = ruleOf('jitter_minutes', 0, 120, '签到抖动');

  checkInt(out, p('steady_rpm'), steady.label, budget.steadyRpm, steady.lo, steady.hi);
  checkInt(out, p('burst_rpm'), burst.label, budget.burstRpm, burst.lo, burst.hi);
  checkInt(out, p('daily_cap'), daily.label, budget.dailyCap, daily.lo, daily.hi);
  checkInt(out, p('inflight'), inflightRule.label, budget.inflight, inflightRule.lo, inflightRule.hi);
  checkInt(out, p('backoff.base_429_sec'), base429.label, budget.backoff?.base429Sec, base429.lo, base429.hi);
  checkInt(out, p('backoff.not_found_sec'), notFound.label, budget.backoff?.notFoundSec, notFound.lo, notFound.hi);
  if (budget.jitterMinutes !== undefined) {
    checkInt(out, p('jitter_minutes'), jitter.label, budget.jitterMinutes, jitter.lo, jitter.hi);
  }

  const gap = ruleOf('min_gap_ms', 0, 60000, '最小间隔');
  const [lo, hi] = budget.minGapMs ?? [0, 0];
  checkInt(out, p('min_gap_ms'), `${gap.label}下界`, lo, gap.lo, gap.hi);
  checkInt(out, p('min_gap_ms'), `${gap.label}上界`, hi, gap.lo, gap.hi);
  if (lo > hi) {
    out.push({ key: p('min_gap_ms'), message: THROTTLE.ERR_GAP_ORDER, code: 'range' });
  }
  if (budget.burstRpm < budget.steadyRpm) {
    out.push({ key: p('burst_rpm'), message: THROTTLE.ERR_BURST_LT_STEADY, code: 'range' });
  }
  return out;
}

/** camelCase 表单值 → snake_case 写回 path；未出现在 patch 里的键不写，exhaust_402_until 永不出现在此。 */
export function throttlePatchToConfigPatch(source: string, patch: Partial<ThrottleBudget>): ConfigPatch {
  const out: ConfigPatch = [];
  const put = (jsonKey: string, value: unknown) => out.push({ key: throttlePath(source, jsonKey), value });

  if ('steadyRpm' in patch) put('steady_rpm', patch.steadyRpm);
  if ('burstRpm' in patch) put('burst_rpm', patch.burstRpm);
  if ('minGapMs' in patch) put('min_gap_ms', patch.minGapMs);
  if ('dailyCap' in patch) put('daily_cap', patch.dailyCap);
  if ('inflight' in patch) put('inflight', patch.inflight);
  if ('jitterMinutes' in patch) put('jitter_minutes', patch.jitterMinutes);
  if ('halveBudgetAfter429' in patch) put('halve_budget_after_429', patch.halveBudgetAfter429);

  const b = patch.backoff;
  if (b) {
    if ('base429Sec' in b) put('backoff.base_429_sec', b.base429Sec);
    if ('notFoundSec' in b) put('backoff.not_found_sec', b.notFoundSec);
  }
  return out;
}

/** 安全默认：advanced 缺失或键缺失一律按 false（F-V1 冻结；UI §7 回退矩阵）。 */
export function fingerprintRewriteEnabled(
  advanced: Record<string, { fingerprintRewrite: boolean } | undefined> | undefined,
  source: string,
): boolean {
  return advanced?.[source]?.fingerprintRewrite === true;
}
