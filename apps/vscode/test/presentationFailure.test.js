const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { EventEmitter } = require('node:events');
const { PassThrough } = require('node:stream');
const { buildSync, transformSync } = require('esbuild');
const { DirectRunSession } = require('../out/directRunSession');
const { decodeEnvelope } = require('../out/presentationProtocol');
const { terminalPresentation, sanitizeEventFrame, INVALID_PRESENTATION_DIAGNOSTIC } = require('../out/presentationProjection');
const fixture = require('./fixtures/presentation-provider-frames');
const source = fs.readFileSync(path.join(__dirname, '..', 'webview', 'graph.tsx'), 'utf8');
function reducer(name, next, dependencies = {}) {
  const fn = source.slice(source.indexOf(`function ${name}(`), source.indexOf(`function ${next}(`));
  return vm.runInNewContext(transformSync(fn, { loader: 'ts', target: 'es2022' }).code + `\n${name}`, {
    ...require('../out/executionProgress'), ...require('../out/runStatus'), ...require('../out/displayObservations'), ...dependencies,
  });
}
const apply = reducer('applyRuntimeEvent', 'applyTerminalSteps', {
  terminalPresentation, eventNodeID: e => e.payload.qualified_node_id,
  recordValue: value => value && typeof value === 'object' ? value : undefined,
});
const summarize = reducer('applyTerminalSteps', 'graphInputDeclarations');
function productionPanel() {
  const Module = require('node:module');
  const compiled = buildSync({ entryPoints: [path.join(__dirname, '..', 'webview', 'inspector.tsx')],
    bundle: true, write: false, platform: 'node', format: 'cjs', packages: 'external', logLevel: 'silent' });
  const loaded = new Module(path.join(__dirname, 'presentation-panel.cjs'), module);
  loaded.filename = loaded.id; loaded.paths = module.paths;
  loaded._compile(compiled.outputFiles[0].text, loaded.filename);
  return loaded.exports.RuntimePane;
}
function child() {
  return Object.assign(new EventEmitter(), {
    stdout: new PassThrough(), stderr: new PassThrough(), stdin: new PassThrough(),
    killed: false, exitCode: null, kill() { this.killed = true; return true; },
  });
}
test('real closed validator rejects both independent legacy preview mutations, not the top-level flag', () => {
  for (const variant of ['failure', 'success']) {
    const raw = fixture.frames(variant)[2];
    assert.equal(raw.event.payload.code_presentation.outputs.length, 11);
    for (const remove of ['neither', 'sentinel', 'envelope-flag']) {
      const cp = structuredClone(raw.event.payload.code_presentation);
      if (remove === 'sentinel') cp.outputs.pop();
      if (remove === 'envelope-flag') delete cp.outputs_preview_truncated;
      assert.throws(() => decodeEnvelope(cp), /unknown-presentation-field/);
    }
    const cp = structuredClone(raw.event.payload.code_presentation);
    cp.outputs.pop(); delete cp.outputs_preview_truncated;
    assert.doesNotThrow(() => decodeEnvelope(cp));
    raw.event.payload.code_presentation = cp;
    sanitizeEventFrame(raw);
    assert.equal(raw.event.payload.presentation_diagnostic, undefined, 'top-level preview flag is not invalid decoration metadata');
  }
});
test('stdio dispatch, production reducer and runtime panel retain real failure before optional diagnostic', () => {
  const React = require('react'), { renderToStaticMarkup } = require('react-dom/server');
  const RuntimePane = productionPanel();
  for (const variant of ['failure', 'success', 'old-failure']) {
    const c = child(), errors = [], accepted = []; let state = {};
    new DirectRunSession(c, { onError: message => errors.push(message), onExit() {}, onFrame(frame) {
      accepted.push(frame);
      if (frame.type === 'run.event') state = apply(state, frame.event);
      if (frame.type === 'run.finished') state = summarize(state, frame.steps);
    } });
    for (const frame of fixture.frames(variant)) {
      const line = JSON.stringify(frame) + '\n', split = Math.floor(line.length / 2);
      c.stdout.write(line.slice(0, split)); c.stdout.write(line.slice(split));
    }
    assert.deepEqual(errors, []); assert.equal(c.killed, false); assert.equal(accepted.length, 4);
    const runtime = state['wrapper/read'];
    assert.equal(runtime.status, variant === 'success' ? 'completed' : 'failed');
    assert.equal(runtime.error, variant === 'success' ? undefined : fixture.error);
    assert.equal(runtime.presentationDiagnostic, variant === 'old-failure' ? undefined : INVALID_PRESENTATION_DIAGNOSTIC);
    const html = renderToStaticMarkup(React.createElement(RuntimePane, { runtime }));
    if (variant !== 'success') {
      assert.ok(html.includes(fixture.error));
      if (variant !== 'old-failure') assert.ok(html.indexOf(fixture.error) < html.indexOf(INVALID_PRESENTATION_DIAGNOSTIC));
    }
    if (variant !== 'old-failure') assert.doesNotMatch(html, /UNCLASSIFIED_SUMMARY/);
    const restarted = apply(state, { kind: 'step/started', payload: { qualified_node_id: 'wrapper/read' } });
    assert.equal(restarted['wrapper/read'].presentationDiagnostic, undefined);
  }
});
test('optional malformed metadata is omitted before caches, including untrusted output classifications', () => {
  for (const change of [
    p => { p.code_presentation.tool_digest = 'invalid'; },
    p => { p.output_value_status = { field00: 'available' }; },
    p => { p.code_presentation.outputs[0].status = 'invented'; },
  ]) {
    const frame = fixture.frames('failure')[2], p = frame.event.payload;
    p.code_presentation.outputs.pop(); delete p.code_presentation.outputs_preview_truncated;
    p.output = { field00: 'NEVER_FORWARD_UNCLASSIFIED' };
    change(p); sanitizeEventFrame(frame); sanitizeEventFrame(frame);
    assert.equal(p.error, fixture.error); assert.equal(p.status, 'failed');
    assert.equal(p.presentation_diagnostic, 'invalid-metadata');
    assert.doesNotMatch(JSON.stringify(frame), /NEVER_FORWARD_UNCLASSIFIED/);
    assert.equal(p.output, undefined); assert.equal(p.code_presentation, undefined);
  }
});
test('invalid core event bodies remain fatal protocol failures', () => {
  const c = child(), errors = [], frames = [];
  new DirectRunSession(c, { onError: e => errors.push(e), onFrame: f => frames.push(f), onExit() {} });
  c.stdout.write(JSON.stringify(fixture.frames('failure')[0]) + '\n');
  c.stdout.write(JSON.stringify({ type: 'run.event', version: 'yawr.stdio/v1', runID: 'synthetic-run',
    event: { kind: 'step/failed', payload: [] } }) + '\n');
  assert.deepEqual(errors, ['invalid run event payload']);
  assert.equal(c.killed, true); assert.equal(frames.length, 1);
});
test('session stdio and replay occurrence projection agree on status, diagnostic and omitted output', () => {
  const { createHash } = require('node:crypto');
  const { SessionStdioClient } = require('../out/sessionStdioClient');
  const { SessionGraphModel, sessionGraphNodeID } = require('../out/sessionCompositeGraph');
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const snapshot = { schema_version: 'investigation-session-manifest/v1',
    session: { session_id: sessionID, status: 'active', root_segment_id: segmentID,
      active_segment_id: segmentID, active_run_id: runID, sequence: 1 },
    segments: { [segmentID]: { segment_id: segmentID, ordinal: 1, runbook_id: 'entry',
      runbook_name: 'Entry', status: 'active', attempt_run_ids: [runID] } },
    attempts: { [runID]: { run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'running' } },
    transitions: {}, occurrences: {}, accepted_commands: {} };
  const frame = (sequence, type, payload) => ({ version: 'yawr.session-stdio/v1', type,
    frameID: 'sha256:' + createHash('sha256').update(JSON.stringify({ sequence, type, payload })).digest('hex'),
    sessionID, segmentID, runID, sessionSequence: sequence, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1, payload });
  for (const variant of ['success', 'failure', 'old-failure']) {
    const model = new SessionGraphModel(sessionID), replay = new SessionGraphModel(sessionID);
    const c = child(), errors = [], accepted = [];
    new SessionStdioClient(c, { sessionID, afterSequence: 0, onError: e => errors.push(e), onExit() {},
      onAcceptedSequence: n => accepted.push(n), onGroup: g => model.applyGroup(g) });
    const event = fixture.frames(variant)[2].event;
    event.run_id = runID;
    event.payload.output.field00 = 'NEVER_CACHE_UNTRUSTED';
    for (const f of [frame(1, 'session.snapshot', snapshot), frame(2, 'run.event', event)]) {
      // Replay reaches the model directly, while the live path first traverses
      // the real SessionStdioClient wire parser.
      replay.applyGroup({ sequence: f.sessionSequence, writerEpoch: 1, handshake: false, graphs: [], frames: [structuredClone(f)] });
      c.stdout.write(JSON.stringify(f) + '\n');
    }
    assert.deepEqual(errors, []); assert.deepEqual(accepted, [1, 2]);
    const key = sessionGraphNodeID(sessionID, segmentID, 'wrapper/read');
    const live = model.snapshot().runtimeNodes[key], history = replay.snapshot().runtimeNodes[key];
    assert.deepEqual(live, history);
    assert.equal(live.occurrences[0].status, variant === 'success' ? 'completed' : 'failed');
    assert.equal(live.occurrences[0].error, variant === 'success' ? undefined : fixture.error);
    if (variant !== 'old-failure') {
      assert.doesNotMatch(JSON.stringify(live), /NEVER_CACHE_UNTRUSTED/);
      assert.equal(live.occurrences[0].presentationDiagnostic, INVALID_PRESENTATION_DIAGNOSTIC);
    }
  }
});
test('complete 42-slot typed events and frozen history preserve exact identity and safety classifications', () => {
  const { decodePresentationState } = require('../out/presentationHistory');
  const cp = structuredClone(fixture.envelope);
  delete cp.outputs_preview_truncated; cp.outputs.pop();
  while (cp.outputs.length < 42) cp.outputs.push({ name: `field${cp.outputs.length}`,
    value_type: 'string', status: 'unavailable', reason: 'missing-descriptor' });
  Object.assign(cp.outputs[41], { status: 'resolved', presentation: { version: 1, kind: 'code', language: 'sql' } });
  delete cp.outputs[41].reason;
  for (const classification of ['available', 'truncated', 'absent', 'redacted', 'unavailable']) {
    const frame = fixture.frames('failure')[2], p = frame.event.payload;
    p.code_presentation = structuredClone(cp);
    p.output = { field41: 'SELECT 1' }; p.output_value_status = { field41: classification };
    const history = { run_id: frame.runID, plan_snapshot_digest: cp.plan_snapshot_digest, checkpoint_sequence: 38,
      occurrences: [{ identity: { qualified_node_id: 'wrapper/read', frame_id: 'synthetic-frame', occurrence_sequence: 1 },
        details: { kind: 'tool', arguments: [], code_presentation: structuredClone(cp) },
        output: structuredClone(p.output), output_value_status: structuredClone(p.output_value_status) }] };
    sanitizeEventFrame(frame);
    const retained = decodePresentationState(history).occurrences[0];
    assert.deepEqual(p.code_presentation, retained.details.code_presentation);
    assert.deepEqual(p.output, retained.output);
    assert.equal(retained.output_value_status.field41, classification);
    assert.equal(p.code_presentation.outputs.length, 42);
    assert.equal(p.presentation_diagnostic, undefined);
    assert.equal(p.output.field41, ['available', 'truncated'].includes(classification) ? 'SELECT 1' : undefined);
    history.plan_snapshot_digest = 'sha256:' + 'c'.repeat(64);
    assert.throws(() => decodePresentationState(history), /invalid-frozen-presentation/);
  }
});
