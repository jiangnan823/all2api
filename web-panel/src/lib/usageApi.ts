/**
 * 请求记录 / 用量 KPI 的 API 客户端（M8-reqlog）。端点与 wire 键为本批冻结契约：
 *
 *   GET /api/requests?limit=300&result=all|success|error
 *     → { requests:[{ts:{unix,local}, result, model, platform, account_id,
 *         latency_ms, prompt_tokens, completion_tokens, request_id}] }（旧→新）
 *   GET /api/usage
 *     → { window, requests, fail, prompt_tokens, completion_tokens,
 *         by_platform:{<id>:{requests,fail}}, by_model:{...同构},
 *         accounts:{<shortId>:{requests,tokens,duration_sec,tok_per_sec}} }（M10 追加）
 *
 * 404/4xx 如实上抛（panelApi.ApiError），不硬编码假成功。解析沿用 poolApi 的
 * 防御式读数：可空字段缺失/形状不对 → null 而不丢条目；结果枚举未知值原样
 * 保留（组件按「未知」徽标渲染，不编造语义）。
 */

import { apiGet } from './panelApi';

/** 一次 /v1 请求的终态（冻结枚举；未知值如实透传展示）。 */
export type RequestResult = 'success' | 'error' | 'stream_error' | 'interrupted';

export interface RequestRow {
  /** 服务端时间戳：unix 秒用于排序/展示，local 串为网关本地时间。 */
  ts: { unix: number; local: string };
  result: RequestResult;
  /** 裸模型名（无 platform/ 前缀）。 */
  model: string;
  platform: string;
  /** 账号 uid 短码前 8；未到达选号即失败时为空串。 */
  accountId: string;
  latencyMs: number;
  /** 上游未携带 usage 时为 null（流式当前恒为 null，不造假数）。 */
  promptTokens: number | null;
  completionTokens: number | null;
  /** 上游 completion id；流中断在首个 chunk 之前时为 null。 */
  requestId: string | null;
}

export interface RequestsSnapshot {
  requests: RequestRow[];
}

/** 结果筛选档（UI 闭集，与后端 ?result= 一致）。 */
export type RequestResultFilter = 'all' | 'success' | 'error';

/** 用量聚合桶（"window":"process" = 本次运行口径，进程重启清零）。 */
export interface UsageBucket {
  requests: number;
  fail: number;
}

/**
 * 单账号用量（M10，GET /api/usage accounts 桶）：键 = 账号 ShortID（uid 前 8），
 * 账号表按短码 join。四个指标均可空——上游未回该键时以 "-" 占位（不造 0）。
 */
export interface AccountUsage {
  requests: number | null;
  tokens: number | null;
  /** 累计耗时（秒）。 */
  durationSec: number | null;
  /** 平均每秒 token。 */
  tokPerSec: number | null;
}

export interface UsageSnapshot {
  window: string;
  requests: number;
  fail: number;
  promptTokens: number;
  completionTokens: number;
  byPlatform: Record<string, UsageBucket>;
  byModel: Record<string, UsageBucket>;
  /** M10 accounts 桶：短码 → 单账号用量；缺失/形状不对的条目跳过（整体缺失 = {}）。 */
  accounts: Record<string, AccountUsage>;
}

export const REQUESTS_FETCH_LIMIT = 300;
export const REQUESTS_POLL_MS = 30_000;
export const USAGE_POLL_MS = 30_000;

/** GET /api/requests → RequestsSnapshot（404/4xx 如实上抛）。 */
export function fetchRequests(result: RequestResultFilter = 'all', limit: number = REQUESTS_FETCH_LIMIT): Promise<RequestsSnapshot> {
  return apiGet(`/api/requests?limit=${limit}&result=${result}`, parseRequestsSnapshot);
}

/** GET /api/usage → UsageSnapshot（404/4xx 如实上抛）。 */
export function fetchUsage(): Promise<UsageSnapshot> {
  return apiGet('/api/usage', parseUsageSnapshot);
}

/**
 * 账号表 join（M10）：先按全量 id 直配，再回落 ShortID（前 8 位，reqlog account_id 同一口径）；
 * 都无 → null（用量列四徽章全 "-"）。快照缺失（usage 未拉到）同样返回 null。
 */
export function accountUsageFor(usage: UsageSnapshot | null, accountId: string): AccountUsage | null {
  if (!usage) return null;
  return usage.accounts[accountId] ?? usage.accounts[accountId.slice(0, 8)] ?? null;
}

// ---------- 蛇形 wire → 防御式解析（poolApi 同款纪律） ----------

function isObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

function numOrNull(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

function numOrZero(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : 0;
}

function strOrEmpty(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

function strOrNull(v: unknown): string | null {
  return typeof v === 'string' && v !== '' ? v : null;
}

/** 结果枚举：冻结四值之外的原样透传（组件按未知徽标渲染，不丢行）。 */
function resultOrRaw(v: unknown): RequestResult {
  return typeof v === 'string' && v !== '' ? (v as RequestResult) : 'error';
}

function parseTs(v: unknown): RequestRow['ts'] {
  const raw = isObject(v) ? v : {};
  return { unix: numOrNull(raw['unix']) ?? 0, local: strOrEmpty(raw['local']) };
}

export function parseRequestsSnapshot(body: unknown): RequestsSnapshot {
  if (!isObject(body)) throw new Error('requests: 响应非对象');
  const rawList = Array.isArray(body['requests']) ? body['requests'] : [];
  const requests: RequestRow[] = [];
  for (const item of rawList) {
    if (!isObject(item)) continue;
    requests.push({
      ts: parseTs(item['ts']),
      result: resultOrRaw(item['result']),
      model: strOrEmpty(item['model']),
      platform: strOrEmpty(item['platform']),
      accountId: strOrEmpty(item['account_id']),
      latencyMs: numOrNull(item['latency_ms']) ?? 0,
      promptTokens: numOrNull(item['prompt_tokens']),
      completionTokens: numOrNull(item['completion_tokens']),
      requestId: strOrNull(item['request_id']),
    });
  }
  return { requests };
}

function parseBucket(v: unknown): UsageBucket {
  const raw = isObject(v) ? v : {};
  return { requests: numOrZero(raw['requests']), fail: numOrZero(raw['fail']) };
}

function parseBucketMap(v: unknown): Record<string, UsageBucket> {
  if (!isObject(v)) return {};
  const out: Record<string, UsageBucket> = {};
  for (const [k, raw] of Object.entries(v)) {
    out[k] = parseBucket(raw);
  }
  return out;
}

/** accounts 桶单条：键存在即入表，四个指标逐键可空（缺失/非数值 → null → UI "-" 占位）。 */
function parseAccountUsage(v: unknown): AccountUsage {
  const raw = isObject(v) ? v : {};
  return {
    requests: numOrNull(raw['requests']),
    tokens: numOrNull(raw['tokens']),
    durationSec: numOrNull(raw['duration_sec']),
    tokPerSec: numOrNull(raw['tok_per_sec']),
  };
}

function parseAccountUsageMap(v: unknown): Record<string, AccountUsage> {
  if (!isObject(v)) return {};
  const out: Record<string, AccountUsage> = {};
  for (const [k, raw] of Object.entries(v)) {
    out[k] = parseAccountUsage(raw);
  }
  return out;
}

export function parseUsageSnapshot(body: unknown): UsageSnapshot {
  if (!isObject(body)) throw new Error('usage: 响应非对象');
  return {
    window: strOrEmpty(body['window']),
    requests: numOrZero(body['requests']),
    fail: numOrZero(body['fail']),
    promptTokens: numOrZero(body['prompt_tokens']),
    completionTokens: numOrZero(body['completion_tokens']),
    byPlatform: parseBucketMap(body['by_platform']),
    byModel: parseBucketMap(body['by_model']),
    accounts: parseAccountUsageMap(body['accounts']),
  };
}
