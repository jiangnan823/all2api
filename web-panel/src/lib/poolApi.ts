/**
 * 账号池 / 运行日志两屏的 API 客户端（M5b）。
 * 端点由后端并行批交付，运行期 404 如实上抛展示，不硬编码假成功。
 *
 * TODO(contract): 以下 wire 形状按本批任务书契约书写，字段名以并行批实际回报为准，
 * 对齐差异统一在此文件的 parse 函数内收口（组件只见 camelCase 契约类型）：
 *   GET /api/accounts → { accounts:[{platform,id,status,credit,health,expires_at,cooldown_end,last_selected}], total, active, cooling, disabled, credit_total }
 *   GET /api/logs?limit=200 → { logs:[{ts,level,line}] }（旧→新）
 */

export interface AccountEntry {
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

import { ApiError, apiGet } from './panelApi';

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
        status: str(item['status'], 'account.status'),
        credit: numOrNull(item['credit']),
        health: typeof item['health'] === 'string' && item['health'] !== '' ? item['health'] : null,
        expiresAt: numOrNull(item['expires_at']),
        cooldownEnd: numOrNull(item['cooldown_end']),
        lastSelected: numOrNull(item['last_selected']),
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
    const line = typeof item['line'] === 'string' ? item['line'] : null;
    if (line === null) continue;
    logs.push({
      ts: numOrNull(item['ts']),
      level: typeof item['level'] === 'string' && item['level'] !== '' ? item['level'] : 'other',
      line,
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
