import { useCallback, useEffect, useRef, useState } from 'react';
import { PACKAGES } from '../i18n/zh';
import { refreshPackages, type PackagesRefreshResult } from '../lib/poolApi';

/**
 * 积分到期提醒卡（M9a，wb2api「积分到期提醒」同构）：按账号显示最近到期批次
 * （日期 + X 天后到期 + 该批积分 + 到期前日均需耗）与随后各批次行。
 * 数据源 GET /api/packages（上游实时逐包查询，FEFO 升序）；单号探查失败该号
 * 显示错误原文，不造零。
 *
 * M10-C：头部加「检查」按钮（POST /api/packages/refresh 实时查询上游，
 * 完成后重拉本卡数据）+「N 个账号 · 实时查询上游」口径文案；refresh 返回的
 * errors 逐账号可折叠展示。60s 轮询保持不变。新增文案为组件内联字典。
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

/** 组件内联字典（zh.ts 本批冻结）：新增文案只落在这里。 */
const COPY = {
  COUNT_NOTE: (n: number): string => `${n} 个账号 · 实时查询上游`,
  CHECKING: '检查中…',
  REFRESHED: (n: number): string => `已刷新 ${n} 个账号`,
  ERRORS_TITLE: '查询失败',
  /** 随后批次最多展示条数（wb2api 同构：3 条 + 「等 N 批」）。 */
  FOLLOWING_SHOWN: 3,
  MORE: (n: number): string => `等 ${n} 批`,
  ALL_SPENT: '该账号所有批次剩余均为 0',
} as const;

function fmtLocal(cycleEnd: PkgCycleEnd | null): string {
  return cycleEnd ? cycleEnd.local.slice(0, 10) : '—';
}

function daysUntil(cycleEnd: PkgCycleEnd | null): number | null {
  if (!cycleEnd) return null;
  return Math.ceil((cycleEnd.unix * 1000 - Date.now()) / 86_400_000);
}

function AccountPackages(props: { account: PkgAccount }) {
  const { account: a } = props;
  // 只剩 0 的批次不参与估算也不倾倒展示（真实上游会返回几十个耗尽的
  // 历史包，全量渲染成一堵 0 墙没有信息量）——FEFO 语义按「有剩余」算。
  const active = a.batches.filter((b) => b.remain > 0);
  if (a.error) {
    return (
      <div className="packages-card__account">
        <h4 className="packages-card__account-title">{a.nickname || a.id}</h4>
        <p className="packages-card__error" role="alert">{a.error}</p>
      </div>
    );
  }
  if (active.length === 0) {
    return (
      <div className="packages-card__account">
        <h4 className="packages-card__account-title">{a.nickname || a.id}</h4>
        <p className="packages-card__nearest">{COPY.ALL_SPENT}</p>
      </div>
    );
  }
  const nearestBatch = active[0];
  const days = daysUntil(nearestBatch.cycle_end);
  // 日均需耗按「该批剩余 ÷ 距到期天数」现算（wb2api 同构口径）；到期当天
  // 剩余就是今天要烧掉的量。
  const daily = days === null ? null : Math.ceil(nearestBatch.remain / Math.max(days, 1));
  const following = active.slice(1, 1 + COPY.FOLLOWING_SHOWN);
  const more = active.length - 1 - following.length;
  return (
    <div className="packages-card__account">
      <h4 className="packages-card__account-title">{a.nickname || a.id}</h4>
      <p className="packages-card__nearest">
        {PACKAGES.NEAREST.replace('{date}', fmtLocal(nearestBatch.cycle_end))
          .replace('{days}', String(days === null ? '—' : Math.max(days, 0)))
          .replace('{remain}', String(nearestBatch.remain))
          .replace('{daily}', String(daily ?? '—'))}
      </p>
      <div className="packages-card__batches">
        {following.map((b, i) => (
          <span key={i} className="packages-card__batch">
            {PACKAGES.BATCH_LINE.replace('{date}', b.cycle_end ? b.cycle_end.local.slice(5, 10) : '—').replace('{remain}', String(b.remain))}
          </span>
        ))}
        {more > 0 && <span className="packages-card__batch packages-card__batch--more">{COPY.MORE(more)}</span>}
      </div>
    </div>
  );
}

export function PackagesCard() {
  const [state, setState] = useState<
    { phase: 'loading' } | { phase: 'error'; message: string } | { phase: 'ready'; accounts: PkgAccount[] }
  >({ phase: 'loading' });
  const seqRef = useRef(0);
  // M10-C「检查」：POST 实时查询期间按钮禁用；结果行内展示（失败逐账号可折叠）
  const [checking, setChecking] = useState(false);
  const [refreshRes, setRefreshRes] = useState<PackagesRefreshResult | null>(null);
  const [refreshErr, setRefreshErr] = useState<string | null>(null);

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

  const check = useCallback(async () => {
    setChecking(true);
    setRefreshRes(null);
    setRefreshErr(null);
    try {
      const res = await refreshPackages();
      setRefreshRes(res);
      await load(); // 实时查询完成后以最新快照重建批次行
    } catch (err) {
      setRefreshErr(err instanceof Error ? err.message : String(err));
    } finally {
      setChecking(false);
    }
  }, [load]);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 60_000);
    return () => window.clearInterval(timer);
  }, [load]);

  return (
    <div className="packages-card" data-testid="packages-card">
      <div className="packages-card__head">
        <h3 className="packages-card__title">{PACKAGES.TITLE}</h3>
        <button type="button" className="btn" disabled={checking} aria-busy={checking} onClick={() => void check()}>
          {checking ? COPY.CHECKING : PACKAGES.REFRESH}
        </button>
      </div>
      <p className="packages-card__note">
        {state.phase === 'ready' ? (
          <>
            <span className="packages-card__count">{COPY.COUNT_NOTE(state.accounts.length)}</span>
            <br />
          </>
        ) : null}
        {PACKAGES.NOTE}
      </p>
      {refreshErr !== null && (
        <p className="packages-card__error" role="alert">
          {refreshErr}
        </p>
      )}
      {refreshRes !== null && (
        <div className="packages-card__refresh" role="status">
          <p className="packages-card__refresh-summary">
            {COPY.REFRESHED(refreshRes.refreshed)}
            {refreshRes.errors.length > 0 ? ` · ${COPY.ERRORS_TITLE} ${refreshRes.errors.length}` : ''}
          </p>
          {refreshRes.errors.length > 0 && (
            <details className="packages-card__refresh-errors">
              <summary>
                {COPY.ERRORS_TITLE}（{refreshRes.errors.length}）
              </summary>
              <ul>
                {refreshRes.errors.map((e) => (
                  <li key={e.id}>
                    <code>{e.id}</code> {e.error}
                  </li>
                ))}
              </ul>
            </details>
          )}
        </div>
      )}
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
