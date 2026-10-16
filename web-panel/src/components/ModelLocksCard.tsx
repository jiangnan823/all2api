import { useCallback, useEffect, useRef, useState } from 'react';
import { MODELLOCKS } from '../i18n/zh';
import { unlockModelLocks } from '../lib/poolApi';

/**
 * 模型锁池卡（M9b）：哪些模型因上游限流不可用、锁了几个账号、最早解锁。
 * 空态 = 所有模型均可选。30s 轮询与账号池同节奏，数据源 GET /api/model-locks。
 *
 * M10-C（wb2api 对齐）：locks[] 增 scope（域）与 available/total（可选·总数）两列；
 * 行级「全池解锁」POST /api/model-locks/unlock body {"model":...}，表头区「全部解锁」body {}；
 * 解锁成功即重拉锁表；busy 期间全部解锁按钮禁用。新增文案为组件内联字典（zh.ts 本批冻结）。
 */

interface ModelLock {
  model: string;
  platform: string;
  /** 域（wb2api scope，如 model 命名空间域名）；缺失/空串 → —。 */
  scope: string | null;
  locked_accounts: number;
  total_accounts: number;
  /** 可选账号数（M10 wire available；缺失 → null → —）。 */
  available: number | null;
  /** 该模型总账号数（M10 wire total；缺失 → null → —）。 */
  total: number | null;
  earliest_unlock: { unix: number; local: string } | null;
  reason: string | null;
}

/** 组件内联字典（zh.ts 本批冻结）：M10 新增文案只落在这里。 */
const COPY = {
  COL_SCOPE: '域',
  COL_AVAIL: '可选/总数',
  COL_ACTIONS: '操作',
  BTN_UNLOCK: '全池解锁',
  BTN_UNLOCK_ALL: '全部解锁',
  UNLOCKING: '解锁中…',
  ERR_PREFIX: '解锁失败：',
} as const;

export function ModelLocksCard() {
  const [state, setState] = useState<
    { phase: 'loading' } | { phase: 'error'; message: string } | { phase: 'ready'; locks: ModelLock[] }
  >({ phase: 'loading' });
  const seqRef = useRef(0);
  // 解锁操作：任一解锁请求在途即全卡禁用（解锁会改变锁的分布，避免并发竞态）
  const [busy, setBusy] = useState(false);
  const [actionErr, setActionErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    try {
      const res = await fetch('/api/model-locks');
      const body = await res.json();
      if (seq !== seqRef.current) return;
      const raw = Array.isArray(body.locks) ? body.locks : [];
      // 防御式读数：单条形状不对丢弃，不拖垮整卡；scope/available/total 可空
      const locks: ModelLock[] = [];
      for (const item of raw) {
        if (item === null || typeof item !== 'object') continue;
        const o = item as Record<string, unknown>;
        if (typeof o['model'] !== 'string' || o['model'] === '') continue;
        locks.push({
          model: o['model'],
          platform: typeof o['platform'] === 'string' ? o['platform'] : '',
          scope: typeof o['scope'] === 'string' && o['scope'] !== '' ? o['scope'] : null,
          locked_accounts: typeof o['locked_accounts'] === 'number' && Number.isFinite(o['locked_accounts']) ? o['locked_accounts'] : 0,
          total_accounts: typeof o['total_accounts'] === 'number' && Number.isFinite(o['total_accounts']) ? o['total_accounts'] : 0,
          available: typeof o['available'] === 'number' && Number.isFinite(o['available']) ? o['available'] : null,
          total: typeof o['total'] === 'number' && Number.isFinite(o['total']) ? o['total'] : null,
          earliest_unlock:
            o['earliest_unlock'] !== null && typeof o['earliest_unlock'] === 'object'
              ? (o['earliest_unlock'] as { unix: number; local: string })
              : null,
          reason: typeof o['reason'] === 'string' && o['reason'] !== '' ? o['reason'] : null,
        });
      }
      setState({ phase: 'ready', locks });
    } catch (err) {
      if (seq === seqRef.current) {
        setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
      }
    }
  }, []);

  const unlock = useCallback(
    async (model: string | null) => {
      setBusy(true);
      setActionErr(null);
      try {
        await unlockModelLocks(model);
        await load();
      } catch (err) {
        setActionErr(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy(false);
      }
    },
    [load],
  );

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 30_000);
    return () => window.clearInterval(timer);
  }, [load]);

  return (
    <div className="modelocks-card" data-testid="model-locks-card">
      <div className="modelocks-card__head">
        <h3 className="modelocks-card__title">{MODELLOCKS.TITLE}</h3>
        <div className="modelocks-card__head-btns">
          <button
            type="button"
            className="btn"
            disabled={busy}
            aria-busy={busy}
            title="解除全部模型锁（账号立即回到可选池）"
            onClick={() => void unlock(null)}
          >
            {COPY.BTN_UNLOCK_ALL}
          </button>
          <button type="button" className="btn" onClick={() => void load()}>
            {MODELLOCKS.REFRESH}
          </button>
        </div>
      </div>
      {actionErr !== null && (
        <p className="modelocks-card__acterr" role="alert">
          {COPY.ERR_PREFIX}
          {actionErr}
        </p>
      )}
      {state.phase === 'loading' ? (
        <p className="modelocks-card__note" role="status">{MODELLOCKS.LOADING}</p>
      ) : state.phase === 'error' ? (
        <p className="modelocks-card__note" role="alert">{state.message}</p>
      ) : state.locks.length === 0 ? (
        <p className="modelocks-card__note">{MODELLOCKS.EMPTY}</p>
      ) : (
        <table className="modelocks-card__table pool-table">
          <thead>
            <tr>
              <th>{MODELLOCKS.COL_MODEL}</th>
              <th>{COPY.COL_SCOPE}</th>
              <th>{MODELLOCKS.COL_LOCKED}</th>
              <th>{COPY.COL_AVAIL}</th>
              <th>{MODELLOCKS.COL_EARLIEST}</th>
              <th>{MODELLOCKS.COL_REASON}</th>
              <th>{COPY.COL_ACTIONS}</th>
            </tr>
          </thead>
          <tbody>
            {state.locks.map((l) => (
              <tr key={`${l.platform}:${l.model}`}>
                <td>{l.model}</td>
                <td>{l.scope ?? '—'}</td>
                <td>{l.locked_accounts}</td>
                <td>
                  {l.available === null && l.total === null ? '—' : `${l.available ?? '—'} / ${l.total ?? '—'}`}
                </td>
                <td>{l.earliest_unlock ? new Date(l.earliest_unlock.unix * 1000).toLocaleString() : '—'}</td>
                <td>{l.reason ?? '—'}</td>
                <td>
                  <button
                    type="button"
                    className="pool-act"
                    disabled={busy}
                    aria-busy={busy}
                    title={`解除 ${l.model} 的全部锁（该模型账号回到可选池）`}
                    onClick={() => void unlock(l.model)}
                  >
                    {COPY.BTN_UNLOCK}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
