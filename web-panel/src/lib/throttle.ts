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

export function editableThrottleFields(source: string): FieldDef[] {
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
  if (value === undefined || Number.isNaN(value)) return;
  if (!Number.isInteger(value)) {
    out.push({ key: path, message: fmt(THROTTLE.ERR_INT, { label }), code: 'range' });
    return;
  }
  if (value < min || value > max) {
    out.push({ key: path, message: fmt(THROTTLE.ERR_RANGE, { label, min, max }), code: 'range' });
  }
}

/** 提交前的结构校验；服务端仍是权威，返回的 key 一律是冻结 JSON path。 */
export function validateThrottleBudget(source: string, budget: ThrottleBudget): FieldError[] {
  const out: FieldError[] = [];
  const p = (k: string) => throttlePath(source, k);

  const steady = editableThrottleFields(source).find((f) => f.key === 'steady_rpm');
  const burst = editableThrottleFields(source).find((f) => f.key === 'burst_rpm');
  checkInt(out, p('steady_rpm'), '稳态请求', budget.steadyRpm, steady?.min ?? 1, steady?.max ?? 60);
  checkInt(out, p('burst_rpm'), '突发上限', budget.burstRpm, burst?.min ?? 1, burst?.max ?? 120);
  checkInt(out, p('daily_cap'), '每日上限', budget.dailyCap, 1, 10000);
  checkInt(out, p('inflight'), '在途请求', budget.inflight, 1, 32);
  checkInt(out, p('backoff.base_429_sec'), '429 退避基数', budget.backoff?.base429Sec, 600, 3600);
  checkInt(out, p('backoff.not_found_sec'), '404 退避', budget.backoff?.notFoundSec, 60, 3600);
  if (budget.jitterMinutes !== undefined) {
    checkInt(out, p('jitter_minutes'), '签到抖动', budget.jitterMinutes, 0, 120);
  }

  const [lo, hi] = budget.minGapMs ?? [0, 0];
  checkInt(out, p('min_gap_ms'), '最小间隔下界', lo, 0, 60000);
  checkInt(out, p('min_gap_ms'), '最小间隔上界', hi, 0, 60000);
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
