import { useState } from 'react';
import { ADVANCED, sourceDisplayName } from '../i18n/zh';
import { DangerAction } from './DangerAction';

/**
 * 请求特征改写开关（UI UE v0.2.4 §5.20 / G4 / F-V1）。
 * 落点唯一 = sources.<source>.advanced.request_fingerprint_rewrite；无全局闸、无批量开。
 */
export interface AdvancedToggleProps {
  source: string;
  flagKey: 'request_fingerprint_rewrite';
  enabled: boolean;
  confirmCopy: string;
  onToggle: (next: boolean) => void;
}

export function AdvancedToggle({ source, flagKey, enabled, confirmCopy, onToggle }: AdvancedToggleProps) {
  const [confirming, setConfirming] = useState(false);
  const impact = `${ADVANCED.IMPACT.replace('{source}', sourceDisplayName(source))}\n${confirmCopy}`;

  function handleClick() {
    if (enabled) {
      onToggle(false); // 关闭是安全方向：不确认、不批量
      return;
    }
    setConfirming(true); // 开启是风险方向：每个源各一次独立知情
  }

  return (
    <div className="adv-toggle" data-flag-path={`sources.${source}.advanced.${flagKey}`}>
      <div className="adv-toggle__row">
        <button
          type="button"
          role="switch"
          aria-checked={enabled}
          aria-label={`${ADVANCED.COLUMN} · ${sourceDisplayName(source)}`}
          className="adv-toggle__switch"
          onClick={handleClick}
        >
          <span className="adv-toggle__knob" />
        </button>
        <span className="adv-toggle__label">{ADVANCED.COLUMN}</span>
        <span className={`adv-toggle__state adv-toggle__state--${enabled ? 'on' : 'off'}`}>
          {enabled ? ADVANCED.ON : ADVANCED.OFF}
        </span>
      </div>
      <p className="adv-toggle__note">{ADVANCED.NOTE}</p>
      <p className="adv-toggle__note">{ADVANCED.SCOPE}</p>
      {confirming && (
        <DangerAction
          confirmWord={ADVANCED.CONFIRM_WORD}
          impactSummary={impact}
          onCancel={() => setConfirming(false)}
          onConfirm={() => {
            setConfirming(false);
            onToggle(true);
          }}
        />
      )}
    </div>
  );
}
