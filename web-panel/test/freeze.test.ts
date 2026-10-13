import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync, existsSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { REASON_COPY, reasonCopy } from '../src/i18n/zh';

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
  /**
   * seq=445 裁三：禁「冷却中」的范围收窄为 **402/quota_exhausted 语境**。
   * 语境判定 = 同一行活代码里出现 402 锚点（quota_exhausted / exhaust_402 / 额度耗尽 / EXHAUST_*）。
   * 非 402 语境的"冷却"（熔断/退避冷却、紧急度排序，pool-02 同族）按 UI UE v0.2.4 口径合法存在，
   * 全层禁词会在队列③ 接线时被迫破例，故不设全局绝迹断言。
   */
  const CTX_402 = /quota_exhausted|exhaust_402|额度耗尽|EXHAUST_/;

  it('唯一形式逐字在场', () => {
    expect(impl.some((f) => f.text.includes('额度耗尽·次日04:00重置'))).toBe(true);
  });

  it('402 语境行内不得出现「冷却中」', () => {
    const hits: string[] = [];
    for (const f of impl) {
      f.code.split('\n').forEach((line, i) => {
        if (CTX_402.test(line) && line.includes('冷却中')) hits.push(`${f.rel}:${i + 1}`);
      });
    }
    expect(hits, `402 行被改写成冷却语义：${hits.join(', ')}`).toEqual([]);
  });

  it('402 锚点文案只有一个来源：REASON_COPY.quota_exhausted.label 逐字等于冻结串', () => {
    // 硬门从"扫字面"升级为"判值"：任何第二写法（含标点/空格变体）都会在这里红，而不是靠 grep 白名单
    expect(reasonCopy('quota_exhausted').label).toBe('额度耗尽·次日04:00重置');
    expect(REASON_COPY.quota_exhausted?.label).toBe('额度耗尽·次日04:00重置');
    // ThrottleForm 只读锚点行共用同一值，不另立第四种写法
    expect(impl.find((f) => f.rel === 'components/ThrottleForm.tsx')?.text).toContain(
      "reasonCopy('quota_exhausted')",
    );
    // 全实现层凡是"额度耗尽…重置"这条红线句，标点/空格形态必须与冻结串完全一致（自然语言里单独提到「额度耗尽」不受限）
    const variants = new Set(
      impl.flatMap((f) => f.text.match(/额度耗尽[^\n'"`]*重置/g) ?? []),
    );
    expect([...variants]).toEqual(['额度耗尽·次日04:00重置']);
  });
});
