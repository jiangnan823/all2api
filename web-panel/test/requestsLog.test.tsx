import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { LogsScreen } from '../src/app/LogsScreen';
import { OverviewScreen } from '../src/app/OverviewScreen';
import { LOGS, USAGE } from '../src/i18n/zh';
import {
  REQUESTS_FETCH_LIMIT,
  REQUESTS_POLL_MS,
  parseRequestsSnapshot,
  parseUsageSnapshot,
} from '../src/lib/usageApi';

/**
 * M8-reqlog 前端门禁：日志屏「请求记录」tab + 总览页用量 KPI 行。
 * fetch 全程 mock（wire 蛇形，逐字对齐冻结契约）。断言面：tab 切换拉取
 * /api/requests?limit=300&result=all、结果筛选下拉换档重拉（服务端口径）、
 * 结果徽标四档 + 未知档、null token/request 字段如实显示 —、30s 轮询、
 * 错误态 + 重试、空态、总览 KPI 行 + per-platform 请求行、axe 零违规。
 */
expect.extend(toHaveNoViolations);

const REQUESTS_URL = `/api/requests?limit=${REQUESTS_FETCH_LIMIT}&result=all`;

/** 旧→新四行：success / error / stream_error / interrupted，含 null token 行。 */
const REQUESTS_WIRE = {
  requests: [
    {
      ts: { unix: 1770000000, local: '2026-02-02 03:06:40' },
      result: 'success',
      model: 'deepseek-v4-flash',
      platform: 'workbuddy',
      account_id: 'abcd1234',
      latency_ms: 739,
      prompt_tokens: 123,
      completion_tokens: 456,
      request_id: 'cmb-1',
    },
    {
      ts: { unix: 1770000060, local: '2026-02-02 03:07:40' },
      result: 'error',
      model: 'm2',
      platform: 'trae',
      account_id: 'effe5678',
      latency_ms: 12,
      prompt_tokens: null,
      completion_tokens: null,
      request_id: null,
    },
    {
      ts: { unix: 1770000120, local: '2026-02-02 03:08:40' },
      result: 'stream_error',
      model: 'm3',
      platform: 'qoder',
      account_id: '99990000',
      latency_ms: 5,
      prompt_tokens: null,
      completion_tokens: null,
      request_id: 'cmb-3',
    },
    {
      ts: { unix: 1770000180, local: '2026-02-02 03:09:40' },
      result: 'interrupted',
      model: 'm3',
      platform: 'qoder',
      account_id: '',
      latency_ms: 0,
      prompt_tokens: null,
      completion_tokens: null,
      request_id: null,
    },
  ],
};

const USAGE_WIRE = {
  window: 'process',
  requests: 6259,
  fail: 3,
  prompt_tokens: 1024,
  completion_tokens: 2048,
  by_platform: { workbuddy: { requests: 6000, fail: 1 }, trae: { requests: 259, fail: 2 } },
  by_model: { 'deepseek-v4-flash': { requests: 6000, fail: 1 } },
};

const ACCOUNTS_WIRE = {
  accounts: [
    {
      nickname: null, platform: 'workbuddy', id: 'wb-1', status: 'active', credit: 100,
      health: '1', expires_at: null, cooldown_end: null, last_selected: null,
      success: 0, fail: 0, in_flight: 0, paused: false, last_success: null, credit_total: 0,
    },
  ],
  total: 1, active: 1, cooling: 0, disabled: 0, credit_total: 100,
};

type Route = { status?: number; body?: unknown };

function installFetch(routes: Record<string, Route>) {
  const fn = vi.fn(async (...args: unknown[]) => {
    const url = typeof args[0] === 'string' ? args[0] : String(args[0]);
    const route = routes[url];
    if (!route) throw new Error(`unexpected fetch: ${url}`);
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => route.body };
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

/** 打开日志屏并切到「请求记录」tab（await 表格首行出现）；返回 fetch mock。 */
async function openRequestsTab(routes: Record<string, Route> = {}) {
  const fn = installFetch({ '/api/logs?limit=200': { body: { logs: [] } }, ...routes });
  render(<LogsScreen />);
  fireEvent.click(screen.getByRole('tab', { name: LOGS.TAB_REQUESTS }));
  await screen.findByText('deepseek-v4-flash');
  return fn;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('请求记录 tab — 就绪渲染', () => {
  it('tab 切换拉取 /api/requests?limit=300&result=all，表头与四行徽标渲染', async () => {
    const fn = await openRequestsTab({ [REQUESTS_URL]: { body: REQUESTS_WIRE } });
    const reqCalls = fn.mock.calls.filter((c) => String(c[0]).startsWith('/api/requests'));
    expect(reqCalls.length).toBeGreaterThan(0);
    expect(reqCalls.every((c) => String(c[0]) === REQUESTS_URL)).toBe(true);

    expect(screen.getByRole('table', { name: LOGS.REQ_LIST_LABEL })).toBeTruthy();
    expect(screen.getByText(LOGS.REQ_COL_TIME)).toBeTruthy();
    expect(screen.getByText(LOGS.REQ_COL_RESULT)).toBeTruthy();
    expect(screen.getByText(LOGS.REQ_COL_MODEL)).toBeTruthy();
    expect(screen.getByText(LOGS.REQ_COL_ACCOUNT)).toBeTruthy();
    expect(screen.getByText(LOGS.REQ_COL_LATENCY)).toBeTruthy();
    expect(screen.getByText(LOGS.REQ_COL_TOKENS)).toBeTruthy();

    const rows = document.querySelectorAll('tbody tr');
    expect(rows).toHaveLength(4);
    const body = document.body;
    expect(body.textContent).toContain(LOGS.RESULT_SUCCESS);
    expect(body.textContent).toContain(LOGS.RESULT_ERROR);
    expect(body.textContent).toContain(LOGS.RESULT_STREAM_ERROR);
    expect(body.textContent).toContain(LOGS.RESULT_INTERRUPTED);
  });

  it('数值列如实：耗时 739 ms、token 123 / 456、null token 与空账号 → —，时间列 HH:MM:SS + local 提示', async () => {
    await openRequestsTab({ [REQUESTS_URL]: { body: REQUESTS_WIRE } });

    expect(screen.getByText('739 ms')).toBeTruthy();
    expect(screen.getByText('123 / 456')).toBeTruthy();
    expect(screen.getByText('abcd1234')).toBeTruthy();
    const rows = [...document.querySelectorAll('tbody tr')];
    // 双 null token 与空账号如实为 —（不补零）
    expect(rows[1].textContent).toContain('—');
    expect(rows[3].textContent).toContain('—');
    const timeCell = rows[0].querySelector('td') as HTMLElement;
    expect(timeCell.textContent).toMatch(/^\d{2}:\d{2}:\d{2}$/);
    expect(timeCell.title).toBe('2026-02-02 03:06:40');
  });

  it('结果筛选换档重拉（服务端口径）；切回全部恢复', async () => {
    const fn = await openRequestsTab({
      [REQUESTS_URL]: { body: REQUESTS_WIRE },
      [`/api/requests?limit=${REQUESTS_FETCH_LIMIT}&result=error`]: {
        body: { requests: [REQUESTS_WIRE.requests[1]] },
      },
    });

    fireEvent.change(screen.getByRole('combobox', { name: LOGS.REQ_FILTER_LABEL }), {
      target: { value: 'error' },
    });
    await screen.findByText('effe5678');
    expect(document.querySelectorAll('tbody tr')).toHaveLength(1);
    expect(fn.mock.calls.some((c) => String(c[0]) === `/api/requests?limit=${REQUESTS_FETCH_LIMIT}&result=error`)).toBe(true);

    fireEvent.change(screen.getByRole('combobox', { name: LOGS.REQ_FILTER_LABEL }), {
      target: { value: 'all' },
    });
    await screen.findByText('abcd1234');
    expect(document.querySelectorAll('tbody tr')).toHaveLength(4);
  });

  it('请求记录 axe 无违规（tablist/tabpanel 结构）', async () => {
    installFetch({ [REQUESTS_URL]: { body: REQUESTS_WIRE } });
    const { container } = render(<LogsScreen />);
    fireEvent.click(screen.getByRole('tab', { name: LOGS.TAB_REQUESTS }));
    await screen.findByText('deepseek-v4-flash');
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe('请求记录 tab — 错误态 / 空态 / 轮询', () => {
  it('404 → 错误态如实展示 + 重试恢复', async () => {
    installFetch({ '/api/logs?limit=200': { body: { logs: [] } }, [REQUESTS_URL]: { status: 404, body: { error: 'not found' } } });
    render(<LogsScreen />);
    fireEvent.click(screen.getByRole('tab', { name: LOGS.TAB_REQUESTS }));
    expect(await screen.findByText(LOGS.REQ_ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('HTTP 404')).toBeTruthy();

    installFetch({ '/api/logs?limit=200': { body: { logs: [] } }, [REQUESTS_URL]: { body: REQUESTS_WIRE } });
    fireEvent.click(screen.getByRole('button', { name: LOGS.RETRY }));
    expect(await screen.findByText('deepseek-v4-flash')).toBeTruthy();
  });

  it('空列表 → 空态文案（不渲染表格）', async () => {
    installFetch({ '/api/logs?limit=200': { body: { logs: [] } }, [REQUESTS_URL]: { body: { requests: [] } } });
    render(<LogsScreen />);
    fireEvent.click(screen.getByRole('tab', { name: LOGS.TAB_REQUESTS }));
    expect(await screen.findByText(LOGS.REQ_EMPTY_TITLE)).toBeTruthy();
    expect(screen.getByText(LOGS.REQ_EMPTY_BODY)).toBeTruthy();
    expect(document.querySelector('table')).toBeNull();
  });

  it('30s 轮询自动重拉；未激活 tab 不发起 /api/requests', async () => {
    vi.useFakeTimers();
    let round = 0;
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = String(args[0]);
      if (url !== REQUESTS_URL) throw new Error(`unexpected fetch: ${url}`);
      round += 1;
      if (round === 1) return { ok: true, status: 200, json: async () => ({ requests: [REQUESTS_WIRE.requests[0]] }) };
      return { ok: true, status: 200, json: async () => ({ requests: REQUESTS_WIRE.requests }) };
    });
    vi.stubGlobal('fetch', fn);
    const { act } = await import('@testing-library/react');
    render(<LogsScreen />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    // 默认 tab（运行日志）只拉 /api/logs，从不拉 /api/requests
    expect(fn.mock.calls.filter((c) => String(c[0]).startsWith('/api/requests')).length).toBe(0);

    fireEvent.click(screen.getByRole('tab', { name: LOGS.TAB_REQUESTS }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(screen.getByText('deepseek-v4-flash')).toBeTruthy();

    const callsAtStart = fn.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(REQUESTS_POLL_MS);
    });
    expect(fn.mock.calls.length).toBeGreaterThan(callsAtStart);
    // 旧→新：轮询后第 4 行是 interrupted
    expect(document.querySelectorAll('tbody tr')).toHaveLength(4);
  });
});

describe('usageApi 解析', () => {
  it('parseRequestsSnapshot：蛇形 wire → camelCase，畸形条目丢行不炸', () => {
    const snap = parseRequestsSnapshot({
      requests: [
        REQUESTS_WIRE.requests[0],
        { ts: 1, result: 'success' }, // 缺形状字段 → 兜底 null/空串，不丢
        'junk',
      ],
    });
    expect(snap.requests).toHaveLength(2);
    expect(snap.requests[0].accountId).toBe('abcd1234');
    expect(snap.requests[0].promptTokens).toBe(123);
    expect(snap.requests[1].promptTokens).toBeNull();
    expect(snap.requests[1].latencyMs).toBe(0);
  });

  it('parseUsageSnapshot：window/桶结构 + 缺失键按 0', () => {
    const snap = parseUsageSnapshot(USAGE_WIRE);
    expect(snap.window).toBe('process');
    expect(snap.requests).toBe(6259);
    expect(snap.fail).toBe(3);
    expect(snap.byPlatform['trae']).toEqual({ requests: 259, fail: 2 });
    expect(snap.byModel['deepseek-v4-flash'].fail).toBe(1);
    const bare = parseUsageSnapshot({});
    expect(bare).toEqual({
      window: '', requests: 0, fail: 0, promptTokens: 0, completionTokens: 0,
      byPlatform: {}, byModel: {},
    });
  });
});

describe('总览页用量 KPI', () => {
  function openOverview(routes: Record<string, Route>) {
    installFetch({ '/api/accounts': { body: ACCOUNTS_WIRE }, ...routes });
    render(<OverviewScreen onOpen={() => {}} />);
    return screen.findByText('WorkBuddy');
  }

  it('KPI 摘要行：本次运行请求/失败/输入/输出 tokens + 徽注', async () => {
    await openOverview({ '/api/usage': { body: USAGE_WIRE } });
    // 徽注与相邻文案同节点拼接，用正则匹配部分串
    expect(await screen.findByText(/进程重启清零/)).toBeTruthy();
    expect(screen.getByText('6,259')).toBeTruthy();
    expect(screen.getByText('3')).toBeTruthy();
    expect(screen.getByText('1,024')).toBeTruthy();
    expect(screen.getByText('2,048')).toBeTruthy();
    expect(screen.getByLabelText(USAGE.ARIA_LABEL)).toBeTruthy();
  });

  it('per-platform 请求行来自 by_platform 桶；窗口内无请求的平台如实为 0', async () => {
    await openOverview({ '/api/usage': { body: USAGE_WIRE } });
    const card = screen.getByText('WorkBuddy').closest('.ov-card') as HTMLElement;
    expect(card.textContent).toContain('6000');
    expect(card.textContent).toContain(USAGE.KPI_FAIL);
    const traeCard = screen.getByText('Trae').closest('.ov-card') as HTMLElement;
    expect(traeCard.textContent).toContain('259');
  });

  it('usage 端点失败 → 只降级提示，不拖垮账号总览；「请求」行维持占位', async () => {
    await openOverview({ '/api/usage': { status: 404, body: { error: 'not found' } } });
    expect(await screen.findByText(USAGE.ERROR_NOTE)).toBeTruthy();
    const card = screen.getByText('WorkBuddy').closest('.ov-card') as HTMLElement;
    expect(card.textContent).toContain('统计维度待接入');
  });
});
