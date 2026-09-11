import type { ResolveReply } from './presentationProtocol';
import type { ColoredSpan } from './presentationScalar';

export function presentationStatus(spans: ColoredSpan[], reply: ResolveReply | undefined, reasons: string[], failure?: string): string {
  const host = spans.some(span => !span.macro), expressions = spans.some(span => span.macro);
  const reason = failure ?? reply?.reason ?? reply?.bindings.find(binding => binding.status !== 'resolved')?.reason ??
    reasons.find(reason => reason !== 'missing-descriptor');
  const channels = [
    host ? reason ? 'code partially highlighted' : 'code highlighted' : '',
    expressions ? 'expressions highlighted' : '',
  ].filter(Boolean);
  if (reason) channels.push(`code ${host ? 'partial' : 'unavailable'}: ${reason}`);
  else if (!host) channels.push('no declared code');
  return channels.join(' · ');
}
