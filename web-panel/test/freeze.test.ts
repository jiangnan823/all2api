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

/**
 * 红线句扫描域（UI UE seq=478 第八条建议）：只看 402 出口面，不含 §9 须知正文。
 * 分工：红线句式归本门，须知逐字归 TC-M1-033 的 copy §9 diff——两条门各管各的，不互相误伤。
 * zh.ts 里的 §9 区段 = `export const CONSENT` 起、`export const THROTTLE` 止（含 CONSENT_APPENDIX 与来源名）。
 */
function stripNoticeBaseline(text: string): string {
  const from = text.indexOf('export const CONSENT = {');
  const to = text.indexOf('export const THROTTLE = {');
  if (from < 0 || to < 0 || to < from) return text;
  return text.slice(0, from) + text.slice(to);
}
const redLineCorpus = impl.map((f) => ({
  rel: f.rel,
  text: f.rel === 'i18n/zh.ts' ? stripNoticeBaseline(f.text) : f.text,
}));

describe('402 文案红线', () => {
  /**
   * seq=445 裁三：禁「冷却中」的范围收窄为 **402/quota_exhausted 语境**。
   * 语境判定 = 同一行活代码里出现 402 锚点（quota_exhausted / exhaust_402 / 额度耗尽 / EXHAUST_*）。
   * 非 402 语境的"冷却"（熔断/退避冷却、紧急度排序，pool-02 同族）按 UI UE v0.2.4 口径合法存在，
   * 全层禁词会在队列③ 接线时被迫破例，故不设全局绝迹断言。
   */
  const CTX_402 = /quota_exhausted|exhaust_402|额度耗尽|EXHAUST_/;

  it('唯一形式逐字在场', () => {
    expect(redLineCorpus.some((f) => f.text.includes('额度耗尽·次日04:00重置'))).toBe(true);
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
    // 红线句式只在 402 出口面内比对，标点/空格形态必须与冻结串完全一致
    const variants = new Set(
      redLineCorpus.flatMap((f) => f.text.match(/额度耗尽[^\n'"`]*重置/g) ?? []),
    );
    expect([...variants]).toEqual(['额度耗尽·次日04:00重置']);
    // 排除区必须是 §9 须知面本身，不能是随手划走的别的文件
    const zh = impl.find((f) => f.rel === 'i18n/zh.ts');
    expect(zh?.text).toContain('CONSENT_APPENDIX');
    expect(stripNoticeBaseline(zh?.text ?? '').length).toBeLessThan((zh?.text.length ?? 0) - 10);
  });
});

describe('§1.2 遮罩 token 定稿（UI UE seq=478 第四点）', () => {
  const tokens = rawAll.find((f) => f.rel === 'styles/tokens.css')?.text ?? '';
  const light = tokens.slice(0, tokens.indexOf('[data-theme="dark"]'));
  const dark = tokens.slice(tokens.indexOf('[data-theme="dark"]'));

  it('--scrim 值两档逐字对齐 UI UE 定稿', () => {
    expect(light).toMatch(/--scrim:\s*rgba\(0,\s*0,\s*0,\s*0?\.45\)/);
    expect(dark).toMatch(/--scrim:\s*rgba\(0,\s*0,\s*0,\s*0?\.65\)/);
  });

  it('遮罩不复用为禁用态/只读态覆盖层：scrim 只挂在模态弹窗上（危险确认 / M7b 成长任务）', () => {
    // 先剥注释再取选择器：注释里提到 scrim 不算使用；段注释紧贴规则时不得污染选择器捕获
    const panel = stripComments(rawAll.find((f) => f.rel === 'styles/panel.css')?.text ?? '');
    const users = [...panel.matchAll(/([^{}\s][^{}]*)\{[^}]*var\(--scrim\)/gs)].map((m) => m[1].trim());
    // M7b 成长任务弹层与危险确认同属模态对话框（遮罩+居中卡+焦点陷阱），准入白名单；
    // 禁用态/只读态等非模态覆盖层仍然一律不得使用 scrim。
    expect(users.sort()).toEqual(['.danger-action__scrim', '.growth-scrim']);
  });
});

describe('§1.1 层级纪律：只用四档，禁第五档与魔法数', () => {
  const tokens = rawAll.find((f) => f.rel === 'styles/tokens.css')?.text ?? '';

  it('z 档只有 sticky / drawer / dialog / toast 四个', () => {
    const scales = [...tokens.matchAll(/--z-([a-z]+):/g)].map((m) => m[1]);
    expect(scales.sort()).toEqual(['dialog', 'drawer', 'sticky', 'toast']);
  });

  it('样式里所有 z-index 都必须引用 var(--z-*)，且 toast 高于 dialog', () => {
    for (const f of impl) {
      for (const line of f.code.split('\n')) {
        const m = line.match(/z-index:\s*([^;]+);/);
        if (!m) continue;
        expect(m[1].trim(), `${f.rel}: ${line.trim()}`).toMatch(/^var\(--z-(sticky|drawer|dialog|toast)\)$/);
      }
    }
    expect(tokens).toMatch(/--z-toast:\s*60/);
    expect(tokens).toMatch(/--z-dialog:\s*50/);
    // 危险确认弹窗在 --z-dialog(50)，热生效回执必须走 --z-toast(60) 才压得住遮罩
    expect(Number(tokens.match(/--z-toast:\s*(\d+)/)?.[1])).toBeGreaterThan(
      Number(tokens.match(/--z-dialog:\s*(\d+)/)?.[1]),
    );
  });
});

describe('接口面字段集合冻结（D2-001 模子推广，UI UE seq=478 第二点）', () => {
  const SRC: string = ROOT;
  function fieldSet(file: string, iface: string): string[] {
    const src = readFileSync(join(SRC, file), 'utf8');
    const start = src.indexOf(`export interface ${iface}`);
    expect(start, `${iface} 未找到`).toBeGreaterThan(-1);
    const body = src.slice(start, src.indexOf('\n}', start));
    return [...body.matchAll(/^\s{2}(\w+)(\??):/gm)].map((m) => `${m[1]}${m[2] ? '?' : ''}`).sort();
  }

  it('DangerActionProps 集合已冻结：第五个 prop、别名或可选性变更 = 契约面改动走 D2', () => {
    expect(fieldSet('components/DangerAction.tsx', 'DangerActionProps')).toEqual([
      'confirmWord',
      'impactSummary',
      'onCancel?',
      'onConfirm',
    ]);
  });

  it('ConsentGateProps 集合已冻结（onAbort 必选在列）', () => {
    expect(fieldSet('components/ConsentGate.tsx', 'ConsentGateProps')).toEqual([
      'noticeVersion',
      'onAbort',
      'onAck',
      'scrollRequired?',
      'source',
    ]);
  });

  it('ThrottleFormProps 集合已冻结（受控三字段）', () => {
    expect(fieldSet('components/ThrottleForm.tsx', 'ThrottleFormProps')).toEqual([
      'budget',
      'onSave',
      'source',
    ]);
  });
});

/**
 * 留痕说明句的可达性与折行纪律（项目经理 seq=501 二①②，测试 seq=482 批三复验清单）。
 * 归口提醒：本门只管"渲染位置 + 不许截断"这两条结构面；CHECK_NOTE 正文逐字仍由 TC-M1-033 的 copy §9 diff 独占守卫。
 */
describe('§5.19 留痕说明句：label 关联内 + 折行不缩句', () => {
  const gate = impl.find((f) => f.rel === 'components/ConsentGate.tsx')?.text ?? '';
  const panel = rawAll.find((f) => f.rel === 'styles/panel.css')?.text ?? '';

  it('CHECK_NOTE 渲染在 <label className="consent-gate__check"> 之内（脱离 label = 读屏只听半句承诺，P1）', () => {
    const labelOpen = gate.indexOf('<label className="consent-gate__check">');
    const noteRender = gate.indexOf('{CONSENT.CHECK_NOTE}');
    const labelClose = gate.indexOf('</label>', labelOpen);
    expect(labelOpen).toBeGreaterThan(-1);
    expect(noteRender).toBeGreaterThan(labelOpen);
    expect(noteRender).toBeLessThan(labelClose);
    // 反面写法钉死：不允许再退回 label 外的兄弟节点
    expect(gate).not.toMatch(/<\/label>\s*<p className="consent-gate__check-note"/);
  });

  it('折行只许调行高/间距：note 规则内禁 text-overflow / line-clamp / nowrap', () => {
    const rule = panel.match(/\.consent-gate__check-note\s*\{([^}]*)\}/)?.[1] ?? '';
    expect(rule).toContain('line-height');
    for (const forbidden of ['text-overflow', 'line-clamp', 'white-space']) {
      expect(rule, `${forbidden} 会把 §9 长句截掉`).not.toContain(forbidden);
    }
  });
});
