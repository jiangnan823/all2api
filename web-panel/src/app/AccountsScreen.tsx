import { useCallback, useEffect, useRef, useState } from 'react';
import { ACCOUNTS } from '../i18n/zh';
import {
  ACCOUNTS_POLL_MS,
  fetchAccounts,
  statusTone,
  type AccountEntry,
  type AccountsSnapshot,
} from '../lib/poolApi';

/**
 * 账号池屏（M5b）：状态卡行（总数/可用/冷却中/禁用/积分合计，等宽数字）+ 账号表格
 * （平台/账号 id 全量显示+点击复制/状态徽色/积分/健康度/到期/冷却至）+ 手动刷新 + 30s 轮询。
 * 数据端点 GET /api/accounts 由后端并行批交付：运行期 404 如实展示错误态，
 * 空池显示引导文案（把 workbuddy-*.json 放入 auths\ 后重启）。全部内存态，不落 storage。
 */
type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; data: AccountsSnapshot };

const TONE_CLASS: Record<ReturnType<typeof statusTone>, string> = {
  ok: 'pool-badge--ok',
  warn: 'pool-badge--warn',
  err: 'pool-badge--err',
  idle: 'pool-badge--idle',
};

/** 状态串 → 中文徽标文案（未知值原样显示，不翻译瞎猜）。 */
const STATUS_LABEL: Record<string, string> = {
  active: '可用',
  cooling: '冷却中',
  disabled: '已禁用',
};

function statusLabel(status: string): string {
  return STATUS_LABEL[status.toLowerCase()] ?? status;
}

/** Unix 秒 → 本地展示串；null/0 = 无值（显示 —，不编造时间）。 */
function fmtUnix(sec: number | null, dash: string): string {
  if (sec === null || sec === 0) return dash;
  const d = new Date(sec * 1000);
  if (Number.isNaN(d.getTime())) return dash;
  return d.toLocaleString();
}

function fmtCredit(n: number | null, dash: string): string {
  return n === null ? dash : n.toLocaleString();
}

/** 账号 ID 全量显示 + 点击复制（clipboard API 不可用时如实不提示成功，不假成功）。 */
function AccountIdCell(props: { id: string }) {
  const { id } = props;
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<number | null>(null);

  useEffect(
    () => () => {
      if (timerRef.current !== null) window.clearTimeout(timerRef.current);
    },
    [],
  );

  const onClick = () => {
    navigator.clipboard
      ?.writeText(id)
      .then(() => {
        setCopied(true);
        if (timerRef.current !== null) window.clearTimeout(timerRef.current);
        timerRef.current = window.setTimeout(() => setCopied(false), 1500);
      })
      .catch(() => setCopied(false));
  };

  return (
    <button
      type="button"
      className="pool-copy"
      onClick={onClick}
      title={ACCOUNTS.COPY}
      aria-label={`${ACCOUNTS.COPY}: ${id}`}
    >
      <code>{id}</code>
      {copied && (
        <span className="pool-copy__hint" role="status">
          {ACCOUNTS.COPIED}
        </span>
      )}
    </button>
  );
}

function AccountRow(props: { entry: AccountEntry; showPlatform: boolean }) {
  const { entry: a, showPlatform } = props;
  return (
    <tr>
      {showPlatform ? <td>{a.platform}</td> : null}
      <td>
        {a.nickname ? <span className="pool-table__nick">{a.nickname}</span> : null}
        <AccountIdCell id={a.id} />
      </td>
      <td>
        <span className={`pool-badge ${TONE_CLASS[statusTone(a.status)]}`}>{statusLabel(a.status)}</span>
      </td>
      <td className="pool-table__num">{fmtCredit(a.credit, ACCOUNTS.DASH)}</td>
      <td>{a.health ?? ACCOUNTS.DASH}</td>
      <td className="pool-table__num">{fmtUnix(a.expiresAt, ACCOUNTS.DASH)}</td>
      <td className="pool-table__num">{fmtUnix(a.cooldownEnd, ACCOUNTS.DASH)}</td>
    </tr>
  );
}

/**
 * 账号池屏容器：加载/错误/空池/就绪四态；就绪态 = 状态卡行 + 表格。
 * 30s 轮询在四态下都保持（错误态下一轮也可能自愈），手动「刷新」立即重拉。
 *
 * v1.2 二级菜单：`platform` 传入时分平台过滤（undefined = 总览，聚合取全量）。
 * 过滤是纯前端切片——/api/accounts 一次拉全量，避免按平台轮询多次。
 */
export function AccountsScreen(props: { platform?: 'workbuddy' | 'qoder' | 'trae' }) {
  const filter = props.platform;
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
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
    const timer = window.setInterval(() => void load(), ACCOUNTS_POLL_MS);
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
  // v1.2：平台切片（明细页）；聚合卡也按切片重算（总览页走全量，不做二次聚合）。
  const scoped = filter ? data.accounts.filter((a) => a.platform === filter) : data.accounts;
  const agg = filter
    ? {
        total: scoped.length,
        active: scoped.filter((a) => a.status === 'active').length,
        cooling: scoped.filter((a) => a.status === 'cooling').length,
        disabled: scoped.filter((a) => a.status === 'disabled').length,
        creditTotal: scoped.reduce((sum, a) => sum + (a.credit ?? 0), 0),
      }
    : { total: data.total, active: data.active, cooling: data.cooling, disabled: data.disabled, creditTotal: data.creditTotal };
  if (scoped.length === 0) {
    return (
      <div className="pool-empty">
        <h3 className="pool-empty__title">{ACCOUNTS.EMPTY_TITLE}</h3>
        <p className="pool-empty__body">{ACCOUNTS.EMPTY_BODY}</p>
        <button type="button" className="btn" onClick={() => void load()}>
          {ACCOUNTS.REFRESH}
        </button>
      </div>
    );
  }

  return (
    <div className="pool-screen">
      <div className="pool-screen__toolbar">
        <p className="pool-screen__sub">{ACCOUNTS.SUB}</p>
        <button type="button" className="btn" onClick={() => void load()}>
          {ACCOUNTS.REFRESH}
        </button>
      </div>

      <ul className="pool-cards" aria-label={ACCOUNTS.CARD_TOTAL}>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_TOTAL}</span>
          <span className="pool-card__value">{agg.total}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_ACTIVE}</span>
          <span className="pool-card__value pool-card__value--ok">{agg.active}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_COOLING}</span>
          <span className="pool-card__value pool-card__value--warn">{agg.cooling}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_DISABLED}</span>
          <span className="pool-card__value pool-card__value--err">{agg.disabled}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_CREDIT}</span>
          <span className="pool-card__value">{fmtCredit(agg.creditTotal, ACCOUNTS.DASH)}</span>
        </li>
      </ul>

      <table className="pool-table">
        <thead>
          <tr>
            {filter ? null : <th scope="col">{ACCOUNTS.COL_PLATFORM}</th>}
            <th scope="col">{ACCOUNTS.COL_ACCOUNT}</th>
            <th scope="col">{ACCOUNTS.COL_STATUS}</th>
            <th scope="col">{ACCOUNTS.COL_CREDIT}</th>
            <th scope="col">{ACCOUNTS.COL_HEALTH}</th>
            <th scope="col">{ACCOUNTS.COL_EXPIRES}</th>
            <th scope="col">{ACCOUNTS.COL_COOLDOWN}</th>
          </tr>
        </thead>
        <tbody>
          {scoped.map((a) => (
            <AccountRow key={`${a.platform}:${a.id}`} entry={a} showPlatform={!filter} />
          ))}
        </tbody>
      </table>
    </div>
  );
}
