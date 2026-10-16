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
  /** 状态副行（M10 wire status_note；如 "429 rate limit"）。空串 = 无，UI 不渲染副行。 */
  statusNote: string;
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
  /** 已暂停选号数（M10 wire paused_count；缺失按 0，不丢卡）。 */
  pausedCount: number;
  /** 粘性会话数（M10 wire sticky_sessions；缺失按 0）。 */
  stickySessions: number;
  /** 积分剩余和（M10 wire credit_sum；与 credit_total=总额 组成 KPI 双值；缺失 → null → 显示 —）。 */
  creditSum: number | null;
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
        // M10：状态副行——缺失/非串按空串（= 无副行），不丢条目
        statusNote: typeof item['status_note'] === 'string' ? item['status_note'] : '',
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
    // M10 汇总：计数缺失按 0；credit_sum 可空（缺失 → KPI 显示 —）
    pausedCount: count('paused_count'),
    stickySessions: count('sticky_sessions'),
    creditSum: numOrNull(body['credit_sum']),
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

/**
 * 到期时间双值（M10：绝对日期 + 相对天数，wb2api「17 小时前」式同源换算）：
 * date = 本地 YYYY-MM-DD；rel = N 天后 / 今天到期 / 今天过期 / 已过期 N 天（负数天不显示负号）。
 * null/0 → null（调用方显示 —，不编造时间）。
 */
export function expiryLabel(unix: number | null): { date: string; rel: string } | null {
  if (unix === null || unix === 0) return null;
  const d = new Date(unix * 1000);
  if (Number.isNaN(d.getTime())) return null;
  const pad = (n: number): string => String(n).padStart(2, '0');
  const date = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const diffMs = unix * 1000 - Date.now();
  const days = Math.floor(diffMs / 86_400_000);
  let rel: string;
  if (days >= 1) rel = `${days} 天后`;
  else if (diffMs > 0) rel = '今天到期';
  else if (diffMs > -86_400_000) rel = '今天过期';
  else rel = `已过期 ${-days} 天`;
  return { date, rel };
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

// ---------- 批量操作 / 启停 / 解锁 / 积分实时查询（M10-C，wb2api 对齐） ----------

/** 批量操作闭集（wire {"op":...}）：travel 仅 workbuddy 上游有语义，UI 按平台渲染。 */
export type BatchOpKind = 'checkin' | 'travel' | 'active' | 'keepalive';

/** 单账号失败明细（逐账号原样上抛，UI 可折叠展示）。 */
export interface BatchOpFailure {
  id: string;
  error: string;
}

/** POST /api/accounts/{platform}/batch-op → {requested, done, skipped, failed:[{id,error}]}。 */
export interface BatchOpResult {
  requested: number;
  done: number;
  skipped: number;
  failed: BatchOpFailure[];
}

/**
 * 平台级批量操作：POST /api/accounts/{platform}/batch-op body {"op":"checkin"|"travel"|"active"|"keepalive"}。
 * 计数缺失按 0；failed 逐条防御（缺 id/error 的条目丢弃），{error} 信封逐字上抛。
 */
export async function batchOp(platform: string, op: BatchOpKind): Promise<BatchOpResult> {
  const res = await fetch(`/api/accounts/${encodeURIComponent(platform)}/batch-op`, {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ op }),
  });
  let parsed: unknown;
  try {
    parsed = await res.json();
  } catch {
    throw new Error(`HTTP ${res.status}`);
  }
  if (!res.ok) {
    const msg = isObject(parsed) && typeof parsed['error'] === 'string' ? parsed['error'] : `HTTP ${res.status}`;
    throw new Error(msg);
  }
  if (!isObject(parsed)) throw new Error('batch-op: 响应非对象');
  const failed: BatchOpFailure[] = [];
  const rawFailed = Array.isArray(parsed['failed']) ? parsed['failed'] : [];
  for (const item of rawFailed) {
    if (!isObject(item)) continue;
    if (typeof item['id'] !== 'string' || typeof item['error'] !== 'string') continue;
    failed.push({ id: item['id'], error: item['error'] });
  }
  return {
    requested: numOrZero(parsed['requested']),
    done: numOrZero(parsed['done']),
    skipped: numOrZero(parsed['skipped']),
    failed,
  };
}

/**
 * 账号启停（M10）：POST /api/accounts/{platform}:{id}/disable 与 /enable。
 * 与 pause/resume 正交：disable = 管理员停用（凭证保留，仅退出选号）。
 * 响应不参与判定，成功一律以刷新后的快照为准。
 */
export function setAccountDisabled(platform: string, id: string, disable: boolean): Promise<void> {
  return apiPostEmpty(
    `/api/accounts/${encodeURIComponent(platform)}:${encodeURIComponent(id)}/${disable ? 'disable' : 'enable'}`,
  );
}

/**
 * 模型锁解锁（M10）：POST /api/model-locks/unlock。
 * model = null → body {}（表头「全部解锁」）；否则 body {"model":...}（行级「全池解锁」）。
 * {error} 信封逐字上抛；成功响应形状不参与判定（以刷新后的锁表为准）。
 */
export async function unlockModelLocks(model: string | null): Promise<void> {
  const res = await fetch('/api/model-locks/unlock', {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: model === null ? '{}' : JSON.stringify({ model }),
  });
  if (res.ok) return;
  let msg = `HTTP ${res.status}`;
  try {
    const body = (await res.json()) as { error?: unknown };
    if (typeof body?.error === 'string' && body.error !== '') msg = body.error;
  } catch {
    // 非 JSON 响应：保留 HTTP 码
  }
  throw new Error(msg);
}

/** POST /api/packages/refresh → {refreshed, errors:[{id,error}]}（积分实时查询上游）。 */
export interface PackagesRefreshResult {
  refreshed: number;
  errors: BatchOpFailure[];
}

/** POST /api/packages/refresh：即时逐账号查询上游积分批次。计数缺失按 0，errors 逐条防御。 */
export async function refreshPackages(): Promise<PackagesRefreshResult> {
  const res = await fetch('/api/packages/refresh', {
    method: 'POST',
    headers: { ...authHeaders(), 'Content-Type': 'application/json' },
    body: '{}',
  });
  let parsed: unknown;
  try {
    parsed = await res.json();
  } catch {
    throw new Error(`HTTP ${res.status}`);
  }
  if (!res.ok) {
    const msg = isObject(parsed) && typeof parsed['error'] === 'string' ? parsed['error'] : `HTTP ${res.status}`;
    throw new Error(msg);
  }
  if (!isObject(parsed)) throw new Error('packages/refresh: 响应非对象');
  const errors: BatchOpFailure[] = [];
  const rawErrors = Array.isArray(parsed['errors']) ? parsed['errors'] : [];
  for (const item of rawErrors) {
    if (!isObject(item)) continue;
    if (typeof item['id'] !== 'string' || typeof item['error'] !== 'string') continue;
    errors.push({ id: item['id'], error: item['error'] });
  }
  return { refreshed: numOrZero(parsed['refreshed']), errors };
}
