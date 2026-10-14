import { describe, it, expect, vi, afterEach } from 'vitest';
import {
  ApiError,
  apiGet,
  parseConfigGroups,
  parsePanelConfig,
  parseServerTime,
  setApiKeyProvider,
} from '../src/lib/panelApi';

/**
 * 面板 API 层门禁：X-API-Key 透传钩子（未配置不带头 = 后端开放模式语义）、
 * HTTP/网络/非 JSON 错误映射、蛇形 wire → 契约 camelCase 的防御式解析。
 * 后端无保存端点是如实边界：本层只存在 GET 路径。
 */

function installFetch(impl: () => Promise<unknown>) {
  // 占位 ...args：vi.fn 的调用签名需与 fetch 一致（url, init），否则 mock.calls 元组退化为 []
  const fn = vi.fn(async (..._args: unknown[]) => impl());
  vi.stubGlobal('fetch', fn);
  return fn;
}

function headersOf(fn: ReturnType<typeof installFetch>, call: number): Record<string, string> {
  const init = fn.mock.calls[call]?.[1] as RequestInit | undefined;
  return (init?.headers as Record<string, string> | undefined) ?? {};
}

afterEach(() => {
  vi.unstubAllGlobals();
  setApiKeyProvider(() => '');
});

describe('apiGet — X-API-Key 透传钩子', () => {
  it('默认（未注入 key）不带头，与后端开放模式语义一致', async () => {
    const fn = installFetch(async () => ({ ok: true, status: 200, json: async () => ({}) }));
    await apiGet('/api/config', (v) => v);
    expect(headersOf(fn, 0)['X-API-Key']).toBeUndefined();
  });

  it('注入 provider 后透传（trim 空白）；空白串回到不带头', async () => {
    const fn = installFetch(async () => ({ ok: true, status: 200, json: async () => ({}) }));
    setApiKeyProvider(() => '  sk-test  ');
    await apiGet('/api/time', (v) => v);
    expect(headersOf(fn, 0)['X-API-Key']).toBe('sk-test');
    setApiKeyProvider(() => '   ');
    await apiGet('/api/time', (v) => v);
    expect(headersOf(fn, 1)['X-API-Key']).toBeUndefined();
  });

  it('请求为相对路径 GET（G5：base 由运行时注入，不硬编码 host）', async () => {
    const fn = installFetch(async () => ({ ok: true, status: 200, json: async () => ({}) }));
    await apiGet('/api/config/fields', (v) => v);
    expect(fn.mock.calls[0][0]).toBe('/api/config/fields');
    expect((fn.mock.calls[0][1] as RequestInit).method).toBe('GET');
  });
});

describe('apiGet — 错误映射', () => {
  it('HTTP 非 2xx → ApiError 携 status（后端 withAPIKey 401 走此通道）', async () => {
    installFetch(async () => ({ ok: false, status: 401, json: async () => ({ error: 'unauthorized' }) }));
    const err = await apiGet('/api/config', (v) => v).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(401);
    expect((err as ApiError).message).toBe('HTTP 401');
  });

  it('网络拒绝 → ApiError(network error)', async () => {
    installFetch(async () => {
      throw new TypeError('fetch failed');
    });
    await expect(apiGet('/api/config', (v) => v)).rejects.toMatchObject({ message: 'network error' });
  });

  it('响应体非 JSON → ApiError', async () => {
    installFetch(async () => ({
      ok: true,
      status: 200,
      json: async () => {
        throw new SyntaxError('Unexpected token');
      },
    }));
    await expect(apiGet('/api/config', (v) => v)).rejects.toBeInstanceOf(ApiError);
  });
});

describe('parsePanelConfig — 蛇形 wire → PanelConfig', () => {
  const BUDGET = {
    steady_rpm: 4, burst_rpm: 12, min_gap_ms: [200, 1200], daily_cap: 300, inflight: 1,
    backoff: { base_429_sec: 600, not_found_sec: 300, exhaust_402_until: 'next-04:00' },
    jitter_minutes: 15, halve_budget_after_429: true,
  };

  it('throttle/advanced/feature_flags/notice_version 全量换算', () => {
    const out = parsePanelConfig({
      raw: { sources: {} },
      feature_flags: { responses_api: true, session_stickiness: false, redis_mirror: false, debug_body_sampling: false },
      throttle: { qoder: BUDGET },
      advanced: { qoder: { request_fingerprint_rewrite: false } },
      notice_version: '1',
    });
    expect(out.throttle.qoder).toEqual({
      steadyRpm: 4, burstRpm: 12, minGapMs: [200, 1200], dailyCap: 300, inflight: 1,
      backoff: { base429Sec: 600, notFoundSec: 300, exhaust402Until: 'next-04:00' },
      jitterMinutes: 15, halveBudgetAfter429: true,
    });
    expect(out.advanced.qoder?.fingerprintRewrite).toBe(false);
    expect(out.featureFlags.responsesApi).toBe(true);
    expect(out.noticeVersion).toBe('1');
  });

  it('advanced 内层键缺失 → false（F-V1 安全默认），notice_version 缺失容忍为空串', () => {
    const out = parsePanelConfig({
      raw: {}, feature_flags: {}, throttle: { trae: BUDGET }, advanced: { trae: {} },
    });
    expect(out.advanced.trae?.fingerprintRewrite).toBe(false);
    expect(out.noticeVersion).toBe('');
  });

  it('exhaust_402_until 出现第二取值 → 拒收（只读锚点唯一合法值）', () => {
    const bad = { ...BUDGET, backoff: { ...BUDGET.backoff, exhaust_402_until: 'next-05:00' } };
    expect(() => parsePanelConfig({ raw: {}, feature_flags: {}, throttle: { qoder: bad }, advanced: {} })).toThrow(ApiError);
  });

  it('min_gap_ms 缺成员 / steady_rpm 非数值 → 拒收', () => {
    const gapBad = { ...BUDGET, min_gap_ms: [200] };
    expect(() => parsePanelConfig({ raw: {}, feature_flags: {}, throttle: { qoder: gapBad }, advanced: {} })).toThrow(ApiError);
    const numBad = { ...BUDGET, steady_rpm: '4' };
    expect(() => parsePanelConfig({ raw: {}, feature_flags: {}, throttle: { qoder: numBad }, advanced: {} })).toThrow(ApiError);
  });
});

describe('parseConfigGroups / parseServerTime', () => {
  it('requires_confirm 蛇形键 → 契约 requiresConfirm；label/effect 逐字段保留', () => {
    const groups = parseConfigGroups([
      {
        group: 'danger.qoder',
        fields: [
          {
            key: 'sources.qoder.advanced.request_fingerprint_rewrite',
            label: 'config.field.fingerprint_rewrite.label',
            type: 'boolean',
            description: 'config.field.fingerprint_rewrite.desc',
            effect: 'next_request',
            requires_confirm: true,
          },
        ],
      },
    ]);
    expect(groups[0]?.fields[0]).toMatchObject({ requiresConfirm: true, effect: 'next_request' });
  });

  it('未知 type / effect → 拒收（枚举闭集，不静默放行）', () => {
    expect(() => parseConfigGroups([{ group: 'g', fields: [{ key: 'x', label: 'l', type: 'float', effect: 'immediate' }] }])).toThrow(ApiError);
    expect(() => parseConfigGroups([{ group: 'g', fields: [{ key: 'x', label: 'l', type: 'number', effect: 'on_save' }] }])).toThrow(ApiError);
  });

  it('非数组响应 → 拒收', () => {
    expect(() => parseConfigGroups({ error: 'oops' })).toThrow(ApiError);
  });

  it('parseServerTime：now/local/timezone', () => {
    const t = parseServerTime({ now: 1760420400, local: '2026-10-14 12:00:00', timezone: 'UTC' });
    expect(t).toEqual({ now: 1760420400, local: '2026-10-14 12:00:00', timezone: 'UTC' });
  });
});
