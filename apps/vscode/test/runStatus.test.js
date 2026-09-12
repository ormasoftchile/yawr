'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const { isIssueStepStatus, isSettledStepStatus, isTerminalRunStatus } = require('../out/runStatus');

test('terminal run statuses match the core engine contract', () => {
  for (const status of ['completed', 'failed', 'cancelled', 'indeterminate', 'denied', 'blocked']) {
    assert.equal(isTerminalRunStatus(status), true, status);
  }
  for (const status of ['pending', 'running', 'waiting', 'starting', 'idle', '']) {
    assert.equal(isTerminalRunStatus(status), false, status);
  }
});

test('step outcome classification includes denied and indeterminate states', () => {
  for (const status of ['completed', 'failed', 'skipped', 'denied', 'indeterminate', 'cancelled', 'blocked']) {
    assert.equal(isSettledStepStatus(status), true, status);
  }
  for (const status of ['failed', 'denied', 'indeterminate', 'cancelled', 'blocked']) {
    assert.equal(isIssueStepStatus(status), true, status);
  }
  for (const status of ['pending', 'running', 'waiting', 'delaying']) {
    assert.equal(isSettledStepStatus(status), false, status);
    assert.equal(isIssueStepStatus(status), false, status);
  }
});