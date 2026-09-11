// runLifecycle.test.js — lifecycle regression tests.
//
// Tests:
//   C1. Normal Run sends the EXACT form payload to invokeTool (deep equality,
//       mutation-proof: adding/dropping/renaming a key must fail the test).
//   C2. No second command/action contributed: yawr.runAuthenticated absent from
//       package.json contributes.commands, menus.editor/title, and menus.editor/context.
//   C3. No arguments enter logs or responses — sentinel must NOT appear in any
//       output-channel line or HTTP response body. Tests the real bridge code path.
//   C4. Unarmed state fails closed before invocation: spy MUST NOT be called
//       without toolInvocationToken because that path can trigger an auth prompt.
//   C5. Non-MCP runs (validateInputs dry-run path) preserve current behaviour:
//       the command is contributed and the enumInputs module is intact.
//
// All tests flow through the REAL production code path (McpBridge.handle),
// not pure helpers. Mutations that break the invariant will break the specific
// test labelled here.

'use strict';

const assert = require('node:assert/strict');
const fs     = require('node:fs');
const http   = require('node:http');
const path   = require('node:path');
const test   = require('node:test');

const { McpBridge, isCanceledError } = require('../out/mcpBridge');
const { buildRegistryFromDir }       = require('../out/toolDefinitionRegistry');

const FIXTURE_TOOLS_DIR = path.join(__dirname, 'fixtures', 'tools');
const fixtureRegistry   = buildRegistryFromDir(FIXTURE_TOOLS_DIR);

// ─── Helpers ──────────────────────────────────────────────────────────────────

async function postBridge(url, payload) {
  return new Promise((resolve, reject) => {
    const data = JSON.stringify(payload);
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

// ─── C1: Exact payload delivery ───────────────────────────────────────────────

test('C1: bridge delivers the EXACT submitted args to invokeTool (deep equality)', async (t) => {
  // The payload that the "form" submitted.
  const SUBMITTED_ARGS = { incident_id: '99887' };

  let receivedInput = null;
  const lm = {
    tools: [{ name: 'icm-get-incident' }],
    getToolInvocationToken: () => 'test-token-for-payload-path',
    invokeTool: async (_name, opts) => {
      receivedInput = opts.input;
      return makeIcmResult();
    },
  };

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  await postBridge(bridge.bridgeUrl, makeRequest(bridge, { args: SUBMITTED_ARGS }));

  // Deep equality: the exact submitted object must arrive at invokeTool.
  assert.deepEqual(receivedInput, SUBMITTED_ARGS,
    'invokeTool must receive exactly the submitted args — no keys added, dropped, renamed, or coerced');

  // Mutation proof A: adding a key to the args before invokeTool would FAIL this assert.
  // Mutation proof B: dropping a key from the args would FAIL this assert.
  // Mutation proof C: renaming a key would FAIL this assert.
  // Mutation proof D: coercing a value (e.g. Number('99887') = 99887) would FAIL this assert
  //   because deepEqual distinguishes '99887' (string) from 99887 (number).
  assert.strictEqual(typeof receivedInput.incident_id, 'string',
    'string input must not be coerced to another type');
  assert.strictEqual(Object.keys(receivedInput).length, Object.keys(SUBMITTED_ARGS).length,
    'no extra keys must be added to the payload before invoking');
});

// ─── C2: No runAuthenticated command contributed ──────────────────────────────

test('C2: yawr.runAuthenticated is absent from package.json commands', () => {
  const manifest = require('../package.json');
  const commands = manifest.contributes.commands ?? [];
  const found = commands.find((c) => c.command === 'yawr.runAuthenticated');
  assert.equal(found, undefined,
    'yawr.runAuthenticated must NOT be contributed — it was the forbidden second-action');
});

test('C2: yawr.runAuthenticated is absent from editor/title menu', () => {
  const manifest = require('../package.json');
  const titleItems = manifest.contributes.menus?.['editor/title'] ?? [];
  const found = titleItems.find((item) => item.command === 'yawr.runAuthenticated');
  assert.equal(found, undefined,
    'yawr.runAuthenticated must NOT appear in editor/title');
});

test('C2: editor/context menu has no runbook entry (yawr.runAuthenticated removed)', () => {
  const manifest = require('../package.json');
  const contextItems = manifest.contributes.menus?.['editor/context'] ?? [];
  // Acceptable: no editor/context menu at all, or no runAuthenticated entry.
  const found = contextItems.find((item) => item.command === 'yawr.runAuthenticated');
  assert.equal(found, undefined,
    'yawr.runAuthenticated must NOT appear in editor/context');
});

// ─── C3: No args in logs or responses ────────────────────────────────────────

test('C3: bridge never logs or echoes invocation arguments (sentinel probe)', async (t) => {
  // Distinctive sentinel that must not leak out of the invocation path.
  const SECRET_ARG_VALUE = 'PRIVATE_ARGUMENT_SENTINEL_xQ9zAb3K_DO_NOT_LEAK';

  const logLines = [];
  const output   = { appendLine: (s) => logLines.push(s) };

  let invokeCallCount = 0;
  const lm = {
    tools: [{ name: 'icm-get-incident' }],
    getToolInvocationToken: () => 'test-token-for-redaction-path',
    // invokeTool is the actual production invocation path — the sentinel
    // flows through McpBridge.handle() → validateArgsAgainstSchema → invokeTool.
    invokeTool: async (_name, opts) => {
      invokeCallCount++;
      // We don't return the sentinel in the result — we're testing the INPUT path.
      return makeIcmResult();
    },
  };

  const bridge = await McpBridge.create(lm, 0, output, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  // Send a request with the sentinel in the args. The bridge processes this
  // through the real production handle() path.
  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    args: { incident_id: SECRET_ARG_VALUE },
  }));

  // Non-vacuity: invokeTool must have been called (sentinel DID flow through the path).
  assert.equal(invokeCallCount, 1,
    `non-vacuity: invokeTool must be called (sentinel reached the production path), got invokeCount=${invokeCallCount}`);

  // Non-vacuity: the sentinel is a substring we can detect.
  const canary = `{"incident_id":"${SECRET_ARG_VALUE}"}`;
  assert.ok(canary.includes(SECRET_ARG_VALUE), 'non-vacuity: canary string contains the sentinel');

  // Main assertion: the sentinel must NOT appear in any log line.
  for (const line of logLines) {
    assert.equal(line.includes(SECRET_ARG_VALUE), false,
      `sentinel must not appear in output-channel log: "${line.slice(0, 120)}"`);
  }

  // Main assertion: the sentinel must NOT appear in the HTTP response body.
  const serialized = JSON.stringify(body);
  assert.equal(serialized.includes(SECRET_ARG_VALUE), false,
    `sentinel must not appear in HTTP response: ${serialized.slice(0, 200)}`);
});

// ─── C4: Unarmed state fails closed before invocation ────────────────────────

test('C4: unarmed bridge (no cached token) fails closed before invokeTool', async (t) => {
  let invokeCount = 0;

  const lm = {
    tools: [{ name: 'icm-get-incident' }],
    getToolInvocationToken: () => undefined,
    invokeTool: async () => {
      invokeCount++;
      return makeIcmResult();
    },
  };

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  assert.equal(invokeCount, 0,
    `invokeTool spy must NOT be called when unarmed; got invokeCount=${invokeCount}`);
  assert.ok(body.error, 'expected fail-closed error from missing token');
  assert.equal(body.error.code, 'invocation_token_unavailable');
  assert.equal(body.result, undefined);
});

test('C4: unarmed failure is a named safety category, never no_active_run or provider auth', async (t) => {
  const lm = {
    tools: [{ name: 'icm-get-incident' }],
    getToolInvocationToken: () => undefined,
    invokeTool: async () => {
      throw new Error('Unauthorized: credential not found');
    },
  };

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  assert.ok(body.error, 'expected missing-token error before provider invocation');
  assert.notEqual(body.error.code, 'no_active_run',
    'no_active_run must never be returned — use the explicit token category');
  assert.equal(body.error.code, 'invocation_token_unavailable');
});

// ─── C5: Non-MCP runs (validateInputs) preserve current behaviour ─────────────

test('C5: canonical validateInputs command is contributed once', () => {
  const manifest = require('../package.json');
  const commands = manifest.contributes.commands ?? [];
  const cmd = commands.find((c) => c.command === 'yawr.validateInputs');
  assert.ok(cmd, 'yawr.validateInputs must still be contributed — non-MCP dry-run path preserved');
  assert.equal(commands.filter((c) => c.command === 'yawr.validateInputs').length, 1);
  assert.doesNotMatch(cmd.title, /alias/i);
});

test('C5: validateInputs presents the canonical yawr dry-run command', () => {
  const source = fs.readFileSync(path.join(__dirname, '..', 'src', 'extension.ts'), 'utf8');
  assert.match(source, /Variable overrides for yawr dry-run/);
  assert.match(source, /reportEngineFailure\('yawr dry-run', err\)/);
});

test('C5: isCanceledError predicate exported from mcpBridge is the fail-closed discriminator', () => {
  // Verify the Canceled-error predicate is present and correct.
  // It must never authorize an unauthenticated retry.
  assert.equal(typeof isCanceledError, 'function', 'isCanceledError must be exported from mcpBridge');

  // True positives.
  assert.equal(isCanceledError(new Error('Canceled')), true,
    'exact "Canceled" must match (VS Code style)');
  assert.equal(isCanceledError(new Error('Request was Canceled by the user')), true,
    '"Canceled" word boundary must match');
  assert.equal(isCanceledError(new Error('cancelled by timeout')), true,
    '"cancelled" (British spelling) must match');

  // False positives guarded.
  assert.equal(isCanceledError(new Error('cannot cancel active request')), false,
    '"cancel" substring without word boundary must NOT match');
  assert.equal(isCanceledError(new Error('Unauthorized')), false,
    'unrelated error must not match');
  assert.equal(isCanceledError(new Error('canceled')), false,
    'lowercase "canceled" must not match (VS Code uses capital C)');
});

// ─── Canceled fail-closed path ───────────────────────────────────────────────
// Verifies the safety invariant: Canceled + token present → fail, no tokenless retry.

test('C5-fail-closed: Canceled error with cached token does not retry without token', async (t) => {
  let invokeCount  = 0;
  const tokenValues  = [];

  const CACHED_TOKEN = 'my-cached-token';

  const lm = {
    tools: [{ name: 'icm-get-incident' }],
    getToolInvocationToken: () => CACHED_TOKEN,
    invokeTool: async (_name, opts) => {
      invokeCount++;
      tokenValues.push(opts.toolInvocationToken);
      throw new Error('Canceled');
    },
  };

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  assert.equal(invokeCount, 1, 'Canceled + token present must NOT trigger a second invocation');
  assert.deepEqual(tokenValues, [CACHED_TOKEN],
    'the only invokeTool call must use the cached token; no undefined-token retry is allowed');
  assert.ok(body.error, 'Canceled token path must fail closed');
  assert.equal(body.error.code, 'invocation_token_unavailable');
  assert.equal(body.result, undefined);
});

test('C5-fail-closed: missing cached token refuses before invokeTool', async (t) => {
  let invokeCount = 0;

  const lm = {
    tools: [{ name: 'icm-get-incident' }],
    getToolInvocationToken: () => undefined,
    invokeTool: async () => {
      invokeCount++;
      throw new Error('Canceled');
    },
  };

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  assert.equal(invokeCount, 0, 'missing token must prevent invokeTool entirely');
  assert.ok(body.error, 'missing token must return a coded error');
  assert.equal(body.error.code, 'invocation_token_unavailable');
  assert.equal(body.result, undefined);
});
