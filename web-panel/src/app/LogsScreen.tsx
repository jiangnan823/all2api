import { useCallback, useEffect, useRef, useState } from 'react';
import { LOGS, settingsLine } from '../i18n/zh';
import {
  LOGS_POLL_MS,
  fetchLogs,
  logLevelTone,
  logMatchesFilter,
  type LogLevelFilter,
  type LogsSnapshot,
} from '../lib/poolApi';
import {
  REQUESTS_FETCH_LIMIT,
  REQUESTS_POLL_MS,
  fetchRequests,
  type RequestResult,
  type RequestResultFilter,
  type RequestRow,
  type RequestsSnapshot,
} from '../lib/usageApi';

/**
 * 运行日志屏（M5b 日志 + M8-reqlog 请求记录）：顶部双 tab ——
 * 「运行日志」为等宽日志列表（时间戳 + 级别徽色 + 正文）+ 级别过滤 + 自动滚动，
 * 10s 轮询 GET /api/logs?limit=200；「请求记录」（M8）为 /v1 请求表
 * （时间/结果徽标/模型/账号短码/耗时/Token 列）+ 结果筛选下拉 + 刷新，
 * 30s 轮询 GET /api/requests?limit=300&result=…（结果过滤为服务端口径）。
 * 两个视图各自持有加载/错误/空/就绪四态，只挂载激活 tab（不挂载不轮询）。
 * 全部内存态，不落 storage。
 */

/** 屏内 tab：运行日志（默认，兼容 M5b 行为）| 请求记录（M8）。 */
type LogTab = 'runtime' | 'requests';

type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; data: LogsSnapshot };

type RequestsState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; data: RequestsSnapshot };

const TONE_CLASS: Record<ReturnType<typeof logLevelTone>, string> = {
  err: 'logs-line__level--err',
  warn: 'logs-line__level--warn',
  idle: 'logs-line__level--idle',
};

/** 请求结果 → 徽标档：非冻结枚举值如实落「未知」灰档，不编造语义。 */
function resultTone(result: RequestResult): 'ok' | 'err' | 'warn' | 'idle' {
  switch (result) {
    case 'success':
      return 'ok';
    case 'error':
    case 'stream_error':
      return 'err';
    case 'interrupted':
      return 'warn';
    default:
      return 'idle';
  }
}

function resultLabel(result: RequestResult): string {
  switch (result) {
    case 'success':
      return LOGS.RESULT_SUCCESS;
    case 'error':
      return LOGS.RESULT_ERROR;
    case 'stream_error':
      return LOGS.RESULT_STREAM_ERROR;
    case 'interrupted':
      return LOGS.RESULT_INTERRUPTED;
    default:
      return LOGS.RESULT_UNKNOWN;
  }
}

/** Unix 秒 → HH:MM:SS 展示串；null = —（不编造时间）。 */
function fmtTs(sec: number | null): string {
  if (sec === null || sec === 0) return LOGS.DASH;
  const d = new Date(sec * 1000);
  if (Number.isNaN(d.getTime())) return LOGS.DASH;
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function LogLine(props: { ts: number | null; level: string; line: string }) {
  const { ts, level, line } = props;
  return (
    <li className="logs-line">
      <time className="logs-line__ts">{fmtTs(ts)}</time>
      <span className={`logs-line__level ${TONE_CLASS[logLevelTone(level)]}`}>{level}</span>
      <span className="logs-line__text">{line}</span>
    </li>
  );
}

/**
 * 运行日志 tab（原 M5b LogsScreen 主体原样下沉）：加载/错误/空态/就绪四态；
 * 就绪态提供级别过滤与自动滚动开关。过滤为纯展示层（不重拉接口）。
 */
function RuntimeLogPanel() {
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
  const [filter, setFilter] = useState<LogLevelFilter>('all');
  const [autoScroll, setAutoScroll] = useState(true);
  const seqRef = useRef(0);
  const listRef = useRef<HTMLUListElement | null>(null);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    try {
      const data = await fetchLogs();
      if (seq === seqRef.current) setState({ phase: 'ready', data });
    } catch (err) {
      if (seq === seqRef.current) {
        setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
      }
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), LOGS_POLL_MS);
    return () => window.clearInterval(timer);
  }, [load]);

  // 自动滚动：每次数据提交后把视口推到列表底部（关掉开关即停止追随，手动滚动不受干扰）
  useEffect(() => {
    if (state.phase !== 'ready' || !autoScroll) return;
    const el = listRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [state, autoScroll, filter]);

  if (state.phase === 'loading') {
    return <p className="pool-note" role="status">{LOGS.LOADING}</p>;
  }

  if (state.phase === 'error') {
    return (
      <div className="pool-error" role="alert">
        <h3 className="pool-error__title">{LOGS.ERROR_TITLE}</h3>
        <p className="pool-error__detail">{state.message}</p>
        <p className="pool-error__hint">{LOGS.ERROR_HINT}</p>
        <button type="button" className="btn" onClick={() => void load()}>
          {LOGS.RETRY}
        </button>
      </div>
    );
  }

  const { data } = state;
  const visible = data.logs.filter((entry) => logMatchesFilter(entry, filter));

  return (
    <>
      <div className="pool-screen__toolbar">
        <p className="pool-screen__sub">{LOGS.SUB}</p>
        <button type="button" className="btn" onClick={() => void load()}>
          {LOGS.REFRESH}
        </button>
      </div>

      <div className="logs-screen__controls">
        <fieldset className="logs-screen__filter">
          <legend>{LOGS.LEVEL_LABEL}</legend>
          {(
            [
              ['all', LOGS.FILTER_ALL],
              ['info', LOGS.FILTER_INFO],
              ['error', LOGS.FILTER_ERROR],
            ] as const
          ).map(([value, label]) => (
            <label key={value} className="logs-screen__filter-item">
              <input
                type="radio"
                name="logs-filter"
                value={value}
                checked={filter === value}
                onChange={() => setFilter(value)}
              />
              {label}
            </label>
          ))}
        </fieldset>

        <label className="logs-screen__autoscroll">
          <input
            type="checkbox"
            checked={autoScroll}
            onChange={(e) => setAutoScroll(e.target.checked)}
          />
          {LOGS.AUTOSCROLL}
        </label>
      </div>

      {data.logs.length === 0 ? (
        <div className="pool-empty">
          <h3 className="pool-empty__title">{LOGS.EMPTY_TITLE}</h3>
          <p className="pool-empty__body">{LOGS.EMPTY_BODY}</p>
        </div>
      ) : (
        <ul className="logs-list" aria-label={LOGS.LIST_LABEL} ref={listRef}>
          {visible.map((entry, i) => (
            <LogLine key={`${i}:${entry.ts ?? 'x'}`} ts={entry.ts} level={entry.level} line={entry.line} />
          ))}
        </ul>
      )}
    </>
  );
}

/** 请求记录单行：时间 / 结果徽标 / 模型（含平台） / 账号短码 / 耗时 / Token。 */
function RequestRowView({ row }: { row: RequestRow }) {
  return (
    <tr>
      <td className="growth-table__num" title={row.ts.local}>{fmtTs(row.ts.unix)}</td>
      <td>
        <span className={`pool-badge pool-badge--${resultTone(row.result)}`}>{resultLabel(row.result)}</span>
      </td>
      <td>
        <code>{row.model === '' ? LOGS.DASH : row.model}</code>
        {row.platform !== '' && <span className="logs-line__ts"> · {row.platform}</span>}
      </td>
      <td className="growth-table__num">{row.accountId === '' ? LOGS.DASH : row.accountId}</td>
      <td className="growth-table__num">{settingsLine(LOGS.REQ_MS, { ms: String(row.latencyMs) })}</td>
      <td className="growth-table__num">
        {row.promptTokens === null && row.completionTokens === null
          ? LOGS.DASH
          : `${row.promptTokens ?? LOGS.DASH} / ${row.completionTokens ?? LOGS.DASH}`}
      </td>
    </tr>
  );
}

/**
 * 请求记录 tab（M8）：GET /api/requests?limit=300&result=…，结果筛选为
 * 服务端口径（切档即重拉）；30s 轮询 + 手动刷新。旧→新直陈，不排序改写。
 */
function RequestsPanel() {
  const [state, setState] = useState<RequestsState>({ phase: 'loading' });
  const [filter, setFilter] = useState<RequestResultFilter>('all');
  const seqRef = useRef(0);

  const load = useCallback(async (result: RequestResultFilter) => {
    const seq = ++seqRef.current;
    try {
      const data = await fetchRequests(result, REQUESTS_FETCH_LIMIT);
      if (seq === seqRef.current) setState({ phase: 'ready', data });
    } catch (err) {
      if (seq === seqRef.current) {
        setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
      }
    }
  }, []);

  useEffect(() => {
    void load(filter);
    const timer = window.setInterval(() => void load(filter), REQUESTS_POLL_MS);
    return () => window.clearInterval(timer);
  }, [load, filter]);

  if (state.phase === 'loading') {
    return <p className="pool-note" role="status">{LOGS.REQ_LOADING}</p>;
  }

  if (state.phase === 'error') {
    return (
      <div className="pool-error" role="alert">
        <h3 className="pool-error__title">{LOGS.REQ_ERROR_TITLE}</h3>
        <p className="pool-error__detail">{state.message}</p>
        <p className="pool-error__hint">{LOGS.ERROR_HINT}</p>
        <button type="button" className="btn" onClick={() => void load(filter)}>
          {LOGS.RETRY}
        </button>
      </div>
    );
  }

  const { data } = state;
  return (
    <>
      <div className="pool-screen__toolbar">
        <p className="pool-screen__sub">{LOGS.REQ_SUB}</p>
        <label className="logs-screen__filter-item">
          {LOGS.REQ_FILTER_LABEL}
          <select
            value={filter}
            aria-label={LOGS.REQ_FILTER_LABEL}
            onChange={(e) => setFilter(e.target.value as RequestResultFilter)}
          >
            <option value="all">{LOGS.FILTER_ALL}</option>
            <option value="success">{LOGS.REQ_FILTER_SUCCESS}</option>
            <option value="error">{LOGS.REQ_FILTER_ERROR}</option>
          </select>
        </label>
        <button type="button" className="btn" onClick={() => void load(filter)}>
          {LOGS.REFRESH}
        </button>
      </div>

      {data.requests.length === 0 ? (
        <div className="pool-empty">
          <h3 className="pool-empty__title">{LOGS.REQ_EMPTY_TITLE}</h3>
          <p className="pool-empty__body">{LOGS.REQ_EMPTY_BODY}</p>
        </div>
      ) : (
        <div className="growth-modal__scroll">
          <table className="growth-table" aria-label={LOGS.REQ_LIST_LABEL}>
            <thead>
              <tr>
                <th scope="col">{LOGS.REQ_COL_TIME}</th>
                <th scope="col">{LOGS.REQ_COL_RESULT}</th>
                <th scope="col">{LOGS.REQ_COL_MODEL}</th>
                <th scope="col">{LOGS.REQ_COL_ACCOUNT}</th>
                <th scope="col">{LOGS.REQ_COL_LATENCY}</th>
                <th scope="col">{LOGS.REQ_COL_TOKENS}</th>
              </tr>
            </thead>
            <tbody>
              {data.requests.map((row, i) => (
                <RequestRowView key={`${i}:${row.ts.unix}:${row.requestId ?? 'x'}`} row={row} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

/**
 * 日志屏容器：双 tab（运行日志 / 请求记录），tab 状态纯本地。
 * 只挂载激活 tab 的视图 —— 未激活端点不轮询。
 */
export function LogsScreen() {
  const [tab, setTab] = useState<LogTab>('runtime');

  return (
    <div className="pool-screen logs-screen">
      <div className="pool-screen__toolbar" role="tablist" aria-label={LOGS.TAB_LABEL}>
        <button
          type="button"
          role="tab"
          id="logs-tab-runtime"
          aria-selected={tab === 'runtime'}
          aria-controls="logs-panel-runtime"
          onClick={() => setTab('runtime')}
        >
          {LOGS.TAB_RUNTIME}
        </button>
        <button
          type="button"
          role="tab"
          id="logs-tab-requests"
          aria-selected={tab === 'requests'}
          aria-controls="logs-panel-requests"
          onClick={() => setTab('requests')}
        >
          {LOGS.TAB_REQUESTS}
        </button>
      </div>
      {tab === 'runtime' ? (
        <div role="tabpanel" id="logs-panel-runtime" aria-labelledby="logs-tab-runtime">
          <RuntimeLogPanel />
        </div>
      ) : (
        <div role="tabpanel" id="logs-panel-requests" aria-labelledby="logs-tab-requests">
          <RequestsPanel />
        </div>
      )}
    </div>
  );
}
