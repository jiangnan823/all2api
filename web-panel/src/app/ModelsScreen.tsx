import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { ApiError, apiGet } from '../lib/panelApi';
import { MODELS, MODEL_SOURCE_NAMES, modelCapLabel } from '../i18n/zh';

/**
 * 模型屏（M5c）。数据源 GET /api/models（端点由并行批交付，wire 契约：
 * {source:"local", models:[{platform, model, context_size?, max_output?, supports_tool?,
 * supports_stream?, is_free?, note?}]}）。wb2api 参考实现里的「积分倍率/思考档位」列在本
 * 网关没有对应数据（本地目录无该字段、上游实时目录未接入）——如实裁剪不渲染，只在
 * 顶部说明行交代目录来源。能力三态如实展示：字段缺失 = 未知（—），不编造 yes/no。
 * 平台过滤下拉是纯客户端筛选（数据一次性拉全量）。不落任何 storage（M1 红线）。
 */

/** 单条模型行（wire 全 snake；可选能力字段缺失 = 目录未声明，展示为未知）。 */
interface ModelRow {
  platform: string;
  model: string;
  contextSize: number | null;
  maxOutput: number | null;
  supportsTool: boolean | null;
  supportsStream: boolean | null;
  isFree: boolean | null;
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

function parseModelsWire(body: unknown): { source: string; models: ModelRow[] } {
  const root = asObj(body, '(root)');
  const arr = root['models'];
  if (!Array.isArray(arr)) throw new ApiError('models: models 非数组');
  const models = arr.map((v, i) => {
    const m = asObj(v, `models[${i}]`);
    return {
      platform: reqStr(m['platform'], `models[${i}].platform`),
      // M5a wire carries `name`（adapter.ModelInfo 既有标签）；`model` 为契约兼容名
      model: typeof m['model'] === 'string' && m['model'] !== '' ? m['model'] : reqStr(m['name'], `models[${i}].name`),
      contextSize: optNum(m['context_size']),
      maxOutput: optNum(m['max_output']),
      supportsTool: optTri(m['supports_tool']),
      supportsStream: optTri(m['supports_stream']),
      isFree: optTri(m['is_free']),
      note: typeof m['note'] === 'string' && m['note'] !== '' ? m['note'] : null,
    };
  });
  // 目录来源标识：契约只承诺 "local"；其他值原样透传展示（不装懂），缺失按 local 兜底
  const source = typeof root['source'] === 'string' && root['source'] !== '' ? root['source'] : 'local';
  return { source, models };
}

export function ModelsScreen() {
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
  const [platform, setPlatform] = useState<string>('all');
  const seqRef = useRef(0);

  const load = useCallback(async () => {
    const seq = ++seqRef.current;
    setState({ phase: 'loading' });
    try {
      const { source, models } = await apiGet('/api/models', parseModelsWire);
      if (seq !== seqRef.current) return;
      setState({ phase: 'ready', source, models });
    } catch (err) {
      if (seq !== seqRef.current) return;
      setState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
    }
  }, []);

  useEffect(() => {
    void load();
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
        <button type="button" className="btn" onClick={() => void load()}>
          {MODELS.RETRY}
        </button>
      </div>
    );
  }

  return (
    <div className="models-screen">
      {/* 顶部诚实说明：目录来自本地配置（workbuddy 默认模型 + ModelMap / qoder 静态快照），
          上游实时目录未接入；wb2api 式的积分倍率/思考档位无数据来源，本屏不渲染 */}
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
          {state.source === 'local'
            ? MODELS.SOURCE_LOCAL
            : settingsSourceLine(state.source)}
        </p>
        <p className="models-screen__count">{settingsCountLine(visible.length)}</p>
        <button type="button" className="btn" onClick={() => void load()}>
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
                    {m.note !== null && <span className="models-screen__model-note">{m.note}</span>}
                  </td>
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

function settingsSourceLine(source: string): string {
  return `${MODELS.SOURCE_PREFIX}${source}`;
}

function settingsCountLine(count: number): string {
  return MODELS.COUNT.replace('{count}', String(count));
}
