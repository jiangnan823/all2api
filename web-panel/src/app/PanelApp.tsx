import { useCallback, useEffect, useRef, useState } from 'react';
import { PANEL, settingsLine } from '../i18n/zh';
import { probePillStatus, PILL_POLL_MS, type PillStatus } from '../lib/statusPill';
import { SettingsScreen } from './SettingsScreen';

type Theme = 'light' | 'dark';
type SectionId = 'settings' | 'connect';

/** 首屏跟随系统；用户一旦手动切换即以 data-theme 显式覆盖（tokens.css 两档都有落值）。 */
function preferredTheme(): Theme {
  if (typeof window.matchMedia !== 'function') return 'light';
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

const NAV: ReadonlyArray<{ id: SectionId; label: string; icon: string }> = [
  { id: 'settings', label: PANEL.NAV_SETTINGS, icon: '⚙' },
  { id: 'connect', label: PANEL.NAV_CONNECT, icon: '⤴' },
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

function EmptyState() {
  return (
    <div className="shell__empty">
      <h3 className="shell__empty-title">{PANEL.EMPTY_TITLE}</h3>
      <p className="shell__empty-body">{PANEL.EMPTY_BODY}</p>
    </div>
  );
}

/**
 * B1 面板外壳：左 rail（功能主干，不含来源）+ 顶栏 + 内容区，结构取自 UI UE
 * 《all2api-panel-hifi-v0.2》。B1 交付骨架与真实可用的主题切换；
 * M2 B2 起配置节挂载真实功能屏（SettingsScreen），接入节（B4/B5）仍为空状态待接线。
 * M2 第二批：topbar 状态 pill 接真实探测（/api/time），不再是不接线的占位文案。
 */
export function PanelApp() {
  const [theme, toggleTheme] = useTheme();
  const [active, setActive] = useState<SectionId>('settings');

  return (
    <div className="shell">
      <aside className="shell__rail" aria-label={PANEL.RAIL_ARIA}>
        <div className="shell__logo" aria-hidden="true">
          {PANEL.LOGO}
        </div>
        <nav className="shell__nav">
          {NAV.map((item) => (
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
              <span className="shell__visually-hidden">{item.label}</span>
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
            明/暗
          </button>
        </header>

        <main className="shell__content">
          <section className="shell__section" id="settings" aria-labelledby="shell-h-settings">
            <div className="shell__section-head">
              <h2 className="shell__section-title" id="shell-h-settings">
                {PANEL.SETTINGS_TITLE}
              </h2>
            </div>
            <p className="shell__section-sub">{PANEL.SETTINGS_SUB}</p>
            {/* M2 B2 起设置节为真实功能屏：读取 /api/config + /api/config/fields（只读），接入节仍为空状态 */}
            <SettingsScreen />
          </section>

          <section className="shell__section" id="connect" aria-labelledby="shell-h-connect">
            <div className="shell__section-head">
              <h2 className="shell__section-title" id="shell-h-connect">
                {PANEL.CONNECT_TITLE}
              </h2>
            </div>
            <p className="shell__section-sub">{PANEL.CONNECT_SUB}</p>
            <EmptyState />
          </section>
        </main>
      </div>
    </div>
  );
}
