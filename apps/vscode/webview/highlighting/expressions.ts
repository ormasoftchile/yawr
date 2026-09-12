import { decodeExpressionPresentation, expressionTextMatches, safeExpressionText, type ExpressionDetailValue } from '../../src/expressionPresentationProtocol';
import type { ColoredSpan } from '../../src/presentationScalar';
import { expressionSpans } from '../../src/expressionPresentationColors';
import { palette } from './browserClient';

export interface VerifiedExpressionValue extends ExpressionDetailValue { verifiedText: string }
export async function verifiedExpressionValues(details: unknown, signal: AbortSignal): Promise<Map<string, VerifiedExpressionValue>> {
  const result = new Map<string, VerifiedExpressionValue>();
  if (!details || typeof details !== 'object') return result;
  const envelope = decodeExpressionPresentation((details as Record<string, unknown>).expression_presentation);
  if (!envelope) return result;
  for (const value of envelope.values) {
    if (signal.aborted) return new Map();
    const text = safeExpressionText(details, value.path);
    if (text === undefined || text.length !== value.text_length) continue;
    try {
      const bytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text));
      const digest = 'sha256:' + [...new Uint8Array(bytes)].map(byte => byte.toString(16).padStart(2, '0')).join('');
      if (expressionTextMatches(text, value, digest)) result.set(value.path, { ...value, verifiedText: text });
    } catch { /* Unavailable digest verification means plaintext, not guessed spans. */ }
  }
  return signal.aborted ? new Map() : result;
}
export function currentExpressionSpans(value?: ExpressionDetailValue): ColoredSpan[] {
  return value && document.body.dataset.highlightingEnabled !== 'false' ? expressionSpans(value, palette()) : [];
}
export function appendTokenText(container: HTMLElement, text: string, spans: ColoredSpan[]): void {
  const fragments: Node[] = [];
  let offset = 0;
  for (const span of spans) {
    fragments.push(document.createTextNode(text.slice(offset, span.start)));
    const token = document.createElement('span');
    if (span.expressionClass) token.dataset.expressionClass = span.expressionClass;
    token.style.color = span.color; token.textContent = text.slice(span.start, span.end);
    fragments.push(token); offset = span.end;
  }
  fragments.push(document.createTextNode(text.slice(offset)));
  container.replaceChildren(...fragments);
}
