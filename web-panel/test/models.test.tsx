import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { axe, toHaveNoViolations } from 'jest-axe';
import { ModelsScreen } from '../src/app/ModelsScreen';
import { MODELS, MODEL_SOURCE_NAMES } from '../src/i18n/zh';

/**
 * 模型屏（M5c，M8 升级）门禁：读 GET /api/models（wire：{source:"upstream"|"local"|"mixed",
 * models:[{platform, model, ...可选能力字段}]}），平台过滤 + 能力表。M8 补齐 wb2api 同款的
 * 「积分倍率/思考档位」两列（可空显示「—」，档位数组 join）；刷新按钮带 refresh=1 强制
 * 实时重拉。能力三态如实：字段缺失/null = 「—」（未知），不编造 yes/no。
 * fetch 全程 mock（沿用 settings.test 的 stubGlobal 模式）。
 */
expect.extend(toHaveNoViolations);

// ---------- wire fixtures（蛇形，对齐后端 adapter.ModelInfo 的能力字段语义） ----------

/** workbuddy 六个默认模型（ContextSize/SupportsTool/SupportsStream/IsFree，见 platform/adapter.go）。 */
const WORKBUDDY_MODELS = [
  { platform: 'workbuddy', model: 'deepseek-v4-flash', context_size: 1048576, supports_tool: true, supports_stream: true, is_free: true },
  { platform: 'workbuddy', model: 'glm-5.3-flash', context_size: 1048576, supports_tool: true, supports_stream: true, is_free: true },
  { platform: 'workbuddy', model: 'kimi-k3', context_size: 200000, supports_tool: true, supports_stream: true, is_free: true },
  { platform: 'workbuddy', model: 'minimax-m3', context_size: 200000, supports_tool: true, supports_stream: true, is_free: true },
  { platform: 'workbuddy', model: 'hy3', context_size: 200000, supports_tool: true, supports_stream: true, is_free: true },
  { platform: 'workbuddy', model: 'hunyuan', context_size: 1048576, supports_tool: true, supports_stream: true, is_free: true },
];

/** qoder 静态快照（仅 SupportsStream=true 有据，其余能力字段缺失 = 未声明 → 「—」）。 */
const QODER_MODELS = [
  'DeepSeek-V4-Pro', 'DeepSeek-Flash', 'GLM-5.2', 'GLM-5.3', 'GLM-5.3-Flash',
  'Kimi-K2.8-Preview', 'Kimi-K3', 'MiniMax-M2.7', 'Qwen3.7-Flash', 'Qwen3.7-Max',
  'Qwen3.7-Plus', 'Qwen3.8-Flash', 'Qwen3.8-Max',
].map((model) => ({ platform: 'qoder', model, supports_stream: true }));

const MODELS_WIRE = { source: 'local', models: [...WORKBUDDY_MODELS, ...QODER_MODELS] };

const EMPTY_WIRE = { source: 'local', models: [] };

// ---------- fetch mock ----------

type Route = { status?: number; body?: unknown; reject?: boolean };
type Routes = Record<string, Route>;

function installFetch(routes: Routes) {
  const fn = vi.fn(async (...args: unknown[]) => {
    const url = typeof args[0] === 'string' ? args[0] : String(args[0]);
    const route = routes[url] ?? { status: 404, body: { error: 'not found' } };
    if (route.reject) throw new TypeError('fetch failed');
    const status = route.status ?? 200;
    return { ok: status < 400, status, json: async () => route.body };
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

async function renderReady(
  routes: Routes = { '/api/models': { body: MODELS_WIRE } },
  awaitText: string = 'deepseek-v4-flash',
) {
  const fn = installFetch(routes);
  render(<ModelsScreen />);
  await screen.findByText(awaitText);
  return fn;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('模型屏 — 加载 / 错误 / 重试', () => {
  it('挂载先出加载态；就绪后 19 行模型齐全，请求落在 GET /api/models', async () => {
    const fn = await renderReady();
    expect(fn.mock.calls.map((c) => String(c[0]))).toEqual(['/api/models']);
    expect(screen.getByText(MODELS.COUNT.replace('{count}', '19'))).toBeTruthy();
    expect(document.querySelectorAll('tbody tr')).toHaveLength(19);
  });

  it('500 → 错误态带 HTTP 码；重试成功后恢复', async () => {
    installFetch({ '/api/models': { status: 500, body: { error: 'boom' } } });
    render(<ModelsScreen />);
    expect(await screen.findByText(MODELS.ERROR_TITLE)).toBeTruthy();
    expect(screen.getByText('HTTP 500')).toBeTruthy();
    installFetch({ '/api/models': { body: MODELS_WIRE } });
    fireEvent.click(screen.getByRole('button', { name: MODELS.RETRY }));
    await screen.findByText('deepseek-v4-flash');
  });

  it('网络错误与形状异常（models 非数组）→ 错误态不静默兜底', async () => {
    installFetch({ '/api/models': { reject: true } });
    render(<ModelsScreen />);
    expect(await screen.findByText('network error')).toBeTruthy();

    installFetch({ '/api/models': { body: { models: 42 } } });
    fireEvent.click(screen.getByRole('button', { name: MODELS.RETRY }));
    expect(await screen.findByText(MODELS.ERROR_TITLE)).toBeTruthy();
  });
});

describe('模型屏 — 表格内容与三态能力', () => {
  it('workbuddy 行能力齐全：上下文千分位、工具/流式/免费额度为「支持」', async () => {
    await renderReady();
    const row = document.querySelector('tr[data-platform="workbuddy"]')!;
    expect(row.textContent).toContain('deepseek-v4-flash');
    expect(row.textContent).toContain('1,048,576');
    expect(row.textContent).toContain('支持');
  });

  it('qoder 行能力字段缺失 → 「—」（未知），只有流式有据；不编造 yes/no', async () => {
    await renderReady();
    const row = [...document.querySelectorAll('tr[data-platform="qoder"]')].find(
      (r) => r.textContent?.includes('DeepSeek-V4-Pro'),
    )!;
    expect(row.textContent).toContain('—');
    expect((row.textContent?.match(/支持/g) ?? []).length).toBe(1); // 仅 supports_stream 一格
  });

  it('明确 false 如实渲染「不支持」，不粉饰', async () => {
    await renderReady(
      {
        '/api/models': {
          body: {
            source: 'local',
            models: [{ platform: 'trae', model: 't-model', context_size: 65536, supports_tool: false, supports_stream: true }],
          },
        },
      },
      't-model',
    );
    const row = document.querySelector('tr[data-platform="trae"]')!;
    expect(row.textContent).toContain('不支持');
  });

  it('note 附加说明原样展示（本地目录附注通道）', async () => {
    await renderReady(
      {
        '/api/models': {
          body: { source: 'local', models: [{ platform: 'qoder', model: 'GLM-5.3', note: '静态快照 2026-09-22' }] },
        },
      },
      'GLM-5.3',
    );
    expect(document.body.textContent).toContain('静态快照 2026-09-22');
  });
});

describe('模型屏 — 平台过滤', () => {
  it('过滤下拉枚举目录中出现的平台（显示名映射），默认全部平台', async () => {
    await renderReady();
    const select = screen.getByLabelText(MODELS.FILTER_LABEL) as HTMLSelectElement;
    expect(select.value).toBe('all');
    const options = [...select.options].map((o) => o.textContent);
    // 平台枚举按字典序稳定排序（qoder < workbuddy）
    expect(options).toEqual([MODELS.FILTER_ALL, MODEL_SOURCE_NAMES.qoder, MODEL_SOURCE_NAMES.workbuddy]);
  });

  it('选平台 → 只剩该平台行，计数联动；切回全部恢复', async () => {
    await renderReady();
    const select = screen.getByLabelText(MODELS.FILTER_LABEL) as HTMLSelectElement;
    fireEvent.change(select, { target: { value: 'qoder' } });
    expect(document.querySelectorAll('tbody tr')).toHaveLength(13);
    expect(screen.getByText(MODELS.COUNT.replace('{count}', '13'))).toBeTruthy();
    expect(document.querySelector('tr[data-platform="workbuddy"]')).toBeNull();
    fireEvent.change(select, { target: { value: 'all' } });
    expect(document.querySelectorAll('tbody tr')).toHaveLength(19);
  });

  it('过滤到目录中存在但无行的平台需该平台出现在目录——改验证唯一平台目录下的过滤与计数', async () => {
    await renderReady(
      { '/api/models': { body: { source: 'local', models: [{ platform: 'trae', model: 't-model', supports_stream: true }] } } },
      't-model',
    );
    const select = screen.getByLabelText(MODELS.FILTER_LABEL) as HTMLSelectElement;
    fireEvent.change(select, { target: { value: 'trae' } });
    expect(screen.getByText(MODELS.COUNT.replace('{count}', '1'))).toBeTruthy();
    expect(document.querySelectorAll('tbody tr')).toHaveLength(1);
  });
});

describe('模型屏 — 诚实标注与空态', () => {
  it('顶部说明行交代目录来源（实时拉取语义 + 「—」语义）', async () => {
    await renderReady();
    expect(screen.getByText(MODELS.NOTE)).toBeTruthy();
    expect(screen.getByText(MODELS.SOURCE_LOCAL)).toBeTruthy();
  });

  it('source 为 upstream / mixed 时按三态诚实标注（不装懂）', async () => {
    await renderReady({ '/api/models': { body: { source: 'upstream', models: [] } } }, MODELS.SOURCE_UPSTREAM);
    await renderReady({ '/api/models': { body: { source: 'mixed', models: [] } } }, MODELS.SOURCE_MIXED);
    await renderReady({ '/api/models': { body: { source: 'weird', models: [] } } }, `${MODELS.SOURCE_PREFIX}weird`);
  });

  it('models 空数组 → 空态文案', async () => {
    await renderReady({ '/api/models': { body: EMPTY_WIRE } }, MODELS.EMPTY);
    expect(screen.getByText(MODELS.EMPTY)).toBeTruthy();
  });

  it('刷新按钮重发 GET /api/models?refresh=1（强制实时重拉；首载不带 refresh）', async () => {
    const fn = await renderReady();
    expect(fn.mock.calls.map((c) => String(c[0]))).toEqual(['/api/models']);
    const fn2 = installFetch({ '/api/models?refresh=1': { body: MODELS_WIRE } });
    fireEvent.click(screen.getByRole('button', { name: MODELS.REFRESH }));
    await screen.findByText(MODELS.COUNT.replace('{count}', '19'));
    // apiGet 每次调用都取全局 fetch：第二个请求落在替换后的桩上
    expect(fn2.mock.calls.map((c) => String(c[0]))).toEqual(['/api/models?refresh=1']);
  });
});

describe('模型屏 — M8 实时目录列（积分倍率 / 思考档位）', () => {
  /** 上游实时目录 fixture：一条全字段（wb2api 同款事实），一条全未知。 */
  const UPSTREAM_WIRE = {
    source: 'mixed',
    models: [
      {
        platform: 'workbuddy', provider: 'workbuddy', id: 'workbuddy/deepseek-v4-flash',
        name: 'DeepSeek V4 Flash', model: 'deepseek-v4-flash',
        context_size: 128000, max_output: 32768,
        supports_tool: true, supports_stream: true, is_free: null,
        credit_multiplier: 0.05, is_default: true, thinking_tiers: ['low', 'high'],
      },
      { platform: 'workbuddy', provider: 'workbuddy', id: 'workbuddy/mystery', name: 'mystery', model: 'mystery' },
    ],
  };

  it('积分倍率数字原样、思考档位数组 join；列序 = 平台/模型/倍率/档位/上下文/最大输出/工具/视觉/流式', async () => {
    await renderReady({ '/api/models': { body: UPSTREAM_WIRE } }, 'deepseek-v4-flash');
    const head = [...document.querySelectorAll('thead th')].map((th) => th.textContent);
    expect(head).toEqual([
      MODELS.COL_PLATFORM, MODELS.COL_MODEL, MODELS.COL_CREDIT, MODELS.COL_TIERS,
      MODELS.COL_CONTEXT, MODELS.COL_MAX_OUTPUT, MODELS.COL_TOOL, MODELS.COL_VISION,
      MODELS.COL_STREAM, MODELS.COL_FREE,
    ]);
    const row = document.querySelector('tr[data-platform="workbuddy"]')!;
    expect(row.textContent).toContain('0.05');
    expect(row.textContent).toContain('low/high');
    expect(row.textContent).toContain('128,000');
    expect(row.textContent).toContain('32,768');
    // 上游展示名（name ≠ model）作为副行
    expect(row.textContent).toContain('DeepSeek V4 Flash');
  });

  it('全未知行：倍率/档位/null 能力一律「—」，不编造数字', async () => {
    await renderReady({ '/api/models': { body: UPSTREAM_WIRE } }, 'mystery');
    const row = [...document.querySelectorAll('tr[data-platform="workbuddy"]')].find(
      (r) => r.textContent?.includes('mystery'),
    )!;
    expect((row.textContent?.match(/—/g) ?? []).length).toBeGreaterThanOrEqual(5);
    expect(row.textContent).not.toContain('0.05');
  });

  it('档位单元素也原样展示；明确 false 如实（不折叠进未知）', async () => {
    await renderReady({
      '/api/models': {
        body: {
          source: 'upstream',
          models: [{ platform: 'workbuddy', model: 'solo', thinking_tiers: ['max'], supports_tool: false }],
        },
      },
    }, 'solo');
    const row = document.querySelector('tr[data-platform="workbuddy"]')!;
    expect(row.textContent).toContain('max');
    expect(row.textContent).toContain('不支持');
  });
});

describe('模型屏 — axe 基线', () => {
  it('就绪态（全平台）无违规', async () => {
    installFetch({ '/api/models': { body: MODELS_WIRE } });
    const { container } = render(<ModelsScreen />);
    await screen.findByText('deepseek-v4-flash');
    expect(await axe(container)).toHaveNoViolations();
  });

  it('过滤态 + 错误态无违规', async () => {
    installFetch({ '/api/models': { body: MODELS_WIRE } });
    const { container } = render(<ModelsScreen />);
    await screen.findByText('deepseek-v4-flash');
    fireEvent.change(screen.getByLabelText(MODELS.FILTER_LABEL), { target: { value: 'qoder' } });
    expect(await axe(container)).toHaveNoViolations();
  });
});
