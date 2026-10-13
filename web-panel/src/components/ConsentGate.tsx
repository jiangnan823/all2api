import { useCallback, useEffect, useRef, useState } from 'react';
import type { ConsentAck } from '../types/api-contract';
import { CONSENT, consentAppendix, consentCheckLabel, sourceDisplayName } from '../i18n/zh';
import { echoTimeLocal } from '../lib/consent';

/**
 * 接入须知强制知情同意门（UI UE v0.2.4 §5.19；Q11；M1 验收第 6 条判定对象）。
 * 纯受控：不碰网络、不存凭证；留痕只出 source + noticeVersion（ackAt 仅即时回显，服务端落戳）。
 */
export interface ConsentGateProps {
  source: string;
  noticeVersion: string;
  scrollRequired?: boolean;
  onAck: (a: ConsentAck) => void;
  onAbort: () => void;
}

const SCROLL_EPSILON_PX = 8;

/** 滚动判定式冻结在 §5.19：scrollTop + clientHeight >= scrollHeight - 8。 */
export function isReadToBottom(el: { scrollTop: number; clientHeight: number; scrollHeight: number }): boolean {
  return el.scrollTop + el.clientHeight >= el.scrollHeight - SCROLL_EPSILON_PX;
}

function readPercent(el: { scrollTop: number; clientHeight: number; scrollHeight: number }): number {
  if (el.scrollHeight <= 0) return 100;
  return Math.min(100, Math.round(((el.scrollTop + el.clientHeight) / el.scrollHeight) * 100));
}

export function ConsentGate({ source, noticeVersion, scrollRequired = true, onAck, onAbort }: ConsentGateProps) {
  const regionRef = useRef<HTMLDivElement | null>(null);
  const [reached, setReached] = useState(!scrollRequired);
  const [percent, setPercent] = useState(scrollRequired ? 0 : 100);
  const [checked, setChecked] = useState(false);

  const measure = useCallback(() => {
    const el = regionRef.current;
    if (!el) return;
    // scrollHeight=0 表示尚未布局，不能据此判定"已读到底"
    const done = el.scrollHeight > 0
      && (el.scrollHeight <= el.clientHeight || isReadToBottom(el));
    setReached((prev) => prev || done);
    setPercent(readPercent(el));
  }, []);

  useEffect(() => {
    if (!scrollRequired) return;
    measure();
    const el = regionRef.current;
    if (!el || typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(() => measure());
    ro.observe(el);
    return () => ro.disconnect();
  }, [scrollRequired, measure]);

  const appendix = consentAppendix(source);
  const canCheck = reached;
  const canContinue = reached && checked;

  function handleAck() {
    if (!canContinue) return;
    onAck({ source, noticeVersion, ackAt: echoTimeLocal() });
  }

  return (
    <section className="consent-gate" data-notice-version={noticeVersion} aria-labelledby="consent-title">
      <header className="consent-gate__head">
        <h2 id="consent-title" className="consent-gate__title">{CONSENT.GENERAL_TITLE}</h2>
        <p className="consent-gate__note">{CONSENT.TOP_NOTE}</p>
      </header>

      <div
        ref={regionRef}
        className="consent-gate__scroll"
        tabIndex={0}
        role="region"
        aria-label={CONSENT.REGION_LABEL}
        onScroll={measure}
      >
        <ol className="consent-gate__list">
          {CONSENT.GENERAL_ITEMS.map((line) => <li key={line}>{line}</li>)}
        </ol>

        <h3 className="consent-gate__subtitle">{appendix.title}</h3>
        {appendix.items.length > 0 && (
          <ol className="consent-gate__list consent-gate__list--source" data-source={source}>
            {appendix.items.map((line) => <li key={line}>{line}</li>)}
          </ol>
        )}

        <h3 className="consent-gate__subtitle">{CONSENT.CLOSING_TITLE}</h3>
        <ol className="consent-gate__list">
          {CONSENT.CLOSING_ITEMS.map((line) => <li key={line}>{line}</li>)}
        </ol>

        <p className="consent-gate__version">{CONSENT.VERSION.replace('{noticeVersion}', noticeVersion)}</p>
      </div>

      <div className="consent-gate__progress">
        <div
          className="consent-gate__bar"
          role="progressbar"
          aria-label={CONSENT.PROGRESS_ARIA}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={percent}
        >
          <span className="consent-gate__bar-fill" style={{ width: `${percent}%` }} />
        </div>
        <p className="consent-gate__progress-text">
          {reached
            ? <span role="status" aria-live="polite">{CONSENT.REACHED}</span>
            : <span>{CONSENT.PROGRESS.replace('{percent}', String(percent))}</span>}
        </p>
      </div>

      {/* 留痕说明句必须在 label 关联内（PM seq=501 ①，缺陷级验收线）：
          脱离 label 时读屏聚焦勾选框只听到半句承诺，等于把 O-5 服务端留痕的边界藏在用户听不到的地方。 */}
      <label className="consent-gate__check">
        <input
          type="checkbox"
          name={`consent:${source}:${noticeVersion}`}
          checked={checked}
          disabled={!canCheck}
          onChange={(e) => setChecked(e.target.checked)}
        />
        <span>{consentCheckLabel(sourceDisplayName(source))}</span>
        <span className="consent-gate__check-note">{CONSENT.CHECK_NOTE}</span>
      </label>

      <footer className="consent-gate__actions">
        <button type="button" className="btn btn--abort" onClick={onAbort} disabled={false}>
          {CONSENT.ABORT}
        </button>
        <button
          type="button"
          className="btn btn--primary"
          onClick={handleAck}
          disabled={!canContinue}
        >
          {CONSENT.CONTINUE}
        </button>
      </footer>
      <p className="consent-gate__abort-hint">{CONSENT.ABORT_HINT}</p>
    </section>
  );
}
