import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { render, screen, fireEvent, within, cleanup, act } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { PanelApp } from '../src/app/PanelApp';
import { PANEL } from '../src/i18n/zh';

/**
 * B1 外壳门禁：axe 基线 + 导航可达性 + 主题切换的内存态承诺 + 接线批的安全红线自查。
 * 安全扫描面 = 本批新增的入口层（src/app、src/main.tsx、index.html）；
 * freeze.test.ts 的 IMPL_DIRS 不含这两处，故红线在本文件自证，不动冻结测试。
 * M2 B2 起设置节挂真实功能屏（SettingsScreen 会发 /api/config 等请求），
 * 本文件用永不 settle 的 fetch 桩把它钉在加载态——外壳断言不依赖设置屏内部实现（那是 settings.test 的面）。
 * M2 第二批：topbar 状态 pill 接真实探测（/api/time）；默认桩永不 settle = pill 停留「未接线」，
 * pill 各态由本文件的专用 describe 用可控 fetch 桩驱动（settings.test 只管设置屏自身）。
 */
expect.extend(toHaveNoViolations);

/** 永不 settle 的 fetch：设置屏钉加载态、pill 钉「未接线」首观感。 */
const HANGING_FETCH = vi.fn(() => new Promise(() => {}));

beforeEach(() => {
  vi.stubGlobal('fetch', HANGING_FETCH);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  delete document.documentElement.dataset.theme;
});

describe('外壳结构', () => {
  it('渲染标题、两节（设置节已接线，接入节空状态）与 pill（首次探测未落定 = 未接线）', () => {
    render(<PanelApp />);
    expect(screen.getByRole('heading', { level: 1, name: PANEL.TITLE })).toBeTruthy();
    expect(screen.getByRole('heading', { level: 2, name: PANEL.SETTINGS_TITLE })).toBeTruthy();
    expect(screen.getByRole('heading', { level: 2, name: PANEL.CONNECT_TITLE })).toBeTruthy();
    expect(screen.getAllByText(PANEL.EMPTY_TITLE)).toHaveLength(1);
    expect(screen.getByText(PANEL.STATUS_UNWIRED)).toBeTruthy();
  });

  it('首屏未手动切换时 data-theme 落系统偏好（jsdom 无暗色偏好 → light）', () => {
    render(<PanelApp />);
    expect(document.documentElement.dataset.theme).toBe('light');
  });

  it('axe 无违规', async () => {
    const { container } = render(<PanelApp />);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe('导航与主题（键盘可达）', () => {
  it('图标导航带可访问名，默认当前项为配置', () => {
    render(<PanelApp />);
    const rail = screen.getByRole('complementary', { name: PANEL.RAIL_ARIA });
    const links = within(rail).getAllByRole('link');
    expect(links).toHaveLength(2);
    expect(links[0].getAttribute('aria-current')).toBe('page');
    expect(links[1].getAttribute('aria-current')).toBeNull();
  });

  it('点击接入导航后当前项转移，锚点指向真实存在的节', () => {
    render(<PanelApp />);
    const connect = screen.getByRole('link', { name: PANEL.NAV_CONNECT });
    fireEvent.click(connect);
    expect(connect.getAttribute('aria-current')).toBe('page');
    expect(screen.getByRole('link', { name: PANEL.NAV_SETTINGS }).getAttribute('aria-current')).toBeNull();
    expect(connect.getAttribute('href')).toBe('#connect');
    expect(document.getElementById('connect')).toBeTruthy();
    expect(document.getElementById('settings')).toBeTruthy();
  });

  it('主题按钮切换 data-theme，且只落内存态', () => {
    render(<PanelApp />);
    const btn = screen.getByRole('button', { name: PANEL.THEME });
    fireEvent.click(btn);
    expect(document.documentElement.dataset.theme).toBe('dark');
    expect(btn.getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(btn);
    expect(document.documentElement.dataset.theme).toBe('light');
    expect(btn.getAttribute('aria-pressed')).toBe('false');
  });
});

/**
 * topbar 状态 pill 的接线门禁（M2 第二批）。判定面与 lib/statusPill.ts 一致：
 * GET /api/time 成功 = 已连接（附服务器时间偏差）；401 = 服务在但 key 不符；其余 = 离线。
 * 轮询用 fake timers 驱动；fetch 桩按 URL 分流（设置屏的其他请求保持挂起，不影响 pill 断言）。
 */
describe('状态 pill（/api/time 探测接线）', () => {
  const TIME_WIRE = { now: 1760420400, local: '2026-10-14 12:00:00', timezone: 'Asia/Shanghai' };

  type Route = { status?: number; body?: unknown; reject?: boolean };

  /** pill 用 role=status；设置屏加载态也是 role=status，故按外壳类名取值避免歧义。 */
  const pill = () => document.querySelector('p.shell__status') as HTMLElement;

  function installPillFetch(routes: Record<string, Route>) {
    // 占位 ...args：与 fetch 签名一致（url, init），保住 mock.calls 元组形状
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = typeof args[0] === 'string' ? args[0] : String(args[0]);
      const route = routes[url];
      if (!route) return new Promise(() => {}); // 其余端点永不 settle（钉住设置屏）
      if (route.reject) throw new TypeError('fetch failed');
      const status = route.status ?? 200;
      return { ok: status < 400, status, json: async () => route.body };
    });
    vi.stubGlobal('fetch', fn);
    return fn;
  }

  it('/api/time 成功 → 已连接 + 服务器时区与偏差角注', async () => {
    installPillFetch({ '/api/time': { body: TIME_WIRE } });
    render(<PanelApp />);
    expect(await screen.findByText(PANEL.STATUS_ONLINE)).toBeTruthy();
    const p = pill();
    expect(p.className).toContain('shell__status--ok');
    expect(p.textContent).toContain('Asia/Shanghai');
    expect(p.textContent).toMatch(/相差约 \d+ 秒/);
  });

  it('/api/time 401 → 未授权（服务在但 key 不符，不算离线）', async () => {
    installPillFetch({ '/api/time': { status: 401, body: { error: 'unauthorized' } } });
    render(<PanelApp />);
    expect(await screen.findByText(PANEL.STATUS_UNAUTHORIZED)).toBeTruthy();
    expect(pill().className).toContain('shell__status--warn');
  });

  it('/api/time 网络失败 → 离线带诊断信息；手动「立即重新探测」可恢复在线', async () => {
    const fn = installPillFetch({ '/api/time': { reject: true } });
    render(<PanelApp />);
    expect(await screen.findByText(PANEL.STATUS_OFFLINE)).toBeTruthy();
    expect(pill().className).toContain('shell__status--err');
    expect(pill().textContent).toContain('network error');
    expect(fn.mock.calls.filter((c) => String(c[0]) === '/api/time')).toHaveLength(1);

    installPillFetch({ '/api/time': { body: TIME_WIRE } });
    fireEvent.click(screen.getByRole('button', { name: PANEL.STATUS_REFRESH }));
    expect(await screen.findByText(PANEL.STATUS_ONLINE)).toBeTruthy();
  });

  it('/api/time 5xx → 离线，诊断信息为 HTTP 码（不硬编码假成功）', async () => {
    installPillFetch({ '/api/time': { status: 503, body: { error: 'unavailable' } } });
    render(<PanelApp />);
    expect(await screen.findByText(PANEL.STATUS_OFFLINE)).toBeTruthy();
    expect(pill().textContent).toContain('HTTP 503');
  });

  it('周期轮询：PILL_POLL_MS 一轮后自动重探，离线可在下一轮内翻转为在线', async () => {
    vi.useFakeTimers();
    try {
      let fail = true;
      const fn = vi.fn(async (...args: unknown[]) => {
        if (String(args[0]) !== '/api/time') return new Promise(() => {});
        if (fail) throw new TypeError('fetch failed');
        return { ok: true, status: 200, json: async () => TIME_WIRE };
      });
      vi.stubGlobal('fetch', fn);
      render(<PanelApp />);
      // 首轮探测：React 的状态提交需要 act 包裹（fake timers 下 findBy* 不可用）
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1);
      });
      expect(pill().textContent).toContain(PANEL.STATUS_OFFLINE);
      const callsAtStart = fn.mock.calls.length;

      fail = false;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
      expect(pill().textContent).toContain(PANEL.STATUS_ONLINE);
      expect(fn.mock.calls.length).toBeGreaterThan(callsAtStart);
    } finally {
      vi.useRealTimers();
    }
  });

  it('未授权态在下一轮轮询内翻转为在线（断线与恢复都可感知）', async () => {
    vi.useFakeTimers();
    try {
      let status = 401;
      vi.stubGlobal(
        'fetch',
        vi.fn(async (...args: unknown[]) => {
          if (String(args[0]) !== '/api/time') return new Promise(() => {});
          if (status !== 200) return { ok: false, status, json: async () => ({ error: 'unauthorized' }) };
          return { ok: true, status: 200, json: async () => TIME_WIRE };
        }),
      );
      render(<PanelApp />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1);
      });
      expect(pill().textContent).toContain(PANEL.STATUS_UNAUTHORIZED);

      status = 200;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
      expect(pill().textContent).toContain(PANEL.STATUS_ONLINE);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('入口层安全红线自查（接线批继承 M1 口径）', () => {
  const TARGETS = ['src/app/PanelApp.tsx', 'src/main.tsx', 'index.html'] as const;
  const files = TARGETS.map((rel) => ({ rel, path: join(process.cwd(), rel) }));

  it('待扫文件齐备', () => {
    expect(files.filter((f) => existsSync(f.path)).length).toBe(TARGETS.length);
  });

  const sources = files.map((f) => ({ rel: f.rel, text: readFileSync(f.path, 'utf8') }));

  it.each([
    'innerHTML',
    'document.write',
    'eval(',
    'new Function(',
    'localStorage',
    'sessionStorage',
    'indexedDB',
    'postMessage(',
  ])('入口层零命中：%s', (forbidden) => {
    const hits = sources.filter((s) => s.text.includes(forbidden)).map((s) => s.rel);
    expect(hits, `${forbidden} 出现在 ${hits.join(', ')}`).toEqual([]);
  });

  it('入口层不出现硬编码 host（G5：只用注入值或 location.origin）', () => {
    const hits = sources.filter(({ text }) => /https?:\/\/[^\s"'`]+/.test(text)).map((s) => s.rel);
    expect(hits, `出现绝对 URL：${hits.join(', ')}`).toEqual([]);
  });
});
