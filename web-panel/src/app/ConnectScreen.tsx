import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { ApiError, apiGet } from '../lib/panelApi';
import { CONNECT, MODEL_SOURCE_NAMES, settingsLine } from '../i18n/zh';

/**
 * 连接屏（M8-connect，story-map P0 补真屏——M5b 删除 connect 空状态节后此处首次重建）。
 * 三块只读内容卡，互不阻塞、各自如实出错：
 *   1. Base URL：location.origin + '/v1' 纯前端拼装 + 一键复制（不做 external_base_url 换算，
 *      面板与网关同源部署，浏览器地址即对外地址）；
 *   2. API Key 状态：读 GET /api/config 的 raw.api_key 有无 → 已配置 / 开放模式徽标；
 *      本屏不提供编辑，改 key 引导到配置页（onOpenSettings 由外壳接线传入）；
 *   3. 可用模型：GET /api/models（后端端点已存在，wire：{models:[{platform,id,name,...}]}），
 *      id/name 表格 + 平台徽标 + 一键复制模型名称；失败如实错误态，不编造目录。
 * 全部内存态，不落 storage（M1 红线）。
 */

/** /api/models 行（M8 只消费 id/name/platform 三列；能力列归「模型」屏，不重复渲染）。 */
interface ConnectModel {
  platform: string;
  id: string;
  name: string | null;
}

type KeyState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; configured: boolean };

type ModelsState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; models: ConnectModel[] };

// ---- 防御式解析（形状不对即抛 ApiError；错误前缀 connect: 标明来源端点） ----

function asObj(v: unknown, where: string): Record<string, unknown> {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) {
    throw new ApiError(`connect: ${where} 非对象`);
  }
  return v as Record<string, unknown>;
}

function reqStr(v: unknown, where: string): string {
  if (typeof v !== 'string' || v === '') {
    throw new ApiError(`connect: ${where} 非字符串`);
  }
  return v;
}

/** GET /api/config → raw.api_key 有无（字段缺失/空串 = 开放模式；raw 缺失 = 形状异常如实上抛）。 */
function parseKeyConfigured(body: unknown): boolean {
  const root = asObj(body, 'config(root)');
  const raw = asObj(root['raw'], 'config.raw');
  return typeof raw['api_key'] === 'string' && raw['api_key'] !== '';
}

function parseConnectModels(body: unknown): ConnectModel[] {
  const root = asObj(body, 'models(root)');
  const arr = root['models'];
  if (!Array.isArray(arr)) throw new ApiError('connect: models 非数组');
  return arr.map((v, i) => {
    const m = asObj(v, `models[${i}]`);
    return {
      platform: reqStr(m['platform'], `models[${i}].platform`),
      id: reqStr(m['id'], `models[${i}].id`),
      // name 为展示名（客户端 model 参数的取值）；个别目录行可能缺省 → 模型名回退 id，不装懂
      name: typeof m['name'] === 'string' && m['name'] !== '' ? m['name'] : null,
    };
  });
}

export function ConnectScreen(props: { onOpenSettings: () => void }) {
  const [keyState, setKeyState] = useState<KeyState>({ phase: 'loading' });
  const [modelsState, setModelsState] = useState<ModelsState>({ phase: 'loading' });
  /** 过期响应守卫：两块各自独立计数（重试/并发下旧响应不得覆盖新状态）。 */
  const keySeqRef = useRef(0);
  const modelsSeqRef = useRef(0);

  const loadKey = useCallback(async () => {
    const seq = ++keySeqRef.current;
    try {
      const configured = await apiGet('/api/config', parseKeyConfigured);
      if (seq !== keySeqRef.current) return;
      setKeyState({ phase: 'ready', configured });
    } catch (err) {
      if (seq !== keySeqRef.current) return;
      setKeyState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
    }
  }, []);

  const loadModels = useCallback(async () => {
    const seq = ++modelsSeqRef.current;
    try {
      const models = await apiGet('/api/models', parseConnectModels);
      if (seq !== modelsSeqRef.current) return;
      setModelsState({ phase: 'ready', models });
    } catch (err) {
      if (seq !== modelsSeqRef.current) return;
      setModelsState({ phase: 'error', message: err instanceof Error ? err.message : String(err) });
    }
  }, []);

  useEffect(() => {
    void loadKey();
    void loadModels();
  }, [loadKey, loadModels]);

  // Base URL 纯前端拼装：面板与网关同源部署，浏览器地址即网关地址
  const baseUrl = `${window.location.origin}/v1`;

  return (
    <div className="connect-screen">
      <p className="connect-screen__note">{CONNECT.SUB}</p>

      {/* ---- 卡一：Base URL ---- */}
      <section className="connect-screen__card" aria-labelledby="connect-baseurl-h">
        <h3 className="connect-screen__card-title" id="connect-baseurl-h">
          {CONNECT.BASEURL_TITLE}
        </h3>
        <p className="connect-screen__card-hint">{CONNECT.BASEURL_HINT}</p>
        <div className="connect-screen__url-row">
          <code className="connect-screen__url">{baseUrl}</code>
          <CopyButton value={baseUrl} label={CONNECT.COPY} copiedLabel={CONNECT.COPIED} />
        </div>
      </section>

      {/* ---- 卡二：API Key 状态（只读；改 key 引导配置页） ---- */}
      <section className="connect-screen__card" aria-labelledby="connect-key-h">
        <h3 className="connect-screen__card-title" id="connect-key-h">
          {CONNECT.KEY_TITLE}
        </h3>
        {keyState.phase === 'loading' && (
          <p className="connect-screen__state" role="status">
            {CONNECT.KEY_LOADING}
          </p>
        )}
        {keyState.phase === 'error' && (
          <div className="connect-screen__state connect-screen__state--error" role="alert">
            <p className="connect-screen__state-title">{CONNECT.KEY_ERROR_TITLE}</p>
            <p className="connect-screen__state-msg">{keyState.message}</p>
            <p className="connect-screen__state-hint">{CONNECT.KEY_ERROR_HINT}</p>
            <button type="button" className="btn" onClick={() => void loadKey()}>
              {CONNECT.RETRY}
            </button>
          </div>
        )}
        {keyState.phase === 'ready' && (
          <>
            <p className="connect-screen__key-line">
              <span className={`connect-screen__badge connect-screen__badge--${keyState.configured ? 'on' : 'off'}`}>
                {keyState.configured ? CONNECT.KEY_CONFIGURED : CONNECT.KEY_OPEN}
              </span>
            </p>
            <p className="connect-screen__card-hint">
              {keyState.configured ? CONNECT.KEY_CONFIGURED_HINT : CONNECT.KEY_OPEN_HINT}
            </p>
            <a className="btn connect-screen__goto" href="#settings" onClick={props.onOpenSettings}>
              {CONNECT.KEY_GOTO_SETTINGS}
            </a>
          </>
        )}
      </section>

      {/* ---- 卡三：可用模型（id/name 表 + 平台徽标 + 复制模型名） ---- */}
      <section className="connect-screen__card" aria-labelledby="connect-models-h">
        <h3 className="connect-screen__card-title" id="connect-models-h">
          {CONNECT.MODELS_TITLE}
        </h3>
        <p className="connect-screen__card-hint">{CONNECT.MODELS_HINT}</p>
        {modelsState.phase === 'loading' && (
          <p className="connect-screen__state" role="status">
            {CONNECT.MODELS_LOADING}
          </p>
        )}
        {modelsState.phase === 'error' && (
          <div className="connect-screen__state connect-screen__state--error" role="alert">
            <p className="connect-screen__state-title">{CONNECT.MODELS_ERROR_TITLE}</p>
            <p className="connect-screen__state-msg">{modelsState.message}</p>
            <p className="connect-screen__state-hint">{CONNECT.MODELS_ERROR_HINT}</p>
            <button type="button" className="btn" onClick={() => void loadModels()}>
              {CONNECT.RETRY}
            </button>
          </div>
        )}
        {modelsState.phase === 'ready' &&
          (modelsState.models.length === 0 ? (
            <p className="connect-screen__state">{CONNECT.MODELS_EMPTY}</p>
          ) : (
            <>
              <p className="connect-screen__count" role="status">
                {settingsLine(CONNECT.MODELS_COUNT, { count: modelsState.models.length })}
              </p>
              <div className="connect-screen__scroll">
                <table className="connect-screen__table">
                  <thead>
                    <tr>
                      <th scope="col">{CONNECT.COL_PLATFORM}</th>
                      <th scope="col">{CONNECT.COL_ID}</th>
                      <th scope="col">{CONNECT.COL_NAME}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {modelsState.models.map((m) => (
                      <tr key={m.id} data-platform={m.platform}>
                        <td>
                          <span className="connect-screen__platform">{MODEL_SOURCE_NAMES[m.platform] ?? m.platform}</span>
                        </td>
                        <td className="connect-screen__model-id">
                          <code>{m.id}</code>
                        </td>
                        <td className="connect-screen__model-name">
                          <CopyButton value={m.name ?? m.id} label={CONNECT.COPY_MODEL} copiedLabel={CONNECT.COPIED}>
                            <code>{m.name ?? m.id}</code>
                          </CopyButton>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          ))}
      </section>
    </div>
  );
}

/**
 * 一键复制按钮（M8）：clipboard API 不可用/被拒时如实不提示成功，不假成功
 * （与账号屏 AccountIdCell 同一纪律）。children 作按钮主体（如模型名 code），
 * 缺省用 label 文本。
 */
function CopyButton(props: { value: string; label: string; copiedLabel: string; children?: ReactNode }) {
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<number | null>(null);

  useEffect(
    () => () => {
      if (timerRef.current !== null) window.clearTimeout(timerRef.current);
    },
    [],
  );

  const onClick = () => {
    // 双重可选链：clipboard 对象存在但 writeText 缺失时也不抛（如实不提示成功）
    navigator.clipboard
      ?.writeText?.(props.value)
      .then(() => {
        setCopied(true);
        if (timerRef.current !== null) window.clearTimeout(timerRef.current);
        timerRef.current = window.setTimeout(() => setCopied(false), 1500);
      })
      .catch(() => setCopied(false));
  };

  return (
    <button
      type="button"
      className="connect-screen__copy"
      onClick={onClick}
      title={props.label}
      aria-label={`${props.label}: ${props.value}`}
    >
      {props.children ?? props.label}
      {copied && (
        <span className="connect-screen__copied" role="status">
          {props.copiedLabel}
        </span>
      )}
    </button>
  );
}
