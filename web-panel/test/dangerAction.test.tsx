import { describe, it, expect, vi, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { DangerAction, type DangerActionProps } from '../src/components/DangerAction';
import { AdvancedToggle } from '../src/components/AdvancedToggle';
import { CONSENT, DANGER, ADVANCED } from '../src/i18n/zh';

/**
 * UI UE seq=444 裁定的 §5.13 六条冻结口径（D2-001）→ 可执行断言。
 * 口径本体只在 props v0.2.5，这里只做消费侧验证。
 */
afterEach(cleanup);

const WORD = ADVANCED.CONFIRM_WORD;

function dialog(opts: { onCancel?: () => void; onConfirm?: () => void } = {}) {
  const onConfirm = opts.onConfirm ?? vi.fn();
  const onCancel = opts.onCancel;
  const view = render(
    <DangerAction confirmWord={WORD} impactSummary='只影响这一个来源' onConfirm={onConfirm} onCancel={onCancel} />,
  );
  return { view, onConfirm, cancel: onCancel };
}

function cancelBtn() {
  return screen.getByRole('button', { name: DANGER.CANCEL }) as HTMLButtonElement;
}

describe('D2-001 口径 1｜名字只用 onCancel', () => {
  it('实装字段清单 = confirmWord / impactSummary / onConfirm / onCancel?（无 title、无 onDismiss/onClose/onCancelReason）', () => {
    const src = readFileSync(join(process.cwd(), 'src/components/DangerAction.tsx'), 'utf8');
    const body = src.slice(
      src.indexOf('export interface DangerActionProps'),
      src.indexOf('}', src.indexOf('onCancel')),
    );
    const fields = [...body.matchAll(/^\s{2}(\w+)(\??):/gm)].map((m) => `${m[1]}${m[2] ? '?' : ''}`);
    expect(fields.sort()).toEqual(['confirmWord', 'impactSummary', 'onCancel?', 'onConfirm']);
    for (const alias of ['onDismiss', 'onClose', 'onCancelReason', 'title']) {
      expect(body, `${alias} 不得成为第二个名字`).not.toContain(alias);
    }
  });

  it('TypeScript 侧同形：只给四个字段即可通过类型检查', () => {
    const props: DangerActionProps = { confirmWord: WORD, impactSummary: 'x', onConfirm: vi.fn(), onCancel: vi.fn() };
    expect(Object.keys(props).sort()).toEqual(['confirmWord', 'impactSummary', 'onCancel', 'onConfirm']);
  });
});

describe('D2-001 口径 2｜所有非肯定关闭路径统一走 onCancel', () => {
  it('「保持关闭」按钮', () => {
    const onCancel = vi.fn();
    dialog({ onCancel });
    fireEvent.click(cancelBtn());
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('Esc——即使已逐字输入确认词也照样是关闭路径，不是提交', () => {
    const onCancel = vi.fn();
    const { onConfirm } = dialog({ onCancel });
    fireEvent.change(screen.getByRole('textbox'), { target: { value: WORD } });
    expect(cancelBtn()).toBeTruthy();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('遮罩点击（点在面板内不关闭）', () => {
    const onCancel = vi.fn();
    const { view } = dialog({ onCancel });
    const scrim = view.container.firstElementChild as HTMLElement;
    fireEvent.mouseDown(scrim);
    expect(onCancel).toHaveBeenCalledTimes(1);
    fireEvent.mouseDown(screen.getByRole('dialog'));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});

describe('D2-001 口径 3/4｜单态 + 取消零副作用', () => {
  it('取消不发 patch、不改开关、不写 ConsentAck、不产生 FieldError', () => {
    const onToggle = vi.fn();
    render(
      <AdvancedToggle
        source="qoder"
        flagKey="request_fingerprint_rewrite"
        enabled={false}
        confirmCopy={ADVANCED.NOTE}
        onToggle={onToggle}
      />,
    );
    fireEvent.click(screen.getByRole('switch'));
    expect(screen.getByRole('dialog')).toBeTruthy();
    fireEvent.click(cancelBtn());
    expect(onToggle).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).toBeNull();
    expect((screen.getByRole('switch') as HTMLButtonElement).getAttribute('aria-checked')).toBe('false');
    expect(document.querySelector('[role="alert"]')).toBeNull();
  });
});

describe('D2-001 口径 5｜不传 onCancel 时组件必须自持关闭能力', () => {
  it('没有外部关闭回调也能关，且仍然零写回（禁止出现没有出口的危险确认框）', () => {
    const { onConfirm } = dialog();
    expect(screen.getByRole('dialog')).toBeTruthy();
    fireEvent.click(cancelBtn());
    expect(onConfirm).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('Esc 在无 onCancel 时同样自持关闭', () => {
    dialog();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('遮罩点击在无 onCancel 时同样自持关闭', () => {
    const { view } = dialog();
    fireEvent.mouseDown(view.container.firstElementChild as HTMLElement);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('下一次挂载重新可用（自持关闭是一次性收敛，不是永久失效）', () => {
    const first = dialog();
    fireEvent.click(cancelBtn());
    expect(screen.queryByRole('dialog')).toBeNull();
    first.view.unmount();
    dialog();
    expect(screen.getByRole('dialog')).toBeTruthy();
  });
});

describe('D2-001 口径 6｜关闭后焦点回到触发该弹窗的控件', () => {
  it('从该源改写开关打开→取消→焦点落回同一个开关', () => {
    render(
      <AdvancedToggle
        source="qoder"
        flagKey="request_fingerprint_rewrite"
        enabled={false}
        confirmCopy={ADVANCED.NOTE}
        onToggle={vi.fn()}
      />,
    );
    const sw = screen.getByRole('switch') as HTMLButtonElement;
    sw.focus();
    fireEvent.click(sw);
    expect(document.activeElement).not.toBe(sw); // 默认焦点已在「保持关闭」
    expect(document.activeElement?.textContent).toBe(DANGER.CANCEL);
    fireEvent.click(cancelBtn());
    expect(document.activeElement).toBe(sw);
  });
});

describe('红线未回退', () => {
  it('危险按钮仍是描边不填充，且默认焦点在安全侧', () => {
    dialog({ onCancel: vi.fn() });
    const dangerous = screen.getByRole('button', { name: DANGER.CONFIRM }) as HTMLButtonElement;
    expect(dangerous.className).toContain('btn--danger-outline');
    expect(dangerous.disabled).toBe(true);
    expect(document.activeElement?.textContent).toBe(DANGER.CANCEL);
  });

  it('取消出口不进留痕链：ConsentGateProps 里仍只有 onAck + onAbort，没有第三个确认回调', () => {
    expect(CONSENT.CHECK_LABEL).toContain('{source}');
    // 唯一留痕事件仍是 onAck（source + notice_version + 服务端时间戳）；此处只钉住"未开出第二个证据面"
    const consentSrc = readFileSync(join(process.cwd(), 'src/components/ConsentGate.tsx'), 'utf8');
    expect(consentSrc).toContain('onAck');
    expect(consentSrc).not.toContain('onCancel');
  });
});
