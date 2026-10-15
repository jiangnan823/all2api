import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { SettingsScreen } from '../src/app/SettingsScreen';
import { setApiKeyProvider } from '../src/lib/panelApi';
import { setDeliveredThrottleFields } from '../src/lib/throttle';
import { ADVANCED, DANGER, SETTINGS, THROTTLE, settingsLine, sourceDisplayName } from '../src/i18n/zh';

/**
 * 设置屏（M2 B2）门禁：真实功能屏 = 读 /api/config + /api/config/fields（wire 全蛇形）+
 * 元数据驱动表单 + dirty 草稿（后端只读、保存未开放的如实边界）。
 * fetch 全程 mock（沿用 shell.test 的 stubGlobal 模式）；wire fixture 逐字段对齐后端
 * internal/api（api.go panelConfigResponse / configfields.go handleConfigFields），不自造形状。
 */
expect.extend(toHaveNoViolations);

// ---------- wire fixtures（蛇形，与后端实际返回一致） ----------

function budgetWire(
  steady: number, burst: number, gap: [number, number], daily: number, inflight: number,
  extra: { jitter?: number; halve?: boolean } = {},
) {
  return {
    steady_rpm: steady,
    burst_rpm: burst,
    min_gap_ms: gap,
    daily_cap: daily,
    inflight,
    backoff: { base_429_sec: 600, not_found_sec: 300, exhaust_402_until: 'next-04:00' },
    jitter_minutes: extra.jitter ?? 15,
    halve_budget_after_429: extra.halve ?? false,
  };
}

const CONFIG_WIRE = {
  raw: {
    listen: '127.0.0.1:8317',
    external_base_url: '',
    pool: { credit_floor: 100, max_retries: 3, cooldown_sec: 60 },
    router: { model_map: {}, responses_api: { enabled: false } },
    schedule: { signin_cron: '0 9,21 * * *', task_cron: '0 10 * * *', token_keepalive_cron: '0 22 * * *' },
    sources: {
      qoder: { enabled: true },
      trae: { enabled: true },
      workbuddy: { enabled: true },
    },
  },
  feature_flags: { responses_api: false, session_stickiness: false, redis_mirror: false, debug_body_sampling: false },
  throttle: {
    qoder: budgetWire(4, 12, [200, 1200], 300, 1, { halve: true }),
    trae: budgetWire(2, 6, [500, 2000], 100, 1),
    workbuddy: budgetWire(6, 20, [100, 900], 600, 2),
  },
  advanced: {
    qoder: { request_fingerprint_rewrite: false },
    trae: { request_fingerprint_rewrite: false },
    workbuddy: { request_fingerprint_rewrite: false },
  },
  notice_version: '1',
};

function fnum(key: string, label: string, unit: string, min: number, max: number) {
  return { key, label, type: 'number', unit, min, max, effect: 'next_request' };
}

function throttleGroup(source: string, opts: { jitter: boolean; halve: boolean }) {
  const p = `sources.${source}.throttle.`;
  const fields: unknown[] = [
    fnum(`${p}steady_rpm`, 'config.field.steady_rpm.label', 'rpm', 1, 120),
    fnum(`${p}burst_rpm`, 'config.field.burst_rpm.label', 'rpm', 1, 300),
    fnum(`${p}min_gap_ms`, 'config.field.min_gap_ms.label', 'ms', 0, 60000),
    fnum(`${p}daily_cap`, 'config.field.daily_cap.label', 'count', 1, 10000),
    fnum(`${p}inflight`, 'config.field.inflight.label', 'count', 1, 32),
    fnum(`${p}backoff.base_429_sec`, 'config.field.base_429_sec.label', 'sec', 600, 86400),
    fnum(`${p}backoff.not_found_sec`, 'config.field.not_found_sec.label', 'sec', 0, 3600),
  ];
  if (opts.jitter) fields.push(fnum(`${p}jitter_minutes`, 'config.field.jitter_minutes.label', 'min', 0, 120));
  if (opts.halve) {
    fields.push({ key: `${p}halve_budget_after_429`, label: 'config.field.halve_budget_after_429.label', type: 'boolean', effect: 'next_request' });
  }
  return { group: `sources_throttle.${source}`, fields };
}

function dangerGroup(source: string) {
  return {
    group: `danger.${source}`,
    fields: [
      {
        key: `sources.${source}.advanced.request_fingerprint_rewrite`,
        label: 'config.field.fingerprint_rewrite.label',
        type: 'boolean',
        description: 'config.field.fingerprint_rewrite.desc',
        effect: 'next_request',
        requires_confirm: true,
      },
      { key: `sources.${source}.enabled`, label: 'config.field.source_enabled.label', type: 'boolean', effect: 'next_request' },
    ],
  };
}

/** 通用组（gateway/routing/pooling/schedule）：M2 第二批起按交付元数据渲染，值取 raw 透传。 */
const FIELDS_WIRE = [
  {
    group: 'gateway',
    fields: [
      { key: 'listen', label: 'config.field.listen.label', type: 'string', effect: 'restart_required' },
      { key: 'external_base_url', label: 'config.field.external_base_url.label', type: 'string', description: 'config.field.external_base_url.desc', effect: 'next_request' },
    ],
  },
  { group: 'routing', fields: [{ key: 'router.responses_api.enabled', label: 'config.field.responses_api.label', type: 'boolean', effect: 'next_request' }] },
  {
    group: 'pooling',
    fields: [
      fnum('pool.credit_floor', 'config.field.credit_floor.label', 'credit', 0, 1000000),
      fnum('pool.max_retries', 'config.field.max_retries.label', 'count', 0, 10),
    ],
  },
  {
    group: 'schedule',
    fields: [
      { key: 'schedule.signin_cron', label: 'config.field.signin_cron.label', type: 'cron', effect: 'next_request' },
      { key: 'schedule.task_cron', label: 'config.field.task_cron.label', type: 'cron', effect: 'next_request' },
    ],
  },
  throttleGroup('qoder', { jitter: true, halve: true }),
  dangerGroup('qoder'),
  throttleGroup('trae', { jitter: false, halve: false }),
  dangerGroup('trae'),
  throttleGroup('workbuddy', { jitter: true, halve: false }),
  dangerGroup('workbuddy'),
];

/** 保存流成功回包：applied = 服务端回读（steady_rpm 已是 5），ok=true。 */
const SAVE_OK_WIRE = {
  ok: true,
  applied: {
    ...CONFIG_WIRE,
    throttle: {
      ...CONFIG_WIRE.throttle,
      qoder: { ...CONFIG_WIRE.throttle.qoder, steady_rpm: 5 },
    },
  },
};

/** POST /api/config 的调用项（url, init）；测试里找保存请求用。 */
type PostCall = [string, RequestInit];

function postCallsOf(fn: ReturnType<typeof installFetch>): PostCall[] {
  return fn.mock.calls
    .filter((c) => String(c[0]) === '/api/config' && (c[1] as RequestInit | undefined)?.method === 'POST')
    .map((c) => [String(c[0]), c[1] as RequestInit]);
}

const TIME_WIRE = { now: 1760420400, local: '2026-10-14 12:00:00', timezone: 'UTC' };

// ---------- fetch mock ----------

type Route = { status?: number; body?: unknown; reject?: boolean };
type Routes = Record<string, Route>;

const OK_ROUTES: Routes = {
  '/api/config': { body: CONFIG_WIRE },
  '/api/config/fields': { body: FIELDS_WIRE },
  '/api/time': { body: TIME_WIRE },
};

function installFetch(routes: Routes) {
  // 占位 ...args：vi.fn 的调用签名需与 fetch 一致（url, init），否则 mock.calls 元组退化为 []
  const fn = vi.fn(async (...args: unknown[]) => {
    const url = typeof args[0] === 'string' ? args[0] : String(args[0]);
    const method = (args[1] as RequestInit | undefined)?.method ?? 'GET';
    // GET 与 POST 共用 '/api/config' 端点：'POST /api/config' 键分流 POST，其余按 GET 落 '/api/config'
    const route = (method !== 'GET' ? routes[`POST ${url}`] : undefined) ?? routes[url]
      ?? { status: 404, body: { error: 'not found' } };
    if (route.reject) throw new TypeError('fetch failed');
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => route.body };
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

async function renderReady(routes: Routes = OK_ROUTES): Promise<ReturnType<typeof installFetch> | undefined> {
  const fn = installFetch(routes);
  render(<SettingsScreen />);
  await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('qoder')}`);
  return fn;
}

const steadyInput = () => screen.getByLabelText('稳态请求') as HTMLInputElement;
const throttleSave = () => screen.getByRole('button', { name: THROTTLE.SAVE }) as HTMLButtonElement;

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  setDeliveredThrottleFields(null); // 交付注册表是模块态，测试间必须清空（lib/throttle.ts 数据源缝）
  setApiKeyProvider(() => '');      // X-API-Key provider 同为模块态，清空回开放模式
});

describe('设置屏 — 加载 / 错误 / 重试', () => {
  it('挂载先出加载态，config + fields 两请求都发出后渲染默认源（字典序首个）', async () => {
    const fn = installFetch(OK_ROUTES);
    render(<SettingsScreen />);
    expect(screen.getByRole('status').textContent).toBe(SETTINGS.LOADING);
    await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('qoder')}`);
    const urls = fn.mock.calls.map((c) => String(c[0]));
    expect(urls).toContain('/api/config');
    expect(urls).toContain('/api/config/fields');
    expect((screen.getByRole('radio', { name: 'Qoder' }) as HTMLInputElement).checked).toBe(true);
  });

  it('config 500 → 错误态带诊断信息；重试成功后恢复', async () => {
    installFetch({ ...OK_ROUTES, '/api/config': { status: 500, body: { error: 'boom' } } });
    render(<SettingsScreen />);
    expect(await screen.findByText(SETTINGS.ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('HTTP 500')).toBeTruthy();
    installFetch(OK_ROUTES); // apiGet 每次调用都取全局 fetch，桩可直接换
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.RETRY }));
    await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('qoder')}`);
  });

  it('/api/time 失败只降级角注，不影响配置展示', async () => {
    await renderReady({ ...OK_ROUTES, '/api/time': { reject: true } });
    expect(screen.getByText(SETTINGS.TIME_UNAVAILABLE)).toBeTruthy();
    expect(steadyInput().value).toBe('4');
  });

  it('/api/time 成功 → 服务器时间角注带本地串、时区与本机偏差', async () => {
    await renderReady();
    const meta = document.querySelector('p.settings-screen__meta');
    expect(meta?.textContent).toContain(TIME_WIRE.local);
    expect(meta?.textContent).toContain(TIME_WIRE.timezone);
    expect(meta?.textContent).toMatch(/相差约 \d+ 秒/);
  });
});

describe('设置屏 — 元数据驱动（/api/config/fields 是字段权威）', () => {
  it('label/单位/范围来自后端交付：steady 1–120、退避 600–86400（非前端旧表 60/3600）', async () => {
    await renderReady();
    const steadyRow = document.querySelector('tr[data-path="sources.qoder.throttle.steady_rpm"]');
    expect(steadyRow?.textContent).toContain('稳态请求');
    expect(steadyRow?.textContent).toContain('1–120');
    expect(steadyRow?.textContent).toContain(THROTTLE.UNIT_RPM);
    const backoffRow = document.querySelector('tr[data-path="sources.qoder.throttle.backoff.base_429_sec"]');
    expect(backoffRow?.textContent).toContain('600–86400');
    expect(backoffRow?.textContent).toContain(THROTTLE.UNIT_SEC);
    const dailyRow = document.querySelector('tr[data-path="sources.qoder.throttle.daily_cap"]');
    expect(dailyRow?.textContent).toContain(THROTTLE.UNIT_DAILY);
    const inflightRow = document.querySelector('tr[data-path="sources.qoder.throttle.inflight"]');
    expect(inflightRow?.textContent).toContain(THROTTLE.UNIT_INFLIGHT);
  });

  it('通用组按交付元数据渲染：值取 raw 透传（嵌套 path + 顶层键），原始文案 key 不外漏', async () => {
    await renderReady();
    const text = document.body.textContent ?? '';
    // 原始文案 key 一律经 zh.ts 换算，不外漏
    expect(text).not.toContain('config.field.listen.label');
    expect(text).not.toContain('config.field.credit_floor.label');
    // 四组标题在文档中逐一可见
    for (const t of ['网关', '路由', '池化', '定时任务']) {
      expect(text).toContain(t);
    }
    // 值来自 raw：顶层字符串 listen、嵌套数值 pool.credit_floor、布尔 router.responses_api.enabled、cron 串
    expect((document.querySelector('input[name="listen"]') as HTMLInputElement).value).toBe('127.0.0.1:8317');
    expect((document.querySelector('input[name="pool.credit_floor"]') as HTMLInputElement).value).toBe('100');
    expect((document.querySelector('input[name="pool.max_retries"]') as HTMLInputElement).value).toBe('3');
    expect((document.querySelector('input[name="router.responses_api.enabled"]') as HTMLInputElement).checked).toBe(false);
    // v1.2：cron 字段渲染为 24 小时制时点选择器（9、21 勾选），原始 cron 串不再以文本框暴露
    const signinChecks = [...document.querySelectorAll<HTMLInputElement>('input[name="schedule.signin_cron:9"], input[name="schedule.signin_cron:21"]')];
    expect(signinChecks.every((c) => c.checked)).toBe(true);
    expect(signinChecks).toHaveLength(2);
    expect(document.querySelector('input[name="schedule.signin_cron"]')).toBeNull();
    // effect=restart_required 的生效标注
    expect(text).toContain(SETTINGS.GENERIC_EFFECT_RESTART);
  });

  it('通用组进 dirty 跟踪：改字段点亮 dirty 汇总（含组内装饰点），与按源改动合并计数', async () => {
    await renderReady();
    const listen = document.querySelector('input[name="listen"]') as HTMLInputElement;
    fireEvent.change(listen, { target: { value: '0.0.0.0:7800' } });
    expect(screen.getByText(settingsLine(SETTINGS.DIRTY_SUMMARY, { count: 1 }))).toBeTruthy();
    expect(document.querySelector('fieldset[data-group="gateway"] .settings-screen__dirty-dot')).not.toBeNull();
    // 再改一个按源字段 → 合并计数 2
    fireEvent.change(steadyInput(), { target: { value: '5' } });
    fireEvent.click(throttleSave());
    expect(screen.getByText(settingsLine(SETTINGS.DIRTY_SUMMARY, { count: 2 }))).toBeTruthy();
    // 放弃本地改动同时复位通用组
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.DISCARD }));
    expect(screen.getByText(SETTINGS.SYNCED_NOTE)).toBeTruthy();
    expect((document.querySelector('input[name="listen"]') as HTMLInputElement).value).toBe('127.0.0.1:8317');
  });

  it('通用组保存流：PATCH 按冻结 path 携带改动值（number 发数值、boolean 发布尔、cron 经时点选择换算）', async () => {
    const fn = await renderReady({
      ...OK_ROUTES,
      'POST /api/config': { body: SAVE_OK_WIRE },
    });
    fireEvent.change(document.querySelector('input[name="pool.credit_floor"]') as HTMLInputElement, {
      target: { value: '150' },
    });
    fireEvent.click(document.querySelector('input[name="router.responses_api.enabled"]') as HTMLInputElement);
    // v1.2：时点选择器操作——先去掉 21 点，再加 20 点 → cron 从 0 9,21 变为 0 9,20
    fireEvent.click(document.querySelector('input[name="schedule.signin_cron:21"]') as HTMLInputElement);
    fireEvent.click(document.querySelector('input[name="schedule.signin_cron:20"]') as HTMLInputElement);
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.SAVE }));
    await screen.findByText(SETTINGS.SAVE_OK_NOTE);
    const posts = postCallsOf(fn!);
    // PATCH 顺序跟随组交付序（routing 组先于 pooling），逐项断言冻结 path 与原生类型
    expect(JSON.parse(posts[0][1].body as string)).toEqual([
      { key: 'router.responses_api.enabled', value: true },
      { key: 'pool.credit_floor', value: 150 },
      { key: 'schedule.signin_cron', value: '0 9,20 * * *' },
    ]);
  });

  it('交付边界参与校验：steady=100/burst=150（≤后端上限 120/300，旧前端表会拒）进草稿并点亮 dirty', async () => {
    await renderReady();
    fireEvent.change(steadyInput(), { target: { value: '100' } });
    fireEvent.change(screen.getByLabelText('突发上限'), { target: { value: '150' } }); // 150 ≥ 100，不触发突发<稳态规则
    fireEvent.click(throttleSave());
    expect(screen.queryAllByRole('alert')).toHaveLength(0);
    expect(document.querySelector('.settings-screen__dirty-dot')).not.toBeNull();
    expect(screen.getByText(settingsLine(SETTINGS.DIRTY_SUMMARY, { count: 1 }))).toBeTruthy();
  });

  it('显示条件由交付集决定：Qoder 有减半行，WorkBuddy 有抖动无减半，Trae 两者皆无', async () => {
    await renderReady();
    expect(document.body.textContent).toContain(THROTTLE.HALVE);
    fireEvent.click(screen.getByRole('radio', { name: 'Trae' }));
    await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('trae')}`);
    expect(steadyInput().value).toBe('2');
    let text = document.body.textContent ?? '';
    expect(text).not.toContain('签到抖动');
    expect(text).not.toContain(THROTTLE.HALVE);
    fireEvent.click(screen.getByRole('radio', { name: 'WorkBuddy' }));
    await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('workbuddy')}`);
    text = document.body.textContent ?? '';
    expect(text).toContain('签到抖动');
    expect(text).not.toContain(THROTTLE.HALVE);
  });
});

describe('设置屏 — dirty 跟踪与保存流', () => {
  it('初始同步态：保存/放弃置灰 + 基线文案；改动后激活，放弃回到服务端基线', async () => {
    await renderReady();
    const save = screen.getByRole('button', { name: SETTINGS.SAVE }) as HTMLButtonElement;
    const discard = screen.getByRole('button', { name: SETTINGS.DISCARD }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(screen.getByText(SETTINGS.SYNCED_NOTE)).toBeTruthy();
    expect(discard.disabled).toBe(true);
    fireEvent.change(steadyInput(), { target: { value: '5' } });
    fireEvent.click(throttleSave());
    expect(discard.disabled).toBe(false);
    fireEvent.click(discard);
    expect(screen.getByText(SETTINGS.SYNCED_NOTE)).toBeTruthy();
    expect(document.querySelector('.settings-screen__dirty-dot')).toBeNull();
    // 放弃后表单内部草稿也一并重挂：输入框回到服务端值
    expect(steadyInput().value).toBe('4');
  });

  it('保存流主路径：PATCH 只发改动字段（含 enabled 布尔），成功按服务端回读重建基线', async () => {
    // POST /api/config（保存）与 GET /api/config（读取）在路由表按方法分流
    const fn = await renderReady({
      ...OK_ROUTES,
      'POST /api/config': { body: SAVE_OK_WIRE },
    });
    fireEvent.change(steadyInput(), { target: { value: '5' } });
    fireEvent.click(throttleSave());
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.SAVE }));
    await screen.findByText(SETTINGS.SAVE_OK_NOTE);
    const posts = postCallsOf(fn!);
    expect(posts).toHaveLength(1);
    expect(JSON.parse(posts[0][1].body as string)).toEqual([
      { key: 'sources.qoder.throttle.steady_rpm', value: 5 },
    ]);
    // 服务端回读为准：基线更新为 5，dirty 清空
    expect(document.querySelector('.settings-screen__dirty-dot')).toBeNull();
    expect(steadyInput().value).toBe('5');
  });

  it('保存流按需携带 enabled 布尔键：停源改动进同一 PATCH（path 冻结）', async () => {
    const fn = await renderReady({
      ...OK_ROUTES,
      'POST /api/config': { body: SAVE_OK_WIRE },
    });
    const cb = screen.getByRole('checkbox', { name: new RegExp(SETTINGS.SOURCE_ENABLED_LABEL) }) as HTMLInputElement;
    fireEvent.click(cb);
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.SAVE }));
    await screen.findByText(SETTINGS.SAVE_OK_NOTE);
    const posts = postCallsOf(fn!);
    expect(JSON.parse(posts[0][1].body as string)).toEqual([
      { key: 'sources.qoder.enabled', value: false },
    ]);
  });

  it('保存流字段级拒绝：ok=false + field_errors 逐条红字，草稿保留可再保存', async () => {
    await renderReady({
      ...OK_ROUTES,
      'POST /api/config': {
        body: { ok: false, field_errors: [{ key: 'sources.qoder.throttle.steady_rpm', message: '越界', code: 'range' }] },
      },
    });
    fireEvent.change(steadyInput(), { target: { value: '5' } });
    fireEvent.click(throttleSave());
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.SAVE }));
    await screen.findByText(SETTINGS.SAVE_REJECTED_TITLE);
    const alert = screen.getAllByRole('alert').find((a) => a.textContent?.includes(SETTINGS.SAVE_REJECTED_TITLE))!;
    expect(alert.textContent).toContain('sources.qoder.throttle.steady_rpm');
    expect(alert.textContent).toContain('越界');
    // 草稿保留：dirty 仍在，可修正后再保存
    expect(screen.getByText(settingsLine(SETTINGS.DIRTY_SUMMARY, { count: 1 }))).toBeTruthy();
  });

  it('保存流 401 → 如实展示 HTTP 401；404（后端端点未部署）同样不假成功', async () => {
    await renderReady({ ...OK_ROUTES, 'POST /api/config': { status: 401, body: { error: 'unauthorized' } } });
    fireEvent.change(steadyInput(), { target: { value: '5' } });
    fireEvent.click(throttleSave());
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.SAVE }));
    expect(await screen.findByText(settingsLine(SETTINGS.SAVE_FAILED, { detail: 'HTTP 401' }))).toBeTruthy();

    installFetch({ ...OK_ROUTES, 'POST /api/config': { status: 404, body: { error: 'not found' } } });
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.SAVE }));
    expect(await screen.findByText(settingsLine(SETTINGS.SAVE_FAILED, { detail: 'HTTP 404' }))).toBeTruthy();
    // 失败后 dirty 保留（改动不丢）
    expect(screen.getByText(settingsLine(SETTINGS.DIRTY_SUMMARY, { count: 1 }))).toBeTruthy();
  });

  it('保存流网络错误 → 如实展示；成功态被新一轮改动覆盖回 dirty 文案', async () => {
    await renderReady({ ...OK_ROUTES, 'POST /api/config': { reject: true } });
    fireEvent.change(steadyInput(), { target: { value: '5' } });
    fireEvent.click(throttleSave());
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.SAVE }));
    expect(await screen.findByText(settingsLine(SETTINGS.SAVE_FAILED, { detail: 'network error' }))).toBeTruthy();

    installFetch({ ...OK_ROUTES, 'POST /api/config': { body: SAVE_OK_WIRE } });
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.SAVE }));
    await screen.findByText(SETTINGS.SAVE_OK_NOTE);
    // 新一轮改动把汇总行拉回 dirty 文案（成功态被覆盖）
    fireEvent.change(steadyInput(), { target: { value: '6' } });
    fireEvent.click(throttleSave());
    expect(screen.getByText(settingsLine(SETTINGS.DIRTY_SUMMARY, { count: 1 }))).toBeTruthy();
  });
});

describe('设置屏 — 高级区（指纹开关 / 来源启用）', () => {
  it('指纹开启走 DangerAction 逐字确认，确认后进草稿并点亮 dirty', async () => {
    await renderReady();
    const sw = screen.getByRole('switch', { name: `${ADVANCED.COLUMN} · ${sourceDisplayName('qoder')}` });
    expect(sw.getAttribute('aria-checked')).toBe('false');
    fireEvent.click(sw);
    const dialog = screen.getByRole('dialog');
    expect(dialog.textContent).toContain('仅影响 Qoder 这一个来源');
    // 页面上另有 X-API-Key 输入框（同样 role=textbox），确认输入按 name 属性在弹窗内精确查找
    const confirmInput = dialog.querySelector('input[name="danger-confirm-word"]') as HTMLInputElement;
    fireEvent.change(confirmInput, { target: { value: ADVANCED.CONFIRM_WORD } });
    fireEvent.click(screen.getByRole('button', { name: DANGER.CONFIRM }));
    expect(screen.getByRole('switch', { name: `${ADVANCED.COLUMN} · ${sourceDisplayName('qoder')}` }).getAttribute('aria-checked')).toBe('true');
    expect(document.querySelector('.settings-screen__dirty-dot')).not.toBeNull();
  });

  it('来源启用开关（danger 组交付）切进草稿；停用态如实显示已停用', async () => {
    await renderReady();
    const cb = screen.getByRole('checkbox', { name: new RegExp(SETTINGS.SOURCE_ENABLED_LABEL) }) as HTMLInputElement;
    expect(cb.checked).toBe(true);
    expect(screen.getByText(SETTINGS.SOURCE_ENABLED_ON)).toBeTruthy();
    fireEvent.click(cb);
    expect(screen.getByText(SETTINGS.SOURCE_ENABLED_OFF)).toBeTruthy();
    expect(document.querySelector('.settings-screen__dirty-dot')).not.toBeNull();
  });
});

describe('设置屏 — X-API-Key 来源（内存态，M1 红线）', () => {
  /** 保存流是天然的带证请求：粘贴 key → 改节流值 → 保存 → 检查 POST /api/config 的请求头。 */
  async function saveWithKey(fn: ReturnType<typeof installFetch> | undefined, steady: string) {
    fireEvent.change(steadyInput(), { target: { value: steady } });
    fireEvent.click(throttleSave());
    const before = fn!.mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: SETTINGS.SAVE }));
    await screen.findByText(SETTINGS.SAVE_OK_NOTE);
    return fn!.mock.calls.slice(before).filter((c) => String(c[0]) === '/api/config' && (c[1] as RequestInit).method === 'POST');
  }

  it('粘贴 key 后保存请求带头（trim 生效）；清空回到开放模式不带头', async () => {
    const fn = await renderReady({ ...OK_ROUTES, 'POST /api/config': { body: SAVE_OK_WIRE } });
    const input = screen.getByLabelText(SETTINGS.APIKEY_TITLE) as HTMLInputElement;

    fireEvent.change(input, { target: { value: '  sk-gateway-1  ' } });
    const withKey = await saveWithKey(fn, '5');
    expect(withKey).toHaveLength(1);
    expect(((withKey[0][1] as RequestInit).headers as Record<string, string>)['X-API-Key']).toBe('sk-gateway-1');

    // 保存成功后基线已是 5，第二轮改成 6 再触发一次保存
    fireEvent.change(input, { target: { value: '   ' } });
    const cleared = await saveWithKey(fn, '6');
    expect(cleared).toHaveLength(1);
    expect(((cleared[0][1] as RequestInit).headers as Record<string, string>)['X-API-Key']).toBeUndefined();
  });

  it('输入框不落任何 storage（M1 红线）：内存态在重渲染后仍保持，storage API 全程未被触碰', async () => {
    await renderReady();
    const input = screen.getByLabelText(SETTINGS.APIKEY_TITLE) as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'sk-memory-only' } });
    fireEvent.click(screen.getByRole('radio', { name: 'Trae' }));
    await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('trae')}`);
    fireEvent.click(screen.getByRole('radio', { name: 'Qoder' }));
    await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('qoder')}`);
    expect((screen.getByLabelText(SETTINGS.APIKEY_TITLE) as HTMLInputElement).value).toBe('sk-memory-only');
  });
});

describe('设置屏 — API Key 明文/密文切换（M8-connect 眼睛按钮）', () => {
  it('默认密文；点击切明文再切回，type 与 aria-pressed 联动且不影响内存态值', async () => {
    await renderReady();
    const input = screen.getByLabelText(SETTINGS.APIKEY_TITLE) as HTMLInputElement;
    expect(input.type).toBe('password');
    const eye = screen.getByRole('button', { name: SETTINGS.APIKEY_SHOW });
    expect(eye.getAttribute('aria-pressed')).toBe('false');

    fireEvent.change(input, { target: { value: 'sk-visible-check' } });
    fireEvent.click(eye);
    expect(input.type).toBe('text');
    expect(eye.getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: SETTINGS.APIKEY_HIDE })).toBeTruthy();
    // 显隐纯前端：输入框内存态不因切换丢失
    expect((screen.getByLabelText(SETTINGS.APIKEY_TITLE) as HTMLInputElement).value).toBe('sk-visible-check');

    fireEvent.click(screen.getByRole('button', { name: SETTINGS.APIKEY_HIDE }));
    expect(input.type).toBe('password');
    expect(eye.getAttribute('aria-pressed')).toBe('false');
  });
});

describe('设置屏 — axe 基线', () => {
  it('就绪态（默认源）无违规', async () => {
    installFetch(OK_ROUTES);
    const { container } = render(<SettingsScreen />);
    await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('qoder')}`);
    expect(await axe(container)).toHaveNoViolations();
  });
});
