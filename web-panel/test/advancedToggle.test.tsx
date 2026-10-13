import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { AdvancedToggle } from '../src/components/AdvancedToggle';
import { ADVANCED, DANGER } from '../src/i18n/zh';
import { fingerprintRewriteEnabled } from '../src/lib/throttle';

const CONFIRM = '该来源的请求特征将被改写，可能触发平台风控。';

afterEach(cleanup);

function setup(enabled: boolean) {
  const onToggle = vi.fn();
  render(
    <AdvancedToggle
      source="qoder"
      flagKey="request_fingerprint_rewrite"
      enabled={enabled}
      confirmCopy={CONFIRM}
      onToggle={onToggle}
    />,
  );
  return { onToggle, sw: screen.getByRole('switch') as HTMLButtonElement };
}

describe('AdvancedToggle — 安全默认（F-V1）', () => {
  it('advanced 缺失、源缺失、键缺失一律按 false', () => {
    expect(fingerprintRewriteEnabled(undefined, 'qoder')).toBe(false);
    expect(fingerprintRewriteEnabled({}, 'qoder')).toBe(false);
    expect(fingerprintRewriteEnabled({ trae: { fingerprintRewrite: true } }, 'qoder')).toBe(false);
    expect(fingerprintRewriteEnabled({ qoder: undefined }, 'qoder')).toBe(false);
    expect(fingerprintRewriteEnabled({ qoder: { fingerprintRewrite: true } }, 'qoder')).toBe(true);
  });

  it('关闭态渲染「已关闭」，开关不落第二落点', () => {
    setup(false);
    expect(screen.getByRole('switch').getAttribute('aria-checked')).toBe('false');
    expect(document.body.textContent).toContain(ADVANCED.OFF);
    const el = document.querySelector('.adv-toggle');
    expect(el?.getAttribute('data-flag-path')).toBe('sources.qoder.advanced.request_fingerprint_rewrite');
  });
});

describe('AdvancedToggle — 开启走该源一次独立 DangerAction', () => {
  it('点击开启只弹确认，不直接改写', () => {
    const { onToggle } = setup(false);
    fireEvent.click(screen.getByRole('switch'));
    expect(onToggle).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog')).not.toBeNull();
  });

  it('确认按钮在逐字输入确认词前不可用', () => {
    setup(false);
    fireEvent.click(screen.getByRole('switch'));
    const confirm = screen.getByRole('button', { name: DANGER.CONFIRM }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '开启' } });
    expect(confirm.disabled).toBe(true);
    fireEvent.change(screen.getByRole('textbox'), { target: { value: ADVANCED.CONFIRM_WORD } });
    expect(confirm.disabled).toBe(false);
  });

  it('默认焦点在「保持关闭」，取消不产生任何写回', () => {
    const { onToggle } = setup(false);
    fireEvent.click(screen.getByRole('switch'));
    expect(document.activeElement?.textContent).toBe(DANGER.CANCEL);
    fireEvent.click(screen.getByRole('button', { name: DANGER.CANCEL }));
    expect(onToggle).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('确认后才 onToggle(true)，影响面摘要点明只影响该源', () => {
    const { onToggle } = setup(false);
    fireEvent.click(screen.getByRole('switch'));
    const dialog = screen.getByRole('dialog');
    expect(dialog.textContent).toContain('仅影响 Qoder 这一个来源');
    expect(dialog.textContent).toContain(CONFIRM);
    fireEvent.change(screen.getByRole('textbox'), { target: { value: ADVANCED.CONFIRM_WORD } });
    fireEvent.click(screen.getByRole('button', { name: DANGER.CONFIRM }));
    expect(onToggle).toHaveBeenCalledWith(true);
  });

  it('措辞用「请求特征改写」，并披露按源生效', () => {
    setup(false);
    const text = document.body.textContent ?? '';
    expect(text).toContain(ADVANCED.COLUMN);
    expect(text).toContain('每个来源逐一生效');
    expect(text).toContain('不波及其他来源');
  });
});

describe('AdvancedToggle — 关闭是安全方向', () => {
  it('已开启时点击直接关，不弹确认', () => {
    const { onToggle } = setup(true);
    fireEvent.click(screen.getByRole('switch'));
    expect(onToggle).toHaveBeenCalledWith(false);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('无"一次开全部"入口：本组件不渲染任何批量按钮', () => {
    setup(false);
    const text = document.body.textContent ?? '';
    expect(text).not.toMatch(/全部开启|一键开启/);
    expect(screen.queryAllByRole('button')).toHaveLength(0);
  });
});
