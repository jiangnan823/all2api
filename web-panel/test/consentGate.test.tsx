import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { ConsentGate, isReadToBottom } from '../src/components/ConsentGate';
import { consentRequestBody, consentRejectionOf } from '../src/lib/consent';
import { CONSENT } from '../src/i18n/zh';

const SH = 1000; // scrollHeight
const CH = 400; // clientHeight

function stub(el: HTMLElement, scrollTop: number) {
  Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => SH });
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => CH });
  Object.defineProperty(el, 'scrollTop', { configurable: true, get: () => scrollTop, set: () => {} });
}

function gate(props: Partial<Parameters<typeof ConsentGate>[0]> = {}) {
  const onAck = vi.fn();
  const onAbort = vi.fn();
  render(
    <ConsentGate
      source="qoder"
      noticeVersion="1"
      onAck={onAck}
      onAbort={onAbort}
      {...props}
    />,
  );
  return { onAck, onAbort, region: screen.getByRole('region', { name: CONSENT.REGION_LABEL }) };
}

function checkbox() {
  return screen.getByRole('checkbox') as HTMLInputElement;
}
function continueBtn() {
  return screen.getByRole('button', { name: CONSENT.CONTINUE }) as HTMLButtonElement;
}
function abortBtn() {
  return screen.getByRole('button', { name: CONSENT.ABORT }) as HTMLButtonElement;
}

afterEach(cleanup);

describe('isReadToBottom — §5.19 判定式', () => {
  it('差 8px 内即算读到底，8px 之外不算', () => {
    expect(isReadToBottom({ scrollTop: SH - CH - 9, clientHeight: CH, scrollHeight: SH })).toBe(false);
    expect(isReadToBottom({ scrollTop: SH - CH - 8, clientHeight: CH, scrollHeight: SH })).toBe(true);
    expect(isReadToBottom({ scrollTop: SH - CH, clientHeight: CH, scrollHeight: SH })).toBe(true);
  });
});

describe('ConsentGate — 门控三态', () => {
  it('态①：未读到底 → 勾选框与继续都禁用，放弃仍可用', () => {
    const { region } = gate();
    stub(region, 0);
    fireEvent.scroll(region);
    expect(checkbox().disabled).toBe(true);
    expect(continueBtn().disabled).toBe(true);
    expect(abortBtn().disabled).toBe(false);
  });

  it('态②：读到底未勾选 → 勾选框可用、继续仍禁用', () => {
    const { region } = gate();
    stub(region, SH - CH);
    fireEvent.scroll(region);
    expect(checkbox().disabled).toBe(false);
    expect(checkbox().checked).toBe(false);
    expect(continueBtn().disabled).toBe(true);
    expect(screen.getByRole('status').textContent).toContain(CONSENT.REACHED);
  });

  it('态③：读到底且已勾选 → 继续可用', () => {
    const { region } = gate();
    stub(region, SH - CH);
    fireEvent.scroll(region);
    fireEvent.click(checkbox());
    expect(continueBtn().disabled).toBe(false);
  });

  it('键盘/程序滚动同样计入：滚动容器可聚焦', () => {
    const { region } = gate();
    expect(region.getAttribute('tabindex')).toBe('0');
    region.focus();
    stub(region, SH - CH - 8);
    fireEvent.scroll(region);
    expect(checkbox().disabled).toBe(false);
  });
});

describe('ConsentGate — 三条禁则', () => {
  it('不预勾选', () => {
    gate();
    expect(checkbox().checked).toBe(false);
  });

  it('不倒计时自动勾选：未滚动时永远保持禁用', () => {
    vi.useFakeTimers();
    const { region } = gate();
    stub(region, 0);
    fireEvent.scroll(region);
    vi.advanceTimersByTime(30_000);
    expect(checkbox().disabled).toBe(true);
    expect(checkbox().checked).toBe(false);
    vi.useRealTimers();
  });

  it('放弃接入不需要任何前置，且与继续同级可达', () => {
    const { onAbort } = gate();
    fireEvent.click(abortBtn());
    expect(onAbort).toHaveBeenCalledTimes(1);
    expect(abortBtn().className).toContain('btn');
    expect(document.body.textContent).toContain(CONSENT.ABORT_HINT);
  });

  it('内容不足一屏时不卡死（无"文末"可滚），但勾选仍需人手', () => {
    gate();
    const region = screen.getByRole('region', { name: CONSENT.REGION_LABEL });
    Object.defineProperty(region, 'scrollHeight', { configurable: true, get: () => 200 });
    Object.defineProperty(region, 'clientHeight', { configurable: true, get: () => 400 });
    Object.defineProperty(region, 'scrollTop', { configurable: true, get: () => 0, set: () => {} });
    fireEvent.scroll(region);
    expect(checkbox().disabled).toBe(false);
    expect(checkbox().checked).toBe(false);
    expect(continueBtn().disabled).toBe(true);
  });
});

describe('ConsentGate — 留痕最小化（G3 / O-5）', () => {
  it('请求体只有 source + notice_version 两个键，不含凭证内容', () => {
    const { onAck, region } = gate();
    stub(region, SH - CH);
    fireEvent.scroll(region);
    fireEvent.click(checkbox());
    fireEvent.click(continueBtn());

    expect(onAck).toHaveBeenCalledTimes(1);
    const ack = onAck.mock.calls[0][0];
    expect(ack.source).toBe('qoder');
    expect(ack.noticeVersion).toBe('1');
    expect(typeof ack.ackAt.unix).toBe('number');
    expect(Object.keys(consentRequestBody(ack)).sort()).toEqual(['notice_version', 'source']);
    expect(JSON.stringify(consentRequestBody(ack))).not.toMatch('credential');
  });

  it('noticeVersion 变更后随请求体透传，前端不自造版本', () => {
    const { onAck, region } = gate({ noticeVersion: '2' });
    stub(region, SH - CH);
    fireEvent.scroll(region);
    fireEvent.click(checkbox());
    fireEvent.click(continueBtn());
    expect(consentRequestBody(onAck.mock.calls[0][0])).toEqual({ source: 'qoder', notice_version: '2' });
  });

  it('组件不碰网络', () => {
    const spy = vi.spyOn(globalThis, 'fetch');
    gate();
    expect(spy).not.toHaveBeenCalled();
    spy.mockRestore();
  });
});

describe('consent 拒绝分派（TC-M1-029：只认 code，不匹配中文 message）', () => {
  it('consent_missing / consent_stale 各自命中，其余返回 null', () => {
    expect(consentRejectionOf([{ key: 'consent.notice_version', message: '请先阅读并勾选接入须知', code: 'consent_missing' }]))
      .toBe('consent_missing');
    expect(consentRejectionOf([{ key: 'consent.notice_version', message: '须知已更新', code: 'consent_stale' }]))
      .toBe('consent_stale');
    expect(consentRejectionOf([{ key: 'sources.qoder.throttle.steady_rpm', message: '越界', code: 'range' }])).toBeNull();
    expect(consentRejectionOf(undefined)).toBeNull();
  });

  it('message 扰动不影响分派结果', () => {
    const a = consentRejectionOf([{ key: 'k', message: '文案甲', code: 'consent_stale' }]);
    const b = consentRejectionOf([{ key: 'k', message: '完全不同的一句话', code: 'consent_stale' }]);
    expect(a).toBe(b);
  });
});

describe('ConsentGate — §9 逐字基准（Qoder 硬要求，禁止删弱）', () => {
  const text = () => document.body.textContent ?? '';

  it('两条条款口径 + 出处 + 防误读半句原样在场', () => {
    gate();
    expect(text()).toContain('不得将账户凭证提供给第三方访问你的账户');
    expect(text()).toContain('账户一旦被终止将无法重新激活');
    expect(text()).toContain('2.3 账户安全、2.4 处置情形、15.2 终止不可恢复');
    expect(text()).toContain('保守频率仅降低触发风控的概率，不能消除上述条款风险。');
  });

  it('通用四段 / 收尾两段 / 勾选标签齐全', () => {
    gate();
    expect(text()).toContain(CONSENT.GENERAL_TITLE);
    for (const line of CONSENT.GENERAL_ITEMS) expect(text()).toContain(line);
    expect(text()).toContain(CONSENT.CLOSING_TITLE);
    for (const line of CONSENT.CLOSING_ITEMS) expect(text()).toContain(line);
    expect(text()).toContain('我已阅读并理解上述风险');
    expect(text()).toContain('这是我本人的 Qoder 账号');
  });

  it('新增来源只加一段文案，结构不变（未知来源回退）', () => {
    gate({ source: 'newplatform' });
    expect(text()).toContain('该来源需注意');
    expect(checkbox().disabled).toBe(true);
  });
});

describe('ConsentGate — copy §9 v0.1.2 WorkBuddy / Trae 逐字基准（PM seq=438）', () => {
  const text = () => document.body.textContent ?? '';

  it('WorkBuddy 三条逐字在场，含「代你执行例行操作」与保活项', () => {
    gate({ source: 'workbuddy' });
    expect(text()).toContain('WorkBuddy 账号需特别注意');
    expect(text()).toContain(
      '除了转发你的对话请求，网关还会代你执行例行操作：自动签到、活跃上报、凭证保活。这类操作等同于让网关代替你操作账号，平台若判定异常，可能限流或使凭证失效。',
    );
    expect(text()).toContain(
      '出现「凭证已过期」提示时，通常是平台的处置表现，不是你设备或网关的故障；出路是回到账号页重新授权即可恢复。',
    );
    expect(text()).toContain(
      '本来源默认节奏已按保守档配置：稳态 6 次/分钟、日请求上限 600、请求间随机间隔 0.1–0.9 秒、同一时刻最多 2 个在途请求。保守频率仅降低触发风控的概率，不能消除上述风险。',
    );
  });

  it('Trae 三条逐字在场，含额度规则不公开与本机撤换说明', () => {
    gate({ source: 'trae' });
    expect(text()).toContain('Trae 账号需特别注意');
    expect(text()).toContain(
      'Trae 的免费额度规则由平台不公开说明，平台可随时调整额度或可用模型。额度缩水、模型下线属平台行为，非网关故障；网关无法替你承诺额度长期不变。',
    );
    expect(text()).toContain(
      '本来源默认节奏已按最保守档配置：稳态 2 次/分钟、日请求上限 100、请求间随机间隔 0.5–2 秒、同一时刻仅 1 个在途请求。保守频率仅降低触发风控的概率，不能消除上述风险。',
    );
    expect(text()).toContain(
      '设备验证只是把你已授予的授权换取凭证保存在本机；随时可在「账号管理」删除账号并撤销留存凭证。',
    );
  });

  it('WorkBuddy 段只出现本段数值，不串写他源数值', () => {
    gate({ source: 'workbuddy' });
    expect(text()).toContain('稳态 6 次/分钟、日请求上限 600');
    expect(text()).not.toContain('稳态 4 次/分钟');
    expect(text()).not.toContain('稳态 2 次/分钟');
  });

  it('Trae 段只出现本段数值，不串写他源数值', () => {
    gate({ source: 'trae' });
    expect(text()).toContain('稳态 2 次/分钟、日请求上限 100');
    expect(text()).not.toContain('稳态 4 次/分钟');
    expect(text()).not.toContain('稳态 6 次/分钟');
  });

  it('Qoder 段数值与末句措辞未被 W/T 基准带偏（上述条款风险 vs 上述风险）', () => {
    gate({ source: 'qoder' });
    expect(text()).toContain('稳态 4 次/分钟、日请求上限 300');
    // W/T 段末句逐字为「不能消除上述风险。」，Qoder 段为「…上述条款风险。」——两种基准形态都不得归一
    expect(text()).toContain('不能消除上述条款风险。');
    expect(text()).not.toContain('不能消除上述风险。');
  });
});

describe('ConsentGate — scrollRequired=false 出口', () => {
  it('关闭滚动门槛时勾选框直接可用（勾选仍是人工动作）', () => {
    gate({ scrollRequired: false });
    expect(checkbox().disabled).toBe(false);
    expect(continueBtn().disabled).toBe(true);
  });
});
