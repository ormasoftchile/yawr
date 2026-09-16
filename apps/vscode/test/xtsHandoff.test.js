'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const { launchXtsWithHandoff, xtsParameterArguments } = require('../out/xtsHandoff');

function createCancellation() {
  let cancelled = false;
  const listeners = new Set();
  return {
    token: {
      get isCancellationRequested() { return cancelled; },
      onCancellationRequested(listener) {
        if (cancelled) {
          listener();
          return { dispose() {} };
        }
        listeners.add(listener);
        return { dispose() { listeners.delete(listener); } };
      },
    },
    cancel() {
      if (cancelled) return;
      cancelled = true;
      for (const listener of [...listeners]) listener();
      listeners.clear();
    },
  };
}

function createHarness(overrides = {}) {
  const calls = { dispatches: 0, verifications: 0, errors: [], reminders: 0 };
  const dependencies = {
    dispatch: async () => {
      calls.dispatches++;
      if (overrides.dispatchError) throw overrides.dispatchError;
      if (overrides.dispatch) return overrides.dispatch(calls);
      return Object.prototype.hasOwnProperty.call(overrides, 'dispatchResult')
        ? overrides.dispatchResult
        : undefined;
    },
    verifyView: async () => {
      calls.verifications++;
      return overrides.verifyView ? overrides.verifyView() : 'opened';
    },
    showDispatchError: (message) => { calls.errors.push(message); },
    showReminder: () => { calls.reminders++; },
  };
  return { calls, dependencies };
}

async function launch(overrides = {}) {
  const cancellation = overrides.cancellation ?? createCancellation();
  const harness = createHarness(overrides);
  const result = await launchXtsWithHandoff(
    overrides.focus ?? true,
    cancellation.token,
    harness.dependencies,
  );
  return { ...harness, cancellation, result };
}

test('void XTS command completes only after operator verification of the real view', async () => {
  const { calls, result } = await launch();
  assert.equal(calls.dispatches, 1);
  assert.equal(calls.reminders, 1);
  assert.equal(calls.verifications, 1);
  assert.deepEqual(result, { status: 'completed', result: { status: 'opened' } });
});

test('cancellation before dispatch returns execution-not-started', async () => {
  const cancellation = createCancellation();
  cancellation.cancel();
  const { calls, result } = await launch({ cancellation });
  assert.equal(result.status, 'execution-not-started');
  assert.equal(calls.dispatches, 0);
});

test('cancellation after dispatch suppresses acknowledgment and reminder', async () => {
  const cancellation = createCancellation();
  let dispatchStarted;
  const started = new Promise((resolve) => { dispatchStarted = resolve; });
  let resolveDispatch;
  const dispatched = new Promise((resolve) => { resolveDispatch = resolve; });
  const launched = launch({
    cancellation,
    dispatch: () => {
      dispatchStarted();
      return dispatched;
    },
  });

  await started;
  cancellation.cancel();
  resolveDispatch({ status: 'opened' });
  const { calls, result } = await launched;

  assert.equal(calls.dispatches, 1);
  assert.equal(calls.reminders, 0);
  assert.equal(result.status, 'execution-not-started');
});

test('dispatch rejection after cancellation suppresses error UI and returns canonical cancellation', async () => {
  const cancellation = createCancellation();
  let dispatchStarted;
  const started = new Promise((resolve) => { dispatchStarted = resolve; });
  let rejectDispatch;
  const dispatched = new Promise((_resolve, reject) => { rejectDispatch = reject; });
  const launched = launch({
    cancellation,
    dispatch: () => {
      dispatchStarted();
      return dispatched;
    },
  });

  await started;
  cancellation.cancel();
  rejectDispatch(new Error('dispatch failed after cancellation'));
  const { calls, result } = await launched;

  assert.equal(calls.dispatches, 1);
  assert.equal(calls.errors.length, 0);
  assert.equal(calls.reminders, 0);
  assert.deepEqual(result, {
    status: 'execution-not-started',
    error: { code: 'USER_CANCELLED', message: 'XTS handoff was cancelled.' },
  });
});

test('focus=false still dispatches only after the handler-authorized handoff and skips the reminder', async () => {
  const { calls, result } = await launch({ focus: false });
  assert.equal(calls.dispatches, 1);
  assert.equal(calls.reminders, 0);
  assert.equal(result.status, 'completed');
});

test('operator-reported startup failure and command exceptions cannot report opened', async () => {
  const nonOpened = await launch({ verifyView: async () => 'failed' });
  assert.equal(nonOpened.calls.reminders, 0);
  assert.equal(nonOpened.result.status, 'failed');
  assert.equal(nonOpened.result.result, undefined);

  const failed = await launch({ dispatchError: new Error('boom') });
  assert.equal(failed.calls.reminders, 0);
  assert.equal(failed.calls.errors.length, 1);
  assert.equal(failed.result.status, 'failed');
});

test('dispatch return values never replace real-view verification or prematurely acknowledge', async () => {
  for (const dispatchResult of [undefined, null, { status: 'opened' }, { status: 'view-not-found' }]) {
    let verifyStarted, release;
    const started = new Promise(resolve => { verifyStarted = resolve; });
    const verified = new Promise(resolve => { release = resolve; });
    let settled = false;
    const pending = launch({ dispatchResult, verifyView: () => { verifyStarted(); return verified; } })
      .then(value => { settled = true; return value; });
    await started;
    await new Promise(setImmediate);
    assert.equal(settled, false);
    release('opened');
    assert.equal((await pending).result.result.status, 'opened');
  }
});

test('verification errors and cancellation never report opened', async () => {
  const failed = await launch({ verifyView: async () => { throw new Error('view check failed'); } });
  assert.equal(failed.result.status, 'failed');
  const cancelled = await launch({ verifyView: async () => 'cancelled' });
  assert.equal(cancelled.result.status, 'execution-not-started');
  assert.equal(cancelled.calls.reminders, 0);
});

test('XTS CLI parameters preserve environment, server and database exactly', () => {
  assert.equal(xtsParameterArguments('ProdEus1a', { server: 'server-859807057', database: 'database-859807057' }),
    '-p environment:ProdEus1a -p server:server-859807057 -p database:database-859807057');
  // Current XTS parses values up to the next "-p"; it does not unquote shell strings.
  const args = xtsParameterArguments('ProdEus1a', { server: 'host\\instance', database: 'Database With Spaces', count: 3 });
  const parsed = Object.fromEntries([...args.matchAll(/-p\s+(\w+):([^\s]+(?:\s+(?!-p\s)[^\s]*)*)/gi)]
    .map(match => [match[1], match[2].trim()]));
  assert.deepEqual(parsed, { environment: 'ProdEus1a', server: 'host\\instance', database: 'Database With Spaces', count: '3' });
});

test('ambiguous CLI parameters are rejected rather than changing the requested environment or values', () => {
  for (const parameters of [{ environment: 'other' }, { server: 'host -p environment:other' },
    { server: ' host' }, { database: '' }, { 'bad-name': 'value' }, { server: {} }]) {
    assert.throws(() => xtsParameterArguments('ProdEus1a', parameters));
  }
});
