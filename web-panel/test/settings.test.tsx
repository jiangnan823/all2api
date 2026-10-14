import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { SettingsScreen } from '../src/app/SettingsScreen';
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

/** 通用组（gateway/pooling…）不属于按源设置面：在场但应被屏幕忽略。 */
const FIELDS_WIRE = [
  { group: 'gateway', fields: [{ key: 'listen', label: 'config.field.listen.label', type: 'string', effect: 'restart_required' }] },
  { group: 'pooling', fields: [fnum('pool.credit_floor', 'config.field.credit_floor.label', 'credit', 0, 1000000)] },
  throttleGroup('qoder', { jitter: true, halve: true }),
  dangerGroup('qoder'),
  throttleGroup('trae', { jitter: false, halve: false }),
  dangerGroup('trae'),
  throttleGroup('workbuddy', { jitter: true, halve: false }),
  dangerGroup('workbuddy'),
];

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
  const fn = vi.fn(async (input: unknown) => {
    const url = typeof input === 'string' ? input : String(input);
    const route = routes[url] ?? { status: 404, body: { error: 'not found' } };
    if (route.reject) throw new TypeError('fetch failed');
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => route.body };
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

async function renderReady(routes: Routes = OK_ROUTES) {
  installFetch(routes);
  render(<SettingsScreen />);
  await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('qoder')}`);
}

const steadyInput = () => screen.getByLabelText('稳态请求') as HTMLInputElement;
const throttleSave = () => screen.getByRole('button', { name: THROTTLE.SAVE }) as HTMLButtonElement;

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  setDeliveredThrottleFields(null); // 交付注册表是模块态，测试间必须清空（lib/throttle.ts 数据源缝）
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

  it('通用组（gateway/pooling）不进按源屏，原始文案 key 不外漏', async () => {
    await renderReady();
    const text = document.body.textContent ?? '';
    expect(text).not.toContain('config.field.listen.label');
    expect(text).not.toContain('config.field.credit_floor.label');
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

describe('设置屏 — dirty 跟踪与只读边界', () => {
  it('保存按钮常置灰 + 诚实文案；放弃本地改动回到服务端基线', async () => {
    await renderReady();
    const save = screen.getByRole('button', { name: SETTINGS.SAVE }) as HTMLButtonElement;
    const discard = screen.getByRole('button', { name: SETTINGS.DISCARD }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(screen.getByText(SETTINGS.SAVE_DISABLED_NOTE)).toBeTruthy();
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
});

describe('设置屏 — 高级区（指纹开关 / 来源启用）', () => {
  it('指纹开启走 DangerAction 逐字确认，确认后进草稿并点亮 dirty', async () => {
    await renderReady();
    const sw = screen.getByRole('switch', { name: `${ADVANCED.COLUMN} · ${sourceDisplayName('qoder')}` });
    expect(sw.getAttribute('aria-checked')).toBe('false');
    fireEvent.click(sw);
    const dialog = screen.getByRole('dialog');
    expect(dialog.textContent).toContain('仅影响 Qoder 这一个来源');
    fireEvent.change(screen.getByRole('textbox'), { target: { value: ADVANCED.CONFIRM_WORD } });
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

describe('设置屏 — axe 基线', () => {
  it('就绪态（默认源）无违规', async () => {
    installFetch(OK_ROUTES);
    const { container } = render(<SettingsScreen />);
    await screen.findByText(`${THROTTLE.TITLE} · ${sourceDisplayName('qoder')}`);
    expect(await axe(container)).toHaveNoViolations();
  });
});
