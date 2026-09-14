const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const React = require('react');
const { renderToStaticMarkup } = require('react-dom/server');
const { transformSync } = require('esbuild');
const { currentExecutionNode, ordinaryVisualNodeID, executionViewMode, executionViewport, animateExecutionViewport, EXECUTION_PAN_DURATION } = require('../out/executionView');
const { currentActivities, graphExecutionNodeID } = require('../out/executionProgress');
const { projectWorkflow } = require('../out/workflowProjection');
const { sessionGraphTopologyKey } = require('../out/sessionCompositeGraph');
const ids = new Set(['parent', 'first', 'second', 'third']);
const active = (nodeID, extra = {}) => ({ nodeID, inGraph: true, container: false, status: 'running', ...extra });
const cursor = (activities, status = 'running', reached, previous, pending) =>
  currentExecutionNode(activities, status, ids, reached, previous, pending);

test('ordinary playback admits only real canonical graph identities and resolves aliases before queueing', () => {
  const document = {
    nodes: [{ id: 'parent', data: { kind: 'parallel' } },
      { id: 'child', data: { step_id: 'child', group_id: 'lane' } },
      { id: 'synthetic', data: { synthetic: true } },
      { id: 'entry', data: { kind: 'session-entry' } }],
    groups: [{ id: 'lane', kind: 'parallel-branch', parent_node_id: 'parent' }],
  };
  for (const identity of ['child', 'parent/child']) assert.equal(ordinaryVisualNodeID(document, identity), 'child');
  for (const identity of ['runtime-only-wrapper', 'parent/dynamic', 'synthetic', 'entry']) {
    assert.equal(ordinaryVisualNodeID(document, identity), undefined);
  }
  assert.equal(ordinaryVisualNodeID(undefined, 'child'), undefined);
});

test('actual renderer never falls back to runtime activity for a live current or progress marker', () => {
  const source = fs.readFileSync(require.resolve('../webview/graph.tsx'), 'utf8');
  const start = source.indexOf('  const currentNodeID = visualPlayback');
  const end = source.indexOf('  useLayoutEffect(', start);
  const resolve = vm.runInNewContext(
    transformSync(`function resolve(visualPlayback, visualStep, runStatus) {${source.slice(start, end)}return currentNodeID;}\nresolve`, { loader: 'ts' }).code,
    { document: {}, graphExecutionNodeID: (_document, id) => id === 'known' ? id : undefined,
      liveCurrentNodeID: 'immediate-runtime', isExecutionEnded: value => value === 'completed' },
  );
  assert.equal(resolve(true, { nodeID: 'known' }, 'running'), 'known');
  assert.equal(resolve(true, { nodeID: 'unknown' }, 'running'), undefined);
  assert.equal(resolve(false, undefined, 'running'), undefined);
  assert.equal(resolve(true, { nodeID: 'known' }, 'completed'), 'known');
  assert.equal(resolve(false, undefined, 'completed'), 'immediate-runtime', 'terminal Last reached remains available');
  const progress = source.slice(source.indexOf('  const executionProgressing ='), source.indexOf('  const executionPosition ='));
  assert.doesNotMatch(progress, /activities|runtimeNodes/);
});

test('current cursor survives completion/start gaps without modifying runtime evidence', () => {
  let previous;
  const steps = [
    { active: [active('first')], reached: 'first', expected: 'first' },
    { active: [], reached: 'first', expected: 'first' },
    { active: [active('second')], reached: 'second', expected: 'second' },
    { active: [], reached: 'second', expected: 'second' },
    { active: [active('third')], reached: 'third', expected: 'third' },
  ];
  for (const step of steps) {
    const before = JSON.stringify(step);
    previous = cursor(step.active, 'running', step.reached, previous);
    assert.equal(previous, step.expected);
    assert.equal(JSON.stringify(step), before);
  }
});

test('parallel starts retain a current leaf; containers yield to leaves and input takes priority', () => {
  const parent = active('parent', { container: true });
  assert.equal(cursor([parent, active('first')], 'running', 'first', 'parent'), 'first');
  assert.equal(cursor([active('second'), active('first'), parent], 'running', 'second', 'first'), 'first');
  assert.equal(cursor([active('second'), parent], 'running', 'second', 'first'), 'second');
  assert.equal(cursor([active('first'), active('second')], 'waiting', 'second', 'first', 'second'), 'second');
});

test('waiting, debug pause, retry delay and reconnect preserve the current location', () => {
  for (const status of ['waiting', 'paused', 'paused_at_boundary', 'handoff_pending', 'reconnecting', 'running']) {
    assert.equal(cursor([active('first', { status: 'delaying' })], status, 'first', 'first'), 'first');
    assert.equal(cursor([], status, 'first', 'first'), 'first');
  }
});

test('terminal outcomes retain last reached while new runs and reset clear the cursor', () => {
  for (const status of ['completed', 'failed', 'cancelled', 'indeterminate', 'resolved', 'abandoned']) {
    assert.equal(cursor([active('first')], status, 'second', 'first'), 'second');
  }
  for (const status of ['starting', 'idle', 'not-started']) {
    assert.equal(cursor([active('first')], status, 'first', 'first'), undefined);
  }
  assert.equal(cursor([], 'running', 'missing', 'removed'), undefined);
});

test('qualified parallel aliases select the actual loaded graph node, not an invented child', () => {
  const doc = { nodes: [{ id: 'parent', data: { kind: 'parallel' } },
    { id: 'first', data: { kind: 'tool', step_id: 'first', group_id: 'group' } }],
    groups: [{ id: 'group', kind: 'parallel-branch', parent_node_id: 'parent' }] };
  const activities = currentActivities(doc, {
    parent: { status: 'running' }, 'parent/first': { status: 'running' },
  }, 'running');
  assert.equal(cursor(activities, 'running', 'parent/first'), 'first');
  assert.equal(graphExecutionNodeID(doc, 'parent/first'), 'first');
  assert.equal(graphExecutionNodeID(doc, 'parent'), 'parent');
  assert.equal(graphExecutionNodeID(doc, 'parent/external'), undefined);
  assert.equal(cursor([], 'completed', graphExecutionNodeID(doc, 'parent/first'), 'second'), 'first');
  assert.equal(cursor([active('second'), ...activities], 'waiting', 'parent/first', 'second', 'parent/first'), 'first');
  const external = active('parent/external', { inGraph: false });
  assert.equal(cursor([external, active('parent', { container: true })]), 'parent');
  assert.equal(cursor([external], 'running', external.nodeID), undefined);
});

test('execution technical-step topology stays identical through all status transitions until reset', () => {
  const doc = {
    nodes: ['parent', 'first', 'second', 'third'].map(id => ({ id, data: { kind: 'assign' } })),
    edges: [['parent', 'first'], ['first', 'second'], ['second', 'third']].map(([source, target]) =>
      ({ id: `${source}-${target}`, source, target, type: 'sequence' })),
    frames: [], groups: [],
  };
  let topology;
  for (const [status, node] of [['starting', undefined], ['running', 'first'], ['waiting', 'second'],
    ['paused', 'second'], ['running', 'third'], ['completed', 'third'], ['failed', 'third']]) {
    const projection = projectWorkflow(doc, {}, {
      mode: executionViewMode('workflow', status), expandedNodeIDs: new Set(),
      pinnedNodeIDs: new Set(node ? [node] : []), collapsedGroupIDs: new Set(),
    });
    const key = sessionGraphTopologyKey(projection.document);
    topology ??= key;
    assert.equal(key, topology);
    assert.equal(projection.segments.size, 0);
  }
  assert.equal(executionViewMode('workflow', 'idle'), 'workflow');
  assert.equal(executionViewMode('all', 'idle'), 'all');
});

const viewport = { x: -100, y: -200, zoom: 0.8 };
const canvas = { left: 50, top: 100, right: 850, bottom: 700 };
test('fully visible nodes preserve viewport exactly, including nodes close to an edge', () => {
  for (const node of [
    { left: 100, top: 150, right: 300, bottom: 250 },
    { left: 50, top: 100, right: 850, bottom: 700 },
    { left: 51, top: 101, right: 260, bottom: 185 },
  ]) assert.equal(executionViewport(viewport, canvas, node), undefined);
});

test('offscreen steps use minimal bounded-duration pan, preserve zoom and do not recenter the other axis', () => {
  assert.deepEqual(executionViewport(viewport, canvas, { left: 100, top: 720, right: 300, bottom: 800 }),
    { x: -100, y: -324, zoom: 0.8 });
  assert.deepEqual(executionViewport(viewport, canvas, { left: -100, top: 150, right: 100, bottom: 250 }),
    { x: 74, y: -200, zoom: 0.8 });
  assert.deepEqual(executionViewport(viewport, canvas, { left: 860, top: 10, right: 1060, bottom: 90 }),
    { x: -334, y: -86, zoom: 0.8 });
  assert.ok(EXECUTION_PAN_DURATION > 0 && EXECUTION_PAN_DURATION <= 300);
  assert.deepEqual(viewport, { x: -100, y: -200, zoom: 0.8 });
});

test('oversized nodes covering the viewport never oscillate; unmeasured or hidden canvases do not move', () => {
  assert.equal(executionViewport(viewport, canvas, { left: 0, top: 0, right: 900, bottom: 900 }), undefined);
  assert.equal(executionViewport(viewport, canvas, { left: 0, top: 0, right: 0, bottom: 0 }), undefined);
  assert.equal(executionViewport(viewport, { ...canvas, right: canvas.left }, canvas), undefined);
});

function animationClock() {
  let time = 0, id = 0;
  const callbacks = new Map();
  return {
    now: () => time,
    requestFrame(callback) { callbacks.set(++id, callback); return id; },
    cancelFrame(frame) { callbacks.delete(frame); },
    tick(next) {
      time = next;
      const pending = [...callbacks.values()];
      callbacks.clear();
      pending.forEach(callback => callback(time));
    },
    get pending() { return callbacks.size; },
  };
}

test('the actual webview animation adapter preserves the browser Window receiver', () => {
  const source = fs.readFileSync(require.resolve('../webview/graph.tsx'), 'utf8');
  const start = source.indexOf('{ now: () => performance.now(), requestFrame:');
  const end = source.indexOf('},', start) + 1;
  assert.ok(start >= 0 && end > start);
  const browser = {
    requestAnimationFrame(callback) {
      if (this !== browser) throw new TypeError('Illegal invocation');
      callback(20);
      return 7;
    },
    cancelAnimationFrame(frame) {
      if (this !== browser) throw new TypeError('Illegal invocation');
      assert.equal(frame, 7);
    },
  };
  const clock = vm.runInNewContext(`(${source.slice(start, end)})`, {
    window: browser, requestAnimationFrame: browser.requestAnimationFrame,
    cancelAnimationFrame: browser.cancelAnimationFrame, performance: { now: () => 0 },
  });
  let sampled;
  const frame = clock.requestFrame(time => { sampled = time; });
  assert.equal(sampled, 20);
  clock.cancelFrame(frame);
});

test('animated pan has smooth monotonic intermediate frames, constant zoom and an exact bounded endpoint', () => {
  const clock = animationClock(), frames = [];
  const to = { x: 80, y: -1600, zoom: viewport.zoom };
  animateExecutionViewport(viewport, to, value => frames.push(value), clock, false);
  for (let time = 0; time <= 280; time += 20) clock.tick(time);
  assert.equal(frames.length, 15);
  assert.deepEqual(frames[0], viewport);
  assert.deepEqual(frames.at(-1), to);
  assert.ok(frames.every(value => value.zoom === viewport.zoom));
  assert.ok(frames.slice(1).every((value, index) => value.y < frames[index].y && value.x > frames[index].x));
  assert.equal(clock.pending, 0);
});

test('rapid advances, terminal transitions and user gestures cancel the old pan without late writes', () => {
  const clock = animationClock(), frames = [];
  const cancel = animateExecutionViewport(viewport, { ...viewport, y: -1600 }, value => frames.push(value), clock, false);
  clock.tick(40);
  cancel();
  const count = frames.length;
  clock.tick(80);
  assert.equal(frames.length, count);
  assert.equal(clock.pending, 0);
  const next = { ...viewport, x: 100 };
  animateExecutionViewport(frames.at(-1), next, value => frames.push(value), clock, false);
  clock.tick(360);
  assert.deepEqual(frames.at(-1), next);
});

test('reduced motion applies exactly one viewport update and schedules no animation', () => {
  const clock = animationClock(), frames = [], to = { ...viewport, y: -500 };
  animateExecutionViewport(viewport, to, value => frames.push(value), clock, true)();
  assert.deepEqual(frames, [to]);
  assert.equal(clock.pending, 0);
});

test('production wiring has no top log, defers no node update to an effect, and keeps evidence in inspector', () => {
  const source = fs.readFileSync(require.resolve('../webview/graph.tsx'), 'utf8');
  const css = fs.readFileSync(require.resolve('../webview/graph.css'), 'utf8');
  assert.match(source, /<aside[^>]*className="inspector"[^>]*>\s*<ActivityDetails/);
  assert.equal((source.match(/<ActivityDetails/g) || []).length, 1);
  assert.doesNotMatch(css, /execution-position-strip/);
  assert.match(source, /const renderNodes = useMemo\(\(\) => preserveLayoutMeasurements\(displayNodes, measuredNodes\)/);
  assert.doesNotMatch(source, /setRenderNodes/);
  assert.match(source, /diagnostics=\{runDiagnostics\}/);
  assert.match(source, /logs: appendRuntimeLog/);
  assert.match(source, /aria-current=\{isCurrent && !executionPosition.terminal \? 'step'/);
  assert.match(source, /executionViewport\(flow.getViewport\(\), canvas.getBoundingClientRect\(\), target.getBoundingClientRect\(\)\)/);
  const animation = css.match(/@keyframes execution-progress \{([\s\S]*?)\n\}/)?.[1];
  assert.ok(animation);
  assert.doesNotMatch(animation, /width|height|padding|margin|transform|outline/);
  assert.match(css, /prefers-reduced-motion: reduce/);
});

test('actual step renderer distinguishes current work, input, pause, blocked and terminal evidence', () => {
  const source = fs.readFileSync(require.resolve('../webview/graph.tsx'), 'utf8');
  const code = transformSync(source.slice(source.indexOf('function StepNode('), source.indexOf('function FrameNode(')),
    { loader: 'tsx' }).code;
  for (const [status, context, expected] of [
    ['running', { progressing: true }, 'running'],
    ['running', { status: 'waiting' }, 'waiting'],
    ['running', { status: 'paused' }, 'paused'],
    ['delaying', {}, 'delaying'],
    ['completed', {}, 'completed'],
    ['failed', {}, 'failed'],
    ['completed', { blocked: true }, 'blocked'],
    ['running', { terminal: true }, 'no-final-status'],
  ]) {
    const runtime = { first: { status, ...(context.blocked ? { output: { outcome_category: 'blocked' } } : {}) } };
    const component = vm.runInNewContext(code + '\nStepNode', {
      React, useContext: value => value, RuntimeNodesContext: runtime,
      ExecutionPositionContext: { nodeID: 'first', terminal: false, ...context },
      DebugBreakpointsContext: new Set(), breakpointKey: (id, phase) => `${id}:${phase}`,
      kindLabels: {}, Position: { Left: 'left', Right: 'right', Top: 'top', Bottom: 'bottom' },
      NodeToolbar: ({ isVisible, children }) => isVisible ? React.createElement('div', null, children) : null,
      Handle: () => null, ArrowLeft: () => null, ArrowRight: () => null,
    });
    const html = renderToStaticMarkup(React.createElement(component, { data: { id: 'first', kind: 'tool' }, selected: true }));
    assert.ok(html.includes(`status-${expected}`), html);
    assert.equal(html.includes('aria-current="step"'), !context.terminal);
    assert.equal(html.includes(' execution-progress'), !!context.progressing);
    assert.ok(html.includes(' selected'));
    assert.equal(runtime.first.status, status, 'visual state must not rewrite run evidence');
  }
});
