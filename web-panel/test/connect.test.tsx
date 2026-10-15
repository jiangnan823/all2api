import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { ConnectScreen } from '../src/app/ConnectScreen';
import { CONNECT, MODEL_SOURCE_NAMES } from '../src/i18n/zh';

/**
 * 连接屏（M8-connect）门禁：三块内容卡各自独立渲染与出错——
 *   卡一 Base URL = location.origin + '/v1' + 一键复制；
 *   卡二 API Key 状态 = /api/config 的 raw.api_key 有无 → 已配置/开放模式徽标 + 去配置页引导；
 *   卡三 可用模型 = /api/models 的 id/name 表 + 平台徽标 + 一键复制模型名，失败如实错误态。
 * fetch 全程 mock（沿用 models.test 的 stubGlobal 模式）；clipboard 以可配置属性注入 navigator。
 */
expect.extend(toHaveNoViolations);

// ---------- wire fixtures ----------

const CONFIG_WITH_KEY = { raw: { listen: ':8080', api_key: 'sk-gateway' }, notice_version: '1' };
const CONFIG_OPEN = { raw: { listen: ':8080' }, notice_version: '1' };

const MODELS_WIRE = {
  models: [
    { platform: 'workbuddy', id: 'workbuddy/deepseek-v4-flash', name: 'deepseek-v4-flash' },
    { platform: 'workbuddy', id: 'workbuddy/glm-5.3-flash', name: 'glm-5.3-flash' },
    { platform: 'qoder', id: 'qoder/GLM-5.3', name: 'GLM-5.3' },
  ],
};

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

// ---------- clipboard stub（jsdom 无实现，按可配置属性注入） ----------

const writeText = vi.fn(async () => undefined);

beforeEach(() => {
  Object.defineProperty(window.navigator, 'clipboard', { value: { writeText }, configurable: true });
});

async function renderReady(routes: Routes = { '/api/config': { body: CONFIG_WITH_KEY }, '/api/models': { body: MODELS_WIRE } }) {
  const fn = installFetch(routes);
  render(<ConnectScreen onOpenSettings={() => {}} />);
  await screen.findByText(CONNECT.BASEURL_TITLE);
  await screen.findByText('deepseek-v4-flash');
  return fn;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  // @ts-expect-error 测试桩清理：还原 jsdom 无 clipboard 的初始形态
  delete window.navigator.clipboard;
  writeText.mockClear();
});

describe('连接屏 — 三块内容卡渲染', () => {
  it('挂载并发拉取 /api/config + /api/models；三卡齐全', async () => {
    const fn = await renderReady();
    expect(fn.mock.calls.map((c) => String(c[0])).sort()).toEqual(['/api/config', '/api/models']);
    expect(screen.getByRole('heading', { level: 3, name: CONNECT.BASEURL_TITLE })).toBeTruthy();
    expect(screen.getByRole('heading', { level: 3, name: CONNECT.KEY_TITLE })).toBeTruthy();
    expect(screen.getByRole('heading', { level: 3, name: CONNECT.MODELS_TITLE })).toBeTruthy();
  });

  it('卡一：Base URL = location.origin + "/v1"，原样展示', async () => {
    await renderReady();
    expect(screen.getByText(`${window.location.origin}/v1`)).toBeTruthy();
  });

  it('卡二：api_key 非空 → 「已配置」徽标 + 携带头说明 + 去配置页引导', async () => {
    await renderReady();
    expect(screen.getByText(CONNECT.KEY_CONFIGURED)).toBeTruthy();
    expect(screen.getByText(CONNECT.KEY_CONFIGURED_HINT)).toBeTruthy();
    const goto = screen.getByRole('link', { name: CONNECT.KEY_GOTO_SETTINGS });
    expect(goto.getAttribute('href')).toBe('#settings');
  });

  it('卡二：api_key 缺失 → 「开放模式」徽标（不装作已配置）', async () => {
    await renderReady({ '/api/config': { body: CONFIG_OPEN }, '/api/models': { body: MODELS_WIRE } });
    expect(screen.getByText(CONNECT.KEY_OPEN)).toBeTruthy();
    expect(screen.getByText(CONNECT.KEY_OPEN_HINT)).toBeTruthy();
  });

  it('卡三：id/name 表格三行，平台徽标走显示名映射；计数行联动', async () => {
    await renderReady();
    expect(document.querySelectorAll('tbody tr')).toHaveLength(3);
    // workbuddy 两行 → 平台徽标重复出现，按出现次数断言
    expect(screen.getAllByText(MODEL_SOURCE_NAMES.workbuddy)).toHaveLength(2);
    expect(screen.getByText(MODEL_SOURCE_NAMES.qoder)).toBeTruthy();
    expect(screen.getByText('workbuddy/deepseek-v4-flash')).toBeTruthy();
    expect(screen.getByText(CONNECT.MODELS_COUNT.replace('{count}', '3'))).toBeTruthy();
  });

  it('卡三：models 空数组 → 空态文案', async () => {
    installFetch({ '/api/config': { body: CONFIG_WITH_KEY }, '/api/models': { body: { models: [] } } });
    render(<ConnectScreen onOpenSettings={() => {}} />);
    expect(await screen.findByText(CONNECT.MODELS_EMPTY)).toBeTruthy();
  });
});

describe('连接屏 — 一键复制', () => {
  it('Base URL 复制：writeText 收到完整 URL，出现「已复制」回执', async () => {
    await renderReady();
    fireEvent.click(screen.getByRole('button', { name: `${CONNECT.COPY}: ${window.location.origin}/v1` }));
    await screen.findByText(CONNECT.COPIED);
    expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/v1`);
  });

  it('模型名复制：writeText 收到模型名称（客户端 model 参数取值）', async () => {
    await renderReady();
    fireEvent.click(screen.getByRole('button', { name: `${CONNECT.COPY_MODEL}: deepseek-v4-flash` }));
    await screen.findByText(CONNECT.COPIED);
    expect(writeText).toHaveBeenCalledWith('deepseek-v4-flash');
  });

  it('clipboard 不可用 → 如实不出现「已复制」，不假成功', async () => {
    Object.defineProperty(window.navigator, 'clipboard', { value: {}, configurable: true });
    await renderReady();
    fireEvent.click(screen.getByRole('button', { name: `${CONNECT.COPY}: ${window.location.origin}/v1` }));
    expect(screen.queryByText(CONNECT.COPIED)).toBeNull();
  });
});

describe('连接屏 — 如实错误态', () => {
  it('卡二 /api/config 401 → 错误卡 + 重试成功后恢复徽标', async () => {
    installFetch({ '/api/config': { status: 401, body: { error: 'unauthorized' } }, '/api/models': { body: MODELS_WIRE } });
    render(<ConnectScreen onOpenSettings={() => {}} />);
    expect(await screen.findByText(CONNECT.KEY_ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('HTTP 401')).toBeTruthy();
    installFetch({ '/api/config': { body: CONFIG_WITH_KEY }, '/api/models': { body: MODELS_WIRE } });
    fireEvent.click(screen.getByRole('button', { name: CONNECT.RETRY }));
    expect(await screen.findByText(CONNECT.KEY_CONFIGURED)).toBeTruthy();
  });

  it('卡三 /api/models 失败 → 如实错误态（错误消息 + 重试），不影响卡二', async () => {
    installFetch({ '/api/config': { body: CONFIG_WITH_KEY }, '/api/models': { reject: true } });
    render(<ConnectScreen onOpenSettings={() => {}} />);
    expect(await screen.findByText(CONNECT.MODELS_ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('network error')).toBeTruthy();
    expect(screen.getByText(CONNECT.KEY_CONFIGURED)).toBeTruthy(); // 卡二不受牵连
    installFetch({ '/api/config': { body: CONFIG_WITH_KEY }, '/api/models': { body: { models: [{ platform: 'trae', id: 'trae/t-1', name: 't-1' }] } } });
    fireEvent.click(screen.getByRole('button', { name: CONNECT.RETRY }));
    await screen.findByText('t-1');
  });

  it('形状异常（models 非数组）→ 错误态不静默兜底', async () => {
    installFetch({ '/api/config': { body: CONFIG_WITH_KEY }, '/api/models': { body: { models: 42 } } });
    render(<ConnectScreen onOpenSettings={() => {}} />);
    expect(await screen.findByText(CONNECT.MODELS_ERROR_TITLE)).toBeTruthy();
  });

  it('「去配置页修改」点击触发外壳换页回调', async () => {
    const onOpenSettings = vi.fn();
    installFetch({ '/api/config': { body: CONFIG_WITH_KEY }, '/api/models': { body: MODELS_WIRE } });
    render(<ConnectScreen onOpenSettings={onOpenSettings} />);
    await screen.findByText(CONNECT.KEY_CONFIGURED);
    fireEvent.click(screen.getByRole('link', { name: CONNECT.KEY_GOTO_SETTINGS }));
    expect(onOpenSettings).toHaveBeenCalledTimes(1);
  });
});

describe('连接屏 — axe 基线', () => {
  it('就绪态（已配置 + 模型表）无违规', async () => {
    installFetch({ '/api/config': { body: CONFIG_WITH_KEY }, '/api/models': { body: MODELS_WIRE } });
    const { container } = render(<ConnectScreen onOpenSettings={() => {}} />);
    await screen.findByText('deepseek-v4-flash');
    expect(await axe(container)).toHaveNoViolations();
  });

  it('开放模式 + 模型空态无违规', async () => {
    installFetch({ '/api/config': { body: CONFIG_OPEN }, '/api/models': { body: { models: [] } } });
    const { container } = render(<ConnectScreen onOpenSettings={() => {}} />);
    await screen.findByText(CONNECT.MODELS_EMPTY);
    expect(await axe(container)).toHaveNoViolations();
  });

  it('双卡错误态无违规', async () => {
    installFetch({ '/api/config': { status: 500, body: {} }, '/api/models': { status: 500, body: {} } });
    const { container } = render(<ConnectScreen onOpenSettings={() => {}} />);
    await screen.findByText(CONNECT.KEY_ERROR_TITLE);
    expect(await axe(container)).toHaveNoViolations();
  });
});
