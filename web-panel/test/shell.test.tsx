import { describe, it, expect, afterEach } from 'vitest';
import { readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { render, screen, fireEvent, within, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { PanelApp } from '../src/app/PanelApp';
import { PANEL } from '../src/i18n/zh';

/**
 * B1 外壳门禁：axe 基线 + 导航可达性 + 主题切换的内存态承诺 + 接线批的安全红线自查。
 * 安全扫描面 = 本批新增的入口层（src/app、src/main.tsx、index.html）；
 * freeze.test.ts 的 IMPL_DIRS 不含这两处，故红线在本文件自证，不动冻结测试。
 */
expect.extend(toHaveNoViolations);

afterEach(() => {
  cleanup();
  delete document.documentElement.dataset.theme;
});

describe('外壳结构', () => {
  it('渲染标题、两节与未接线状态（无伪造数据）', () => {
    render(<PanelApp />);
    expect(screen.getByRole('heading', { level: 1, name: PANEL.TITLE })).toBeTruthy();
    expect(screen.getByRole('heading', { level: 2, name: PANEL.SETTINGS_TITLE })).toBeTruthy();
    expect(screen.getByRole('heading', { level: 2, name: PANEL.CONNECT_TITLE })).toBeTruthy();
    expect(screen.getAllByText(PANEL.EMPTY_TITLE)).toHaveLength(2);
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
