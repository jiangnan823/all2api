import { vi } from 'vitest';

// React 19 需要显式声明 act 测试环境；jsdom 无 ResizeObserver，组件已做存在性判断。
vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
