/**
 * 成长任务 API 客户端（M7b，wb2api 同款「积分任务」弹层的数据面）。
 * 端点由后端并行批交付，运行期 404/501 如实上抛展示，不硬编码假成功。
 *
 * TODO(contract): 以下 wire 形状按 M7b 冻结契约书写，键名逐字对齐，字段名以并行批实际回报为准，
 * 对齐差异统一在此文件的 parse 函数内收口（组件只见 camelCase 契约类型）：
 *   GET  /api/accounts/{platform}:{id}/growth-tasks
 *     → 200 {source:"upstream", tasks:[{code,title,progress_done,progress_total,reward_credit,reward_energy,status}]}
 *       status ∈ claimed | claimable | completed | in_progress | accepted | not_accepted
 *     → 502 {error:"上游失败原因"}（逐字显示）
 *   POST /api/accounts/{platform}:{id}/growth-accept       body {} → {accepted:N}
 *   POST /api/accounts/{platform}:{id}/growth-claim-auto   body {} → {claimed:N,credit_gained:N}
 *   失败 = 4xx/502 + {error:"中文原因"}，逐字上抛（后端保证不回显凭证）。
 */

import { ApiError, authHeaders } from './panelApi';

/** 单条成长任务（camelCase 契约面）。全部字段可空防御：缺失/形状不对 → null，条目不丢。 */
export interface GrowthTask {
  /** 上游任务标识（身份键；wire 缺失该键的条目整条丢弃，不静默造数）。 */
  code: string;
  /** 展示名；wire 缺失时回落为 code（展示层永远有内容可显示）。 */
  title: string;
  progressDone: number | null;
  progressTotal: number | null;
  rewardCredit: number | null;
  rewardEnergy: number | null;
  /** 服务端原始状态串（六值闭集之外的原样透传显示，不翻译瞎猜）；缺失 → 空串。 */
  status: string;
}

export interface GrowthTasksSnapshot {
  /** wire source（如 "upstream"）；缺失为 null（展示层不消费，仅契约保真）。 */
  source: string | null;
  tasks: GrowthTask[];
}

export interface GrowthAcceptResult {
  accepted: number;
}

export interface GrowthClaimAutoResult {
  claimed: number;
  creditGained: number;
}

function isObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

function numOrNull(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

/** 计数字段防御式读数：缺失/非数值按 0（accepted/claimed/credit_gained 永远给 UI 一个数）。 */
function numOrZero(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : 0;
}

function strOrNull(v: unknown): string | null {
  return typeof v === 'string' && v !== '' ? v : null;
}

/** 成长任务路径：/api/accounts/{platform}:{id}/{action}（platform/id 逐段编码，冒号为字面分隔符）。 */
function growthPath(platform: string, id: string, action: 'growth-tasks' | 'growth-accept' | 'growth-claim-auto'): string {
  return `/api/accounts/${encodeURIComponent(platform)}:${encodeURIComponent(id)}/${action}`;
}

/**
 * 三端点共用通道：GET / POST(body={})。
 * - 4xx/502 一律先解 {error} 信封，原文逐字抛 ApiError（message = 原文，不包装不翻译）；
 *   信封缺失/非 JSON 回落 `HTTP {status}`；status 随 ApiError 保留供调用方分档。
 * - 2xx 非 JSON / 非对象 = 形状异常，抛 ApiError（不静默兜底假成功）。
 */
async function growthRequest(path: string, method: 'GET' | 'POST'): Promise<Record<string, unknown>> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      headers: {
        ...authHeaders(),
        // body 恒为 {}：契约未定义请求体，给解码器一个合法空对象，防后端 JSON Decode EOF
        ...(method === 'POST' ? { 'Content-Type': 'application/json' } : {}),
      },
      ...(method === 'POST' ? { body: '{}' } : {}),
    });
  } catch {
    throw new ApiError('network error');
  }
  let parsed: unknown;
  try {
    parsed = await res.json();
  } catch {
    throw new ApiError(`HTTP ${res.status}`, res.status);
  }
  if (!res.ok) {
    const msg =
      isObject(parsed) && typeof parsed['error'] === 'string' && parsed['error'] !== ''
        ? (parsed['error'] as string)
        : `HTTP ${res.status}`;
    throw new ApiError(msg, res.status);
  }
  if (!isObject(parsed)) throw new ApiError(`growth: ${path} 响应非对象`, res.status);
  return parsed;
}

/** GET growth-tasks wire → GrowthTasksSnapshot（防御式：数组内形状不对的条目整条丢弃）。 */
export function parseGrowthTasks(body: unknown): GrowthTasksSnapshot {
  const root = isObject(body) ? body : {};
  const rawList = Array.isArray(root['tasks']) ? root['tasks'] : [];
  const tasks: GrowthTask[] = [];
  for (const item of rawList) {
    if (!isObject(item)) continue;
    const code = strOrNull(item['code']);
    if (code === null) continue;
    tasks.push({
      code,
      title: strOrNull(item['title']) ?? code,
      progressDone: numOrNull(item['progress_done']),
      progressTotal: numOrNull(item['progress_total']),
      rewardCredit: numOrNull(item['reward_credit']),
      rewardEnergy: numOrNull(item['reward_energy']),
      status: typeof item['status'] === 'string' ? item['status'] : '',
    });
  }
  return { source: strOrNull(root['source']), tasks };
}

/** GET /api/accounts/{platform}:{id}/growth-tasks → 任务清单（404/502 如实上抛）。 */
export function fetchGrowthTasks(platform: string, id: string): Promise<GrowthTasksSnapshot> {
  return growthRequest(growthPath(platform, id, 'growth-tasks'), 'GET').then(parseGrowthTasks);
}

/** POST /api/accounts/{platform}:{id}/growth-accept → 全部接受（成功后调用方应刷新清单）。 */
export function acceptAll(platform: string, id: string): Promise<GrowthAcceptResult> {
  return growthRequest(growthPath(platform, id, 'growth-accept'), 'POST').then((body) => ({
    accepted: numOrZero(body['accepted']),
  }));
}

/** POST /api/accounts/{platform}:{id}/growth-claim-auto → 一键完成可自动任务（credit_gained 用于成功提示）。 */
export function claimAuto(platform: string, id: string): Promise<GrowthClaimAutoResult> {
  return growthRequest(growthPath(platform, id, 'growth-claim-auto'), 'POST').then((body) => ({
    claimed: numOrZero(body['claimed']),
    creditGained: numOrZero(body['credit_gained']),
  }));
}
