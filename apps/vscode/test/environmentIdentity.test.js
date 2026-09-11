'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const { environmentValue, runtimeEnvironment } = require('../out/environmentIdentity');

test('reads only the canonical YAWR environment value', () => {
  assert.equal(environmentValue({ YAWR_E2E_BINARY: 'runtime' }, 'E2E_BINARY'), 'runtime');
  assert.equal(environmentValue({}, 'E2E_BINARY'), undefined);
});

test('generated integration environment contains only the canonical name', () => {
  assert.deepEqual(runtimeEnvironment('VSCODE_BRIDGE_URL', 'loopback'),
    { YAWR_VSCODE_BRIDGE_URL: 'loopback' });
});
