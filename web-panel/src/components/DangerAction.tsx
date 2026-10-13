import { useEffect, useRef, useState } from 'react';
import { DANGER } from '../i18n/zh';

/**
 * 危险操作二次确认（UI UE v0.2.5 §5.13 增补后口径）。
 * onCancel 已由 UI UE 裁定进契约面：所有非肯定关闭路径统一走它，取消零副作用；
 * 保持可选，但不传时组件必须自持关闭能力（仍能关、仍零写回）。
 */
export interface DangerActionProps {
  confirmWord: string;
  impactSummary: string;
  onConfirm: () => void;
  onCancel?: () => void;
}

export function DangerAction({ confirmWord, impactSummary, onConfirm, onCancel }: DangerActionProps) {
  const [typed, setTyped] = useState('');
  const [dismissed, setDismissed] = useState(false);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const cancelRef = useRef<HTMLButtonElement | null>(null);
  const returnFocusRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    returnFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    cancelRef.current?.focus();
    return () => {
      // §5.13 增补第 6 条：关闭后焦点回到触发该弹窗的控件
      const target = returnFocusRef.current;
      if (target && target.isConnected) target.focus();
    };
  }, []);

  /** 非肯定关闭的统一出口：有 onCancel 就走它，没有就自持关闭，两条路径都零写回。 */
  const close = () => {
    setTyped('');
    if (onCancel) onCancel();
    else setDismissed(true);
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
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

  if (dismissed) return null;

  const armed = typed === confirmWord;

  return (
    <div
      className="danger-action__scrim"
      onMouseDown={(e) => {
        // 遮罩点击属非肯定关闭路径，与「保持关闭」同一出口
        if (e.target === e.currentTarget) close();
      }}
    >
      <div
        ref={panelRef}
        className="danger-action"
        role="dialog"
        aria-modal="true"
        aria-labelledby="danger-action-title"
        onKeyDown={handleKeyDown}
      >
        <h3 id="danger-action-title" className="danger-action__title">
          {DANGER.TITLE}
        </h3>
        <p className="danger-action__impact">{impactSummary}</p>
        <label className="danger-action__type">
          <span>
            {DANGER.TYPE_HINT_PREFIX}
            <code className="danger-action__word">{confirmWord}</code>
            {DANGER.TYPE_HINT_SUFFIX}
          </span>
          <input
            type="text"
            name="danger-confirm-word"
            autoComplete="off"
            spellCheck={false}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
          />
        </label>
        <div className="danger-action__actions">
          <button ref={cancelRef} type="button" className="btn" onClick={close}>
            {DANGER.CANCEL}
          </button>
          <button
            type="button"
            className="btn btn--danger-outline"
            disabled={!armed}
            onClick={() => {
              setTyped('');
              onConfirm();
            }}
          >
            {DANGER.CONFIRM}
          </button>
        </div>
      </div>
    </div>
  );
}
