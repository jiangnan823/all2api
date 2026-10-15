import { useCallback, useEffect, useRef, useState } from 'react';
import { ACCOUNTS, GROWTH } from '../i18n/zh';
import { GrowthTasksModal } from '../components/GrowthTasksModal';
import {
  ACCOUNTS_POLL_MS,
  addQoderAccount,
  addWorkbuddyAccount,
  fetchAccounts,
  pauseAccount,
  relativeTime,
  removeAccount,
  resumeAccount,
  statusTone,
  type AccountEntry,
  type AccountsSnapshot,
} from '../lib/poolApi';

/**
 * 账号池屏（M6b，向 wb2api 看齐的富列表格）：状态卡行（总数/可用/冷却中/禁用/积分合计，等宽数字）
 * + 账号表格（平台/账号=昵称+短码可复制/状态徽标/积分=剩余/总额+进度条/成功 失败/在途/健康度/
 * 到期/冷却至/最近成功=相对时间/行内操作=任务(M7b 成长任务弹层)|暂停选号|恢复选号+删除）+ 手动刷新 + 30s 轮询。
 * 行内操作走 POST /api/accounts/{platform}:{id}/{pause|resume|remove}，删除前 confirm（文案含
 * 「凭证文件将被删除」）；{error} 信封逐字上抛展示。全部内存态，不落 storage。
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

/** 状态串 → 中文徽标文案（未知值原样显示，不翻译瞎猜）。disabled 按 M6b 验收口径显示「凭证失效」。 */
const STATUS_LABEL: Record<string, string> = {
  active: '可用',
  cooling: '冷却中',
  disabled: '凭证失效',
};

function statusLabel(status: string): string {
  return STATUS_LABEL[status.toLowerCase()] ?? status;
}

/** 账号 ID → 短码副行：前 8 位…后 4 位（≤12 位原样），全量在 title 与复制内容里。 */
function shortId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id;
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

/**
 * 账号 ID 短码 + 点击复制全量（M6b：昵称主显后 id 降为短码副行，全量在 title/复制内容里；
 * clipboard API 不可用时如实不提示成功，不假成功）。
 */
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
      title={id}
      aria-label={`${ACCOUNTS.COPY}: ${id}`}
    >
      <code>{shortId(id)}</code>
      {copied && (
        <span className="pool-copy__hint" role="status">
          {ACCOUNTS.COPIED}
        </span>
      )}
    </button>
  );
}

type AccountAction = 'pause' | 'resume' | 'remove';

/** 状态列：paused 判定优先于 status（灰徽「已暂停选号」）；否则按 statusTone 徽色 + 中文文案。 */
function StatusBadge(props: { entry: AccountEntry }) {
  const { entry: a } = props;
  if (a.paused) {
    return <span className="pool-badge pool-badge--idle">{ACCOUNTS.STATUS_PAUSED}</span>;
  }
  return <span className={`pool-badge ${TONE_CLASS[statusTone(a.status)]}`}>{statusLabel(a.status)}</span>;
}

/**
 * 积分列（M6b）：「剩余 / 总额」双值 + 细进度条（剩余/总额）。
 * 总额缺失或为 0（契约：未知/未播种为 0，与缺失同义）、或剩余缺失 → 只显单值不画条；
 * 百分比钳在 0–100（剩余>总额的服务端异常如实显示数字，条不越界）。
 */
function CreditCell(props: { entry: AccountEntry }) {
  const { credit, creditTotal } = props.entry;
  const hasTotal = creditTotal !== null && creditTotal > 0;
  const pct =
    hasTotal && credit !== null ? Math.max(0, Math.min(100, Math.round((credit / creditTotal) * 100))) : null;
  return (
    <div className="pool-credit">
      <span className="pool-table__num">
        {credit === null ? ACCOUNTS.DASH : credit.toLocaleString()}
        {hasTotal && credit !== null && <span className="pool-credit__of"> / {creditTotal.toLocaleString()}</span>}
      </span>
      {pct !== null && (
        <span className="pool-creditbar" title={`剩余 ${credit} / ${creditTotal}（${pct}%）`} aria-hidden="true">
          <i style={{ width: `${pct}%` }} />
        </span>
      )}
    </div>
  );
}

function AccountRow(props: {
  entry: AccountEntry;
  showPlatform: boolean;
  busy: boolean;
  onAction: (action: AccountAction, entry: AccountEntry) => void;
  onTasks: (entry: AccountEntry) => void;
}) {
  const { entry: a, showPlatform, busy, onAction, onTasks } = props;
  return (
    <tr>
      {showPlatform ? <td>{a.platform}</td> : null}
      <td>
        {a.nickname ? <span className="pool-table__nick">{a.nickname}</span> : null}
        <AccountIdCell id={a.id} />
      </td>
      <td>
        <StatusBadge entry={a} />
      </td>
      <td>
        <CreditCell entry={a} />
      </td>
      <td className="pool-table__num">
        {a.success.toLocaleString()} /{' '}
        <span className={a.fail > 0 ? 'pool-fail pool-fail--bad' : 'pool-fail'}>{a.fail.toLocaleString()}</span>
      </td>
      <td className="pool-table__num">
        {a.inFlight > 0 ? <span className="pool-num--hl">{a.inFlight}</span> : <span className="pool-num--dim">0</span>}
      </td>
      <td>{a.health ?? ACCOUNTS.DASH}</td>
      <td className="pool-table__num">{fmtUnix(a.expiresAt, ACCOUNTS.DASH)}</td>
      <td className="pool-table__num">{cooldownLabel(a.cooldownEnd)}</td>
      <td className="pool-table__num">{relativeTime(a.lastSuccess) ?? ACCOUNTS.DASH}</td>
      <AccountActions entry={a} busy={busy} onAction={onAction} onTasks={onTasks} />
    </tr>
  );
}

/** 行内操作（M6b + M7b）：任务(成长任务弹层) + 暂停/恢复选号 + 删除（删除确认在容器层统一拦）。 */
function AccountActions(props: {
  entry: AccountEntry;
  busy: boolean;
  onAction: (action: AccountAction, entry: AccountEntry) => void;
  onTasks: (entry: AccountEntry) => void;
}) {
  const { entry: a, busy, onAction, onTasks } = props;
  return (
    <td className="pool-acts">
      <button type="button" className="pool-act" onClick={() => onTasks(a)}>
        {GROWTH.BTN_OPEN}
      </button>
      {a.paused ? (
        <button
          type="button"
          className="pool-act pool-act--primary"
          disabled={busy}
          onClick={() => onAction('resume', a)}
        >
          {ACCOUNTS.BTN_RESUME}
        </button>
      ) : (
        <button type="button" className="pool-act" disabled={busy} onClick={() => onAction('pause', a)}>
          {ACCOUNTS.BTN_PAUSE}
        </button>
      )}
      <button
        type="button"
        className="pool-act pool-act--danger"
        disabled={busy}
        onClick={() => onAction('remove', a)}
      >
        {ACCOUNTS.BTN_REMOVE}
      </button>
    </td>
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
  // M6b 行内操作：同账号按钮在请求期间禁用；{error} 信封原文展示（不翻译不包装）
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  // M7b 成长任务弹层：行内「任务」按钮的目标账号（弹层自身拉数据，池子轮询不干扰）
  const [growthTarget, setGrowthTarget] = useState<{ platform: string; id: string; nickname: string | null } | null>(
    null,
  );

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

  const runAction = useCallback(
    async (action: AccountAction, entry: AccountEntry) => {
      // 删除是不可逆操作（后端删凭证文件），confirm 拦截文案必须说清后果
      if (action === 'remove' && !window.confirm(ACCOUNTS.CONFIRM_REMOVE)) return;
      const key = `${entry.platform}:${entry.id}`;
      setBusyKey(key);
      setActionError(null);
      try {
        if (action === 'pause') await pauseAccount(entry.platform, entry.id);
        else if (action === 'resume') await resumeAccount(entry.platform, entry.id);
        else await removeAccount(entry.platform, entry.id);
        await load(); // 以服务端快照为准重建表格：删除成功即从表格消失
      } catch (err) {
        setActionError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusyKey(null);
      }
    },
    [load],
  );

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
          ? `${ACCOUNTS.EMPTY_BODY} ${ACCOUNTS.EMPTY_BODY_QO}`
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
      {actionError !== null && (
        <p className="pool-acterr" role="alert">
          {ACCOUNTS.ACTION_ERR_PREFIX}
          {actionError}
        </p>
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

      {/* M6b 列数增加，窄屏横向滚动，表格自身不再撑破版心 */}
      <div className="pool-scroll">
        <table className="pool-table">
          <thead>
            <tr>
              {filter ? null : <th scope="col">{ACCOUNTS.COL_PLATFORM}</th>}
              <th scope="col">{ACCOUNTS.COL_ACCOUNT}</th>
              <th scope="col">{ACCOUNTS.COL_STATUS}</th>
              <th scope="col" title="上游平台的点数余额：签到/任务/充值获得，对话按用量扣除">
                {ACCOUNTS.COL_CREDIT}
              </th>
              <th scope="col" title="本次运行累计：成功 / 失败">
                {ACCOUNTS.COL_SUCCESS}
              </th>
              <th scope="col" title="已发出、尚未返回响应的请求数">
                {ACCOUNTS.COL_INFLIGHT}
              </th>
              <th scope="col" title="近期请求质量分（0–1）：成功上升、失败下降；新账号从 1.0 起步">
                {ACCOUNTS.COL_HEALTH}
              </th>
              <th scope="col">{ACCOUNTS.COL_EXPIRES}</th>
              <th scope="col">{ACCOUNTS.COL_COOLDOWN}</th>
              <th scope="col" title="最近一次请求成功的时间">
                {ACCOUNTS.COL_LASTOK}
              </th>
              <th scope="col">{ACCOUNTS.COL_ACTIONS}</th>
            </tr>
          </thead>
          <tbody>
            {scoped.map((a) => (
              <AccountRow
                key={`${a.platform}:${a.id}`}
                entry={a}
                showPlatform={!filter}
                busy={busyKey === `${a.platform}:${a.id}`}
                onAction={(action, entry) => void runAction(action, entry)}
                onTasks={(entry) => setGrowthTarget({ platform: entry.platform, id: entry.id, nickname: entry.nickname })}
              />
            ))}
          </tbody>
        </table>
      </div>

      {growthTarget !== null && (
        <GrowthTasksModal
          platform={growthTarget.platform}
          id={growthTarget.id}
          nickname={growthTarget.nickname}
          onClose={() => setGrowthTarget(null)}
        />
      )}
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
