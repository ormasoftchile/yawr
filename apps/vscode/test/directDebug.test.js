'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const { parseDirectDebugConfig, validateDirectDebugTargets } = require('../out/directDebug');

test('parseDirectDebugConfig accepts bounded breakpoint targets and watches', () => {
  assert.deepEqual(parseDirectDebugConfig({
    enabled: true,
    breakpoints: [{
      step: 'get-incident',
      phase: 'after',
      callPath: [{ step_id: 'inspect-primary' }],
    }],
    watches: ['incident_status'],
  }), {
    enabled: true,
    breakpoints: [{
      step: 'get-incident',
      phase: 'after',
      callPath: [{ step_id: 'inspect-primary' }],
    }],
    watches: ['incident_status'],
  });
});

test('parseDirectDebugConfig rejects malformed and oversized input', () => {
  for (const value of [
    null,
    { enabled: false, breakpoints: [] },
    { enabled: true, breakpoints: [{ step: '', phase: 'after' }] },
    { enabled: true, breakpoints: [{ step: 'one', phase: 'during' }] },
    { enabled: true, breakpoints: [{ step: 'one', phase: 'before', callPath: [{ step_id: '' }] }] },
    { enabled: true, breakpoints: [], watches: new Array(33).fill('value') },
    { enabled: true, breakpoints: [], watches: [''] },
    { enabled: true, breakpoints: [], extra: true },
  ]) {
    assert.throws(() => parseDirectDebugConfig(value), /debug configuration/i);
  }
});

test('parseDirectDebugConfig returns undefined when debug mode is absent', () => {
  assert.equal(parseDirectDebugConfig(undefined), undefined);
});

test('validateDirectDebugTargets requires an exact graph step and call path', () => {
  const config = parseDirectDebugConfig({
    enabled: true,
    breakpoints: [{ step: 'get-incident', phase: 'after', callPath: [{ step_id: 'inspect-primary' }] }],
  });
  const nodes = [{
    id: 'inspect-primary/get-incident',
    data: { step_id: 'get-incident', call_path: ['inspect-primary'] },
  }];
  assert.doesNotThrow(() => validateDirectDebugTargets(config, nodes));
  assert.throws(
    () => validateDirectDebugTargets(config, [{ id: 'get-incident', data: { step_id: 'get-incident', call_path: [] } }]),
    /does not exist in the loaded graph/i,
  );
});
