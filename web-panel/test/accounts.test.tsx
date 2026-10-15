import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, fireEvent, cleanup, act, within } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { AccountsScreen } from '../src/app/AccountsScreen';
import { ACCOUNTS_POLL_MS } from '../src/lib/poolApi';
import { ACCOUNTS } from '../src/i18n/zh';

/**
 * 账号池屏门禁（M5b）：fetch 全程 mock（wire 蛇形，形状按任务书契约；
 * 后端并行批交付前运行期 404 如实展示错误态）。
 * 断言面：就绪渲染（状态卡行 + 表格全列 + 状态徽色）、错误态 + 重试、空池引导、
 * 30s 轮询自愈、账号 ID 点击复制（clipboard mock）、axe 零违规。
 */
expect.extend(toHaveNoViolations);

/** 服务端 wire 形状（蛇形）。credit_total / 冷却 / 到期 / health 均按可空字段覆盖。 */
const ACCOUNTS_WIRE = {
  accounts: [
    {
      platform: 'workbuddy',
      id: 'wb-0123456789abcdef',
      nickname: '40 多岁的年轻小伙',
      status: 'active',
      credit: 1234.5,
      health: 'good',
      expires_at: { unix: 1790000000, local: '2026-10-15 22:00:00' },
      cooldown_end: null,
      last_selected: { unix: 1760000000, local: '2026-10-15 12:00:00' },
    },
    {
      platform: 'trae',
      id: 'tr-ffff00001111',
      status: 'cooling',
      credit: 88,
      health: 'rate_limited',
      expires_at: 1790000000,
      cooldown_end: 1760001000,
      last_selected: 1760000000,
    },
    {
      platform: 'qoder',
      id: 'qd-deadbeef42',
      status: 'disabled',
      credit: 0,
      health: '',
      expires_at: 0,
      cooldown_end: 0,
      last_selected: 0,
    },
  ],
  total: 3,
  active: 1,
  cooling: 1,
  disabled: 1,
  credit_total: 1322.5,
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

beforeEach(() => {
  // jsdom 无 clipboard：桩上 writeText 供复制断言
  Object.defineProperty(navigator, 'clipboard', {
    value: { writeText: vi.fn().mockResolvedValue(undefined) },
    configurable: true,
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('账号池屏 — 就绪渲染', () => {
  it('状态卡行五行齐备且数值取自服务端汇总字段（等宽数字容器）', async () => {
    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    render(<AccountsScreen />);
    expect(await screen.findByText(ACCOUNTS.CARD_TOTAL)).toBeTruthy();
    const cards = document.querySelector('ul.pool-cards') as HTMLElement;
    expect(cards).toBeTruthy();
    // 数值文案走 toLocaleString（等宽数字容器内），credit_total 含千分位
    expect(cards.textContent).toContain('1,322.5'); // credit_total
    expect(cards.textContent).toContain(ACCOUNTS.CARD_TOTAL);
    const totalCard = cards.querySelectorAll('li.pool-card')[0] as HTMLElement;
    expect(totalCard.textContent).toBe(`${ACCOUNTS.CARD_TOTAL}3`);
    // 五张卡 = 总数/可用/冷却/禁用/积分
    expect(cards.querySelectorAll('li.pool-card')).toHaveLength(5);
    // 状态徽标同文案（可用/冷却中/已禁用）也在文档里，getByText 会撞多个 → 限定在卡片容器内
    for (const label of [
      ACCOUNTS.CARD_TOTAL,
      ACCOUNTS.CARD_ACTIVE,
      ACCOUNTS.CARD_COOLING,
      ACCOUNTS.CARD_DISABLED,
      ACCOUNTS.CARD_CREDIT,
    ]) {
      expect(within(cards).getAllByText(label).length).toBeGreaterThanOrEqual(1);
    }
  });

  it('表格七列齐备：平台/账号 id 全量/状态徽色/积分/健康度/到期/冷却至', async () => {
    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    render(<AccountsScreen />);
    await screen.findByText('wb-0123456789abcdef');
    const table = document.querySelector('table.pool-table') as HTMLTableElement;
    const headers = Array.from(table.querySelectorAll('thead th')).map((th) => th.textContent);
    expect(headers).toEqual([
      ACCOUNTS.COL_PLATFORM,
      ACCOUNTS.COL_ACCOUNT,
      ACCOUNTS.COL_STATUS,
      ACCOUNTS.COL_CREDIT,
      ACCOUNTS.COL_HEALTH,
      ACCOUNTS.COL_EXPIRES,
      ACCOUNTS.COL_COOLDOWN,
    ]);
    const rows = Array.from(table.querySelectorAll('tbody tr'));
    expect(rows).toHaveLength(3);
    // 账号 id 全量显示（不截断）
    expect(table.textContent).toContain('wb-0123456789abcdef');
    expect(table.textContent).toContain('tr-ffff00001111');
    expect(table.textContent).toContain('qd-deadbeef42');
    // v1.2：昵称主显（有昵称的账号），无昵称账号只有 id
    expect(table.textContent).toContain('40 多岁的年轻小伙');
    // v1.2：状态徽标中文文案（未知值原样显示）
    const badges = Array.from(table.querySelectorAll('.pool-badge'));
    expect(badges.map((b) => b.className)).toEqual([
      'pool-badge pool-badge--ok',
      'pool-badge pool-badge--warn',
      'pool-badge pool-badge--err',
    ]);
    expect(badges.map((b) => b.textContent)).toEqual(['可用', '冷却中', '已禁用']);
    // v1.2：到期时间解析 TimeLocal 对象形态（此前恒显示 —）
    // credit 缺失（null）显示 —，不编造；health 空串同样 —
    expect(table.textContent).toContain(ACCOUNTS.DASH);
  });

  it('点击账号 ID 复制全量 id，出现「已复制」提示', async () => {
    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    render(<AccountsScreen />);
    await screen.findByText('wb-0123456789abcdef');
    const copyBtn = screen.getByRole('button', { name: `${ACCOUNTS.COPY}: wb-0123456789abcdef` });
    fireEvent.click(copyBtn);
    expect(await screen.findByText(ACCOUNTS.COPIED)).toBeTruthy();
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith('wb-0123456789abcdef');
  });

  it('axe 无违规', async () => {
    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    const { container } = render(<AccountsScreen />);
    await screen.findByText('wb-0123456789abcdef');
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe('账号池屏 — 错误态 / 空态 / 轮询', () => {
  it('404（后端并行批未交付）→ 错误态如实展示 HTTP 码 + 重试按钮', async () => {
    installFetch({ '/api/accounts': { status: 404, body: { error: 'not found' } } });
    render(<AccountsScreen />);
    expect(await screen.findByText(ACCOUNTS.ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('HTTP 404')).toBeTruthy();
    expect(screen.getByText(ACCOUNTS.ERROR_HINT)).toBeTruthy();
    expect(screen.getByRole('button', { name: ACCOUNTS.RETRY })).toBeTruthy();
  });

  it('重试成功 → 错误态翻转为就绪渲染', async () => {
    installFetch({ '/api/accounts': { status: 500, body: { error: 'boom' } } });
    render(<AccountsScreen />);
    expect(await screen.findByText(ACCOUNTS.ERROR_TITLE)).toBeTruthy();

    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    fireEvent.click(screen.getByRole('button', { name: ACCOUNTS.RETRY }));
    expect(await screen.findByText('wb-0123456789abcdef')).toBeTruthy();
  });

  it('空池 → 添加向导引导（C1：空态必须有添加入口，不再教用户放文件）+ 刷新按钮', async () => {
    installFetch({ '/api/accounts': { body: { accounts: [], total: 0, active: 0, cooling: 0, disabled: 0, credit_total: 0 } } });
    render(<AccountsScreen />);
    expect(await screen.findByText(ACCOUNTS.EMPTY_TITLE)).toBeTruthy();
    expect(screen.getByText(ACCOUNTS.EMPTY_BODY, { exact: false })).toBeTruthy();
    expect(screen.getByRole('button', { name: ACCOUNTS.ADD_OPEN })).toBeTruthy();
    expect(screen.getByRole('button', { name: ACCOUNTS.REFRESH })).toBeTruthy();
  });

  it('30s 轮询自动重拉；错误态在下一轮自愈', async () => {
    vi.useFakeTimers();
    let ok = false;
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = String(args[0]);
      if (url !== '/api/accounts') throw new Error(`unexpected fetch: ${url}`);
      if (!ok) return { ok: false, status: 503, json: async () => ({ error: 'unavailable' }) };
      return { ok: true, status: 200, json: async () => ACCOUNTS_WIRE };
    });
    vi.stubGlobal('fetch', fn);
    render(<AccountsScreen />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(screen.getByText(ACCOUNTS.ERROR_TITLE)).toBeTruthy();
    const callsAtStart = fn.mock.calls.length;

    ok = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ACCOUNTS_POLL_MS);
    });
    expect(fn.mock.calls.length).toBeGreaterThan(callsAtStart);
    expect(screen.getByText('wb-0123456789abcdef')).toBeTruthy();
  });
});
