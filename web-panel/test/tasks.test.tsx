import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { TasksScreen } from '../src/app/TasksScreen';
import { TASKS, taskDisplayName } from '../src/i18n/zh';

/**
 * 任务中心屏（M5c）门禁：读 GET /api/tasks（wire 全蛇形），四任务卡片 + cron + enabled
 * 徽色 + 最近一次运行统计；手动触发端点不存在 → 只读说明 + 无执行按钮（如实边界）。
 * fetch 全程 mock（沿用 settings.test 的 stubGlobal 模式）；wire fixture 逐字段对齐
 * 任务批交付契约 {tasks:[{name, cron, enabled, last_run:{total,success,failed,skipped,at}|null}]}。
 */
expect.extend(toHaveNoViolations);

// ---------- wire fixtures（蛇形，与任务批交付契约一致） ----------

const TASKS_WIRE = {
  tasks: [
    {
      name: 'signin',
      cron: '0 9,21 * * *',
      enabled: true,
      last_run: { name: 'signin', total: 3, success: 2, failed: 1, skipped: 0, at: { unix: 1760000200, local: '2026-10-15 10:00:00' } },
    },
    {
      name: 'active_check',
      cron: '0 10 * * *',
      enabled: true,
      last_run: { total: 5, success: 5, failed: 0, skipped: 0, at: { unix: 1760000200, local: '2026-10-15 10:00:00' } },
    },
    {
      name: 'cat_travel',
      cron: '30 8 * * *',
      enabled: false,
      last_run: null,
    },
    {
      name: 'token_keepalive',
      cron: '0 22 * * *',
      enabled: true,
      last_run: { total: 4, success: 3, failed: 0, skipped: 1, at: { unix: 1760000200, local: '2026-10-15 10:00:00' } },
    },
  ],
};

const EMPTY_WIRE = { tasks: [] };

// ---------- fetch mock ----------

type Route = { status?: number; body?: unknown; reject?: boolean };
type Routes = Record<string, Route>;

function installFetch(routes: Routes) {
  const fn = vi.fn(async (...args: unknown[]) => {
    const url = typeof args[0] === 'string' ? args[0] : String(args[0]);
    const route = routes[url] ?? { status: 404, body: { error: 'not found' } };
    if (route.reject) throw new TypeError('fetch failed');
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => route.body };
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

async function renderReady(
  routes: Routes = { '/api/tasks': { body: TASKS_WIRE } },
  awaitText: string = taskDisplayName('signin'),
) {
  const fn = installFetch(routes);
  render(<TasksScreen />);
  await screen.findByText(awaitText);
  return fn;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('任务中心屏 — 加载 / 错误 / 重试', () => {
  it('挂载先出加载态；就绪后四任务卡片齐全，请求落在 GET /api/tasks', async () => {
    const fn = await renderReady();
    expect(fn.mock.calls.map((c) => String(c[0]))).toEqual(['/api/tasks']);
    for (const name of ['signin', 'active_check', 'cat_travel', 'token_keepalive']) {
      expect(document.querySelector(`li[data-task="${name}"]`)).not.toBeNull();
    }
  });

  it('500 → 错误态带 HTTP 码与诊断信息；重试成功后恢复', async () => {
    installFetch({ '/api/tasks': { status: 500, body: { error: 'boom' } } });
    render(<TasksScreen />);
    expect(await screen.findByText(TASKS.ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('HTTP 500')).toBeTruthy();
    installFetch({ '/api/tasks': { body: TASKS_WIRE } }); // apiGet 每次调用都取全局 fetch，桩可直接换
    fireEvent.click(screen.getByRole('button', { name: TASKS.RETRY }));
    await screen.findByText(taskDisplayName('signin'));
  });

  it('网络错误 → 如实展示 network error，不假成功', async () => {
    installFetch({ '/api/tasks': { reject: true } });
    render(<TasksScreen />);
    expect(await screen.findByText('network error')).toBeTruthy();
  });

  it('形状异常（tasks 非数组）→ 错误态不静默兜底', async () => {
    installFetch({ '/api/tasks': { body: { tasks: 'nope' } } });
    render(<TasksScreen />);
    expect(await screen.findByText(TASKS.ERROR_TITLE)).toBeTruthy();
  });
});

describe('任务中心屏 — 卡片内容（中文名 / cron / 徽色 / 统计）', () => {
  it('中文名与说明来自 i18n 映射；cron 原样展示', async () => {
    await renderReady();
    const signin = document.querySelector('li[data-task="signin"]')!;
    expect(signin.textContent).toContain('自动签到');
    expect(signin.textContent).toContain('每日签到');
    expect(signin.textContent).toContain('0 9,21 * * *');
    const cat = document.querySelector('li[data-task="cat_travel"]')!;
    expect(cat.textContent).toContain('猫猫旅行');
  });

  it('未知任务名如实回显原始标识，不装懂', async () => {
    await renderReady(
      { '/api/tasks': { body: { tasks: [{ name: 'future_task', cron: '0 6 * * *', enabled: true, last_run: null }] } } },
      'future_task',
    );
  });

  it('enabled 徽色：true → 已启用（on 徽标），false → 已停用（off 徽标）', async () => {
    await renderReady();
    const on = document.querySelector('li[data-task="signin"] .tasks-screen__badge--on')!;
    expect(on.textContent).toBe(TASKS.ENABLED_ON);
    const off = document.querySelector('li[data-task="cat_travel"] .tasks-screen__badge--off')!;
    expect(off.textContent).toBe(TASKS.ENABLED_OFF);
  });

  it('last_run 统计五元组齐全（total/success/failed/skipped/at），wire 的重复 name 字段被忽略', async () => {
    await renderReady();
    const signin = document.querySelector('li[data-task="signin"]')!;
    expect(signin.textContent).toContain('3'); // total
    expect(signin.textContent).toContain('2'); // success
    expect(signin.textContent).toContain('1'); // failed
    expect(signin.textContent).toContain('2026-10-15 10:00:00');
    expect(signin.textContent).toContain(TASKS.STAT_SKIPPED); // skipped=0 也展示列名
  });

  it('last_run=null → 「尚未运行」中性空态，不渲染统计行', async () => {
    await renderReady();
    const cat = document.querySelector('li[data-task="cat_travel"]')!;
    expect(cat.textContent).toContain(TASKS.NEVER_RUN);
    expect(cat.querySelector('.tasks-screen__stats')).toBeNull();
  });
});

describe('任务中心屏 — 只读边界与空态', () => {
  it('顶部诚实说明 + 计数 + 刷新按钮；页面无任何「执行/立即运行」按钮（手动触发端点不存在）', async () => {
    await renderReady();
    expect(screen.getByText(TASKS.NOTE)).toBeTruthy();
    expect(screen.getByText(TASKS.COUNT.replace('{count}', '4'))).toBeTruthy();
    // 只读边界：按钮 = 三个平台页签 + 刷新（错误态才有的重试），无任何执行类按钮
    const buttons = screen.getAllByRole('button').map((b) => b.textContent);
    expect(buttons).toEqual(['WorkBuddy', 'Qoder', 'Trae', TASKS.REFRESH]);
  });

  it('空 tasks → 空态文案 + 计数为 0', async () => {
    await renderReady({ '/api/tasks': { body: EMPTY_WIRE } }, TASKS.EMPTY);
    expect(screen.getByText(TASKS.EMPTY)).toBeTruthy();
    expect(screen.getByText(TASKS.COUNT.replace('{count}', '0'))).toBeTruthy();
  });

  it('刷新按钮重发 GET /api/tasks 并以新数据重渲染', async () => {
    const fn = await renderReady();
    expect(fn.mock.calls).toHaveLength(1);
    const fn2 = installFetch({
      '/api/tasks': { body: { tasks: [{ name: 'signin', cron: '0 8 * * *', enabled: false, last_run: null }] } },
    });
    fireEvent.click(screen.getByRole('button', { name: TASKS.REFRESH }));
    await screen.findByText('0 8 * * *');
    // apiGet 每次调用都取全局 fetch：第二个请求落在替换后的桩上
    expect(fn2.mock.calls.map((c) => String(c[0]))).toEqual(['/api/tasks']);
    expect(document.querySelector('li[data-task="cat_travel"]')).toBeNull();
  });
});

describe('任务中心屏 — axe 基线', () => {
  it('就绪态无违规', async () => {
    installFetch({ '/api/tasks': { body: TASKS_WIRE } });
    const { container } = render(<TasksScreen />);
    await screen.findByText(taskDisplayName('signin'));
    expect(await axe(container)).toHaveNoViolations();
  });

  it('错误态无违规', async () => {
    installFetch({ '/api/tasks': { status: 500, body: { error: 'boom' } } });
    const { container } = render(<TasksScreen />);
    await screen.findByText(TASKS.ERROR_TITLE);
    expect(await axe(container)).toHaveNoViolations();
  });
});
