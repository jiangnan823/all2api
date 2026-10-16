import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { FieldError, ServerTimeResponse, ThrottleBudget } from '../types/api-contract';
import { AdvancedToggle, ThrottleForm, TimeSlotPicker } from '../components';
import {
  fetchConfigGroups,
  restoreDefaults,
  fetchPanelConfig,
  fetchServerTime,
  savePanelConfig,
  setApiKeyProvider,
  ApiError,
} from '../lib/panelApi';
import {
  buildSettingsModel,
  draftEquals,
  genericDraftToConfigPatch,
  genericEquals,
  type GenericSection,
  type GenericValues,
  type SettingsModel,
  type SourceDraft,
} from '../lib/settingsModel';
import { budgetDiff, setDeliveredThrottleFields, throttlePatchToConfigPatch } from '../lib/throttle';
import { SETTINGS, settingsLine, sourceDisplayName } from '../i18n/zh';

/**
 * 设置屏容器（M2 B2 → M2 第二批 → M10-A 定时任务区下放）：进入时并行拉 GET /api/config +
 * GET /api/config/fields，按源装配 ThrottleForm（元数据驱动）与 AdvancedToggle（指纹开关，
 * 内含 DangerAction 确认），并按交付元数据渲染通用配置组（gateway/routing/pooling，值取 raw
 * 透传）。schedule 组（定时任务）M10-A 起下放到账号池各平台页（PlatformTasksCard），
 * 本屏对交付元数据里的 schedule 组整组不渲染。cron 字段仍以共享 TimeSlotPicker（24 时点）
 * 呈现——元数据里若还有其他 cron 字段照常可编辑。
 * 保存流（M2 第二批）：dirty 非空时「保存全部改动」激活 → POST /api/config（ConfigPatch，
 * 只发改动字段的冻结 JSON path）；成功按服务端回读 applied 重建基线，字段级拒绝逐条红字，
 * 401/404 等按真实错误码如实展示（后端保存端点并行开发中，不硬编码假成功）。
 * X-API-Key：内存态输入（M1 红线——不落任何 storage），粘贴即生效于后续全部请求，
 * 清空即回到开放模式；刷新丢失是预期行为，UI 文案如实说明。
 */

/**
 * M10-A：定时任务（schedule 组）下放到账号池各平台页（PlatformTasksCard 承接编辑面），
 * 配置页整组摘除——在模型装配后统一过滤，drafts/dirty/保存补丁三处自然不含 schedule。
 */
function withoutScheduleGroup(model: SettingsModel): SettingsModel {
  return { ...model, generic: model.generic.filter((s) => s.group !== 'schedule') };
}

type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; model: SettingsModel };

type SaveState =
  | { phase: 'idle' }
  | { phase: 'saving' }
  | { phase: 'saved' }
  | { phase: 'field-errors'; errors: FieldError[] }
  | { phase: 'failed'; message: string; status?: number };

export function SettingsScreen() {
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
  const [drafts, setDrafts] = useState<Record<string, SourceDraft> | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [time, setTime] = useState<ServerTimeResponse | null>(null);
  const [timeFailed, setTimeFailed] = useState(false);
  /** 草稿基线重置（放弃改动 / 重新加载）时自增，强制 ThrottleForm 丢弃内部草稿重新挂载。 */
  const [formEpoch, setFormEpoch] = useState(0);
  /** 通用配置组的本地草稿（key = 冻结 JSON path）。服务端基线在 model.generic[].server。 */
  const [genericDrafts, setGenericDrafts] = useState<Record<string, GenericValues> | null>(null);
  const [saveState, setSaveState] = useState<SaveState>({ phase: 'idle' });
  const seqRef = useRef(0);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    setState({ phase: 'loading' });
    setTime(null);
    setTimeFailed(false);
    setSaveState({ phase: 'idle' });
    try {
      const [config, groups] = await Promise.all([fetchPanelConfig(), fetchConfigGroups()]);
      if (seq !== seqRef.current) return;
      const model = withoutScheduleGroup(buildSettingsModel(config, groups));
      // 元数据成为 ThrottleForm 的字段数据源（props/视觉不变，见 lib/throttle.ts 注册表注释）
      setDeliveredThrottleFields(model.throttleFields);
      setState({ phase: 'ready', model });
      setDrafts(model.server);
      setGenericDrafts(Object.fromEntries(model.generic.map((g) => [g.group, { ...g.server }])));
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

  const dirtyGroups = useMemo(() => {
    if (!model || !genericDrafts) return [] as string[];
    return model.generic
      .filter((g) => {
        const d = genericDrafts[g.group];
        return d !== undefined && !genericEquals(g, d);
      })
      .map((g) => g.group);
  }, [model, genericDrafts]);

  const dirtyCount = dirtySources.length + dirtyGroups.length;

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

  function applyGenericField(group: string, key: string, value: string | number | boolean) {
    setGenericDrafts((d) => {
      const cur = d?.[group];
      if (cur === undefined) return d;
      return { ...d, [group]: { ...cur, [key]: value } };
    });
  }

  function discardAll() {
    if (!model) return;
    setDrafts(model.server);
    setGenericDrafts(Object.fromEntries(model.generic.map((g) => [g.group, { ...g.server }])));
    setFormEpoch((e) => e + 1);
    setSaveState({ phase: 'idle' });
  }

  /**
   * 保存流：装配「只含改动字段」的 ConfigPatch 并 POST /api/config。
   * 200 + ok=true → 以服务端回读 applied 为准重建基线（不信本地乐观值，契约明文）；
   * 200 + fieldErrors → 逐字段红字（草稿保留，改完可再保存）；
   * 401 → key 缺失/不符；404/501 → 后端端点未部署（并行开发中，如实展示）。
   */
  async function saveAll() {
    if (!model || !drafts || !genericDrafts) return;
    const patch = [
      ...dirtyGroups.flatMap((g) => {
        const section = model.generic.find((s) => s.group === g);
        const draft = genericDrafts[g];
        return section && draft ? genericDraftToConfigPatch(section, draft) : [];
      }),
      ...dirtySources.flatMap((s) => {
        const draft = drafts[s];
        if (!draft) return [];
        const flagKeys: Array<{ key: string; value: boolean | undefined }> = [];
        const base = model.server[s];
        if (draft.fingerprint !== base.fingerprint) {
          flagKeys.push({ key: `sources.${s}.advanced.request_fingerprint_rewrite`, value: draft.fingerprint });
        }
        if (draft.enabled !== base.enabled) {
          flagKeys.push({ key: `sources.${s}.enabled`, value: draft.enabled });
        }
        return [...throttlePatchToConfigPatch(s, budgetDiff(base.budget, draft.budget)), ...flagKeys];
      }),
    ];
    if (patch.length === 0) return;
    setSaveState({ phase: 'saving' });
    try {
      const res = await savePanelConfig(patch);
      if (res.ok && res.applied) {
        // 服务端回读为准：重建全部基线，包括 generic 值与 advanced/enabled（可能与其他写入方合流）
        const next = withoutScheduleGroup(buildSettingsModel(res.applied, await fetchConfigGroups()));
        setDeliveredThrottleFields(next.throttleFields);
        setState({ phase: 'ready', model: next });
        setDrafts(next.server);
        setGenericDrafts(Object.fromEntries(next.generic.map((g) => [g.group, { ...g.server }])));
        setSelected((prev) => (prev && next.sources.includes(prev) ? prev : (next.sources[0] ?? null)));
        setFormEpoch((e) => e + 1);
        setSaveState({ phase: 'saved' });
      } else if (res.fieldErrors && res.fieldErrors.length > 0) {
        setSaveState({ phase: 'field-errors', errors: res.fieldErrors });
      } else {
        // ok=false 且无 fieldErrors：形状异常，如实报错不装懂
        setSaveState({ phase: 'failed', message: SETTINGS.SAVE_MALFORMED_RESPONSE });
      }
    } catch (err) {
      const status = err instanceof ApiError ? err.status : undefined;
      setSaveState({
        phase: 'failed',
        message: err instanceof Error ? err.message : String(err),
        status,
      });
    }
  }

  /** 恢复默认设置（v1.2 主人裁定「就没有个默认的设置吗」）：POST restore-defaults，
   *  连接四项（listen/api_key/auth_dir/state_file）后端保留，其余回出厂；
   *  成功后重建基线（与保存流同路径）。二次确认用原生 confirm（一次性动作，非危险态字段）。 */
  async function restoreAll() {
    if (!model) return;
    setSaveState({ phase: 'saving' });
    try {
      await restoreDefaults();
      const applied = await fetchPanelConfig();
      const next = withoutScheduleGroup(buildSettingsModel(applied, await fetchConfigGroups()));
      setDeliveredThrottleFields(next.throttleFields);
      setState({ phase: 'ready', model: next });
      setDrafts(next.server);
      setGenericDrafts(Object.fromEntries(next.generic.map((g) => [g.group, { ...g.server }])));
      setFormEpoch((e) => e + 1);
      setSaveState({ phase: 'saved' });
    } catch (err) {
      setSaveState({
        phase: 'failed',
        message: err instanceof Error ? err.message : String(err),
      });
    }
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

  const { sources, danger, generic } = state.model;
  const current = selected !== null && drafts !== null ? drafts[selected] : null;
  const currentDanger = selected !== null ? danger[selected] : undefined;
  const dirtySet = new Set(dirtySources);
  const saving = saveState.phase === 'saving';

  return (
    <div className="settings-screen">
      <ApiKeySection />

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

          <GenericConfigSections
            sections={generic}
            drafts={genericDrafts}
            dirtyGroups={dirtyGroups}
            onChange={applyGenericField}
          />

          <footer className="settings-screen__bar">
            <p className="settings-screen__bar-note" role="status">
              {dirtyCount > 0
                ? settingsLine(SETTINGS.DIRTY_SUMMARY, { count: dirtyCount })
                : saveState.phase === 'saved'
                  ? SETTINGS.SAVE_OK_NOTE
                  : SETTINGS.SYNCED_NOTE}
            </p>
            <button
              type="button"
              className="btn btn--primary"
              disabled={dirtyCount === 0 || saving}
              onClick={() => void saveAll()}
            >
              {saving ? SETTINGS.SAVING : SETTINGS.SAVE}
            </button>
            <button
              type="button"
              className="btn"
              disabled={dirtyCount === 0 || saving}
              onClick={discardAll}
            >
              {SETTINGS.DISCARD}
            </button>
          </footer>
          {saveState.phase === 'field-errors' && (
            <div className="settings-screen__save-errors" role="alert">
              <p className="settings-screen__save-errors-title">{SETTINGS.SAVE_REJECTED_TITLE}</p>
              <ul>
                {saveState.errors.map((e) => (
                  <li key={`${e.key}:${e.message}`}>
                    <code>{e.key}</code> — {e.message}
                  </li>
                ))}
              </ul>
            </div>
          )}
          {saveState.phase === 'failed' && (
            <p className="settings-screen__save-failed" role="alert">
              {settingsLine(SETTINGS.SAVE_FAILED, {
                detail: saveState.status !== undefined ? `HTTP ${saveState.status}` : saveState.message,
              })}
            </p>
          )}
          <div className="row">
            <button
              type="button"
              className="btn"
              onClick={() => {
                if (window.confirm(SETTINGS.RESTORE_CONFIRM)) void restoreAll();
              }}
              disabled={saveState.phase === 'saving'}
            >
              {SETTINGS.RESTORE_DEFAULTS}
            </button>
          </div>
          <p className="settings-screen__readonly-note">{SETTINGS.SAVE_NOTE}</p>
        </>
      )}
    </div>
  );
}

/**
 * 通用配置组渲染（M2 第二批）：gateway/routing/pooling/schedule 的 FieldDef 元数据直渲。
 * 每组一段 fieldset；number/cron/string → 单值输入，boolean → 勾选；requiresConfirm 字段
 * 给危险区样式标注（保存流仍只按 JSON path 提交，确认语义与后端一致：值由用户显式改动）。
 */
function GenericConfigSections(props: {
  sections: GenericSection[];
  drafts: Record<string, GenericValues> | null;
  dirtyGroups: string[];
  onChange: (group: string, key: string, value: string | number | boolean) => void;
}) {
  const { sections, drafts, dirtyGroups, onChange } = props;
  if (sections.length === 0) return null;
  return (
    <div className="settings-screen__generic">
      {sections.map((s) => {
        const values = drafts?.[s.group];
        const dirty = dirtyGroups.includes(s.group);
        return (
          <fieldset key={s.group} className="settings-screen__generic-group" data-group={s.group}>
            <legend>
              {s.title}
              {dirty && (
                <span className="settings-screen__dirty-dot" title={SETTINGS.DIRTY_BADGE} aria-hidden="true" />
              )}
            </legend>
            {values === undefined ? (
              <p className="settings-screen__meta">{SETTINGS.LOADING}</p>
            ) : (
              s.fields.map((f) => {
                const value = values[f.key];
                const errHint = f.requiresConfirm ? SETTINGS.GENERIC_CONFIRM_NOTE : null;
                return (
                  <label key={f.key} className="settings-screen__generic-field">
                    <span className="settings-screen__generic-label">
                      {f.label}
                      {f.unit !== undefined && <span className="settings-screen__generic-unit">（{f.unit}）</span>}
                    </span>
                    {f.type === 'boolean' ? (
                      <input
                        type="checkbox"
                        name={f.key}
                        checked={value === true}
                        onChange={(e) => onChange(s.group, f.key, e.target.checked)}
                      />
                    ) : f.type === 'cron' ? (
                      /* M10-A 起 cron 字段的 24 时点选择 UI 为共享组件（PlatformTasksCard 同款） */
                      <TimeSlotPicker
                        name={f.key}
                        value={typeof value === 'string' ? value : ''}
                        onChange={(expr) => onChange(s.group, f.key, expr)}
                      />
                    ) : (
                      <input
                        type={f.type === 'number' ? 'number' : 'text'}
                        name={f.key}
                        value={typeof value === 'string' || typeof value === 'number' ? String(value) : ''}
                        min={f.min}
                        max={f.max}
                        onChange={(e) => {
                          if (f.type === 'number') {
                            // 空输入保持空串（NaN 不落草稿），非空按数值落
                            const n = e.target.value === '' ? '' : Number(e.target.value);
                            onChange(s.group, f.key, typeof n === 'number' && Number.isFinite(n) ? n : '');
                          } else {
                            onChange(s.group, f.key, e.target.value);
                          }
                        }}
                      />
                    )}
                    {f.description !== undefined && (
                      <span className="settings-screen__generic-desc">{f.description}</span>
                    )}
                    {f.effect === 'restart_required' && (
                      <span className="settings-screen__generic-effect">{SETTINGS.GENERIC_EFFECT_RESTART}</span>
                    )}
                    {errHint !== null && <span className="settings-screen__generic-effect">{errHint}</span>}
                  </label>
                );
              })
            )}
          </fieldset>
        );
      })}
    </div>
  );
}

/**
 * X-API-Key 内存态输入（M2 第二批）。粘贴即注入 panelApi provider（后续全部 /api/* 请求带头）；
 * 清空 = 开放模式（不带头）。红线：值只存 React 内存态，任何 storage 均不落（M1 决策）；
 * 刷新丢失是预期行为——文案如实说明，不假装持久化。
 * M8-connect：明文/密文眼睛切换（纯前端显隐，不影响注入 provider 的值本身）。
 */
function ApiKeySection() {
  const [key, setKey] = useState('');
  const [show, setShow] = useState(false);
  const apply = (next: string) => {
    setKey(next);
    setApiKeyProvider(() => next);
  };
  return (
    <section className="settings-screen__apikey">
      <label className="settings-screen__apikey-label" htmlFor="settings-apikey-input">
        {SETTINGS.APIKEY_TITLE}
      </label>
      <span className="settings-screen__apikey-row">
        <input
          id="settings-apikey-input"
          type={show ? 'text' : 'password'}
          autoComplete="off"
          className="settings-screen__apikey-input"
          placeholder={SETTINGS.APIKEY_PLACEHOLDER}
          value={key}
          onChange={(e) => apply(e.target.value)}
        />
        <button
          type="button"
          className="settings-screen__apikey-eye"
          onClick={() => setShow((v) => !v)}
          aria-pressed={show}
          title={show ? SETTINGS.APIKEY_HIDE : SETTINGS.APIKEY_SHOW}
        >
          {show ? SETTINGS.APIKEY_HIDE : SETTINGS.APIKEY_SHOW}
        </button>
      </span>
      <p className="settings-screen__apikey-note">{SETTINGS.APIKEY_NOTE}</p>
    </section>
  );
}
