import { describe, it, expect, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import type { ThrottleBudget } from '../src/types/api-contract';
import { ConsentGate } from '../src/components/ConsentGate';
import { ThrottleForm } from '../src/components/ThrottleForm';
import { AdvancedToggle } from '../src/components/AdvancedToggle';

expect.extend(toHaveNoViolations);

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

afterEach(cleanup);

describe('axe 无障碍基线（§5.19 滚动容器 / §5.20 表格 / 开关）', () => {
  it('ConsentGate 无违规', async () => {
    const { container } = render(<ConsentGate source="qoder" noticeVersion="1" onAck={() => {}} onAbort={() => {}} />);
    expect(await axe(container)).toHaveNoViolations();
  });

  it('ThrottleForm 无违规', async () => {
    const { container } = render(<ThrottleForm source="qoder" budget={QODER} onSave={() => {}} />);
    expect(await axe(container)).toHaveNoViolations();
  });

  it('AdvancedToggle 关闭态无违规', async () => {
    const { container } = render(
      <AdvancedToggle
        source="qoder"
        flagKey="request_fingerprint_rewrite"
        enabled={false}
        confirmCopy="确认句"
        onToggle={() => {}}
      />,
    );
    expect(await axe(container)).toHaveNoViolations();
  });
});
