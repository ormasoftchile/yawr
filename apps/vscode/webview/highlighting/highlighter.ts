import { browserTokens, configureHighlighting } from './browserClient';
import { decodeEnvelope, safeOutputs, supportedDescriptor, type Descriptor, type ValueStatus } from '../../src/presentationProtocol';
import { decodePresentationState } from '../../src/presentationHistory';
import { parseStepDetails } from '../../src/stepDetails';
import { record } from '../../src/presentationProtocol';
import { decodeExpressionPresentation, safeExpressionText } from '../../src/expressionPresentationProtocol';
import { appendTokenText, currentExpressionSpans, verifiedExpressionValues } from './expressions';
import { overlayExpressionSpans } from '../../src/expressionPresentationColors';
import type { ColoredSpan } from '../../src/presentationScalar';
export { decodeEnvelope, safeOutputs };
export { projectWorkflow, workflowIssueIndex } from '../../src/workflowProjection';
export { selectDisplayPresentation, sanitizeDisplayPayload } from '../../src/displayPresentation';
export { retainDisplayObservation, reconcileDisplayDocument, reconcileDisplayOverlay } from '../../src/displayObservations';
export { loadDisplayWithdrawals, storeDisplayWithdrawals } from '../../src/displayObservationStorage';
export { parseDisplayJSON } from '../../src/displayPresentationJSON';
export function validateDocumentPresentation(value: unknown): unknown {
  const document = record(value);
  if (document.presentation_state !== undefined) document.presentation_state = decodePresentationState(document.presentation_state);
  if (Array.isArray(document.nodes)) for (const node of document.nodes) {
    const data = record(record(node).data);
    if (data.details !== undefined) {
      const details = record(data.details);
      if (typeof details.kind !== 'string') throw new Error('invalid-step-details');
      data.details = parseStepDetails(details, details.kind, 'preview');
    }
  }
  return document;
}
configureHighlighting('/preview/assets/highlighting/worker.js');

export function mountCode(container: HTMLElement, text: string | undefined, descriptor: Descriptor | undefined, label: string, status: ValueStatus = 'available', details?: unknown, expressionPath?: string): () => void {
  let cleanup = mountCodeOnce(container, text, descriptor, label, status, details, expressionPath);
  const observer = new MutationObserver(() => {
    cleanup();
    cleanup = mountCodeOnce(container, text, descriptor, label, status, details, expressionPath);
  });
  observer.observe(document.body, { attributes: true, attributeFilter: ['class', 'data-highlighting-enabled'] });
  return () => { observer.disconnect(); cleanup(); };
}
function mountCodeOnce(container: HTMLElement, text: string | undefined, descriptor: Descriptor | undefined, label: string, status: ValueStatus, details?: unknown, expressionPath?: string): () => void {
  const controller = new AbortController();
  const safe = status === 'available' || status === 'truncated' ? text : undefined;
  const figure = document.createElement('figure'), caption = document.createElement('figcaption');
  figure.className = 'yawr-code';
  caption.textContent = label + (status === 'truncated' ? ' · safe excerpt (truncated)' : safe === undefined ? ` · ${status}` : '');
  figure.append(caption); container.replaceChildren(figure);
  if (safe !== undefined) {
    const pre = document.createElement('pre'), code = document.createElement('code'), copy = document.createElement('button');
    code.textContent = safe; pre.append(code); figure.append(pre);
    copy.textContent = 'Copy'; copy.onclick = () => { void navigator.clipboard.writeText(safe); }; caption.append(copy);
    let host: ColoredSpan[] = [], expressions: ColoredSpan[] = [];
    const paint = () => {
      if (!controller.signal.aborted) appendTokenText(code, safe, overlayExpressionSpans(host, expressions));
    };
    if (supportedDescriptor(descriptor) && document.body.dataset.highlightingEnabled !== 'false') {
      void browserTokens(safe, descriptor, controller.signal).then(spans => { host = spans; paint(); },
        () => { if (!controller.signal.aborted) caption.prepend(document.createTextNode('Host colors unavailable · ')); });
    }
    if (expressionPath && safeExpressionText(details, expressionPath) === safe) {
      void verifiedExpressionValues(details, controller.signal).then(values => {
        const candidate = values.get(expressionPath);
        const value = candidate?.verifiedText === safe ? candidate : undefined;
        expressions = currentExpressionSpans(value);
        if (value) code.dataset.expressionPath = expressionPath;
        paint();
      });
    }
  }
  return () => controller.abort();
}
export function mountAuthoredExpressions(container: HTMLElement, details: unknown, exclude: string[] = []): () => void {
    const controller = new AbortController();
    container.replaceChildren();
    const envelope = decodeExpressionPresentation(details && typeof details === 'object'
      ? (details as Record<string, unknown>).expression_presentation : undefined);
    const nodes = new Map<string, { code: HTMLElement; text: string }>();
    for (const value of envelope?.values ?? []) {
      if (exclude.includes(value.path)) continue;
      const text = safeExpressionText(details, value.path);
      if (text === undefined) continue;
      const figure = document.createElement('figure'), caption = document.createElement('figcaption');
      const pre = document.createElement('pre'), code = document.createElement('code');
      figure.className = 'yawr-code'; caption.textContent = `Authored value · ${value.path}`;
      code.textContent = text; pre.append(code); figure.append(caption, pre); container.append(figure);
      nodes.set(value.path, { code, text });
    }
    let values: Awaited<ReturnType<typeof verifiedExpressionValues>> = new Map();
    const paint = () => {
      if (controller.signal.aborted) return;
      for (const [path, { code, text }] of nodes) {
        const candidate = values.get(path);
        const value = candidate?.verifiedText === text ? candidate : undefined;
        if (value) code.dataset.expressionPath = path;
        appendTokenText(code, text, currentExpressionSpans(value));
      }
    };
    const observer = new MutationObserver(paint);
    observer.observe(document.body, { attributes: true, attributeFilter: ['class', 'data-highlighting-enabled'] });
    void verifiedExpressionValues(details, controller.signal).then(result => { values = result; paint(); });
    return () => { controller.abort(); observer.disconnect(); };
}
export function highlightFences(container: HTMLElement): () => void {
  const cleanups: Array<() => void> = [];
  container.querySelectorAll('pre > code').forEach(code => {
    const language = ['sql', 'kql', 'powershell'].find(lang => code.classList.contains(`language-${lang}`));
    if (!language || !code.parentElement) return;
    const text = code.textContent ?? '', target = document.createElement('div');
    code.parentElement.replaceWith(target);
    cleanups.push(mountCode(target, text, { version: 1, kind: 'code', language }, `Markdown ${language} fence`));
  });
  return () => cleanups.forEach(cleanup => cleanup());
}
