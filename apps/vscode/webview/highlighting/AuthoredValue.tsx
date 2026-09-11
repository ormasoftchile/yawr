import React, { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import type { ExpressionDetailValue } from '../../src/expressionPresentationProtocol';
import { safeExpressionText } from '../../src/expressionPresentationProtocol';
import { currentExpressionSpans, verifiedExpressionValues, type VerifiedExpressionValue } from './expressions';
import type { ColoredSpan } from '../../src/presentationScalar';

const AuthoredContext = createContext<{ details?: unknown; values: Map<string, VerifiedExpressionValue> }>({ values: new Map() });
export function AuthoredValues({ details, children }: { details: unknown; children: ReactNode }) {
  const [verified, setVerified] = useState<{ details: unknown; values: Map<string, VerifiedExpressionValue> }>();
  const [, setAppearance] = useState(0);
  useEffect(() => {
    const observer = new MutationObserver(() => setAppearance(value => value + 1));
    observer.observe(document.body, { attributes: true, attributeFilter: ['class', 'data-highlighting-enabled'] });
    return () => observer.disconnect();
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    void verifiedExpressionValues(details, controller.signal).then(values => {
      if (!controller.signal.aborted) setVerified({ details, values });
    });
    return () => controller.abort();
  }, [details]);
  return <AuthoredContext.Provider value={{ details, values: verified && verified.details === details ? verified.values : new Map() }}>{children}</AuthoredContext.Provider>;
}
export function useAuthoredExpression(path: string | undefined, text: string | undefined): ExpressionDetailValue | undefined {
  const context = useContext(AuthoredContext);
  if (!path || text === undefined || typeof document === 'undefined' || document.body.dataset.highlightingEnabled === 'false' ||
    safeExpressionText(context.details, path) !== text) return undefined;
  const value = context.values.get(path);
  return value?.verifiedText === text ? value : undefined;
}
export function TokenText({ text, spans }: { text: string; spans: ColoredSpan[] }) {
  const parts: ReactNode[] = [];
  let offset = 0;
  for (const span of spans) {
    parts.push(text.slice(offset, span.start));
    parts.push(<span key={`${span.start}:${span.end}`} data-expression-class={span.expressionClass} style={{ color: span.color }}>{text.slice(span.start, span.end)}</span>);
    offset = span.end;
  }
  parts.push(text.slice(offset));
  return <>{parts}</>;
}
export function AuthoredValue({ value, path, depth = 0 }: { value: unknown; path: string; depth?: number }) {
  const expression = useAuthoredExpression(path, typeof value === 'string' ? value : undefined);
  if (typeof value === 'string') return <span className="authored-value" data-expression-path={expression ? path : undefined}>
    <TokenText text={value} spans={currentExpressionSpans(expression)} />
  </span>;
  if (depth >= 128) return <>…</>;
  if (value && typeof value === 'object') {
    const entries = Object.entries(value);
    return <>{Array.isArray(value) ? '[' : '{'}{entries.map(([key, child], index) => <React.Fragment key={key}>
      {index ? ', ' : ''}{Array.isArray(value) ? '' : `${JSON.stringify(key)}: `}
      <AuthoredValue value={child} path={`${path}/${key.replace(/~/g, '~0').replace(/\//g, '~1')}`} depth={depth + 1} />
    </React.Fragment>)}{Array.isArray(value) ? ']' : '}'}</>;
  }
  return <>{value === undefined ? '—' : String(value)}</>;
}
