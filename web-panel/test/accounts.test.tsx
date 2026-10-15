import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, fireEvent, cleanup, act, within, waitFor } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { AccountsScreen } from '../src/app/AccountsScreen';
import { ACCOUNTS_POLL_MS, relativeTime } from '../src/lib/poolApi';
import { ACCOUNTS } from '../src/i18n/zh';

/**
 * 账号池屏门禁（M5b → M6b 富列 + 行内操作）：fetch 全程 mock（wire 蛇形，形状按任务书冻结契约）。
 * 断言面：就绪渲染（状态卡行 + 富列表格全列 + 状态徽色 + 积分进度条 + 相对时间）、
 * 错误态 + 重试、空池引导、30s 轮询自愈、账号 ID 短码点击复制（clipboard mock）、
 * 暂停/恢复/删除行内操作端点与 confirm 流、{error} 信封逐字上抛、relativeTime 边界、
 * 积分条百分比边界、axe 零违规。
 */
expect.extend(toHaveNoViolations);

const WB_ID = 'wb-0123456789abcdef';
const WB_PAUSED_ID = 'wb-pausedaccount01';
const NOW_SEC = Math.floor(Date.now() / 1000);

/** 服务端 wire 形状（蛇形）。M6b 起每条可带 success/fail/in_flight/paused/last_success/credit_total；
 * trae 一条刻意保持旧形状（无任何新字段）验证防御式解析：不丢条目、新列按默认值渲染。 */
const ACCOUNTS_WIRE = {
  accounts: [
    {
      platform: 'workbuddy',
      id: WB_ID,
      nickname: '40 多岁的年轻小伙',
      status: 'active',
      credit: 1234.5,
      health: 'good',
      expires_at: { unix: 1790000000, local: '2026-10-15 22:00:00' },
      cooldown_end: null,
      last_selected: { unix: 1760000000, local: '2026-10-15 12:00:00' },
      success: 2577,
      fail: 3,
      in_flight: 1,
      paused: false,
      last_success: { unix: NOW_SEC - 29, local: '2026-10-15 21:30:00' },
      credit_total: 6679,
    },
    {
      // M6b 防御面：旧形状（新字段全缺）→ 计数 0 / 暂停 false / 时间 — ，条目不丢
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
      success: 0,
      fail: 0,
      in_flight: 0,
      paused: false,
      last_success: null,
      credit_total: 0,
    },
    {
      // paused=true 且 status=active：灰徽「已暂停选号」优先于可用，按钮切到「恢复选号」
      platform: 'workbuddy',
      id: WB_PAUSED_ID,
      nickname: '暂停演示号',
      status: 'active',
      credit: 600,
      health: 'good',
      expires_at: null,
      cooldown_end: null,
      last_selected: null,
      success: 10,
      fail: 0,
      in_flight: 0,
      paused: true,
      last_success: { unix: NOW_SEC - 3600 * 6, local: '2026-10-15 15:30:00' },
      credit_total: 1000,
    },
  ],
  total: 4,
  active: 2,
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

/** 定位某账号所在行（复制按钮的 aria-label 带全量 id，短码化后仍可精确定位）。 */
async function findRow(id: string): Promise<HTMLElement> {
  const copyBtn = await screen.findByRole('button', { name: `${ACCOUNTS.COPY}: ${id}` });
  const row = copyBtn.closest('tr');
  if (!row) throw new Error(`row not found for ${id}`);
  return row as HTMLElement;
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
  vi.restoreAllMocks();
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
    expect(totalCard.textContent).toBe(`${ACCOUNTS.CARD_TOTAL}4`);
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

  it('表格 M6b 十一列齐备：平台/账号/状态/积分/成功 失败/在途/健康度/到期/冷却至/最近成功/操作', async () => {
    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    render(<AccountsScreen />);
    await findRow(WB_ID);
    const table = document.querySelector('table.pool-table') as HTMLTableElement;
    const headers = Array.from(table.querySelectorAll('thead th')).map((th) => th.textContent);
    expect(headers).toEqual([
      ACCOUNTS.COL_PLATFORM,
      ACCOUNTS.COL_ACCOUNT,
      ACCOUNTS.COL_STATUS,
      ACCOUNTS.COL_CREDIT,
      ACCOUNTS.COL_SUCCESS,
      ACCOUNTS.COL_INFLIGHT,
      ACCOUNTS.COL_HEALTH,
      ACCOUNTS.COL_EXPIRES,
      ACCOUNTS.COL_COOLDOWN,
      ACCOUNTS.COL_LASTOK,
      ACCOUNTS.COL_ACTIONS,
    ]);
    const rows = Array.from(table.querySelectorAll('tbody tr'));
    expect(rows).toHaveLength(4);
    // M6b：昵称主显，id 降为短码副行（前 8…后 4），全量在 title / 复制内容里
    expect(table.textContent).toContain('40 多岁的年轻小伙');
    expect(table.textContent).toContain('wb-01234…cdef');
    expect(table.textContent).toContain('tr-ffff0…1111');
    expect((table.querySelector(`button[title="${WB_ID}"]`) as HTMLElement | null) ?? null).toBeTruthy();
    // 状态徽标：可用 / 冷却中 / 凭证失效（M6b disabled 口径）/ 已暂停选号（paused 优先，灰徽）
    const badges = Array.from(table.querySelectorAll('tbody .pool-badge'));
    expect(badges.map((b) => b.className)).toEqual([
      'pool-badge pool-badge--ok',
      'pool-badge pool-badge--warn',
      'pool-badge pool-badge--err',
      'pool-badge pool-badge--idle',
    ]);
    expect(badges.map((b) => b.textContent)).toEqual([
      '可用',
      '冷却中',
      '凭证失效',
      ACCOUNTS.STATUS_PAUSED,
    ]);
    // 到期时间解析 TimeLocal 对象形态；credit/health 缺失显示 —，不编造
    expect(table.textContent).toContain(ACCOUNTS.DASH);
  });

  it('M6b 富列取数：成功/失败、在途高亮、最近成功相对时间、积分双值+进度条、旧形状默认值', async () => {
    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    render(<AccountsScreen />);
    const wbRow = await findRow(WB_ID);
    // 成功 / 失败：2577 → 2,577；失败 >0 红色
    expect(wbRow.textContent).toContain('2,577 / 3');
    expect(wbRow.querySelector('.pool-fail--bad')).toBeTruthy();
    // 在途 >0 高亮（全表唯一）
    const hl = Array.from((document.querySelectorAll('.pool-num--hl')));
    expect(hl).toHaveLength(1);
    expect(hl[0].textContent).toBe('1');
    // 积分双值 + 进度条：1234.5/6679 → 18%；总额带千分位
    expect(wbRow.textContent).toContain('1,234.5');
    expect(wbRow.textContent).toContain(' / 6,679');
    const bar = wbRow.querySelector('.pool-creditbar i') as HTMLElement;
    expect(bar).toBeTruthy();
    expect(bar.style.width).toBe('18%');
    // 最近成功：29 秒前（相对时间，纯前端换算）；暂停演示号 6 小时前
    expect(wbRow.textContent).toContain('29 秒前');
    const pausedRow = await findRow(WB_PAUSED_ID);
    expect(pausedRow.textContent).toContain('6 小时前');
    // 旧形状（trae）：新字段全缺 → 在途 0 弱化、最近成功 —、积分只显单值 88 无条
    const traeRow = await findRow('tr-ffff00001111');
    expect(traeRow.querySelector('.pool-num--dim')?.textContent).toBe('0');
    expect(traeRow.querySelector('.pool-creditbar')).toBeNull();
    expect(traeRow.textContent).toContain('88');
    // qoder：credit 0 且 credit_total 0（未知/未播种）→ 只显单值 0，无条；最近成功 —
    const qoderRow = await findRow('qd-deadbeef42');
    expect(qoderRow.querySelector('.pool-creditbar')).toBeNull();
    expect(qoderRow.querySelector('.pool-fail--bad')).toBeNull();
    // 暂停行按钮态：恢复选号（非暂停行是暂停选号）
    expect(within(pausedRow).getByRole('button', { name: ACCOUNTS.BTN_RESUME })).toBeTruthy();
    expect(within(wbRow).getByRole('button', { name: ACCOUNTS.BTN_PAUSE })).toBeTruthy();
    // 每行都有删除按钮
    for (const row of [wbRow, traeRow, qoderRow, pausedRow]) {
      expect(within(row).getByRole('button', { name: ACCOUNTS.BTN_REMOVE })).toBeTruthy();
    }
  });

  it('paused 判定优先于 status：disabled+paused → 灰徽「已暂停选号」而非红徽，按钮为恢复选号', async () => {
    installFetch({
      '/api/accounts': {
        body: {
          accounts: [
            {
              platform: 'workbuddy',
              id: 'wb-pauseddis01',
              status: 'disabled',
              credit: 0,
              health: null,
              expires_at: 0,
              cooldown_end: 0,
              last_selected: 0,
              paused: true,
            },
          ],
          total: 1,
          active: 0,
          cooling: 0,
          disabled: 1,
          credit_total: 0,
        },
      },
    });
    render(<AccountsScreen />);
    const row = await findRow('wb-pauseddis01');
    const badge = row.querySelector('.pool-badge') as HTMLElement;
    expect(badge.className).toBe('pool-badge pool-badge--idle');
    expect(badge.textContent).toBe(ACCOUNTS.STATUS_PAUSED);
    expect(row.querySelector('.pool-badge--err')).toBeNull();
    expect(within(row).getByRole('button', { name: ACCOUNTS.BTN_RESUME })).toBeTruthy();
    expect(within(row).queryByRole('button', { name: ACCOUNTS.BTN_PAUSE })).toBeNull();
  });

  it('点击账号 ID 短码复制全量 id，出现「已复制」提示', async () => {
    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    render(<AccountsScreen />);
    await findRow(WB_ID);
    const copyBtn = screen.getByRole('button', { name: `${ACCOUNTS.COPY}: ${WB_ID}` });
    fireEvent.click(copyBtn);
    expect(await screen.findByText(ACCOUNTS.COPIED)).toBeTruthy();
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(WB_ID);
  });

  it('axe 无违规', async () => {
    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    const { container } = render(<AccountsScreen />);
    await findRow(WB_ID);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe('账号池屏 — 行内操作（M6b）', () => {
  it('未暂停账号点「暂停选号」→ POST /pause，成功后重拉快照（按钮翻转为恢复选号）', async () => {
    let pausedNow = false;
    const wirePaused = {
      ...ACCOUNTS_WIRE,
      accounts: ACCOUNTS_WIRE.accounts.map((a) => (a.id === WB_ID ? { ...a, paused: true } : a)),
    };
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = String(args[0]);
      if (url === '/api/accounts') {
        return { ok: true, status: 200, json: async () => (pausedNow ? wirePaused : ACCOUNTS_WIRE) };
      }
      if (url === `/api/accounts/workbuddy:${WB_ID}/pause`) {
        pausedNow = true;
        return { ok: true, status: 200, json: async () => ({ ok: true, paused: true }) };
      }
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal('fetch', fn);
    render(<AccountsScreen />);
    const row = await findRow(WB_ID);
    fireEvent.click(within(row).getByRole('button', { name: ACCOUNTS.BTN_PAUSE }));
    const call = fn.mock.calls.find((c) => String(c[0]).endsWith('/pause'));
    expect(call).toBeTruthy();
    expect((call?.[1] as RequestInit).method).toBe('POST');
    expect((call?.[1] as RequestInit).body).toBe('{}');
    // 重拉后的快照里 paused=true → 全表有两个「恢复选号」（wb + 暂停演示号）
    await waitFor(() =>
      expect(screen.getAllByRole('button', { name: ACCOUNTS.BTN_RESUME })).toHaveLength(2),
    );
  });

  it('已暂停账号点「恢复选号」→ POST /resume', async () => {
    const fn = installFetch({
      '/api/accounts': { body: ACCOUNTS_WIRE },
      [`/api/accounts/workbuddy:${WB_PAUSED_ID}/resume`]: { body: { ok: true, paused: false } },
    });
    render(<AccountsScreen />);
    const row = await findRow(WB_PAUSED_ID);
    fireEvent.click(within(row).getByRole('button', { name: ACCOUNTS.BTN_RESUME }));
    await waitFor(() =>
      expect(fn.mock.calls.some((c) => String(c[0]) === `/api/accounts/workbuddy:${WB_PAUSED_ID}/resume`)).toBe(true),
    );
  });

  it('删除：confirm 文案含「凭证文件将被删除」；确认 → POST remove → 行从表格消失', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockImplementation(() => true);
    let removed = false;
    const wireWithoutWb = {
      ...ACCOUNTS_WIRE,
      accounts: ACCOUNTS_WIRE.accounts.filter((a) => a.id !== WB_ID),
      total: 3,
    };
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = String(args[0]);
      if (url === '/api/accounts') {
        return { ok: true, status: 200, json: async () => (removed ? wireWithoutWb : ACCOUNTS_WIRE) };
      }
      if (url === `/api/accounts/workbuddy:${WB_ID}/remove`) {
        removed = true;
        return { ok: true, status: 200, json: async () => ({ ok: true }) };
      }
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal('fetch', fn);
    render(<AccountsScreen />);
    const row = await findRow(WB_ID);
    fireEvent.click(within(row).getByRole('button', { name: ACCOUNTS.BTN_REMOVE }));
    await waitFor(() => expect(confirmSpy).toHaveBeenCalled());
    expect(String(confirmSpy.mock.calls[0][0])).toContain('凭证文件将被删除');
    await waitFor(() =>
      expect(fn.mock.calls.some((c) => String(c[0]).endsWith('/remove') && (c[1] as RequestInit).method === 'POST')).toBe(
        true,
      ),
    );
    // 重拉后的快照不再含该账号 → 行消失
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: `${ACCOUNTS.COPY}: ${WB_ID}` })).toBeNull(),
    );
  });

  it('删除：confirm 取消 → 不发 remove 请求', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockImplementation(() => false);
    const fn = installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE } });
    render(<AccountsScreen />);
    const row = await findRow(WB_ID);
    fireEvent.click(within(row).getByRole('button', { name: ACCOUNTS.BTN_REMOVE }));
    expect(confirmSpy).toHaveBeenCalledTimes(1);
    await act(async () => {});
    expect(fn.mock.calls.some((c) => String(c[0]).endsWith('/remove'))).toBe(false);
  });

  it('行内操作失败：4xx {error:"中文原因"} 逐字上抛，不翻译不包装', async () => {
    installFetch({
      '/api/accounts': { body: ACCOUNTS_WIRE },
      [`/api/accounts/workbuddy:${WB_ID}/pause`]: { status: 400, body: { error: '该账号正在冷却，暂不能暂停' } },
    });
    render(<AccountsScreen />);
    const row = await findRow(WB_ID);
    fireEvent.click(within(row).getByRole('button', { name: ACCOUNTS.BTN_PAUSE }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe(`${ACCOUNTS.ACTION_ERR_PREFIX}该账号正在冷却，暂不能暂停`);
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
    await screen.findByRole('button', { name: `${ACCOUNTS.COPY}: ${WB_ID}` });
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
    expect(screen.getByRole('button', { name: `${ACCOUNTS.COPY}: ${WB_ID}` })).toBeTruthy();
  });
});

describe('relativeTime 相对时间（M6b 最近成功列）', () => {
  /** 冻结时钟：unix 秒。 */
  function freezeAt(sec: number) {
    vi.useFakeTimers();
    vi.setSystemTime(sec * 1000);
  }

  it('null/0 → null（调用方显示 —，不编造时间）', () => {
    expect(relativeTime(null)).toBeNull();
    expect(relativeTime(0)).toBeNull();
  });

  it('刚刚：当前时刻与未来时间（时钟偏移不出现负数）', () => {
    freezeAt(1_800_000_000);
    expect(relativeTime(1_800_000_000)).toBe('刚刚');
    expect(relativeTime(1_800_000_030)).toBe('刚刚');
  });

  it('秒级：29 秒前', () => {
    freezeAt(1_800_000_000);
    expect(relativeTime(1_800_000_000 - 29)).toBe('29 秒前');
    expect(relativeTime(1_800_000_000 - 59)).toBe('59 秒前');
  });

  it('分钟级：边界 60s 归分钟；5 分钟前', () => {
    freezeAt(1_800_000_000);
    expect(relativeTime(1_800_000_000 - 60)).toBe('1 分钟前');
    expect(relativeTime(1_800_000_000 - 300)).toBe('5 分钟前');
    expect(relativeTime(1_800_000_000 - 3599)).toBe('59 分钟前');
  });

  it('小时级：边界 3600s 归小时；6 小时前', () => {
    freezeAt(1_800_000_000);
    expect(relativeTime(1_800_000_000 - 3600)).toBe('1 小时前');
    expect(relativeTime(1_800_000_000 - 3600 * 6)).toBe('6 小时前');
    expect(relativeTime(1_800_000_000 - 86399)).toBe('23 小时前');
  });

  it('天级：边界 86400s 归天；3 天前', () => {
    freezeAt(1_800_000_000);
    expect(relativeTime(1_800_000_000 - 86400)).toBe('1 天前');
    expect(relativeTime(1_800_000_000 - 86400 * 3)).toBe('3 天前');
  });
});

describe('积分列进度条百分比边界（M6b）', () => {
  function wireWith(credit: unknown, creditTotal: unknown, withTotalKey = true) {
    const account: Record<string, unknown> = {
      platform: 'workbuddy',
      id: 'wb-bar',
      status: 'active',
      credit,
      health: null,
      expires_at: 0,
      cooldown_end: 0,
      last_selected: 0,
      success: 0,
      fail: 0,
      in_flight: 0,
      paused: false,
      last_success: null,
    };
    if (withTotalKey) account['credit_total'] = creditTotal;
    return { accounts: [account], total: 1, active: 1, cooling: 0, disabled: 0, credit_total: 0 };
  }

  async function renderBar(credit: unknown, creditTotal: unknown, withTotalKey = true) {
    installFetch({ '/api/accounts': { body: wireWith(credit, creditTotal, withTotalKey) } });
    render(<AccountsScreen />);
    const row = await findRow('wb-bar');
    return {
      row,
      bar: row.querySelector('.pool-creditbar i') as HTMLElement | null,
      cell: row.querySelector('.pool-credit') as HTMLElement,
    };
  }

  it('正常比例：600/1000 → 条宽 60%，双值显示', async () => {
    const { bar, cell } = await renderBar(600, 1000);
    expect(bar?.style.width).toBe('60%');
    expect(cell.textContent).toContain('600');
    expect(cell.textContent).toContain('/ 1,000');
  });

  it('0/0（未知/未播种）→ 只显单值 0，无条', async () => {
    const { bar, cell } = await renderBar(0, 0);
    expect(bar).toBeNull();
    expect(cell.textContent).toBe('0');
  });

  it('总额键缺失 → 只显单值，无条', async () => {
    const { bar, cell } = await renderBar(88, undefined, false);
    expect(bar).toBeNull();
    expect(cell.textContent).toBe('88');
  });

  it('剩余缺失 → 显示 —，无条（不编造比例）', async () => {
    const { bar, cell } = await renderBar(null, 500);
    expect(bar).toBeNull();
    expect(cell.textContent).toBe(ACCOUNTS.DASH);
  });

  it('剩余 > 总额（服务端异常）→ 数字如实显示，条钳到 100%', async () => {
    const { bar, cell } = await renderBar(2000, 1000);
    expect(bar?.style.width).toBe('100%');
    expect(cell.textContent).toContain('2,000');
    expect(cell.textContent).toContain('/ 1,000');
  });
});
