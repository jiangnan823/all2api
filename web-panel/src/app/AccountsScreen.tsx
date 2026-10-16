import { useCallback, useEffect, useRef, useState } from 'react';
import { ACCOUNTS, GROWTH, LOGIN } from '../i18n/zh';
import { BatchActions } from '../components/BatchActions';
import { UsageBadges } from '../components/UsageBadges';
import { GrowthTasksModal, ModelLocksCard, PackagesCard } from '../components';
import {
  ACCOUNTS_POLL_MS,
  addQoderAccount,
  addWorkbuddyAccount,
  checkinAccount,
  expiryLabel,
  fetchAccounts,
  refreshAccountBalance,
  pauseAccount,
  relativeTime,
  removeAccount,
  resumeAccount,
  setAccountDisabled,
  statusTone,
  type AccountEntry,
  type AccountsSnapshot,
} from '../lib/poolApi';
import { accountUsageFor, fetchUsage, type UsageSnapshot } from '../lib/usageApi';
import { LOGIN_POLL_MS, pollWorkbuddyLogin, startWorkbuddyLogin } from '../lib/loginApi';
import { ApiError } from '../lib/panelApi';
import '../components/PoolExtras.css';

/**
 * 账号池屏（M6b 富列表格 → M10-C 对齐 wb2api）：状态卡行（总数/可用/冷却/禁用/已暂停选号/
 * 粘性会话/积分剩余·总额双值）+ 批量操作（全部签到/旅行巡检/活跃上报/全部保活，travel 仅
 * workbuddy）+ 账号表格（平台/账号/状态徽标+status_note 副行/积分双值+进度条/成功 失败/在途/
 * 用量徽章组/健康度/到期=绝对日期+相对天数/冷却至/最近成功=相对时间/行内操作=任务|签到|余额|
 * 暂停恢复|禁用启用|删除）+ 手动刷新 + 30s 轮询。行内操作走 POST /api/accounts/{platform}:{id}/…，
 * 删除前 confirm（文案含「凭证文件将被删除」）；{error} 信封逐字上抛展示。全部内存态，不落 storage。
 * M8：添加向导双 tab——workbuddy 新增「浏览器登录」（设备授权流）与「粘贴凭证 JSON」并列。
 */

/** M10-C 新增文案（zh.ts 本批冻结，不改动；既有键引用原样保留）。 */
const M10 = {
  CARD_STICKY: '粘性会话',
  COL_USAGE: '用量',
  COL_USAGE_TITLE: '本次运行按账号累计：请求次数 / token / 耗时 / tok/s',
  BTN_DISABLE: '禁用',
  BTN_ENABLE: '启用',
  CREDIT_DUAL_TITLE: '剩余 / 总额',
} as const;
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

type AccountAction = 'pause' | 'resume' | 'remove' | 'checkin' | 'balance' | 'disable' | 'enable';

/**
 * 状态列（M10-C）：徽标 + 副行——paused 判定优先于 status（灰徽「已暂停选号」）；
 * 否则按 statusTone 徽色 + 中文文案；status_note 非空时在徽标下方以小字渲染（如 429 rate limit）。
 */
function StatusBadge(props: { entry: AccountEntry }) {
  const { entry: a } = props;
  return (
    <span className="pool-status">
      {a.paused ? (
        <span className="pool-badge pool-badge--idle">{ACCOUNTS.STATUS_PAUSED}</span>
      ) : (
        <span className={`pool-badge ${TONE_CLASS[statusTone(a.status)]}`}>{statusLabel(a.status)}</span>
      )}
      {a.statusNote !== '' && (
        <span className="pool-status__note" title={a.statusNote}>
          {a.statusNote}
        </span>
      )}
    </span>
  );
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

/** 到期列（M10-C）：绝对日期 + 相对天数双值（wb2api 同式）；无到期概念显示 —。 */
function ExpiryCell(props: { unix: number | null }) {
  const label = expiryLabel(props.unix);
  if (label === null) return <td className="pool-table__num">{ACCOUNTS.DASH}</td>;
  return (
    <td className="pool-table__num">
      <span className="pool-exp">
        <span className="pool-exp__date" title={`到期于 ${label.date}（${label.rel}）`}>
          {label.date}
        </span>
        <span className="pool-exp__rel">（{label.rel}）</span>
      </span>
    </td>
  );
}

function AccountRow(props: {
  entry: AccountEntry;
  showPlatform: boolean;
  busy: boolean;
  usage: UsageSnapshot | null;
  onAction: (action: AccountAction, entry: AccountEntry) => void;
  onTasks: (entry: AccountEntry) => void;
}) {
  const { entry: a, showPlatform, busy, usage, onAction, onTasks } = props;
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
      <td>
        <UsageBadges usage={accountUsageFor(usage, a.id)} />
      </td>
      <td>{a.health ?? ACCOUNTS.DASH}</td>
      <ExpiryCell unix={a.expiresAt} />
      <td className="pool-table__num">{cooldownLabel(a.cooldownEnd)}</td>
      <td className="pool-table__num">{relativeTime(a.lastSuccess) ?? ACCOUNTS.DASH}</td>
      <AccountActions entry={a} busy={busy} onAction={onAction} onTasks={onTasks} />
    </tr>
  );
}

/**
 * 行内操作（M6b + M7b + M10-C）：任务(成长任务弹层) + 签到/余额 + 暂停/恢复选号 +
 * 禁用/启用（按当前 status 切换文案；disabled → 启用，其余 → 禁用）+ 删除（删除确认在容器层统一拦）。
 */
function AccountActions(props: {
  entry: AccountEntry;
  busy: boolean;
  onAction: (action: AccountAction, entry: AccountEntry) => void;
  onTasks: (entry: AccountEntry) => void;
}) {
  const { entry: a, busy, onAction, onTasks } = props;
  const isDisabled = a.status.toLowerCase() === 'disabled';
  return (
    <td className="pool-acts">
      <button type="button" className="pool-act" onClick={() => onTasks(a)}>
        {GROWTH.BTN_OPEN}
      </button>
      <button
        type="button"
        className="pool-act"
        disabled={busy}
        title={ACCOUNTS.BTN_CHECKIN_TITLE}
        onClick={() => onAction('checkin', a)}
      >
        {ACCOUNTS.BTN_CHECKIN}
      </button>
      <button
        type="button"
        className="pool-act"
        disabled={busy}
        title={ACCOUNTS.BTN_BALANCE_TITLE}
        onClick={() => onAction('balance', a)}
      >
        {ACCOUNTS.BTN_BALANCE}
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
      {isDisabled ? (
        <button
          type="button"
          className="pool-act"
          disabled={busy}
          title="重新启用该账号（回到选号池）"
          onClick={() => onAction('enable', a)}
        >
          {M10.BTN_ENABLE}
        </button>
      ) : (
        <button
          type="button"
          className="pool-act"
          disabled={busy}
          title="停用该账号（凭证保留，仅退出选号池）"
          onClick={() => onAction('disable', a)}
        >
          {M10.BTN_DISABLE}
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
  // M10-C 用量列：GET /api/usage accounts 桶（短码 join）。辅助数据——拉取失败置 null
  // （用量列全 "—"），绝不拖垮账号池屏本体。
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
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

  const loadUsage = useCallback(async () => {
    const seq = ++usageSeqRef.current;
    try {
      const data = await fetchUsage();
      if (seq === usageSeqRef.current) setUsage(data);
    } catch {
      if (seq === usageSeqRef.current) setUsage(null);
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
        else if (action === 'checkin') await checkinAccount(entry.platform, entry.id);
        else if (action === 'balance') await refreshAccountBalance(entry.platform, entry.id);
        else if (action === 'disable') await setAccountDisabled(entry.platform, entry.id, true);
        else if (action === 'enable') await setAccountDisabled(entry.platform, entry.id, false);
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
    void loadUsage();
    const timer = window.setInterval(() => void load(), ACCOUNTS_POLL_MS);
    const usageTimer = window.setInterval(() => void loadUsage(), ACCOUNTS_POLL_MS);
    return () => {
      window.clearInterval(timer);
      window.clearInterval(usageTimer);
    };
  }, [load, loadUsage]);

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
  // M10-C：已暂停/积分剩余·总额 在切片页按账号行重算（服务端汇总字段是全网关口径）；
  // 粘性会话是网关级概念（无按平台拆分的 wire 字段），恒取快照汇总值。
  const scoped = filter ? data.accounts.filter((a) => a.platform === filter) : data.accounts;
  const agg = filter
    ? {
        total: scoped.length,
        active: scoped.filter((a) => a.status === 'active').length,
        cooling: scoped.filter((a) => a.status === 'cooling').length,
        disabled: scoped.filter((a) => a.status === 'disabled').length,
        paused: scoped.filter((a) => a.paused).length,
        sticky: data.stickySessions,
        creditSum: scoped.reduce((sum, a) => sum + (a.credit ?? 0), 0),
        creditTotal: scoped.reduce((sum, a) => sum + (a.creditTotal ?? 0), 0),
      }
    : {
        total: data.total,
        active: data.active,
        cooling: data.cooling,
        disabled: data.disabled,
        paused: data.pausedCount,
        sticky: data.stickySessions,
        creditSum: data.creditSum,
        creditTotal: data.creditTotal,
      };
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
        {filter !== 'trae' && wizardOpen && filter !== undefined && (
          <AddAccountPanel
            platform={filter}
            onAdded={() => {
              setWizardOpen(false);
              void load();
            }}
          />
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
          <span className="pool-card__label">{ACCOUNTS.STATUS_PAUSED}</span>
          <span className="pool-card__value pool-card__value--dim">{agg.paused}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{M10.CARD_STICKY}</span>
          <span className="pool-card__value">{agg.sticky}</span>
        </li>
        <li className="pool-card">
          <span className="pool-card__label">{ACCOUNTS.CARD_CREDIT}</span>
          <span className="pool-card__value" title={M10.CREDIT_DUAL_TITLE}>
            {fmtCredit(agg.creditSum, ACCOUNTS.DASH)}
            <span className="pool-card__sub"> / {fmtCredit(agg.creditTotal, ACCOUNTS.DASH)}</span>
          </span>
        </li>
      </ul>

      {/* M10-C 批量操作（wb2api 卡头同款）：travel 仅 workbuddy；完成回调重拉快照 */}
      {filter !== undefined && <BatchActions platform={filter} onDone={() => void load()} />}

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
              <th scope="col" title={M10.COL_USAGE_TITLE}>
                {M10.COL_USAGE}
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
                usage={usage}
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
      {filter !== undefined && (
        <>
          {/* 积分到期提醒是 WorkBuddy 专属面（上游只有它有积分包）——qoder/trae 页不渲染 */}
          {filter === 'workbuddy' && <PackagesCard />}
          {/* 模型锁池按平台域过滤：各软件页只看自己的锁 */}
          <ModelLocksCard platform={filter} />
        </>
      )}
    </div>
  );
}

/**
 * 添加账号面板（v1.2 §1a，主人裁定：添加账号是面板内一等操作；M8 起向导化）：
 * - workbuddy：双 tab——「浏览器登录」（设备授权流，默认）与「粘贴凭证 JSON」（P0 直通）
 * - qoder：粘贴 PAT（pt-…）
 * 校验与落盘在后端；成功后回调刷新账号池；字段校验错误（400 + {error}）逐字显示，不静默兜底。
 */
function AddAccountPanel(props: { platform: 'workbuddy' | 'qoder' | 'trae'; onAdded: () => void }) {
  const { platform } = props;
  // M8：workbuddy 双 tab（浏览器登录为主口）；qoder 保持单表单。
  const [tab, setTab] = useState<'login' | 'paste'>('login');
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

  // 粘贴 tab 内容（workbuddy JSON / qoder PAT 共用骨架；错误与成功提示逐字上抛）。
  const pasteBody = (
    <>
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
    </>
  );

  if (platform === 'qoder') {
    return (
      <div className="add-account" role="form" aria-label={ACCOUNTS.ADD_TITLE}>
        <h3 className="add-account__title">{ACCOUNTS.ADD_TITLE}</h3>
        {pasteBody}
      </div>
    );
  }

  // workbuddy：双 tab。两个 tab 都保持挂载（hidden 切换可见性）——浏览器登录的
  // 3s 轮询不因用户切去看粘贴 tab 而中断，完成后自动刷新账号池。
  return (
    <div className="add-account" role="form" aria-label={ACCOUNTS.ADD_TITLE}>
      <h3 className="add-account__title">{ACCOUNTS.ADD_TITLE}</h3>
      <div className="row" role="tablist" aria-label={ACCOUNTS.ADD_TITLE}>
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'login'}
          className="btn"
          onClick={() => setTab('login')}
        >
          {LOGIN.TAB_BROWSER}
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'paste'}
          className="btn"
          onClick={() => setTab('paste')}
        >
          {LOGIN.TAB_PASTE}
        </button>
      </div>
      <div hidden={tab !== 'login'}>
        <BrowserLoginTab onAdded={props.onAdded} />
      </div>
      <div hidden={tab !== 'paste'}>{pasteBody}</div>
    </div>
  );
}

/**
 * 浏览器登录 tab（M8 设备授权流）：生成链接 → 打开/复制 → 每 3s 自动轮询
 * （单飞：上一发未返回不发下一发）→ 完成提示账号昵称；过期/失败态如实展示并可重新生成。
 * 轮询间的瞬态传输故障（网络错误/5xx）不终止等待——登录会话有 15 分钟 TTL 兜底；
 * 语义性失败（4xx / {status:"error"} / 形状异常）才落到失败态。
 * 全部内存态，不落 storage；凭证材料只经后端，本组件不接触。
 */
type LoginFlow =
  | { phase: 'idle' }
  | { phase: 'starting' }
  | { phase: 'waiting'; state: string; url: string }
  | { phase: 'done'; uid: string; nickname: string }
  | { phase: 'expired' }
  | { phase: 'error'; message: string };

function BrowserLoginTab(props: { onAdded: () => void }) {
  const { onAdded } = props;
  const [flow, setFlow] = useState<LoginFlow>({ phase: 'idle' });
  const [copied, setCopied] = useState(false);
  const inFlightRef = useRef(false);
  const timerRef = useRef<number | null>(null);
  const copiedTimer = useRef<number | null>(null);

  useEffect(
    () => () => {
      if (timerRef.current !== null) window.clearInterval(timerRef.current);
      if (copiedTimer.current !== null) window.clearTimeout(copiedTimer.current);
    },
    [],
  );

  const start = async () => {
    setFlow({ phase: 'starting' });
    try {
      const res = await startWorkbuddyLogin();
      setFlow({ phase: 'waiting', state: res.state, url: res.loginUrl });
    } catch (err) {
      setFlow({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
    }
  };

  // 轮询循环：仅在 waiting 相位活跃；flow 变更即重建（pending 不 setFlow，节奏稳定）。
  useEffect(() => {
    if (flow.phase !== 'waiting') return undefined;
    const state = flow.state;
    let stopped = false;
    const tick = async () => {
      if (stopped || inFlightRef.current) return;
      inFlightRef.current = true;
      try {
        const res = await pollWorkbuddyLogin(state);
        if (stopped) return;
        if (res.status === 'pending') return; // 继续等
        if (res.status === 'done') {
          setFlow({ phase: 'done', uid: res.uid, nickname: res.nickname });
          return;
        }
        if (res.status === 'expired') setFlow({ phase: 'expired' });
        else setFlow({ phase: 'error', message: res.error });
      } catch (err) {
        if (stopped) return;
        // 瞬态传输故障不杀等待：面板↔网关同源，TTL 是最终兜底。
        const status = err instanceof ApiError ? err.status : undefined;
        if (status === undefined || (status >= 500 && status <= 599)) return;
        setFlow({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
      } finally {
        inFlightRef.current = false;
      }
    };
    void tick();
    timerRef.current = window.setInterval(() => void tick(), LOGIN_POLL_MS);
    return () => {
      stopped = true;
      if (timerRef.current !== null) {
        window.clearInterval(timerRef.current);
        timerRef.current = null;
      }
    };
  }, [flow]);

  const copyLink = () => {
    if (flow.phase !== 'waiting') return;
    navigator.clipboard
      ?.writeText(flow.url)
      .then(() => {
        setCopied(true);
        if (copiedTimer.current !== null) window.clearTimeout(copiedTimer.current);
        copiedTimer.current = window.setTimeout(() => setCopied(false), 1500);
      })
      .catch(() => setCopied(false));
  };

  const openPage = () => {
    if (flow.phase === 'waiting') window.open(flow.url, '_blank', 'noopener');
  };

  return (
    <>
      <p className="add-account__hint">{LOGIN.LINK_HINT}</p>
      {(flow.phase === 'idle' || flow.phase === 'starting') && (
        <div className="row">
          <button
            type="button"
            className="btn btn--primary"
            disabled={flow.phase === 'starting'}
            onClick={() => void start()}
          >
            {flow.phase === 'starting' ? LOGIN.STARTING : LOGIN.START}
          </button>
        </div>
      )}
      {flow.phase === 'waiting' && (
        <>
          <div className="row">
            <a href={flow.url} target="_blank" rel="noopener noreferrer" className="pool-note">
              {flow.url}
            </a>
          </div>
          <div className="row">
            <button type="button" className="btn" onClick={openPage}>
              {LOGIN.OPEN_PAGE}
            </button>
            <button type="button" className="btn" onClick={copyLink}>
              {copied ? LOGIN.COPIED : LOGIN.COPY_LINK}
            </button>
            <button type="button" className="btn" onClick={() => void start()}>
              {LOGIN.RESTART}
            </button>
          </div>
          <p className="add-account__hint" role="status">
            {LOGIN.WAITING}
          </p>
        </>
      )}
      {flow.phase === 'done' && (
        <>
          <p className="add-account__ok" role="status">
            {LOGIN.DONE_PREFIX} {flow.nickname !== '' ? flow.nickname : flow.uid}
            {flow.nickname !== '' ? `（${flow.uid}）` : ''}
          </p>
          <div className="row">
            <button type="button" className="btn btn--primary" onClick={onAdded}>
              {LOGIN.FINISH}
            </button>
            <button type="button" className="btn" onClick={() => void start()}>
              {LOGIN.RESTART}
            </button>
          </div>
        </>
      )}
      {(flow.phase === 'expired' || flow.phase === 'error') && (
        <>
          <p className="add-account__error" role="alert">
            {flow.phase === 'expired' ? LOGIN.EXPIRED : `${LOGIN.ERROR_PREFIX}${flow.message}`}
          </p>
          <div className="row">
            <button type="button" className="btn btn--primary" onClick={() => void start()}>
              {LOGIN.RESTART}
            </button>
          </div>
        </>
      )}
    </>
  );
}
