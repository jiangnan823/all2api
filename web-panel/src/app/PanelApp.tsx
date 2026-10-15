import { useCallback, useEffect, useRef, useState } from 'react';
import { NAV, PANEL, settingsLine } from '../i18n/zh';
import { probePillStatus, PILL_POLL_MS, type PillStatus } from '../lib/statusPill';
import { AccountsScreen } from './AccountsScreen';
import { LogsScreen } from './LogsScreen';
import { SettingsScreen } from './SettingsScreen';

type Theme = 'light' | 'dark';
type SectionId = 'accounts' | 'tasks' | 'models' | 'settings' | 'logs';

/** 首屏跟随系统；用户一旦手动切换即以 data-theme 显式覆盖（tokens.css 两档都有落值）。 */
function preferredTheme(): Theme {
  if (typeof window.matchMedia !== 'function') return 'light';
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

/**
 * 左栏五项中文页签（M5b，信息架构对齐参考控制台）：账号池为首屏；
 * 「用量 / 积分构成」暂不做，不留死链接。任务中心 / 模型两屏由并行批交付文件
 * （src/app/TasksScreen.tsx、src/app/ModelsScreen.tsx），此处渲染占位组件，集成方替换。
 */
const NAV_ITEMS: ReadonlyArray<{ id: SectionId; label: string; icon: string }> = [
  { id: 'accounts', label: NAV.ACCOUNTS, icon: '▤' },
  { id: 'tasks', label: NAV.TASKS, icon: '☑' },
  { id: 'models', label: NAV.MODELS, icon: '◫' },
  { id: 'settings', label: NAV.CONFIG, icon: '⚙' },
  { id: 'logs', label: NAV.LOGS, icon: '⌁' },
];

/** 主题只在内存态切换：面板不落任何 storage（M1 安全红线，接线批继续生效）。 */
function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(preferredTheme);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  return [theme, () => setTheme((t) => (t === 'dark' ? 'light' : 'dark'))];
}

/**
 * topbar 状态 pill（M2 第二批接线）：GET /api/time 判服务连通（成功 = 在线并给出服务器时间
 * 偏差；401 = key 不符；其余 = 离线）。挂载即探测一次，之后按 PILL_POLL_MS 周期轮询，
 * 断线在下一轮内可见；「立即探测」按钮供手动刷新。全部内存态，不落 storage。
 */
function StatusPill() {
  const [status, setStatus] = useState<PillStatus>({ phase: 'initial' });
  const [checking, setChecking] = useState(false);
  const seqRef = useRef(0);

  const probe = useCallback(async () => {
    const seq = ++seqRef.current;
    setChecking(true);
    const next = await probePillStatus();
    if (seq === seqRef.current) setStatus(next);
    setChecking(false);
  }, []);

  useEffect(() => {
    void probe();
    const timer = window.setInterval(() => void probe(), PILL_POLL_MS);
    return () => window.clearInterval(timer);
  }, [probe]);

  const copy = pillCopy(status);
  const tone = pillTone(status);
  return (
    <p className={`shell__status shell__status--${tone}`} role="status">
      <span className="shell__status-dot" aria-hidden="true" />
      <span>{PANEL.STATUS_LABEL}</span>
      <strong>{copy.value}</strong>
      {copy.detail !== null && <span className="shell__status-detail">{copy.detail}</span>}
      <button
        type="button"
        className="shell__status-refresh"
        onClick={() => void probe()}
        disabled={checking}
        title={PANEL.STATUS_REFRESH}
        aria-label={PANEL.STATUS_REFRESH}
      >
        ↻
      </button>
    </p>
  );
}

function pillTone(status: PillStatus): 'idle' | 'ok' | 'warn' | 'err' {
  switch (status.phase) {
    case 'online': return 'ok';
    case 'unauthorized': return 'warn';
    case 'offline': return 'err';
    default: return 'idle';
  }
}

function pillCopy(status: PillStatus): { value: string; detail: string | null } {
  switch (status.phase) {
    case 'online':
      return {
        value: PANEL.STATUS_ONLINE,
        detail: settingsLine(PANEL.STATUS_SKEW, { sec: status.skewSec, tz: status.time.timezone }),
      };
    case 'unauthorized':
      return { value: PANEL.STATUS_UNAUTHORIZED, detail: null };
    case 'offline':
      return { value: PANEL.STATUS_OFFLINE, detail: status.detail };
    case 'initial':
      return { value: PANEL.STATUS_UNWIRED, detail: null };
    case 'checking':
      // 探测中不额外占位：展示沿用上一轮 status（状态机另有 phase，但 UI 只在首发时见 initial）
      return { value: PANEL.STATUS_CHECKING, detail: null };
  }
}

/**
 * 并行批占位屏（M5b）：任务中心 / 模型两屏由并行 agent 交付（TasksScreen / ModelsScreen），
 * 合入前这里如实标注「并行开发中」，集成方在 ScreenFor 分支处替换为真实组件。
 */
function PlaceholderScreen(props: { name: string }) {
  return (
    <div className="pool-empty">
      <h3 className="pool-empty__title">{PANEL.PLACEHOLDER_TITLE}</h3>
      <p className="pool-empty__body">{settingsLine(PANEL.PLACEHOLDER_BODY, { name: props.name })}</p>
    </div>
  );
}

/**
 * B1 面板外壳 → M5b 信息架构重建：左 rail 五项可见中文页签（icon + 文字同显）+
 * 顶栏 + 单活动视图内容区。账号池首屏；配置页签原样挂载 SettingsScreen；
 * 日志页签挂 LogsScreen；任务中心 / 模型为并行批占位。connect 空状态节随重建退役。
 * 主题切换仍只在内存态（M1 红线）。
 */
export function PanelApp() {
  const [theme, toggleTheme] = useTheme();
  const [active, setActive] = useState<SectionId>('accounts');

  return (
    <div className="shell">
      <aside className="shell__rail" aria-label={PANEL.RAIL_ARIA}>
        <div className="shell__logo" aria-hidden="true">
          {PANEL.LOGO}
        </div>
        <nav className="shell__nav">
          {NAV_ITEMS.map((item) => (
            <a
              key={item.id}
              className="shell__nav-link"
              href={`#${item.id}`}
              title={item.label}
              aria-current={active === item.id ? 'page' : undefined}
              onClick={() => setActive(item.id)}
            >
              <span className="shell__nav-ico" aria-hidden="true">
                {item.icon}
              </span>
              <span className="shell__nav-text">{item.label}</span>
            </a>
          ))}
        </nav>
      </aside>

      <div className="shell__body">
        <header className="shell__topbar">
          <h1 className="shell__title">{PANEL.TITLE}</h1>
          <span className="shell__spacer" />
          <StatusPill />
          <button
            type="button"
            className="btn"
            onClick={toggleTheme}
            aria-label={PANEL.THEME}
            aria-pressed={theme === 'dark'}
          >
            {PANEL.THEME_SHORT}
          </button>
        </header>

        <main className="shell__content">
          {active === 'accounts' && (
            <section className="shell__section" id="accounts" aria-labelledby="shell-h-accounts">
              <div className="shell__section-head">
                <h2 className="shell__section-title" id="shell-h-accounts">
                  {NAV.ACCOUNTS}
                </h2>
              </div>
              <AccountsScreen />
            </section>
          )}

          {active === 'tasks' && (
            <section className="shell__section" id="tasks" aria-labelledby="shell-h-tasks">
              <div className="shell__section-head">
                <h2 className="shell__section-title" id="shell-h-tasks">
                  {NAV.TASKS}
                </h2>
              </div>
              {/* 并行批占位：集成方替换为 <TasksScreen />（src/app/TasksScreen.tsx，归并行批所有） */}
              <PlaceholderScreen name={NAV.TASKS} />
            </section>
          )}

          {active === 'models' && (
            <section className="shell__section" id="models" aria-labelledby="shell-h-models">
              <div className="shell__section-head">
                <h2 className="shell__section-title" id="shell-h-models">
                  {NAV.MODELS}
                </h2>
              </div>
              {/* 并行批占位：集成方替换为 <ModelsScreen />（src/app/ModelsScreen.tsx，归并行批所有） */}
              <PlaceholderScreen name={NAV.MODELS} />
            </section>
          )}

          {active === 'settings' && (
            <section className="shell__section" id="settings" aria-labelledby="shell-h-settings">
              <div className="shell__section-head">
                <h2 className="shell__section-title" id="shell-h-settings">
                  {NAV.CONFIG}
                </h2>
              </div>
              <p className="shell__section-sub">{PANEL.SETTINGS_SUB}</p>
              <SettingsScreen />
            </section>
          )}

          {active === 'logs' && (
            <section className="shell__section" id="logs" aria-labelledby="shell-h-logs">
              <div className="shell__section-head">
                <h2 className="shell__section-title" id="shell-h-logs">
                  {NAV.LOGS}
                </h2>
              </div>
              <LogsScreen />
            </section>
          )}
        </main>
      </div>
    </div>
  );
}
