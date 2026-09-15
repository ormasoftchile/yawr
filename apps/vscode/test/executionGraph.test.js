const test = require('node:test');
const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const { ExecutionGraphAssembly } = require('../out/executionGraphAssembly');
const { mergeExecutionGraph, executionHistory, executionReturnEdges, anchorExecutionLayout } = require('../out/executionGraph');
const { ordinaryVisualNodeID } = require('../out/executionView');
const { buildStdioRunArgs } = require('../out/directRunSession');

test('ordinary legacy-shaped runbooks request live graphs independently of typed Results', () => {
  assert.deepEqual(buildStdioRunArgs('root.yaml', {}, undefined, false, new Set(), undefined, false, true),
    ['run', '--stdio', '--require-capabilities', 'yawr.run-graph/v1', 'root.yaml']);
});

const node = (id, frame = 'root', extra = {}) => ({ id, position: { x: 0, y: 0 },
  data: { id, kind: 'noop', title: id, frame_id: frame, ...extra } });
const frame = (id, parent) => ({ id, runbook_id: id, runbook_path: `${id}.yaml`, depth: parent ? 1 : 0,
  ...(parent ? { parent_include_node_id: parent } : {}) });
const base = () => ({ schema_version: '1', hash: 'static', runbook: { id: 'root', name: 'Root', path: 'root.yaml' },
  nodes: [node('before'), node('call', 'root', { kind: 'include' }), node('after')],
  frames: [frame('root')], groups: [], edges: [{ id: 'e0', source: 'before', target: 'call', type: 'sequence' }] });
function update() {
  const document = base();
  document.hash = 'dynamic';
  document.nodes.push(node('opaque-child-1', 'child'));
  document.frames.push(frame('child', 'call'));
  document.groups.push({ id: 'child-group', kind: 'include-frame', parent_node_id: 'call', frame_id: 'child' });
  document.nodes.at(-1).data.group_id = 'child-group';
  document.nodes.at(-1).parentNode = 'child-group';
  document.nodes.at(-1).extent = 'parent';
  document.edges.push({ id: 'e0-dynamic', source: 'call', target: 'opaque-child-1', type: 'include' });
  return { document, nodeIDs: ['opaque-child-1'] };
}
function chunks(value, size = 100, revision = 1) {
  const bytes = Buffer.from(JSON.stringify(value));
  const digest = `sha256:${createHash('sha256').update(bytes).digest('hex')}`;
  return Array.from({ length: Math.ceil(bytes.length / size) }, (_, index) => ({
    revision, digest, offset: index * size, totalBytes: bytes.length,
    data: bytes.subarray(index * size, (index + 1) * size).toString('base64'),
  }));
}

test('execution graph chunks publish only a complete digest-verified document', () => {
  const assembly = new ExecutionGraphAssembly();
  const parts = chunks(update());
  for (const part of parts.slice(0, -1)) assert.equal(assembly.accept(part), undefined);
  assert.deepEqual(assembly.accept(parts.at(-1)), update());
  assembly.complete();
});
for (const [name, change] of [
  ['digest', chunk => ({ ...chunk, digest: `sha256:${'0'.repeat(64)}` })],
  ['offset', chunk => ({ ...chunk, offset: 1 })],
  ['oversize', chunk => ({ ...chunk, totalBytes: 33 * 1024 * 1024 })],
  ['base64', chunk => ({ ...chunk, data: '*invalid' })],
  ['revision', chunk => ({ ...chunk, revision: 0 })],
]) test(`execution graph rejects invalid ${name}`, () => {
  assert.throws(() => new ExecutionGraphAssembly().accept(change(chunks(update(), 10000)[0])), /execution graph/);
});
test('execution graph rejects interleaved, duplicate and truncated revisions', () => {
  const assembly = new ExecutionGraphAssembly(), parts = chunks(update());
  assembly.accept(parts[0]);
  assert.throws(() => assembly.accept(parts[0]), /contiguous/);
  assert.throws(() => assembly.accept({ ...parts[1], revision: 2 }), /contiguous/);
  assert.throws(() => assembly.complete(), /all chunks/);
});
test('execution graph rejects missing node bindings', () => {
  const value = update(); value.nodeIDs.push('missing');
  assert.throws(() => new ExecutionGraphAssembly().accept(chunks(value, 10000)[0]), /bindings/);
});
test('graph growth preserves queued static identities and all prior dynamic invocations', () => {
  const original = base(), first = update();
  const merged = mergeExecutionGraph(original, first.document, first.nodeIDs);
  assert.equal(ordinaryVisualNodeID(merged, 'call'), 'call');
  assert.equal(merged.nodes[0], original.nodes[0], 'static nodes must not be replaced during pacing');
  const second = update();
  second.document.nodes.push(node('opaque-child-2', 'second'));
  second.document.frames.push(frame('second', 'call'));
  second.nodeIDs.push('opaque-child-2');
  const final = mergeExecutionGraph(merged, second.document, second.nodeIDs);
  assert.equal(final.nodes.length, 5);
  assert.equal(final.frames.length, 3);
  assert.ok(final.nodes.some(node => node.id === 'opaque-child-1'));
  assert.ok(final.edges.some(edge => edge.source === 'call' && edge.target === 'opaque-child-1'));
});
test('graph updates fail explicitly on wrong runbook or unavailable parent', () => {
  const next = update();
  next.document.runbook.id = 'other';
  assert.throws(() => mergeExecutionGraph(base(), next.document, next.nodeIDs), /different runbook/);
  const missing = update();
  missing.document.edges.at(-1).source = 'not-loaded';
  assert.throws(() => mergeExecutionGraph(base(), missing.document, missing.nodeIDs), /unavailable parent/);
});
test('history retains execution order and legitimate repeated visits after completion', () => {
  const doc = mergeExecutionGraph(base(), update().document, update().nodeIDs);
  const history = executionHistory(doc, {
    after: { status: 'completed', startedEventSequence: 4 },
    call: { status: 'completed', occurrences: [
      { status: 'completed', startedEventSequence: 1 }, { status: 'completed', startedEventSequence: 3 },
    ] },
    'opaque-child-1': { status: 'completed', qualifiedNodeID: 'call/child', startedEventSequence: 2 },
    'runtime-wrapper': { status: 'completed', startedEventSequence: 0 },
  });
  assert.deepEqual(history.map(item => item.nodeID), ['call', 'opaque-child-1', 'call', 'after']);
  assert.equal(history[1].path, 'call/child');
});
test('layout expansion anchors the exact current node including nested frame offsets', () => {
  const old = [{ id: 'group', position: { x: 100, y: 100 } },
    { id: 'current', parentNode: 'group', position: { x: 10, y: 20 } }];
  const next = [{ id: 'group', position: { x: 200, y: 400 } },
    { id: 'current', parentNode: 'group', position: { x: 30, y: 40 } },
    { id: 'new', position: { x: 500, y: 800 } }];
  const anchored = anchorExecutionLayout(old, next, 'current');
  assert.deepEqual(anchored[0].position, { x: 80, y: 80 });
  assert.deepEqual(anchored[1].position, { x: 30, y: 40 });
  assert.equal(anchored[0].position.x + anchored[1].position.x, 110);
  assert.equal(anchored[0].position.y + anchored[1].position.y, 120);
});

test('observed child completion connects back to its parent, never inventing a concurrent return', () => {
  const doc = mergeExecutionGraph(base(), update().document, update().nodeIDs);
  const runtime = {
    'opaque-child-1': { status: 'completed', startedEventSequence: 1, finishedAt: '2026-01-01T00:00:01Z' },
    after: { status: 'completed', startedEventSequence: 2, startedAt: '2026-01-01T00:00:02Z' },
  };
  assert.deepEqual(executionReturnEdges(doc, runtime).map(edge => [edge.source, edge.target]), [['opaque-child-1', 'after']]);
  runtime.after.startedAt = '2026-01-01T00:00:00Z';
  assert.deepEqual(executionReturnEdges(doc, runtime), []);
});
