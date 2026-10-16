import { useState } from 'react';
import { batchOp, type BatchOpFailure, type BatchOpKind, type BatchOpResult } from '../lib/poolApi';

/**
 * 批量操作按钮组（M10-C，wb2api「账号池卡头」同款）：全部签到 / 旅行巡检 / 活跃上报 / 全部保活。
 * travel 仅 workbuddy 上游有语义 → 只在 workbuddy 渲染，其余平台三键。
 * POST /api/accounts/{platform}/batch-op body {"op":...} → {requested,done,skipped,failed:[{id,error}]}；
 * busy 期间全部按钮禁用；完成后回调 onDone 重拉快照；结果行内展示，
 * 失败逐账号可折叠（details/summary），{error} 原文不翻译不包装。
 *
 * 文案为组件内联字典（zh.ts 本批冻结，不改动）；按钮沿用 pool-act 紧凑样式。
 */

const COPY = {
  CHECKIN: '全部签到',
  TRAVEL: '旅行巡检',
  ACTIVE: '活跃上报',
  KEEPALIVE: '全部保活',
  BUSY_SUFFIX: '…',
  SUMMARY: (requested: number, done: number, skipped: number, failed: number): string =>
    `已执行 ${requested} 个账号：成功 ${done} · 跳过 ${skipped} · 失败 ${failed}`,
  FAILED_TITLE: '失败明细',
} as const;

/** 平台 → 按钮集：workbuddy 四键（含 travel），其余三键。 */
function opsFor(platform: string): Array<{ op: BatchOpKind; label: string }> {
  const ops: Array<{ op: BatchOpKind; label: string }> = [
    { op: 'checkin', label: COPY.CHECKIN },
    { op: 'active', label: COPY.ACTIVE },
    { op: 'keepalive', label: COPY.KEEPALIVE },
  ];
  if (platform === 'workbuddy') ops.splice(1, 0, { op: 'travel', label: COPY.TRAVEL });
  return ops;
}

/** 失败明细行：短码 + {error} 原文。 */
function FailedItem(props: { failure: BatchOpFailure }) {
  const { failure } = props;
  const shortId = failure.id.length > 12 ? `${failure.id.slice(0, 8)}…${failure.id.slice(-4)}` : failure.id;
  return (
    <li className="pool-batch__fail-item">
      <code>{shortId}</code> {failure.error}
    </li>
  );
}

export function BatchActions(props: { platform: string; onDone: () => void }) {
  const { platform, onDone } = props;
  const [busyOp, setBusyOp] = useState<BatchOpKind | null>(null);
  const [result, setResult] = useState<BatchOpResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  const run = async (op: BatchOpKind) => {
    setBusyOp(op);
    setError(null);
    setResult(null);
    try {
      const res = await batchOp(platform, op);
      setResult(res);
      onDone(); // 以服务端快照为准重建表格（签到/保活可能改状态与积分）
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusyOp(null);
    }
  };

  return (
    <div className="pool-batch">
      <div className="pool-batch__row" role="group" aria-label="批量操作">
        {opsFor(platform).map(({ op, label }) => (
          <button
            key={op}
            type="button"
            className="pool-act"
            disabled={busyOp !== null}
            aria-busy={busyOp === op}
            onClick={() => void run(op)}
          >
            {busyOp === op ? `${label}${COPY.BUSY_SUFFIX}` : label}
          </button>
        ))}
      </div>
      {error !== null && (
        <p className="pool-batch__error" role="alert">
          {error}
        </p>
      )}
      {result !== null && (
        <div className="pool-batch__result">
          <p className="pool-batch__summary" role="status">
            {COPY.SUMMARY(result.requested, result.done, result.skipped, result.failed.length)}
          </p>
          {result.failed.length > 0 && (
            <details className="pool-batch__fails">
              <summary>
                {COPY.FAILED_TITLE}（{result.failed.length}）
              </summary>
              <ul>
                {result.failed.map((f) => (
                  <FailedItem key={f.id} failure={f} />
                ))}
              </ul>
            </details>
          )}
        </div>
      )}
    </div>
  );
}
