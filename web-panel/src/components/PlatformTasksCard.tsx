import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError, apiGet, fetchPanelConfig, savePanelConfig } from '../lib/panelApi';
import type { ConfigPatch, FieldError, PanelConfig } from '../types/api-contract';
import { TASKS, SETTINGS, settingsLine, taskDescription, taskDisplayName } from '../i18n/zh';
import { TimeSlotPicker } from './TimeSlotPicker';

/**
 * 平台定时任务卡（M10-A，任务中心拆散进账号池各平台页）：props.platform ∈ workbuddy/qoder/trae，
 * 数据源 GET /api/tasks 按 task.platform 过滤本平台任务（platform 为 M10 新增 wire 键），
 * 每任务一行：任务名（中文名 + 一行说明）+ 24 小时时点选择器 + 启用徽标。
 * 调度值读 GET /api/config 的冻结配置键、保存走与配置页同一个 POST /api/config
 * （ConfigPatch 只发改动键）；键名冻结不得改：signin_cron_{platform} / task_cron_{platform} /
 * token_keepalive_{platform}（schedule 命名空间下）与 cat_travel_cron（仅 workbuddy 任务会命中）。
 * 如实边界：只做调度编辑，不做「立即运行」（手动触发端点不存在）；启用状态是网关实况
 * （冻结键里没有 enable 键），只读徽标展示；未知任务名无冻结键 → 只读行如实回显，不发明归属。
 * X-API-Key 经 panelApi provider 注入；挂载即拉取 + 60s 低频轮询（只轮任务实况，不打扰草稿）；
 * 全部内存态，不落 storage（M1 红线）。
 */

export type TaskPlatform = 'workbuddy' | 'qoder' | 'trae';

export interface PlatformTasksCardProps {
  platform: TaskPlatform;
}

/** 任务实况（wire 全蛇形：name/cron/enabled/platform；last_run 等未消费键解析时忽略）。 */
interface TaskEntry {
  name: string;
  cron: string;
  enabled: boolean;
  platform: string;
}

type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; tasks: TaskEntry[]; config: PanelConfig };

type SaveState =
  | { phase: 'idle' }
  | { phase: 'saving' }
  | { phase: 'saved' }
  | { phase: 'field-errors'; errors: FieldError[] }
  | { phase: 'failed'; message: string; status?: number };

// ---- 防御式解析（形状不对即抛 ApiError，不静默兜底；错误前缀 tasks: 标明来源端点） ----

function asObj(v: unknown, where: string): Record<string, unknown> {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) {
    throw new ApiError(`tasks: ${where} 非对象`);
  }
  return v as Record<string, unknown>;
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
    return {
      name: reqStr(t['name'], `tasks[${i}].name`),
      cron: reqStr(t['cron'], `tasks[${i}].cron`),
      enabled: t['enabled'] === true,
      platform: reqStr(t['platform'], `tasks[${i}].platform`),
    };
  });
}

/**
 * (任务名, 平台) → 冻结配置键（M10 wire，键名不得改；schedule 命名空间沿用既有 JSON path 约定）。
 * 网关注册的任务名带平台后缀（"signin@workbuddy"，B2 平台化注册），此处剥掉 @后缀再归一。
 * 未知任务名返回 null：无键即无编辑面，渲染只读行，不在前端发明键名。
 */
function taskConfigKey(name: string, platform: TaskPlatform): string | null {
  const at = name.indexOf('@');
  const kind = at === -1 ? name : name.slice(0, at);
  switch (kind) {
    case 'signin': return `schedule.signin_cron_${platform}`;
    case 'active_check': return `schedule.task_cron_${platform}`;
    case 'token_keepalive': return `schedule.token_keepalive_${platform}`;
    case 'cat_travel': return 'schedule.cat_travel_cron';
    default: return null;
  }
}

/** 冻结键 → raw 配置值（键形如 schedule.leaf，取 raw.schedule.leaf；缺失/非串 = 未下发）。 */
function scheduleValue(config: PanelConfig, key: string): string | undefined {
  const sched = config.raw['schedule'];
  if (sched === null || typeof sched !== 'object' || Array.isArray(sched)) return undefined;
  const v = (sched as Record<string, unknown>)[key.slice('schedule.'.length)];
  return typeof v === 'string' ? v : undefined;
}

/**
 * 本平台任务的调度基线（key = 冻结配置键）。配置已下发用配置值；尚未下发（后端并行批
 * 未合入）回落 wire 的 cron 实况——两条数据源同源（调度器注册值即配置值），如实不造数。
 */
function buildBaseline(tasks: TaskEntry[], platform: TaskPlatform, config: PanelConfig): Record<string, string> {
  const baseline: Record<string, string> = {};
  for (const t of tasks) {
    if (t.platform !== platform) continue;
    const key = taskConfigKey(t.name, platform);
    if (key === null) continue;
    // 平台键空串 = 回落全局（B2 语义），编辑基线同样回落任务实况 cron——?? 不跳空串，用 ||。
    baseline[key] = scheduleValue(config, key) || t.cron;
  }
  return baseline;
}

/**
 * 任务卡低频轮询周期（承任务中心 M8-connect 语义）：任务由网关 cron 调度，分钟级变化，
 * 60s 自动重拉任务实况足够（enabled 徽标与新增任务可见）；轮询只合并任务实况，
 * 不重置调度草稿——未保存的编辑不被后台刷新冲掉。
 */
export const TASKS_POLL_MS = 60_000;

export function PlatformTasksCard({ platform }: PlatformTasksCardProps) {
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
  /** 调度草稿（key = 冻结配置键）；基线在 state.ready.config + tasks。 */
  const [drafts, setDrafts] = useState<Record<string, string> | null>(null);
  const [saveState, setSaveState] = useState<SaveState>({ phase: 'idle' });
  /** 过期响应守卫：快速刷新/重试/轮询时旧响应不得覆盖新状态。 */
  const seqRef = useRef(0);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    setSaveState({ phase: 'idle' });
    try {
      const [tasks, config] = await Promise.all([apiGet('/api/tasks', parseTasksWire), fetchPanelConfig()]);
      if (seq !== seqRef.current) return;
      setState({ phase: 'ready', tasks, config });
      setDrafts(null); // 草稿随基线重建（挂载/手动刷新/重试都是显式重置点）
    } catch (err) {
      if (seq !== seqRef.current) return;
      setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
    }
  }, []);

  /** 轮询专用：只重拉任务实况并合并，草稿与配置基线原样保留（见 TASKS_POLL_MS 注释）。 */
  const loadTasksOnly = useCallback(async () => {
    const seq = ++seqRef.current;
    try {
      const tasks = await apiGet('/api/tasks', parseTasksWire);
      if (seq !== seqRef.current) return;
      setState((prev) => (prev.phase === 'ready' ? { ...prev, tasks } : prev));
    } catch {
      // 轮询失败不打断编辑态：错误在下一轮自愈（与任务中心静默刷新同一语义）
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void loadTasksOnly(), TASKS_POLL_MS);
    return () => window.clearInterval(timer);
  }, [load, loadTasksOnly]);

  const model = state.phase === 'ready' ? state : null;
  const scoped = model !== null ? model.tasks.filter((t) => t.platform === platform) : [];
  const baseline = model !== null ? buildBaseline(model.tasks, platform, model.config) : {};
  const dirtyKeys = drafts === null
    ? [] as string[]
    : Object.keys(drafts).filter((k) => drafts[k] !== baseline[k]);

  function setDraft(key: string, expr: string) {
    setDrafts((d) => ({ ...(d ?? baseline), [key]: expr }));
  }

  function discardAll() {
    setDrafts(null);
    setSaveState({ phase: 'idle' });
  }

  /**
   * 保存流（与配置页同一端点）：装配「只含改动冻结键」的 ConfigPatch 并 POST /api/config。
   * 200 + ok=true → 以服务端回读 applied 为准重建基线；fieldErrors 逐字段红字（草稿保留）；
   * 401/404 等按真实错误码如实展示，不硬编码假成功。
   */
  async function saveAll() {
    if (model === null || drafts === null || dirtyKeys.length === 0) return;
    const patch: ConfigPatch = dirtyKeys.map((key) => ({ key, value: drafts[key] }));
    setSaveState({ phase: 'saving' });
    try {
      const res = await savePanelConfig(patch);
      const applied = res.applied;
      if (res.ok && applied) {
        setState((prev) => (prev.phase === 'ready' ? { ...prev, config: applied } : prev));
        setDrafts(null);
        setSaveState({ phase: 'saved' });
      } else if (res.fieldErrors && res.fieldErrors.length > 0) {
        setSaveState({ phase: 'field-errors', errors: res.fieldErrors });
      } else {
        setSaveState({ phase: 'failed', message: SETTINGS.SAVE_MALFORMED_RESPONSE });
      }
    } catch (err) {
      const status = err instanceof ApiError ? err.status : undefined;
      setSaveState({
        phase: 'failed',
        message: err instanceof Error ? err.message : String(err),
        status,
      });
    }
  }

  if (state.phase === 'loading') {
    return (
      <div className="platform-tasks" data-testid="platform-tasks-card">
        <CardHead />
        <div className="platform-tasks__state" role="status">
          {TASKS.LOADING}
        </div>
      </div>
    );
  }

  if (state.phase === 'error') {
    return (
      <div className="platform-tasks" data-testid="platform-tasks-card">
        <CardHead onRefresh={() => void load()} />
        <div className="platform-tasks__state platform-tasks__state--error" role="alert">
          <p className="platform-tasks__state-title">{TASKS.ERROR_TITLE}</p>
          <p className="platform-tasks__state-msg">{state.message}</p>
          <p className="platform-tasks__state-hint">{TASKS.ERROR_HINT}</p>
          <button type="button" className="btn" onClick={() => void load()}>
            {TASKS.RETRY}
          </button>
        </div>
      </div>
    );
  }

  const saving = saveState.phase === 'saving';

  return (
    <div className="platform-tasks" data-testid="platform-tasks-card">
      <CardHead onRefresh={() => void load()} />
      <p className="platform-tasks__note">{TASKS.NOTE}</p>

      {scoped.length === 0 ? (
        <p className="platform-tasks__state">{platform === 'workbuddy' ? TASKS.EMPTY : TASKS.PLATFORM_NONE}</p>
      ) : (
        <ul className="platform-tasks__rows">
          {scoped.map((t) => {
            const key = taskConfigKey(t.name, platform);
            return (
              <li key={t.name} className="platform-tasks__row" data-task={t.name}>
                <div className="platform-tasks__row-head">
                  <h4 className="platform-tasks__row-title">{taskDisplayName(t.name)}</h4>
                  <span className={`platform-tasks__badge platform-tasks__badge--${t.enabled ? 'on' : 'off'}`}>
                    {t.enabled ? TASKS.ENABLED_ON : TASKS.ENABLED_OFF}
                  </span>
                </div>
                <p className="platform-tasks__row-desc">{taskDescription(t.name)}</p>
                {key === null ? (
                  <p className="platform-tasks__row-unknown">
                    <span>{TASKS.CRON_LABEL}</span>
                    <code>{t.cron}</code>
                    <span>{TASKS.UNKNOWN_TASK}</span>
                  </p>
                ) : (
                  <TimeSlotPicker
                    name={key}
                    value={drafts?.[key] ?? baseline[key] ?? t.cron}
                    onChange={(expr) => setDraft(key, expr)}
                  />
                )}
              </li>
            );
          })}
        </ul>
      )}

      {scoped.length > 0 && (
        <>
          <footer className="platform-tasks__bar">
            <p className="platform-tasks__bar-note" role="status">
              {`${settingsLine(TASKS.COUNT, { count: scoped.length })} · ${
                dirtyKeys.length > 0
                  ? settingsLine(TASKS.DIRTY, { count: dirtyKeys.length })
                  : saveState.phase === 'saved'
                    ? TASKS.SAVE_OK
                    : TASKS.SYNCED
              }`}
            </p>
            <button
              type="button"
              className="btn btn--primary"
              disabled={dirtyKeys.length === 0 || saving}
              onClick={() => void saveAll()}
            >
              {saving ? TASKS.SAVING : TASKS.SAVE}
            </button>
            <button type="button" className="btn" disabled={dirtyKeys.length === 0 || saving} onClick={discardAll}>
              {TASKS.DISCARD}
            </button>
          </footer>
          {saveState.phase === 'field-errors' && (
            <div className="platform-tasks__save-errors" role="alert">
              <p className="platform-tasks__state-title">{TASKS.SAVE_REJECTED_TITLE}</p>
              <ul>
                {saveState.errors.map((e) => (
                  <li key={`${e.key}:${e.message}`}>
                    <code>{e.key}</code> — {e.message}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {saveState.phase === 'failed' && (
            <p className="platform-tasks__save-failed" role="alert">
              {settingsLine(TASKS.SAVE_FAILED, {
                detail: saveState.status !== undefined ? `HTTP ${saveState.status}` : saveState.message,
              })}
            </p>
          )}
        </>
      )}
    </div>
  );
}

/** 卡头：标题 + 手动刷新（挂载/错误态也复用，错误态无刷新回调时省略按钮）。 */
function CardHead(props: { onRefresh?: () => void }) {
  return (
    <div className="platform-tasks__head">
      <h3 className="platform-tasks__title">{TASKS.TITLE}</h3>
      {props.onRefresh !== undefined && (
        <button type="button" className="btn" onClick={props.onRefresh}>
          {TASKS.REFRESH}
        </button>
      )}
    </div>
  );
}
