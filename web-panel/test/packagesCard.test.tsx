import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, waitFor } from '@testing-library/react';
import { PackagesCard } from '../src/components/PackagesCard';
import { PACKAGES } from '../src/i18n/zh';

/**
 * 积分到期提醒卡 M10-C 增量门禁：头部「检查」按钮 = POST /api/packages/refresh
 * （body {}）→ 成功后重拉 GET /api/packages；「N 个账号 · 实时查询上游」口径文案；
 * refresh 返回的 errors 逐账号可折叠；POST 失败 {error} 逐字上抛；60s 轮询不受影响。
 */

const PKG_WIRE = {
  accounts: [
    {
      platform: 'workbuddy',
      id: 'wb-pkg0001',
      nickname: '积分演示号',
      total_remain: 4600,
      nearest: { unix: 1795000000, local: '2026-11-18 00:00:00' },
      daily_burn: 92,
      batches: [{ remain: 4600, cycle_end: { unix: 1795000000, local: '2026-11-18 00:00:00' } }],
    },
    {
      platform: 'workbuddy',
      id: 'wb-pkg0002',
      nickname: null,
      total_remain: 100,
      nearest: null,
      daily_burn: null,
      batches: [],
      error: '单号探查失败：上游超时',
    },
  ],
};

function okRefresh() {
  return { ok: true, status: 200, json: async () => ({ refreshed: 2, errors: [] }) };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('PackagesCard — M10 检查按钮', () => {
  it('就绪态头部出现「N 个账号 · 实时查询上游」口径文案（沿用既有 NOTE 行）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        if (url === '/api/packages') return { ok: true, status: 200, json: async () => PKG_WIRE };
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );
    render(<PackagesCard />);
    expect(await screen.findByText('2 个账号 · 实时查询上游')).toBeTruthy();
    expect(screen.getByText(PACKAGES.NOTE)).toBeTruthy();
  });

  it('点「检查」→ POST /api/packages/refresh body {} → 完成后重拉 GET，摘要显示已刷新数', async () => {
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = String(args[0]);
      const init = (args[1] ?? {}) as { method?: string; body?: string };
      if (url === '/api/packages' && init.method === undefined) {
        return { ok: true, status: 200, json: async () => PKG_WIRE };
      }
      if (url === '/api/packages/refresh' && init.method === 'POST') return okRefresh();
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal('fetch', fn);
    render(<PackagesCard />);
    await screen.findByText('2 个账号 · 实时查询上游');
    fireEvent.click(screen.getByRole('button', { name: PACKAGES.REFRESH }));
    const call = fn.mock.calls.find((c) => String(c[0]).endsWith('/api/packages/refresh'));
    expect(call).toBeTruthy();
    expect((call?.[1] as RequestInit).body).toBe('{}');
    await waitFor(() => expect(screen.getByRole('status').textContent).toContain('已刷新 2 个账号'));
    // 完成后重拉：GET /api/packages 至少两次（初始 + 刷新后）
    const gets = fn.mock.calls.filter((c) => String(c[0]) === '/api/packages' && (c[1] as RequestInit | undefined)?.method === undefined);
    expect(gets.length).toBeGreaterThanOrEqual(2);
  });

  it('refresh 带 errors → 摘要附失败数，展开逐账号 {error} 原文', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (...args: unknown[]) => {
        const url = String(args[0]);
        if (url === '/api/packages/refresh') {
          return {
            ok: true,
            status: 200,
            json: async () => ({ refreshed: 1, errors: [{ id: 'wb-pkg0002', error: '上游超时' }] }),
          };
        }
        if (url === '/api/packages') return { ok: true, status: 200, json: async () => PKG_WIRE };
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );
    render(<PackagesCard />);
    await screen.findByText('2 个账号 · 实时查询上游');
    fireEvent.click(screen.getByRole('button', { name: PACKAGES.REFRESH }));
    const summary = await screen.findByRole('status');
    expect(summary.textContent).toContain('已刷新 1 个账号');
    const details = document.querySelector('details.packages-card__refresh-errors') as HTMLDetailsElement;
    expect(details).toBeTruthy();
    fireEvent.click(details.querySelector('summary') as HTMLElement);
    expect(details.textContent).toContain('wb-pkg0002');
    expect(details.textContent).toContain('上游超时');
  });

  it('POST 失败 {error} 逐字上抛 role=alert，不造假成功', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (...args: unknown[]) => {
        const url = String(args[0]);
        if (url === '/api/packages/refresh') {
          return { ok: false, status: 501, json: async () => ({ error: '实时查询尚未交付' }) };
        }
        if (url === '/api/packages') return { ok: true, status: 200, json: async () => PKG_WIRE };
        throw new Error(`unexpected fetch: ${url}`);
      }),
    );
    render(<PackagesCard />);
    await screen.findByText('2 个账号 · 实时查询上游');
    fireEvent.click(screen.getByRole('button', { name: PACKAGES.REFRESH }));
    // 卡内已有账号级 role=alert（wire 第二条探查失败）→ 按 refresh 错误原文精确定位
    expect(await screen.findByText('实时查询尚未交付')).toBeTruthy();
    const alerts = await screen.findAllByRole('alert');
    expect(alerts.map((a) => a.textContent)).toContain('实时查询尚未交付');
  });
});
