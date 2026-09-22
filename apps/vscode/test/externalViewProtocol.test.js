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

function genericExternalViewHandler(source) {
  const start = source.indexOf('function makeExternalViewOpenHandler(');
  const end = source.indexOf('async function previewGraph(', start);
  assert.ok(start >= 0 && end > start, 'compiled generic external view handler must be present');
  return source.slice(start, end);
}

test('External view uses the canonical yawr.host-action/v1 wire version', () => {
  assert.equal(WIRE_VERSION, 'yawr.host-action/v1');
});

test('External view bridge exposes one generic open-view capability with no product-owned view', () => {
  const source = compiledExtension();
  assert.match(source, /external-view\.open/);
  assert.doesNotMatch(source, /external-view\.open-(?!view\b)[\w.-]+/);
  assert.equal((source.match(/\[['"]external-view\.open['"],\s*\{\s*handler:/g) ?? []).length, 2);
});

test('generic external view handler dispatches the configured command, exact relative filename and CLI parameters', async () => {
  const body = genericExternalViewHandler(compiledExtension());
  const calls = [];
  let activations = 0;
  const factory = vm.runInNewContext(`${body}\nmakeExternalViewOpenHandler`, {
    vscode: {
      workspace: { getConfiguration: () => ({ get: (_key, def) => def }) },
      extensions: { getExtension: () => ({ isActive: false, activate: async () => { activations++; } }) },
      commands: { executeCommand: async (...args) => { calls.push(args); } },
    },
    externalViewHandoff_1: require('../out/externalViewHandoff'),
    externalViewVerification_1: { waitForExternalViewVerification: async () => 'opened' },
    enumInputs_1: require('../out/enumInputs'),
  });
  const handler = factory({ webview: {} }, () => true, () => {});
  const result = await handler({
    request: { view_path: 'Database Replicas.view', environment: 'ProdEus1a',
      parameters: { server: 'server-859807057', database: 'database-859807057' }, focus: true },
    cancellationToken: { isCancellationRequested: false },
  });
  assert.deepEqual(calls, [['externalView.openByPath', 'Database Replicas.view',
    '-p environment:ProdEus1a -p server:server-859807057 -p database:database-859807057']]);
  assert.equal(result.result.status, 'opened');
});

test('cancellation during external view activation prevents dispatch of the command', async () => {
  const token = { isCancellationRequested: false };
  const calls = [];
  const factory = vm.runInNewContext(`${genericExternalViewHandler(compiledExtension())}\nmakeExternalViewOpenHandler`, {
    vscode: {
      workspace: { getConfiguration: () => ({ get: (key, def) => key.includes('extensionId') ? 'custom.ext' : def }) },
      extensions: { getExtension: () => ({ isActive: false, activate: async () => { token.isCancellationRequested = true; } }) },
      commands: { executeCommand: async (...args) => calls.push(args) },
    },
    externalViewHandoff_1: require('../out/externalViewHandoff'),
    externalViewVerification_1: { waitForExternalViewVerification: async () => { throw new Error('must not verify a cancelled launch'); } },
    enumInputs_1: require('../out/enumInputs'),
  });
  const result = await factory({ webview: {} }, () => true, () => {})({
    request: { view_path: 'Database Replicas.view', environment: 'ProdEus1a',
      parameters: { server: 'server', database: 'database' }, focus: true },
    cancellationToken: token,
  });
  assert.equal(result.status, 'execution-not-started');
  assert.equal(calls.length, 0);
});

test('generic external view handler delegates fail-closed real-view verification instead of requiring a command acknowledgment', () => {
  const body = genericExternalViewHandler(compiledExtension());
  assert.match(body, /launchExternalViewWithHandoff/);
  assert.match(body, /waitForExternalViewVerification/);
  assert.doesNotMatch(body, /parseAcknowledgment|isLaunchAcknowledgment/);
  assert.doesNotMatch(body, /result \?\? null|return \{ status: ['"]completed['"], result \}/);
});

test('generic external view handler requires panel confirmation with no modal or opt-out fallback', () => {
  const source = compiledExtension();
  const body = genericExternalViewHandler(source);
  const manifest = require('../package.json');

  assert.match(body, /if \(!panelConfirmed\)/);
  assert.match(body, /CONFIRMATION_REQUIRED/);
  assert.doesNotMatch(body, /modal:\s*true|showConfirmation|confirmationEnabled|disableConfirmation/);
  assert.equal(manifest.contributes.configuration.properties['yawr.externalView.showHandoffConfirmation'], undefined);
  assert.ok(body.indexOf('if (!panelConfirmed)') < body.indexOf('launchExternalViewWithHandoff'));
});

test('all generic external view registrations use the command initialization deadline', () => {
  const source = compiledExtension();
  const timeout = source.match(/const EXTERNAL_VIEW_HOST_ACTION_TIMEOUT_MS\s*=\s*([\d_]+)/);
  assert.ok(timeout);
  assert.ok(Number(timeout[1].replaceAll('_', '')) > 300_000);
  assert.equal((source.match(/timeoutMs:\s*EXTERNAL_VIEW_HOST_ACTION_TIMEOUT_MS/g) ?? []).length, 2);
  assert.equal(
    (source.match(/executeCommand\(\s*command/g) ?? []).length,
    1,
  );
});