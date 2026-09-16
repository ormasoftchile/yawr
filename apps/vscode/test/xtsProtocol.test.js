'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const { WIRE_VERSION } = require('../out/hostActionBridge');

function compiledExtension() {
  return fs.readFileSync(path.join(__dirname, '..', 'out', 'extension.js'), 'utf8');
}

function genericXtsHandler(source) {
  const start = source.indexOf('function makeXtsOpenViewHandler(');
  const end = source.indexOf('async function previewGraph(', start);
  assert.ok(start >= 0 && end > start, 'compiled generic XTS handler must be present');
  return source.slice(start, end);
}

test('XTS uses the canonical yawr.host-action/v1 wire version', () => {
  assert.equal(WIRE_VERSION, 'yawr.host-action/v1');
});

test('XTS bridge exposes one generic open-view capability with no product-owned view', () => {
  const source = compiledExtension();
  assert.match(source, /xts\.open-view/);
  assert.doesNotMatch(source, /xts\.open-(?!view\b)[\w.-]+/);
  assert.equal((source.match(/\[['"]xts\.open-view['"],\s*\{\s*handler:/g) ?? []).length, 2);
});

test('generic XTS handler dispatches the current command, exact relative filename and CLI parameters', async () => {
  const body = genericXtsHandler(compiledExtension());
  const calls = [];
  let activations = 0;
  const factory = vm.runInNewContext(`${body}\nmakeXtsOpenViewHandler`, {
    vscode: {
      extensions: { getExtension: () => ({ isActive: false, activate: async () => { activations++; } }) },
      commands: { executeCommand: async (...args) => { assert.equal(activations, 1); calls.push(args); } },
    },
    xtsHandoff_1: require('../out/xtsHandoff'),
    xtsViewVerification_1: { waitForXtsViewVerification: async () => 'opened' },
  });
  const handler = factory({ webview: {} }, () => true, () => {});
  const result = await handler({
    request: { view_path: 'Database Replicas.xts', environment: 'ProdEus1a',
      parameters: { server: 'server-859807057', database: 'database-859807057' }, focus: true },
    cancellationToken: { isCancellationRequested: false },
  });
  assert.deepEqual(calls, [['xts.openViewByPath', 'Database Replicas.xts',
    '-p environment:ProdEus1a -p server:server-859807057 -p database:database-859807057']]);
  assert.equal(result.result.status, 'opened');
  assert.doesNotMatch(compiledExtension(), /xts\.openViewWithParameters/);
});

test('cancellation during XTS activation prevents dispatch of the current command', async () => {
  const token = { isCancellationRequested: false };
  const calls = [];
  const factory = vm.runInNewContext(`${genericXtsHandler(compiledExtension())}\nmakeXtsOpenViewHandler`, {
    vscode: {
      extensions: { getExtension: () => ({ isActive: false, activate: async () => { token.isCancellationRequested = true; } }) },
      commands: { executeCommand: async (...args) => calls.push(args) },
    },
    xtsHandoff_1: require('../out/xtsHandoff'),
    xtsViewVerification_1: { waitForXtsViewVerification: async () => { throw new Error('must not verify a cancelled launch'); } },
  });
  const result = await factory({ webview: {} }, () => true, () => {})({
    request: { view_path: 'Database Replicas.xts', environment: 'ProdEus1a',
      parameters: { server: 'server', database: 'database' }, focus: true },
    cancellationToken: token,
  });
  assert.equal(result.status, 'execution-not-started');
  assert.equal(calls.length, 0);
});

test('generic XTS handler delegates fail-closed real-view verification instead of requiring a command acknowledgment', () => {
  const body = genericXtsHandler(compiledExtension());
  assert.match(body, /launchXtsWithHandoff/);
  assert.match(body, /waitForXtsViewVerification/);
  assert.doesNotMatch(body, /parseAcknowledgment|isXtsLaunchAcknowledgment/);
  assert.doesNotMatch(body, /result \?\? null|return \{ status: ['"]completed['"], result \}/);
});

test('generic XTS handler requires panel confirmation with no modal or opt-out fallback', () => {
  const source = compiledExtension();
  const body = genericXtsHandler(source);
  const manifest = require('../package.json');

  assert.match(body, /if \(!panelConfirmed\)/);
  assert.match(body, /CONFIRMATION_REQUIRED/);
  assert.doesNotMatch(body, /modal:\s*true|showConfirmation|confirmationEnabled|disableConfirmation|XTS_HANDOFF_SETTING/);
  assert.equal(manifest.contributes.configuration.properties['yawr.xts.showHandoffConfirmation'], undefined);
  assert.ok(body.indexOf('if (!panelConfirmed)') < body.indexOf('launchXtsWithHandoff'));
});

test('all generic XTS registrations use the command initialization deadline', () => {
  const source = compiledExtension();
  const timeout = source.match(/const XTS_HOST_ACTION_TIMEOUT_MS\s*=\s*([\d_]+)/);
  assert.ok(timeout);
  assert.ok(Number(timeout[1].replaceAll('_', '')) > 300_000);
  assert.equal((source.match(/timeoutMs:\s*XTS_HOST_ACTION_TIMEOUT_MS/g) ?? []).length, 2);
  assert.equal(
    (source.match(/executeCommand\(\s*['"]xts\.openViewByPath['"]/g) ?? []).length,
    1,
  );
});