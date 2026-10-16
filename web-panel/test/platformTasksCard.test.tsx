import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, act } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { PlatformTasksCard, TASKS_POLL_MS } from '../src/components';
import { TASKS, taskDisplayName } from '../src/i18n/zh';

/**
 * 平台定时任务卡（M10-A）门禁：GET /api/tasks 按 task.platform 过滤本平台任务（platform 为
 * M10 新增 wire 键，缺失即形状异常）+ GET /api/config 读冻结调度键；每任务一行 =
 * 任务名 + 24 小时时点选择器 + 启用徽标；保存走与配置页同一 POST /api/config（只发改动冻结键，
 * 键名冻结：signin_cron_{platform} / task_cron_{platform} / token_keepalive_{platform} /
 * cat_travel_cron）。只做调度编辑，无「立即运行」；未知任务名只读回显，不发明键名。
 * fetch 全程 mock（沿用 tasks/settings 测试的 stubGlobal 模式）；wire fixture 逐字段对齐
 * M10 冻结契约 {tasks:[{name, cron, enabled, last_run, platform}]}。
 */
expect.extend(toHaveNoViolations);

// ---------- wire fixtures（蛇形，与 M10 冻结契约一致） ----------

const TASKS_WIRE = {
  tasks: [
    {
      name: 'signin',
      cron: '0 9,21 * * *',
      enabled: true,
      platform: 'workbuddy',
      last_run: { name: 'signin', total: 3, success: 2, failed: 1, skipped: 0, at: { unix: 1760000200, local: '2026-10-15 10:00:00' } },
    },
    { name: 'active_check', cron: '0 10 * * *', enabled: true, platform: 'workbuddy', last_run: null },
    { name: 'cat_travel', cron: '0 8 * * *', enabled: false, platform: 'workbuddy', last_run: null },
    { name: 'token_keepalive', cron: '0 22 * * *', enabled: true, platform: 'workbuddy', last_run: null },
    { name: 'signin', cron: '0 7 * * *', enabled: true, platform: 'qoder', last_run: null },
  ],
};

const CONFIG_RAW = {
  schedule: {
    signin_cron_workbuddy: '0 9,21 * * *',
    task_cron_workbuddy: '0 10 * * *',
    token_keepalive_workbuddy: '0 22 * * *',
    cat_travel_cron: '0 8 * * *',
    signin_cron_qoder: '0 7 * * *',
  },
};

const CONFIG_WIRE = { raw: CONFIG_RAW, feature_flags: {}, throttle: {}, advanced: {}, notice_version: '1' };

/** 保存成功回包：applied.raw 的 signin_cron_workbuddy 已是 0 9,20（服务端回读为准）。 */
const SAVE_OK_WIRE = {
  ok: true,
  applied: { ...CONFIG_WIRE, raw: { schedule: { ...CONFIG_RAW.schedule, signin_cron_workbuddy: '0 9,20 * * *' } } },
};

// ---------- fetch mock ----------

type Route = { status?: number; body?: unknown; reject?: boolean };
type Routes = Record<string, Route>;

function installFetch(routes: Routes) {
  const fn = vi.fn(async (...args: unknown[]) => {
    const url = typeof args[0] === 'string' ? args[0] : String(args[0]);
    const method = (args[1] as RequestInit | undefined)?.method ?? 'GET';
    const route = (method !== 'GET' ? routes[`POST ${url}`] : undefined) ?? routes[url]
      ?? { status: 404, body: { error: 'not found' } };
    if (route.reject) throw new TypeError('fetch failed');
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => route.body };
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

const OK_ROUTES: Routes = { '/api/tasks': { body: TASKS_WIRE }, '/api/config': { body: CONFIG_WIRE } };

function postCallsOf(fn: ReturnType<typeof installFetch>) {
  return fn.mock.calls
    .filter((c) => String(c[0]) === '/api/config' && (c[1] as RequestInit | undefined)?.method === 'POST')
    .map((c) => JSON.parse((c[1] as RequestInit).body as string));
}

async function renderReady(platform: 'workbuddy' | 'qoder' | 'trae' = 'workbuddy', routes: Routes = OK_ROUTES) {
  const fn = installFetch(routes);
  render(<PlatformTasksCard platform={platform} />);
  await screen.findByText(taskDisplayName('signin'));
  return fn;
}

/** 状态汇总行 = 「已注册 N 个定时任务 · 状态」（与组件 bar-note 拼装一致，getByText 按整节点匹配）。 */
const noteOf = (state: string) => `${TASKS.COUNT.replace('{count}', '4')} · ${state}`;

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('平台任务卡 — 加载 / 错误 / 重试', () => {
  it('挂载并行拉 GET /api/tasks + GET /api/config；就绪后渲染本平台任务行', async () => {
    const fn = await renderReady();
    const urls = fn.mock.calls.map((c) => String(c[0])).sort();
    expect(urls).toEqual(['/api/config', '/api/tasks']);
    for (const name of ['signin', 'active_check', 'cat_travel', 'token_keepalive']) {
      expect(document.querySelector(`li[data-task="${name}"]`)).not.toBeNull();
    }
  });

  it('/api/tasks 500 → 错误态带 HTTP 码；重试成功后恢复', async () => {
    installFetch({ ...OK_ROUTES, '/api/tasks': { status: 500, body: { error: 'boom' } } });
    render(<PlatformTasksCard platform="workbuddy" />);
    expect(await screen.findByText(TASKS.ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('HTTP 500')).toBeTruthy();
    installFetch(OK_ROUTES); // apiGet 每次调用都取全局 fetch，桩可直接换
    fireEvent.click(screen.getByRole('button', { name: TASKS.RETRY }));
    await screen.findByText(taskDisplayName('signin'));
  });

  it('网络错误 / tasks 非数组 / platform 键缺失 → 均错误态不静默兜底（platform 为 M10 新增键）', async () => {
    installFetch({ '/api/tasks': { reject: true } });
    render(<PlatformTasksCard platform="workbuddy" />);
    expect(await screen.findByText('network error')).toBeTruthy();

    cleanup();
    installFetch({ ...OK_ROUTES, '/api/tasks': { body: { tasks: 'nope' } } });
    render(<PlatformTasksCard platform="workbuddy" />);
    expect(await screen.findByText(TASKS.ERROR_TITLE)).toBeTruthy();

    cleanup();
    const legacy = { tasks: [{ name: 'signin', cron: '0 9 * * *', enabled: true, last_run: null }] };
    installFetch({ ...OK_ROUTES, '/api/tasks': { body: legacy } });
    render(<PlatformTasksCard platform="workbuddy" />);
    expect(await screen.findByText(TASKS.ERROR_TITLE)).toBeTruthy();
  });
});

describe('平台任务卡 — 平台过滤与行内容', () => {
  it('workbuddy 卡只显示 workbuddy 任务；qoder 的 signin 行不出现', async () => {
    await renderReady();
    expect(document.querySelector('li[data-task="signin"]')?.getAttribute('data-task')).toBe('signin');
    expect(document.querySelector('.platform-tasks__rows')!.children.length).toBe(4);
    expect(document.body.textContent).toContain(taskDisplayName('cat_travel'));
    // qoder 任务被过滤：其 cron（0 7 * * *）不出现在 workbuddy 卡
    expect(document.body.textContent).not.toContain('0 7 * * *');
  });

  it('qoder 卡只显示 qoder 任务，时点选择器绑定 qoder 冻结键', async () => {
    await renderReady('qoder');
    expect(document.querySelector('.platform-tasks__rows')!.children.length).toBe(1);
    expect((document.querySelector('input[name="schedule.signin_cron_qoder:7"]') as HTMLInputElement).checked).toBe(true);
    expect(document.querySelector('input[name="schedule.signin_cron_workbuddy:9"]')).toBeNull();
  });

  it('中文名与说明来自 i18n 映射；启用徽标 on/off 如实', async () => {
    await renderReady();
    const signin = document.querySelector('li[data-task="signin"]')!;
    expect(signin.textContent).toContain('自动签到');
    expect(signin.textContent).toContain('每日签到');
    expect(signin.querySelector('.platform-tasks__badge--on')!.textContent).toBe(TASKS.ENABLED_ON);
    const cat = document.querySelector('li[data-task="cat_travel"]')!;
    expect(cat.textContent).toContain('猫猫旅行');
    expect(cat.querySelector('.platform-tasks__badge--off')!.textContent).toBe(TASKS.ENABLED_OFF);
  });

  it('未知任务名 → 只读行（cron 原样 + 无键说明），不渲染时点选择器，不发明归属', async () => {
    installFetch({
      ...OK_ROUTES,
      '/api/tasks': { body: { tasks: [{ name: 'future_task', cron: '0 6 * * *', enabled: true, platform: 'workbuddy', last_run: null }] } },
    });
    render(<PlatformTasksCard platform="workbuddy" />);
    await screen.findByText('future_task');
    const row = document.querySelector('li[data-task="future_task"]')!;
    expect(row.textContent).toContain('future_task');
    expect(row.textContent).toContain('0 6 * * *');
    expect(row.textContent).toContain(TASKS.UNKNOWN_TASK);
    expect(row.querySelector('input[type="checkbox"]')).toBeNull();
  });

  it('空平台（trae）→ 平台空态文案；workbuddy 空任务 → 全局空态文案', async () => {
    installFetch(OK_ROUTES);
    render(<PlatformTasksCard platform="trae" />);
    expect(await screen.findByText(TASKS.PLATFORM_NONE)).toBeTruthy();
    cleanup();
    installFetch({ ...OK_ROUTES, '/api/tasks': { body: { tasks: [] } } });
    render(<PlatformTasksCard platform="workbuddy" />);
    expect(await screen.findByText(TASKS.EMPTY)).toBeTruthy();
  });
});

describe('平台任务卡 — 调度编辑与保存流（冻结配置键）', () => {
  it('时点选择器值来自冻结键：signin 9/21 勾选、cat_travel 8 点勾选', async () => {
    await renderReady();
    expect((document.querySelector('input[name="schedule.signin_cron_workbuddy:9"]') as HTMLInputElement).checked).toBe(true);
    expect((document.querySelector('input[name="schedule.signin_cron_workbuddy:21"]') as HTMLInputElement).checked).toBe(true);
    expect((document.querySelector('input[name="schedule.cat_travel_cron:8"]') as HTMLInputElement).checked).toBe(true);
  });

  it('勾选换算 cron 进草稿点亮 dirty；保存 PATCH 只发改动冻结键，成功按回读重建基线', async () => {
    const fn = await renderReady('workbuddy', { ...OK_ROUTES, 'POST /api/config': { body: SAVE_OK_WIRE } });
    // 初始同步态：保存/放弃置灰
    const save = () => screen.getByRole('button', { name: TASKS.SAVE }) as HTMLButtonElement;
    expect(save().disabled).toBe(true);
    expect(screen.getByText(noteOf(TASKS.SYNCED))).toBeTruthy();
    // 去掉 21 点、加 20 点 → signin_cron_workbuddy: 0 9,21 → 0 9,20
    fireEvent.click(document.querySelector('input[name="schedule.signin_cron_workbuddy:21"]') as HTMLInputElement);
    fireEvent.click(document.querySelector('input[name="schedule.signin_cron_workbuddy:20"]') as HTMLInputElement);
    expect(screen.getByText(noteOf(TASKS.DIRTY.replace('{count}', '1')))).toBeTruthy();
    expect(save().disabled).toBe(false);
    fireEvent.click(save());
    await screen.findByText(noteOf(TASKS.SAVE_OK));
    expect(postCallsOf(fn)).toEqual([
      [{ key: 'schedule.signin_cron_workbuddy', value: '0 9,20 * * *' }],
    ]);
    // 服务端回读为准：21 已不在基线，20 勾选；dirty 清空保存再次置灰
    expect((document.querySelector('input[name="schedule.signin_cron_workbuddy:21"]') as HTMLInputElement).checked).toBe(false);
    expect((document.querySelector('input[name="schedule.signin_cron_workbuddy:20"]') as HTMLInputElement).checked).toBe(true);
    expect(save().disabled).toBe(true);
  });

  it('多键同改 → 单次 PATCH 携带全部改动冻结键（task_cron / token_keepalive 同模式）', async () => {
    const fn = await renderReady('workbuddy', { ...OK_ROUTES, 'POST /api/config': { body: SAVE_OK_WIRE } });
    fireEvent.click(document.querySelector('input[name="schedule.task_cron_workbuddy:10"]') as HTMLInputElement);
    fireEvent.click(document.querySelector('input[name="schedule.task_cron_workbuddy:11"]') as HTMLInputElement);
    fireEvent.click(document.querySelector('input[name="schedule.token_keepalive_workbuddy:22"]') as HTMLInputElement);
    fireEvent.click(document.querySelector('input[name="schedule.token_keepalive_workbuddy:23"]') as HTMLInputElement);
    fireEvent.click(screen.getByRole('button', { name: TASKS.SAVE }));
    await screen.findByText(noteOf(TASKS.SAVE_OK));
    expect(postCallsOf(fn)).toEqual([
      [
        { key: 'schedule.task_cron_workbuddy', value: '0 10,11 * * *' },
        { key: 'schedule.token_keepalive_workbuddy', value: '0 22,23 * * *' },
      ],
    ]);
  });

  it('放弃改动 → 回基线（勾选还原、dirty 清空），不发生任何请求', async () => {
    const fn = await renderReady();
    fireEvent.click(document.querySelector('input[name="schedule.signin_cron_workbuddy:21"]') as HTMLInputElement);
    expect(screen.getByText(noteOf(TASKS.DIRTY.replace('{count}', '1')))).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: TASKS.DISCARD }));
    expect((document.querySelector('input[name="schedule.signin_cron_workbuddy:21"]') as HTMLInputElement).checked).toBe(true);
    expect(screen.getByText(noteOf(TASKS.SYNCED))).toBeTruthy();
    expect(fn.mock.calls.filter((c) => (c[1] as RequestInit | undefined)?.method === 'POST')).toHaveLength(0);
  });

  it('字段级拒绝 → 逐条红字（冻结键原样上屏），草稿保留可再保存', async () => {
    await renderReady('workbuddy', {
      ...OK_ROUTES,
      'POST /api/config': { body: { ok: false, field_errors: [{ key: 'schedule.signin_cron_workbuddy', message: '越界', code: 'range' }] } },
    });
    fireEvent.click(document.querySelector('input[name="schedule.signin_cron_workbuddy:21"]') as HTMLInputElement);
    fireEvent.click(screen.getByRole('button', { name: TASKS.SAVE }));
    await screen.findByText(TASKS.SAVE_REJECTED_TITLE);
    const alert = screen.getAllByRole('alert').find((a) => a.textContent?.includes(TASKS.SAVE_REJECTED_TITLE))!;
    expect(alert.textContent).toContain('schedule.signin_cron_workbuddy');
    expect(alert.textContent).toContain('越界');
    expect(screen.getByText(noteOf(TASKS.DIRTY.replace('{count}', '1')))).toBeTruthy();
  });

  it('401 / 404 / 网络错误 → 如实展示不假成功，dirty 保留', async () => {
    await renderReady('workbuddy', { ...OK_ROUTES, 'POST /api/config': { status: 401, body: { error: 'unauthorized' } } });
    fireEvent.click(document.querySelector('input[name="schedule.signin_cron_workbuddy:21"]') as HTMLInputElement);
    fireEvent.click(screen.getByRole('button', { name: TASKS.SAVE }));
    expect(await screen.findByText(TASKS.SAVE_FAILED.replace('{detail}', 'HTTP 401'))).toBeTruthy();
    expect(screen.getByText(noteOf(TASKS.DIRTY.replace('{count}', '1')))).toBeTruthy();

    installFetch({ ...OK_ROUTES, 'POST /api/config': { status: 404, body: { error: 'not found' } } });
    fireEvent.click(screen.getByRole('button', { name: TASKS.SAVE }));
    expect(await screen.findByText(TASKS.SAVE_FAILED.replace('{detail}', 'HTTP 404'))).toBeTruthy();

    installFetch({ ...OK_ROUTES, 'POST /api/config': { reject: true } });
    fireEvent.click(screen.getByRole('button', { name: TASKS.SAVE }));
    expect(await screen.findByText(TASKS.SAVE_FAILED.replace('{detail}', 'network error'))).toBeTruthy();
  });
});

describe('平台任务卡 — 只读边界与轮询', () => {
  it('只做调度编辑：无任何「立即运行/执行」按钮；按钮 = 刷新 + 保存 + 放弃', async () => {
    await renderReady();
    expect(document.body.textContent).not.toContain('立即运行');
    const buttons = screen.getAllByRole('button').map((b) => b.textContent);
    expect(buttons).toEqual([TASKS.REFRESH, TASKS.SAVE, TASKS.DISCARD]);
  });

  it('手动刷新重发两请求并重置草稿基线', async () => {
    const fn = await renderReady();
    fireEvent.click(document.querySelector('input[name="schedule.signin_cron_workbuddy:21"]') as HTMLInputElement);
    expect(screen.getByText(noteOf(TASKS.DIRTY.replace('{count}', '1')))).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: TASKS.REFRESH }));
    await screen.findByText(noteOf(TASKS.SYNCED));
    expect(fn.mock.calls.filter((c) => String(c[0]) === '/api/tasks')).toHaveLength(2);
    expect(fn.mock.calls.filter((c) => String(c[0]) === '/api/config')).toHaveLength(2);
  });

  it('60s 低频轮询：只重拉任务实况（+1 个 /api/tasks），不重拉配置、不冲掉未保存草稿；卸载零新增', async () => {
    vi.useFakeTimers();
    try {
      const fn = vi.fn(async (...args: unknown[]) => {
        const url = String(args[0]);
        const route: { body: unknown } | null =
          url === '/api/tasks' ? { body: TASKS_WIRE } : url === '/api/config' ? { body: CONFIG_WIRE } : null;
        if (route === null) throw new Error(`unexpected fetch: ${url}`);
        return { ok: true, status: 200, json: async () => route.body };
      });
      vi.stubGlobal('fetch', fn);
      const { unmount } = render(<PlatformTasksCard platform="workbuddy" />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1);
      });
      expect(screen.getByText(taskDisplayName('signin'))).toBeTruthy();
      const tasksCalls = () => fn.mock.calls.filter((c) => String(c[0]) === '/api/tasks').length;
      const configCalls = () => fn.mock.calls.filter((c) => String(c[0]) === '/api/config').length;
      expect([tasksCalls(), configCalls()]).toEqual([1, 1]);

      // 编辑进草稿（脏），轮询不得冲掉
      fireEvent.click(document.querySelector('input[name="schedule.signin_cron_workbuddy:21"]') as HTMLInputElement);
      expect(screen.getByText(noteOf(TASKS.DIRTY.replace('{count}', '1')))).toBeTruthy();

      await act(async () => {
        await vi.advanceTimersByTimeAsync(TASKS_POLL_MS);
      });
      expect([tasksCalls(), configCalls()]).toEqual([2, 1]); // 只轮任务实况
      expect(screen.getByText(noteOf(TASKS.DIRTY.replace('{count}', '1')))).toBeTruthy(); // 草稿仍在

      unmount();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(TASKS_POLL_MS * 3);
      });
      expect(tasksCalls()).toBe(2); // 卸载清理解绑，零新增
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('平台任务卡 — axe 基线', () => {
  it('就绪态无违规', async () => {
    installFetch(OK_ROUTES);
    const { container } = render(<PlatformTasksCard platform="workbuddy" />);
    await screen.findByText(taskDisplayName('signin'));
    expect(await axe(container)).toHaveNoViolations();
  });

  it('错误态无违规', async () => {
    installFetch({ '/api/tasks': { status: 500, body: { error: 'boom' } }, '/api/config': { body: CONFIG_WIRE } });
    const { container } = render(<PlatformTasksCard platform="workbuddy" />);
    await screen.findByText(TASKS.ERROR_TITLE);
    expect(await axe(container)).toHaveNoViolations();
  });
});
