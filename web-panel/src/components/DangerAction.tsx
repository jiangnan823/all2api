import { useEffect, useRef, useState } from 'react';
import { DANGER } from '../i18n/zh';

/**
 * 危险操作二次确认（UI UE v0.2.4 §5.13）。
 * 三个 props 为冻结项；onCancel 为 §5.13 未声明的关闭回调缺口，可选、不改变冻结字段语义（已登记待 UI UE 复核）。
 */
export interface DangerActionProps {
  confirmWord: string;
  impactSummary: string;
  onConfirm: () => void;
  onCancel?: () => void;
}

export function DangerAction({ confirmWord, impactSummary, onConfirm, onCancel }: DangerActionProps) {
  const [typed, setTyped] = useState('');
  const cancelRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    cancelRef.current?.focus();
  }, []);

  const armed = typed === confirmWord;

  return (
    <div
      className="danger-action"
      role="dialog"
      aria-modal="true"
      aria-labelledby="danger-action-title"
      onKeyDown={(e) => {
        if (e.key === 'Escape' && !armed) onCancel?.();
      }}
    >
      <h3 id="danger-action-title" className="danger-action__title">{DANGER.TITLE}</h3>
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
        <button ref={cancelRef} type="button" className="btn" onClick={() => onCancel?.()}>
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
  );
}
