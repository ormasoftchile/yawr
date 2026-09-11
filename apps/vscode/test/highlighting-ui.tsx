import React from 'react';
import { createRoot } from 'react-dom/client';
import { StepInspector } from '../webview/inspector';
import { configureHighlighting } from '../webview/highlighting/browserClient';
import { parseGraphDocument } from '../src/directGraphPreview';
import '../webview/graph.css';

declare function acquireVsCodeApi(): { postMessage(value: unknown): void };
const api = acquireVsCodeApi();
configureHighlighting(document.body.dataset.worker ?? '');
const root = createRoot(document.getElementById('root')!);
const copies: string[] = [];
Object.defineProperty(navigator, 'clipboard', { value: { writeText: async (text: string) => { copies.push(text); } } });
window.addEventListener('message', event => {
  const message = event.data;
  if (message?.type !== 'graph') return;
  if (['vscode-light', 'vscode-dark', 'vscode-high-contrast', 'vscode-high-contrast-light'].includes(message.theme)) {
    document.body.className = message.theme;
  }
  const graph = parseGraphDocument(JSON.stringify(message.graph));
  root.render(<main>{graph.nodes.filter(node => node.data.details?.code_presentation || node.data.details?.expression_presentation).map(node => {
    const retained = graph.presentation_state?.occurrences.filter(o => o.identity.qualified_node_id === node.id) ?? [];
    return <section key={node.id} data-node={node.id}>
      <StepInspector node={node} runtime={{ status: 'retained', retainedPresentations: retained }} debugControls={null} />
      <StepInspector node={node} runtime={undefined} debugControls={null} />
    </section>;
  })}</main>);
  let attempts = 0;
  const timer = setInterval(() => {
    const code = [...document.querySelectorAll('.yawr-code pre code')];
    const expressionsExpected = graph.nodes.some(node => node.data.details?.expression_presentation?.values.some(v => v.tokens.length));
    if ((!code.length || code.some(value => !value.querySelector('span')) ||
      expressionsExpected && !document.querySelector('[data-expression-class]')) && attempts++ < 60) return;
    clearInterval(timer);
    document.querySelectorAll<HTMLButtonElement>('.yawr-code button').forEach(button => button.click());
    api.postMessage({ type: 'rendered', code: code.map(value => ({
      text: value.textContent, spans: value.querySelectorAll('span').length,
      colors: [...new Set([...value.querySelectorAll('span')].map(span => getComputedStyle(span).color))],
      whiteSpace: getComputedStyle(value.closest('pre')!).whiteSpace,
      label: value.closest('figure')?.querySelector('figcaption')?.textContent,
    })), expressions: [...document.querySelectorAll<HTMLElement>('[data-expression-path]')].map(value => ({
      path: value.dataset.expressionPath, text: value.textContent,
      tokens: [...value.querySelectorAll<HTMLElement>('[data-expression-class]')].map(token => ({
        class: token.dataset.expressionClass, text: token.textContent, color: getComputedStyle(token).color,
      })),
    })), copies, text: document.body.textContent, html: document.body.innerHTML });
  }, 100);
});
api.postMessage({ type: 'ready' });
