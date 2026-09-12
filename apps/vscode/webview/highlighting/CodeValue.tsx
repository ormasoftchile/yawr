import React, { useEffect, useState } from 'react';
import { MAX_CODE_UNITS, supportedDescriptor, type Descriptor, type ValueStatus } from '../../src/presentationProtocol';
import type { ColoredSpan } from '../../src/presentationScalar';
import { browserTokens } from './browserClient';
import { useAuthoredExpression } from './AuthoredValue';
import { currentExpressionSpans } from './expressions';
import { overlayExpressionSpans } from '../../src/expressionPresentationColors';

export function CodeValue({ text, descriptor, label, status = 'available', expressionPath }: {
  text?: string; descriptor?: Descriptor; label: string; status?: ValueStatus; expressionPath?: string;
}) {
  const safe = status === 'available' || status === 'truncated' ? text : undefined;
  const expression = useAuthoredExpression(expressionPath, safe);
  const [rendered, setRendered] = useState<{ text: string; spans: ColoredSpan[] }>();
  const [reason, setReason] = useState('');
  const [appearance, setAppearance] = useState(0);
  useEffect(() => {
    const changed = () => setAppearance(value => value + 1);
    const observer = new MutationObserver(changed);
    observer.observe(document.body, { attributes: true, attributeFilter: ['class', 'data-highlighting-enabled'] });
    return () => observer.disconnect();
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    setRendered(undefined); setReason('');
    if (safe !== undefined && supportedDescriptor(descriptor) && document.body.dataset.highlightingEnabled !== 'false') {
      void browserTokens(safe, descriptor, controller.signal).then(
        spans => { if (!controller.signal.aborted) setRendered({ text: safe, spans }); },
        error => { if (!controller.signal.aborted) setReason(error instanceof Error ? error.message : 'unavailable'); },
      );
    }
    return () => controller.abort();
  }, [safe, descriptor?.language, descriptor?.version, status, appearance]);
  if (safe === undefined) return <div className="yawr-code"><strong>{label}</strong><p>{status}</p></div>;
  const spans = overlayExpressionSpans(rendered?.text === safe ? rendered.spans : [], currentExpressionSpans(expression));
  const parts: React.ReactNode[] = [];
  let offset = 0;
  for (const span of spans) {
    parts.push(safe.slice(offset, span.start));
    parts.push(<span key={`${span.start}:${span.end}`} data-expression-class={span.expressionClass} style={{ color: span.color }}>{safe.slice(span.start, span.end)}</span>);
    offset = span.end;
  }
  parts.push(safe.slice(offset));
  return <figure className="yawr-code">
    <figcaption>{label}{status === 'truncated' ? ' · safe excerpt (truncated)' : ''}
      {safe.length > MAX_CODE_UNITS ? ' · plaintext (highlighting limit)' : reason ? ` · plaintext (${reason})` : !descriptor ? ' · plaintext' : ''}
      <button type="button" onClick={() => { void navigator.clipboard.writeText(safe); }} aria-label={`Copy ${label}`}>Copy</button>
    </figcaption>
    <pre><code data-expression-path={expression ? expressionPath : undefined}>{parts}</code></pre>
  </figure>;
}
