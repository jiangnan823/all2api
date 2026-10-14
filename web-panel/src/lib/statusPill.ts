import type { ServerTimeResponse } from '../types/api-contract';
import { fetchServerTime } from './panelApi';

/**
 * topbar 状态 pill 的接线状态机（M2 第二批）。
 * 判定面（契约 v0.5.1）：
 * - GET /api/time 成功 = 服务连通（同一 /api/* 面、withAPIKey 同门；契约注释：面板的时钟基准）。
 * - HTTP 401 = 服务在但 key 不符（withAPIKey 语义）：不是离线，是鉴权态——pill 如实区分。
 * - 其余失败（网络 / 4xx/5xx / 非 JSON）= 离线。
 * - /api/auth/verify 是凭证校验 POST（请求体 source+consent，AuthVerifyResponse），
 *   不是健康探针——连通性不烧它；它的 401/fieldErrors 语义归接入向导（后续批）。
 */

export type PillStatus =
  | { phase: 'initial' }                       // 尚未完成首次探测（保持 B1 的「未接线」观感不闪烁）
  | { phase: 'checking' }                      // 后续周期性探测中，保持上一轮结果展示
  | { phase: 'online'; time: ServerTimeResponse; skewSec: number }   // 连通 + 服务器时间偏差
  | { phase: 'unauthorized' }                  // 服务在，key 缺失/不符（401）
  | { phase: 'offline'; detail: string };      // 网络失败 / 非 401 错误码

/** 周期探测间隔（毫秒）。断线可感知的最低限：30 秒一轮，失败轮不重试轰炸。 */
export const PILL_POLL_MS = 30_000;

export async function probePillStatus(): Promise<PillStatus> {
  try {
    const t = await fetchServerTime();
    // now 是 Unix 秒（契约冻结）；偏差 = |服务端时钟 − 本机时钟|，供用户判断本机时钟漂移
    const skewSec = Math.abs(Math.round(t.now - Date.now() / 1000));
    return { phase: 'online', time: t, skewSec };
  } catch (err) {
    if (err instanceof Error && 'status' in err && (err as { status?: number }).status === 401) {
      return { phase: 'unauthorized' };
    }
    return { phase: 'offline', detail: err instanceof Error ? err.message : String(err) };
  }
}
