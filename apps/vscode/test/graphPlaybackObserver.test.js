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
          ? { result: { objectId: `observer:${sessionId ?? 'root'}` } } : {} }) });
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
    sample(sessionId, sample) {
      if (instrumented.has(sessionId)) socket.event('Runtime.bindingCalled', {
        name: '__yawrReadOnlyGraphPlaybackSample', payload: JSON.stringify(sample),
      }, sessionId);
    },
    drain() {
      for (const [id, timer] of timers) if (timer.delay === 8000) {
        timers.delete(id); timer.callback();
      }
    },
  };
}

test('the installed observer records CURRENT in nested webview targets, not just the prior completed graph', async () => {
  const h = observerHarness();
  const observer = await h.connect();
  await new Promise(setImmediate);
  h.sample('outer', { at: 0, ids: [], progress: [], status: 'completed' });
  await observer.waitForGraph();
  const observation = observer.observe();
  h.sample('inner', { at: 1, ids: ['first'], progress: ['first'], status: 'running' });
  h.sample('inner', { at: 501, ids: ['results'], progress: ['results'], status: 'completed', results: 'available' });
  h.sample('inner', { at: 1001, ids: [], progress: [], status: 'completed', results: 'available' });
  h.drain();
  const samples = await observation;
  await observer.close();
  assert.ok(samples.some(sample => sample.ids.length), 'the entire run must display ordinary current steps');
  assert.deepEqual(Array.from(samples.filter(sample => sample.ids.length), sample => sample.ids[0]), ['first', 'results']);
});
