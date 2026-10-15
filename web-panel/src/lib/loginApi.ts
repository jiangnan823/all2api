/**
 * 浏览器登录 API 客户端（M8，WorkBuddy 设备授权流的面板数据面）。
 * wire 为后端冻结契约，键名逐字对齐；{error} 信封与网络错误如实上抛，
 * 不硬编码假成功。凭证材料只经后端落盘，本文件不触碰任何 token。
 *
 *   POST /api/accounts/workbuddy/login-start  body {}
 *     → 200 {"state":"…","login_url":"https://…授权链接"}
 *     → 502 {"error":"…"}（上游失败，逐字显示）
 *   GET  /api/accounts/workbuddy/login-poll?state=…
 *     → 200 {"status":"pending"}                        继续轮询（3s）
 *     → 200 {"status":"done","uid":"…","nickname":"…"}  凭证已落盘入池
 *     → 200 {"status":"expired"}                        state 未知/过期/已消费
 *     → 200 {"status":"error","error":"…"}              登录完成但身份解析/落盘失败
 */

import { ApiError, authHeaders } from './panelApi';

/** 轮询节奏（M8 冻结口径：3 秒）。 */
export const LOGIN_POLL_MS = 3_000;

export interface LoginStartResult {
  state: string;
  loginUrl: string;
}

/** login-poll 四态闭集（冻结 wire）；未知 status 视为形状异常上抛，不静默兜底。 */
export type LoginPollResult =
  | { status: 'pending' }
  | { status: 'done'; uid: string; nickname: string }
  | { status: 'expired' }
  | { status: 'error'; error: string };

function isObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

/** 4xx/502 的 {error} 信封原文（缺失/空串回落 HTTP 码，不编造原因）。 */
function envelopeError(parsed: unknown, status: number): string {
  if (isObject(parsed)) {
    const msg = parsed['error'];
    if (typeof msg === 'string' && msg !== '') return msg;
  }
  return `HTTP ${status}`;
}

async function parseJson(res: Response): Promise<unknown> {
  try {
    return await res.json();
  } catch {
    throw new ApiError(`HTTP ${res.status}`, res.status);
  }
}

/** POST /api/accounts/workbuddy/login-start（body 恒为 {}）→ {state, loginUrl}。 */
export async function startWorkbuddyLogin(): Promise<LoginStartResult> {
  let res: Response;
  try {
    res = await fetch('/api/accounts/workbuddy/login-start', {
      method: 'POST',
      headers: { ...authHeaders(), 'Content-Type': 'application/json' },
      body: '{}',
    });
  } catch {
    throw new ApiError('network error');
  }
  const parsed = await parseJson(res);
  if (!res.ok) throw new ApiError(envelopeError(parsed, res.status), res.status);
  if (!isObject(parsed) || typeof parsed['state'] !== 'string' || parsed['state'] === '' ||
    typeof parsed['login_url'] !== 'string' || parsed['login_url'] === '') {
    throw new ApiError('login: login-start 响应形状异常', res.status);
  }
  return { state: parsed['state'], loginUrl: parsed['login_url'] };
}

/** GET /api/accounts/workbuddy/login-poll?state=… → 四态闭集。 */
export async function pollWorkbuddyLogin(state: string): Promise<LoginPollResult> {
  let res: Response;
  try {
    res = await fetch(`/api/accounts/workbuddy/login-poll?state=${encodeURIComponent(state)}`, {
      headers: authHeaders(),
    });
  } catch {
    throw new ApiError('network error');
  }
  const parsed = await parseJson(res);
  if (!res.ok) throw new ApiError(envelopeError(parsed, res.status), res.status);
  if (!isObject(parsed)) throw new ApiError('login: login-poll 响应非对象', res.status);
  const status = parsed['status'];
  if (status === 'pending') return { status: 'pending' };
  if (status === 'expired') return { status: 'expired' };
  if (status === 'done') {
    if (typeof parsed['uid'] !== 'string' || parsed['uid'] === '') {
      throw new ApiError('login: done 缺少 uid', res.status);
    }
    // 昵称是展示字段：缺失/非串回落空串（UI 回落 uid 短码），不丢完成态。
    const nickname = typeof parsed['nickname'] === 'string' ? parsed['nickname'] : '';
    return { status: 'done', uid: parsed['uid'], nickname };
  }
  if (status === 'error') {
    return { status: 'error', error: envelopeError(parsed, res.status) };
  }
  throw new ApiError(`login: 未知 status ${String(status)}`, res.status);
}
