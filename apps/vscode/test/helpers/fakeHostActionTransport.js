'use strict';

// FakeHostActionTransport — test double for HostActionTransport.
// Export this from test helpers; never use the production webviewPanelTransport in unit tests.

/**
 * Creates a FakeHostActionTransport that captures all acks and cancels emitted
 * by the bridge. Useful for unit tests that assert on bridge output.
 *
 * Returns: { transport, acks, cancels, nextAck(), nextCancel() }
 *   - acks:     array of all ack envelopes received so far (cumulative)
 *   - cancels:  array of all cancel envelopes received so far (cumulative)
 *   - nextAck(): Promise that resolves with the next sequential ack
 *   - nextCancel(): Promise that resolves with the next sequential cancel
 */
function createFakeTransport() {
  const acks = [];
  const cancels = [];
  let ackWaiters = [];
  let cancelWaiters = [];

  const transport = {
    sendAck(ack) {
      acks.push(ack);
      if (ackWaiters.length > 0) ackWaiters.shift()(ack);
    },
    sendCancel(cancel) {
      cancels.push(cancel);
      if (cancelWaiters.length > 0) cancelWaiters.shift()(cancel);
    },
  };

  let ackCursor = 0;
  /** Resolves with the next ack in arrival order, waiting if needed. */
  function nextAck() {
    if (ackCursor < acks.length) return Promise.resolve(acks[ackCursor++]);
    return new Promise((resolve) => {
      ackWaiters.push((a) => { ackCursor++; resolve(a); });
    });
  }

  let cancelCursor = 0;
  /** Resolves with the next cancel in arrival order, waiting if needed. */
  function nextCancel() {
    if (cancelCursor < cancels.length) return Promise.resolve(cancels[cancelCursor++]);
    return new Promise((resolve) => {
      cancelWaiters.push((c) => { cancelCursor++; resolve(c); });
    });
  }

  return { transport, acks, cancels, nextAck, nextCancel };
}

module.exports = { createFakeTransport };
