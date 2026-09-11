const test = require('node:test');
const assert = require('node:assert/strict');
const { SessionGraphModel, sessionGraphNodeID } = require('../out/sessionCompositeGraph');
const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
function model() {
  const model = new SessionGraphModel(sessionID);
  model.applyGroup({ sequence: 1, writerEpoch: 1, handshake: false, graphs: [], frames: [{
    version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:snapshot',
    sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1, segmentID, runID,
    payload: { schema_version: 'investigation-session-manifest/v1',
      session: { session_id: sessionID, status: 'active', root_segment_id: segmentID,
        active_segment_id: segmentID, active_run_id: runID, sequence: 1 },
      segments: { [segmentID]: { segment_id: segmentID, ordinal: 1, runbook_id: 'safe',
        runbook_name: 'Safe synthetic', status: 'active', attempt_run_ids: [runID] } },
      attempts: { [runID]: { run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'running' } },
      transitions: {}, occurrences: {}, accepted_commands: {},
    },
  }] });
  return model;
}
function send(model, sequence, kind, overrides = {}) {
  return model.applyGroup({ sequence, writerEpoch: 1, handshake: false, graphs: [], frames: [{
    version: 'yawr.session-stdio/v1', type: 'run.event', frameID: `sha256:event-${sequence}`,
    sessionID, sessionSequence: sequence, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1, segmentID, runID,
    payload: { event_id: `event-${sequence}`, run_id: runID, runbook_id: 'safe', sequence,
      timestamp: `2026-09-07T18:00:${String(sequence).padStart(2, '0')}Z`, kind: `step/${kind}`,
      payload: { qualified_node_id: 'include/parallel/child', phase: 'execute', invocation: 1,
        retry_attempt: 1, frame_id: 'root/include', frame_step_index: 0,
        dispatch_occurrence_id: 'dispatch', execution_lane: 'left', ...overrides } },
  }] });
}
const state = model => model.snapshot().runtimeNodes[sessionGraphNodeID(sessionID, segmentID, 'include/parallel/child')];
for (const terminal of ['completed', 'failed', 'cancelled', 'blocked', 'denied']) {
  test(`session old producer late Started is accepted without regressing ${terminal}`, () => {
    const value = model();
    send(value, 2, terminal);
    send(value, 3, 'started');
    assert.equal(value.snapshot().sequence, 3);
    assert.equal(state(value).status, terminal);
    assert.equal(state(value).occurrences.length, 1);
    send(value, 4, 'started', { invocation: 2 });
    assert.equal(state(value).status, 'running');
  });
}
test('session late terminal from older invocation cannot replace newer running; lane identity is distinct', () => {
  const value = model();
  send(value, 2, 'started');
  send(value, 3, 'started', { invocation: 2 });
  send(value, 4, 'failed');
  assert.equal(state(value).status, 'running');
  assert.equal(state(value).invocation, 2);
  send(value, 5, 'started', { invocation: 2, execution_lane: 'right' });
  assert.equal(state(value).occurrences.length, 3);
  assert.equal(state(value).executionLane, 'right');
});
test('unqualified output log cannot invent a new pending occurrence over an active framed tool', () => {
  const value = model();
  send(value, 2, 'started');
  send(value, 3, 'output', { invocation: undefined, retry_attempt: undefined, phase: undefined,
    frame_id: undefined, frame_step_index: undefined, dispatch_occurrence_id: undefined,
    execution_lane: undefined, line: 'synthetic local tool output' });
  assert.equal(state(value).status, 'running');
  assert.equal(state(value).occurrences.length, 1);
  assert.equal(state(value).logs[0].line, 'synthetic local tool output');
});
test('session new frame ordering uses known start sequence, not random frame ID', () => {
  const value = model();
  send(value, 2, 'started', { frame_id: 'z-old', occurrence_sequence: 1 });
  send(value, 3, 'started', { frame_id: 'a-new', occurrence_sequence: 1 });
  send(value, 4, 'completed', { frame_id: 'z-old', occurrence_sequence: 1 });
  assert.equal(state(value).status, 'running');
  assert.equal(state(value).frameID, 'a-new');
});
for (const field of ['phase', 'invocation', 'retry_attempt', 'occurrence_sequence',
  'frame_id', 'frame_step_index', 'dispatch_occurrence_id', 'execution_lane']) {
  for (const reverse of [false, true]) {
    test(`session symmetric identity presence: ${field}, reverse=${reverse}`, () => {
      const value = model(), known = { occurrence_sequence: 9 }, absent = { ...known, [field]: undefined };
      send(value, 2, 'started', reverse ? absent : known);
      send(value, 3, 'completed', reverse ? absent : known);
      send(value, 4, 'started', reverse ? known : absent);
      assert.equal(state(value).occurrences.length, 2);
      assert.equal(state(value).status, 'running');
    });
  }
  test(`session both absent remains compatible and invalid is uncertain: ${field}`, () => {
    const value = model();
    send(value, 2, 'completed', { [field]: undefined });
    send(value, 3, 'started', { [field]: undefined });
    assert.equal(state(value).status, 'completed');
    assert.equal(state(value).occurrences.length, 1);
    send(value, 4, 'started', { [field]: null });
    assert.equal(state(value).status, 'running');
    assert.equal(state(value).occurrences.length, 2);
  });
}
test('session loop frame and concurrent lane counters reset without regressing latest', () => {
  const value = model();
  send(value, 2, 'started', { frame_id: 'old', invocation: 8, occurrence_sequence: 9 });
  send(value, 3, 'completed', { frame_id: 'old', invocation: 8, occurrence_sequence: 9 });
  send(value, 4, 'started', { frame_id: 'next-loop', occurrence_sequence: 1, kind: 'tool' });
  send(value, 5, 'started', { frame_id: 'next-loop', occurrence_sequence: 1, retry_attempt: 2 });
  send(value, 6, 'completed', { frame_id: 'next-loop', occurrence_sequence: 1, retry_attempt: 1 });
  assert.equal(state(value).retryAttempt, 2);
  assert.equal(state(value).status, 'running');
  send(value, 7, 'started', { frame_id: 'other', execution_lane: 'right', occurrence_sequence: 1, kind: 'tool' });
  assert.equal(state(value).frameID, 'other');
  assert.equal(state(value).stepKind, 'tool');
});
