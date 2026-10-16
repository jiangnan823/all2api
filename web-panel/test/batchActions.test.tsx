import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, waitFor } from '@testing-library/react';
import { BatchActions } from '../src/components/BatchActions';
import { batchOp, type BatchOpResult } from '../src/lib/poolApi';

/**
 * 批量操作按钮组门禁（M10-C）：按平台渲染键位（workbuddy 四键含 travel，其余三键）、
 * POST /api/accounts/{platform}/batch-op body {"op":...}、busy 期间全键禁用、
 * 结果摘要（requested/done/skipped/failed）、失败逐账号可折叠、{error} 信封逐字上抛、
 * 完成回调 onDone（容器据此重拉快照）。
 */

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function okWire(over: Partial<BatchOpResult> = {}): BatchOpResult {
  return {
    requested: 3,
    done: 2,
    skipped: 0,
    failed: [{ id: 'wb-fail0001', error: '上游 429：请稍后再试' }],
    ...over,
  };
}

describe('BatchActions 键位按平台渲染', () => {
  it('workbuddy：四键（全部签到/旅行巡检/活跃上报/全部保活）', () => {
    vi.stubGlobal('fetch', vi.fn());
    render(<BatchActions platform="workbuddy" onDone={() => {}} />);
    const labels = Array.from(document.querySelectorAll('.pool-batch button')).map((b) => b.textContent);
    expect(labels).toEqual(['全部签到', '旅行巡检', '活跃上报', '全部保活']);
  });

  it('qoder：三键（无 travel——仅 workbuddy 上游有该语义）', () => {
    vi.stubGlobal('fetch', vi.fn());
    render(<BatchActions platform="qoder" onDone={() => {}} />);
    const labels = Array.from(document.querySelectorAll('.pool-batch button')).map((b) => b.textContent);
    expect(labels).toEqual(['全部签到', '活跃上报', '全部保活']);
  });
});

describe('BatchActions 执行流', () => {
  it('点击 → POST batch-op body {"op":"checkin"} → 摘要 + 失败可折叠 + onDone 回调', async () => {
    const fn = vi.fn(async (...args: unknown[]) => {
      const url = String(args[0]);
      const init = (args[1] ?? {}) as { method?: string; body?: string };
      if (url === '/api/accounts/workbuddy/batch-op' && init.method === 'POST') {
        return { ok: true, status: 200, json: async () => okWire() };
      }
      throw new Error(`unexpected fetch: ${url}`);
    });
    vi.stubGlobal('fetch', fn);
    const onDone = vi.fn();
    render(<BatchActions platform="workbuddy" onDone={onDone} />);
    fireEvent.click(screen.getByRole('button', { name: '全部签到' }));
    const call = fn.mock.calls.find((c) => String(c[0]).endsWith('/batch-op'));
    expect(call).toBeTruthy();
    expect((call?.[1] as RequestInit).body).toBe('{"op":"checkin"}');
    expect(await screen.findByRole('status')).toBeTruthy();
    expect(screen.getByRole('status').textContent).toBe('已执行 3 个账号：成功 2 · 跳过 0 · 失败 1');
    expect(onDone).toHaveBeenCalledTimes(1);
    // 失败明细默认折叠，展开后逐账号 {error} 原文
    const details = document.querySelector('details.pool-batch__fails') as HTMLDetailsElement;
    expect(details).toBeTruthy();
    expect(details.open).toBe(false);
    fireEvent.click(details.querySelector('summary') as HTMLElement);
    expect(details.textContent).toContain('wb-fail0001');
    expect(details.textContent).toContain('上游 429：请稍后再试');
  });

  it('travel 键 → body {"op":"travel"}；skipped 计数如实显示', async () => {
    const fn = vi.fn(async (..._args: unknown[]) => ({
      ok: true,
      status: 200,
      json: async () => okWire({ requested: 4, done: 1, skipped: 3, failed: [] }),
    }));
    vi.stubGlobal('fetch', fn);
    render(<BatchActions platform="workbuddy" onDone={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: '旅行巡检' }));
    const call = fn.mock.calls.find((c) => String(c[0]).endsWith('/batch-op'));
    expect((call?.[1] as RequestInit).body).toBe('{"op":"travel"}');
    expect(await screen.findByRole('status')).toBeTruthy();
    expect(screen.getByRole('status').textContent).toBe('已执行 4 个账号：成功 1 · 跳过 3 · 失败 0');
    // 无失败 → 不渲染折叠块
    expect(document.querySelector('details.pool-batch__fails')).toBeNull();
  });

  it('busy 期间全部键禁用且带 aria-busy；完成后恢复', async () => {
    let release!: (v: BatchOpResult) => void;
    const gate = new Promise<BatchOpResult>((res) => {
      release = res;
    });
    const fn = vi.fn(async () => gate.then((body) => ({ ok: true, status: 200, json: async () => body })));
    vi.stubGlobal('fetch', fn);
    render(<BatchActions platform="qoder" onDone={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: '全部保活' }));
    // 在途：全键禁用 + 执行中键 aria-busy + 文案带省略号
    await waitFor(() => expect(screen.getByRole('button', { name: '全部保活…' })).toBeTruthy());
    for (const b of Array.from(document.querySelectorAll('.pool-batch button'))) {
      expect((b as HTMLButtonElement).disabled).toBe(true);
    }
    expect(screen.getByRole('button', { name: '全部保活…' }).getAttribute('aria-busy')).toBe('true');
    release(okWire({ failed: [] }));
    await waitFor(() => expect((screen.getByRole('button', { name: '全部保活' }) as HTMLButtonElement).disabled).toBe(false));
  });

  it('4xx {error:"中文原因"} 逐字上抛到 role=alert，onDone 不回调', async () => {
    const onDone = vi.fn();
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({ ok: false, status: 400, json: async () => ({ error: '该平台暂不支持批量签到' }) })),
    );
    render(<BatchActions platform="qoder" onDone={onDone} />);
    fireEvent.click(screen.getByRole('button', { name: '全部签到' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe('该平台暂不支持批量签到');
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe('batchOp API 解析（poolApi）', () => {
  it('计数缺失按 0，failed 里形状不对的条目丢弃', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({
        ok: true,
        status: 200,
        json: async () => ({ done: 1, failed: [{ id: 'a1', error: 'x' }, { id: 'bad' }, 'junk', null] }),
      })),
    );
    const res = await batchOp('workbuddy', 'active');
    expect(res).toEqual({ requested: 0, done: 1, skipped: 0, failed: [{ id: 'a1', error: 'x' }] });
  });

  it('非 2xx {error} 逐字上抛', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({ ok: false, status: 501, json: async () => ({ error: '批量操作尚未交付' }) })),
    );
    await expect(batchOp('trae', 'keepalive')).rejects.toThrow('批量操作尚未交付');
  });
});
