import type { AccountUsage } from '../lib/usageApi';

/**
 * 用量徽章组（M10-C，wb2api「用量」列同款）：四小徽章 = 次数 / tok / 秒 / tok每秒，
 * 形如 2698次 · 463.6mtok · 49s · 40.6tok/s。缺失指标以 "—" 占位（不造 0）。
 * 数据源 GET /api/usage accounts 桶（按账号 ShortID join），快照未拉到时 usage 为 null → 全 "—"。
 *
 * 文案为组件内联字典（zh.ts 本批冻结，不改动）；徽章样式类沿用 pool-* BEM 前缀。
 */

/** 万以下原样 tok，万级 ktok，百万级 mtok（wb2api 同款一位小数）。 */
export function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}mtok`;
  if (n >= 10_000) return `${(n / 1_000).toFixed(1)}ktok`;
  return `${Math.round(n)}tok`;
}

/** 秒：<10s 保留一位小数（0.4s 不坍缩成 0s），其余取整。 */
export function fmtDuration(sec: number): string {
  return sec > 0 && sec < 10 ? `${Number(sec.toFixed(1))}s` : `${Math.round(sec)}s`;
}

/** tok/s：一位小数（40.6tok/s）。 */
export function fmtTokPerSec(n: number): string {
  return `${Number(n.toFixed(1))}tok/s`;
}

/** 可空指标 → 展示串；null/undefined（键缺失或该账号无记录）→ null（徽章显示 "—"）。 */
function opt(v: number | null | undefined, fmt: (n: number) => string): string | null {
  return typeof v === 'number' ? fmt(v) : null;
}

/** 单徽章：值缺失 → "—" 占位（配 --empty 弱化样式，仍保持四格对齐）。 */
function Badge(props: { text: string | null; title: string }) {
  const { text, title } = props;
  return (
    <span className={text === null ? 'pool-usage__badge pool-usage__badge--empty' : 'pool-usage__badge'} title={title}>
      {text ?? '—'}
    </span>
  );
}

export function UsageBadges(props: { usage: AccountUsage | null }) {
  const u = props.usage;
  return (
    <span className="pool-usage" aria-label="用量">
      <Badge text={opt(u?.requests, (n) => `${n.toLocaleString()}次`)} title="本次运行请求次数" />
      <Badge text={opt(u?.tokens, fmtTokens)} title="本次运行累计 token" />
      <Badge text={opt(u?.durationSec, fmtDuration)} title="本次运行累计耗时" />
      <Badge text={opt(u?.tokPerSec, fmtTokPerSec)} title="平均速率（tok/s）" />
    </span>
  );
}
