import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync, existsSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';

/**
 * 冻结令（seq=405）自检门禁：把"别再引新名 / 单位硬门 / 402 红线"变成可执行断言。
 * 扫描面 = 实现层（components / lib / i18n / styles）；
 * src/types/api-contract.ts 是冻结契约镜像，其废弃声明按原文保留，不参与"禁词"扫描。
 */
const CANDIDATES = [join(process.cwd(), 'src'), join(process.cwd(), '../src')].map((p) => resolve(p));
const ROOT = CANDIDATES.find((p) => existsSync(p));
if (!ROOT) throw new Error(`找不到 src 目录：${CANDIDATES.join(', ')}`);
const IMPL_DIRS = ['components', 'lib', 'i18n', 'styles'];

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else if (/\.(ts|tsx|css)$/.test(name)) out.push(p);
  }
  return out;
}

const rawAll = walk(ROOT).map((p) => ({ rel: relative(ROOT, p).replace(/\\/g, '/'), text: readFileSync(p, 'utf8') }));
/** 禁词扫描只看活代码：注释里允许出现"废弃声明"（契约镜像与说明句必须能写下 max_rps 才能宣布它死了）。 */
const stripComments = (s: string) => s.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^(\s*\/\/.*)$/gm, '');
const all = rawAll.map((f) => ({ ...f, code: stripComments(f.text) }));
const impl = all.filter((f) => IMPL_DIRS.includes(f.rel.split('/')[0]));

it('语料非空', () => {
  expect(all.length).toBeGreaterThan(8);
  expect(impl.length).toBeGreaterThan(5);
});

describe('G1 单位硬门（实现层）', () => {
  it('不再出现速率语义的 rps 家族字段', () => {
    for (const forbidden of ['max_rps', 'rate_limit_rps', 'requestsPerMin', 'burst_size']) {
      const hit = impl.filter((f) => f.code.includes(forbidden)).map((f) => f.rel);
      expect(hit, `${forbidden} 出现在 ${hit.join(', ')}`).toEqual([]);
    }
  });

  it('界面不出现「每秒」', () => {
    expect(impl.filter((f) => /每秒/.test(f.code)).map((f) => f.rel)).toEqual([]);
  });

  it('实现层唯一速率口径 = _rpm', () => {
    expect(impl.some((f) => f.text.includes('steady_rpm') && f.text.includes('burst_rpm'))).toBe(true);
  });
});

describe('命名权威（裁定一／二）', () => {
  const required = [
    'steady_rpm', 'burst_rpm', 'min_gap_ms', 'daily_cap', 'inflight',
    'backoff.base_429_sec', 'backoff.not_found_sec', 'backoff.exhaust_402_until',
    'jitter_minutes', 'halve_budget_after_429',
    'request_fingerprint_rewrite', 'notice_version',
  ];

  it.each(required)('冻结名在场：%s', (name) => {
    expect(impl.some((f) => f.text.includes(name)), name).toBe(true);
  });

  const banned = [
    'daily_req_cap', 'concurrent_sessions', 'backoff_429_base_sec', 'backoff_404_sec',
    'signin_jitter_minutes', 'halve_daily_budget_after_429', 'fingerprint_scrub',
    'features.request_fingerprint_rewrite', 'rateBudgets',
  ];

  it.each(banned)('废弃／自创名不得回潮：%s', (name) => {
    expect(impl.some((f) => f.text.includes(name)), name).toBe(false);
  });
});

describe('G3 / F-V1 落点唯一', () => {
  it('consent 请求体只有 source + notice_version，不含凭证字段', () => {
    const lib = impl.find((f) => f.rel === 'lib/consent.ts');
    expect(lib?.text).toContain('notice_version');
    expect(lib?.text).not.toMatch(/accountLabel|credential|expiresAt/);
  });

  it('指纹开关只有 sources.<id>.advanced.request_fingerprint_rewrite 一条 path', () => {
    const paths = impl.flatMap((f) => f.text.match(/\.advanced\.[A-Za-z_$][\w$.{}-]*/g) ?? []);
    for (const p of paths) {
      expect(p === '.advanced.request_fingerprint_rewrite' || p === '.advanced.${flagKey}').toBe(true);
    }
    expect(all.some((f) => f.text.includes('.advanced.request_fingerprint_rewrite'))).toBe(true);
  });
});

describe('402 文案红线', () => {
  it('唯一形式逐字在场，"冷却中"绝迹', () => {
    expect(impl.some((f) => f.text.includes('额度耗尽·次日04:00重置'))).toBe(true);
    expect(impl.filter((f) => f.code.includes('冷却中')).map((f) => f.rel)).toEqual([]);
  });
});
