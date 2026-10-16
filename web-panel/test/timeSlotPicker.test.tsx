import { useState } from 'react';
import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { TimeSlotPicker } from '../src/components';
import { CRON_FIELD } from '../src/i18n/zh';

/**
 * 共享 24 时点选择器（M10-A 自 SettingsScreen CronHoursField 抽出）门禁：
 * 每日 `0 h,h * * *` 形态 → 24 勾选框（勾选即时换算回 cron）；非每日形态 / 空值 →
 * 原始表达式文本框回落（带可访问名，axe label 规则）；最后一个时点不可取消（不产生非法空表达式）。
 * 消费方 = SettingsScreen（通用组 cron 字段）与 PlatformTasksCard（平台任务调度行）。
 */
expect.extend(toHaveNoViolations);

afterEach(cleanup);

/** 受控组件需要宿主回填 value：harness 持草稿，onSeen 旁路记录每次 onChange。 */
function Harness(props: { initial: string; onSeen?: (expr: string) => void }) {
  const [value, setValue] = useState(props.initial);
  return (
    <TimeSlotPicker
      name="k"
      value={value}
      onChange={(expr) => {
        props.onSeen?.(expr);
        setValue(expr);
      }}
    />
  );
}

describe('TimeSlotPicker — 每日形态（勾选换算）', () => {
  it('既有表达式解析为勾选（09、21 on，其余 off），24 个时点齐全', () => {
    render(<TimeSlotPicker name="k" value="0 9,21 * * *" onChange={() => {}} />);
    const hours = [...document.querySelectorAll<HTMLInputElement>('input[name^="k:"]')];
    expect(hours).toHaveLength(24);
    const on = hours.filter((c) => c.checked).map((c) => c.name);
    expect(on).toEqual(['k:9', 'k:21']);
  });

  it('勾选变化即时换算回 cron（去 21 加 20 → 0 9,20）；最后一个时点不可取消（不产生空表达式）', () => {
    const seen: string[] = [];
    render(<Harness initial="0 9,21 * * *" onSeen={(e) => seen.push(e)} />);
    fireEvent.click(screen.getByRole('checkbox', { name: '21' })); // → 0 9
    fireEvent.click(screen.getByRole('checkbox', { name: '20' })); // → 0 9,20
    fireEvent.click(screen.getByRole('checkbox', { name: '20' })); // → 0 9
    fireEvent.click(screen.getByRole('checkbox', { name: '09' })); // 最后一个时点 → 拦截
    expect(seen).toEqual(['0 9 * * *', '0 9,20 * * *', '0 9 * * *']);
    expect((screen.getByRole('checkbox', { name: '09' }) as HTMLInputElement).checked).toBe(true);
  });

  it('勾选组带可访问名；axe 无违规', async () => {
    const { container } = render(<TimeSlotPicker name="k" value="0 9 * * *" onChange={() => {}} />);
    expect(screen.getByRole('group', { name: CRON_FIELD.HOURS_LABEL })).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe('TimeSlotPicker — 回落（非每日形态 / 空值）', () => {
  it('分钟非 0 / 通配形态 → 原始表达式文本框 + 回落说明，可访问名齐备', async () => {
    const { container } = render(<TimeSlotPicker name="k" value="30 8 * * *" onChange={() => {}} />);
    const input = screen.getByLabelText(CRON_FIELD.FALLBACK_LABEL) as HTMLInputElement;
    expect(input.value).toBe('30 8 * * *');
    expect(document.body.textContent).toContain(CRON_FIELD.FALLBACK_EDITED);
    expect(document.querySelector('input[name^="k:"]')).toBeNull();
    expect(await axe(container)).toHaveNoViolations();
  });

  it('空值 → 回落文本框（FALLBACK_SHAPE 分支）；输入每日形态后自动切回时点选择器', () => {
    render(<Harness initial="" />);
    expect(screen.getByLabelText(CRON_FIELD.FALLBACK_LABEL)).toBeTruthy();
    expect(document.body.textContent).toContain(CRON_FIELD.FALLBACK_SHAPE);
    fireEvent.change(screen.getByLabelText(CRON_FIELD.FALLBACK_LABEL), { target: { value: '0 6 * * *' } });
    // 值变每日形态即回选择器（回落是“当前值的形状”决定的展示分支）
    expect(screen.queryByLabelText(CRON_FIELD.FALLBACK_LABEL)).toBeNull();
    expect((screen.getByRole('checkbox', { name: '06' }) as HTMLInputElement).checked).toBe(true);
  });
});
