const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const { transformSync } = require('esbuild');

function observerHarness() {
  let socket, timerID = 0;
  const timers = new Map(), requests = [], instrumented = new Set();
  class Socket {
    constructor() { socket = this; queueMicrotask(() => this.onopen()); }
    event(method, params, sessionId) {
      this.onmessage({ data: JSON.stringify({ method, params, sessionId }) });
    }
    send(serialized) {
      const request = JSON.parse(serialized);
      requests.push(request);
      queueMicrotask(() => {
        const { id, method, params, sessionId } = request;
        this.onmessage({ data: JSON.stringify({ id, result: method === 'Runtime.evaluate'
          ? { result: { objectId: `observer:${sessionId ?? 'root'}` } }
          : method === 'Page.addScriptToEvaluateOnNewDocument' ? { identifier: `script:${sessionId ?? 'root'}` } : {} }) });
        if (method === 'Runtime.enable') {
          this.event('Runtime.executionContextCreated', { context: { id: 1, auxData: { isDefault: true } } }, sessionId);
        } else if (method === 'Runtime.evaluate') {
          instrumented.add(sessionId ?? 'root');
        } else if (method === 'Target.setAutoAttach' && params.autoAttach) {
          if (!sessionId) this.event('Target.attachedToTarget', { sessionId: 'outer', targetInfo: { type: 'iframe' } });
          if (sessionId === 'outer') this.event('Target.attachedToTarget', { sessionId: 'inner', targetInfo: { type: 'iframe' } }, 'outer');
        }
      });
    }
    close() {}
  }
  const output = { exports: {} };
  vm.runInNewContext(transformSync(fs.readFileSync(require.resolve('./suite/graphPlaybackObserver.ts'), 'utf8'), {
    loader: 'ts', format: 'cjs', target: 'es2022',
  }).code, {
    module: output, exports: output.exports, WebSocket: Socket,
    fetch: async () => ({ json: async () => [{ type: 'page', webSocketDebuggerUrl: 'ws://isolated-editor' }] }),
    setTimeout(callback, delay) { timers.set(++timerID, { callback, delay }); return timerID; },
    clearTimeout(id) { timers.delete(id); },
  });
  return {
    connect: () => output.exports.connectGraphObserver('1234'), requests,
    event: (method, params, sessionId) => socket.event(method, params, sessionId),
    sample(sessionId, sample) {
      if (instrumented.has(sessionId)) socket.event('Runtime.bindingCalled', {
        name: '__yawrReadOnlyGraphPlaybackSample', executionContextId: 1, payload: JSON.stringify({
          documentID: `${sessionId}:1`, graphTitle: 'Expected graph', visibility: 'visible',
          nodes: ['first', 'results'], ...sample,
        }),
      }, sessionId);
    },
    drain(delay = 8000) {
      for (const [id, timer] of timers) if (timer.delay === delay) {
        timers.delete(id); timer.callback();
      }
    },
  };
}

test('the installed observer records CURRENT in nested webview targets, not just the prior completed graph', async () => {
  const h = observerHarness();
  const observer = await h.connect();
  await new Promise(setImmediate);
  h.sample('outer', { at: 0, ids: [], progress: [], status: 'completed', graphTitle: 'Prior graph' });
  const readiness = observer.waitForGraph('Expected graph');
  h.sample('outer', { at: 0.5, ids: [], progress: [], status: 'idle' });
  h.drain(20);
  await readiness;
  const observation = observer.observe();
  h.sample('inner', { at: 1, ids: ['first'], progress: ['first'], status: 'running' });
  h.sample('outer', { at: 2, ids: [], progress: [], status: 'idle' });
  h.sample('outer', { at: 3, ids: ['unrelated'], progress: [], status: 'running', graphTitle: 'Prior graph' });
  h.sample('inner', { at: 501, ids: ['results'], progress: ['results'], status: 'completed', results: 'available' });
  h.sample('inner', { at: 1001, ids: [], progress: [], status: 'completed', results: 'available' });
  await new Promise(setImmediate);
  h.drain();
  const samples = await observation;
  await observer.close();
  assert.ok(samples.some(sample => sample.ids.length), 'the entire run must display ordinary current steps');
  assert.deepEqual(Array.from(samples.filter(sample => sample.ids.length), sample => sample.ids[0]), ['first', 'results']);
  assert.equal(samples.length, 3, 'another idle or differently titled graph must not manufacture a CURRENT gap');
});

test('the installed observer instruments future documents before enabling each nested target', async () => {
  const h = observerHarness();
  const observer = await h.connect();
  await new Promise(setImmediate);
  for (const sessionId of [undefined, 'outer', 'inner']) {
    const requests = h.requests.filter(request => request.sessionId === sessionId);
    const preload = requests.findIndex(request => request.method === 'Page.addScriptToEvaluateOnNewDocument');
    assert.ok(preload >= 0);
    assert.ok(preload < requests.findIndex(request => request.method === 'Runtime.enable'));
    assert.equal(requests[preload].params.source,
      requests.find(request => request.method === 'Runtime.evaluate').params.expression);
    assert.match(requests[preload].params.source, /if \(window\.__yawrReadOnlyGraphPlaybackSampleObserver\) return/);
  }
  await observer.close();
  assert.equal(h.requests.filter(request => request.method === 'Page.removeScriptToEvaluateOnNewDocument').length, 3);
});

test('the installed observer retains competing CURRENT streams and samples a longer scoped backlog', async () => {
  const h = observerHarness();
  const observer = await h.connect();
  await new Promise(setImmediate);
  h.sample('outer', { at: 0, ids: [], progress: [], status: 'idle' });
  await observer.waitForGraph('Expected graph');
  const observation = observer.observe(11500);
  h.sample('inner', { at: 1, ids: ['first'], progress: ['first'], status: 'running' });
  h.sample('outer', { at: 2, ids: ['duplicate'], progress: ['duplicate'], status: 'running' });
  h.sample('inner', { at: 10500, ids: ['results'], progress: ['results'], status: 'completed' });
  h.sample('inner', { at: 11000, ids: [], progress: [], status: 'completed' });
  await new Promise(setImmediate);
  h.drain(11500);
  const samples = await observation;
  await observer.close();
  assert.deepEqual(Array.from(samples.filter(sample => sample.ids.length), sample => sample.ids[0]), ['first', 'duplicate', 'results']);
  assert.equal(samples.at(-1).at, 11000, 'observation must cover the complete configured playback window');
});

test('the installed observer rejects destruction of the executing graph instead of hiding its replacement', async () => {
  const h = observerHarness();
  const observer = await h.connect();
  await new Promise(setImmediate);
  h.sample('outer', { at: 0, ids: [], progress: [], status: 'idle' });
  await observer.waitForGraph('Expected graph');
  const observation = observer.observe();
  h.sample('inner', { at: 1, ids: ['first'], progress: ['first'], status: 'running' });
  h.event('Runtime.executionContextDestroyed', { executionContextId: 1 }, 'inner');
  const rejected = assert.rejects(observation, /graph document was destroyed/);
  await new Promise(setImmediate);
  h.drain();
  await new Promise(setImmediate);
  h.sample('inner', { at: 1001, ids: [], progress: [], status: 'completed' });
  await new Promise(setImmediate);
  h.drain(1000);
  await rejected;
  await observer.close();
});

test('the installed playback window starts at the first CURRENT, not the idle preview', async () => {
  const h = observerHarness();
  const observer = await h.connect();
  await new Promise(setImmediate);
  h.sample('outer', { at: 0, ids: [], progress: [], status: 'idle' });
  await observer.waitForGraph('Expected graph');
  let finished = false;
  const observation = observer.observe().then(samples => { finished = true; return samples; });
  h.drain();
  await new Promise(setImmediate);
  assert.equal(finished, false, 'an idle preview cannot consume the execution sampling window');
  h.sample('inner', { at: 9000, ids: ['first'], progress: ['first'], status: 'running' });
  h.sample('inner', { at: 9500, ids: ['results'], progress: ['results'], status: 'completed' });
  h.sample('inner', { at: 10000, ids: [], progress: [], status: 'completed' });
  await new Promise(setImmediate);
  assert.equal(finished, false);
  h.drain();
  const samples = await observation;
  assert.deepEqual(Array.from(samples, sample => sample.ids[0]), ['first', 'results', undefined]);
  await observer.close();
});

test('the installed observer fails explicitly when CURRENT never arrives', async () => {
  const h = observerHarness();
  const observer = await h.connect();
  await new Promise(setImmediate);
  h.sample('outer', { at: 0, ids: [], progress: [], status: 'idle' });
  await observer.waitForGraph('Expected graph');
  const observation = observer.observe();
  const rejected = assert.rejects(observation, /never displayed CURRENT/);
  h.drain(20000);
  await rejected;
  await observer.close();
});

test('the installed observer continues through a slow backlog and retains replay after its drain', async () => {
  const h = observerHarness();
  const observer = await h.connect();
  await new Promise(setImmediate);
  h.sample('outer', { at: 0, ids: [], progress: [], status: 'idle' });
  await observer.waitForGraph('Expected graph');
  let finished = false;
  const observation = observer.observe().then(samples => { finished = true; return samples; });
  h.sample('inner', { at: 1, ids: ['first'], progress: ['first'], status: 'running' });
  await new Promise(setImmediate);
  h.drain();
  await new Promise(setImmediate);
  assert.equal(finished, false, 'the minimum window is not an upper bound on paced playback');
  h.sample('inner', { at: 12000, ids: ['results'], progress: ['results'], status: 'completed' });
  h.sample('inner', { at: 12500, ids: [], progress: [], status: 'completed' });
  await new Promise(setImmediate);
  h.sample('inner', { at: 12600, ids: ['replay'], progress: ['replay'], status: 'completed' });
  h.drain(1000);
  const samples = await observation;
  assert.deepEqual(Array.from(samples, sample => sample.ids[0]), ['first', 'results', undefined, 'replay']);
  await observer.close();
});

test('the installed observer fails explicitly if CURRENT never drains', async () => {
  const h = observerHarness();
  const observer = await h.connect();
  await new Promise(setImmediate);
  h.sample('outer', { at: 0, ids: [], progress: [], status: 'idle' });
  await observer.waitForGraph('Expected graph');
  const observation = observer.observe();
  const rejected = assert.rejects(observation, /playback did not drain/);
  h.sample('inner', { at: 1, ids: ['first'], progress: ['first'], status: 'running' });
  await new Promise(setImmediate);
  h.drain();
  await new Promise(setImmediate);
  h.drain(20000);
  await rejected;
  await observer.close();
});
