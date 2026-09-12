// HAB-11 — end-to-end yawr.host-action/v1 round-trip inside the VS Code extension host.
//
// This test proves: runtime/preview request → registered test.echo handler →
// structured ack → simulated run-resume, without any XTS dependencies (AC-HA-10).
//
// It runs via `npm run test:e2e` (vscode-test / @vscode/test-electron) inside
// a real VS Code extension host. The bridge module has no vscode import, so
// the test exercises the full bridge logic while remaining XTS-free.

import * as assert from 'assert';
import * as path from 'path';
import * as vscode from 'vscode';

// Resolve the compiled bridge output relative to this test file.
// tsconfig.test.json maps test/suite → out/test/suite, so:
//   out/test/suite/hostActionBridge.test.js → ../../hostActionBridge
const {
  createHostActionBridge,
  testEchoHandler,
  WIRE_VERSION,
  ACK_TYPE,
  CANCEL_TYPE,
  ERROR_CAPABILITY_NOT_REGISTERED,
  ERROR_HANDLER_TIMEOUT,
} = require(path.join(__dirname, '..', '..', 'hostActionBridge'));

// Inline FakeHostActionTransport (no external file dependency in host context).
function createFakeTransport() {
  const acks: unknown[] = [];
  const cancels: unknown[] = [];
  let ackWaiters: Array<(a: unknown) => void> = [];
  let cancelWaiters: Array<(c: unknown) => void> = [];
  let ackCursor = 0;
  let cancelCursor = 0;
  const transport = {
    sendAck(ack: unknown) {
      acks.push(ack);
      if (ackWaiters.length > 0) ackWaiters.shift()!(ack);
    },
    sendCancel(cancel: unknown) {
      cancels.push(cancel);
      if (cancelWaiters.length > 0) cancelWaiters.shift()!(cancel);
    },
  };
  function nextAck(): Promise<unknown> {
    if (ackCursor < acks.length) return Promise.resolve(acks[ackCursor++]);
    return new Promise((r) => { ackWaiters.push((a) => { ackCursor++; r(a); }); });
  }
  function nextCancel(): Promise<unknown> {
    if (cancelCursor < cancels.length) return Promise.resolve(cancels[cancelCursor++]);
    return new Promise((r) => { cancelWaiters.push((c) => { cancelCursor++; r(c); }); });
  }
  return { transport, acks, cancels, nextAck, nextCancel };
}

// Deterministic correlation tuple (matches tv-host-action-v1.yaml).
const CORR = {
  correlationId:    'a1b2c3d4-e5f6-7890-abcd-ef1234567890',
  previewSessionId: 'b2c3d4e5-f6a7-8901-bcde-f12345678901',
  requestId:        'c3d4e5f6-a7b8-9012-cdef-123456789012',
  runId:            'run-0001',
  turnId:           'turn-0001',
};

function request(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    type: 'yawr.host-action.request',
    version: WIRE_VERSION,
    capability: 'test.echo',
    request: { echo: 'hello-e2e' },
    ...CORR,
    ...overrides,
  };
}

suite('HAB-11 — end-to-end yawr.host-action/v1 round-trip (no XTS)', () => {
  test('HAB-11: preview request → test.echo handler → completed ack → run-resume simulated', async () => {
    const { transport, nextAck } = createFakeTransport();
    const registry = new Map([['test.echo', { handler: testEchoHandler }]]);
    const bridge = createHostActionBridge(registry, transport);

    // Simulate: Yawr preview emits a host-action request.
    const receivePromise = bridge.receive(request({ request: { echo: 'run-resume-proof' } }));
    // Wait for the ack (simulates the preview receiving the structured reply).
    const ack = await nextAck() as Record<string, unknown>;
    await receivePromise;

    // The ack must have status: "completed" with the echoed result.
    assert.strictEqual(ack.type, ACK_TYPE, 'ack type must be yawr.host-action.ack');
    assert.strictEqual(ack.version, WIRE_VERSION, 'ack version must be yawr.host-action/v1');
    assert.strictEqual(ack.status, 'completed', 'ack status must be completed');
    assert.deepStrictEqual(ack.result, { echo: 'run-resume-proof' }, 'result must echo the input');
    assert.strictEqual(ack.error, null, 'error must be null on success');

    // All five correlation fields must be echoed verbatim (CI-1).
    assert.strictEqual(ack.correlationId,    CORR.correlationId);
    assert.strictEqual(ack.previewSessionId, CORR.previewSessionId);
    assert.strictEqual(ack.requestId,        CORR.requestId);
    assert.strictEqual(ack.runId,            CORR.runId);
    assert.strictEqual(ack.turnId,           CORR.turnId);
  });

  test('HAB-11b: unsupported capability delivers CAPABILITY_NOT_REGISTERED ack (no XTS needed)', async () => {
    const { transport, nextAck } = createFakeTransport();
    const bridge = createHostActionBridge(new Map([['test.echo', { handler: testEchoHandler }]]), transport);
    bridge.receive(request({ capability: 'no.such.capability' }));
    const ack = await nextAck() as Record<string, unknown>;
    assert.strictEqual(ack.status, 'unsupported');
    assert.strictEqual((ack.error as Record<string, unknown>).code, ERROR_CAPABILITY_NOT_REGISTERED);
  });

  test('HAB-11c: panel disposal while request pending → cancel envelope, no ack', async () => {
    let releaseHandler!: (r: unknown) => void;
    const slowHandler = async () => new Promise<unknown>((r) => { releaseHandler = r; });
    const { transport, acks, cancels } = createFakeTransport();
    const bridge = createHostActionBridge(
      new Map([['test.echo', { handler: () => slowHandler(), timeoutMs: 60_000 }]]),
      transport,
    );
    const pendingReceive = bridge.receive(request());
    bridge.cancelAllPending('panel-disposed');
    releaseHandler({ status: 'completed', result: {} });
    await pendingReceive;

    assert.strictEqual(acks.length, 0, 'disposal must not emit an ack after the cancel envelope');
    assert.strictEqual(cancels.length, 1, 'one cancel per pending request on disposal');
    const cancel = cancels[0] as Record<string, unknown>;
    assert.strictEqual(cancel.type, CANCEL_TYPE);
    assert.strictEqual(cancel.reason, 'panel-disposed');
    assert.strictEqual(cancel.status, 'execution-not-started');
  });

  test('HAB-11d: handler timeout produces timed-out ack with HANDLER_TIMEOUT', async () => {
    const neverHandler = async () => new Promise<never>(() => { /* never resolves */ });
    const { transport, nextAck } = createFakeTransport();
    const bridge = createHostActionBridge(
      new Map([['test.echo', { handler: neverHandler, timeoutMs: 20 }]]),
      transport,
    );
    const pending = bridge.receive(request());
    const ack = await nextAck() as Record<string, unknown>;
    await pending;
    assert.strictEqual(ack.status, 'timed-out');
    assert.strictEqual((ack.error as Record<string, unknown>).code, ERROR_HANDLER_TIMEOUT);
  });

  test('HAB-11e: generic XTS request without view_path is rejected before dispatch', async function () {
    this.timeout(30_000);
    const extension = vscode.extensions.getExtension('ormasoftchile.yawr-preview');
    assert.ok(extension);
    await extension!.activate();
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>('yawr.test.openHostActionPanel');
    assert.ok(panel);

    const ackPromise = new Promise<Record<string, unknown>>((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('missing-server ack timeout')), 15_000);
      const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
        const envelope = message as Record<string, unknown>;
        if (envelope?.type !== 'yawr.test.ackCaptured') return;
        clearTimeout(timeout);
        subscription.dispose();
        resolve(envelope.ack as Record<string, unknown>);
      });
    });

    const delivered = await panel.webview.postMessage({
      type: 'yawr.test.inject',
      payload: request({
        capability: 'xts.open-view',
        request: { environment: 'LocalTestEnvironment', parameters: {}, focus: true },
      }),
    });
    assert.ok(delivered);
    const ack = await ackPromise;
    assert.strictEqual(ack.status, 'failed');
    assert.strictEqual(ack.result, null);
    assert.strictEqual((ack.error as Record<string, unknown>).code, 'INVALID_REQUEST');
    panel.dispose();
  });
});
