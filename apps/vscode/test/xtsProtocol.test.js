'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

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
  assert.doesNotMatch(source, /xts\.open-sterling-servers-and-databases/);
  assert.doesNotMatch(source, /makeXtsOpenSterlingHandler/);
  assert.doesNotMatch(source, /chongliu\/sterling servers and databases\.xts/);
});

test('generic XTS handler maps wire fields to the public XTS command without changing parameters', () => {
  const body = genericXtsHandler(compiledExtension());
  assert.match(body, /executeCommand\(['"]xts\.openViewWithParameters['"], \{/);
  assert.match(body, /viewPath,/);
  assert.match(body, /environment,/);
  assert.match(body, /parameters: params,/);
  assert.match(body, /focus,/);
  assert.match(body, /correlationId: args\.correlationId/);
  assert.doesNotMatch(body, /search_string|server_name|\.xts['"]/);
});

test('generic XTS handler delegates fail-closed acknowledgment handling to the handoff orchestrator', () => {
  const body = genericXtsHandler(compiledExtension());
  assert.match(body, /launchXtsWithHandoff/);
  assert.match(body, /parseAcknowledgment/);
  assert.match(body, /isXtsLaunchAcknowledgment/);
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
    (source.match(/executeCommand\(\s*['"]xts\.openViewWithParameters['"]/g) ?? []).length,
    1,
  );
});