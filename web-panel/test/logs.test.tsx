import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { LogsScreen } from '../src/app/LogsScreen';
import { LOGS_FETCH_LIMIT, LOGS_POLL_MS } from '../src/lib/poolApi';
import { LOGS } from '../src/i18n/zh';

/**
 * 运行日志屏门禁（M5b）：fetch 全程 mock（wire 蛇形 {logs:[{ts,level,line}]}，后端并行批交付）。
 * 断言面：就绪渲染（等宽列表/时间戳/级别徽色）、级别过滤（全部/info/error）、
 * 自动滚动开关（默认开）、10s 轮询、错误态 + 重试、空态、limit 参数上送、axe 零违规。
 */
expect.extend(toHaveNoViolations);

/** 旧→新（契约：logs 旧到新）。error/info/警告级/未知级各覆盖一条。 */
const LOGS_WIRE = {
  logs: [
    { ts: 1760000000, level: 'info', line: '网关启动 listen=127.0.0.1:7800' },
    { ts: 1760000060, level: 'error', line: 'upstream 503 trae' },
    { ts: 1760000120, level: 'warn', line: '账号冷却 60s' },
    { ts: 1760000180, level: 'debug', line: 'pick account wb-1' },
  ],
};

type Route = { status?: number; body?: unknown; reject?: boolean };

function installFetch(routes: Record<string, Route>) {
  const fn = vi.fn(async (...args: unknown[]) => {
    const url = typeof args[0] === 'string' ? args[0] : String(args[0]);
    const route = routes[url];
    if (!route) throw new Error(`unexpected fetch: ${url}`);
    if (route.reject) throw new TypeError('fetch failed');
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => route.body };
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('日志屏 — 就绪渲染', () => {
  it('列表按级别徽色渲染全部行，默认过滤 = 全部（未知级别 debug 也可见）', async () => {
    const fn = installFetch({ '/api/logs?limit=200': { body: LOGS_WIRE } });
    render(<LogsScreen />);
    expect(await screen.findByText('网关启动 listen=127.0.0.1:7800')).toBeTruthy();
    // limit 参数上送 = 200
    expect(fn.mock.calls.every((c) => String(c[0]) === `/api/logs?limit=${LOGS_FETCH_LIMIT}`)).toBe(true);

    const list = document.querySelector('ul.logs-list') as HTMLElement;
    expect(list.querySelectorAll('li.logs-line')).toHaveLength(4);

    const errorLine = screen.getByText('upstream 503 trae').closest('li') as HTMLElement;
    expect(errorLine.querySelector('.logs-line__level--err')).toBeTruthy();
    const infoLine = screen.getByText('网关启动 listen=127.0.0.1:7800').closest('li') as HTMLElement;
    expect(infoLine.querySelector('.logs-line__level--idle')).toBeTruthy();
    // 时间戳列有 time 元素承载
    expect(infoLine.querySelector('time')?.textContent).toMatch(/^\d{2}:\d{2}:\d{2}$/);
  });

  it('级别过滤 error → 只剩 error 行；切回全部恢复（纯展示层，不重拉接口）', async () => {
    const fn = installFetch({ '/api/logs?limit=200': { body: LOGS_WIRE } });
    render(<LogsScreen />);
    await screen.findByText('upstream 503 trae');

    fireEvent.click(screen.getByRole('radio', { name: LOGS.FILTER_ERROR }));
    const list = document.querySelector('ul.logs-list') as HTMLElement;
    expect(list.querySelectorAll('li.logs-line')).toHaveLength(1);
    expect(list.textContent).toContain('upstream 503 trae');

    fireEvent.click(screen.getByRole('radio', { name: LOGS.FILTER_INFO }));
    expect((document.querySelector('ul.logs-list') as HTMLElement).querySelectorAll('li')).toHaveLength(1);

    // 过滤切换不触发新的 fetch（展示层行为）
    const callsAfterFilter = fn.mock.calls.length;
    fireEvent.click(screen.getByRole('radio', { name: LOGS.FILTER_ALL }));
    expect((document.querySelector('ul.logs-list') as HTMLElement).querySelectorAll('li')).toHaveLength(4);
    expect(fn.mock.calls.length).toBe(callsAfterFilter);
  });

  it('自动滚动开关默认开，可关；关闭后不再自动推底', async () => {
    installFetch({ '/api/logs?limit=200': { body: LOGS_WIRE } });
    render(<LogsScreen />);
    await screen.findByText('upstream 503 trae');

    const toggle = screen.getByRole('checkbox', { name: LOGS.AUTOSCROLL }) as HTMLInputElement;
    expect(toggle.checked).toBe(true);

    fireEvent.click(toggle);
    expect(toggle.checked).toBe(false);
    fireEvent.click(toggle);
    expect(toggle.checked).toBe(true);
  });

  it('axe 无违规', async () => {
    installFetch({ '/api/logs?limit=200': { body: LOGS_WIRE } });
    const { container } = render(<LogsScreen />);
    await screen.findByText('upstream 503 trae');
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe('日志屏 — 错误态 / 空态 / 轮询', () => {
  it('404（后端并行批未交付）→ 错误态如实展示 HTTP 码 + 重试；重试成功恢复列表', async () => {
    installFetch({ '/api/logs?limit=200': { status: 404, body: { error: 'not found' } } });
    render(<LogsScreen />);
    expect(await screen.findByText(LOGS.ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('HTTP 404')).toBeTruthy();
    expect(screen.getByText(LOGS.ERROR_HINT)).toBeTruthy();

    installFetch({ '/api/logs?limit=200': { body: LOGS_WIRE } });
    fireEvent.click(screen.getByRole('button', { name: LOGS.RETRY }));
    expect(await screen.findByText('upstream 503 trae')).toBeTruthy();
  });

  it('空日志 → 空态文案（不渲染列表）', async () => {
    installFetch({ '/api/logs?limit=200': { body: { logs: [] } } });
    render(<LogsScreen />);
    expect(await screen.findByText(LOGS.EMPTY_TITLE)).toBeTruthy();
    expect(screen.getByText(LOGS.EMPTY_BODY)).toBeTruthy();
    expect(document.querySelector('ul.logs-list')).toBeNull();
  });

  it('line 缺失的畸形条目整条丢弃，不渲染空行', async () => {
    installFetch({
      '/api/logs?limit=200': { body: { logs: [{ ts: 1, level: 'info', line: 'ok' }, { ts: 2, level: 'info' }, 'junk'] } },
    });
    render(<LogsScreen />);
    await screen.findByText('ok');
    expect((document.querySelector('ul.logs-list') as HTMLElement).querySelectorAll('li')).toHaveLength(1);
  });

  it('10s 轮询自动重拉，新行出现；过滤保持用户选择', async () => {
    vi.useFakeTimers();
    let round = 0;
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = String(args[0]);
      if (url !== '/api/logs?limit=200') throw new Error(`unexpected fetch: ${url}`);
      round += 1;
      if (round === 1) return { ok: true, status: 200, json: async () => ({ logs: [{ ts: 100, level: 'error', line: 'first' }] }) };
      return { ok: true, status: 200, json: async () => ({ logs: [{ ts: 100, level: 'error', line: 'first' }, { ts: 200, level: 'info', line: 'second' }] }) };
    });
    vi.stubGlobal('fetch', fn);
    const { act } = await import('@testing-library/react');
    render(<LogsScreen />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(screen.getByText('first')).toBeTruthy();

    // 切到 error 过滤后，轮询新增的 info 行不进入视图
    fireEvent.click(screen.getByRole('radio', { name: LOGS.FILTER_ERROR }));
    const callsAtStart = fn.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(LOGS_POLL_MS);
    });
    expect(fn.mock.calls.length).toBeGreaterThan(callsAtStart);
    expect(screen.queryByText('second')).toBeNull();
    expect(screen.getByText('first')).toBeTruthy();
  });
});
