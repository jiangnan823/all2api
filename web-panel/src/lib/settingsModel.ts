import type { ConfigGroup, FieldDef, PanelConfig, ThrottleBudget } from '../types/api-contract';
import { configFieldCopy, configUnitCopy } from '../i18n/zh';

/**
 * 设置屏模型（M2 B2）：GET /api/config + GET /api/config/fields 的两条响应合并成
 * 「按源草稿」一棵树。字段元数据在这里换算成展示文案（label/unit 都是后端下发的文案层 key，
 * UI §5.16 不内联中文），此后组件层只见展示串。
 * 显示条件纪律（后端 configfields.go 裁定）：FieldDef present = render——
 * jitter / halve 是否交付完全由交付集决定，前端不自行猜测来源类别。
 */

export interface SourceDraft {
  budget: ThrottleBudget;
  fingerprint: boolean;
  enabled: boolean;
}

export interface SourceDangerFields {
  /** requiresConfirm 布尔位（当前唯一成员：request_fingerprint_rewrite）→ AdvancedToggle + DangerAction。 */
  fingerprint?: FieldDef;
  /** 普通布尔位（sources.<id>.enabled 一键摘源）→ 直接开关，无危险壳。 */
  enabled?: FieldDef;
}

export interface SettingsModel {
  sources: string[];                                   // 排序稳定（后端交付已排序，这里再保一道）
  throttleFields: Record<string, FieldDef[]>;          // 短键（steady_rpm…），label/单位已解析
  danger: Record<string, SourceDangerFields>;
  server: Record<string, SourceDraft>;                 // 服务端基线（dirty 比较的对端）
  noticeVersion: string;
}

const THROTTLE_GROUP_PREFIX = 'sources_throttle.';
const DANGER_GROUP_PREFIX = 'danger.';

function cloneBudget(b: ThrottleBudget): ThrottleBudget {
  return { ...b, minGapMs: [b.minGapMs[0], b.minGapMs[1]], backoff: { ...b.backoff } };
}

/** 交付集没有的键（Go 零值序列化导致 wire 上恒在）按交付条件丢弃，让 ThrottleForm 的渲染条件与后端交付对齐。
 *  对齐看的是**全类型**交付键集（含布尔 halve_budget_after_429），不是数值表子集。 */
function alignBudgetToFields(b: ThrottleBudget, deliveredKeys: ReadonlySet<string>): ThrottleBudget {
  return {
    ...cloneBudget(b),
    jitterMinutes: deliveredKeys.has('jitter_minutes') ? b.jitterMinutes : undefined,
    halveBudgetAfter429: deliveredKeys.has('halve_budget_after_429') ? b.halveBudgetAfter429 : undefined,
  };
}

/** raw 透传里的 sources.<id>.enabled：缺失/形状不对按启用展示（Go bool 恒序列化，缺失即异常输入）。 */
function sourceEnabledFromRaw(raw: Record<string, unknown>, source: string): boolean {
  const sources = raw['sources'];
  if (sources === null || typeof sources !== 'object' || Array.isArray(sources)) return true;
  const entry = (sources as Record<string, unknown>)[source];
  if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) return true;
  const enabled = (entry as Record<string, unknown>)['enabled'];
  return typeof enabled === 'boolean' ? enabled : true;
}

function resolveCopy(f: FieldDef): FieldDef {
  return {
    ...f,
    label: configFieldCopy(f.label),
    description: f.description === undefined ? undefined : configFieldCopy(f.description),
    unit: f.unit === undefined ? undefined : configUnitCopy(f.unit, f.key),
  };
}

export function buildSettingsModel(config: PanelConfig, groups: ConfigGroup[]): SettingsModel {
  const throttleFields: Record<string, FieldDef[]> = {};
  const deliveredThrottleKeys: Record<string, ReadonlySet<string>> = {};
  const danger: Record<string, SourceDangerFields> = {};

  for (const g of groups) {
    if (g.group.startsWith(THROTTLE_GROUP_PREFIX)) {
      const source = g.group.slice(THROTTLE_GROUP_PREFIX.length);
      const pathPrefix = `sources.${source}.throttle.`;
      const stripped = g.fields
        .filter((f) => f.key.startsWith(pathPrefix))
        .map((f) => ({ ...f, key: f.key.slice(pathPrefix.length) }));
      deliveredThrottleKeys[source] = new Set(stripped.map((f) => f.key));
      // 数值表只吃数值字段；布尔交付（halve_budget_after_429）经预算对齐驱动勾选行
      throttleFields[source] = stripped.filter((f) => f.type === 'number').map(resolveCopy);
    } else if (g.group.startsWith(DANGER_GROUP_PREFIX)) {
      const source = g.group.slice(DANGER_GROUP_PREFIX.length);
      const slot: SourceDangerFields = danger[source] ?? {};
      for (const f of g.fields) {
        if (f.key === `sources.${source}.advanced.request_fingerprint_rewrite`) {
          slot.fingerprint = resolveCopy(f);
        } else if (f.key === `sources.${source}.enabled`) {
          slot.enabled = resolveCopy(f);
        }
      }
      danger[source] = slot;
    }
    // 其余组（gateway/routing/pooling/schedule）属通用配置表单范围，设置屏本批只装配来源相关组
  }

  const sources = Object.keys(config.throttle).sort();
  const server: Record<string, SourceDraft> = {};
  for (const s of sources) {
    const delivered = deliveredThrottleKeys[s];
    server[s] = {
      budget: delivered ? alignBudgetToFields(config.throttle[s], delivered) : cloneBudget(config.throttle[s]),
      // F-V1 安全默认：advanced 缺失或键缺失一律按 false
      fingerprint: config.advanced[s]?.fingerprintRewrite === true,
      enabled: sourceEnabledFromRaw(config.raw, s),
    };
  }

  return { sources, throttleFields, danger, server, noticeVersion: config.noticeVersion };
}

/** dirty 判定：逐字段比较草稿与服务端基线（exhaust_402_until 为只读锚点，不参与）。 */
export function draftEquals(a: SourceDraft, b: SourceDraft): boolean {
  return (
    a.enabled === b.enabled &&
    a.fingerprint === b.fingerprint &&
    a.budget.steadyRpm === b.budget.steadyRpm &&
    a.budget.burstRpm === b.budget.burstRpm &&
    a.budget.dailyCap === b.budget.dailyCap &&
    a.budget.inflight === b.budget.inflight &&
    a.budget.minGapMs[0] === b.budget.minGapMs[0] &&
    a.budget.minGapMs[1] === b.budget.minGapMs[1] &&
    a.budget.jitterMinutes === b.budget.jitterMinutes &&
    a.budget.halveBudgetAfter429 === b.budget.halveBudgetAfter429 &&
    a.budget.backoff.base429Sec === b.budget.backoff.base429Sec &&
    a.budget.backoff.notFoundSec === b.budget.backoff.notFoundSec
  );
}
