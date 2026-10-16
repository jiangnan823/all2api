import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, waitFor } from '@testing-library/react';
import { ModelLocksCard } from '../src/components/ModelLocksCard';
import { MODELLOCKS } from '../src/i18n/zh';
import { unlockModelLocks } from '../src/lib/poolApi';

/**
 * 模型锁池卡 M10-C 增量门禁：locks[] 增 scope（域）与 available/total（可选/总数）两列、
 * 行级「全池解锁」POST /api/model-locks/unlock body {"model":...}、表头区「全部解锁」body {}、
 * 解锁成功后重拉锁表、busy 期间解锁键全禁用、{error} 逐字上抛、可空列 "—" 占位。
 */

const LOCKS_WIRE = {
  locks: [
    {
      model: 'deepseek-v4-flash',
      platform: 'workbuddy',
      scope: 'cn-east.api.workbuddy.example',
      locked_accounts: 2,
      total_accounts: 5,
      available: 3,
      total: 10,
      earliest_unlock: { unix: 1_800_000_000, local: '2026-01-15 12:00:00' },
      reason: '429 rate limit',
    },
    {
      // 旧形状：scope/available/total 缺失 → "—" 占位
      model: 'qwen5-coder',
      platform: 'qoder',
      locked_accounts: 1,
      total_accounts: 1,
      earliest_unlock: null,
      reason: null,
    },
  ],
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function install(routes: Record<string, { status?: number; body?: unknown }>) {
  return vi.fn(async (...args: unknown[]) => {
    const url = String(args[0]);
    const route = routes[url];
    if (!route) throw new Error(`unexpected fetch: ${url}`);
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => route.body };
  });
}

describe('ModelLocksCard — M10 列与解锁', () => {
  it('七列表头齐备：模型/域/锁定账号/可选·总数/最早解锁/原因/操作', async () => {
    vi.stubGlobal('fetch', install({ '/api/model-locks': { body: LOCKS_WIRE } }));
    render(<ModelLocksCard />);
    expect(await screen.findByText('deepseek-v4-flash')).toBeTruthy();
    const ths = Array.from(document.querySelectorAll('.modelocks-card__table th')).map((th) => th.textContent);
    expect(ths).toEqual([MODELLOCKS.COL_MODEL, '域', MODELLOCKS.COL_LOCKED, '可选/总数', MODELLOCKS.COL_EARLIEST, MODELLOCKS.COL_REASON, '操作']);
    // 域 + 可选/总数取数；旧形状行 → "—" 占位
    const rows = Array.from(document.querySelectorAll('.modelocks-card__table tbody tr'));
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain('cn-east.api.workbuddy.example');
    expect(rows[0].textContent).toContain('3 / 10');
    expect(rows[1].textContent).toContain('—');
    // 表头区双按钮：全部解锁 + 刷新
    expect(screen.getByRole('button', { name: '全部解锁' })).toBeTruthy();
    expect(screen.getByRole('button', { name: MODELLOCKS.REFRESH })).toBeTruthy();
    // 行级按钮：每行一个「全池解锁」
    expect(screen.getAllByRole('button', { name: '全池解锁' })).toHaveLength(2);
  });

  it('行级「全池解锁」→ POST body {"model":"deepseek-v4-flash"} → 成功后重拉锁表', async () => {
    let unlocked = false;
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = String(args[0]);
      const init = (args[1] ?? {}) as { method?: string; body?: string };
      if (url === '/api/model-locks' && init.method === undefined) {
        return {
          ok: true,
          status: 200,
          json: async () => (unlocked ? { locks: [] } : LOCKS_WIRE),
        };
      }
      if (url === '/api/model-locks/unlock' && init.method === 'POST') {
        unlocked = true;
        expect(init.body).toBe('{"model":"deepseek-v4-flash"}');
        return { ok: true, status: 200, json: async () => ({ unlocked: 1 }) };
      }
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal('fetch', fn);
    render(<ModelLocksCard />);
    fireEvent.click((await screen.findAllByRole('button', { name: '全池解锁' }))[0]);
    await waitFor(() => expect(screen.getByText(MODELLOCKS.EMPTY)).toBeTruthy());
  });

  it('表头区「全部解锁」→ POST body {}（全模型）', async () => {
    const fn = install({
      '/api/model-locks': { body: LOCKS_WIRE },
      '/api/model-locks/unlock': { body: { unlocked: 2 } },
    });
    vi.stubGlobal('fetch', fn);
    render(<ModelLocksCard />);
    await screen.findByText('deepseek-v4-flash');
    fireEvent.click(screen.getByRole('button', { name: '全部解锁' }));
    await waitFor(() => {
      const call = fn.mock.calls.find((c) => String(c[0]) === '/api/model-locks/unlock');
      expect(call).toBeTruthy();
      expect((call?.[1] as RequestInit).body).toBe('{}');
    });
  });

  it('解锁失败 {error} → 「解锁失败：」前缀逐字上抛 role=alert', async () => {
    vi.stubGlobal(
      'fetch',
      install({
        '/api/model-locks': { body: LOCKS_WIRE },
        '/api/model-locks/unlock': { status: 400, body: { error: '该模型不在锁池' } },
      }),
    );
    render(<ModelLocksCard />);
    await screen.findByText('deepseek-v4-flash');
    fireEvent.click(screen.getByRole('button', { name: '全部解锁' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe('解锁失败：该模型不在锁池');
  });

  it('解锁在途：解锁键全禁用 + aria-busy', async () => {
    let release!: () => void;
    const gate = new Promise<void>((res) => {
      release = res;
    });
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = String(args[0]);
      if (url === '/api/model-locks/unlock') {
        await gate;
        return { ok: true, status: 200, json: async () => ({ unlocked: 1 }) };
      }
      if (url === '/api/model-locks') return { ok: true, status: 200, json: async () => LOCKS_WIRE };
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal('fetch', fn);
    render(<ModelLocksCard />);
    await screen.findByText('deepseek-v4-flash');
    fireEvent.click(screen.getByRole('button', { name: '全部解锁' }));
    await waitFor(() =>
      expect((screen.getByRole('button', { name: '全部解锁' }) as HTMLButtonElement).disabled).toBe(true),
    );
    // busy 面 = 解锁键（全部解锁 + 各行全池解锁）；「刷新」不参与解锁互斥
    const unlockBtns = Array.from(document.querySelectorAll('.modelocks-card button')).filter(
      (b) => b.textContent === '全部解锁' || b.textContent === '全池解锁',
    );
    expect(unlockBtns.length).toBe(3);
    for (const b of unlockBtns) {
      expect((b as HTMLButtonElement).disabled).toBe(true);
      expect(b.getAttribute('aria-busy')).toBe('true');
    }
    release();
    await waitFor(() =>
      expect((screen.getByRole('button', { name: '全部解锁' }) as HTMLButtonElement).disabled).toBe(false),
    );
  });
});

describe('unlockModelLocks API（poolApi）', () => {
  it('model=null → body {}；非 2xx {error} 逐字上抛', async () => {
    const fn = vi.fn(async (...args: unknown[]) => {
      const init = (args[1] ?? {}) as { body?: string };
      if (init.body === '{}') return { ok: true, status: 200, json: async () => ({}) };
      return { ok: false, status: 409, json: async () => ({ error: '锁状态已变化' }) };
    });
    vi.stubGlobal('fetch', fn);
    await expect(unlockModelLocks(null)).resolves.toBeUndefined();
    await expect(unlockModelLocks('deepseek-v4-flash')).rejects.toThrow('锁状态已变化');
  });
});
