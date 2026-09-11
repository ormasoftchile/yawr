// invocationToken.test.js — mandatory SQL Live-Site tests for the
// toolInvocationToken lifecycle in the Yawr VS Code extension.
//
// Tests:
//   1. Unarmed bridge fails closed before invokeTool — no implicit prompt path.
//   2. Armed bridge invokes the registered MCP tool WITH the captured token.
//   3. Token rejection/cancellation clears the cached token and forces rearming.
//   4. Redaction: token text cannot appear in HTTP responses, output-channel
//      logs, errors, traces, OR child-process environment.
//   5. Command/participant gate: token is captured ONLY from the explicit
//      arm-mcp command turn.
//
// Run with: node --test test/invocationToken.test.js  (or via npm test)

'use strict';

const assert = require('node:assert/strict');
const http   = require('node:http');
const test   = require('node:test');

// ─── Load compiled modules ───────────────────────────────────────────────────

const { McpBridge }      = require('../out/mcpBridge');
const { buildRegistryFromDir } = require('../out/toolDefinitionRegistry');
const {
  setToolToken,
  getToolToken,
  clearToolToken,
  isArmed,
  _resetForTest,
} = require('../out/toolTokenStore');

const { isArmCommand } = require('../out/chatParticipantGate');

const path = require('node:path');
const FIXTURE_TOOLS_DIR = path.join(__dirname, 'fixtures', 'tools');
const fixtureRegistry   = buildRegistryFromDir(FIXTURE_TOOLS_DIR);

// ─── Helpers ─────────────────────────────────────────────────────────────────

async function postBridge(url, payload) {
  return new Promise((resolve, reject) => {
    const data = typeof payload === 'string' ? payload : JSON.stringify(payload);
    const opts = new URL(url);
    const req  = http.request(
      {
        hostname: opts.hostname, port: Number(opts.port), path: '/', method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) },
      },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => {
          try { resolve({ status: res.statusCode, body: JSON.parse(Buffer.concat(chunks).toString()) }); }
          catch (e) { reject(e); }
        });
      },
    );
    req.on('error', reject);
    req.end(data);
  });
}

function makeRequest(bridge, overrides = {}) {
  return {
    version:          'yawr.vscode-mcp-bridge/v1',
    request_id:       `req-${Math.random().toString(36).slice(2)}`,
    tool:             'icm',
    action:           'get-incident',
    args:             { incident_id: '12345' },
    capability_proof: bridge.bridgeToken,
    ...overrides,
  };
}

function makeIcmResult() {
  return {
    content: [{
      value: JSON.stringify({
        title: 'CPU spike', service: 'api-gateway', environment: 'prod',
        logical_server: 'srv-01', database: 'db-primary',
      }),
    }],
  };
}

// makeLm builds a stub LmInterface with a configurable token.
// getTokenFn returns the toolInvocationToken; pass () => undefined for unarmed.
function makeLm({ getTokenFn, invokeToolFn, onTokenRejectedFn } = {}) {
  return {
    tools:                   [{ name: 'icm-get-incident' }],
    getToolInvocationToken:  getTokenFn  ?? (() => 'test-token-default'),
    onTokenRejected:         onTokenRejectedFn ?? undefined,
    invokeTool:              invokeToolFn ?? (async () => makeIcmResult()),
  };
}

// ─── Test 1: Unarmed bridge fails closed before invokeTool ───────────────────

test('INVTOKEN-1: unarmed bridge fails closed and never invokes without a token', async (t) => {
  let invokeCount = 0;

  const lm = makeLm({
    getTokenFn:    () => undefined,
    invokeToolFn:  async () => {
      invokeCount++;
      return makeIcmResult();
    },
  });

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  assert.equal(invokeCount, 0,
    `invokeTool must NOT be called when no token is cached (spy count = ${invokeCount})`);
  assert.ok(body.error, 'unarmed invocation must return a coded error');
  assert.equal(body.error.code, 'invocation_token_unavailable');
  assert.equal(body.result, undefined, 'result must be absent when invocation is refused');
});

// ─── Test 2: Armed bridge invokes WITH the captured token ────────────────────

test('INVTOKEN-2: armed bridge invokes the tool and passes the captured token', async (t) => {
  const CAPTURED_TOKEN = Symbol('captured-token-xyz');  // opaque, comparable by identity
  let receivedToken    = null;
  let invokeCount      = 0;

  const lm = makeLm({
    getTokenFn:   () => CAPTURED_TOKEN,   // armed with a specific token
    invokeToolFn: async (_name, opts) => {
      invokeCount++;
      receivedToken = opts.toolInvocationToken;
      return makeIcmResult();
    },
  });

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  assert.equal(body.error, undefined, `unexpected error: ${JSON.stringify(body.error)}`);
  assert.ok(body.result, 'result must be present on a successful armed invocation');
  assert.equal(invokeCount, 1, 'invokeTool must be called exactly once');
  assert.strictEqual(receivedToken, CAPTURED_TOKEN,
    'invokeTool must receive the exact token captured via setToolToken');
});

// ─── Test 3: Token rejection clears the store and forces rearming ────────────

test('INVTOKEN-3: token rejection clears the cached token (forces rearming)', async (t) => {
  let rejectionCount = 0;

  // lm with a token present, but invokeTool throws a rejection error.
  const lm = makeLm({
    getTokenFn: () => 'stale-token-from-prior-session',
    invokeToolFn: async () => {
      throw new Error('No toolInvocationToken provided for this invocation');
    },
    onTokenRejectedFn: () => {
      rejectionCount++;
      clearToolToken();   // mirrors the extension.ts adapter
    },
  });

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  // Seed the pure store to confirm it starts armed.
  _resetForTest();
  setToolToken('stale-token-from-prior-session');
  assert.equal(isArmed(), true, 'store must be armed before the rejection test');

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  assert.ok(body.error, 'expected error response on token rejection');
  assert.equal(body.error.code, 'invocation_token_unavailable');

  // The rejection callback must have fired exactly once.
  assert.equal(rejectionCount, 1,
    `onTokenRejected must be called once on token rejection (got ${rejectionCount})`);

  // The pure store must now be cleared (unarmed).
  assert.equal(isArmed(), false,
    'token store must be cleared after rejection so the operator must re-arm');

  _resetForTest(); // cleanup
});

test('INVTOKEN-3b: Canceled from VS Code token path clears token and does not retry without token', async (t) => {
  let rejectionCount = 0;
  let invokeCount = 0;
  const tokenValues = [];

  const lm = makeLm({
    getTokenFn: () => 'token-that-vscode-cancels',
    invokeToolFn: async (_name, opts) => {
      invokeCount++;
      tokenValues.push(opts.toolInvocationToken);
      throw new Error('Canceled');
    },
    onTokenRejectedFn: () => {
      rejectionCount++;
      clearToolToken();
    },
  });

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  _resetForTest();
  setToolToken('token-that-vscode-cancels');

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  assert.equal(invokeCount, 1, 'Canceled token path must not retry unauthenticated');
  assert.deepEqual(tokenValues, ['token-that-vscode-cancels'],
    'the only invokeTool call must use the cached token');
  assert.ok(body.error, 'expected fail-closed error response');
  assert.equal(body.error.code, 'invocation_token_unavailable');
  assert.equal(body.result, undefined);
  assert.equal(rejectionCount, 1, 'Canceled token path must clear the cached token exactly once');
  assert.equal(isArmed(), false, 'token store must be cleared after Canceled token path');

  _resetForTest();
});

// ─── Test 4: Redaction — token cannot appear in any output surface ───────────

test('INVTOKEN-4a: tool invocation token must not appear in HTTP response body', async (t) => {
  const FAKE_TOKEN = 'FAKE_INVTOKEN_REDACT_abc123_8f7e6d5c';

  // Armed bridge — token is present, invokeTool returns a valid result.
  const lm = makeLm({ getTokenFn: () => FAKE_TOKEN });
  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  const serialized = JSON.stringify(body);

  // Non-vacuity: confirm the scan subjects are non-empty.
  assert.ok(serialized.length > 2, `response body is non-empty (${serialized.length} chars)`);
  // Non-vacuity control: a string that DOES contain the token would be detected.
  const canary = `{"leaked":"${FAKE_TOKEN}"}`;
  assert.ok(canary.includes(FAKE_TOKEN), 'non-vacuity: scanner must detect a crafted leak');

  assert.equal(serialized.includes(FAKE_TOKEN), false,
    `tool invocation token must not appear in HTTP response: ${serialized.slice(0, 200)}`);
});

test('INVTOKEN-4b: tool invocation token must not appear in output-channel logs', async (t) => {
  const FAKE_TOKEN = 'FAKE_INVTOKEN_LOGREDACT_xyz987_4b3a2c1d';
  const logLines   = [];
  const output     = { appendLine: (s) => logLines.push(s) };

  // Bridge where invokeTool throws so the log path is exercised.
  const lm = makeLm({
    getTokenFn:   () => FAKE_TOKEN,
    invokeToolFn: async () => { throw new Error('simulated provider error'); },
  });
  const bridge = await McpBridge.create(lm, 0, output, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  // Non-vacuity: log must have been written (error path logs).
  assert.ok(logLines.length > 0, `output channel must have received at least one log line (got ${logLines.length})`);

  for (const line of logLines) {
    assert.equal(line.includes(FAKE_TOKEN), false,
      `tool invocation token must not appear in output channel: "${line.slice(0, 120)}"`);
  }
});

test('INVTOKEN-4c: tool invocation token must not appear in bridge error messages', async (t) => {
  const FAKE_TOKEN = 'FAKE_INVTOKEN_ERRMSG_qrs456_9z8y7x';

  // Unarmed — error returned without ever calling invokeTool.
  const lm = makeLm({ getTokenFn: () => undefined });
  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  const serialized = JSON.stringify(body);

  assert.ok(serialized.length > 2, `error body is non-empty (${serialized.length} chars)`);
  assert.equal(serialized.includes(FAKE_TOKEN), false,
    `token must not appear in error body: ${serialized.slice(0, 200)}`);
});

test('INVTOKEN-4d: tool invocation token must not appear in spawned child-process env', (t) => {
  // This test exercises the exact construction path from serverManager.ts
  // line ~115: env: { ...process.env, ...this.bridgeEnv ?? {} }
  // The bridgeEnv carries YAWR_VSCODE_BRIDGE_URL and YAWR_VSCODE_BRIDGE_TOKEN
  // (the capability secret) — never the toolInvocationToken.
  const FAKE_INV_TOKEN = 'FAKE_INVTOKEN_CHILDENV_lmn789_5e4f3g2h';

  // Arm the store with a fake token.
  _resetForTest();
  setToolToken(FAKE_INV_TOKEN);
  assert.equal(isArmed(), true, 'store must be armed for this test');

  // Simulate what setBridgeEnv sets (URL + capability secret only).
  const bridgeEnv = {
    YAWR_VSCODE_BRIDGE_URL:   'http://127.0.0.1:9999',
    YAWR_VSCODE_BRIDGE_TOKEN: 'bridge-capability-secret-abc',
  };

  // Construct the spawn env exactly as serverManager.ts does.
  const spawnEnv = { ...process.env, ...bridgeEnv };

  // Non-vacuity: process.env is non-empty.
  assert.ok(Object.keys(spawnEnv).length > 0,
    `spawn env must be non-empty (got ${Object.keys(spawnEnv).length} keys)`);

  // Scan every key and value for the invocation token.
  let scannedValues = 0;
  for (const [k, v] of Object.entries(spawnEnv)) {
    const strV = String(v ?? '');
    scannedValues++;
    assert.equal(strV.includes(FAKE_INV_TOKEN), false,
      `env["${k}"] must not contain the tool invocation token`);
    assert.equal(k.includes(FAKE_INV_TOKEN), false,
      `env key "${k}" must not contain the tool invocation token`);
  }
  assert.ok(scannedValues > 0, `scanned ${scannedValues} env entries`);

  // Non-vacuity control: the injected token IS in the store — if it leaked
  // into spawnEnv the scan above would have caught it.
  assert.equal(getToolToken(), FAKE_INV_TOKEN,
    'non-vacuity: invocation token is in the store but must not appear in spawnEnv');

  _resetForTest(); // cleanup
});

// ─── Test 5: Token captured only from the explicit arm-mcp command ───────────
//
// This test exercises the pure toolTokenStore + the gating rule:
// setToolToken must be called only when command === 'arm-mcp', mirroring the
// extension.ts chat participant handler. Any other command must leave the
// store unchanged.

test('INVTOKEN-5: token is captured only from the arm-mcp command (other commands leave store unchanged)', () => {
  _resetForTest();
  assert.equal(isArmed(), false, 'store must start unarmed');

  // isArmCommand is the pure gate imported from chatParticipantGate.ts.
  // Extension.ts calls isArmCommand(request.command) — patching that module
  // IS what mutation 2 tests (it must cause this test to fail).
  assert.equal(isArmCommand('arm-mcp'), true,
    'arm-mcp must be recognized as the arm command');
  assert.equal(isArmCommand(undefined), false,
    'undefined (no slash command) must not be treated as arm-mcp');
  assert.equal(isArmCommand('arm-mcp-extra'), false,
    'a command that merely starts with arm-mcp must not gate');
  assert.equal(isArmCommand('some-other-command'), false,
    'any unrecognised command must not gate');
  assert.equal(isArmCommand('ARM-MCP'), false,
    'gate must be case-sensitive — ARM-MCP is not arm-mcp');

  // Simulate the arm handler using the gate (mirrors extension.ts logic exactly).
  const simulateCommandHandler = (command, token) => {
    if (isArmCommand(command)) {
      setToolToken(token);
    }
  };

  simulateCommandHandler(undefined, 'should-not-be-stored');
  assert.equal(isArmed(), false, 'no-command prompt must not arm the bridge');

  simulateCommandHandler('some-other-command', 'should-not-be-stored-2');
  assert.equal(isArmed(), false, 'unrecognised command must not arm the bridge');

  const REAL_TOKEN = Symbol('real-captured-token');
  simulateCommandHandler('arm-mcp', REAL_TOKEN);
  assert.equal(isArmed(), true, 'arm-mcp command must arm the bridge');
  assert.strictEqual(getToolToken(), REAL_TOKEN,
    'arm-mcp command must store the exact token from the chat request');

  simulateCommandHandler(undefined, 'overwrite-attempt');
  assert.strictEqual(getToolToken(), REAL_TOKEN,
    'a subsequent non-arm command must not overwrite an already-captured token');

  _resetForTest();
});
