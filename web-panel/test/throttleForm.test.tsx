import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import type { ThrottleBudget } from '../src/types/api-contract';
import { ThrottleForm } from '../src/components/ThrottleForm';
import { THROTTLE, reasonCopy } from '../src/i18n/zh';
import { throttlePatchToConfigPatch, throttlePath, validateThrottleBudget } from '../src/lib/throttle';

const QODER: ThrottleBudget = {
  steadyRpm: 4,
  burstRpm: 12,
  minGapMs: [200, 1200],
  dailyCap: 300,
  inflight: 1,
  backoff: { base429Sec: 600, notFoundSec: 300, exhaust402Until: 'next-04:00' },
  jitterMinutes: 15,
  halveBudgetAfter429: true,
};

const TRAE: ThrottleBudget = {
  steadyRpm: 2,
  burstRpm: 6,
  minGapMs: [500, 2000],
  dailyCap: 100,
  inflight: 1,
  backoff: { base429Sec: 600, notFoundSec: 300, exhaust402Until: 'next-04:00' },
};

afterEach(cleanup);

function setup(source = 'qoder', budget: ThrottleBudget = QODER) {
  const onSave = vi.fn();
  render(<ThrottleForm source={source} budget={budget} onSave={onSave} />);
  return { onSave };
}

const steady = () => screen.getByLabelText('稳态请求') as HTMLInputElement;
const save = () => screen.getByRole('button', { name: THROTTLE.SAVE }) as HTMLButtonElement;

describe('ThrottleForm — G1 单位硬门', () => {
  it('界面单位只有「次/分钟」，不出现 rps／每秒', () => {
    setup();
    const text = document.body.textContent ?? '';
    expect(text).toContain(THROTTLE.UNIT_RPM);
    expect(text).not.toMatch(/rps|每秒/i);
    expect(text).not.toMatch(/max_rps|rate_limit_rps|requestsPerMin/);
  });

  it('零换算：输入 4 → 写回值仍是 4（不存在 ×60/÷60）', () => {
    const { onSave } = setup();
    expect(steady().value).toBe('4');
    fireEvent.change(steady(), { target: { value: '5' } });
    fireEvent.click(save());
    expect(onSave).toHaveBeenCalledTimes(1);
    expect(onSave.mock.calls[0][0]).toEqual({ steadyRpm: 5 });
    expect(throttlePatchToConfigPatch('qoder', onSave.mock.calls[0][0])).toEqual([
      { key: 'sources.qoder.throttle.steady_rpm', value: 5 },
    ]);
  });

  it('无改动不产生写回', () => {
    const { onSave } = setup();
    fireEvent.click(save());
    expect(onSave).not.toHaveBeenCalled();
  });

  it('只写真正改过的段，backoff 用嵌套 path', () => {
    const { onSave } = setup();
    fireEvent.change(screen.getByLabelText('429 退避基数'), { target: { value: '900' } });
    fireEvent.click(save());
    const patch = onSave.mock.calls[0][0] as Partial<ThrottleBudget>;
    expect(Object.keys(patch)).toEqual(['backoff']);
    const written = throttlePatchToConfigPatch('qoder', patch);
    expect(written).toContainEqual({ key: 'sources.qoder.throttle.backoff.base_429_sec', value: 900 });
    expect(written.map((w) => w.key)).not.toContain('sources.qoder.throttle.steady_rpm');
    expect(JSON.stringify(written)).not.toContain('exhaust_402_until');
  });

  it('exhaust_402_until 永不出现在写回 patch', () => {
    const patch = throttlePatchToConfigPatch('qoder', {
      backoff: { base429Sec: 600, notFoundSec: 300, exhaust402Until: 'next-04:00' },
    });
    expect(JSON.stringify(patch)).not.toContain('exhaust_402_until');
  });
});

describe('ThrottleForm — 只读锚点与 402 红线', () => {
  it('「触发限流后」是只读文本，没有输入框', () => {
    setup();
    const row = document.querySelector(`tr[data-path="${throttlePath('qoder', 'backoff.exhaust_402_until')}"]`);
    expect(row).not.toBeNull();
    expect(row?.querySelector('input')).toBeNull();
    expect(row?.textContent).toContain('额度耗尽·次日04:00重置');
    expect(row?.textContent).not.toContain('冷却');
  });

  it('402 文案与 api-contract.ts:60 唯一基准逐字一致', () => {
    expect(reasonCopy('quota_exhausted').label).toBe('额度耗尽·次日04:00重置');
  });

  it('rate_limited / quota_exhausted / not_found 不合并（PM 门禁三条）', () => {
    setup();
    expect(reasonCopy('rate_limited').label).toBe('触发限流，已降频');
    expect(reasonCopy('not_found').label).toBe('接口或模型不存在');
    expect(reasonCopy('quota_exhausted').label).not.toBe(reasonCopy('rate_limited').label);
  });
});

describe('ThrottleForm — 字段级越界报错', () => {
  it('稳态超范围 → 红字落在 steady_rpm 上，且不写回', () => {
    const { onSave } = setup();
    fireEvent.change(steady(), { target: { value: '999' } });
    fireEvent.click(save());
    expect(onSave).not.toHaveBeenCalled();
    const alerts = screen.getAllByRole('alert').map((a) => a.textContent ?? '');
    expect(alerts.some((t) => t.includes('稳态请求'))).toBe(true);
    expect(save().disabled).toBe(true);
    const errs = validateThrottleBudget('qoder', { ...QODER, steadyRpm: 999 });
    expect(errs.some((e) => e.key === 'sources.qoder.throttle.steady_rpm' && e.code === 'range')).toBe(true);
  });

  it('突发低于稳态 / 间隔下界大于上界 → 各自报错', () => {
    const burst = validateThrottleBudget('qoder', { ...QODER, burstRpm: 1 });
    expect(burst.some((e) => e.key === 'sources.qoder.throttle.burst_rpm')).toBe(true);
    const gap = validateThrottleBudget('qoder', { ...QODER, minGapMs: [2000, 500] });
    expect(gap.some((e) => e.key === 'sources.qoder.throttle.min_gap_ms')).toBe(true);
  });

  it('非整数被拒绝（rpm 只允许整数）', () => {
    const errs = validateThrottleBudget('qoder', { ...QODER, steadyRpm: 4.5 });
    expect(errs[0]?.code).toBe('range');
    expect(errs[0]?.message).toContain('整数');
  });

  it('合规预算零报错', () => {
    expect(validateThrottleBudget('qoder', QODER)).toEqual([]);
    expect(validateThrottleBudget('trae', TRAE)).toEqual([]);
  });
});

describe('ThrottleForm — 按源有无决定渲染', () => {
  it('Trae 无 halve_budget_after_429 / jitter → 两列不渲染', () => {
    setup('trae', TRAE);
    const text = document.body.textContent ?? '';
    expect(text).not.toContain(THROTTLE.HALVE);
    expect(text).not.toContain('签到抖动');
  });

  it('Qoder 两列都在', () => {
    setup();
    const text = document.body.textContent ?? '';
    expect(text).toContain(THROTTLE.HALVE);
    expect(text).toContain('签到抖动');
  });

  it('min_gap_ms 两个输入框对应 [lo, hi]', () => {
    const { onSave } = setup();
    fireEvent.change(screen.getByLabelText(`最小间隔 · ${THROTTLE.GAP_HI}`), { target: { value: '1500' } });
    fireEvent.click(save());
    expect(onSave.mock.calls[0][0]).toEqual({ minGapMs: [200, 1500] });
  });

  it('还原按钮丢弃未保存的草稿', () => {
    setup();
    fireEvent.change(steady(), { target: { value: '30' } });
    expect(steady().value).toBe('30');
    fireEvent.click(screen.getByRole('button', { name: THROTTLE.RESET }));
    expect(steady().value).toBe('4');
  });
});
