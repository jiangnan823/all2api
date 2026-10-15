import { useCallback, useEffect, useRef, useState } from 'react';
import { LOGS } from '../i18n/zh';
import {
  LOGS_POLL_MS,
  fetchLogs,
  logLevelTone,
  logMatchesFilter,
  type LogLevelFilter,
  type LogsSnapshot,
} from '../lib/poolApi';

/**
 * 运行日志屏（M5b）：等宽日志列表（时间戳 + 级别徽色 + 正文）+ 级别过滤（全部/info/error）
 * + 自动滚动开关（默认开）+ 手动刷新；10s 轮询。数据端点 GET /api/logs?limit=200
 * 由后端并行批交付：运行期 404 如实展示错误态。全部内存态，不落 storage。
 */

type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; data: LogsSnapshot };

const TONE_CLASS: Record<ReturnType<typeof logLevelTone>, string> = {
  err: 'logs-line__level--err',
  warn: 'logs-line__level--warn',
  idle: 'logs-line__level--idle',
};

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
 * 日志屏容器：加载/错误/空态/就绪四态；就绪态提供级别过滤与自动滚动开关。
 * 过滤为纯展示层（不重拉接口）；自动滚动开着时，新数据到达把列表底边推入视野。
 */
export function LogsScreen() {
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
    <div className="pool-screen logs-screen">
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
    </div>
  );
}
