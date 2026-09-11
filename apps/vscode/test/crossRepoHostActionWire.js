'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const test = require('node:test');

const { createHostActionBridge } = require('../out/hostActionBridge');

const coreRoot = value('CORE_ROOT') || path.resolve(__dirname, '..', '..', '..', 'runtime');
const fixturePath = path.join(
  path.resolve(coreRoot),
  'internal',
  'serve',
  'testdata',
  'host_action_wire_v1.json',
);
if (!fs.existsSync(fixturePath)) {
  throw new Error(`The configured or monorepo Yawr runtime does not contain the canonical host-action fixture: ${fixturePath}`);
}
const wire = JSON.parse(fs.readFileSync(fixturePath, 'utf8'));

function transport() {
  const acks = [];
  const cancels = [];
  return {
    acks,
    cancels,
    value: {
      sendAck: (ack) => acks.push(ack),
      sendCancel: (cancel) => cancels.push(cancel),
    },
  };
}

test('canonical Yawr request produces the exact generic VS Code acknowledgment', async () => {
  const captured = transport();
  let handlerArgs;
  const bridge = createHostActionBridge(new Map([[
    wire.request.capability,
    {
      handler: async (args) => {
        handlerArgs = args;
        return { status: 'completed', result: wire.acknowledgment.result };
      },
    },
  ]]), captured.value);

  await bridge.receive(wire.request);

  assert.deepEqual({
    capability: handlerArgs.capability,
    request: handlerArgs.request,
    correlationId: handlerArgs.correlationId,
    previewSessionId: handlerArgs.previewSessionId,
    requestId: handlerArgs.requestId,
    runId: handlerArgs.runId,
    turnId: handlerArgs.turnId,
  }, {
    capability: wire.request.capability,
    request: wire.request.request,
    correlationId: wire.request.correlationId,
    previewSessionId: wire.request.previewSessionId,
    requestId: wire.request.requestId,
    runId: wire.request.runId,
    turnId: wire.request.turnId,
  });
  assert.deepEqual(captured.acks, [wire.acknowledgment]);
  assert.deepEqual(captured.cancels, []);
});

test('canonical Yawr cancellation stops the matching handler without an acknowledgment', async () => {
  const captured = transport();
  let handlerStarted;
  const started = new Promise((resolve) => { handlerStarted = resolve; });
  let cancellationObserved = false;
  const bridge = createHostActionBridge(new Map([[
    wire.request.capability,
    {
      handler: async (args) => {
        handlerStarted();
        await new Promise((resolve) => args.cancellationToken.onCancellationRequested(resolve));
        cancellationObserved = args.cancellationToken.isCancellationRequested;
        return { status: 'completed', result: wire.acknowledgment.result };
      },
    },
  ]]), captured.value);

  const pending = bridge.receive(wire.request);
  await started;
  await bridge.receive(wire.cancellation);
  await pending;

  assert.equal(cancellationObserved, true);
  assert.deepEqual(captured.acks, []);
  assert.deepEqual(captured.cancels, []);
});