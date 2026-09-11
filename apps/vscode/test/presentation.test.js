const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const { pathToFileURL } = require('node:url');
const { createHash } = require('node:crypto');
const { Worker } = require('node:worker_threads');
const { execFile } = require('node:child_process');
const { promisify } = require('node:util');
const { parseDocument } = require('yaml');
const { decodeReply, decodeEnvelope, decodeDescriptor, decodeCapabilities, safeOutputs } = require('../out/presentationProtocol');
const { mapScalar, resolveScalars, sourceSpans } = require('../out/presentationScalar');
const { sanitizeEventFrame } = require('../out/presentationProjection');
const { parseStepDetails } = require('../out/stepDetails');
const { parseGraphDocument } = require('../out/directGraphPreview');
const { decodePresentationState } = require('../out/presentationHistory');
const { verifyPresentationHelper, resolvePresentation, requireCompatibleExecution, bundledPresentationHelper, finiteHelper } = require('../out/presentationClient');
const vectorBytes = fs.readFileSync(path.join(__dirname, 'fixtures', 'presentation-contract.json'));
const vector = JSON.parse(vectorBytes);
const clone = value => structuredClone(value);

test('canonical vector copied byte-for-byte, tool digest matches overlay bytes', () => {
  assert.equal(createHash('sha256').update(vectorBytes).digest('hex'),
    fs.readFileSync(path.join(__dirname, 'fixtures', 'presentation-contract.sha256'), 'utf8').trim());
  assert.equal('sha256:' + createHash('sha256').update(vector.request.overlays[0].text).digest('hex'),
    vector.expect.bindings[0].tool_digest);
});
test('actual canonical graph descriptor reaches production StepDetails decoder', () => {
  const parsed = parseStepDetails(clone(vector.graph_details_example), 'tool', 'node');
  assert.equal(parsed.code_presentation.arguments[0].presentation.language, 'sql');
  for (const modify of [
    v => { v.code_presentation.outputs[0].presentation.extra = true; },
    v => { v.code_presentation.tool_digest = 'fake'; },
    v => { v.code_presentation.origin = 'frozen'; },
    v => { v.code_presentation.arguments[0].value_type = 'secret'; },
  ]) {
    const value = clone(vector.graph_details_example); modify(value);
    assert.throws(() => parseStepDetails(value, 'tool', 'node'));
  }
  const secret = clone(vector.graph_details_example);
  secret.arguments[0].redacted = true;
  secret.arguments[0].value = 'NEVER_TOKENIZE_SECRET';
  assert.ok(!JSON.stringify(parseStepDetails(secret, 'tool', 'node')).includes('NEVER_TOKENIZE_SECRET'));
});
test('strict future descriptor fallback and capabilities validation', () => {
  assert.equal(decodeDescriptor({ version: 2, kind: 'code', language: 'future-lang' }).version, 2);
  assert.throws(() => decodeDescriptor({ version: 1, kind: 'code', language: 'SQL' }));
  assert.throws(() => decodeCapabilities({ schema_version: 'yawr.presentation-capabilities/v1', resolver_version: 'yawr.core-binding/v1',
    execution_plan_read: ['execution-plan/v1'], execution_plan_write: ['execution-plan/v1'] }));
});
test('current graph keeps future envelope and declared unsupported field as plaintext', () => {
  const details = clone(vector.graph_details_example);
  details.code_presentation.version = 2;
  const parsed = parseStepDetails(details, 'tool', 'future');
  assert.equal(parsed.code_presentation.version, 2);
  assert.equal(parsed.code_presentation.status, 'unsupported');
  assert.equal(parsed.code_presentation.arguments[0].status, 'unsupported');
  const unsupported = clone(vector.graph_details_example);
  Object.assign(unsupported.code_presentation.arguments[0], {
    status: 'unsupported', reason: 'unsupported-language', presentation: { version: 1, kind: 'code', language: 'future-lang' },
  });
  assert.equal(parseStepDetails(unsupported, 'tool', 'unsupported').code_presentation.arguments[0].status, 'unsupported');
});
test('CST map preserves quotes, folding, CRLF, escapes, Unicode and structural whitespace', () => {
  for (const scalar of ["'SELECT ''x'';'", '"SELECT \\U0001F680\\nFROM x"', '|-\n  SELECT 1\n  FROM t\n', '>-\r\n  SELECT 🚀\r\n  FROM t\r\n',
    '|2-\n  SELECT 1\n    FROM t\n', '"SELECT\n  1"', "'SELECT\n\n  1'"]) {
    const source = 'text: ' + scalar;
    const node = parseDocument(source, { keepSourceTokens: true }).get('text', true);
    const mapped = mapScalar(node);
    assert.equal(mapped.text, node.value, scalar);
    const spans = sourceSpans(mapped, [{ start: 0, end: node.value.length, color: '#123456' }], source);
    for (const span of spans) {
      assert.ok(span.start >= 6);
      assert.doesNotMatch(source.slice(span.start, span.end), /[\r\n]/);
    }
    if (scalar.startsWith("'") || scalar.startsWith('"')) {
      assert.equal(spans[0].start, 7);
      assert.equal(spans.at(-1).end, source.length - 1);
    }
  }
});
test('policy classification precedes outputs, IPC and copy projections', () => {
  const envelope = decodeEnvelope(vector.graph_details_example.code_presentation);
  for (const status of [undefined, {}, { script: 'redacted' }, { script: 'unavailable' }, { script: 'absent' }]) {
    assert.ok(!JSON.stringify(safeOutputs(envelope, { script: 'NEVER_TOKENIZE_SECRET' }, status)).includes('NEVER_TOKENIZE_SECRET'));
    for (const wrapper of ['payload', 'event']) {
      const frame = { type: 'run.event', [wrapper]: { kind: 'step/completed', payload: {
        code_presentation: clone(envelope), output: { script: 'NEVER_TOKENIZE_SECRET', table: [{ x: 1 }] }, output_value_status: status,
      } } };
      sanitizeEventFrame(frame);
      assert.ok(!JSON.stringify(frame).includes('NEVER_TOKENIZE_SECRET'));
      assert.deepEqual(frame[wrapper].payload.output.table, [{ x: 1 }]);
    }
  }
  assert.equal(safeOutputs(envelope, { script: '' }, { script: 'available' })[0].text, '');
  assert.equal(safeOutputs(envelope, { script: 'safe excerpt' }, { script: 'truncated' })[0].status, 'truncated');
  assert.equal(safeOutputs(envelope, { script: 42 }, { script: 'available' })[0].status, 'unavailable');
});
test('metadata-free execution does not require capabilities', async () => {
  await requireCompatibleExecution('not-installed', { nodes: [] });
});
test('metadata on unused bound tool fields still blocks an incompatible execution runtime', async () => {
  const reply = decodeReply({ ...clone(vector.expect), dependencies: [] }, vector.request);
  await assert.rejects(requireCompatibleExecution(process.execPath, { nodes: [] }, reply), /requires the current execution-plan\/v3 runtime/);
});
test('finite helper rejects missing binaries, oversized input/output and owned stale work', { timeout: 10000 }, async () => {
  await assert.rejects(verifyPresentationHelper(path.resolve(__dirname, 'not-a-helper.exe')), /helper-unavailable/);
  await assert.rejects(finiteHelper(process.execPath, [], 'x'.repeat(8 * 1024 * 1024 + 1)), /limit-exceeded/);
  await assert.rejects(finiteHelper(process.execPath, ['-e', "process.stdout.write('x'.repeat(8*1024*1024+1))"]), /limit-exceeded/);
  const controller = new AbortController();
  const pending = finiteHelper(process.execPath, ['-e', 'setTimeout(()=>{},20000)'], '', controller.signal);
  setTimeout(() => controller.abort(), 50);
  await assert.rejects(pending, /stale-request/);
});
test('stale generations, URI, range overlap and malformed identity fail closed', () => {
  for (const mutate of [
    r => { r.context.generation++; },
    r => { r.document.uri = 'other'; },
    r => { r.regions[0].range.end = 99999; },
    r => { r.regions.push(r.regions[0]); },
    r => { r.bindings[0].tool_digest = undefined; },
    r => { r.bindings[0].actions[0].arguments[0].presentation.language = 'unknown'; },
  ]) {
    const response = { ...clone(vector.expect), dependencies: [] }; mutate(response);
    assert.throws(() => decodeReply(response, vector.request));
  }
});
test('retained identities remain distinct and unknown safety never enters history cache', () => {
  const envelope = clone(vector.graph_details_example.code_presentation);
  envelope.origin = 'frozen'; envelope.plan_snapshot_digest = 'sha256:' + 'a'.repeat(64);
  const occurrence = { identity: { qualified_node_id: 'wrapper/child', frame_id: 'frame1', retry_attempt: 1 },
    details: { ...clone(vector.graph_details_example), code_presentation: envelope }, output: { script: 'NEVER_TOKENIZE_SECRET' },
    output_value_status: { script: 'redacted' } };
  const state = { run_id: 'run-1', plan_snapshot_digest: envelope.plan_snapshot_digest, checkpoint_sequence: 9,
    occurrences: [occurrence, { ...clone(occurrence), identity: { ...occurrence.identity, retry_attempt: 2 }, output: { script: 'safe' }, output_value_status: { script: 'available' } }] };
  const parsed = decodePresentationState(state);
  assert.ok(!JSON.stringify(parsed).includes('NEVER_TOKENIZE_SECRET'));
  assert.equal(parsed.occurrences[1].output.script, 'safe');
  assert.equal(parsed.occurrences[0].identity.invocation, undefined, 'No fabricated invocation counter');
  const invalid = clone(state); invalid.occurrences[1].identity.retry_attempt = 1;
  assert.throws(() => decodePresentationState(invalid));
});

test('terminal presentation uses qualified identity and separates frame occurrences before caching', () => {
  const { SessionGraphModel, sessionGraphNodeID } = require('../out/sessionCompositeGraph');
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const manifest = {
    schema_version: 'investigation-session-manifest/v1',
    session: { session_id: sessionID, status: 'active', root_segment_id: segmentID,
      active_segment_id: segmentID, active_run_id: runID, sequence: 1 },
    segments: { [segmentID]: { segment_id: segmentID, ordinal: 1, runbook_id: 'entry',
      runbook_name: 'Entry', status: 'active', attempt_run_ids: [runID] } },
    attempts: { [runID]: { run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'running' } },
    transitions: {}, occurrences: {}, accepted_commands: {},
  };
  const model = new SessionGraphModel(sessionID);
  const frame = (sequence, type, payload) => ({
    version: 'yawr.session-stdio/v1', type, frameID: `sha256:test-${sequence}`, sessionID, segmentID, runID,
    sessionSequence: sequence, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1, payload,
  });
  const apply = (sequence, type, payload) => model.applyGroup({
    sequence, writerEpoch: 1, handshake: false, graphs: [], frames: [frame(sequence, type, payload)],
  });
  apply(1, 'session.snapshot', manifest);
  for (const [sequence, frameID, value, status] of [[2, 'frame/a', 'safe-A', 'available'], [3, 'frame/b', 'NEVER_CACHE_SECRET', 'redacted']]) {
    apply(sequence, 'run.event', {
      event_id: `event-${sequence}`, run_id: runID, runbook_id: 'entry', sequence, kind: 'step/completed',
      payload: {
        qualified_node_id: 'wrapper/child', node_id: 'wrong-unqualified', step_id: 'child',
        frame_id: frameID, frame_step_index: 0, invocation: 1, retry_attempt: 1, occurrence_sequence: 1,
        code_presentation: clone(vector.graph_details_example.code_presentation), output: { script: value }, output_value_status: { script: status },
      },
    });
  }
  const nodes = model.snapshot().runtimeNodes;
  const runtime = nodes[sessionGraphNodeID(sessionID, segmentID, 'wrapper/child')];
  assert.equal(nodes[sessionGraphNodeID(sessionID, segmentID, 'wrong-unqualified')], undefined);
  assert.deepEqual(runtime.occurrences.map(o => o.frameID), ['frame/a', 'frame/b']);
  assert.equal(runtime.occurrences[0].output.script, 'safe-A');
  assert.equal(runtime.occurrences[1].output.script, undefined);
  assert.notEqual(runtime.occurrences[0].occurrenceID, runtime.occurrences[1].occurrenceID);
  assert.ok(!JSON.stringify(runtime).includes('NEVER_CACHE_SECRET'));
});
