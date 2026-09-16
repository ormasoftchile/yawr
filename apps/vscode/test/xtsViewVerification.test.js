const test = require('node:test');
const assert = require('node:assert/strict');
const { waitForXtsViewVerification } = require('../out/xtsViewVerification');

function harness(post = async () => true) {
  let cancelled = false;
  const messages = new Set(), cancellations = new Set(), sent = [];
  const args = {
    capability: 'xts.open-view', runId: 'run', turnId: 'turn', correlationId: 'correlation',
    previewSessionId: 'preview', requestId: 'request',
    cancellationToken: {
      get isCancellationRequested() { return cancelled; },
      onCancellationRequested(listener) {
        cancellations.add(listener);
        return { dispose: () => cancellations.delete(listener) };
      },
    },
  };
  const transport = {
    postMessage(message) { sent.push(message); return post(); },
    onDidReceiveMessage(listener) {
      messages.add(listener);
      return { dispose: () => messages.delete(listener) };
    },
  };
  return {
    start: () => waitForXtsViewVerification(args, transport), sent,
    emit: value => [...messages].forEach(listener => listener(value)),
    cancel() { cancelled = true; [...cancellations].forEach(listener => listener()); },
    get listeners() { return messages.size + cancellations.size; },
  };
}

test('real-view verification waits for the matching operator result, not request delivery', async () => {
  const h = harness();
  let settled = false;
  const pending = h.start().then(result => { settled = true; return result; });
  await new Promise(setImmediate);
  assert.equal(settled, false);
  assert.equal(h.sent.length, 1);
  assert.equal(h.listeners, 2);
  const verified = { ...h.sent[0], type: 'yawr.xts.view-verified', status: 'opened' };
  for (const key of ['capability', 'runId', 'turnId', 'correlationId', 'previewSessionId', 'requestId']) {
    h.emit({ ...verified, [key]: 'stale' });
  }
  h.emit({ ...verified, unexpected: true });
  h.emit({ ...verified, status: 'dispatched' });
  await new Promise(setImmediate);
  assert.equal(settled, false);
  h.emit(verified);
  assert.equal(await pending, 'opened');
  assert.equal(h.listeners, 0);
});

test('operator-reported XTS startup failure settles negatively and cleans up listeners', async () => {
  const h = harness();
  const pending = h.start();
  await new Promise(setImmediate);
  h.emit({ ...h.sent[0], type: 'yawr.xts.view-verified', status: 'failed' });
  assert.equal(await pending, 'failed');
  assert.equal(h.listeners, 0);
});

test('cancellation before and during view verification cleans up and suppresses late confirmation', async () => {
  for (const before of [true, false]) {
    const h = harness();
    if (before) h.cancel();
    const pending = h.start();
    await new Promise(setImmediate);
    if (!before) h.cancel();
    assert.equal(await pending, 'cancelled');
    assert.equal(h.listeners, 0);
    if (before) assert.equal(h.sent.length, 0);
    else h.emit({ ...h.sent[0], type: 'yawr.xts.view-verified', status: 'opened' });
  }
});

test('failed or throwing readiness transport cannot leave a pending success-shaped handoff', async () => {
  for (const post of [async () => false, () => { throw new Error('disposed'); }, async () => { throw new Error('disconnected'); }]) {
    const h = harness(post);
    await assert.rejects(h.start());
    assert.equal(h.listeners, 0);
  }
});
