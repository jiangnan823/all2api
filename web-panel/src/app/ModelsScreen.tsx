import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { ApiError, apiGet } from '../lib/panelApi';
import { MODELS, MODEL_SOURCE_NAMES, modelCapLabel } from '../i18n/zh';

/**
 * 模型屏（M5c，M8 升级）。数据源 GET /api/models（wire 契约：{source:"upstream"|"local"|"mixed",
 * models:[{platform, model, name?, context_size?|null, max_output?|null, supports_tool?|null,
 * supports_stream?|null, is_free?|null, credit_multiplier?|null, is_default?|null,
 * thinking_tiers?|null}]}）。WorkBuddy 半区为上游目录实时拉取（网关侧 60s 缓存，刷新按钮
 * 带 refresh=1 强制重拉），Qoder 半区保持静态快照。wb2api 同款的「积分倍率/思考档位」列
 * 就此补齐；能力三态如实展示：字段缺失/null = 未知（—），不编造 yes/no。平台过滤下拉是
 * 纯客户端筛选（数据一次性拉全量）。不落任何 storage（M1 红线）。
 */

/** 单条模型行（wire 全 snake；可选能力字段缺失/null = 目录未声明，展示为未知）。 */
interface ModelRow {
  platform: string;
  model: string;
  /** 展示名（上游目录的 name；与 model 不同时作为副行显示，缺失 = null）。 */
  displayName: string | null;
  contextSize: number | null;
  maxOutput: number | null;
  supportsTool: boolean | null;
  supportsStream: boolean | null;
  isFree: boolean | null;
  creditMultiplier: number | null;
  isDefault: boolean | null;
  thinkingTiers: string[] | null;
  note: string | null;
}

type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; source: string; models: ModelRow[] };

// ---- 防御式解析（形状不对即抛 ApiError；错误前缀 models: 标明来源端点） ----

function asObj(v: unknown, where: string): Record<string, unknown> {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) {
    throw new ApiError(`models: ${where} 非对象`);
  }
  return v as Record<string, unknown>;
}

function reqStr(v: unknown, where: string): string {
  if (typeof v !== 'string' || v === '') {
    throw new ApiError(`models: ${where} 非字符串`);
  }
  return v;
}

function optNum(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

/** 三态布尔：true / false / 缺失（null = 目录未声明，不猜）。 */
function optTri(v: unknown): boolean | null {
  return typeof v === 'boolean' ? v : null;
}

/** 三态字符串数组：非空字符串数组原样收下；其余（缺失/形状不对）= null。 */
function optStrArray(v: unknown): string[] | null {
  if (!Array.isArray(v)) return null;
  const out = v.filter((s): s is string => typeof s === 'string' && s !== '');
  return out.length > 0 ? out : null;
}

function parseModelsWire(body: unknown): { source: string; models: ModelRow[] } {
  const root = asObj(body, '(root)');
  const arr = root['models'];
  if (!Array.isArray(arr)) throw new ApiError('models: models 非数组');
  const models = arr.map((v, i) => {
    const m = asObj(v, `models[${i}]`);
    const model = typeof m['model'] === 'string' && m['model'] !== '' ? m['model'] : reqStr(m['name'], `models[${i}].name`);
    const name = typeof m['name'] === 'string' && m['name'] !== '' && m['name'] !== model ? m['name'] : null;
    return {
      platform: reqStr(m['platform'], `models[${i}].platform`),
      model,
      displayName: name,
      contextSize: optNum(m['context_size']),
      maxOutput: optNum(m['max_output']),
      supportsTool: optTri(m['supports_tool']),
      supportsStream: optTri(m['supports_stream']),
      isFree: optTri(m['is_free']),
      creditMultiplier: optNum(m['credit_multiplier']),
      isDefault: optTri(m['is_default']),
      thinkingTiers: optStrArray(m['thinking_tiers']),
      note: typeof m['note'] === 'string' && m['note'] !== '' ? m['note'] : null,
    };
  });
  // 目录来源标识：契约承诺 "upstream" | "local" | "mixed"；其他值原样透传展示（不装懂），
  // 缺失按 local 兜底
  const source = typeof root['source'] === 'string' && root['source'] !== '' ? root['source'] : 'local';
  return { source, models };
}

export function ModelsScreen() {
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
  const [platform, setPlatform] = useState<string>('all');
  const seqRef = useRef(0);

  const load = useCallback(async (refresh: boolean) => {
    const seq = ++seqRef.current;
    setState({ phase: 'loading' });
    try {
      const { source, models } = await apiGet(refresh ? '/api/models?refresh=1' : '/api/models', parseModelsWire);
      if (seq !== seqRef.current) return;
      setState({ phase: 'ready', source, models });
    } catch (err) {
      if (seq !== seqRef.current) return;
      setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
    }
  }, []);

  useEffect(() => {
    void load(false);
  }, [load]);

  const platforms = useMemo(() => {
    if (state.phase !== 'ready') return [] as string[];
    return [...new Set(state.models.map((m) => m.platform))].sort();
  }, [state]);

  const visible = useMemo(() => {
    if (state.phase !== 'ready') return [] as ModelRow[];
    return platform === 'all' ? state.models : state.models.filter((m) => m.platform === platform);
  }, [state, platform]);

  if (state.phase === 'loading') {
    return (
      <div className="models-screen__state" role="status">
        {MODELS.LOADING}
      </div>
    );
  }

  if (state.phase === 'error') {
    return (
      <div className="models-screen__state models-screen__state--error" role="alert">
        <p className="models-screen__state-title">{MODELS.ERROR_TITLE}</p>
        <p className="models-screen__state-msg">{state.message}</p>
        <p className="models-screen__state-hint">{MODELS.ERROR_HINT}</p>
        <button type="button" className="btn" onClick={() => void load(false)}>
          {MODELS.RETRY}
        </button>
      </div>
    );
  }

  return (
    <div className="models-screen">
      {/* 顶部诚实说明：WorkBuddy 实时目录（网关侧 60s 缓存）+ Qoder 静态快照；未知一律「—」 */}
      <p className="models-screen__note">{MODELS.NOTE}</p>
      <div className="models-screen__bar">
        <label className="models-screen__filter">
          <span>{MODELS.FILTER_LABEL}</span>
          <select value={platform} onChange={(e) => setPlatform(e.target.value)}>
            <option value="all">{MODELS.FILTER_ALL}</option>
            {platforms.map((p) => (
              <option key={p} value={p}>
                {MODEL_SOURCE_NAMES[p] ?? p}
              </option>
            ))}
          </select>
        </label>
        <p className="models-screen__bar-note" role="status">
          {sourceLine(state.source)}
        </p>
        <p className="models-screen__count">{settingsCountLine(visible.length)}</p>
        <button type="button" className="btn" onClick={() => void load(true)}>
          {MODELS.REFRESH}
        </button>
      </div>
      {state.models.length === 0 ? (
        <p className="models-screen__state">{MODELS.EMPTY}</p>
      ) : visible.length === 0 ? (
        <p className="models-screen__state">{MODELS.FILTER_EMPTY}</p>
      ) : (
        <div className="models-screen__scroll">
          <table className="models-screen__table">
            <thead>
              <tr>
                <th scope="col">{MODELS.COL_PLATFORM}</th>
                <th scope="col">{MODELS.COL_MODEL}</th>
                <th scope="col">{MODELS.COL_CREDIT}</th>
                <th scope="col">{MODELS.COL_TIERS}</th>
                <th scope="col">{MODELS.COL_CONTEXT}</th>
                <th scope="col">{MODELS.COL_MAX_OUTPUT}</th>
                <th scope="col">{MODELS.COL_TOOL}</th>
                <th scope="col">{MODELS.COL_STREAM}</th>
                <th scope="col">{MODELS.COL_FREE}</th>
              </tr>
            </thead>
            <tbody>
              {visible.map((m) => (
                <tr key={`${m.platform}/${m.model}`} data-platform={m.platform}>
                  <td>
                    <span className="models-screen__platform">{MODEL_SOURCE_NAMES[m.platform] ?? m.platform}</span>
                  </td>
                  <td className="models-screen__model-name">
                    <code>{m.model}</code>
                    {(m.displayName !== null || m.note !== null) && (
                      <span className="models-screen__model-note">
                        {m.displayName !== null && m.displayName}
                        {m.displayName !== null && m.note !== null && ' · '}
                        {m.note !== null && m.note}
                      </span>
                    )}
                  </td>
                  <td className="models-screen__num">{m.creditMultiplier !== null ? String(m.creditMultiplier) : MODELS.UNKNOWN}</td>
                  <td>{m.thinkingTiers !== null ? m.thinkingTiers.join('/') : MODELS.UNKNOWN}</td>
                  <td className="models-screen__num">{m.contextSize !== null && m.contextSize > 0 ? m.contextSize.toLocaleString() : numOrUnknown(null)}</td>
                  <td className="models-screen__num">{numOrUnknown(m.maxOutput)}</td>
                  <td>{triCopy(m.supportsTool)}</td>
                  <td>{triCopy(m.supportsStream)}</td>
                  <td>{triCopy(m.isFree)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function numOrUnknown(n: number | null): string {
  return n === null ? MODELS.UNKNOWN : n.toLocaleString('en-US');
}

/** 三态能力格：yes/no/未知，语义色只对明确值使用。 */
function triCopy(v: boolean | null): string {
  if (v === true) return modelCapLabel('yes');
  if (v === false) return modelCapLabel('no');
  return MODELS.UNKNOWN;
}

/** 目录来源行：三种诚实标注（upstream/local/mixed），未知值原样透传。 */
function sourceLine(source: string): string {
  if (source === 'local') return MODELS.SOURCE_LOCAL;
  if (source === 'upstream') return MODELS.SOURCE_UPSTREAM;
  if (source === 'mixed') return MODELS.SOURCE_MIXED;
  return `${MODELS.SOURCE_PREFIX}${source}`;
}

function settingsCountLine(count: number): string {
  return MODELS.COUNT.replace('{count}', String(count));
}
