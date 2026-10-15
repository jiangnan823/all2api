import { useCallback, useEffect, useRef, useState } from 'react';
import { PACKAGES } from '../i18n/zh';


/**
 * 积分到期提醒卡（M9a，wb2api「积分到期提醒」同构）：按账号显示最近到期批次
 * （日期 + X 天后到期 + 该批积分 + 到期前日均需耗）与随后各批次行。
 * 数据源 GET /api/packages（上游实时逐包查询，FEFO 升序）；单号探查失败该号
 * 显示错误原文，不造零。
 */
interface PkgCycleEnd {
  unix: number;
  local: string;
}

interface PkgBatch {
  remain: number;
  cycle_end: PkgCycleEnd | null;
}

interface PkgAccount {
  platform: string;
  id: string;
  nickname: string | null;
  total_remain: number | null;
  nearest: PkgCycleEnd | null;
  daily_burn: number | null;
  batches: PkgBatch[];
  error?: string;
}

function fmtLocal(cycleEnd: PkgCycleEnd | null): string {
  return cycleEnd ? cycleEnd.local.slice(0, 10) : '—';
}

function daysUntil(cycleEnd: PkgCycleEnd | null): number | null {
  if (!cycleEnd) return null;
  return Math.ceil((cycleEnd.unix * 1000 - Date.now()) / 86_400_000);
}

function AccountPackages(props: { account: PkgAccount }) {
  const { account: a } = props;
  const nearest = a.nearest;
  const days = daysUntil(nearest);
  const nearestRemain = a.batches[0]?.remain ?? null;
  return (
    <div className="packages-card__account">
      <h4 className="packages-card__account-title">{a.nickname || a.id}</h4>
      {a.error ? (
        <p className="packages-card__error" role="alert">{a.error}</p>
      ) : (
        <>
          <p className="packages-card__nearest">
            {PACKAGES.NEAREST.replace('{date}', nearest ? fmtLocal(nearest) : '—')
              .replace('{days}', String(days))
              .replace('{remain}', String(nearestRemain))}
          </p>
          <div className="packages-card__batches">
            {a.batches.map((b, i) => (
              <span key={i} className="packages-card__batch">
                {PACKAGES.BATCH_LINE.replace('{date}', b.cycle_end ? b.cycle_end.local.slice(5, 10) : '—').replace('{remain}', String(b.remain))}
              </span>
            ))}
          </div>
        </>
      )}
    </div>
  );
}

export function PackagesCard() {
  const [state, setState] = useState<
    { phase: 'loading' } | { phase: 'error'; message: string } | { phase: 'ready'; accounts: PkgAccount[] }
  >({ phase: 'loading' });
  const seqRef = useRef(0);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    try {
      const res = await fetch('/api/packages');
      const body = await res.json();
      if (seq !== seqRef.current) return;
      setState({ phase: 'ready', accounts: Array.isArray(body.accounts) ? body.accounts : [] });
    } catch (err) {
      if (seq === seqRef.current) {
        setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
      }
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 60_000);
    return () => window.clearInterval(timer);
  }, [load]);

  return (
    <div className="packages-card" data-testid="packages-card">
      <div className="packages-card__head">
        <h3 className="packages-card__title">{PACKAGES.TITLE}</h3>
        <button type="button" className="btn" onClick={() => void load()}>
          {PACKAGES.REFRESH}
        </button>
      </div>
      <p className="packages-card__note">{PACKAGES.NOTE}</p>
      {state.phase === 'loading' ? (
        <p className="packages-card__note" role="status">{PACKAGES.LOADING}</p>
      ) : state.phase === 'error' ? (
        <p className="packages-card__note" role="alert">{state.message}</p>
      ) : (
        <div className="packages-card__accounts">
          {state.accounts.map((a) => (
            <AccountPackages key={`${a.platform}:${a.id}`} account={a} />
          ))}
        </div>
      )}
    </div>
  );
}
