import { useCallback, useEffect, useRef, useState } from 'react';
import { GROWTH, settingsLine } from '../i18n/zh';
import {
  acceptAll,
  claimAuto,
  fetchGrowthTasks,
  type GrowthTasksSnapshot,
} from '../lib/growthApi';

/**
 * 成长任务弹层（M7b，wb2api「积分任务」同款布局）：
 * 遮罩 + 居中卡片（标题「积分任务 · {昵称或 id 短码}」/ 说明行 / 四列表格 任务·进度·奖励·状态 /
 * 底部 全部接受 · 一键完成可自动任务(主色) · 重新查询 · 关闭）。
 * - 状态徽标映射：已领取(绿)/可领取(主色)/已完成(蓝)/进行中·已报名·未报名(灰)；未知值原样显示。
 * - 接受/领取成功后重拉清单（以服务端回执为准），成功提示带计数；{error} 信封原文逐字展示。
 * - 加载/错误+重试/空态（tasks 空 = 该账号暂无成长任务）。
 * - Esc / 遮罩点击关闭；打开期间 body 滚动锁定；关闭后焦点回到触发按钮。
 * 全部内存态，不落 storage。
 */
export interface GrowthTasksModalProps {
  platform: string;
  id: string;
  /** 账号昵称（标题主显）；缺失回落 id 短码。 */
  nickname: string | null;
  onClose: () => void;
}

type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; data: GrowthTasksSnapshot };

/** 昵称 → 短码副行：前 8 位…后 4 位（≤12 位原样）——与账号池表格同一规则。 */
function shortId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id;
}

/** 状态串 → 中文徽标文案（未知值原样显示；空串显示 —，不编造状态）。 */
const STATUS_COPY: Record<string, string> = {
  claimed: GROWTH.STATUS_CLAIMED,
  claimable: GROWTH.STATUS_CLAIMABLE,
  completed: GROWTH.STATUS_COMPLETED,
  in_progress: GROWTH.STATUS_IN_PROGRESS,
  accepted: GROWTH.STATUS_ACCEPTED,
  not_accepted: GROWTH.STATUS_NOT_ACCEPTED,
};

function growthStatusLabel(status: string): string {
  if (status === '') return GROWTH.DASH;
  return STATUS_COPY[status] ?? status;
}

/**
 * 状态串 → 徽色档：claimed=绿(ok)/claimable=主色(brand)/completed=蓝(info)，
 * in_progress、accepted、not_accepted 及未知值一律灰(idle)，不夸大。
 */
function growthStatusTone(status: string): 'ok' | 'brand' | 'info' | 'idle' {
  if (status === 'claimed') return 'ok';
  if (status === 'claimable') return 'brand';
  if (status === 'completed') return 'info';
  return 'idle';
}

const TONE_CLASS: Record<'ok' | 'brand' | 'info' | 'idle', string> = {
  ok: 'pool-badge--ok',
  brand: 'growth-status--brand',
  info: 'growth-status--info',
  idle: 'pool-badge--idle',
};

/** 进度格：done/total 齐备 → "1/1"；任一缺失 → —（不编造读数）。 */
function progressLabel(done: number | null, total: number | null): string {
  return done !== null && total !== null ? `${done}/${total}` : GROWTH.DASH;
}

/** 奖励格："+100 分 +5 能"；单项缺失只显示另一项；两项全缺 → —。 */
function rewardLabel(credit: number | null, energy: number | null): string {
  const parts: string[] = [];
  if (credit !== null) parts.push(settingsLine(GROWTH.REWARD_CREDIT, { n: credit }));
  if (energy !== null) parts.push(settingsLine(GROWTH.REWARD_ENERGY, { n: energy }));
  return parts.length > 0 ? parts.join(' ') : GROWTH.DASH;
}

export function GrowthTasksModal({ platform, id, nickname, onClose }: GrowthTasksModalProps) {
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
  const [actionError, setActionError] = useState<string | null>(null);
  const [okMsg, setOkMsg] = useState<string | null>(null);
  const [busy, setBusy] = useState<'accept' | 'claim' | null>(null);
  const seqRef = useRef(0);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const closeRef = useRef<HTMLButtonElement | null>(null);
  const returnFocusRef = useRef<HTMLElement | null>(null);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    setState({ phase: 'loading' });
    try {
      const data = await fetchGrowthTasks(platform, id);
      if (seq === seqRef.current) setState({ phase: 'ready', data });
    } catch (err) {
      if (seq === seqRef.current) {
        setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
      }
    }
  }, [platform, id]);

  useEffect(() => {
    returnFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    closeRef.current?.focus();
    void load();
    // 打开期间锁 body 滚动（wb2api 同款观感），卸载时恢复
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.body.style.overflow = prevOverflow;
      const target = returnFocusRef.current;
      if (target && target.isConnected) target.focus();
    };
  }, [load]);

  const runAction = useCallback(
    async (action: 'accept' | 'claim') => {
      setBusy(action);
      setActionError(null);
      try {
        if (action === 'accept') {
          const r = await acceptAll(platform, id);
          setOkMsg(settingsLine(GROWTH.ACCEPT_OK, { n: r.accepted }));
        } else {
          const r = await claimAuto(platform, id);
          setOkMsg(settingsLine(GROWTH.CLAIM_OK, { n: r.creditGained }));
        }
        await load(); // 以服务端回执为准重建清单（领取的奖励/状态变化立即可见）
      } catch (err) {
        setActionError(`${GROWTH.ACTION_ERR_PREFIX}${err instanceof Error ? err.message : String(err)}`);
      } finally {
        setBusy(null);
      }
    },
    [platform, id, load],
  );

  /** Esc / 遮罩点击属关闭路径；Tab 在面板内兜圈（焦点陷阱）。 */
  const handleKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
      return;
    }
    if (e.key !== 'Tab' || !panelRef.current) return;
    const focusable = panelRef.current.querySelectorAll<HTMLElement>(
      'button, input, [href], select, textarea, [tabindex]:not([tabindex="-1"])',
    );
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    const active = document.activeElement;
    if (e.shiftKey && (active === first || !panelRef.current.contains(active))) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    }
  };

  const refresh = () => {
    setActionError(null);
    setOkMsg(null);
    void load();
  };

  const displayName = nickname ?? shortId(id);

  return (
    <div
      className="growth-scrim"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={panelRef}
        className="growth-modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="growth-modal-title"
        onKeyDown={handleKeyDown}
      >
        <h3 id="growth-modal-title" className="growth-modal__title">
          {settingsLine(GROWTH.TITLE, { name: displayName })}
        </h3>
        <p className="growth-modal__note">{GROWTH.NOTE}</p>

        {state.phase === 'loading' && (
          <p className="pool-note" role="status">
            {GROWTH.LOADING}
          </p>
        )}

        {state.phase === 'error' && (
          <div className="pool-error" role="alert">
            <h4 className="pool-error__title">{GROWTH.ERROR_TITLE}</h4>
            <p className="pool-error__detail">{state.message}</p>
            <button type="button" className="btn" onClick={() => void load()}>
              {GROWTH.RETRY}
            </button>
          </div>
        )}

        {state.phase === 'ready' &&
          (state.data.tasks.length === 0 ? (
            <p className="growth-modal__empty">{GROWTH.EMPTY}</p>
          ) : (
            <div className="growth-modal__scroll">
              <table className="growth-table">
                <thead>
                  <tr>
                    <th scope="col">{GROWTH.COL_TASK}</th>
                    <th scope="col">{GROWTH.COL_PROGRESS}</th>
                    <th scope="col">{GROWTH.COL_REWARD}</th>
                    <th scope="col">{GROWTH.COL_STATUS}</th>
                  </tr>
                </thead>
                <tbody>
                  {state.data.tasks.map((t) => {
                    const tone = growthStatusTone(t.status);
                    return (
                      <tr key={t.code}>
                        <td>
                          <code className="growth-code">{t.code}</code>
                          {t.title}
                        </td>
                        <td className="growth-table__num">{progressLabel(t.progressDone, t.progressTotal)}</td>
                        <td className="growth-table__num">{rewardLabel(t.rewardCredit, t.rewardEnergy)}</td>
                        <td>
                          <span className={`pool-badge ${TONE_CLASS[tone]}`}>{growthStatusLabel(t.status)}</span>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          ))}

        {okMsg !== null && (
          <p className="growth-modal__msg growth-modal__msg--ok" role="status">
            {okMsg}
          </p>
        )}
        {actionError !== null && (
          <p className="growth-modal__msg growth-modal__msg--err" role="alert">
            {actionError}
          </p>
        )}

        <div className="growth-modal__actions">
          <button type="button" className="btn" disabled={busy !== null} onClick={() => void runAction('accept')}>
            {GROWTH.ACCEPT_ALL}
          </button>
          <button
            type="button"
            className="btn btn--primary"
            disabled={busy !== null}
            onClick={() => void runAction('claim')}
          >
            {GROWTH.CLAIM_AUTO}
          </button>
          <button type="button" className="btn" disabled={busy !== null} onClick={refresh}>
            {GROWTH.REFRESH}
          </button>
          <button
            ref={closeRef}
            type="button"
            className="btn"
            onClick={onClose}
          >
            {GROWTH.CLOSE}
          </button>
        </div>
      </div>
    </div>
  );
}
