import type {
  ConfigGroup,
  ConfigPatch,
  ConfigSaveResponse,
  FieldDef,
  FieldEffect,
  FieldError,
  PanelConfig,
  ServerTimeResponse,
  SourceAdvanced,
  ThrottleBudget,
} from '../types/api-contract';

/**
 * 面板 API 客户端（M2 接线批）。端点：GET /api/config、/api/config/fields、/api/time，
 * POST /api/config（保存流，契约 v0.5.1 ConfigPatch / ConfigSaveResponse）。
 * wire key 全蛇形（契约 v0.5.1 §1 / 后端 internal/api），本文件负责蛇形 JSON → 契约 camelCase
 * 类型的唯一换算点；组件与模型层不再见到任何原始蛇形键。
 * 如实边界：保存端点后端并行开发中——404/501 等运行期错误原样上抛，不硬编码假成功。
 */

export class ApiError extends Error {
  readonly status?: number;

  constructor(message: string, status?: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

/**
 * X-API-Key 透传钩子：后端配置了 api_key 时 /api/* 要求该头（或 Bearer）；
 * 未配置时网关即开放模式。面板侧约定同一语义——provider 返回空串/空白即不带头。
 * key 只存内存态（设置屏接线后注入此 provider），面板不落任何 storage（M1 安全红线）。
 */
let apiKeyProvider: () => string = () => '';

export function setApiKeyProvider(provider: () => string): void {
  apiKeyProvider = provider;
}

export function authHeaders(): Record<string, string> {
  const key = apiKeyProvider().trim();
  return key === '' ? {} : { 'X-API-Key': key };
}

async function apiFetch<T>(path: string, method: string, body: string | undefined, parse: (body: unknown) => T): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      headers: { ...authHeaders(), ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
      ...(body !== undefined ? { body } : {}),
    });
  } catch {
    throw new ApiError('network error');
  }
  if (!res.ok) {
    throw new ApiError(`HTTP ${res.status}`, res.status);
  }
  let parsed: unknown;
  try {
    parsed = await res.json();
  } catch {
    throw new ApiError('response is not JSON', res.status);
  }
  return parse(parsed);
}

export function apiGet<T>(path: string, parse: (body: unknown) => T): Promise<T> {
  return apiFetch(path, 'GET', undefined, parse);
}

// ---------- 蛇形 wire → 契约类型的防御式解析（形状不对即抛 ApiError，不静默兜底） ----------

function asObject(v: unknown, where: string): Record<string, unknown> {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) {
    throw new ApiError(`config: ${where} 非对象`);
  }
  return v as Record<string, unknown>;
}

function reqNumber(v: unknown, where: string): number {
  if (typeof v !== 'number' || !Number.isFinite(v)) {
    throw new ApiError(`config: ${where} 非数值`);
  }
  return v;
}

function reqString(v: unknown, where: string): string {
  if (typeof v !== 'string' || v === '') {
    throw new ApiError(`config: ${where} 非字符串`);
  }
  return v;
}

function optNumber(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function optBool(v: unknown): boolean | undefined {
  return typeof v === 'boolean' ? v : undefined;
}

function boolOr(v: unknown, fallback: boolean): boolean {
  return typeof v === 'boolean' ? v : fallback;
}

function parseThrottleBudgetWire(v: unknown, source: string): ThrottleBudget {
  const o = asObject(v, `throttle.${source}`);
  const backoff = asObject(o['backoff'], `throttle.${source}.backoff`);
  const gap = Array.isArray(o['min_gap_ms']) ? o['min_gap_ms'] : [];
  // 只读锚点：契约冻结唯一取值（402 红线的重置时刻），出现第二种值即视为形状异常
  if (backoff['exhaust_402_until'] !== 'next-04:00') {
    throw new ApiError(`config: throttle.${source}.backoff.exhaust_402_until 非法`);
  }
  return {
    steadyRpm: reqNumber(o['steady_rpm'], `throttle.${source}.steady_rpm`),
    burstRpm: reqNumber(o['burst_rpm'], `throttle.${source}.burst_rpm`),
    minGapMs: [
      reqNumber(gap[0], `throttle.${source}.min_gap_ms[0]`),
      reqNumber(gap[1], `throttle.${source}.min_gap_ms[1]`),
    ],
    dailyCap: reqNumber(o['daily_cap'], `throttle.${source}.daily_cap`),
    inflight: reqNumber(o['inflight'], `throttle.${source}.inflight`),
    backoff: {
      base429Sec: reqNumber(backoff['base_429_sec'], `throttle.${source}.backoff.base_429_sec`),
      notFoundSec: reqNumber(backoff['not_found_sec'], `throttle.${source}.backoff.not_found_sec`),
      exhaust402Until: 'next-04:00',
    },
    jitterMinutes: optNumber(o['jitter_minutes']),
    halveBudgetAfter429: optBool(o['halve_budget_after_429']),
  };
}

/** GET /api/config → PanelConfig（后端 panelConfigResponse：raw / feature_flags / throttle / advanced / notice_version）。 */
export function parsePanelConfig(body: unknown): PanelConfig {
  const o = asObject(body, '(root)');
  const flags = asObject(o['feature_flags'], 'feature_flags');

  const throttleWire = asObject(o['throttle'], 'throttle');
  const throttle: Record<string, ThrottleBudget> = {};
  for (const [source, v] of Object.entries(throttleWire)) {
    throttle[source] = parseThrottleBudgetWire(v, source);
  }

  const advancedWire = asObject(o['advanced'], 'advanced');
  const advanced: Record<string, SourceAdvanced> = {};
  for (const [source, v] of Object.entries(advancedWire)) {
    const entry = asObject(v, `advanced.${source}`);
    // F-V1 安全默认：键缺失一律按 false
    advanced[source] = { fingerprintRewrite: boolAt(entry, 'request_fingerprint_rewrite') };
  }

  return {
    raw: asObject(o['raw'], 'raw'),
    featureFlags: {
      responsesApi: boolAt(flags, 'responses_api'),
      sessionStickiness: boolAt(flags, 'session_stickiness'),
      redisMirror: boolAt(flags, 'redis_mirror'),
      debugBodySampling: boolAt(flags, 'debug_body_sampling'),
    },
    throttle,
    advanced,
    noticeVersion: optString(o['notice_version']) ?? '',
  };
}

function boolAt(o: Record<string, unknown>, key: string): boolean {
  return boolOr(o[key], false);
}

function optString(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

const FIELD_TYPES: ReadonlySet<string> = new Set(['number', 'string', 'boolean', 'cron', 'select']);
const FIELD_EFFECTS: ReadonlySet<string> = new Set(['immediate', 'next_request', 'restart_required']);

function parseFieldDef(v: unknown, where: string): FieldDef {
  const o = asObject(v, where);
  const type = reqString(o['type'], `${where}.type`);
  const effect = reqString(o['effect'], `${where}.effect`);
  if (!FIELD_TYPES.has(type)) throw new ApiError(`config: ${where}.type 未知 ${type}`);
  if (!FIELD_EFFECTS.has(effect)) throw new ApiError(`config: ${where}.effect 未知 ${effect}`);
  const unit = optString(o['unit']);
  const description = optString(o['description']);
  const options = Array.isArray(o['options']) && o['options'].every((x) => typeof x === 'string')
    ? (o['options'] as string[])
    : undefined;
  return {
    key: reqString(o['key'], `${where}.key`),
    label: reqString(o['label'], `${where}.label`),
    type: type as FieldDef['type'],
    unit: unit === '' ? undefined : unit,
    min: optNumber(o['min']),
    max: optNumber(o['max']),
    options,
    description: description === '' ? undefined : description,
    effect: effect as FieldEffect,
    requiresConfirm: boolAt(o, 'requires_confirm'),
  };
}

/** GET /api/config/fields → ConfigGroup[]（wire 的 requires_confirm 蛇形键 → 契约 requiresConfirm）。 */
export function parseConfigGroups(body: unknown): ConfigGroup[] {
  if (!Array.isArray(body)) throw new ApiError('config: fields 响应非数组');
  return body.map((g, i) => {
    const o = asObject(g, `fields[${i}]`);
    const fields = Array.isArray(o['fields']) ? o['fields'] : [];
    return {
      group: reqString(o['group'], `fields[${i}].group`),
      fields: fields.map((f, j) => parseFieldDef(f, `fields[${i}].fields[${j}]`)),
    };
  });
}

/** GET /api/time → ServerTimeResponse（UI §5.7/§5.8：倒计时与过期判定的唯一 now 来源）。 */
export function parseServerTime(body: unknown): ServerTimeResponse {
  const o = asObject(body, 'time');
  return {
    now: reqNumber(o['now'], 'time.now'),
    local: reqString(o['local'], 'time.local'),
    timezone: reqString(o['timezone'], 'time.timezone'),
  };
}

// ---------- 四个端点的具名入口（面板各屏只 import 这四个） ----------

export function fetchPanelConfig(): Promise<PanelConfig> {
  return apiGet('/api/config', parsePanelConfig);
}

export function fetchConfigGroups(): Promise<ConfigGroup[]> {
  return apiGet('/api/config/fields', parseConfigGroups);
}

export function fetchServerTime(): Promise<ServerTimeResponse> {
  return apiGet('/api/time', parseServerTime);
}

/** ConfigSaveResponse 的防御式解析：ok 必须是 boolean（服务端裁决位，缺失即形状异常）。 */
function parseConfigSaveResponse(body: unknown): ConfigSaveResponse {
  const o = asObject(body, 'save');
  if (typeof o['ok'] !== 'boolean') {
    throw new ApiError('config: save.ok 非 boolean');
  }
  const fieldErrors = Array.isArray(o['field_errors'])
    ? (o['field_errors'] as unknown[]).map((e, i) => {
        const fe = asObject(e, `save.field_errors[${i}]`);
        // code 为机器可读稳定标识（range / consent_missing / consent_stale），未知值保留原样透传
        const code = optString(fe['code']);
        return {
          key: reqString(fe['key'], `save.field_errors[${i}].key`),
          message: reqString(fe['message'], `save.field_errors[${i}].message`),
          ...(code === undefined ? {} : { code: code as FieldError['code'] }),
        };
      })
    : undefined;
  return {
    ok: o['ok'],
    applied: o['applied'] === undefined ? undefined : parsePanelConfig(o['applied']),
    fieldErrors,
  };
}

/**
 * POST /api/config → ConfigSaveResponse（保存流）。
 * 契约语义：200 即请求被受理，成功/拒绝由 body.ok 与 fieldErrors 表达（逐字段红字通道）；
 * 401 = key 不符（ApiError.status），404/501 = 后端端点未部署（并行开发中，如实上抛展示）。
 */
export function savePanelConfig(patch: ConfigPatch): Promise<ConfigSaveResponse> {
  return apiFetch('/api/config', 'POST', JSON.stringify(patch), parseConfigSaveResponse);
}

/** POST /api/config/restore-defaults（v1.2：一键恢复出厂默认，保留连接四项）。 */
export function restoreDefaults(): Promise<unknown> {
  return apiFetch('/api/config/restore-defaults', 'POST', undefined, (b) => b);
}
