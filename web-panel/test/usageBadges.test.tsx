import { describe, it, expect, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/react';
import { UsageBadges, fmtDuration, fmtTokPerSec, fmtTokens } from '../src/components/UsageBadges';
import { accountUsageFor, parseUsageSnapshot, type AccountUsage } from '../src/lib/usageApi';

/**
 * 用量徽章组门禁（M10-C）：四小徽章 = 次数/tok/秒/tok每秒（wb2api 同式），
 * 缺失指标 "—" 占位；fmt 家族边界（ktok/mtok 进位、<10s 一位小数）；
 * accountUsageFor 短码 join（全量 id 直配 → 前 8 位回落 → 无记录 null）。
 */

afterEach(cleanup);

describe('fmt 家族', () => {
  it('fmtTokens：<1万 tok / 万级 ktok / 百万级 mtok（一位小数）', () => {
    expect(fmtTokens(0)).toBe('0tok');
    expect(fmtTokens(2698)).toBe('2698tok');
    expect(fmtTokens(9999)).toBe('9999tok');
    expect(fmtTokens(10_000)).toBe('10.0ktok');
    expect(fmtTokens(463_600_000)).toBe('463.6mtok');
    expect(fmtTokens(1_234_567)).toBe('1.2mtok');
  });

  it('fmtDuration：<10s 一位小数不坍缩，其余取整', () => {
    expect(fmtDuration(0)).toBe('0s');
    expect(fmtDuration(0.4)).toBe('0.4s');
    expect(fmtDuration(9.96)).toBe('10s');
    expect(fmtDuration(49)).toBe('49s');
    expect(fmtDuration(3678)).toBe('3678s');
  });

  it('fmtTokPerSec：一位小数', () => {
    expect(fmtTokPerSec(40.6)).toBe('40.6tok/s');
    expect(fmtTokPerSec(40.44)).toBe('40.4tok/s');
    expect(fmtTokPerSec(0)).toBe('0tok/s');
  });
});

describe('UsageBadges 组件', () => {
  it('usage=null（快照未拉到）→ 四徽章全 "—"，带 --empty 弱化类', () => {
    render(<UsageBadges usage={null} />);
    const badges = Array.from(document.querySelectorAll('.pool-usage__badge'));
    expect(badges.map((b) => b.textContent)).toEqual(['—', '—', '—', '—']);
    for (const b of badges) expect(b.className).toContain('pool-usage__badge--empty');
  });

  it('全键在场 → 四徽章齐（wb2api 同式）', () => {
    const u: AccountUsage = { requests: 2698, tokens: 463_600_000, durationSec: 49, tokPerSec: 40.6 };
    render(<UsageBadges usage={u} />);
    expect(Array.from(document.querySelectorAll('.pool-usage__badge')).map((b) => b.textContent)).toEqual([
      '2,698次',
      '463.6mtok',
      '49s',
      '40.6tok/s',
    ]);
  });

  it('部分键缺失（wire 宽容）→ 缺的徽章 "—"，不造 0', () => {
    const u: AccountUsage = { requests: 12, tokens: null, durationSec: 3, tokPerSec: null };
    render(<UsageBadges usage={u} />);
    expect(Array.from(document.querySelectorAll('.pool-usage__badge')).map((b) => b.textContent)).toEqual([
      '12次',
      '—',
      '3s',
      '—',
    ]);
  });
});

describe('accountUsageFor 短码 join', () => {
  const snap = parseUsageSnapshot({
    accounts: {
      abcd1234: { requests: 1, tokens: 2, duration_sec: 3, tok_per_sec: 4 },
      'wb-01234': { requests: 9, tokens: 9, duration_sec: 9, tok_per_sec: 9 },
    },
  });

  it('全量 id 直配优先', () => {
    expect(accountUsageFor(snap, 'abcd1234')?.requests).toBe(1);
  });

  it('前 8 位短码回落（reqlog account_id 同口径）', () => {
    expect(accountUsageFor(snap, 'wb-0123456789abcdef')?.requests).toBe(9);
  });

  it('无记录 / 快照缺失 → null', () => {
    expect(accountUsageFor(snap, 'zzzzzzzz')).toBeNull();
    expect(accountUsageFor(null, 'abcd1234')).toBeNull();
  });
});
