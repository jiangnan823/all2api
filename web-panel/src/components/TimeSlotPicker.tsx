import { CRON_FIELD } from '../i18n/zh';
import { buildDailyCron, parseDailyHours } from '../lib/cronHours';

/**
 * 24 小时制时点选择器（共享组件，M10-A 自 SettingsScreen 的 CronHoursField 抽出）。
 * v1.2 主人裁定"用户不该面对 cron 语法"：0–23 整点勾选（如 9,21），勾选变化即时换算
 * 回每日 cron 表达式落草稿（后端契约不变，看到的仍是 cron 值）。
 * 挂载时解析既有表达式——每日 `0 h,h * * *` 形态给选择器，其他形态（分钟非 0/通配/步进）
 * 如实回落到原生文本框并标注原因，不做有损猜测；未勾选任何时点时变化被拦截
 * （至少保留一个时点），不产生非法空表达式。
 * 消费方：SettingsScreen（通用配置组 cron 字段）与 PlatformTasksCard（平台任务调度行）。
 * 样式沿用既有 .cron-field BEM 块（panel.css），两处观感一致。
 */
export interface TimeSlotPickerProps {
  /** 输入框 name 前缀（时点勾选框为 `${name}:${hour}`，与既有测试/样式约定一致）。 */
  name: string;
  /** 当前 cron 表达式（受控值）。 */
  value: string;
  /** 表达式变化回调（勾选换算或回落文本框输入）。 */
  onChange: (expr: string) => void;
}

export function TimeSlotPicker(props: TimeSlotPickerProps) {
  const parsed = parseDailyHours(props.value);
  // 回落是“当前值的形状”决定的展示分支，无需独立状态：值变每日形态即回选择器。
  const fallback = parsed === null && props.value !== '';

  if (fallback || parsed === null) {
    return (
      <span className="cron-field cron-field--fallback">
        <input
          type="text"
          name={props.name}
          value={props.value}
          aria-label={CRON_FIELD.FALLBACK_LABEL}
          onChange={(e) => props.onChange(e.target.value)}
        />
        <span className="settings-screen__generic-desc">
          {fallback ? CRON_FIELD.FALLBACK_EDITED : CRON_FIELD.FALLBACK_SHAPE}
        </span>
      </span>
    );
  }

  const toggle = (hour: number, on: boolean) => {
    const next = on ? [...parsed, hour] : parsed.filter((h) => h !== hour);
    if (next.length === 0) return; // 至少保留一个时点
    props.onChange(buildDailyCron(next));
  };

  return (
    <span className="cron-field">
      <span className="cron-field__hours" role="group" aria-label={CRON_FIELD.HOURS_LABEL}>
        {Array.from({ length: 24 }, (_, h) => (
          <label key={h} className={`cron-field__hour${parsed.includes(h) ? ' cron-field__hour--on' : ''}`}>
            <input
              type="checkbox"
              name={`${props.name}:${h}`}
              checked={parsed.includes(h)}
              onChange={(e) => toggle(h, e.target.checked)}
            />
            {String(h).padStart(2, '0')}
          </label>
        ))}
      </span>
      <span className="settings-screen__generic-desc">{CRON_FIELD.HOURS_HINT}</span>
    </span>
  );
}
