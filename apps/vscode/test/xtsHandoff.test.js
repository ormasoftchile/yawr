'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const { launchXtsWithHandoff } = require('../out/xtsHandoff');

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
  const calls = { dispatches: 0, errors: [], reminders: 0 };
  const dependencies = {
    dispatch: async () => {
      calls.dispatches++;
      if (overrides.dispatchError) throw overrides.dispatchError;
      if (overrides.dispatch) return overrides.dispatch(calls);
      return Object.prototype.hasOwnProperty.call(overrides, 'dispatchResult')
        ? overrides.dispatchResult
        : { status: 'opened' };
    },
    parseAcknowledgment: overrides.parseAcknowledgment ??
      ((value) => value && typeof value.status === 'string' ? value : undefined),
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

test('confirmed XTS handoff dispatches and returns the canonical acknowledgment', async () => {
  const { calls, result } = await launch();
  assert.equal(calls.dispatches, 1);
  assert.equal(calls.reminders, 1);
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

test('non-opened and failed launches do not show a reminder', async () => {
  const nonOpened = await launch({ dispatchResult: { status: 'view-not-found' } });
  assert.equal(nonOpened.calls.reminders, 0);
  assert.deepEqual(nonOpened.result, { status: 'completed', result: { status: 'view-not-found' } });

  const failed = await launch({ dispatchError: new Error('boom') });
  assert.equal(failed.calls.reminders, 0);
  assert.equal(failed.calls.errors.length, 1);
  assert.equal(failed.result.status, 'failed');
});

test('missing or invalid acknowledgments fail closed', async () => {
  const missing = await launch({ dispatchResult: null });
  assert.equal(missing.result.status, 'failed');
  assert.equal(missing.calls.errors.length, 1);

  const invalid = await launch({
    dispatchResult: { status: 'not-allowlisted' },
    parseAcknowledgment: () => undefined,
  });
  assert.equal(invalid.result.status, 'failed');
  assert.equal(invalid.calls.errors.length, 1);
});
