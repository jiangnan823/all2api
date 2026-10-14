import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ServerTimeResponse, ThrottleBudget } from '../types/api-contract';
import { AdvancedToggle, ThrottleForm } from '../components';
import { fetchConfigGroups, fetchPanelConfig, fetchServerTime } from '../lib/panelApi';
import { buildSettingsModel, draftEquals, type SettingsModel, type SourceDraft } from '../lib/settingsModel';
import { setDeliveredThrottleFields } from '../lib/throttle';
import { SETTINGS, settingsLine, sourceDisplayName } from '../i18n/zh';

/**
 * 设置屏容器（M2 B2）：进入时并行拉 GET /api/config + GET /api/config/fields，
 * 按源装配 ThrottleForm（元数据驱动）与 AdvancedToggle（指纹开关，内含 DangerAction 确认）。
 * 如实边界：后端本里程碑为只读接口，无保存端点——保存按钮常置灰，dirty 草稿只留内存
 * （面板不落任何 storage，M1 安全红线），「放弃本地改动」即回到服务端基线。
 */

type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; model: SettingsModel };

export function SettingsScreen() {
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
  const [drafts, setDrafts] = useState<Record<string, SourceDraft> | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [time, setTime] = useState<ServerTimeResponse | null>(null);
  const [timeFailed, setTimeFailed] = useState(false);
  /** 草稿基线重置（放弃改动 / 重新加载）时自增，强制 ThrottleForm 丢弃内部草稿重新挂载。 */
  const [formEpoch, setFormEpoch] = useState(0);
  const seqRef = useRef(0);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    setState({ phase: 'loading' });
    setTime(null);
    setTimeFailed(false);
    try {
      const [config, groups] = await Promise.all([fetchPanelConfig(), fetchConfigGroups()]);
      if (seq !== seqRef.current) return;
      const model = buildSettingsModel(config, groups);
      // 元数据成为 ThrottleForm 的字段数据源（props/视觉不变，见 lib/throttle.ts 注册表注释）
      setDeliveredThrottleFields(model.throttleFields);
      setState({ phase: 'ready', model });
      setDrafts(model.server);
      setSelected((prev) => (prev && model.sources.includes(prev) ? prev : (model.sources[0] ?? null)));
      setFormEpoch((e) => e + 1);
    } catch (err) {
      if (seq !== seqRef.current) return;
      setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
      return;
    }
    // /api/time 是加分项：失败只降级角注，不影响配置展示
    try {
      const t = await fetchServerTime();
      if (seq === seqRef.current) setTime(t);
    } catch {
      if (seq === seqRef.current) setTimeFailed(true);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const model = state.phase === 'ready' ? state.model : null;
  const dirtySources = useMemo(() => {
    if (!model || !drafts) return [] as string[];
    return model.sources.filter((s) => !draftEquals(model.server[s], drafts[s]));
  }, [model, drafts]);

  function applyBudgetPatch(source: string, patch: Partial<ThrottleBudget>) {
    setDrafts((d) => {
      const cur = d?.[source];
      if (!cur) return d;
      return {
        ...d,
        [source]: {
          ...cur,
          budget: {
            ...cur.budget,
            ...patch,
            minGapMs: patch.minGapMs ? [patch.minGapMs[0], patch.minGapMs[1]] : cur.budget.minGapMs,
            backoff: patch.backoff ? { ...cur.budget.backoff, ...patch.backoff } : cur.budget.backoff,
          },
        },
      };
    });
  }

  function applyDraftFlag(source: string, patch: Partial<Pick<SourceDraft, 'fingerprint' | 'enabled'>>) {
    setDrafts((d) => {
      const cur = d?.[source];
      if (!cur) return d;
      return { ...d, [source]: { ...cur, ...patch } };
    });
  }

  function discardAll() {
    if (!model) return;
    setDrafts(model.server);
    setFormEpoch((e) => e + 1);
  }

  if (state.phase === 'loading') {
    return (
      <div className="settings-screen__state" role="status">
        {SETTINGS.LOADING}
      </div>
    );
  }

  if (state.phase === 'error') {
    return (
      <div className="settings-screen__state settings-screen__state--error" role="alert">
        <p className="settings-screen__state-title">{SETTINGS.ERROR_TITLE}</p>
        <p className="settings-screen__state-msg">{state.message}</p>
        <p className="settings-screen__state-hint">{SETTINGS.ERROR_HINT}</p>
        <button type="button" className="btn" onClick={() => void load()}>
          {SETTINGS.RETRY}
        </button>
      </div>
    );
  }

  const { sources, danger } = state.model;
  const current = selected !== null && drafts !== null ? drafts[selected] : null;
  const currentDanger = selected !== null ? danger[selected] : undefined;
  const dirtySet = new Set(dirtySources);

  return (
    <div className="settings-screen">
      {time !== null && (
        <p className="settings-screen__meta">
          {settingsLine(SETTINGS.TIME_NOW, {
            local: time.local,
            tz: time.timezone,
            sec: Math.abs(Math.round(time.now - Date.now() / 1000)),
          })}
        </p>
      )}
      {timeFailed && <p className="settings-screen__meta">{SETTINGS.TIME_UNAVAILABLE}</p>}

      {sources.length === 0 ? (
        <p className="settings-screen__meta">{SETTINGS.NO_SOURCES}</p>
      ) : (
        <>
          <fieldset className="settings-screen__src">
            <legend>{SETTINGS.SOURCE_PICKER_LABEL}</legend>
            {sources.map((s) => (
              <label key={s} className="settings-screen__src-item">
                <input
                  type="radio"
                  name="settings-source"
                  checked={selected === s}
                  onChange={() => setSelected(s)}
                />
                <span>{sourceDisplayName(s)}</span>
                {dirtySet.has(s) && (
                  /* 装饰点：dirty 语义由下方 role=status 汇总行播报，不在此重复读出 */
                  <span className="settings-screen__dirty-dot" title={SETTINGS.DIRTY_BADGE} aria-hidden="true" />
                )}
              </label>
            ))}
          </fieldset>

          {selected !== null && current !== null && (
            <>
              <ThrottleForm
                key={`${selected}#${formEpoch}`}
                source={selected}
                budget={current.budget}
                onSave={(patch) => applyBudgetPatch(selected, patch)}
              />

              <div className="settings-screen__advanced">
                <h3 className="settings-screen__advanced-title">{SETTINGS.ADVANCED_TITLE}</h3>
                {currentDanger?.fingerprint && (
                  <AdvancedToggle
                    source={selected}
                    flagKey="request_fingerprint_rewrite"
                    enabled={current.fingerprint}
                    confirmCopy={currentDanger.fingerprint.description ?? currentDanger.fingerprint.label}
                    onToggle={(next) => applyDraftFlag(selected, { fingerprint: next })}
                  />
                )}
                {currentDanger?.enabled && (
                  <label className="settings-screen__switch">
                    <input
                      type="checkbox"
                      name={`sources.${selected}.enabled`}
                      checked={current.enabled}
                      onChange={(e) => applyDraftFlag(selected, { enabled: e.target.checked })}
                    />
                    <span>{currentDanger.enabled.label}</span>
                    <span className={`settings-screen__switch-state settings-screen__switch-state--${current.enabled ? 'on' : 'off'}`}>
                      {current.enabled ? SETTINGS.SOURCE_ENABLED_ON : SETTINGS.SOURCE_ENABLED_OFF}
                    </span>
                  </label>
                )}
                {!currentDanger?.fingerprint && !currentDanger?.enabled && (
                  <p className="settings-screen__meta">{SETTINGS.ADVANCED_EMPTY}</p>
                )}
              </div>
            </>
          )}

          <footer className="settings-screen__bar">
            <p className="settings-screen__bar-note" role="status">
              {dirtySources.length > 0
                ? settingsLine(SETTINGS.DIRTY_SUMMARY, { count: dirtySources.length })
                : SETTINGS.SYNCED_NOTE}
            </p>
            <button type="button" className="btn btn--primary" disabled title={SETTINGS.SAVE_DISABLED_NOTE}>
              {SETTINGS.SAVE}
            </button>
            <button
              type="button"
              className="btn"
              disabled={dirtySources.length === 0}
              onClick={discardAll}
            >
              {SETTINGS.DISCARD}
            </button>
          </footer>
          <p className="settings-screen__readonly-note">{SETTINGS.SAVE_DISABLED_NOTE}</p>
        </>
      )}
    </div>
  );
}
