import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, waitFor, act } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { AccountsScreen } from '../src/app/AccountsScreen';
import { LOGIN_POLL_MS } from '../src/lib/loginApi';
import { ACCOUNTS, LOGIN } from '../src/i18n/zh';

/**
 * 浏览器登录 tab 门禁（M8）：fetch 全程 mock（wire 蛇形，形状按冻结契约）。
 * 断言面：workbuddy 双 tab（浏览器登录默认 + 粘贴凭证 JSON）、生成链接 wire（POST body={}）、
 * 轮询状态机（第一轮 pending 继续等 → 第二轮 done 展示昵称与 uid；完成不自动关面板，
 * 由「完成」按钮回调收尾）、expired / error 态与重新生成、qoder 面板不出现 tab 行、axe 零违规。
 * 注：仓库未引 jest-dom，文案断言走 textContent；hidden 容器用 role 查询（默认排除隐藏元素）。
 */
expect.extend(toHaveNoViolations);

const EMPTY_POOL = {
  accounts: [],
  total: 0,
  active: 0,
  cooling: 0,
  disabled: 0,
  credit_total: 0,
};

type Route = { status?: number; body?: unknown; reject?: boolean };

/** body 允许传函数（每轮轮询动态应答，状态机测试用）。 */
function resolveBody(route: Route): unknown {
  return typeof route.body === 'function' ? (route.body as () => unknown)() : route.body;
}

function installFetch(routes: Record<string, Route>) {
  const calls: string[] = [];
  const fn = vi.fn(async (...args: unknown[]) => {
    const url = typeof args[0] === 'string' ? args[0] : String(args[0]);
    const init = (args[1] ?? {}) as { method?: string; body?: string };
    calls.push(`${init.method ?? 'GET'} ${url}${init.body !== undefined ? ` body=${init.body}` : ''}`);
    const route = routes[url];
    if (!route) throw new Error(`unexpected fetch: ${url}`);
    if (route.reject) throw new TypeError('fetch failed');
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => resolveBody(route) };
  });
  vi.stubGlobal('fetch', fn);
  return { fn, calls };
}

/** 打开添加向导（空池主入口）：workbuddy 双 tab / qoder 单表单都以「添加账号」标题为就绪锚点。 */
async function openWizard(platform: 'workbuddy' | 'qoder' = 'workbuddy'): Promise<void> {
  render(<AccountsScreen platform={platform} />);
  fireEvent.click(await screen.findByRole('button', { name: ACCOUNTS.ADD_OPEN }));
  await screen.findByRole('heading', { name: ACCOUNTS.ADD_TITLE });
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('添加向导 — 浏览器登录 tab（M8）', () => {
  it('workbuddy 向导双 tab 并列，浏览器登录为默认 tab，粘贴 tab 含原 JSON 表单', async () => {
    installFetch({ '/api/accounts': { body: EMPTY_POOL } });
    await openWizard();

    const loginTab = screen.getByRole('tab', { name: LOGIN.TAB_BROWSER });
    const pasteTab = screen.getByRole('tab', { name: LOGIN.TAB_PASTE });
    expect(loginTab.getAttribute('aria-selected')).toBe('true');
    expect(pasteTab.getAttribute('aria-selected')).toBe('false');
    // 默认 tab：生成登录链接按钮可见；粘贴 tab 隐藏（role 查询默认排除隐藏元素）
    expect(screen.getByRole('button', { name: LOGIN.START })).toBeTruthy();
    expect(screen.queryByRole('textbox', { name: ACCOUNTS.ADD_WB_LABEL })).toBeNull();

    fireEvent.click(pasteTab);
    expect(screen.getByRole('textbox', { name: ACCOUNTS.ADD_WB_LABEL })).toBeTruthy();
    expect(screen.queryByRole('button', { name: LOGIN.START })).toBeNull();
  });

  it('生成链接走 POST login-start（body={}），waiting 态给链接/打开/复制/重新生成与等待提示', async () => {
    const { calls } = installFetch({
      '/api/accounts': { body: EMPTY_POOL },
      '/api/accounts/workbuddy/login-start': {
        body: { state: 'st-ui-1', login_url: 'https://auth.example/wb?state=st-ui-1' },
      },
    });
    await openWizard();
    fireEvent.click(screen.getByRole('button', { name: LOGIN.START }));

    await waitFor(() => {
      const statuses = screen.getAllByRole('status');
      expect(statuses.some((el) => (el.textContent ?? '').includes(LOGIN.WAITING))).toBe(true);
    });
    expect(calls.some((c) => c.startsWith('POST /api/accounts/workbuddy/login-start body={}'))).toBe(true);
    const link = await screen.findByRole('link', { name: 'https://auth.example/wb?state=st-ui-1' });
    expect(link.getAttribute('href')).toBe('https://auth.example/wb?state=st-ui-1');
    expect(screen.getByRole('button', { name: LOGIN.OPEN_PAGE })).toBeTruthy();
    expect(screen.getByRole('button', { name: LOGIN.COPY_LINK })).toBeTruthy();
    expect(screen.getByRole('button', { name: LOGIN.RESTART })).toBeTruthy();
  });

  it('轮询状态机：第一轮 pending 继续等，第二轮 done 展示昵称与 uid；完成不自动关面板', async () => {
    vi.useFakeTimers();
    let pollCount = 0;
    const { calls } = installFetch({
      '/api/accounts': { body: EMPTY_POOL },
      '/api/accounts/workbuddy/login-start': {
        body: { state: 'st-ui-2', login_url: 'https://auth.example/wb?state=st-ui-2' },
      },
      '/api/accounts/workbuddy/login-poll?state=st-ui-2': {
        body: () => {
          pollCount += 1;
          return pollCount === 1 ? { status: 'pending' } : { status: 'done', uid: 'uid-ui-2', nickname: '浏览器号' };
        },
      },
    });
    render(<AccountsScreen platform="workbuddy" />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    fireEvent.click(screen.getByRole('button', { name: ACCOUNTS.ADD_OPEN }));
    fireEvent.click(screen.getByRole('button', { name: LOGIN.START }));

    // login-start 落定 → waiting；紧接的第一轮轮询 pending（继续等，不出现完成态）。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(screen.getByRole('status').textContent).toContain(LOGIN.WAITING);
    expect(screen.queryByText('浏览器号')).toBeNull();
    expect(pollCount).toBe(1);

    // 3s 后第二轮：done → 昵称 + uid 上屏，完成态保持可见（不自动关面板）。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(LOGIN_POLL_MS);
    });
    const okText = screen.getByRole('status').textContent ?? '';
    expect(okText).toContain('浏览器号');
    expect(okText).toContain('uid-ui-2');
    // AccountsScreen 自持 onAdded 闭包（收起向导 + 重拉池子）。「完成」触发后：
    // 完成态消失、回到空池入口；轮询已停（无第三发）。
    fireEvent.click(screen.getByRole('button', { name: LOGIN.FINISH }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(screen.queryByRole('button', { name: LOGIN.FINISH })).toBeNull();
    expect(screen.getByRole('button', { name: ACCOUNTS.ADD_OPEN })).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(LOGIN_POLL_MS * 2);
    });
    expect(pollCount).toBe(2);
    expect(calls.filter((c) => c.startsWith('GET /api/accounts/workbuddy/login-poll')).length).toBe(2);
  });

  it('expired 态如实展示并可重新生成链接', async () => {
    installFetch({
      '/api/accounts': { body: EMPTY_POOL },
      '/api/accounts/workbuddy/login-start': {
        body: { state: 'st-ui-3', login_url: 'https://auth.example/wb?state=st-ui-3' },
      },
      '/api/accounts/workbuddy/login-poll?state=st-ui-3': { body: { status: 'expired' } },
    });
    await openWizard();
    fireEvent.click(screen.getByRole('button', { name: LOGIN.START }));
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain(LOGIN.EXPIRED));
    expect(screen.getByRole('button', { name: LOGIN.RESTART })).toBeTruthy();
  });

  it('error 态逐字展示后端 {error} 原文', async () => {
    installFetch({
      '/api/accounts': { body: EMPTY_POOL },
      '/api/accounts/workbuddy/login-start': {
        body: { state: 'st-ui-4', login_url: 'https://auth.example/wb?state=st-ui-4' },
      },
      '/api/accounts/workbuddy/login-poll?state=st-ui-4': {
        body: { status: 'error', error: '获取账号信息失败: upstream 502' },
      },
    });
    await openWizard();
    fireEvent.click(screen.getByRole('button', { name: LOGIN.START }));
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('获取账号信息失败: upstream 502'));
  });

  it('login-start 上游失败（502 + {error}）落到失败态', async () => {
    installFetch({
      '/api/accounts': { body: EMPTY_POOL },
      '/api/accounts/workbuddy/login-start': {
        status: 502,
        body: { error: '生成登录链接失败: auth state: upstream 500' },
      },
    });
    await openWizard();
    fireEvent.click(screen.getByRole('button', { name: LOGIN.START }));
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('生成登录链接失败'));
  });

  it('qoder 面板保持单表单（无 tab 行、无浏览器登录文案）', async () => {
    installFetch({ '/api/accounts': { body: EMPTY_POOL } });
    await openWizard('qoder');
    expect(screen.queryByRole('tab', { name: LOGIN.TAB_BROWSER })).toBeNull();
    expect(screen.getByRole('textbox', { name: ACCOUNTS.ADD_QO_LABEL })).toBeTruthy();
  });

  it('axe 零违规（waiting 态）', async () => {
    installFetch({
      '/api/accounts': { body: EMPTY_POOL },
      '/api/accounts/workbuddy/login-start': {
        body: { state: 'st-ui-5', login_url: 'https://auth.example/wb?state=st-ui-5' },
      },
      '/api/accounts/workbuddy/login-poll?state=st-ui-5': { body: { status: 'pending' } },
    });
    const { container } = render(<AccountsScreen platform="workbuddy" />);
    fireEvent.click(await screen.findByRole('button', { name: ACCOUNTS.ADD_OPEN }));
    fireEvent.click(screen.getByRole('button', { name: LOGIN.START }));
    await screen.findByRole('status');
    const results = await axe(container);
    expect(results).toHaveNoViolations();
  });
});
