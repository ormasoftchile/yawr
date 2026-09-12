import { parentPort } from 'node:worker_threads';
import { tokenize, warmup } from '../webview/highlighting/tokenizer';
import { resolveScalars, sourceSpans } from '../src/presentationScalar';
import type { ResolveReply } from '../src/presentationProtocol';
import type { Palette } from '../webview/highlighting/tokenizer';
import type { ExpressionResolveReply } from '../src/expressionPresentationProtocol';
import { resolveExpressionScalars } from '../src/expressionPresentationScalar';
import { expressionSpans, overlayExpressionSpans } from '../src/expressionPresentationColors';
void warmup().then(() => parentPort?.postMessage({ ready: true }), () => parentPort?.postMessage({ failed: true }));
parentPort?.on('message', async (value: { id: number; source: string; reply?: ResolveReply; expressions?: ExpressionResolveReply; palette: Palette }) => {
  try {
    const mapped = value.reply ? resolveScalars(value.source, value.reply) : { regions: [], reasons: [] };
    const expressions = resolveExpressionScalars(value.source, value.expressions);
    const spans = [];
    for (const region of mapped.regions) {
      try { spans.push(...sourceSpans(region, await tokenize(region.text, region.descriptor, value.palette), value.source)); }
      catch { mapped.reasons.push('tokenization-unavailable'); }
    }
    const expressionTokens = expressions.flatMap(region => sourceSpans(region, expressionSpans(region.expression, value.palette), value.source));
    parentPort?.postMessage({ id: value.id, spans: overlayExpressionSpans(spans.sort((a,b) => a.start - b.start), expressionTokens), reasons: mapped.reasons });
  } catch { parentPort?.postMessage({ id: value.id, spans: [], reasons: ['tokenization-unavailable'] }); }
});
