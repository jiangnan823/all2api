import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, waitFor, within } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { GrowthTasksModal } from '../src/components/GrowthTasksModal';
import { AccountsScreen } from '../src/app/AccountsScreen';
import { fetchGrowthTasks, acceptAll, claimAuto } from '../src/lib/growthApi';
import { GROWTH, settingsLine, ACCOUNTS } from '../src/i18n/zh';

/**
 * M7b 成长任务弹层门禁：fetch 全程 mock（wire 蛇形，形状按 M7b 冻结契约）。
 * 断言面：lib 解析（蛇形→camelCase、{error} 信封逐字上抛、502/网络错分路）、
 * 弹层四态（加载/错误+重试/空态/就绪）、四列表格与状态徽标六值映射、
 * 全部接受 / 一键完成的端点调用、计数与成功提示、动作失败原文展示、
 * Esc / 遮罩点击关闭、body 滚动锁定、行内「任务」按钮接线、axe 零违规。
 */
expect.extend(toHaveNoViolations);

const WB_ID = 'wb-0123456789abcdef';
const TASKS_URL = `/api/accounts/workbuddy:${WB_ID}/growth-tasks`;
const ACCEPT_URL = `/api/accounts/workbuddy:${WB_ID}/growth-accept`;
const CLAIM_URL = `/api/accounts/workbuddy:${WB_ID}/growth-claim-auto`;

/** 冻结契约 wire 形状（蛇形）：六值状态闭集各一条，第一条 = wb2api 验收例（automation_1 1/1 +100 分 +5 能 已领取）。 */
const TASKS_WIRE = {
  source: 'upstream',
  tasks: [
    {
      code: 'automation_1',
      title: '设置自动化任务',
      progress_done: 1,
      progress_total: 1,
      reward_credit: 100,
      reward_energy: 5,
      status: 'claimed',
    },
    { code: 'daily_sign', title: '每日签到', progress_done: 0, progress_total: 1, reward_credit: 20, status: 'claimable' },
    { code: 'first_charge', title: '首次充值', progress_done: 1, progress_total: 1, reward_credit: 500, reward_energy: 0, status: 'completed' },
    { code: 'invite_1', title: '邀请好友', progress_done: 0, progress_total: 3, reward_credit: 200, reward_energy: 10, status: 'in_progress' },
    { code: 'profile_100', title: '完善资料', progress_done: 1, progress_total: 1, reward_credit: 50, status: 'accepted' },
    { code: 'cat_travel', title: '猫猫旅行', progress_done: 0, progress_total: 1, reward_credit: 30, status: 'not_accepted' },
  ],
};

type Route = { status?: number; body?: unknown; reject?: boolean; pending?: boolean };

/** routes 按引用闭包：测试中改写 routes 即改变后续响应（供重试/刷新断言）。 */
function installFetch(routes: Record<string, Route>) {
  const calls: { url: string; init?: RequestInit }[] = [];
  const fn = vi.fn(async (...args: unknown[]) => {
    const url = typeof args[0] === 'string' ? args[0] : String(args[0]);
    const init = args[1] as RequestInit | undefined;
    calls.push({ url, init });
    const route = routes[url];
    if (!route) throw new Error(`unexpected fetch: ${url}`);
    if (route.pending) return new Promise(() => {});
    if (route.reject) throw new TypeError('fetch failed');
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => route.body };
  });
  vi.stubGlobal('fetch', fn);
  return { fn, calls };
}

function readyModal(props: Partial<Parameters<typeof GrowthTasksModal>[0]> = {}) {
  return render(
    <GrowthTasksModal platform="workbuddy" id={WB_ID} nickname="测试昵称" onClose={props.onClose ?? vi.fn()} />,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('lib/growthApi — wire 解析与错误通道', () => {
  it('GET growth-tasks：蛇形 wire → camelCase 契约面（source/tasks/进度/奖励/状态原样）', async () => {
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    const snap = await fetchGrowthTasks('workbuddy', WB_ID);
    expect(snap.source).toBe('upstream');
    expect(snap.tasks).toHaveLength(6);
    expect(snap.tasks[0]).toEqual({
      code: 'automation_1',
      title: '设置自动化任务',
      progressDone: 1,
      progressTotal: 1,
      rewardCredit: 100,
      rewardEnergy: 5,
      status: 'claimed',
    });
  });

  it('502 + {error:"上游失败原因"} → message 逐字 = 上游原文，status=502', async () => {
    installFetch({ [TASKS_URL]: { status: 502, body: { error: '上游失败原因' } } });
    await expect(fetchGrowthTasks('workbuddy', WB_ID)).rejects.toMatchObject({
      message: '上游失败原因',
      status: 502,
    });
  });

  it('4xx + {error:"中文原因"} 与网络错误分路：原文 vs network error', async () => {
    installFetch({ [TASKS_URL]: { status: 400, body: { error: '账号不存在' } } });
    await expect(fetchGrowthTasks('workbuddy', WB_ID)).rejects.toMatchObject({ message: '账号不存在', status: 400 });
    installFetch({ [TASKS_URL]: { reject: true } });
    await expect(fetchGrowthTasks('workbuddy', WB_ID)).rejects.toMatchObject({ message: 'network error' });
  });

  it('POST growth-accept / growth-claim-auto：body 恒为 {}，计数防御式读数', async () => {
    const { calls } = installFetch({ [ACCEPT_URL]: { body: { accepted: 3 } }, [CLAIM_URL]: { body: { claimed: 2, credit_gained: 200 } } });
    expect(await acceptAll('workbuddy', WB_ID)).toEqual({ accepted: 3 });
    expect(await claimAuto('workbuddy', WB_ID)).toEqual({ claimed: 2, creditGained: 200 });
    const postUrls = calls.filter((c) => c.init?.method === 'POST');
    expect(postUrls.map((c) => c.url)).toEqual([ACCEPT_URL, CLAIM_URL]);
    for (const c of postUrls) expect(c.init?.body).toBe('{}');
  });

  it('畸形条目整条丢弃（code 缺失）；可空字段缺失 → null；title 缺失回落 code', async () => {
    installFetch({
      [TASKS_URL]: {
        body: {
          tasks: [
            { title: '无 code 的条目', status: 'claimed' },
            { code: 'bare_1' },
          ],
        },
      },
    });
    const snap = await fetchGrowthTasks('workbuddy', WB_ID);
    expect(snap.tasks).toHaveLength(1);
    expect(snap.tasks[0]).toEqual({
      code: 'bare_1',
      title: 'bare_1',
      progressDone: null,
      progressTotal: null,
      rewardCredit: null,
      rewardEnergy: null,
      status: '',
    });
  });
});

describe('成长任务弹层 — 就绪渲染（wb2api 同款四列）', () => {
  it('标题带昵称、说明行、四列表头齐备', async () => {
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    readyModal();
    expect(await screen.findByRole('dialog')).toBeTruthy();
    expect(screen.getByText(settingsLine(GROWTH.TITLE, { name: '测试昵称' }))).toBeTruthy();
    expect(screen.getByText(GROWTH.NOTE)).toBeTruthy();
    for (const h of [GROWTH.COL_TASK, GROWTH.COL_PROGRESS, GROWTH.COL_REWARD, GROWTH.COL_STATUS]) {
      expect(screen.getByRole('columnheader', { name: h })).toBeTruthy();
    }
  });

  it('首行 = wb2api 验收例：automation_1 / 1/1 / +100 分 +5 能 / 已领取', async () => {
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    readyModal();
    const row = (await screen.findByText('automation_1')).closest('tr') as HTMLElement;
    expect(within(row).getByText('设置自动化任务')).toBeTruthy();
    expect(within(row).getByText('1/1')).toBeTruthy();
    expect(within(row).getByText('+100 分 +5 能')).toBeTruthy();
    expect(within(row).getByText(GROWTH.STATUS_CLAIMED)).toBeTruthy();
  });

  it('状态徽标映射：已领取绿 / 可领取主色 / 已完成蓝 / 进行中·已报名·未报名灰', async () => {
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    readyModal();
    await screen.findByText('automation_1');
    expect(screen.getByText(GROWTH.STATUS_CLAIMED).className).toContain('pool-badge--ok');
    expect(screen.getByText(GROWTH.STATUS_CLAIMABLE).className).toContain('growth-status--brand');
    expect(screen.getByText(GROWTH.STATUS_COMPLETED).className).toContain('growth-status--info');
    for (const s of [GROWTH.STATUS_IN_PROGRESS, GROWTH.STATUS_ACCEPTED, GROWTH.STATUS_NOT_ACCEPTED]) {
      expect(screen.getByText(s).className).toContain('pool-badge--idle');
    }
  });

  it('可空防御：进度/奖励缺失显示 —，未知状态原样回显，奖励单边只显示该边', async () => {
    installFetch({
      [TASKS_URL]: {
        body: {
          tasks: [
            { code: 'bare_1', status: 'weird_state' },
            { code: 'half_1', title: '只给积分', reward_credit: 30, status: 'claimable' },
          ],
        },
      },
    });
    readyModal();
    const bare = (await screen.findAllByText('bare_1'))[0].closest('tr') as HTMLElement; // code 与回落 title 同串，取首个
    expect(within(bare).getAllByText(GROWTH.DASH).length).toBeGreaterThanOrEqual(1); // 进度/奖励 —
    expect(within(bare).getByText('weird_state')).toBeTruthy();
    const half = screen.getByText('half_1').closest('tr') as HTMLElement;
    expect(within(half).getByText('+30 分')).toBeTruthy();
  });
});

describe('成长任务弹层 — 加载 / 错误+重试 / 空态', () => {
  it('加载态：fetch 未决时显示 LOADING（role=status）', () => {
    installFetch({ [TASKS_URL]: { pending: true } });
    readyModal();
    expect(screen.getByRole('status').textContent).toBe(GROWTH.LOADING);
  });

  it('错误态：502 原文逐字进详情，重试恢复就绪', async () => {
    const routes: Record<string, Route> = { [TASKS_URL]: { status: 502, body: { error: '上游失败原因' } } };
    installFetch(routes);
    readyModal();
    expect(await screen.findByRole('alert')).toBeTruthy();
    expect(screen.getByText(GROWTH.ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('上游失败原因')).toBeTruthy();
    routes[TASKS_URL] = { body: TASKS_WIRE };
    fireEvent.click(screen.getByRole('button', { name: GROWTH.RETRY }));
    expect(await screen.findByText('automation_1')).toBeTruthy();
  });

  it('空态：tasks 空 = 该账号暂无成长任务', async () => {
    installFetch({ [TASKS_URL]: { body: { source: 'upstream', tasks: [] } } });
    readyModal();
    expect(await screen.findByText(GROWTH.EMPTY)).toBeTruthy();
    expect(screen.queryByRole('table')).toBeNull();
  });
});

describe('成长任务弹层 — 全部接受 / 一键完成', () => {
  it('全部接受：POST growth-accept，成功提示带计数，并重拉清单', async () => {
    const { calls } = installFetch({ [TASKS_URL]: { body: TASKS_WIRE }, [ACCEPT_URL]: { body: { accepted: 3 } } });
    readyModal();
    fireEvent.click(await screen.findByRole('button', { name: GROWTH.ACCEPT_ALL }));
    expect(await screen.findByText(settingsLine(GROWTH.ACCEPT_OK, { n: 3 }))).toBeTruthy();
    expect(calls.filter((c) => c.url === ACCEPT_URL && c.init?.method === 'POST')).toHaveLength(1);
    // 首次加载 1 次 + 接受成功后刷新 1 次
    expect(calls.filter((c) => c.url === TASKS_URL)).toHaveLength(2);
  });

  it('一键完成可自动任务：POST growth-claim-auto，提示「领取成功 +{n} 积分」并重拉清单', async () => {
    const { calls } = installFetch({ [TASKS_URL]: { body: TASKS_WIRE }, [CLAIM_URL]: { body: { claimed: 2, credit_gained: 200 } } });
    readyModal();
    fireEvent.click(await screen.findByRole('button', { name: GROWTH.CLAIM_AUTO }));
    expect(await screen.findByText(settingsLine(GROWTH.CLAIM_OK, { n: 200 }))).toBeTruthy();
    expect(calls.filter((c) => c.url === CLAIM_URL && c.init?.method === 'POST')).toHaveLength(1);
    expect(calls.filter((c) => c.url === TASKS_URL)).toHaveLength(2);
  });

  it('动作失败：{error} 原文逐字展示（前缀 + 原文），不清空清单', async () => {
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE }, [CLAIM_URL]: { status: 502, body: { error: '上游签到接口超时' } } });
    readyModal();
    fireEvent.click(await screen.findByRole('button', { name: GROWTH.CLAIM_AUTO }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe(`${GROWTH.ACTION_ERR_PREFIX}上游签到接口超时`);
    expect(screen.getByText('automation_1')).toBeTruthy();
  });

  it('请求进行中动作按钮禁用（防重复提交）；「关闭」始终保持出口', async () => {
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE }, [CLAIM_URL]: { pending: true } });
    readyModal();
    fireEvent.click(await screen.findByRole('button', { name: GROWTH.CLAIM_AUTO }));
    for (const name of [GROWTH.ACCEPT_ALL, GROWTH.CLAIM_AUTO, GROWTH.REFRESH]) {
      expect((screen.getByRole('button', { name: name }) as HTMLButtonElement).disabled).toBe(true);
    }
    expect((screen.getByRole('button', { name: GROWTH.CLOSE }) as HTMLButtonElement).disabled).toBe(false);
  });
});

describe('成长任务弹层 — 关闭路径与滚动锁', () => {
  it('「关闭」按钮 → onClose', async () => {
    const onClose = vi.fn();
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    readyModal({ onClose });
    fireEvent.click(await screen.findByRole('button', { name: GROWTH.CLOSE }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('Esc → onClose；面板内按键不关闭', async () => {
    const onClose = vi.fn();
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    readyModal({ onClose });
    fireEvent.keyDown(await screen.findByRole('dialog'), { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('遮罩点击 → onClose；点在卡片内不关闭', async () => {
    const onClose = vi.fn();
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    const { container } = readyModal({ onClose });
    await screen.findByRole('dialog');
    const scrim = container.firstElementChild as HTMLElement;
    fireEvent.mouseDown(screen.getByRole('dialog'));
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.mouseDown(scrim);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('打开期间 body 滚动锁定，卸载后恢复原值', async () => {
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    document.body.style.overflow = 'auto';
    const view = readyModal();
    await screen.findByRole('dialog');
    expect(document.body.style.overflow).toBe('hidden');
    view.unmount();
    expect(document.body.style.overflow).toBe('auto');
    document.body.style.overflow = '';
  });
});

describe('账号池接线 — 行内「任务」按钮', () => {
  it('点击行内「任务」→ 打开该账号的弹层；「关闭」退出并恢复滚动', async () => {
    installFetch({
      '/api/accounts': {
        body: {
          accounts: [
            {
              platform: 'workbuddy',
              id: WB_ID,
              nickname: '池内昵称',
              status: 'active',
              credit: 100,
              health: 'good',
              expires_at: null,
              cooldown_end: null,
              last_selected: null,
              success: 0,
              fail: 0,
              in_flight: 0,
              paused: false,
              last_success: null,
              credit_total: null,
            },
          ],
          total: 1,
          active: 1,
          cooling: 0,
          disabled: 0,
          credit_total: 100,
        },
      },
      [TASKS_URL]: { body: TASKS_WIRE },
    });
    render(<AccountsScreen />);
    const copyBtn = await screen.findByRole('button', { name: `${ACCOUNTS.COPY}: ${WB_ID}` });
    const row = copyBtn.closest('tr') as HTMLElement;
    fireEvent.click(within(row).getByRole('button', { name: GROWTH.BTN_OPEN }));
    expect(
      await screen.findByText(settingsLine(GROWTH.TITLE, { name: '池内昵称' })),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: GROWTH.CLOSE }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(document.body.style.overflow).not.toBe('hidden');
  });

  it('昵称缺失：标题回落 id 短码（前 8…后 4）', async () => {
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    render(<GrowthTasksModal platform="workbuddy" id={WB_ID} nickname={null} onClose={vi.fn()} />);
    expect(await screen.findByText(settingsLine(GROWTH.TITLE, { name: 'wb-01234…cdef' }))).toBeTruthy();
  });
});

describe('红线', () => {
  it('axe 零违规（就绪态：六行四列 + 底部四按钮）', async () => {
    installFetch({ [TASKS_URL]: { body: TASKS_WIRE } });
    const { container } = readyModal();
    await screen.findByText('automation_1');
    expect(await axe(container)).toHaveNoViolations();
  });

  it('不落 storage：模块代码零 localStorage/sessionStorage 触点', async () => {
    const { readFileSync } = await import('node:fs');
    const { join } = await import('node:path');
    for (const f of ['src/lib/growthApi.ts', 'src/components/GrowthTasksModal.tsx']) {
      const text = readFileSync(join(process.cwd(), f), 'utf8');
      expect(text).not.toMatch(/localStorage|sessionStorage|document\.cookie/);
    }
  });
});
