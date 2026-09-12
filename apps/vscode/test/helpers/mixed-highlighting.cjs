const assert = require('node:assert/strict');
const path = require('node:path');
const { Worker } = require('node:worker_threads');
const { parseDocument } = require('yaml');
const { resolveCodePresentation } = require('../../out/presentationClient');
const { resolveExpressionPresentation } = require('../../out/expressionPresentationClient');
const { resolveScalars } = require('../../out/presentationScalar');
const { pointerParts } = require('../../out/expressionPresentationProtocol');

function workerTokens(message) {
  return new Promise((resolve, reject) => {
    const worker = new Worker(path.join(__dirname, '..', '..', 'out', 'highlighting-worker.cjs'));
    let started;
    const timer = setTimeout(() => { void worker.terminate(); reject(new Error('worker-timeout')); }, 5000);
    worker.on('error', reject);
    worker.on('message', result => {
      if (result.ready) { started = performance.now(); worker.postMessage({ id: 1, palette: 'dark', ...message }); }
      else { clearTimeout(timer); void worker.terminate(); resolve({ ...result, elapsedMs: performance.now() - started }); }
    });
  });
}
function colorAt(spans, offset) { return spans.find(span => span.start <= offset && offset < span.end)?.color; }
async function inspectMixed(helper, request, nodeID) {
  const started = performance.now();
  const [code, expressions] = await Promise.all([
    resolveCodePresentation(helper, request), resolveExpressionPresentation(helper, request),
  ]);
  const source = request.document.text, doc = parseDocument(source, { keepSourceTokens: true });
  const region = code.reply?.regions.find(region => region.status === 'resolved' &&
    doc.getIn([...pointerParts(region.yaml_path).slice(0, -3), 'id']) === nodeID);
  const expression = expressions?.regions.find(value => value.yaml_path === region?.yaml_path);
  const resolveMs = performance.now() - started;
  const [baseline, combined] = await Promise.all([
    workerTokens({ source, reply: code.reply }), workerTokens({ source, reply: code.reply, expressions }),
  ]);
  const within = span => region && span.start >= region.range.start && span.end <= region.range.end;
  const host = combined.spans.filter(span => within(span) && !span.macro);
  const originalHost = baseline.spans.filter(span => within(span) && !span.macro);
  let comparedCodeUnits = 0;
  for (const span of originalHost) {
    for (let offset = span.start; offset < span.end; offset++) {
      assert.equal(colorAt(combined.spans, offset), span.color, `Host color changed at ${offset}`);
      comparedCodeUnits++;
    }
  }
  return {
    status: code.reply?.status, reason: code.reason ?? code.reply?.reason, resolveMs,
    bindings: code.reply?.bindings.map(({ name, status, reason }) => ({ name, status, reason })),
    regions: code.reply?.regions, dependencies: code.reply?.dependencies.length,
    scalarReasons: code.reply ? resolveScalars(source, code.reply).reasons : [],
    expressionStatus: expressions?.status, expressionRegions: expressions?.regions.length,
    region, expression: expression && { yaml_path: expression.yaml_path, range: expression.range,
      tokens: expression.tokens }, comparedCodeUnits,
    baseline: { elapsedMs: baseline.elapsedMs, hostCount: originalHost.length, total: baseline.spans.length, reasons: baseline.reasons },
    combined: { elapsedMs: combined.elapsedMs, hostCount: host.length, total: combined.spans.length, reasons: combined.reasons },
    hostTokens: host.map(span => ({ ...span, text: source.slice(span.start, span.end) })),
    expressionTokens: combined.spans.filter(span => within(span) && span.macro)
      .map(span => ({ ...span, text: source.slice(span.start, span.end) })),
  };
}
module.exports = { inspectMixed, workerTokens, colorAt };
