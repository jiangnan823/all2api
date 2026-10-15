import { useCallback, useEffect, useRef, useState } from 'react';
import { ACCOUNTS } from '../i18n/zh';
import {
  ACCOUNTS_POLL_MS,
  addQoderAccount,
  addWorkbuddyAccount,
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

/** 冷却截止 → 倒计时（"23 分钟"）；无冷却显示 —。纯前端换算，分钟粒度。 */
function cooldownLabel(unix: number | null): string {
  if (unix === null || unix === 0) return '—';
  const ms = unix * 1000 - Date.now();
  if (ms <= 0) return '即将解除';
  const min = Math.ceil(ms / 60000);
  return `${min} 分钟后`;
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
      <td className="pool-table__num">{cooldownLabel(a.cooldownEnd)}</td>
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
  const [wizardOpen, setWizardOpen] = useState(false);
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
    // v1.2 C1/C1b：空池 = 添加向导入口（不是"去放文件"）；trae 未接入显示接入引导。
    const emptyCopy =
      filter === 'trae'
        ? ACCOUNTS.EMPTY_TRAE
        : filter === 'qoder'
          ? `${ACCOUNTS.EMPTY_BODY} ${ACCOUNTS.EMPTY_QO_HINT}`
          : `${ACCOUNTS.EMPTY_BODY} ${ACCOUNTS.EMPTY_WB_HINT}`;
    return (
      <div className="pool-empty">
        <h3 className="pool-empty__title">{ACCOUNTS.EMPTY_TITLE}</h3>
        <p className="pool-empty__body">{emptyCopy}</p>
        {filter !== 'trae' && (
          <button type="button" className="btn btn--primary" onClick={() => setWizardOpen(true)}>
            {ACCOUNTS.ADD_OPEN}
          </button>
        )}
        {filter === 'trae' ? null : null}
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
        <div className="row">
          {filter && (
            <button type="button" className="btn btn--primary" onClick={() => setWizardOpen((v) => !v)}>
              {wizardOpen ? ACCOUNTS.ADD_CLOSE : ACCOUNTS.ADD_OPEN}
            </button>
          )}
          <button type="button" className="btn" onClick={() => void load()}>
            {ACCOUNTS.REFRESH}
          </button>
        </div>
      </div>
      {filter && wizardOpen && (
        <AddAccountPanel
          platform={filter}
          onAdded={() => {
            setWizardOpen(false);
            void load();
          }}
        />
      )}

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
            <th scope="col" title="上游平台的点数余额：签到/任务/充值获得，对话按用量扣除">{ACCOUNTS.COL_CREDIT}</th>
            <th scope="col" title="近期请求质量分（0–1）：成功上升、失败下降；新账号从 1.0 起步">{ACCOUNTS.COL_HEALTH}</th>
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

/**
 * 添加账号面板（v1.2 §1a，主人裁定：添加账号是面板内一等操作）：
 * - workbuddy：粘贴官方桌面端凭证 JSON 原文（account.uid/nickname + auth.accessToken/refreshToken…）
 * - qoder：粘贴 PAT（pt-…）
 * 校验与落盘在后端（POST /api/accounts/{platform}）；成功后回调刷新账号池；
 * 字段校验错误（400 + {error}）逐字显示，不静默兜底。
 */
function AddAccountPanel(props: { platform: 'workbuddy' | 'qoder' | 'trae'; onAdded: () => void }) {
  const { platform } = props;
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [okId, setOkId] = useState<string | null>(null);

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      const res = platform === 'qoder' ? await addQoderAccount(value) : await addWorkbuddyAccount(value);
      setOkId(res.id);
      setValue('');
      props.onAdded();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const canSubmit = platform === 'qoder' ? value.trim().startsWith('pt-') : value.trim() !== '';

  if (platform === 'trae') {
    // 矩阵 §1a：trae 未接入网关，不渲染假表单（此前会误路由 workbuddy 端点）。
    return (
      <div className="add-account" role="note">
        <h3 className="add-account__title">{ACCOUNTS.ADD_TITLE}</h3>
        <p className="add-account__hint">{ACCOUNTS.EMPTY_TRAE}</p>
      </div>
    );
  }

  return (
    <div className="add-account" role="form" aria-label={ACCOUNTS.ADD_TITLE}>
      <h3 className="add-account__title">{ACCOUNTS.ADD_TITLE}</h3>
      {platform === 'workbuddy' && (
        <>
          <p className="add-account__hint">
            打开官方桌面端凭证目录 <code>%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\</code>，
            复制对应账号 <code>*.info</code> 文件的完整 JSON 内容，粘贴到下面：
          </p>
          <textarea
            className="add-account__input"
            aria-label={ACCOUNTS.ADD_WB_LABEL}
            placeholder='{"account":{"uid":"…","nickname":"…"},"auth":{"accessToken":"…","refreshToken":"…","expiresAt":…,"realm":"cn"}}'
            value={value}
            onChange={(e) => setValue(e.target.value)}
            disabled={busy}
          />
        </>
      )}
      {platform === 'qoder' && (
        <>
          <p className="add-account__hint">
            打开 Qoder 客户端 → 设置/账号 → 生成 Personal Access Token（形如 <code>pt-xxxxxxxx</code>），粘贴到下面：
          </p>
          <input
            type="text"
            className="add-account__input"
            aria-label={ACCOUNTS.ADD_QO_LABEL}
            placeholder="pt-xxxxxxxx…"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            disabled={busy}
          />
        </>
      )}
      <div className="row">
        <button type="button" className="btn btn--primary" disabled={busy || !canSubmit} onClick={() => void submit()}>
          {busy ? ACCOUNTS.ADD_SAVING : ACCOUNTS.ADD_SAVE}
        </button>
      </div>
      {okId !== null && (
        <p className="add-account__ok" role="status">
          {ACCOUNTS.ADD_OK_PREFIX} {okId} {ACCOUNTS.ADD_OK_SUFFIX}
        </p>
      )}
      {error !== null && (
        <p className="add-account__error" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
