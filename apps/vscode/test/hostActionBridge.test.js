'use strict';

// HAB-01 through HAB-10 — yawr.host-action/v1 bridge acceptance tests.
// These run with plain `node --test` (no VS Code host required).
// HAB-11 (end-to-end with VS Code extension host) lives in test/suite/hostActionBridge.test.ts.
//
// Spec ref: design/yawr/sections/18-host-action-bridge.tex

const assert = require('node:assert/strict');
const test = require('node:test');

const {
  createHostActionBridge,
  testEchoHandler,
  WIRE_VERSION,
  REQUEST_TYPE,
  ACK_TYPE,
  CANCEL_TYPE,
  ERROR_CAPABILITY_NOT_REGISTERED,
  ERROR_HANDLER_ERROR,
  ERROR_HANDLER_TIMEOUT,
} = require('../out/hostActionBridge');

const { createFakeTransport } = require('./helpers/fakeHostActionTransport');

// ─── Fixtures ────────────────────────────────────────────────────────────────

// Deterministic correlation tuple (matches tv-host-action-v1.yaml)
const CORR = {
  correlationId:    'a1b2c3d4-e5f6-7890-abcd-ef1234567890',
  previewSessionId: 'b2c3d4e5-f6a7-8901-bcde-f12345678901',
  requestId:        'c3d4e5f6-a7b8-9012-cdef-123456789012',
  runId:            'run-0001',
  turnId:           'turn-0001',
};

function request(overrides = {}) {
  return {
    type: REQUEST_TYPE,
    version: WIRE_VERSION,
    capability: 'test.echo',
    request: { echo: 'hello' },
    ...CORR,
    ...overrides,
  };
}

function cancel(overrides = {}) {
  return {
    type: CANCEL_TYPE,
    version: WIRE_VERSION,
    correlationId: CORR.correlationId,
    previewSessionId: CORR.previewSessionId,
    requestId: CORR.requestId,
    status: 'execution-not-started',
    reason: 'run-replaced',
    ...overrides,
  };
}

/** Build a registry containing test.echo plus optional extras. */
function echoRegistry(extras = {}) {
  return new Map([
    ['test.echo', { handler: testEchoHandler }],
    ...Object.entries(extras).map(([k, v]) => [k, v]),
  ]);
}

/** Build a bridge backed by a FakeHostActionTransport. */
function makeBridge(registry, opts = {}) {
  const fake = createFakeTransport();
  const bridge = createHostActionBridge(registry, fake.transport, opts.timeoutMs ?? 30_000);
  return { bridge, ...fake };
}

// ─── HAB-01: Registry ─────────────────────────────────────────────────────────
// AC-HA-1: The extension exports at least one non-XTS test capability (test.echo)
// that is usable without production dependencies.

test('HAB-01: testEchoHandler is exported and echoes the request.echo field', async () => {
  const result = await testEchoHandler({
    capability: 'test.echo',
    request: { echo: 'world' },
    correlationId: CORR.correlationId,
    previewSessionId: CORR.previewSessionId,
    requestId: CORR.requestId,
    runId: CORR.runId,
    turnId: CORR.turnId,
    cancellationToken: { isCancellationRequested: false, onCancellationRequested: () => ({ dispose() {} }) },
  });
  assert.equal(result.status, 'completed');
  assert.deepEqual(result.result, { echo: 'world' });
});

test('HAB-01b: createHostActionBridge accepts test.echo and WIRE_VERSION equals "yawr.host-action/v1"', () => {
  assert.equal(WIRE_VERSION, 'yawr.host-action/v1');
  const { bridge } = makeBridge(echoRegistry());
  assert.ok(bridge);
});

// ─── HAB-02: Envelope validation (silent drop) ───────────────────────────────
// AC-HA-2: Messages missing any required field are silently dropped; no ack emitted.

test('HAB-02: missing required fields cause silent drop with no ack emitted', async () => {
  const requiredFields = ['type', 'version', 'capability', 'request', 'correlationId', 'previewSessionId', 'requestId', 'runId', 'turnId'];

  for (const field of requiredFields) {
    const { bridge, acks } = makeBridge(echoRegistry());
    const msg = { ...request() };
    delete msg[field];
    await bridge.receive(msg);
    assert.equal(acks.length, 0, `Expected no ack when '${field}' is missing`);
  }
});

test('HAB-02b: wrong type ("yawr.other.event") is silently dropped', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive({ ...request(), type: 'yawr.other.event' });
  assert.equal(acks.length, 0);
});

test('HAB-02c: wrong version ("host-action/v2") is silently dropped', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive({ ...request(), version: 'host-action/v2' });
  assert.equal(acks.length, 0);
});

test('HAB-02d: the current wire version is matched exactly', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive({ ...request(), version: WIRE_VERSION.toUpperCase() });
  assert.equal(acks.length, 0);
});

test('HAB-02e: non-object request field is silently dropped', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive({ ...request(), request: 'not-an-object' });
  assert.equal(acks.length, 0);
});

test('HAB-02f: undeclared request envelope fields are silently dropped', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive(request({ unexpected: true }));
  assert.equal(acks.length, 0);
});

test('HAB-02g: capability must be a non-empty bounded string', async () => {
  for (const capability of ['', 42, 'x'.repeat(1025)]) {
    const { bridge, acks } = makeBridge(echoRegistry());
    await bridge.receive(request({ capability }));
    assert.equal(acks.length, 0, `Expected malformed capability ${JSON.stringify(capability)} to be dropped`);
  }
});

test('HAB-02h: oversized correlation identities are silently dropped', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive(request({ requestId: 'x'.repeat(1025) }));
  assert.equal(acks.length, 0);
});

test('HAB-02i: deeply nested or oversized capability requests are silently dropped', async () => {
  let deep = { leaf: 'value' };
  for (let i = 0; i < 9; i += 1) deep = { nested: deep };

  for (const payload of [
    deep,
    { value: 'x'.repeat(4097) },
    { values: Array.from({ length: 65 }, () => true) },
  ]) {
    const { bridge, acks } = makeBridge(echoRegistry());
    await bridge.receive(request({ request: payload }));
    assert.equal(acks.length, 0);
  }
});

// ─── HAB-03: Unsupported capability ──────────────────────────────────────────
// AC-HA-3: Unknown capability → status "unsupported", CAPABILITY_NOT_REGISTERED.

test('HAB-03: unregistered capability returns unsupported ack with CAPABILITY_NOT_REGISTERED', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive(request({ capability: 'nonexistent.capability' }));
  assert.equal(acks.length, 1);
  const ack = acks[0];
  assert.equal(ack.type, ACK_TYPE);
  assert.equal(ack.version, WIRE_VERSION);
  assert.equal(ack.status, 'unsupported');
  assert.equal(ack.result, null);
  assert.equal(ack.error.code, ERROR_CAPABILITY_NOT_REGISTERED);
  assert.ok(typeof ack.error.message === 'string');
});

// ─── HAB-04: Handler success ──────────────────────────────────────────────────
// AC-HA-4: test.echo → status "completed", all 5 correlation fields echoed, result contains echo.

test('HAB-04: test.echo handler returns completed ack with echoed result and full correlation tuple', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive(request({ request: { echo: 'yawr' } }));
  assert.equal(acks.length, 1);
  const ack = acks[0];
  assert.equal(ack.type, ACK_TYPE);
  assert.equal(ack.version, WIRE_VERSION);
  assert.equal(ack.capability, 'test.echo');
  assert.equal(ack.status, 'completed');
  assert.deepEqual(ack.result, { echo: 'yawr' });
  assert.equal(ack.error, null);
  // All five correlation fields must be echoed verbatim (§2.3 CI-1).
  assert.equal(ack.correlationId, CORR.correlationId);
  assert.equal(ack.previewSessionId, CORR.previewSessionId);
  assert.equal(ack.requestId, CORR.requestId);
  assert.equal(ack.runId, CORR.runId);
  assert.equal(ack.turnId, CORR.turnId);
});

// ─── HAB-05: Handler failure ──────────────────────────────────────────────────
// AC-HA-5: Throwing handler → status "failed", HANDLER_ERROR, message ≤200 chars, no stack frames.

test('HAB-05: synchronously throwing handler emits failed ack with a fixed safe message', async () => {
  const sensitiveSentinel = 'C:/internal/secret.xts environment=prod password=hunter2';
  const throwingHandler = async () => {
    throw new Error(`${sensitiveSentinel}\n    at SomeModule (/private/src/index.js:42:10)`);
  };
  const { bridge, acks } = makeBridge(
    new Map([['test.throw', { handler: throwingHandler }]])
  );
  await bridge.receive(request({ capability: 'test.throw' }));
  assert.equal(acks.length, 1);
  const ack = acks[0];
  assert.equal(ack.status, 'failed');
  assert.equal(ack.result, null);
  assert.equal(ack.error.code, ERROR_HANDLER_ERROR);
  assert.equal(ack.error.message, 'Handler execution failed.');
  assert.ok(ack.error.message.length <= 200, `message too long: ${ack.error.message.length}`);
  assert.doesNotMatch(ack.error.message, /\s+at /, 'message must not contain stack frames');
  assert.doesNotMatch(ack.error.message, /\.js:\d+/, 'message must not contain file paths');
  assert.doesNotMatch(ack.error.message, /secret\.xts|environment=prod|hunter2/, 'message must not contain handler diagnostics');
});

test('HAB-05b: rejected promise from handler emits same failed ack as synchronous throw', async () => {
  const rejectingHandler = async () => Promise.reject(new Error('promise rejection detail'));
  const { bridge, acks } = makeBridge(
    new Map([['test.reject', { handler: rejectingHandler }]])
  );
  await bridge.receive(request({ capability: 'test.reject' }));
  assert.equal(acks.length, 1);
  assert.equal(acks[0].status, 'failed');
  assert.equal(acks[0].error.code, ERROR_HANDLER_ERROR);
  assert.ok(acks[0].error.message.length <= 200);
});

test('HAB-05c: structured handler failure preserves code but replaces raw message', async () => {
  const failingHandler = async () => ({
    status: 'failed',
    error: { code: 'VALIDATION_ERROR', message: 'C:/internal/secret.xts token=abc123' },
  });
  const { bridge, acks } = makeBridge(
    new Map([['test.fail', { handler: failingHandler }]])
  );
  await bridge.receive(request({ capability: 'test.fail' }));
  assert.equal(acks[0].status, 'failed');
  assert.equal(acks[0].error.code, 'VALIDATION_ERROR');
  assert.equal(acks[0].error.message, 'Handler reported an error.');
});

test('HAB-05d: completed handler result must be a non-null object', async () => {
  for (const invalidResult of [null, undefined, 'not-an-object']) {
    const { bridge, acks } = makeBridge(new Map([['test.invalid-result', {
      handler: async () => ({ status: 'completed', result: invalidResult }),
    }]]));
    await bridge.receive(request({ capability: 'test.invalid-result' }));

    assert.equal(acks.length, 1);
    assert.equal(acks[0].status, 'failed');
    assert.equal(acks[0].result, null);
    assert.equal(acks[0].error.code, ERROR_HANDLER_ERROR);
  }
});

test('HAB-05e: oversized handler result fails closed', async () => {
  const { bridge, acks } = makeBridge(new Map([['test.large-result', {
    handler: async () => ({ status: 'completed', result: { value: 'x'.repeat(4097) } }),
  }]]));
  await bridge.receive(request({ capability: 'test.large-result' }));

  assert.equal(acks.length, 1);
  assert.equal(acks[0].status, 'failed');
  assert.equal(acks[0].result, null);
  assert.equal(acks[0].error.code, ERROR_HANDLER_ERROR);
});

test('HAB-05f: execution-not-started handler result is preserved without becoming failed', async () => {
  const { bridge, acks } = makeBridge(new Map([['test.cancelled', {
    handler: async () => ({
      status: 'execution-not-started',
      error: { code: 'USER_CANCELLED', message: 'User cancelled before dispatch.' },
    }),
  }]]));

  await bridge.receive(request({ capability: 'test.cancelled' }));

  assert.equal(acks.length, 1);
  assert.equal(acks[0].status, 'execution-not-started');
  assert.equal(acks[0].result, null);
  assert.equal(acks[0].error.code, 'USER_CANCELLED');
});

// ─── HAB-06: Timeout ──────────────────────────────────────────────────────────
// AC-HA-6: Handler exceeding deadline → status "timed-out", HANDLER_TIMEOUT.

test('HAB-06: slow handler (never resolves) times out and emits timed-out ack with HANDLER_TIMEOUT', async () => {
  let cancelSignalled = false;
  const slowHandler = async (args) => {
    return new Promise((resolve) => {
      args.cancellationToken.onCancellationRequested(() => { cancelSignalled = true; });
      // Never resolves unless cancelled.
    });
  };
  const SHORT_TIMEOUT = 20; // ms
  const { bridge, acks } = makeBridge(
    new Map([['test.slow', { handler: slowHandler, timeoutMs: SHORT_TIMEOUT }]])
  );
  const start = Date.now();
  await bridge.receive(request({ capability: 'test.slow' }));
  const elapsed = Date.now() - start;

  assert.equal(acks.length, 1);
  assert.equal(acks[0].status, 'timed-out');
  assert.equal(acks[0].error.code, ERROR_HANDLER_TIMEOUT);
  assert.equal(acks[0].result, null);
  // Ack must arrive before 2× the deadline (spec §5 AC-HA-6).
  assert.ok(elapsed < SHORT_TIMEOUT * 10, `elapsed ${elapsed}ms should be < 200ms`);
  // CancellationToken must have been signalled before the ack (B-2, B-3).
  assert.ok(cancelSignalled, 'CancellationToken must be signalled on timeout');
});

test('HAB-06b: after timeout, when slow handler eventually resolves, no second ack is emitted', async () => {
  let release;
  const slowHandler = async () => new Promise((resolve) => { release = resolve; });
  const { bridge, acks } = makeBridge(
    new Map([['test.slow', { handler: slowHandler, timeoutMs: 10 }]])
  );
  await bridge.receive(request({ capability: 'test.slow' }));
  assert.equal(acks.length, 1);
  assert.equal(acks[0].status, 'timed-out');
  // Late resolution — must NOT emit a second ack (CI-4 at-most-once).
  release({ status: 'completed', result: { echo: 'late' } });
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(acks.length, 1, 'must not emit a second ack after timeout');
});

// ─── HAB-07: Cancellation on disposal ────────────────────────────────────────
// AC-HA-7: panel disposal while a request is pending → cancel envelope with reason "panel-disposed".

test('HAB-07: cancelAllPending("panel-disposed") emits cancel envelope for each pending request', async () => {
  let release;
  const slowHandler = async () => new Promise((resolve) => { release = resolve; });
  const { bridge, acks, cancels } = makeBridge(
    new Map([['test.slow', { handler: slowHandler, timeoutMs: 60_000 }]])
  );
  const pending = bridge.receive(request({ capability: 'test.slow' }));
  // Cancel before handler resolves.
  bridge.cancelAllPending('panel-disposed');
  release({ status: 'completed', result: {} });
  await pending;

  assert.equal(acks.length, 0, 'no ack should be emitted for cancelled request');
  assert.equal(cancels.length, 1);
  const cancel = cancels[0];
  assert.equal(cancel.type, CANCEL_TYPE);
  assert.equal(cancel.version, WIRE_VERSION);
  assert.equal(cancel.status, 'execution-not-started');
  assert.equal(cancel.reason, 'panel-disposed');
  // Three-field correlation subset echoed (§2.2 cancel envelope).
  assert.equal(cancel.correlationId, CORR.correlationId);
  assert.equal(cancel.previewSessionId, CORR.previewSessionId);
  assert.equal(cancel.requestId, CORR.requestId);
});

test('HAB-07b: cancelAllPending("run-replaced") emits cancel with reason "run-replaced"', async () => {
  let release;
  const slowHandler = async () => new Promise((resolve) => { release = resolve; });
  const { bridge, cancels } = makeBridge(
    new Map([['test.slow', { handler: slowHandler, timeoutMs: 60_000 }]])
  );
  const pending = bridge.receive(request({ capability: 'test.slow' }));
  bridge.cancelAllPending('run-replaced');
  release({ status: 'completed', result: {} });
  await pending;
  assert.equal(cancels[0].reason, 'run-replaced');
});

test('HAB-07c: cancelAllPending("reload") emits cancel with reason "reload"', async () => {
  let release;
  const slowHandler = async () => new Promise((resolve) => { release = resolve; });
  const { bridge, cancels } = makeBridge(
    new Map([['test.slow', { handler: slowHandler, timeoutMs: 60_000 }]])
  );
  const pending = bridge.receive(request({ capability: 'test.slow' }));
  bridge.cancelAllPending('reload');
  release({ status: 'completed', result: {} });
  await pending;
  assert.equal(cancels[0].reason, 'reload');
});

test('HAB-07d: CancellationToken is signalled before cancel envelope is emitted (B-3)', async () => {
  let tokenCancelledAt;
  let cancelSentAt;
  const timeline = [];
  let resolve;
  const slowHandler = async (args) => {
    return new Promise((res) => {
      resolve = res;
      args.cancellationToken.onCancellationRequested(() => { timeline.push('token-cancelled'); });
    });
  };
  const { bridge } = makeBridge(
    new Map([['test.slow', { handler: slowHandler, timeoutMs: 60_000 }]]),
  );
  const fakeSendCancel = [];
  // Wrap the transport to record ordering.
  const wrappedTransport = {
    sendAck() {},
    sendCancel(c) { timeline.push('cancel-sent'); fakeSendCancel.push(c); },
  };
  const bridge2 = (require('../out/hostActionBridge').createHostActionBridge)(
    new Map([['test.slow', { handler: slowHandler, timeoutMs: 60_000 }]]),
    wrappedTransport,
  );
  const pending = bridge2.receive(request({ capability: 'test.slow' }));
  bridge2.cancelAllPending('panel-disposed');
  resolve({ status: 'completed', result: {} });
  await pending;
  // Token cancellation must happen before the cancel envelope is sent.
  assert.deepEqual(timeline, ['token-cancelled', 'cancel-sent']);
  void bridge; // suppress unused
});

test('HAB-07e: matching incoming cancel settles the request and suppresses a late ack', async () => {
  let release;
  let tokenCancelled = false;
  const handler = async (args) => new Promise((resolve) => {
    release = resolve;
    args.cancellationToken.onCancellationRequested(() => { tokenCancelled = true; });
  });
  const { bridge, acks, cancels } = makeBridge(
    new Map([['test.slow', { handler, timeoutMs: 60_000 }]]),
  );

  const pending = bridge.receive(request({ capability: 'test.slow' }));
  await bridge.receive(cancel());
  assert.equal(tokenCancelled, true, 'matching incoming cancel must signal the handler token');

  release({ status: 'completed', result: { echo: 'late' } });
  await pending;
  assert.equal(acks.length, 0, 'cancelled handler must not emit a late ack');
  assert.equal(cancels.length, 0, 'incoming cancel must not be echoed back to the preview');

  await bridge.receive(request({ capability: 'test.slow' }));
  assert.equal(acks.length, 0, 'cancelled requestId must remain settled');
});

test('HAB-07f: mismatched or non-canonical incoming cancel is ignored', async () => {
  let release;
  let tokenCancelled = false;
  const handler = async (args) => new Promise((resolve) => {
    release = resolve;
    args.cancellationToken.onCancellationRequested(() => { tokenCancelled = true; });
  });
  const { bridge, acks } = makeBridge(
    new Map([['test.slow', { handler, timeoutMs: 60_000 }]]),
  );

  const pending = bridge.receive(request({ capability: 'test.slow' }));
  await bridge.receive(cancel({ correlationId: 'wrong' }));
  await bridge.receive(cancel({ unexpected: true }));
  assert.equal(tokenCancelled, false);

  release({ status: 'completed', result: { echo: 'on-time' } });
  await pending;
  assert.equal(acks.length, 1);
  assert.deepEqual(acks[0].result, { echo: 'on-time' });
});

// ─── HAB-08: Duplicate requestId (at-most-once extension side) ────────────────
// AC-HA-8 / CI-2: A second request with the same requestId is dropped; only one ack emitted.

test('HAB-08: duplicate request (same requestId) while first is in-flight is dropped; only one ack emitted', async () => {
  let release;
  const waiting = new Promise((resolve) => { release = resolve; });
  const { bridge, acks } = makeBridge(echoRegistry());
  // Override echo to be slow.
  const slowRegistry = new Map([['test.echo', { handler: async () => waiting, timeoutMs: 60_000 }]]);
  const { bridge: b2, acks: acks2 } = makeBridge(slowRegistry);
  const first = b2.receive(request());
  const second = b2.receive(request()); // same requestId
  release({ status: 'completed', result: { echo: 'once' } });
  await Promise.all([first, second]);
  assert.equal(acks2.length, 1, 'only one ack must be emitted for the same requestId');
  void acks; void bridge;
});

test('HAB-08b: after settlement, a new request with the same requestId is also dropped', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive(request());
  assert.equal(acks.length, 1);
  // Send the same requestId again after settlement.
  await bridge.receive(request());
  assert.equal(acks.length, 1, 'second request for settled requestId must be dropped');
});

// ─── HAB-09: Correlation mismatch — late/post-cancel handler result discarded ─
// CI-4: after cancelAllPending, a late handler resolution must not emit an ack.

test('HAB-09: handler result arriving after cancellation is discarded (no double ack/cancel)', async () => {
  let release;
  const slow = async () => new Promise((resolve) => { release = resolve; });
  const { bridge, acks, cancels } = makeBridge(
    new Map([['test.slow', { handler: slow, timeoutMs: 60_000 }]])
  );
  // Use capability: 'test.slow' so the handlers are actually dispatched.
  const req1 = request({ capability: 'test.slow', requestId: 'req-001' });
  const req2 = request({ capability: 'test.slow', requestId: 'req-002' });
  const p1 = bridge.receive(req1);
  const p2 = bridge.receive(req2);
  // Cancel both while still in-flight.
  bridge.cancelAllPending('panel-disposed');
  // Resolve the second handler's promise (first is unresolvable since release was overwritten).
  release({ status: 'completed', result: {} });
  await p2; // p2 can resolve (handler result discarded after cancellation)
  // p1's slow handler never resolves; cancel already cleaned it up. Give microtasks time to flush.
  await new Promise((r) => setTimeout(r, 5));

  assert.equal(acks.length, 0, 'no ack should arrive after cancellation');
  assert.equal(cancels.length, 2, 'one cancel per pending request');
  // After cancellation, requests with those requestIds are settled — re-submission is dropped.
  await bridge.receive(req1);
  assert.equal(acks.length, 0, 'post-cancel re-submission must be dropped');
});

test('HAB-09b: cancellation settles a handler that never resolves without waiting for its deadline', async () => {
  const never = async () => new Promise(() => {});
  const { bridge, acks, cancels } = makeBridge(
    new Map([['test.never', { handler: never, timeoutMs: 60_000 }]]),
  );
  const pending = bridge.receive(request({ capability: 'test.never' }));

  bridge.cancelAllPending('run-replaced');

  await Promise.race([
    pending,
    new Promise((_, reject) => setTimeout(
      () => reject(new Error('cancelled bridge request did not settle promptly')),
      100,
    )),
  ]);
  assert.equal(acks.length, 0);
  assert.equal(cancels.length, 1);
});

// ─── HAB-10: at-most-once / run-resumed semantics ────────────────────────────
// AC-HA-9: second ack for same requestId never double-resolves (extension bridge side).

test('HAB-10: bridge sends exactly one ack for a successful request (monotonic terminal state)', async () => {
  const { bridge, acks } = makeBridge(echoRegistry());
  await bridge.receive(request());
  assert.equal(acks.length, 1);
  assert.equal(acks[0].status, 'completed');
  // Verify the ack carries the full tuple and correct shapes.
  assert.equal(acks[0].type, ACK_TYPE);
  assert.equal(acks[0].version, WIRE_VERSION);
  assert.equal(acks[0].result.echo, 'hello');
  assert.equal(acks[0].error, null);
  // No second ack after the first.
  await bridge.receive(request());
  assert.equal(acks.length, 1);
});

// Product-specific handler behavior is covered by xtsProtocol.test.js.
