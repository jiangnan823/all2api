import { useMemo, useState } from 'react';
import type { FieldError, ThrottleBudget } from '../types/api-contract';
import { THROTTLE, reasonCopy, sourceDisplayName } from '../i18n/zh';
import { editableThrottleFields, throttlePath, validateThrottleBudget } from '../lib/throttle';

/**
 * 按源节流表单（UI UE v0.2.4 §5.20；G1 单位硬门）。
 * 本文件禁止出现任何 ×60 / ÷60 换算：界面值与 steady_rpm / burst_rpm 是同一个数。
 */
export interface ThrottleFormProps {
  source: string;
  budget: ThrottleBudget;
  onSave: (patch: Partial<ThrottleBudget>) => void;
}

function clone(b: ThrottleBudget): ThrottleBudget {
  return { ...b, minGapMs: [b.minGapMs[0], b.minGapMs[1]], backoff: { ...b.backoff } };
}

function diff(a: ThrottleBudget, b: ThrottleBudget): Partial<ThrottleBudget> {
  const patch: Partial<ThrottleBudget> = {};
  if (a.steadyRpm !== b.steadyRpm) patch.steadyRpm = b.steadyRpm;
  if (a.burstRpm !== b.burstRpm) patch.burstRpm = b.burstRpm;
  if (a.minGapMs[0] !== b.minGapMs[0] || a.minGapMs[1] !== b.minGapMs[1]) patch.minGapMs = [b.minGapMs[0], b.minGapMs[1]];
  if (a.dailyCap !== b.dailyCap) patch.dailyCap = b.dailyCap;
  if (a.inflight !== b.inflight) patch.inflight = b.inflight;
  if (a.backoff.base429Sec !== b.backoff.base429Sec || a.backoff.notFoundSec !== b.backoff.notFoundSec) {
    patch.backoff = { ...b.backoff };
  }
  if (a.jitterMinutes !== b.jitterMinutes) patch.jitterMinutes = b.jitterMinutes;
  if (a.halveBudgetAfter429 !== b.halveBudgetAfter429) patch.halveBudgetAfter429 = b.halveBudgetAfter429;
  return patch;
}

type Slot = 'steady_rpm' | 'burst_rpm' | 'daily_cap' | 'inflight' | 'jitter_minutes'
  | 'backoff.base_429_sec' | 'backoff.not_found_sec' | 'min_gap_ms.lo' | 'min_gap_ms.hi';

export function ThrottleForm({ source, budget, onSave }: ThrottleFormProps) {
  const [draft, setDraft] = useState<ThrottleBudget>(() => clone(budget));
  const [errors, setErrors] = useState<FieldError[]>([]);
  const fields = useMemo(() => editableThrottleFields(source), [source]);

  /** 空输入记为 NaN 而不是 0：min=0 的字段（min_gap_ms / jitter_minutes）若转 0 会被"合法"放行（F-5）。 */
  const toInput = (v: number) => (Number.isFinite(v) ? String(v) : '');

  function setNumber(slot: Slot, raw: string) {
    const n = raw === '' ? NaN : Number(raw);
    setErrors([]);
    setDraft((d) => {
      switch (slot) {
        case 'steady_rpm': return { ...d, steadyRpm: n };
        case 'burst_rpm': return { ...d, burstRpm: n };
        case 'daily_cap': return { ...d, dailyCap: n };
        case 'inflight': return { ...d, inflight: n };
        case 'jitter_minutes': return { ...d, jitterMinutes: n };
        case 'backoff.base_429_sec': return { ...d, backoff: { ...d.backoff, base429Sec: n } };
        case 'backoff.not_found_sec': return { ...d, backoff: { ...d.backoff, notFoundSec: n } };
        case 'min_gap_ms.lo': return { ...d, minGapMs: [n, d.minGapMs[1]] };
        case 'min_gap_ms.hi': return { ...d, minGapMs: [d.minGapMs[0], n] };
      }
    });
  }

  function handleSave() {
    const found = validateThrottleBudget(source, draft);
    setErrors(found);
    if (found.length > 0) return;
    const patch = diff(budget, draft);
    if (Object.keys(patch).length > 0) onSave(patch);
  }

  const exhaust = reasonCopy('quota_exhausted');
  const errorFor = (key: string) => errors.filter((e) => e.key === key).map((e) => e.message).join('；');

  return (
    <form
      className="throttle-form"
      onSubmit={(e) => {
        e.preventDefault();
        handleSave();
      }}
    >
      <h3 className="throttle-form__title">{THROTTLE.TITLE} · {sourceDisplayName(source)}</h3>
      <p className="throttle-form__sub">{THROTTLE.SUBTITLE}</p>

      <div className="throttle-form__scroll">
      <table className="throttle-form__table">
        <thead>
          <tr>
            <th scope="col">{THROTTLE.COL_FIELD}</th>
            <th scope="col">{THROTTLE.COL_VALUE}</th>
            <th scope="col">{THROTTLE.COL_UNIT}</th>
            <th scope="col">{THROTTLE.COL_RANGE}</th>
            <th scope="col">{THROTTLE.COL_ERROR}</th>
          </tr>
        </thead>
        <tbody>
          {fields.map((f) => {
            const path = throttlePath(source, f.key);
            const err = errorFor(path);
            if (f.key === 'min_gap_ms') {
              return (
                <tr key={f.key} data-path={path}>
                  <th scope="row">{f.label}</th>
                  <td className="throttle-form__pair">
                    <input
                      type="number" step="1" inputMode="numeric"
                      name={`${path}.0`}
                      aria-label={`${f.label} · ${THROTTLE.GAP_LO}`}
                      value={toInput(draft.minGapMs[0])}
                      onChange={(e) => setNumber('min_gap_ms.lo', e.target.value)}
                    />
                    <input
                      type="number" step="1" inputMode="numeric"
                      name={`${path}.1`}
                      aria-label={`${f.label} · ${THROTTLE.GAP_HI}`}
                      value={toInput(draft.minGapMs[1])}
                      onChange={(e) => setNumber('min_gap_ms.hi', e.target.value)}
                    />
                  </td>
                  <td>{f.unit}</td>
                  <td>{f.min}–{f.max}</td>
                  <td className="throttle-form__err" role={err ? 'alert' : undefined}>{err}</td>
                </tr>
              );
            }
            const value =
              f.key === 'steady_rpm' ? draft.steadyRpm
              : f.key === 'burst_rpm' ? draft.burstRpm
              : f.key === 'daily_cap' ? draft.dailyCap
              : f.key === 'inflight' ? draft.inflight
              : f.key === 'jitter_minutes' ? (draft.jitterMinutes ?? 15)
              : f.key === 'backoff.base_429_sec' ? draft.backoff.base429Sec
              : draft.backoff.notFoundSec;
            return (
              <tr key={f.key} data-path={path}>
                <th scope="row">{f.label}</th>
                <td>
                  <input
                    type="number" step="1" inputMode="numeric"
                    name={path}
                    aria-label={f.label}
                    value={toInput(value)}
                    onChange={(e) => setNumber(f.key as Slot, e.target.value)}
                  />
                </td>
                <td>{f.unit}</td>
                <td>{f.min}–{f.max}</td>
                <td className="throttle-form__err" role={err ? 'alert' : undefined}>{err}</td>
              </tr>
            );
          })}

          {draft.halveBudgetAfter429 !== undefined && (
            <tr data-path={throttlePath(source, 'halve_budget_after_429')}>
              <th scope="row">{THROTTLE.HALVE}</th>
              <td>
                <input
                  type="checkbox"
                  name={throttlePath(source, 'halve_budget_after_429')}
                  aria-label={THROTTLE.HALVE}
                  checked={draft.halveBudgetAfter429}
                  onChange={(e) => {
                    setErrors([]);
                    setDraft((d) => ({ ...d, halveBudgetAfter429: e.target.checked }));
                  }}
                />
              </td>
              <td>{THROTTLE.DASH}</td>
              <td>{THROTTLE.DASH}</td>
              <td />
            </tr>
          )}

          <tr className="throttle-form__readonly" data-path={throttlePath(source, 'backoff.exhaust_402_until')}>
            <th scope="row">{THROTTLE.EXHAUST_COLUMN}</th>
            <td>
              {/* 只读锚点：无输入框——给了用户会以为能改平台重置时刻 */}
              <span className="throttle-form__reason">{exhaust.label}</span>
            </td>
            <td>{THROTTLE.READONLY}</td>
            <td>{THROTTLE.DASH}</td>
            <td>{THROTTLE.EXHAUST_READ_ONLY_NOTE}</td>
          </tr>
        </tbody>
      </table>
      </div>

      <div className="throttle-form__actions">
        <button type="submit" className="btn btn--primary" disabled={errors.length > 0}>
          {THROTTLE.SAVE}
        </button>
        <button
          type="button"
          className="btn"
          onClick={() => {
            setDraft(clone(budget));
            setErrors([]);
          }}
        >
          {THROTTLE.RESET}
        </button>
      </div>
      <p className="throttle-form__footer">{THROTTLE.FOOTER}</p>
    </form>
  );
}
