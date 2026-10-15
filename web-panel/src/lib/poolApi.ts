/**
 * 账号池 / 运行日志两屏的 API 客户端（M5b）。
 * 端点由后端并行批交付，运行期 404 如实上抛展示，不硬编码假成功。
 *
 * TODO(contract): 以下 wire 形状按本批任务书契约书写，字段名以并行批实际回报为准，
 * 对齐差异统一在此文件的 parse 函数内收口（组件只见 camelCase 契约类型）：
 *   GET /api/accounts → { accounts:[{platform,id,status,credit,health,expires_at,cooldown_end,last_selected,
 *     success,fail,in_flight,paused,last_success,credit_total}], total, active, cooling, disabled, credit_total }
 *   POST /api/accounts/{platform}:{id}/pause|resume → {ok:true,paused:bool}
 *   POST /api/accounts/{platform}:{id}/remove → {ok:true}（后端删凭证文件，前端删除前 confirm）
 *   失败 = 4xx + {error:"中文原因"}，逐字上抛。
 *   GET /api/logs?limit=200 → { logs:[{ts,level,line}] }（旧→新）
 */

export interface AccountEntry {
  /** 显示名（workbuddy 凭证的 account.nickname；PAT 账号无）——空时 UI 回落到 id。 */
  nickname: string | null;
  platform: string;
  id: string;
  /** 服务端原始状态串（active / cooling / disabled 等），徽色按 statusTone 映射，未知值如实显示。 */
  status: string;
  credit: number | null;
  health: string | null;
  /** Unix 秒；缺失/0 = 无到期概念。 */
  expiresAt: number | null;
  /** Unix 秒；缺失/0 = 不在冷却。 */
  cooldownEnd: number | null;
  lastSelected: number | null;
  /** 本次运行成功数（M6b wire success；缺失按 0）。 */
  success: number;
  /** 本次运行失败数（M6b wire fail；缺失按 0）。 */
  fail: number;
  /** 当前在途请求数（M6b wire in_flight；缺失按 0）。 */
  inFlight: number;
  /** 选号暂停开关（M6b wire paused；缺失按 false，判定优先于 status）。 */
  paused: boolean;
  /** 最近一次成功时间（M6b wire last_success:{unix,local}|null → Unix 秒）。 */
  lastSuccess: number | null;
  /** 上游配额总额（M6b wire credit_total；未知/未播种为 0，与缺失同义 → 显示 —）。 */
  creditTotal: number | null;
}

export interface AccountsSnapshot {
  accounts: AccountEntry[];
  total: number;
  active: number;
  cooling: number;
  disabled: number;
  creditTotal: number | null;
}

export interface LogEntry {
  /** Unix 秒；服务端缺失时面板显示 — 而不编造时间。 */
  ts: number | null;
  level: string;
  line: string;
}

export interface LogsSnapshot {
  logs: LogEntry[];
}

/** 级别过滤档（UI 闭集）：other 兜底任一未知级别值。 */
export type LogLevelFilter = 'all' | 'info' | 'error';

import { ApiError, apiGet, authHeaders } from './panelApi';

export const LOGS_FETCH_LIMIT = 200;
export const ACCOUNTS_POLL_MS = 30_000;
export const LOGS_POLL_MS = 10_000;

/** GET /api/accounts → AccountsSnapshot（后端并行批交付；404 如实上抛）。 */
export function fetchAccounts(): Promise<AccountsSnapshot> {
  return apiGet('/api/accounts', parseAccountsSnapshot);
}

/** GET /api/logs?limit=N → LogsSnapshot（旧→新；404 如实上抛）。 */
export function fetchLogs(limit: number = LOGS_FETCH_LIMIT): Promise<LogsSnapshot> {
  return apiGet(`/api/logs?limit=${limit}`, parseLogsSnapshot);
}

/** 屏层错误归一：panelApi 的 ApiError message 已带 HTTP 码，直接透传展示。 */
export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError;
}

function isObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

function str(v: unknown, where: string): string {
  if (typeof v !== 'string' || v === '') throw new Error(`accounts: ${where} 非字符串`);
  return v;
}

function numOrNull(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

/** 计数字段（success/fail/in_flight）防御式读数：缺失/非数值按 0，不丢条目。 */
function numOrZero(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : 0;
}

/** 可空字符串：缺失/空串/非串 → null（昵称这类展示字段缺失是常态，不丢条目）。 */
function strOrNull(v: unknown): string | null {
  return typeof v === 'string' && v !== '' ? v : null;
}

/**
 * TimeLocal 双形态（{unix, local}）→ unix 秒。数字直通（兼容旧形态）；
 * 对象形态读 unix；其余 → null。到期/冷却列此前恒显示 — 就是因为把对象喂给了 numOrNull。
 */
function timeLocalToUnix(v: unknown): number | null {
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  if (isObject(v) && typeof v['unix'] === 'number' && Number.isFinite(v['unix'])) return v['unix'] as number;
  return null;
}

/** GET /api/accounts wire → AccountsSnapshot（防御式：数组内形状不对的条目整条丢弃，不静默造数）。 */
export function parseAccountsSnapshot(body: unknown): AccountsSnapshot {
  if (!isObject(body)) throw new Error('accounts: 响应非对象');
  const rawList = Array.isArray(body['accounts']) ? body['accounts'] : [];
  const accounts: AccountEntry[] = [];
  for (const item of rawList) {
    if (!isObject(item)) continue;
    try {
      accounts.push({
        platform: str(item['platform'], 'account.platform'),
        id: str(item['id'], 'account.id'),
        nickname: strOrNull(item['nickname']),
        status: str(item['status'], 'account.status'),
        credit: numOrNull(item['credit']),
        health: typeof item['health'] === 'string' && item['health'] !== '' ? item['health'] : null,
        expiresAt: timeLocalToUnix(item['expires_at']),
        cooldownEnd: timeLocalToUnix(item['cooldown_end']),
        lastSelected: timeLocalToUnix(item['last_selected']),
        // M6b 富列：全部可空防御——缺失不丢条目，计数按 0、暂停按 false、时间按 null
        success: numOrZero(item['success']),
        fail: numOrZero(item['fail']),
        inFlight: numOrZero(item['in_flight']),
        paused: item['paused'] === true,
        lastSuccess: timeLocalToUnix(item['last_success']),
        creditTotal: numOrNull(item['credit_total']),
      });
    } catch {
      // 单条畸形不拖垮整屏：丢弃该条（计数仍以服务端汇总字段为准）
    }
  }
  const count = (key: string): number =>
    typeof body[key] === 'number' && Number.isFinite(body[key]) ? (body[key] as number) : 0;
  const creditTotal = numOrNull(body['credit_total']);
  return {
    accounts,
    total: count('total'),
    active: count('active'),
    cooling: count('cooling'),
    disabled: count('disabled'),
    creditTotal,
  };
}

/** GET /api/logs wire → LogsSnapshot（同上防御式；ts/level 缺失按 null/'other' 兜底显示）。 */
export function parseLogsSnapshot(body: unknown): LogsSnapshot {
  if (!isObject(body)) throw new Error('logs: 响应非对象');
  const rawList = Array.isArray(body['logs']) ? body['logs'] : [];
  const logs: LogEntry[] = [];
  for (const item of rawList) {
    if (!isObject(item)) continue;
    // M5a wire：{level, msg, ts:{unix, local}}。msg 缺失丢条目；ts 取 unix 秒。
    const msg = typeof item['msg'] === 'string' ? item['msg'] : null;
    if (msg === null) continue;
    const tsObj = isObject(item['ts']) ? item['ts'] : {};
    logs.push({
      ts: numOrNull(tsObj['unix']),
      level: typeof item['level'] === 'string' && item['level'] !== '' ? item['level'] : 'other',
      line: msg,
    });
  }
  return { logs };
}

/** 状态串 → 徽色档（CSS 闭集五档 + idle 兜底；未知状态如实显示原文、不着色夸大）。 */
export function statusTone(status: string): 'ok' | 'warn' | 'err' | 'idle' {
  const s = status.toLowerCase();
  if (s === 'active' || s === 'ok' || s === 'available') return 'ok';
  if (s === 'cooling' || s === 'cooldown') return 'warn';
  if (s === 'disabled' || s === 'banned' || s === 'expired') return 'err';
  return 'idle';
}

/**
 * Unix 秒 → 相对时间（M6b「最近成功」列，纯前端换算）：
 * 刚刚（含未来时间/时钟偏移，不显示负数）/ N 秒前 / N 分钟前 / N 小时前 / N 天前。
 * null/0 → null（调用方显示 —，不编造时间）。
 */
export function relativeTime(unix: number | null): string | null {
  if (unix === null || unix === 0) return null;
  const s = Math.floor((Date.now() - unix * 1000) / 1000);
  if (s <= 0) return '刚刚';
  if (s < 60) return `${s} 秒前`;
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
  return `${Math.floor(s / 86400)} 天前`;
}

/** 日志级别 → 徽色档（error 及近似值红色，warn 档黄色，其余中性）。 */
export function logLevelTone(level: string): 'err' | 'warn' | 'idle' {
  const l = level.toLowerCase();
  if (l === 'error' || l === 'fatal' || l === 'err') return 'err';
  if (l === 'warn' || l === 'warning') return 'warn';
  return 'idle';
}

/** 过滤谓词：all 放行全部；info/error 只放行该级别（大小写不敏感，未知级别仅「全部」可见）。 */
export function logMatchesFilter(entry: LogEntry, filter: LogLevelFilter): boolean {
  if (filter === 'all') return true;
  return entry.level.toLowerCase() === filter;
}

// ---------- 添加账号（v1.2 §1a：面板内一等操作，文件只是落盘位置） ----------

export interface AddAccountResult {
  ok: boolean;
  platform: 'workbuddy' | 'qoder';
  id: string;
}

/** POST /api/accounts/workbuddy（body = 官方桌面端凭证 JSON 原文）。 */
export function addWorkbuddyAccount(rawJSON: string): Promise<AddAccountResult> {
  return apiPostJson('/api/accounts/workbuddy', rawJSON);
}

/** POST /api/accounts/qoder（body = {"pat":"pt-…"}）。 */
export function addQoderAccount(pat: string): Promise<AddAccountResult> {
  return apiPostJson('/api/accounts/qoder', JSON.stringify({ pat }));
}

/** 通用 POST + {error} 信封解析（后端拒绝为 400 + 中文原因）。 */
async function apiPostJson(path: string, body: string): Promise<AddAccountResult> {
  let res: Response;
  try {
    res = await fetch(path, {
      method: 'POST',
      headers: { ...authHeaders(), 'Content-Type': 'application/json' },
      body,
    });
  } catch {
    throw new ApiError('network error');
  }
  if (res.status === 401) throw new ApiError('HTTP 401', 401);
  let parsed: { ok?: boolean; id?: string; error?: string };
  try {
    parsed = await res.json();
  } catch {
    throw new ApiError(`HTTP ${res.status}`, res.status);
  }
  if (parsed.ok === true && typeof parsed.id === 'string') {
    return { ok: true, platform: path.endsWith('/qoder') ? 'qoder' : 'workbuddy', id: parsed.id };
  }
  throw new ApiError(parsed.error || `HTTP ${res.status}`, res.status);
}

// ---------- 账号行内操作（M6b）：pause / resume / remove ----------

export interface AccountActionResponse {
  ok: boolean;
  /** 服务端回显的暂停态（pause→true / resume→false）；响应缺失时为 null（以刷新后的快照为准）。 */
  paused: boolean | null;
}

/** 行内操作路径：/api/accounts/{platform}:{id}/{action}（platform/id 逐段编码，冒号为字面分隔符）。 */
function accountActionPath(platform: string, id: string, action: 'pause' | 'resume' | 'remove'): string {
  return `/api/accounts/${encodeURIComponent(platform)}:${encodeURIComponent(id)}/${action}`;
}

/**
 * POST pause/resume/remove 共用通道：成功 {ok:true}（可带 paused 回显）；
 * 失败 4xx + {error:"中文原因"} → ApiError 逐字上抛（message 即原文，不包装不翻译）。
 * body 恒为 {}：契约未定义请求体，给解码器一个合法空对象，防后端 JSON Decode EOF。
 */
async function postAccountAction(path: string): Promise<AccountActionResponse> {
  let res: Response;
  try {
    res = await fetch(path, {
      method: 'POST',
      headers: { ...authHeaders(), 'Content-Type': 'application/json' },
      body: '{}',
    });
  } catch {
    throw new ApiError('network error');
  }
  let parsed: { ok?: unknown; paused?: unknown; error?: unknown };
  try {
    parsed = await res.json();
  } catch {
    throw new ApiError(`HTTP ${res.status}`, res.status);
  }
  if (parsed['ok'] === true) {
    return { ok: true, paused: typeof parsed['paused'] === 'boolean' ? (parsed['paused'] as boolean) : null };
  }
  const msg = typeof parsed['error'] === 'string' && parsed['error'] !== '' ? (parsed['error'] as string) : `HTTP ${res.status}`;
  throw new ApiError(msg, res.status);
}

/** POST /api/accounts/{platform}:{id}/pause → 暂停选号（保活/余额不受影响，语义以后端为准）。 */
export function pauseAccount(platform: string, id: string): Promise<AccountActionResponse> {
  return postAccountAction(accountActionPath(platform, id, 'pause'));
}

/** POST /api/accounts/{platform}:{id}/resume → 恢复选号。 */
export function resumeAccount(platform: string, id: string): Promise<AccountActionResponse> {
  return postAccountAction(accountActionPath(platform, id, 'resume'));
}

/** POST /api/accounts/{platform}:{id}/remove → 删除账号（后端删凭证文件；前端必须先 confirm）。 */
export function removeAccount(platform: string, id: string): Promise<AccountActionResponse> {
  return postAccountAction(accountActionPath(platform, id, 'remove'));
}

/** POST /api/accounts/{platform}:{id}/checkin — 手动签到（上游幂等）。 */
export function checkinAccount(platform: string, id: string): Promise<void> {
  return apiPostEmpty(`/api/accounts/${encodeURIComponent(platform)}:${encodeURIComponent(id)}/checkin`);
}

/** POST /api/accounts/{platform}:{id}/balance — 立即重查上游余额并回写池。 */
export function refreshAccountBalance(platform: string, id: string): Promise<void> {
  return apiPostEmpty(`/api/accounts/${encodeURIComponent(platform)}:${encodeURIComponent(id)}/balance`);
}

/** POST 空体 + {error} 信封逐字上抛。 */
async function apiPostEmpty(path: string): Promise<void> {
  const res = await fetch(path, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: '{}',
  });
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) msg = body.error;
    } catch {
      // 非 JSON 响应：保留 HTTP 码
    }
    throw new Error(msg);
  }
}
