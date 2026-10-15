import { useCallback, useEffect, useRef, useState } from 'react';
import { ACCOUNTS, PANEL } from '../i18n/zh';
import { fetchAccounts, type AccountsSnapshot } from '../lib/poolApi';

/**
 * v1.2 账号池总览页（主人裁定 2026-10-15）：全平台聚合卡 + 每平台摘要卡
 * （账号数/积分/本次运行请求/成功失败/最近成功）+ 下钻链接。只读，不放明细
 * 表格与添加按钮（边界矩阵 §0）。30s 轮询与明细页一致。
 *
 * 用量摘要口径（如实）：per-account 计数来自 /api/accounts 的账户数据；后端
 * 尚未输出 per-platform 请求聚合——M5a 契约（accounts 无 success/fail 字段）
 * 下先展示账号/积分维度，「请求」行如实显示「统计维度待接入」。不造假数。
 */
const PLATFORM_LABEL: Record<string, string> = {
  workbuddy: 'WorkBuddy',
  qoder: 'Qoder',
  trae: 'Trae',
};

/** 平台启用面来自 config.sources；总览无法一次拿全 → 演示期按「是否出现账号」呈现，契约键接入后改读 /api/config。 */
export function OverviewScreen(props: { onOpen: (p: 'workbuddy' | 'qoder' | 'trae') => void }) {
  const [state, setState] = useState<{ phase: 'loading' } | { phase: 'error'; message: string } | { phase: 'ready'; data: AccountsSnapshot }>({ phase: 'loading' });
  const seqRef = useRef(0);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    try {
      const data = await fetchAccounts();
      if (seq === seqRef.current) setState({ phase: 'ready', data });
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

  if (state.phase === 'loading') {
    return <p className="pool-note" role="status">{ACCOUNTS.LOADING}</p>;
  }
  if (state.phase === 'error') {
    return (
      <div className="pool-error" role="alert">
        <h3 className="pool-error__title">{ACCOUNTS.ERROR_TITLE}</h3>
        <p className="pool-error__detail">{state.message}</p>
        <p className="pool-error__hint">{ACCOUNTS.ERROR_HINT}</p>
        <button type="button" className="btn" onClick={() => void load()}>
          {ACCOUNTS.RETRY}
        </button>
      </div>
    );
  }

  const { data } = state;
  const byPlat = new Map<string, typeof data.accounts>();
  for (const a of data.accounts) {
    const list = byPlat.get(a.platform) ?? [];
    list.push(a);
    byPlat.set(a.platform, list);
  }
  const order = ['workbuddy', 'qoder', 'trae'];
  const plats = [...order, ...[...byPlat.keys()].filter((k) => !order.includes(k))];

  return (
    <div className="pool-screen">
      <ul className="pool-cards" aria-label={ACCOUNTS.CARD_TOTAL}>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_TOTAL}</span>
          <span className="pool-card__value">{data.total}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_ACTIVE}</span>
          <span className="pool-card__value pool-card__value--ok">{data.active}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_COOLING}</span>
          <span className="pool-card__value pool-card__value--warn">{data.cooling}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_DISABLED}</span>
          <span className="pool-card__value pool-card__value--err">{data.disabled}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_CREDIT}</span>
          <span className="pool-card__value">{data.creditTotal === null ? '—' : data.creditTotal.toLocaleString()}</span>
        </li>
      </ul>

      <div className="ov-grid">
        {plats.map((p) => {
          const list = byPlat.get(p) ?? [];
          const active = list.filter((a) => a.status === 'active').length;
          const cooling = list.filter((a) => a.status === 'cooling').length;
          const disabled = list.filter((a) => a.status === 'disabled').length;
          const credit = list.reduce((sum, a) => sum + (a.credit ?? 0), 0);
          const lastSuccess = list
            .map((a) => a.lastSelected)
            .filter((t): t is number => t !== null && t > 0)
            .sort((x, y) => y - x)[0];
          return (
            <div className="ov-card" key={p}>
              <h3 className="ov-card__title">{PLATFORM_LABEL[p] ?? p}</h3>
              <dl className="ov-card__rows">
                <div className="ov-card__row"><dt>{ACCOUNTS.OV_ACCOUNTS}</dt><dd>{list.length === 0 ? ACCOUNTS.OV_NONE : `${list.length}（${active} 可用 / ${cooling} 冷却 / ${disabled} 失效）`}</dd></div>
                <div className="ov-card__row"><dt>{ACCOUNTS.OV_CREDIT}</dt><dd>{list.length === 0 ? '—' : credit.toLocaleString()}</dd></div>
                <div className="ov-card__row"><dt>{ACCOUNTS.OV_REQUESTS}</dt><dd>{ACCOUNTS.OV_REQUESTS_TODO}</dd></div>
                <div className="ov-card__row"><dt>{ACCOUNTS.OV_LAST_OK}</dt><dd>{lastSuccess ? new Date(lastSuccess * 1000).toLocaleString() : '—'}</dd></div>
              </dl>
              <button type="button" className="btn" onClick={() => props.onOpen(p as 'workbuddy' | 'qoder' | 'trae')}>
                {PANEL.OV_OPEN_DETAIL}
              </button>
            </div>
          );
        })}
      </div>
    </div>
  );
}
