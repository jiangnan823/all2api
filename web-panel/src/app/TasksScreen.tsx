import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError, apiGet } from '../lib/panelApi';
import { TASKS, settingsLine, taskDescription, taskDisplayName } from '../i18n/zh';

/**
 * 任务中心屏（M5c）。数据源 GET /api/tasks（端点由并行批交付，此处只消费 wire 契约：
 * {tasks:[{name, cron, enabled, last_run:{total,success,failed,skipped,at}|null}]}；
 * name ∈ signin / active_check / cat_travel / token_keepalive；wire 的 last_run 还可能带
 * name 字段——与任务名重复，解析时忽略不展示。中文名与说明的映射在 i18n
 * （TASK_NAME_COPY / TASK_DESC_COPY），未知任务名按原始标识如实回显（可追溯优于装懂）。
 * 如实边界：手动触发端点不存在（仅账号级 recover）——本屏只读展示「自动调度已注册」，
 * 不渲染任何执行按钮。X-API-Key 经 panelApi provider 注入（与全面板一致）；
 * 不落任何 storage（M1 红线）。
 */

/** 最近一次运行统计（wire 全 snake：last_run.total/success/failed/skipped/at）。 */
interface TaskLastRun {
  total: number;
  success: number;
  failed: number;
  skipped: number;
  /** 服务器本地展示串（与 /api/time 的 local 同源风格），面板原样展示不做时区换算。 */
  at: string;
}

interface TaskEntry {
  name: string;
  cron: string;
  enabled: boolean;
  lastRun: TaskLastRun | null;
}

type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; tasks: TaskEntry[] };

// ---- 防御式解析（形状不对即抛 ApiError，不静默兜底；错误前缀 tasks: 标明来源端点） ----

function asObj(v: unknown, where: string): Record<string, unknown> {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) {
    throw new ApiError(`tasks: ${where} 非对象`);
  }
  return v as Record<string, unknown>;
}

function reqNum(v: unknown, where: string): number {
  if (typeof v !== 'number' || !Number.isFinite(v)) {
    throw new ApiError(`tasks: ${where} 非数值`);
  }
  return v;
}

function reqStr(v: unknown, where: string): string {
  if (typeof v !== 'string' || v === '') {
    throw new ApiError(`tasks: ${where} 非字符串`);
  }
  return v;
}

function parseTasksWire(body: unknown): TaskEntry[] {
  const root = asObj(body, '(root)');
  const arr = root['tasks'];
  if (!Array.isArray(arr)) throw new ApiError('tasks: tasks 非数组');
  return arr.map((v, i) => {
    const t = asObj(v, `tasks[${i}]`);
    const name = reqStr(t['name'], `tasks[${i}].name`);
    const cron = reqStr(t['cron'], `tasks[${i}].cron`);
    if (typeof t['enabled'] !== 'boolean') {
      throw new ApiError(`tasks: tasks[${i}].enabled 非布尔`);
    }
    let lastRun: TaskLastRun | null = null;
    if (t['last_run'] !== null && t['last_run'] !== undefined) {
      const l = asObj(t['last_run'], `tasks[${i}].last_run`);
      lastRun = {
        total: reqNum(l['total'], `tasks[${i}].last_run.total`),
        success: reqNum(l['success'], `tasks[${i}].last_run.success`),
        failed: reqNum(l['failed'], `tasks[${i}].last_run.failed`),
        skipped: reqNum(l['skipped'], `tasks[${i}].last_run.skipped`),
        at: reqStr(l['at'], `tasks[${i}].last_run.at`),
      };
    }
    return { name, cron, enabled: t['enabled'], lastRun };
  });
}

export function TasksScreen() {
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
  /** 过期响应守卫：快速刷新/重试时旧响应不得覆盖新状态。 */
  const seqRef = useRef(0);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    setState({ phase: 'loading' });
    try {
      const tasks = await apiGet('/api/tasks', parseTasksWire);
      if (seq !== seqRef.current) return;
      setState({ phase: 'ready', tasks });
    } catch (err) {
      if (seq !== seqRef.current) return;
      setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  if (state.phase === 'loading') {
    return (
      <div className="tasks-screen__state" role="status">
        {TASKS.LOADING}
      </div>
    );
  }

  if (state.phase === 'error') {
    return (
      <div className="tasks-screen__state tasks-screen__state--error" role="alert">
        <p className="tasks-screen__state-title">{TASKS.ERROR_TITLE}</p>
        <p className="tasks-screen__state-msg">{state.message}</p>
        <p className="tasks-screen__state-hint">{TASKS.ERROR_HINT}</p>
        <button type="button" className="btn" onClick={() => void load()}>
          {TASKS.RETRY}
        </button>
      </div>
    );
  }

  return (
    <div className="tasks-screen">
      {/* 顶部诚实说明：只读 + 无手动触发（端点不存在），不装作有执行能力 */}
      <p className="tasks-screen__note">{TASKS.NOTE}</p>
      <div className="tasks-screen__bar">
        <p className="tasks-screen__bar-note" role="status">
          {settingsLine(TASKS.COUNT, { count: state.tasks.length })}
        </p>
        <button type="button" className="btn" onClick={() => void load()}>
          {TASKS.REFRESH}
        </button>
      </div>
      {state.tasks.length === 0 ? (
        <p className="tasks-screen__state">{TASKS.EMPTY}</p>
      ) : (
        <ul className="tasks-screen__grid">
          {state.tasks.map((t) => (
            <TaskCard key={t.name} task={t} />
          ))}
        </ul>
      )}
    </div>
  );
}

/** 单张任务卡：中文名 + 说明 + cron + 启用徽标 + 最近一次运行统计（null → 尚未运行）。 */
function TaskCard({ task }: { task: TaskEntry }) {
  const last = task.lastRun;
  return (
    <li className="tasks-screen__card" data-task={task.name}>
      <div className="tasks-screen__card-head">
        <h3 className="tasks-screen__card-title">{taskDisplayName(task.name)}</h3>
        <span className={`tasks-screen__badge tasks-screen__badge--${task.enabled ? 'on' : 'off'}`}>
          {task.enabled ? TASKS.ENABLED_ON : TASKS.ENABLED_OFF}
        </span>
      </div>
      <p className="tasks-screen__card-desc">{taskDescription(task.name)}</p>
      <p className="tasks-screen__cron">
        <span>{TASKS.CRON_LABEL}</span>
        <code>{task.cron}</code>
      </p>
      <div className="tasks-screen__lastrun">
        <p className="tasks-screen__lastrun-title">{TASKS.LAST_RUN_TITLE}</p>
        {last === null ? (
          <p className="tasks-screen__never">{TASKS.NEVER_RUN}</p>
        ) : (
          <>
            <dl className="tasks-screen__stats">
              <div className="tasks-screen__stat">
                <dt>{TASKS.STAT_TOTAL}</dt>
                <dd>{last.total}</dd>
              </div>
              <div className="tasks-screen__stat">
                <dt>{TASKS.STAT_SUCCESS}</dt>
                <dd className={last.success > 0 ? 'tasks-screen__stat-num--success' : undefined}>{last.success}</dd>
              </div>
              <div className="tasks-screen__stat">
                <dt>{TASKS.STAT_FAILED}</dt>
                <dd className={last.failed > 0 ? 'tasks-screen__stat-num--failed' : undefined}>{last.failed}</dd>
              </div>
              <div className="tasks-screen__stat">
                <dt>{TASKS.STAT_SKIPPED}</dt>
                <dd className={last.skipped > 0 ? 'tasks-screen__stat-num--skipped' : undefined}>{last.skipped}</dd>
              </div>
            </dl>
            <p className="tasks-screen__ran-at">{settingsLine(TASKS.RAN_AT, { at: last.at })}</p>
          </>
        )}
      </div>
    </li>
  );
}
