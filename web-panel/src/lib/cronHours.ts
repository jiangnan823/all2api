import { useState } from 'react';

/**
 * cron 五段表达式 ⇆ 24 小时制时点选择的换算（v1.2 主人裁定：用户不该面对 cron 语法）。
 *
 * 覆盖域：本仓三个调度字段的实际形态——`m h * * *`（分钟固定 0，小时列表）。
 * 超出域的表达式（分钟非 0、通配符、步进、域字段）如实返回 null，由调用方回落
 * 到原生文本框——不做有损猜测。
 */
export function parseDailyHours(expr: string): number[] | null {
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return null;
  const [min, hour, dom, mon, dow] = parts;
  if (min !== '0' || dom !== '*' || mon !== '*' || dow !== '*') return null;
  if (!/^\d{1,2}(,\d{1,2})*$/.test(hour)) return null;
  const hours = hour.split(',').map((h) => Number(h));
  if (hours.some((h) => !Number.isInteger(h) || h < 0 || h > 23)) return null;
  return [...new Set(hours)].sort((a, b) => a - b);
}

/** 时点列表 → cron 表达式（分钟固定 0）。空列表 = 非法（调用方拦截保存）。 */
export function buildDailyCron(hours: number[]): string {
  const uniq = [...new Set(hours)].sort((a, b) => a - b);
  return `0 ${uniq.join(',')} * * *`;
}

/** 单字段编辑态：表达式解析失败时保留文本框（回落），解析成功给时点选择器。 */
export interface DailyHoursState {
  /** null = 非每日形态，走文本框回落 */
  hours: number[] | null;
  raw: string;
}

export function useDailyHours(initial: string): DailyHoursState & { setHours: (h: number[]) => void; setRaw: (r: string) => void } {
  const [state, setState] = useState<DailyHoursState>(() => {
    const hours = parseDailyHours(initial);
    return { hours, raw: hours === null ? initial : '' };
  });
  return {
    ...state,
    setHours(h) {
      setState({ hours: h, raw: '' });
    },
    setRaw(r) {
      setState({ hours: parseDailyHours(r), raw: r });
    },
  };
}
