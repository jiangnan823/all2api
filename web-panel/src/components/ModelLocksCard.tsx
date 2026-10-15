import { useCallback, useEffect, useRef, useState } from 'react';
import { MODELLOCKS } from '../i18n/zh';


/**
 * 模型锁池卡（M9b）：哪些模型因上游限流不可用、锁了几个账号、最早解锁。
 * 空态 = 所有模型均可选。30s 轮询与账号池同节奏，数据源 GET /api/model-locks。
 */
interface ModelLock {
  model: string;
  platform: string;
  locked_accounts: number;
  total_accounts: number;
  earliest_unlock: { unix: number; local: string } | null;
  reason: string | null;
}

export function ModelLocksCard() {
  const [state, setState] = useState<
    { phase: 'loading' } | { phase: 'error'; message: string } | { phase: 'ready'; locks: ModelLock[] }
  >({ phase: 'loading' });
  const seqRef = useRef(0);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    try {
      const res = await fetch('/api/model-locks');
      const body = await res.json();
      if (seq !== seqRef.current) return;
      setState({ phase: 'ready', locks: Array.isArray(body.locks) ? body.locks : [] });
    } catch (err) {
      if (seq === seqRef.current) {
        setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
      }
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 30_000);
    return () => window.clearInterval(timer);
  }, [load]);

  return (
    <div className="modelocks-card" data-testid="model-locks-card">
      <div className="modelocks-card__head">
        <h3 className="modelocks-card__title">{MODELLOCKS.TITLE}</h3>
        <button type="button" className="btn" onClick={() => void load()}>
          {MODELLOCKS.REFRESH}
        </button>
      </div>
      {state.phase === 'loading' ? (
        <p className="modelocks-card__note" role="status">{MODELLOCKS.LOADING}</p>
      ) : state.phase === 'error' ? (
        <p className="modelocks-card__note" role="alert">{state.message}</p>
      ) : state.locks.length === 0 ? (
        <p className="modelocks-card__note">{MODELLOCKS.EMPTY}</p>
      ) : (
        <table className="modelocks-card__table">
          <thead>
            <tr>
              <th>{MODELLOCKS.COL_MODEL}</th>
              <th>{MODELLOCKS.COL_LOCKED}</th>
              <th>{MODELLOCKS.COL_EARLIEST}</th>
              <th>{MODELLOCKS.COL_REASON}</th>
            </tr>
          </thead>
          <tbody>
            {state.locks.map((l) => (
              <tr key={`${l.platform}:${l.model}`}>
                <td>{l.model}</td>
                <td>{l.locked_accounts}</td>
                <td>{l.earliest_unlock ? new Date(l.earliest_unlock.unix * 1000).toLocaleString() : '—'}</td>
                <td>{l.reason ?? '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

