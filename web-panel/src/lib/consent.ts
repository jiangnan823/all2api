import type { ConsentAck, FieldError, TimeLocal } from '../types/api-contract';

/**
 * POST /api/auth/verify 的 consent 请求体（G3 冻结：唯一顶层键 consent，内部只有两个字段）。
 * ack_at 不在请求体里——留痕时间戳由服务端落戳（PM seq=360 O-5），客户端时钟不采信。
 */
export function consentRequestBody(ack: ConsentAck): { source: string; notice_version: string } {
  return { source: ack.source, notice_version: ack.noticeVersion };
}

/** 组件 onAck 的即时回显时间戳：仅供本地展示/兜底，服务端记录以接收时刻为准。 */
export function echoTimeLocal(at: Date = new Date()): TimeLocal {
  const p = (n: number) => String(n).padStart(2, '0');
  return {
    unix: Math.floor(at.getTime() / 1000),
    local: `${at.getFullYear()}-${p(at.getMonth() + 1)}-${p(at.getDate())} ${p(at.getHours())}:${p(at.getMinutes())}:${p(at.getSeconds())}`,
  };
}

/**
 * consent 类拒绝的处置分派（TC-M1-029 口径）：只看 FieldError.code，绝不比较中文 message。
 * 返回 null 表示这批错误与知情同意无关，交由各字段自身落点渲染。
 */
export function consentRejectionOf(
  fieldErrors: FieldError[] | undefined,
): 'consent_missing' | 'consent_stale' | null {
  if (!fieldErrors) return null;
  for (const e of fieldErrors) {
    if (e.code === 'consent_missing') return 'consent_missing';
    if (e.code === 'consent_stale') return 'consent_stale';
  }
  return null;
}
