import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './styles/tokens.css';
import './styles/shell.css';
import './styles/panel.css';
import { PanelApp } from './app/PanelApp';

const host = document.getElementById('root');
if (!host) {
  // #root 由 index.html 提供；缺失说明构建产物被改动。显式失败优于一句不吭的白屏。
  throw new Error('panel entry: #root not found');
}

createRoot(host).render(
  <StrictMode>
    <PanelApp />
  </StrictMode>,
);
