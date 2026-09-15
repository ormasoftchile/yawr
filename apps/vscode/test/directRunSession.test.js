'use strict';

const assert = require('node:assert/strict');
const { EventEmitter, once } = require('node:events');
const { PassThrough } = require('node:stream');
const test = require('node:test');

const {
  DirectRunSession,
  buildStdioRunArgs,
} = require('../out/directRunSession');

test('buildStdioRunArgs carries package map and sorted input values', () => {
  assert.deepEqual(
    buildStdioRunArgs(
      'C:\\work\\incident.runbook.yaml',
      { region: 'westus', environment: 'prod' },
      'C:\\work\\package-map.yaml',
    ),
    [
      'run',
      '--stdio',
      '--require-capabilities', 'yawr.lexical-tool-scopes/v1',
      '--package-map', 'C:\\work\\package-map.yaml',
      '--var', 'environment=prod',
      '--var', 'region=westus',
      'C:\\work\\incident.runbook.yaml',
    ],
  );
});

test('buildStdioRunArgs opts debug runs into the startup handshake', () => {
  assert.deepEqual(
    buildStdioRunArgs('C:\\work\\incident.runbook.yaml', {}, undefined, true),
    ['run', '--stdio', '--require-capabilities', 'yawr.lexical-tool-scopes/v1', '--debug', 'C:\\work\\incident.runbook.yaml'],
  );
});

test('buildStdioRunArgs omits secret inputs from process arguments', () => {
  assert.deepEqual(
    buildStdioRunArgs(
      'C:\\work\\incident.runbook.yaml',
      { environment: 'prod', access_token: 'SENTINEL-secret' },
      undefined,
      false,
      new Set(['access_token']),
    ),
    ['run', '--stdio', '--require-capabilities', 'yawr.lexical-tool-scopes/v1', '--configure', '--var', 'environment=prod', 'C:\\work\\incident.runbook.yaml'],
  );
});

test('buildStdioRunArgs launches a saved route test without input overrides or debugger configuration', () => {
  assert.deepEqual(
    buildStdioRunArgs(
      'C:\\work\\incident.runbook.yaml',
      { environment: 'prod', access_token: 'must-not-leak' },
      'C:\\work\\package-map.yaml',
      false,
      new Set(['access_token']),
      'C:\\work\\.yawr\\route-tests\\failover.route-test.yaml',
    ),
    [
      'run',
      '--stdio',
      '--require-capabilities', 'yawr.lexical-tool-scopes/v1',
      '--package-map', 'C:\\work\\package-map.yaml',
      '--route-test', 'C:\\work\\.yawr\\route-tests\\failover.route-test.yaml',
      'C:\\work\\incident.runbook.yaml',
    ],
  );
});

test('DirectRunSession parses fragmented protocol frames and writes commands', async () => {
  const child = fakeChild();
  const frames = [];
  const errors = [];
  const exits = [];
  let resolveFrame;
  const frameReceived = new Promise((resolve) => { resolveFrame = resolve; });
  const session = new DirectRunSession(child, {
    onFrame: (frame) => {
      frames.push(frame);
      resolveFrame();
    },
    onError: (message) => errors.push(message),
    onExit: (code, signal) => exits.push({ code, signal }),
  });

  child.stdout.write('{"type":"run.started","version":"yawr.');
  child.stdout.write('stdio/v1","runID":"run-1"}\n');
  await frameReceived;
  assert.equal(frames.length, 1);
  assert.equal(frames[0].type, 'run.started');

  session.send({
    type: 'interaction.answer',
    runID: 'run-1',
    turnID: 'turn-1',
    answer: { kind: 'choice', selected: ['o:0'] },
  });
  assert.equal(
    child.stdin.read().toString(),
    '{"type":"interaction.answer","runID":"run-1","turnID":"turn-1","answer":{"kind":"choice","selected":["o:0"]}}\n',
  );

  assert.deepEqual(errors, []);

  child.emit('exit', 0, null);
  assert.deepEqual(exits, [], 'exit fires before stdio streams are fully drained');
  child.emit('close', 0, null);
  assert.deepEqual(exits, [{ code: 0, signal: null }]);
});

test('DirectRunSession dispose requests cancellation and kills the child', () => {
  const child = fakeChild();
  const session = new DirectRunSession(child, {
    onFrame() {},
    onError() {},
    onExit() {},
  });
  session.setRunID('run-2');

  session.dispose();

  assert.equal(
    child.stdin.read().toString(),
    '{"type":"run.cancel","runID":"run-2","reason":"panel disposed"}\n',
  );
  assert.equal(child.killed, true);
});

test('DirectRunSession surfaces child-process errors', () => {
  const child = fakeChild();
  const errors = [];
  const exits = [];
  new DirectRunSession(child, {
    onFrame() {},
    onError: (message) => errors.push(message),
    onExit: (code, signal) => exits.push({ code, signal }),
  });

  child.emit('error', new Error('spawn failed'));
  child.emit('close', null, null);

  assert.deepEqual(errors, ['spawn failed']);
  assert.deepEqual(exits, [{ code: null, signal: null }]);
});

test('DirectRunSession terminates an incompatible protocol', () => {
  const child = fakeChild();
  const errors = [];
  const session = new DirectRunSession(child, {
    onFrame() {},
    onError: (message) => errors.push(message),
    onExit() {},
  });
  session.setRunID('run-incompatible');

  child.stdout.write('{"type":"run.started","version":"wrong"}\n');

  assert.match(errors[0], /unsupported protocol version/i);
  assert.equal(child.killed, true);
});

test('DirectRunSession ignores trailing frames after a fatal protocol error', () => {
  const child = fakeChild();
  const frames = [];
  const errors = [];
  new DirectRunSession(child, {
    onFrame: (frame) => frames.push(frame),
    onError: (message) => errors.push(message),
    onExit() {},
  });

  child.stdout.write(
    '{"type":"run.started","version":"wrong","runID":"run-1"}\n' +
    '{"type":"run.finished","version":"yawr.stdio/v1","runID":"run-1","status":"completed"}\n',
  );

  assert.equal(errors.length, 1);
  assert.deepEqual(frames, []);
});

test('DirectRunSession terminates frames that mismatch the started run ID', () => {
  const child = fakeChild();
  const errors = [];
  new DirectRunSession(child, {
    onFrame() {},
    onError: (message) => errors.push(message),
    onExit() {},
  });

  child.stdout.write('{"type":"run.started","version":"yawr.stdio/v1","runID":"run-1"}\n');
  child.stdout.write('{"type":"run.finished","version":"yawr.stdio/v1","runID":"run-2","status":"completed"}\n');

  assert.match(errors[0], /runID.*does not match/i);
  assert.equal(child.killed, true);
});

test('DirectRunSession requires run IDs on lifecycle frames', () => {
  const child = fakeChild();
  const errors = [];
  new DirectRunSession(child, {
    onFrame() {},
    onError: (message) => errors.push(message),
    onExit() {},
  });

  child.stdout.write('{"type":"run.started","version":"yawr.stdio/v1"}\n');

  assert.match(errors[0], /run\.started.*runID/i);
  assert.equal(child.killed, true);
});

test('DirectRunSession accepts a global protocol.error after run start', () => {
  const child = fakeChild();
  const frames = [];
  const errors = [];
  new DirectRunSession(child, {
    onFrame: (frame) => frames.push(frame),
    onError: (message) => errors.push(message),
    onExit() {},
  });
  child.stdout.write('{"type":"run.started","version":"yawr.stdio/v1","runID":"run-1"}\n');
  child.stdout.write('{"type":"protocol.error","version":"yawr.stdio/v1","error":{"code":"COMMAND_REJECTED"}}\n');

  assert.deepEqual(errors, []);
  assert.equal(frames.length, 2);
  assert.equal(child.killed, false);
});

test('DirectRunSession terminates malformed protocol JSON', () => {
  const child = fakeChild();
  const errors = [];
  new DirectRunSession(child, {
    onFrame() {},
    onError: (message) => errors.push(message),
    onExit() {},
  });

  child.stdout.write('not-json\n');

  assert.match(errors[0], /invalid JSON/i);
  assert.equal(child.killed, true);
});

test('DirectRunSession rejects oversized outbound commands before writing', () => {
  const child = fakeChild();
  const errors = [];
  const session = new DirectRunSession(child, {
    onFrame() {},
    onError: (message) => errors.push(message),
    onExit() {},
  });

  session.send({ type: 'interaction.answer', value: 'x'.repeat(1024 * 1024) });

  assert.match(errors[0], /command exceeds/i);
  assert.equal(child.stdin.read(), null);
  assert.equal(child.killed, true);
});

test('DirectRunSession does not cancel after run.finished', () => {
  const child = fakeChild();
  const session = new DirectRunSession(child, {
    onFrame() {},
    onError() {},
    onExit() {},
  });
  session.setRunID('run-finished');
  child.stdout.write('{"type":"run.finished","version":"yawr.stdio/v1","runID":"run-finished","status":"completed"}\n');

  session.dispose();

  assert.equal(child.stdin.read(), null, 'terminal disposal must not write run.cancel');
  assert.equal(child.killed, true);
});

test('DirectRunSession ignores frames after run.finished', async () => {
  const child = fakeChild();
  const frames = [];
  new DirectRunSession(child, {
    onFrame: (frame) => frames.push(frame),
    onError() {},
    onExit() {},
  });
  child.stdout.write(
    '{"type":"run.started","version":"yawr.stdio/v1","runID":"run-terminal"}\n' +
    '{"type":"run.finished","version":"yawr.stdio/v1","runID":"run-terminal","status":"completed"}\n' +
    '{"type":"run.event","version":"yawr.stdio/v1","runID":"run-terminal","event":{"kind":"step/completed"}}\n',
  );
  const stdoutEnded = once(child.stdout, 'end');
  child.stdout.end();
  await stdoutEnded;

  assert.deepEqual(frames.map((frame) => frame.type), ['run.started', 'run.finished']);
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