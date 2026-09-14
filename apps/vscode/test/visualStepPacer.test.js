const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { VisualStepPacer, minimumStepDisplayMs } = require('../out/visualStepPacer');
const { sessionVisualSteps, sessionGraphNodeID } = require('../out/sessionCompositeGraph');

function harness(interval, requireCommit = false) {
  let now = 0, id = 0;
  const timers = new Map(), writes = [], cancelled = [];
  const clock = {
    now: () => now,
    setTimeout(callback, delay) { timers.set(++id, { callback, at: now + delay }); return id; },
    clearTimeout(id) { cancelled.push(timers.get(id)?.callback); timers.delete(id); },
  };
  const pacer = new VisualStepPacer(step => writes.push({ at: now, step }), clock, requireCommit);
  pacer.start(interval);
  writes.length = 0;
  return {
    pacer, writes, cancelled, timers,
    advance(to) {
      for (;;) {
        const entry = [...timers].filter(([, timer]) => timer.at <= to).sort((a, b) => a[1].at - b[1].at)[0];
        if (!entry) break;
        now = entry[1].at; timers.delete(entry[0]); entry[1].callback();
      }
      now = to;
    },
  };
}

for (const interval of [undefined, 200, 500]) {
  test(`ordered live burst retains each cursor for ${interval ?? 'default 200'}ms`, () => {
    const value = interval ?? 200, h = harness(interval);
    h.pacer.ordinary('a'); h.pacer.ordinary('b'); h.pacer.ordinary('c');
    h.advance(value - 1);
    assert.deepEqual(h.writes.map(value => value.step.nodeID), ['a']);
    h.advance(value * 2);
    assert.deepEqual(h.writes, ['a', 'b', 'c'].map((nodeID, index) => ({
      at: index * value, step: { nodeID, progressing: true },
    })));
    assert.equal(h.timers.size, 0);
  });
}

test('zero delivers ordinary steps immediately without any timer', () => {
  const h = harness(0);
  for (const id of ['a', 'b', 'c']) h.pacer.ordinary(id);
  assert.deepEqual(h.writes.map(value => [value.at, value.step.nodeID]), [[0, 'a'], [0, 'b'], [0, 'c']]);
  assert.equal(h.timers.size, 0);
});

test('browser pacing counts dwell from the React commit, not event arrival or slow rendering', () => {
  const h = harness(200, true);
  h.pacer.ordinary('a'); h.pacer.ordinary('b');
  h.advance(1000);
  assert.equal(h.writes.length, 1);
  h.pacer.acknowledge(h.writes[0].step);
  h.advance(1199); assert.equal(h.writes.length, 1);
  h.advance(1200); assert.equal(h.writes.at(-1).step.nodeID, 'b');
  h.pacer.acknowledge(h.writes[0].step);
  assert.equal(h.timers.size, 0, 'stale commits cannot release a later displayed step');
});

test('a long-running current step can advance immediately after its minimum expires', () => {
  const h = harness(200);
  h.pacer.ordinary('a'); h.advance(1000); h.pacer.ordinary('b'); h.advance(1000);
  assert.equal(h.writes.at(-1).step.nodeID, 'b');
  assert.equal(h.writes.at(-1).at, 1000);
});

for (const reason of ['failure', 'cancelled', 'blocked', 'denied', 'interaction', 'debug pause', 'Results', 'runtime completion', 'user cancel', 'error']) {
  test(`${reason} bypass discards the visual backlog immediately`, () => {
    const h = harness(500);
    h.pacer.ordinary('a'); h.pacer.ordinary('b'); h.advance(50);
    const critical = ['Results', 'runtime completion', 'user cancel', 'error'].includes(reason)
      ? undefined : { nodeID: reason, progressing: false };
    h.pacer.bypass(critical);
    assert.deepEqual(h.writes.at(-1), { at: 50, step: critical });
    const count = h.writes.length;
    h.advance(2000);
    assert.equal(h.writes.length, count);
    assert.equal(h.timers.size, 0);
    h.pacer.ordinary('next');
    assert.equal(h.writes.at(-1).step.nodeID, 'next', 'critical states do not acquire an ordinary dwell interval');
  });
}

test('reconnect snapshots converge immediately and only subsequent live steps are paced', () => {
  const h = harness(200);
  h.pacer.ordinary('old'); h.pacer.ordinary('stale');
  for (const nodeID of ['replay-a', 'replay-b', 'head']) h.pacer.bypass({ nodeID, progressing: false });
  assert.equal(h.timers.size, 0);
  assert.equal(h.writes.at(-1).step.nodeID, 'head');
  h.pacer.ordinary('live-a'); h.pacer.ordinary('live-b');
  h.advance(199); assert.equal(h.writes.at(-1).step.nodeID, 'live-a');
  h.advance(200); assert.equal(h.writes.at(-1).step.nodeID, 'live-b');
});

test('hide clears timers, hidden events do not publish, reveal converges without replay', () => {
  const h = harness(200);
  h.pacer.ordinary('a'); h.pacer.ordinary('b'); h.pacer.setVisible(false);
  const count = h.writes.length;
  h.pacer.ordinary('c'); h.pacer.ordinary('d'); h.advance(1000);
  assert.equal(h.writes.length, count);
  assert.equal(h.timers.size, 0);
  h.pacer.setVisible(true);
  assert.deepEqual(h.writes.at(-1), { at: 1000, step: { nodeID: 'd', progressing: true } });
  h.pacer.ordinary('e'); h.advance(1199);
  assert.equal(h.writes.at(-1).step.nodeID, 'd');
});

test('disposed and cancelled timer callbacks cannot mutate a replacement graph instance', () => {
  const h = harness(200);
  h.pacer.ordinary('a'); h.pacer.ordinary('b'); h.pacer.dispose();
  const count = h.writes.length;
  for (const callback of h.cancelled) callback?.();
  h.pacer.start(0); h.pacer.setVisible(true); h.pacer.ordinary('late'); h.pacer.bypass();
  h.advance(500);
  assert.equal(h.writes.length, count);
});

test('duplicate status updates do not extend dwell, but an ordered loop revisit is retained', () => {
  const h = harness(200);
  h.pacer.ordinary('a'); h.advance(100); h.pacer.ordinary('a'); h.pacer.ordinary('b'); h.pacer.ordinary('a');
  h.advance(400);
  assert.deepEqual(h.writes.map(value => [value.at, value.step.nodeID]), [[0, 'a'], [200, 'b'], [400, 'a']]);
});

test('setting normalization is nonnegative integer; long intervals do not overflow browser timers', () => {
  assert.equal(minimumStepDisplayMs(undefined), 200);
  assert.equal(minimumStepDisplayMs('500'), 200);
  assert.equal(minimumStepDisplayMs(NaN), 200);
  assert.equal(minimumStepDisplayMs(-4), 0);
  assert.equal(minimumStepDisplayMs(500.8), 500);
  const h = harness(3_000_000_000);
  h.pacer.ordinary('a'); h.pacer.ordinary('b'); h.advance(2_147_483_647);
  assert.equal(h.writes.length, 1);
  h.advance(3_000_000_000);
  assert.equal(h.writes.at(-1).step.nodeID, 'b');
});

test('a newly started run snapshots the new setting and invalidates the old queue', () => {
  const h = harness(200);
  h.pacer.ordinary('old'); h.pacer.ordinary('obsolete');
  h.pacer.start(500);
  h.pacer.ordinary('new-a'); h.pacer.ordinary('new-b');
  h.advance(200); assert.equal(h.writes.at(-1).step.nodeID, 'new-a');
  h.advance(500); assert.equal(h.writes.at(-1).step.nodeID, 'new-b');
});

test('session transactions retain all start identities in event order without forwarding raw payloads', () => {
  const frames = ['started', 'completed', 'resumed'].map((kind, index) => ({
    type: 'run.event', sessionID: 'session', segmentID: 'segment',
    payload: { kind: `step/${kind}`, payload: { qualified_node_id: `node-${index}`, output: { private: 'not-forwarded' } } },
  }));
  assert.deepEqual(sessionVisualSteps({ frames }), [
    sessionGraphNodeID('session', 'segment', 'node-0'), sessionGraphNodeID('session', 'segment', 'node-2'),
  ]);
  const nodeID = sessionGraphNodeID('session', 'segment', 'node-0');
  frames[0].payload.sequence = 10;
  assert.deepEqual(sessionVisualSteps({ frames: [frames[0]] }, {
    [nodeID]: { status: 'completed', occurrences: [{ startedEventSequence: 1 }] },
  }), [], 'ignored late Started is not replayed visually');
  assert.deepEqual(sessionVisualSteps({ frames: [frames[0]] }, {
    [nodeID]: { status: 'completed', occurrences: [{ startedEventSequence: 10 }] },
  }), [nodeID], 'a start completed within the same transaction is still paced');
});

test('setting metadata and host lifecycle wiring keep pacing strictly in the webview', () => {
  const manifest = require('../package.json');
  const setting = manifest.contributes.configuration.properties['yawr.preview.minimumStepDisplayMs'];
  assert.equal(setting.type, 'integer'); assert.equal(setting.default, 200); assert.equal(setting.minimum, 0);
  assert.match(setting.description, /editor visualization only.*runtime execution.*never delayed/);
  assert.match(setting.description, /next run or session/);
  const host = fs.readFileSync(require.resolve('../src/extension.ts'), 'utf8');
  assert.match(host, /liveAttachment && !group.handshake/);
  assert.match(host, /if \(group.handshake\) liveAttachment = true/);
  assert.equal((host.match(/minimumStepDisplayMs: pacingInterval\(\)/g) || []).length, 4);
  assert.doesNotMatch(host, /new VisualStepPacer|await.*pacingInterval/);
  assert.match(host, /panel.onDidChangeViewState/);
  const view = fs.readFileSync(require.resolve('../webview/graph.tsx'), 'utf8');
  assert.match(view, /const cancelRun = \(\) => \{\s*visualPacerRef.current\?\.bypass\(\)/);
  assert.match(view, /pacer.dispose\(\)/);
  assert.match(view, /visibilitychange/);
});
