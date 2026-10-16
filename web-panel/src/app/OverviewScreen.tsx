import { useCallback, useEffect, useRef, useState } from 'react';
import { ACCOUNTS, PANEL, USAGE } from '../i18n/zh';
import { fetchAccounts, type AccountsSnapshot } from '../lib/poolApi';
import { USAGE_POLL_MS, fetchUsage, type UsageSnapshot } from '../lib/usageApi';

/**
 * v1.2 账号池总览页（主人裁定 2026-10-15）：全平台聚合卡 + 每平台摘要卡
 * （账号数/积分/本次运行请求/成功失败/最近成功）+ 下钻链接。只读，不放明细
 * 表格与添加按钮（边界矩阵 §0）。30s 轮询与明细页一致。
 *
 * 用量摘要口径（M8-reqlog 接入后如实）：KPI 摘要行来自 GET /api/usage 的
 * "window":"process" 口径（本次运行，进程重启清零）；per-platform「请求」行
 * 同源取 by_platform 桶（本窗口无请求的平台如实显示 0）。token 口径只含
 * 携带 usage 的非流式回包（流式上游当前不下发），不做折算补零。
 */
const PLATFORM_LABEL: Record<string, string> = {
  workbuddy: 'WorkBuddy',
  qoder: 'Qoder',
  trae: 'Trae',
};

/** 平台启用面来自 config.sources；总览无法一次拿全 → 演示期按「是否出现账号」呈现，契约键接入后改读 /api/config。 */
export function OverviewScreen(props: { onOpen: (p: 'workbuddy' | 'qoder' | 'trae') => void }) {
  const [state, setState] = useState<{ phase: 'loading' } | { phase: 'error'; message: string } | { phase: 'ready'; data: AccountsSnapshot }>({ phase: 'loading' });
  const [usage, setUsage] = useState<{ phase: 'loading' } | { phase: 'error' } | { phase: 'ready'; data: UsageSnapshot }>({ phase: 'loading' });
  const seqRef = useRef(0);
  const usageSeqRef = useRef(0);

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

  // 用量 KPI 独立拉取：失败只降级本摘要行，不拖垮账号总览主数据。
  const loadUsage = useCallback(async () => {
    const seq = ++usageSeqRef.current;
    try {
      const data = await fetchUsage();
      if (seq === usageSeqRef.current) setUsage({ phase: 'ready', data });
    } catch {
      if (seq === usageSeqRef.current) setUsage({ phase: 'error' });
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 30_000);
    return () => window.clearInterval(timer);
  }, [load]);

  useEffect(() => {
    void loadUsage();
    const timer = window.setInterval(() => void loadUsage(), USAGE_POLL_MS);
    return () => window.clearInterval(timer);
  }, [loadUsage]);

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
          {/* M10：剩余/总额双值（credit_sum/credit_total），与明细页 KPI 同口径 */}
          <span className="pool-card__value">
            {data.creditSum === null && data.creditTotal === null
              ? '—'
              : `${(data.creditSum ?? 0).toLocaleString()} / ${(data.creditTotal ?? 0).toLocaleString()}`}
          </span>
        </li>
      </ul>

      {usage.phase === 'ready' && (
        <section aria-label={USAGE.ARIA_LABEL}>
          <p className="pool-screen__sub">
            {USAGE.WINDOW_BADGE}（{USAGE.WINDOW_NOTE}）
          </p>
          <ul className="pool-cards">
            <li className="pool-card">
              <span className="pool-card__label">{USAGE.KPI_REQUESTS}</span>
              <span className="pool-card__value">{usage.data.requests.toLocaleString()}</span>
            </li>
            <li className="pool-card">
              <span className="pool-card__label">{USAGE.KPI_FAIL}</span>
              <span className={`pool-card__value${usage.data.fail > 0 ? ' pool-card__value--err' : ''}`}>
                {usage.data.fail.toLocaleString()}
              </span>
            </li>
            <li className="pool-card">
              <span className="pool-card__label">{USAGE.KPI_PROMPT}</span>
              <span className="pool-card__value">{usage.data.promptTokens.toLocaleString()}</span>
            </li>
            <li className="pool-card">
              <span className="pool-card__label">{USAGE.KPI_COMPLETION}</span>
              <span className="pool-card__value">{usage.data.completionTokens.toLocaleString()}</span>
            </li>
          </ul>
        </section>
      )}
      {usage.phase === 'error' && (
        <p className="pool-note" role="status">{USAGE.ERROR_NOTE}</p>
      )}

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
          // 本次运行请求/失败（M8 by_platform 桶）：窗口内无请求的平台如实为 0；
          // 用量数据未就绪/读取失败时维持「待接入」占位，不编数。
          const bucket = usage.phase === 'ready' ? (usage.data.byPlatform[p] ?? { requests: 0, fail: 0 }) : null;
          return (
            <div className="ov-card" key={p}>
              <h3 className="ov-card__title">{PLATFORM_LABEL[p] ?? p}</h3>
              <dl className="ov-card__rows">
                <div className="ov-card__row"><dt>{ACCOUNTS.OV_ACCOUNTS}</dt><dd>{list.length === 0 ? ACCOUNTS.OV_NONE : `${list.length}（${active} 可用 / ${cooling} 冷却 / ${disabled} 失效）`}</dd></div>
                <div className="ov-card__row"><dt>{ACCOUNTS.OV_CREDIT}</dt><dd>{list.length === 0 ? '—' : credit.toLocaleString()}</dd></div>
                <div className="ov-card__row"><dt>{ACCOUNTS.OV_REQUESTS}</dt><dd>{bucket === null ? ACCOUNTS.OV_REQUESTS_TODO : `${bucket.requests}（${USAGE.KPI_FAIL} ${bucket.fail}）`}</dd></div>
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
