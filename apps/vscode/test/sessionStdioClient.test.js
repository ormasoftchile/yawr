'use strict';

const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const { EventEmitter } = require('node:events');
const { PassThrough } = require('node:stream');
const test = require('node:test');

const {
  buildSessionAttachArgs,
  buildSessionGraphArgs,
  buildSessionStartArgs,
  withSessionPackageMap,
  SessionStdioClient,
  SESSION_STDIO_PROTOCOL_VERSION,
} = require('../out/sessionStdioClient');

const sessionID = '11111111-1111-4111-8111-111111111111';
const segmentID = '22222222-2222-4222-8222-222222222222';
const runID = '33333333-3333-4333-8333-333333333333';

function digest(data) {
  return `sha256:${createHash('sha256').update(data).digest('hex')}`;
}

function frame(overrides) {
  return {
    version: SESSION_STDIO_PROTOCOL_VERSION,
    type: 'session.snapshot',
    frameID: digest(Buffer.from(JSON.stringify(overrides))),
    sessionID,
    sessionSequence: 1,
    sequenceIndex: 0,
    sequenceCount: 1,
    writerEpoch: 4,
    ...overrides,
  };
}

test('client accepts only a complete hash-verified graph sequence', () => {
  const child = fakeChild();
  const groups = [];
  const accepted = [];
  const errors = [];
  const graphJSON = Buffer.from(JSON.stringify({
    schema_version: '1',
    runbook: { id: 'root', name: 'Root', path: 'root.runbook.yaml' },
    frames: [], groups: [], nodes: [], edges: [],
  }));
  const wholeBlobHash = digest(graphJSON);
  const split = Math.ceil(graphJSON.length / 2);
  new SessionStdioClient(child, {
    sessionID,
    afterSequence: 0,
    onGroup: (group) => groups.push(group),
    onAcceptedSequence: (sequence) => accepted.push(sequence),
    onError: (message) => errors.push(message),
    onExit() {},
  });

  test('actual session frame wrapper sanitizes classified output before onGroup delivery', () => {
    const vector = require('./fixtures/presentation-contract.json');
    const child = fakeChild(), groups = [], errors = [];
    new SessionStdioClient(child, { sessionID, afterSequence: 0, onGroup: group => groups.push(group),
      onAcceptedSequence() {}, onError: message => errors.push(message), onExit() {} });
    child.stdout.write(JSON.stringify(frame({
      type: 'run.event', segmentID, runID,
      payload: { kind: 'step/completed', payload: {
        code_presentation: structuredClone(vector.graph_details_example.code_presentation),
        output: { script: 'NEVER_FORWARD_SECRET', table: [{ x: 1 }] },
        output_value_status: { script: 'redacted' },
      } },
    })) + '\n');
    assert.deepEqual(errors, []);
    assert.equal(groups.length, 1);
    const payload = groups[0].frames[0].payload.payload;
    assert.equal(payload.output.script, undefined);
    assert.equal(payload.output_value_status.script, 'redacted');
    assert.deepEqual(payload.output.table, [{ x: 1 }]);
    assert.ok(payload.code_presentation);
  });
  child.stdout.write(`${JSON.stringify(frame({
    type: 'segment.graph', segmentID, runID, sequenceIndex: 0, sequenceCount: 2,
    payload: {
      graphRevision: 1, chunkIndex: 0, chunkCount: 2, wholeBlobHash,
      data: graphJSON.subarray(0, split).toString('base64'),
    },
  }))}\n`);
  assert.deepEqual(groups, []);
  assert.deepEqual(accepted, []);

  child.stdout.write(`${JSON.stringify(frame({
    type: 'segment.graph', segmentID, runID, sequenceIndex: 1, sequenceCount: 2,
    payload: {
      graphRevision: 1, chunkIndex: 1, chunkCount: 2, wholeBlobHash,
      data: graphJSON.subarray(split).toString('base64'),
    },
  }))}\n`);

  assert.deepEqual(errors, []);
  assert.deepEqual(accepted, [1]);
  assert.equal(groups.length, 1);
  assert.equal(groups[0].graphs.length, 1);
  assert.equal(groups[0].graphs[0].segmentID, segmentID);
  assert.equal(groups[0].graphs[0].revision, 1);
  assert.equal(groups[0].graphs[0].document.runbook.id, 'root');
  assert.equal(groups[0].graphs[0].encodedDocument, graphJSON.toString('utf8'));
});

test('client rejects a graph hash mismatch without advancing the cursor', () => {
  const child = fakeChild();
  const accepted = [];
  const errors = [];
  new SessionStdioClient(child, {
    sessionID,
    afterSequence: 0,
    onGroup() {},
    onAcceptedSequence: (sequence) => accepted.push(sequence),
    onError: (message) => errors.push(message),
    onExit() {},
  });

  child.stdout.write(`${JSON.stringify(frame({
    type: 'segment.graph', segmentID, runID,
    payload: {
      graphRevision: 1, chunkIndex: 0, chunkCount: 1,
      wholeBlobHash: digest(Buffer.from('different')),
      data: Buffer.from('{}').toString('base64'),
    },
  }))}\n`);

  assert.deepEqual(accepted, []);
  assert.match(errors[0], /graph.*digest/i);
  assert.equal(child.killed, true);
});

test('client rejects a sequence whose aggregate encoded frames exceed the limit', () => {
  const child = fakeChild();
  const errors = [];
  new SessionStdioClient(child, {
    sessionID,
    afterSequence: 0,
    limits: { maxSequenceBytes: 64 },
    onGroup() {},
    onAcceptedSequence() { assert.fail('oversized sequence must not advance'); },
    onError: (message) => errors.push(message),
    onExit() {},
  });

  child.stdout.write(`${JSON.stringify(frame({
    type: 'run.event', sequenceIndex: 0, sequenceCount: 2,
    payload: { value: 'larger than the injected aggregate limit' },
  }))}\n`);
  assert.match(errors[0], /sequence.*bytes/i);
  assert.equal(child.killed, true);
});

test('client rejects a reconstructed graph whose decoded bytes exceed the limit', () => {
  const child = fakeChild();
  const errors = [];
  const graphJSON = Buffer.from(JSON.stringify({
    schema_version: '1', runbook: { id: 'root' }, frames: [], groups: [], nodes: [], edges: [],
  }));
  new SessionStdioClient(child, {
    sessionID,
    afterSequence: 0,
    limits: { maxGraphBytes: 16 },
    onGroup() {},
    onAcceptedSequence() { assert.fail('oversized graph must not advance'); },
    onError: (message) => errors.push(message),
    onExit() {},
  });

  child.stdout.write(`${JSON.stringify(frame({
    type: 'segment.graph', segmentID, runID,
    payload: {
      graphRevision: 1, chunkIndex: 0, chunkCount: 1, wholeBlobHash: digest(graphJSON),
      data: graphJSON.toString('base64'),
    },
  }))}\n`);
  assert.match(errors[0], /graph.*bytes/i);
  assert.equal(child.killed, true);
});

test('attach-at-head handshake updates epoch without advancing sequence and fences commands', () => {
  const child = fakeChild();
  const groups = [];
  const accepted = [];
  const client = new SessionStdioClient(child, {
    sessionID,
    afterSequence: 5,
    onGroup: (group) => groups.push(group),
    onAcceptedSequence: (sequence) => accepted.push(sequence),
    onError: (message) => assert.fail(message),
    onExit() {},
  });

  child.stdout.write(`${JSON.stringify(frame({
    sessionSequence: 5,
    writerEpoch: 9,
    payload: { schema_version: 'investigation-session-manifest/v1', session: { session_id: sessionID, sequence: 5 } },
  }))}\n`);

  assert.equal(groups.length, 1);
  assert.equal(groups[0].handshake, true);
  assert.deepEqual(accepted, []);
  assert.equal(client.acceptedSequence, 5);
  assert.equal(client.writerEpoch, 9);

  client.send({
    type: 'session.resume',
    commandID: '44444444-4444-4444-8444-444444444444',
  });
  assert.deepEqual(JSON.parse(child.stdin.read().toString()), {
    version: SESSION_STDIO_PROTOCOL_VERSION,
    type: 'session.resume',
    commandID: '44444444-4444-4444-8444-444444444444',
    sessionID,
    writerEpoch: 9,
    expectedSequence: 5,
  });
});

test('handshake callback failures are reported and stop the child without escaping', () => {
  const child = fakeChild();
  const errors = [];
  new SessionStdioClient(child, {
    sessionID,
    afterSequence: 5,
    onGroup() { throw new Error('host projection failed'); },
    onAcceptedSequence() { assert.fail('handshake must not advance the cursor'); },
    onError: (message) => errors.push(message),
    onExit() {},
  });

  assert.doesNotThrow(() => child.stdout.write(`${JSON.stringify(frame({
    sessionSequence: 5,
    writerEpoch: 9,
    payload: { schema_version: 'investigation-session-manifest/v1', session: { session_id: sessionID, sequence: 5 } },
  }))}\n`));
  assert.deepEqual(errors, ['host projection failed']);
  assert.equal(child.killed, true);
});

test('protocol.error is reported directly and stops the attachment', () => {
  const child = fakeChild();
  const groups = [];
  const errors = [];
  new SessionStdioClient(child, {
    sessionID,
    afterSequence: 5,
    onGroup: (group) => groups.push(group),
    onAcceptedSequence() { assert.fail('protocol errors must not advance the cursor'); },
    onError: (message) => errors.push(message),
    onExit() {},
  });

  child.stdout.write(`${JSON.stringify(frame({
    type: 'protocol.error', sessionSequence: 5, writerEpoch: 9,
    payload: { code: 'session-already-active', message: 'Another attachment owns the writer lease.' },
  }))}\n`);

  assert.deepEqual(groups, []);
  assert.deepEqual(errors, ['session-already-active: Another attachment owns the writer lease.']);
  assert.equal(child.killed, true);
});

function fakeChild() {
  const child = new EventEmitter();
  child.stdin = new PassThrough();
  child.stdout = new PassThrough();
  child.stderr = new PassThrough();
  child.killed = false;
  child.exitCode = null;
  child.kill = () => {
    child.killed = true;
    return true;
  };
  return child;
}

test('session launch args preserve client identity, omit private inputs, and reconnect cursor', () => {
  const runbook = 'C:\\work\\incident.runbook.yaml';
  const projectRoot = 'C:\\work';
  const creationCommandID = '44444444-4444-4444-8444-444444444444';
  assert.deepEqual(buildSessionStartArgs(
    runbook,
    sessionID,
    creationCommandID,
    { region: 'westus', environment: 'prod', access_token: 'private-never-in-argv' },
    new Set(['access_token']),
    projectRoot,
  ), [
    'session', 'start', runbook,
    '--session-id', sessionID,
    '--command-id', creationCommandID,
    '--stdio',
    '--tool-dir', projectRoot,
    '--var', 'environment=prod',
    '--var', 'region=westus',
  ]);
  assert.deepEqual(buildSessionAttachArgs(sessionID, 12, projectRoot), [
    'session', 'attach', sessionID,
    '--stdio', '--after-sequence', '12',
    '--tool-dir', projectRoot,
  ]);
  assert.deepEqual(buildSessionGraphArgs(sessionID, segmentID, 3), [
    'session', 'graph', sessionID,
    '--segment-id', segmentID,
    '--revision', '3',
  ]);
  assert.deepEqual(withSessionPackageMap(
    buildSessionAttachArgs(sessionID, 12, projectRoot),
    'C:\\work\\package-map.yaml',
  ), [
    'session', 'attach', sessionID,
    '--stdio', '--after-sequence', '12',
    '--tool-dir', projectRoot,
    '--package-map', 'C:\\work\\package-map.yaml',
  ]);
});

test('client accepts a deferred graph marker without assembling a graph', () => {
  const child = fakeChild();
  const groups = [];
  const accepted = [];
  new SessionStdioClient(child, {
    sessionID,
    afterSequence: 0,
    onGroup: (group) => groups.push(group),
    onAcceptedSequence: (sequence) => accepted.push(sequence),
    onError: (message) => assert.fail(message),
    onExit() {},
  });
  child.stdout.write(`${JSON.stringify(frame({
    type: 'segment.graph.available', segmentID, runID,
    payload: { graphRevision: 1, wholeBlobHash: `sha256:${'a'.repeat(64)}` },
  }))}\n`);
  assert.deepEqual(accepted, [1]);
  assert.equal(groups.length, 1);
  assert.deepEqual(groups[0].graphs, []);
  assert.equal(groups[0].frames[0].type, 'segment.graph.available');
});

test('pre-lease protocol.error reports the structured error at the requested cursor', () => {
  const child = fakeChild();
  const errors = [];
  new SessionStdioClient(child, {
    sessionID,
    afterSequence: 5,
    onGroup() { assert.fail('pre-lease errors are not graph groups'); },
    onAcceptedSequence() { assert.fail('pre-lease errors must not advance'); },
    onError: (message) => errors.push(message),
    onExit() {},
  });
  child.stdout.write(`${JSON.stringify(frame({
    type: 'protocol.error', sessionSequence: 5, writerEpoch: 0,
    payload: { code: 'session-already-active', message: 'another writer owns the session' },
  }))}\n`);
  assert.deepEqual(errors, ['session-already-active: another writer owns the session']);
  assert.equal(child.killed, true);
});