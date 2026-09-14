const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const { transformSync } = require('esbuild');
const progress = require('../out/executionProgress');
const status = require('../out/runStatus');
const source = fs.readFileSync(require.resolve('../webview/graph.tsx'), 'utf8');
const reducer = source.slice(source.indexOf('function applyRuntimeEvent('), source.indexOf('function applyTerminalSteps('));
const apply = vm.runInNewContext(transformSync(reducer, { loader: 'ts', target: 'es2022' }).code + '\napplyRuntimeEvent', {
  ...progress, ...status, ...require('../out/displayObservations'), terminalPresentation: () => ({}),
  eventNodeID: event => event.payload.qualified_node_id,
  recordValue: value => value && typeof value === 'object' ? value : undefined,
});
const event = (kind, sequence, identity = {}) => ({ kind: `step/${kind}`, run_id: 'run', sequence,
  timestamp: `2026-09-07T18:00:${String(sequence).padStart(2, '0')}Z`,
  payload: { qualified_node_id: 'include/branch/tool', phase: 'execute', invocation: 1,
    retry_attempt: 1, occurrence_sequence: 1, frame_id: 'frame', frame_step_index: 0,
    dispatch_occurrence_id: 'dispatch', execution_lane: 'left', ...identity } });
const id = 'include/branch/tool';
const graph = (...nodes) => ({ nodes: nodes.map(([id, kind = 'noop']) => ({
  id, data: { id, kind, title: `Title ${id}` }, position: { x: 0, y: 0 },
})), frames: [], groups: [], edges: [], runbook: { id: 'synthetic' } });

for (const terminal of ['completed', 'failed', 'indeterminate', 'skipped', 'cancelled', 'denied', 'blocked']) {
  test(`direct compatibility: late Started/Resumed/Delaying cannot regress ${terminal}`, () => {
    const complete = apply({}, event(terminal, 1));
    for (const late of ['started', 'resumed', 'delaying']) {
      const next = apply(complete, event(late, 2));
      assert.equal(next[id].status, terminal);
      assert.equal(next[id].occurrences[0].status, terminal);
      assert.equal(next[id].lastActivityAt, complete[id].lastActivityAt);
    }
  });
}
for (const identity of [
  { invocation: 2 }, { retry_attempt: 2 }, { phase: 'compensate' }, { frame_id: 'other' },
  { frame_step_index: 1 }, { dispatch_occurrence_id: 'other' }, { execution_lane: 'right' },
  { occurrence_sequence: 2 },
]) {
  test(`direct new occurrence is running: ${JSON.stringify(identity)}`, () => {
    const next = apply(apply({}, event('completed', 1)), event('started', 2, identity));
    assert.equal(next[id].status, 'running');
    assert.equal(next[id].occurrences.length, 2);
    assert.equal(next[id].finishedAt, undefined);
  });
}
test('old invocation terminal/late start cannot replace the current invocation, without occurrence_sequence', () => {
  let state = apply({}, event('started', 1, { occurrence_sequence: undefined }));
  state = apply(state, event('started', 2, { invocation: 2, occurrence_sequence: undefined }));
  state = apply(state, event('failed', 3, { occurrence_sequence: undefined }));
  state = apply(state, event('started', 4, { occurrence_sequence: undefined }));
  assert.equal(state[id].status, 'running');
  assert.equal(state[id].occurrences.find(value => value.invocation === 1).status, 'failed');
  assert.equal(progress.latestProgress(state[id]).invocation, 2);
});
test('canonical counts use one nonoverlapping domain, blocked output takes precedence, external children excluded', () => {
  const doc = graph(['done'], ['blocked'], ['skip'], ['active'], ['not-visited'], ['include', 'include']);
  const runtime = {
    done: { status: 'completed' }, blocked: { status: 'completed', output: { outcome_category: 'blocked' } },
    skip: { status: 'skipped' }, active: { status: 'running' }, include: { status: 'running' },
    'include/dynamic/tool': { status: 'running' },
  };
  const counts = progress.canonicalProgress(doc, runtime, 'running');
  assert.deepEqual(counts, { total: 6, completed: 1, issues: 1, skipped: 1, running: 2, remaining: 1, missingFinal: 0 });
  const ended = progress.canonicalProgress(doc, runtime, 'completed');
  assert.equal(ended.running, 0);
  assert.equal(ended.remaining, 3);
  assert.equal(ended.missingFinal, 3);
  assert.equal(ended.completed + ended.issues + ended.skipped + ended.running + ended.remaining, ended.total);
});
test('persisted stale top-level status is normalized only from known terminal occurrence evidence', () => {
  const complete = apply({}, event('completed', 1));
  const stale = { ...complete[id], status: 'running' };
  assert.equal(progress.normalizeRuntimeStatuses({ [id]: stale })[id].status, 'completed');
  assert.equal(stale.status, 'running', 'input snapshots are not rewritten');
  assert.equal(progress.normalizeRuntimeStatuses({ unknown: { status: 'running' } }).unknown.status, 'running');
});
test('parallel runtime occurrences and dynamic child show exact paths; tool/input/paused use actual metadata', () => {
  let runtime = apply({}, event('started', 1));
  runtime = apply(runtime, event('started', 2, { execution_lane: 'right', dispatch_occurrence_id: 'right' }));
  runtime.include = { status: 'running', qualifiedNodeID: 'include' };
  const doc = graph(['include', 'include'], [id, 'tool']);
  let activities = progress.currentActivities(doc, runtime, 'running', 'run');
  assert.equal(activities.length, 3);
  assert.equal(activities[0].label, 'Waiting for tool result');
  assert.equal(activities[2].label, 'Waiting for child steps');
  assert.ok(activities.slice(0, 2).every(value => value.path === id));
  activities = progress.currentActivities(graph(['include', 'include']), runtime, 'waiting', 'run',
    { nodeID: id, turnID: 'turn', kind: 'collector' });
  assert.equal(activities[0].label, 'Waiting for input');
  assert.equal(activities[0].inGraph, false);
  assert.equal(progress.currentActivities(doc, runtime, 'paused', 'run')[0].label, 'Paused');
  assert.deepEqual(progress.currentActivities(doc, runtime, 'completed', 'run'), []);
  assert.deepEqual(progress.currentActivities(doc, runtime, 'cancelled', 'run'), []);
});
test('resolved old turn does not manufacture input waiting and unknown child kind stays Active', () => {
  const runtime = { 'include/external': { status: 'running', runID: 'new' } };
  const active = progress.currentActivities(graph(), runtime, 'waiting', 'new',
    { nodeID: 'include/external', turnID: 'old-turn', runID: 'old', kind: 'collector' });
  assert.equal(active[0].label, 'Active');
  assert.equal(active[0].path, 'include/external');
});
test('late old framed terminal does not replace newer frame when both use occurrence_sequence one', () => {
  let runtime = apply({}, event('started', 1, { frame_id: 'z-old-frame' }));
  runtime = apply(runtime, event('started', 2, { frame_id: 'a-new-frame' }));
  runtime = apply(runtime, event('completed', 3, { frame_id: 'z-old-frame' }));
  assert.equal(runtime[id].status, 'running');
  assert.equal(progress.latestProgress(runtime[id]).frameID, 'a-new-frame');
  assert.equal(progress.currentActivities(graph([id, 'tool']), runtime, 'running', 'run')[0].frameID, 'a-new-frame');
});
test('closed session status suppresses active locations without fabricating step completion', () => {
  const runtime = { child: { status: 'running' } };
  for (const status of ['resolved', 'escalated', 'abandoned']) {
    assert.deepEqual(progress.currentActivities(graph(['child']), runtime, status), []);
    assert.equal(progress.canonicalProgress(graph(['child']), runtime, status).remaining, 1);
  }
});

const identityFields = ['phase', 'invocation', 'retry_attempt', 'occurrence_sequence',
  'frame_id', 'frame_step_index', 'dispatch_occurrence_id', 'execution_lane'];
for (const field of identityFields) {
  for (const reverse of [false, true]) {
    test(`direct identity requires symmetric presence: ${field}, reverse=${reverse}`, () => {
      const known = { occurrence_sequence: 9 }, absent = { ...known, [field]: undefined };
      let runtime = apply({}, event('started', 1, reverse ? absent : known));
      runtime = apply(runtime, event('completed', 2, reverse ? absent : known));
      runtime = apply(runtime, event('started', 3, reverse ? known : absent));
      assert.equal(runtime[id].occurrences.length, 2);
      assert.equal(progress.latestProgress(runtime[id]).status, 'running');
    });
  }
  test(`direct symmetric absence retains legacy terminal: ${field}`, () => {
    const identity = { [field]: undefined };
    const runtime = apply(apply({}, event('completed', 1, identity)), event('started', 2, identity));
    assert.equal(runtime[id].status, 'completed');
    assert.equal(runtime[id].occurrences.length, 1);
  });
  test(`direct invalid identity is not known evidence: ${field}`, () => {
    for (const invalid of [null, -1, false, {}, 0.5, '']) {
      const runtime = apply(apply({}, event('completed', 1)), event('started', 2, { [field]: invalid }));
      assert.equal(runtime[id].status, 'running', `${field}: ${JSON.stringify(invalid)}`);
      assert.equal(runtime[id].occurrences.length, 2);
    }
  });
}
test('direct frame/lane resets use starts; retries/loop invocations remain local', () => {
  let runtime = apply({}, event('started', 1, { occurrence_sequence: 9, invocation: 9 }));
  runtime = apply(runtime, event('completed', 2, { occurrence_sequence: 9, invocation: 9 }));
  runtime = apply(runtime, event('started', 3, { frame_id: 'next-loop', invocation: 1, occurrence_sequence: 1 }));
  runtime = apply(runtime, event('started', 4, { frame_id: 'next-loop', retry_attempt: 2, occurrence_sequence: 1 }));
  runtime = apply(runtime, event('completed', 5, { frame_id: 'next-loop', retry_attempt: 1, occurrence_sequence: 1 }));
  runtime = apply(runtime, event('started', 6, { execution_lane: 'right', frame_id: 'concurrent', occurrence_sequence: 1 }));
  assert.equal(progress.latestProgress(runtime[id]).frameID, 'concurrent');
  const active = progress.currentActivities(graph([id, 'tool']), runtime, 'running', 'run');
  assert.equal(active.length, 2);
  assert.ok(active.some(item => item.frameID === 'next-loop' && item.retryAttempt === 2));
});
test('dynamic producer tool kind is truthful; arbitrary output kind is not classification', () => {
  const runtime = apply({}, event('started', 1, { kind: 'tool' }));
  assert.equal(progress.currentActivities(graph(), runtime, 'running', 'run')[0].label, 'Waiting for tool result');
  const unknown = apply({}, event('started', 1, { output: { kind: 'tool' }, kind: {} }));
  assert.equal(progress.currentActivities(graph(), unknown, 'running', 'run')[0].label, 'Active');
});
test('terminal display projects every active status without touching occurrence history', () => {
  for (const status of ['running', 'waiting', 'delaying']) {
    const runtime = { child: { status, occurrences: [{ status, runID: 'run', occurrenceID: 'one' }] }, unvisited: { status: 'pending' } };
    const view = progress.displayRuntimeStatuses(runtime, 'indeterminate');
    assert.equal(view.child.status, 'no-final-status');
    assert.equal(view.unvisited.status, 'pending');
    assert.equal(runtime.child.status, status);
    assert.equal(view.child.occurrences, runtime.child.occurrences);
    assert.equal(view.child.occurrences[0].status, status);
    assert.equal(progress.canonicalProgress(graph(['child'], ['unvisited']), runtime, 'indeterminate').missingFinal, 2);
  }
});
test('actual App pending handler rejects stale lifecycle and turn identities', () => {
  const start = source.indexOf("      } else if (message.type === 'run.frame') {") + "      } else if (message.type === 'run.frame') {".length;
  const end = source.indexOf("      } else if (message.type === 'run.error')", start);
  const pendingRef = {}, runFinishedRef = { current: false }, runIDRef = { current: 'run' };
  let pending, runStatus = 'running';
  const pacedStarts = [];
  const bypasses = [];
  let deferPending = false;
  const pendingUpdates = [];
  const noop = () => {};
  const receive = vm.runInNewContext(transformSync(`function receive(message) {${source.slice(start, end)}}\nreceive`, { loader: 'ts' }).code, {
    pendingRef, runFinishedRef, runIDRef, directRunScopeRef: { current: 'run' },
    pacer: { bypass(step) { bypasses.push(step); }, complete() {}, ordinary(nodeID) { pacedStarts.push(nodeID); } },
    settledVisualOccurrencesRef: { current: new Set() }, ...progress, ...require('../out/executionView'),
    eventNodeID: event => event.payload?.qualified_node_id,
    recordValue: value => value && typeof value === 'object' ? value : undefined,
    directDocumentRef: { current: graph([id]) }, resolvedTurnsRef: { current: new Set() }, hostRequestRef: {},
    clearActiveRun() { pending = pendingRef.current = undefined; runIDRef.current = undefined; },
    setPending(value) {
      if (deferPending && typeof value === 'function') pendingUpdates.push(value);
      else pending = typeof value === 'function' ? value(pending) : value;
    },
    setRunStatus(value) { runStatus = typeof value === 'function' ? value(runStatus) : value; },
    setExecutionNodeID: noop, setRunID: noop, setRunStarting: noop, setRouteTestRunning: noop,
    setRuntimeNodes: noop, setRouteTestOutcome: noop, setRunError: noop, setResults: noop, ...status,
  });
  const send = (runID, turnID) => receive({ frame: { type: 'interaction.pending', interaction: { runID, turnID, kind: 'collector', nodeID: id } } });
  send('old', 'turn'); send('run', ''); send('run', undefined);
  assert.equal(pending, undefined); assert.equal(runStatus, 'running');
  send('run', 'valid');
  assert.equal(pending.turnID, 'valid'); assert.equal(runStatus, 'waiting');
  receive({ frame: { type: 'run.event', event: event('started', 1, { invocation: 10 }) } });
  assert.deepEqual(pacedStarts, [], 'concurrent starts cannot displace an actionable prompt');
  send('run', 'different');
  assert.equal(pending.turnID, 'valid');
  receive({ frame: { type: 'interaction.resolved', runID: 'old', turnID: 'valid' } });
  assert.equal(pending.turnID, 'valid');
  deferPending = true;
  receive({ frame: { type: 'interaction.resolved', runID: 'run', turnID: 'valid' } });
  assert.equal(bypasses.at(-1).nodeID, id, 'resolved prompts retain their marker until the next paced step');
  assert.equal(bypasses.at(-1).progressing, false, 'prompt resolution must not manufacture ordinary progress');
  receive({ frame: { type: 'run.event', event: event('started', 2, { invocation: 20 }) } });
  assert.deepEqual(pacedStarts, [id], 'the next live start must be queued even before React commits prompt resolution');
  pacedStarts.length = 0;
  deferPending = false;
  for (const update of pendingUpdates) pending = update(pending);
  send('run', 'valid');
  assert.equal(pending, undefined); assert.equal(runStatus, 'running');
  receive({ frame: { type: 'run.event', event: event('completed', 1) } });
  receive({ frame: { type: 'run.event', event: event('started', 2) } });
  assert.deepEqual(pacedStarts, [], 'a late start must not enter the visual pacing queue');
  receive({ frame: { type: 'run.event', event: event('started', 3, { invocation: 2 }) } });
  assert.deepEqual(pacedStarts, [id], 'a new invocation remains eligible for pacing');
  receive({ frame: { type: 'run.finished', status: 'indeterminate' } });
  send('run', 'new'); send('old', 'old');
  assert.equal(pending, undefined); assert.equal(runStatus, 'indeterminate');
  runFinishedRef.current = false; runIDRef.current = 'run';
  receive({ frame: { type: 'run.event', event: { kind: 'run/completed', run_id: 'run' } } });
  receive({ frame: { type: 'run.started', runID: 'old' } });
  send('old', 'late-after-terminal-event');
  assert.equal(pending, undefined); assert.equal(runStatus, 'completed');
});
test('static parallel group identity maps qualified runtime children for counts and Locate only', () => {
  const doc = graph(['together', 'parallel'], ['left', 'tool'], ['right', 'tool'], ['unvisited']);
  doc.groups = [{ id: 'left-group', kind: 'parallel-branch', parent_node_id: 'together' },
    { id: 'right-group', kind: 'parallel-branch', parent_node_id: 'together' }];
  doc.nodes[1].data.group_id = 'left-group'; doc.nodes[1].data.step_id = 'left';
  doc.nodes[2].data.group_id = 'right-group'; doc.nodes[2].data.step_id = 'right';
  const runtime = { together: { status: 'running' }, 'together/left': { status: 'running' }, 'together/right': { status: 'running' } };
  assert.equal(progress.canonicalProgress(doc, runtime, 'running').running, 3);
  const active = progress.currentActivities(doc, runtime, 'running');
  assert.ok(active.some(item => item.nodeID === 'left' && item.path === 'together/left' && item.inGraph));
  const view = progress.displayRuntimeStatuses(runtime, 'indeterminate', doc);
  assert.equal(view.left.status, 'no-final-status');
  assert.equal(view.right.status, 'no-final-status');
  assert.equal(runtime.left, undefined);
  assert.equal(runtime['together/left'].status, 'running');
  assert.equal(view.unvisited, undefined);
});
