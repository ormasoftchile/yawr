// mcpBridge.test.js — unit tests for the Yawr↔VS Code loopback bridge.
//
// All vscode.lm calls are stubbed; no real authenticated MCP tool is required.
// Run with: node --test test/mcpBridge.test.js  (or via npm test)

'use strict';

const assert = require('node:assert/strict');
const http = require('node:http');
const path = require('node:path');
const test = require('node:test');

// ─── Load compiled bridge ────────────────────────────────────────────────────
const {
  McpBridge,
  resolveSpec,
  normalizeResult,
  extractTextFromResult,
} = require('../out/mcpBridge');

const { buildRegistryFromDir } = require('../out/toolDefinitionRegistry');

const FIXTURE_TOOLS_DIR = path.join(__dirname, 'fixtures', 'tools');

// Load the fixture registry once (sync) — all tests share it.
const fixtureRegistry = buildRegistryFromDir(FIXTURE_TOOLS_DIR);

// ─── Helpers ─────────────────────────────────────────────────────────────────

// postBridge sends one HTTP POST to the bridge and returns { status, body }.
async function postBridge(url, payload) {
  return new Promise((resolve, reject) => {
    const data = typeof payload === 'string' ? payload : JSON.stringify(payload);
    const opts = new URL(url);
    const req = http.request(
      { hostname: opts.hostname, port: Number(opts.port), path: '/', method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) } },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => {
          try {
            resolve({ status: res.statusCode, body: JSON.parse(Buffer.concat(chunks).toString()) });
          } catch (e) {
            reject(e);
          }
        });
      },
    );
    req.on('error', reject);
    req.end(data);
  });
}

// makeRequest builds a valid bridge request.
function makeRequest(bridge, overrides = {}) {
  return {
    version: 'yawr.vscode-mcp-bridge/v1',
    request_id: `req-${Math.random().toString(36).slice(2)}`,
    tool: 'icm',
    action: 'get-incident',
    args: { incident_id: '12345' },
    capability_proof: bridge.bridgeToken,
    ...overrides,
  };
}

// makeResult builds a valid icm/get-incident MCP tool result.
function makeIcmResult() {
  return {
    content: [
      {
        value: JSON.stringify({
          title: 'CPU spike',
          service: 'api-gateway',
          environment: 'prod',
          logical_server: 'srv-01',
          database: 'db-primary',
        }),
      },
    ],
  };
}

// makeLm returns a stub lm interface. invokeToolFn is called for each
// invocation; defaults to returning a valid icm result.
function makeLm(invokeToolFn, tools) {
  return {
    tools: tools ?? [
      { name: 'icm-get-incident' },
    ],
    getToolInvocationToken: () => 'test-invocation-token',
    invokeTool: invokeToolFn ??
      (async (_name, _opts, _token) => makeIcmResult()),
  };
}

// createBridge creates a McpBridge using the fixture registry.
function createBridge(lm, overrides) {
  return McpBridge.create(lm ?? makeLm(), 0, undefined, {
    registry: fixtureRegistry,
    overrides,
  });
}

// createBridgeWithRegistry creates a McpBridge with a custom registry.
// Used by optional-field regression tests which need an inline spec.
function createBridgeWithRegistry(registry, lm) {
  return McpBridge.create(lm ?? makeLm(), 0, undefined, { registry });
}

// ─── Pure unit tests (no network) ────────────────────────────────────────────

test('fixture registry: icm/get-incident has five declared output fields', () => {
  const spec = fixtureRegistry['icm/get-incident'];
  assert.ok(spec, 'icm/get-incident must be in the fixture registry');
  assert.equal(spec.registeredName, 'icm-get-incident');
  const fields = Object.keys(spec.outputFields);
  assert.deepEqual(fields.sort(), ['database', 'environment', 'logical_server', 'service', 'title'].sort());
  // All fields are required
  for (const f of fields) {
    assert.equal(spec.outputFields[f].required, true, `${f} must be required`);
    assert.equal(spec.outputFields[f].type, 'string');
  }
});

test('fixture registry: ops-meta/act is present with correct registered name', () => {
  const spec = fixtureRegistry['ops-meta/act'];
  assert.ok(spec, 'ops-meta/act must be in the fixture registry');
  assert.equal(spec.registeredName, 'ops-meta',
    'registeredName must derive from meta.name');
});

test('precedence: action-level vscode_tool beats transport-level and fallback', () => {
  const spec = fixtureRegistry['precedence-tool/action-with-action-level'];
  assert.ok(spec);
  assert.equal(spec.registeredName, 'action-level-name',
    'action-level vscode_tool must beat transport-level');
});

test('precedence: transport-level vscode_tool beats logical-name fallback', () => {
  const spec = fixtureRegistry['precedence-tool/action-with-transport-level'];
  assert.ok(spec);
  assert.equal(spec.registeredName, 'transport-level-name',
    'transport-level vscode_tool must beat logical-name fallback');
});

test('precedence: logical tool name is the fallback when no vscode_tool is declared', () => {
  const spec = fixtureRegistry['fallback-tool/fallback-action'];
  assert.ok(spec, 'fallback-tool/fallback-action must be in registry');
  assert.equal(spec.registeredName, 'fallback-tool',
    'logical tool name must be used when no vscode_tool is declared at any level');
});

test('resolveSpec: returns correct spec for known pair', () => {
  const spec = resolveSpec('icm', 'get-incident', fixtureRegistry);
  assert.ok(spec);
  assert.equal(spec.registeredName, 'icm-get-incident');
});

test('resolveSpec: returns undefined for unknown pair', () => {
  assert.equal(resolveSpec('no-such', 'tool', fixtureRegistry), undefined);
});

test('resolveSpec: settings override replaces registeredName', () => {
  const overrides = { 'icm/get-incident': 'my-org-icm-get-incident' };
  const spec = resolveSpec('icm', 'get-incident', fixtureRegistry, overrides);
  assert.ok(spec);
  assert.equal(spec.registeredName, 'my-org-icm-get-incident',
    'settings override must replace YAML-derived registeredName');
  // outputFields should still come from the YAML
  assert.ok(spec.outputFields['title']);
});

test('resolveSpec: empty override map has no effect', () => {
  const spec = resolveSpec('icm', 'get-incident', fixtureRegistry, {});
  assert.equal(spec?.registeredName, 'icm-get-incident');
});

// Build an icm spec inline so normalizeResult tests are self-contained.
const ICM_SPEC = {
  registeredName: 'icm-get-incident',
  outputFields: {
    title:          { type: 'string', required: true },
    service:        { type: 'string', required: true },
    environment:    { type: 'string', required: true },
    logical_server: { type: 'string', required: true },
    database:       { type: 'string', required: true },
  },
};

// Neutral inline spec for optional-field normalizeResult tests.
// Field names are abstract to avoid coupling to any consumer contract.
const OPS_OPT_SPEC = {
  registeredName: 'ops-optional-probe',
  outputFields: {
    status:      { type: 'string', required: true },
    detail_id:   { type: 'string', required: false },
    detail_name: { type: 'string', required: false },
  },
};

const ICM_LIVE_TOOL_NAME = 'mcp_icm_mcp_serve_get_incident_details_by_id';
const ICM_LIVE_REGISTRY = {
  'icm/get-incident': {
    ...ICM_SPEC,
    registeredName: ICM_LIVE_TOOL_NAME,
  },
};

// Inline registry for optional-field bridge regression tests.
const OPS_OPT_REGISTRY = { 'ops-optional/probe': OPS_OPT_SPEC };

test('normalizeResult: accepts a fully-valid icm result', () => {
  const result = normalizeResult(
    { title: 'T', service: 'S', environment: 'E', logical_server: 'L', database: 'D' },
    ICM_SPEC,
  );
  assert.equal(result.ok, true);
  assert.deepEqual(result.value, { title: 'T', service: 'S', environment: 'E', logical_server: 'L', database: 'D' });
});

test('normalizeResult: required field absent → fails', () => {
  const result = normalizeResult(
    { title: 'T', service: 'S', environment: 'E', logical_server: 'L' /* database missing */ },
    ICM_SPEC,
  );
  assert.equal(result.ok, false);
  assert.match(result.reason, /database/);
});

test('normalizeResult: optional field ABSENT → succeeds (no-suggestion case)', () => {
  // regression test: was broken by the original hard-coded outputFields
  const result = normalizeResult(
    { status: 'not-found' },
    OPS_OPT_SPEC,
  );
  assert.equal(result.ok, true, `expected ok but got reason: ${result.ok ? '' : result.reason}`);
  assert.equal(result.value['status'], 'not-found');
  assert.equal('detail_id' in result.value, false,
    'absent optional field must not appear in normalized output');
});

test('normalizeResult: optional field PRESENT with correct type → succeeds', () => {
  const result = normalizeResult(
    { status: 'found', detail_id: 'D-1', detail_name: 'Alpha' },
    OPS_OPT_SPEC,
  );
  assert.equal(result.ok, true, `expected ok but got reason: ${result.ok ? '' : result.reason}`);
  assert.equal(result.value['no_such_field'], undefined); // guard: unknown keys absent
  assert.equal(result.value['detail_id'], 'D-1');
  assert.equal(result.value['detail_name'], 'Alpha');
});

test('normalizeResult: optional field PRESENT but wrong type → fails', () => {
  const result = normalizeResult(
    { status: 'found', detail_id: 42 /* should be string */ },
    OPS_OPT_SPEC,
  );
  assert.equal(result.ok, false);
  assert.match(result.reason, /detail_id/);
});

test('normalizeResult: optional field PRESENT but null → fails', () => {
  const result = normalizeResult(
    { status: 'found', detail_id: null },
    OPS_OPT_SPEC,
  );
  assert.equal(result.ok, false);
  assert.match(result.reason, /null/);
});

test('normalizeResult: ignores unknown MCP result fields', () => {
  const result = normalizeResult(
    { title: 'T', service: 'S', environment: 'E', logical_server: 'L', database: 'D', summary: 'additive server field' },
    ICM_SPEC,
  );
  assert.equal(result.ok, true, `expected ok but got reason: ${result.ok ? '' : result.reason}`);
  assert.deepEqual(result.value, {
    title: 'T',
    service: 'S',
    environment: 'E',
    logical_server: 'L',
    database: 'D',
  });
  assert.equal('summary' in result.value, false, 'unknown server field must not be forwarded');
});

test('normalizeResult: fails on type-incompatible value', () => {
  const result = normalizeResult(
    { title: 42 /* should be string */, service: 'S', environment: 'E', logical_server: 'L', database: 'D' },
    ICM_SPEC,
  );
  assert.equal(result.ok, false);
  assert.match(result.reason, /title/);
});

test('normalizeResult: fails on null declared field', () => {
  const result = normalizeResult(
    { title: null, service: 'S', environment: 'E', logical_server: 'L', database: 'D' },
    ICM_SPEC,
  );
  assert.equal(result.ok, false);
});

test('normalizeResult: fails when input is not an object', () => {
  assert.equal(normalizeResult('string', ICM_SPEC).ok, false);
  assert.equal(normalizeResult(null, ICM_SPEC).ok, false);
  assert.equal(normalizeResult([1, 2], ICM_SPEC).ok, false);
});

test('extractTextFromResult: reads value fields from content', () => {
  const text = extractTextFromResult({ content: [{ value: 'hello' }, { value: ' world' }] });
  assert.equal(text, 'hello world');
});

test('extractTextFromResult: reads text fields from content', () => {
  const text = extractTextFromResult({ content: [{ text: 'hi' }] });
  assert.equal(text, 'hi');
});

// ─── Network tests (bridge HTTP server) ──────────────────────────────────────

test('bridge starts on loopback and responds to a valid request', async (t) => {
  const bridge = await createBridge();
  t.after(() => bridge.dispose());

  assert.match(bridge.bridgeUrl, /^http:\/\/127\.0\.0\.1:/);
  const { status, body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.equal(status, 200);
  assert.equal(body.version, 'yawr.vscode-mcp-bridge/v1');
  assert.ok(body.result);
});

test('tool discovery: invokeTool is called with the registered tool name', async (t) => {
  let calledWith = null;
  const lm = makeLm(async (name, _opts, _token) => {
    calledWith = name;
    return makeIcmResult();
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.equal(calledWith, 'icm-get-incident');
});

test('invocation argument shaping: args from request reach invokeTool input', async (t) => {
  let receivedInput = null;
  const lm = makeLm(async (_name, opts, _token) => {
    receivedInput = opts.input;
    return makeIcmResult();
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const args = { incident_id: 'INC-999', region: 'us-west-2' };
  await postBridge(bridge.bridgeUrl, makeRequest(bridge, { args }));
  assert.deepEqual(receivedInput, args);
});

test('result normalization success: response contains expected fields', async (t) => {
  const bridge = await createBridge();
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.result);
  assert.equal(body.result.title, 'CPU spike');
  assert.equal(body.result.service, 'api-gateway');
  assert.equal(body.result.environment, 'prod');
  assert.equal(body.result.logical_server, 'srv-01');
  assert.equal(body.result.database, 'db-primary');
  assert.equal(body.error, undefined);
});

test('settings override: invokeTool is called with the overridden tool name', async (t) => {
  let calledWith = null;
  const lm = makeLm(async (name) => {
    calledWith = name;
    return makeIcmResult();
  }, [{ name: 'my-org-icm-get-incident' }]);
  const bridge = await McpBridge.create(lm, 0, undefined, {
    registry: fixtureRegistry,
    overrides: { 'icm/get-incident': 'my-org-icm-get-incident' },
  });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.equal(calledWith, 'my-org-icm-get-incident',
    'invokeTool must use the settings-override name');
  assert.ok(body.result, 'result must be present when override resolves correctly');
});

test('missing declared output field: bridge returns normalization error', async (t) => {
  const lm = makeLm(async () => ({
    content: [{
      value: JSON.stringify(
        { title: 'T', service: 'S', environment: 'E', logical_server: 'L' /* database missing */ }
      ),
    }],
  }));
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.error, 'expected an error response');
  assert.equal(body.error.code, 'result_normalization_error');
  assert.match(body.error.message, /database/);
  assert.equal(body.result, undefined);
});

test('type-incompatible field: bridge returns normalization error', async (t) => {
  const lm = makeLm(async () => ({
    content: [{
      value: JSON.stringify(
        { title: 999 /* number, not string */, service: 'S', environment: 'E', logical_server: 'L', database: 'D' }
      ),
    }],
  }));
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.error);
  assert.equal(body.error.code, 'result_normalization_error');
  assert.match(body.error.message, /title/);
});

test('additive MCP result field: bridge normalizes declared fields and ignores summary', async (t) => {
  let invokeCount = 0;
  let invokedName = '';
  const lm = makeLm(async (name) => {
    invokeCount++;
    invokedName = name;
    return {
      content: [{
        value: JSON.stringify({
          title: 'CPU spike',
          service: 'api-gateway',
          environment: 'prod',
          logical_server: 'srv-01',
          database: 'db-primary',
          summary: 'additive server field from live ICM-shaped payload',
        }),
      }],
    };
  }, [{ name: ICM_LIVE_TOOL_NAME }]);
  const bridge = await createBridgeWithRegistry(ICM_LIVE_REGISTRY, lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.equal(invokedName, ICM_LIVE_TOOL_NAME, 'fixture must exercise the exact live ICM MCP tool name');
  assert.equal(invokeCount, 1, 'non-vacuity: bridge must call invokeTool exactly once');
  assert.equal(body.error, undefined, `unexpected error: ${JSON.stringify(body.error)}`);
  assert.deepEqual(body.result, {
    title: 'CPU spike',
    service: 'api-gateway',
    environment: 'prod',
    logical_server: 'srv-01',
    database: 'db-primary',
  });
  assert.equal('summary' in body.result, false, 'unknown summary field must be ignored, not forwarded');
});

// Optional-field bridge regression: the bridge must handle BOTH outcomes —
// all optional fields present AND optional fields absent. Uses a neutral
// inline registry (OPS_OPT_REGISTRY) to avoid consumer-contract coupling.
test('optional-field regression: "present" outcome (all fields) normalizes successfully', async (t) => {
  const lm = {
    tools: [{ name: 'ops-optional-probe' }],
    getToolInvocationToken: () => 'test-invocation-token',
    invokeTool: async () => ({
      content: [{
        value: JSON.stringify({
          status: 'found',
          detail_id: 'D-1',
          detail_name: 'Alpha',
        }),
      }],
    }),
  };
  const bridge = await createBridgeWithRegistry(OPS_OPT_REGISTRY, lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    tool: 'ops-optional',
    action: 'probe',
  }));
  assert.equal(body.error, undefined, `unexpected error: ${JSON.stringify(body.error)}`);
  assert.ok(body.result);
  assert.equal(body.result['status'], 'found');
  assert.equal(body.result['detail_id'], 'D-1');
  assert.equal(body.result['detail_name'], 'Alpha');
});

test('optional-field regression: "absent" outcome (only required field) normalizes successfully', async (t) => {
  const lm = {
    tools: [{ name: 'ops-optional-probe' }],
    getToolInvocationToken: () => 'test-invocation-token',
    invokeTool: async () => ({
      content: [{
        value: JSON.stringify({
          status: 'not-found',
          // detail_id and detail_name absent — valid because required: false
        }),
      }],
    }),
  };
  const bridge = await createBridgeWithRegistry(OPS_OPT_REGISTRY, lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    tool: 'ops-optional',
    action: 'probe',
  }));
  assert.equal(body.error, undefined, `unexpected error: ${JSON.stringify(body.error)}`);
  assert.ok(body.result);
  assert.equal(body.result['status'], 'not-found');
  assert.equal('detail_id' in body.result, false,
    'absent optional field must not appear in the response result');
});

test('tool-not-found error: names the logical action and attempted registered name', async (t) => {
  const bridge = await createBridge();
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    tool: 'nonexistent',
    action: 'do-thing',
  }));
  assert.ok(body.error);
  assert.equal(body.error.code, 'tool_not_found');
  assert.match(body.error.message, /nonexistent\/do-thing/);
});

test('tool-unavailable error: names logical action, attempted name, and available names', async (t) => {
  const lm = makeLm(undefined, [{ name: 'some-other-tool' }]); // icm-get-incident not present
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.error);
  assert.equal(body.error.code, 'tool_unavailable');
  // Must name the logical action
  assert.match(body.error.message, /icm\/get-incident/,
    'error must name the logical action');
  // Must name the attempted registered name
  assert.match(body.error.message, /icm-get-incident/,
    'error must name the attempted registered name');
  // Must list available names
  assert.match(body.error.message, /some-other-tool/,
    'error must list available tool names');
});

test('authorization unavailable: invokeTool throws auth error → coded response', async (t) => {
  const lm = makeLm(async () => {
    throw new Error('Unauthorized: credential not found');
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.error);
  assert.equal(body.error.code, 'authorization_unavailable');
  assert.equal(body.result, undefined);
});

test('timeout: deadline in the past returns deadline_exceeded', async (t) => {
  let invokeCount = 0;
  const lm = makeLm(async (_name, _opts, _token) => {
    invokeCount++;
    return makeIcmResult();
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    deadline_unix_ms: Date.now() - 1000, // already past
  }));
  assert.ok(body.error);
  assert.equal(body.error.code, 'deadline_exceeded');
  assert.equal(invokeCount, 0, 'invokeTool must not be called when deadline is past');
});

test('cancellation: invokeTool cancellation token fires when deadline expires', async (t) => {
  let tokenCancelledDuringInvoke = false;
  const lm = makeLm(async (_name, _opts, token) => {
    await new Promise((resolve) => {
      token.onCancellationRequested(resolve);
      setTimeout(resolve, 2000); // safety fallback
    });
    tokenCancelledDuringInvoke = token.isCancellationRequested;
    throw new Error('cancelled');
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    deadline_unix_ms: Date.now() + 80, // 80ms deadline
  }));
  assert.ok(body.error);
  assert.equal(body.error.code, 'deadline_exceeded');
  assert.equal(tokenCancelledDuringInvoke, true, 'cancellation token must be cancelled on deadline');
});

test('cancellation: disposing bridge cancels active invokeTool token', async () => {
  let invocationStarted;
  const started = new Promise((resolve) => { invocationStarted = resolve; });
  let cancellationObserved;
  const cancelled = new Promise((resolve) => { cancellationObserved = resolve; });
  const lm = makeLm(async (_name, _opts, token) => {
    invocationStarted();
    await new Promise((resolve) => token.onCancellationRequested(resolve));
    cancellationObserved(token.isCancellationRequested);
    throw new Error('cancelled by bridge disposal');
  });
  const bridge = await createBridge(lm);
  const request = postBridge(bridge.bridgeUrl, makeRequest(bridge)).catch(() => undefined);

  await started;
  bridge.dispose();

  assert.equal(await cancelled, true);
  await request;
});

test('cancellation: provider resolving after bridge disposal returns bridge_disconnected', async () => {
  let invocationStarted;
  const started = new Promise((resolve) => { invocationStarted = resolve; });
  let resolveProvider;
  const providerResult = new Promise((resolve) => { resolveProvider = resolve; });
  let providerObservedCancellation = false;
  const lm = makeLm(async (_name, _opts, token) => {
    token.onCancellationRequested(() => { providerObservedCancellation = true; });
    invocationStarted();
    return providerResult;
  });
  const bridge = await createBridge(lm);
  const request = postBridge(bridge.bridgeUrl, makeRequest(bridge));

  await started;
  bridge.dispose();
  assert.equal(providerObservedCancellation, true, 'bridge disposal must cancel the provider token first');
  resolveProvider(makeIcmResult());

  const { status, body } = await request;
  assert.equal(status, 503, 'a disconnected bridge must never return HTTP 200 success');
  assert.equal(body.error?.code, 'bridge_disconnected');
  assert.equal(body.result, undefined);
});

test('cancellation: provider rejecting after bridge disposal returns bridge_disconnected', async () => {
  let invocationStarted;
  const started = new Promise((resolve) => { invocationStarted = resolve; });
  let rejectProvider;
  const providerResult = new Promise((_resolve, reject) => { rejectProvider = reject; });
  let providerObservedCancellation = false;
  const lm = makeLm(async (_name, _opts, token) => {
    token.onCancellationRequested(() => { providerObservedCancellation = true; });
    invocationStarted();
    return providerResult;
  });
  const bridge = await createBridge(lm);
  const requestPayload = makeRequest(bridge);
  const request = postBridge(bridge.bridgeUrl, requestPayload);

  await started;
  bridge.dispose();
  assert.equal(providerObservedCancellation, true, 'bridge disposal must cancel the provider token first');
  rejectProvider(new Error('cancelled by bridge disposal'));

  const { status, body } = await request;
  assert.equal(status, 503, 'a disconnected bridge must never return HTTP 200 success');
  assert.deepEqual(body, {
    version: 'yawr.vscode-mcp-bridge/v1',
    request_id: requestPayload.request_id,
    error: { code: 'bridge_disconnected', message: 'Bridge is shutting down' },
  });
});

test('cancellation: disposing bridge while reading a request body prevents invokeTool', async () => {
  let invokeCount = 0;
  const bridge = await createBridge(makeLm(async () => {
    invokeCount++;
    return makeIcmResult();
  }));
  const request = new (require('node:events').EventEmitter)();
  let status;
  let responseBody = '';
  const response = {
    headersSent: false,
    writeHead(nextStatus) {
      status = nextStatus;
      this.headersSent = true;
    },
    end(chunk = '') {
      responseBody += String(chunk);
    },
  };
  const payload = JSON.stringify(makeRequest(bridge));

  const dispatched = bridge.dispatch(request, response);
  request.emit('data', Buffer.from(payload.slice(0, 10)));
  bridge.dispose();
  request.emit('data', Buffer.from(payload.slice(10)));
  request.emit('end');
  await dispatched;

  assert.equal(invokeCount, 0, 'invokeTool must not run after disposal crosses readBody');
  assert.equal(status, 503);
  assert.equal(JSON.parse(responseBody).error.code, 'bridge_disconnected');
});

test('duplicate request_id: invokeTool called exactly once', async (t) => {
  let invokeCount = 0;
  const lm = makeLm(async () => {
    invokeCount++;
    await new Promise((r) => setTimeout(r, 30));
    return makeIcmResult();
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const req = makeRequest(bridge);
  const [r1, r2] = await Promise.all([
    postBridge(bridge.bridgeUrl, req),
    postBridge(bridge.bridgeUrl, req),
  ]);
  assert.equal(invokeCount, 1, 'invokeTool must be called exactly once for duplicate request_id');
  assert.equal(r1.body.request_id, req.request_id);
  assert.equal(r2.body.request_id, req.request_id);
  assert.deepEqual(r1.body.result, r2.body.result);
});

test('duplicate request_id: disposal returns identical HTTP 503 bridge_disconnected responses', async () => {
  let invokeCount = 0;
  let invocationStarted;
  const started = new Promise((resolve) => { invocationStarted = resolve; });
  let resolveProvider;
  const providerResult = new Promise((resolve) => { resolveProvider = resolve; });
  const lm = makeLm(async () => {
    invokeCount++;
    invocationStarted();
    return providerResult;
  });
  const bridge = await createBridge(lm);
  const payload = makeRequest(bridge);

  function startDispatch() {
    const request = new (require('node:events').EventEmitter)();
    let status;
    let responseBody = '';
    const response = {
      headersSent: false,
      writeHead(nextStatus) {
        status = nextStatus;
        this.headersSent = true;
      },
      end(chunk = '') {
        responseBody += String(chunk);
      },
    };
    const dispatched = bridge.dispatch(request, response);
    request.emit('data', Buffer.from(JSON.stringify(payload)));
    request.emit('end');
    return {
      dispatched,
      result: () => ({ status, body: JSON.parse(responseBody) }),
    };
  }

  const original = startDispatch();
  await started;
  const duplicate = startDispatch();
  await Promise.resolve();

  bridge.dispose();
  resolveProvider(makeIcmResult());
  await Promise.all([original.dispatched, duplicate.dispatched]);

  const originalResult = original.result();
  const duplicateResult = duplicate.result();
  assert.equal(invokeCount, 1, 'coalesced callers must share one provider invocation');
  assert.equal(originalResult.status, 503);
  assert.equal(duplicateResult.status, 503);
  assert.equal(originalResult.body.error?.code, 'bridge_disconnected');
  assert.deepEqual(duplicateResult.body, originalResult.body);
});

test('capability rejection: wrong token → HTTP 403 capability_rejected', async (t) => {
  const bridge = await createBridge();
  t.after(() => bridge.dispose());

  const { status, body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    capability_proof: 'wrong-secret-value',
  }));
  assert.equal(status, 403);
  assert.ok(body.error);
  assert.equal(body.error.code, 'capability_rejected');
});

test('version mismatch: wrong version → coded error with our version echoed', async (t) => {
  const bridge = await createBridge();
  t.after(() => bridge.dispose());

  const { status, body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    version: 'vscode-mcp-bridge/v99',
  }));
  assert.equal(status, 400);
  assert.ok(body.error);
  assert.equal(body.error.code, 'version_mismatch');
  assert.equal(body.version, 'yawr.vscode-mcp-bridge/v1');
});

test('malformed request: non-JSON body → coded error', async (t) => {
  const bridge = await createBridge();
  t.after(() => bridge.dispose());

  const { status, body } = await postBridge(bridge.bridgeUrl, 'not json at all');
  assert.equal(status, 400);
  assert.ok(body.error);
  assert.equal(body.error.code, 'malformed_request');
});

test('malformed request: missing request_id → coded error', async (t) => {
  const bridge = await createBridge();
  t.after(() => bridge.dispose());

  const req = makeRequest(bridge);
  delete req.request_id;
  const { body } = await postBridge(bridge.bridgeUrl, req);
  assert.ok(body.error);
  assert.equal(body.error.code, 'malformed_request');
});

test('unregistered tool: tool not in registry → tool_not_found', async (t) => {
  const bridge = await createBridge();
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    tool: 'nonexistent',
    action: 'do-thing',
  }));
  assert.ok(body.error);
  assert.equal(body.error.code, 'tool_not_found');
});

test('tool unavailable in vscode.lm.tools: returns tool_unavailable', async (t) => {
  const lm = makeLm(undefined, []); // no tools registered
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.error);
  assert.equal(body.error.code, 'tool_unavailable');
});

test('redaction sweep: capability secret never appears in any output surface', async (t) => {
  const logLines = [];
  const output = { appendLine: (s) => logLines.push(s) };

  let invokeCount = 0;
  const lm = makeLm(async () => {
    invokeCount++;
    throw new Error('intentional failure');
  });
  const bridge = await McpBridge.create(lm, 0, output, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const secret = bridge.bridgeToken;

  const { body: b1 } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    capability_proof: 'wrong-' + secret,
  }));
  const { body: b2 } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  for (const body of [b1, b2]) {
    const serialized = JSON.stringify(body);
    assert.equal(
      serialized.includes(secret),
      false,
      `capability secret must not appear in response: ${serialized}`,
    );
  }

  for (const line of logLines) {
    assert.equal(
      line.includes(secret),
      false,
      `capability secret must not appear in log: ${line}`,
    );
  }
});

// ─── Mutation-control verification comments ───────────────────────────────────
// The three mutations below are run manually as part of the delivery report.
//
// Mutation 1 — make optional fields strict (treat every declared field as
//   required): in normalizeResult, change `if (!fieldSpec.required)` to always
//   return the "missing" error.  → the "optional-field regression: absent outcome"
//   and "optional field ABSENT → succeeds" test must FAIL.
//
// Mutation 2 — ignore the settings override: in resolveSpec, remove the
//   `overrides?.[key]` branch and always return the raw spec.
//   → the "settings override" tests must FAIL.
//
// Mutation 3 — always use logical-name fallback: in buildRegistryFromDir,
//   always set `registeredName = toolName` (skip action-level and
//   transport-level vscode_tool).  → the precedence tests must FAIL.

// ─── Deliverable D: input schema validation ───────────────────────────────────
// Tests use a fixture modelled on the real ICM MCP tool:
//   mcp_icm_mcp_serve_get_incident_details_by_id
//   inputSchema: { properties: { incidentId: { type: 'integer' } }, required: ['incidentId'] }
//
// Core applies vscode_input mapping before args reach the wire; we validate
// the POST-mapping, POST-coercion args against the live inputSchema here.

const { validateArgsAgainstSchema } = require('../out/mcpBridge');

// ICM MCP tool schema fixture — general mechanism, no ICM-specific code in src/.
const ICM_INPUT_SCHEMA = {
  type: 'object',
  properties: {
    incidentId: { type: 'integer' },
  },
  required: ['incidentId'],
};

test('validateArgsAgainstSchema: valid integer incidentId → undefined (no error)', () => {
  const err = validateArgsAgainstSchema({ incidentId: 12345 }, ICM_INPUT_SCHEMA);
  assert.equal(err, undefined, `expected no error but got: ${err}`);
});

test('validateArgsAgainstSchema: missing required incidentId → error', () => {
  const err = validateArgsAgainstSchema({}, ICM_INPUT_SCHEMA);
  assert.ok(typeof err === 'string', 'expected an error string');
  assert.match(err, /incidentId/, 'error must name the missing parameter');
});

test('validateArgsAgainstSchema: extra parameter not in schema → error', () => {
  const err = validateArgsAgainstSchema({ incidentId: 1, extraParam: 'x' }, ICM_INPUT_SCHEMA);
  assert.ok(typeof err === 'string');
  assert.match(err, /extraParam/);
});

test('validateArgsAgainstSchema: incidentId as string (not integer) → type error', () => {
  const err = validateArgsAgainstSchema({ incidentId: '12345' }, ICM_INPUT_SCHEMA);
  assert.ok(typeof err === 'string');
  assert.match(err, /incidentId/);
  assert.match(err, /integer/);
});

test('validateArgsAgainstSchema: incidentId as float (not integer) → type error', () => {
  const err = validateArgsAgainstSchema({ incidentId: 123.5 }, ICM_INPUT_SCHEMA);
  assert.ok(typeof err === 'string');
  assert.match(err, /incidentId/);
});

test('validateArgsAgainstSchema: no schema → bridge proceeds without validation', async (t) => {
  // When a tool has no inputSchema the bridge must NOT block the request.
  const lm = makeLm(async (_name, opts) => {
    return makeIcmResult();
  }, [{ name: 'icm-get-incident' /* no inputSchema */ }]);
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.equal(body.error, undefined, `unexpected error: ${JSON.stringify(body.error)}`);
  assert.ok(body.result);
});

test('input_validation_error: missing required param → HTTP 200 with coded error', async (t) => {
  // Bridge returns HTTP 200 with a structured error body (not 4xx) because
  // the protocol error is at the MCP layer, not the HTTP layer.
  const ICM_SCHEMA_REGISTRY = {
    'icm/get-incident': {
      registeredName: 'icm-get-incident',
      outputFields: {
        title:          { type: 'string', required: true },
        service:        { type: 'string', required: true },
        environment:    { type: 'string', required: true },
        logical_server: { type: 'string', required: true },
        database:       { type: 'string', required: true },
      },
    },
  };
  const lm = makeLm(undefined, [{
    name: 'icm-get-incident',
    inputSchema: {
      type: 'object',
      properties: { incidentId: { type: 'integer' } },
      required: ['incidentId'],
    },
  }]);
  const bridge = await McpBridge.create(lm, 0, undefined, { registry: ICM_SCHEMA_REGISTRY });
  t.after(() => bridge.dispose());

  // Send args that are missing incidentId (provider name after coercion).
  const { status, body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    args: { incident_id: 12345 }, // wrong name — provider expects incidentId
  }));
  assert.equal(status, 200);
  assert.ok(body.error, 'expected an error body');
  assert.equal(body.error.code, 'input_validation_error');
  assert.match(body.error.message, /incidentId/, 'error must name the missing parameter');
  assert.equal(body.result, undefined, 'result must be absent on validation error');
});

test('input_validation_error: extra parameter → coded error, invokeTool not called', async (t) => {
  let invokeCalled = false;
  const lm = makeLm(async () => {
    invokeCalled = true;
    return makeIcmResult();
  }, [{
    name: 'icm-get-incident',
    inputSchema: {
      type: 'object',
      properties: { incidentId: { type: 'integer' } },
      required: ['incidentId'],
    },
  }]);
  const ICM_SCHEMA_REGISTRY = {
    'icm/get-incident': {
      registeredName: 'icm-get-incident',
      outputFields: {
        title:          { type: 'string', required: true },
        service:        { type: 'string', required: true },
        environment:    { type: 'string', required: true },
        logical_server: { type: 'string', required: true },
        database:       { type: 'string', required: true },
      },
    },
  };
  const bridge = await McpBridge.create(lm, 0, undefined, { registry: ICM_SCHEMA_REGISTRY });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    args: { incidentId: 1, rogue: 'hax' },
  }));
  assert.ok(body.error);
  assert.equal(body.error.code, 'input_validation_error');
  assert.match(body.error.message, /rogue/);
  assert.equal(invokeCalled, false, 'invokeTool must not be called when args fail schema validation');
});

test('input_validation_error: type mismatch → coded error', async (t) => {
  const lm = makeLm(undefined, [{
    name: 'icm-get-incident',
    inputSchema: {
      type: 'object',
      properties: { incidentId: { type: 'integer' } },
      required: ['incidentId'],
    },
  }]);
  const ICM_SCHEMA_REGISTRY = {
    'icm/get-incident': {
      registeredName: 'icm-get-incident',
      outputFields: {
        title:          { type: 'string', required: true },
        service:        { type: 'string', required: true },
        environment:    { type: 'string', required: true },
        logical_server: { type: 'string', required: true },
        database:       { type: 'string', required: true },
      },
    },
  };
  const bridge = await McpBridge.create(lm, 0, undefined, { registry: ICM_SCHEMA_REGISTRY });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    args: { incidentId: 'INC-123' }, // string, not integer
  }));
  assert.ok(body.error);
  assert.equal(body.error.code, 'input_validation_error');
  assert.match(body.error.message, /incidentId/);
});

test('input validation: no secret in input_validation_error message', async (t) => {
  const lm = makeLm(undefined, [{
    name: 'icm-get-incident',
    inputSchema: {
      type: 'object',
      properties: { incidentId: { type: 'integer' } },
      required: ['incidentId'],
    },
  }]);
  const ICM_SCHEMA_REGISTRY = {
    'icm/get-incident': {
      registeredName: 'icm-get-incident',
      outputFields: { title: { type: 'string', required: true }, service: { type: 'string', required: true }, environment: { type: 'string', required: true }, logical_server: { type: 'string', required: true }, database: { type: 'string', required: true } },
    },
  };
  const bridge = await McpBridge.create(lm, 0, undefined, { registry: ICM_SCHEMA_REGISTRY });
  t.after(() => bridge.dispose());

  const secret = bridge.bridgeToken;
  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, { args: {} }));
  assert.equal(body.error?.code, 'input_validation_error');
  assert.equal(JSON.stringify(body).includes(secret), false,
    'capability secret must not appear in error message');
});

// ─── Deliverable E: invocation error classification ───────────────────────────
// Allowlisted category set:
//   invocation_token_unavailable — VS Code requires toolInvocationToken;
//     calling with undefined triggers this (bridge runs outside chat handler).
//   authorization_unavailable    — auth/credential/forbidden provider failure.
//   provider_input_rejected      — provider's own input-validation failure.
//   invocation_error             — unrecognised or ambiguous exception (fallback).

const { classifyInvocationError } = require('../out/mcpBridge');

test('classifier: invocation token missing → invocation_token_unavailable', () => {
  assert.equal(classifyInvocationError(new Error('No toolInvocationToken provided')),
    'invocation_token_unavailable');
  assert.equal(classifyInvocationError(new Error('invocation token is required')),
    'invocation_token_unavailable');
  assert.equal(classifyInvocationError(new Error('toolInvocationToken must not be undefined')),
    'invocation_token_unavailable');
});

test('classifier: auth/credential error → authorization_unavailable', () => {
  assert.equal(classifyInvocationError(new Error('Unauthorized: credential not found')),
    'authorization_unavailable');
  assert.equal(classifyInvocationError(new Error('Forbidden: access denied')),
    'authorization_unavailable');
  assert.equal(classifyInvocationError(new Error('auth challenge failed')),
    'authorization_unavailable');
});

test('classifier: provider input rejected → provider_input_rejected', () => {
  assert.equal(classifyInvocationError(new Error('invalid input: schema mismatch')),
    'provider_input_rejected');
  assert.equal(classifyInvocationError(new Error('input_invalid: missing field')),
    'provider_input_rejected');
  assert.equal(classifyInvocationError(new Error('bad request: invalid param')),
    'provider_input_rejected');
  assert.equal(classifyInvocationError(new Error('validation_error from provider')),
    'provider_input_rejected');
});

test('classifier: unrecognised exception → invocation_error (conservative fallback)', () => {
  assert.equal(classifyInvocationError(new Error('something totally unexpected happened')),
    'invocation_error');
  assert.equal(classifyInvocationError(new Error('ECONN_RESET')),
    'invocation_error');
  assert.equal(classifyInvocationError('a plain string error'),
    'invocation_error');
  assert.equal(classifyInvocationError(null),
    'invocation_error');
});

test('bridge: invocation_token_unavailable is returned with safe message', async (t) => {
  const lm = makeLm(async () => {
    throw new Error('No toolInvocationToken provided for this invocation');
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.error);
  assert.equal(body.error.code, 'invocation_token_unavailable');
  assert.equal(body.result, undefined);
});

test('bridge: Canceled with cached token fails closed and never retries without token', async (t) => {
  const logLines = [];
  const output = { appendLine: (s) => logLines.push(s) };
  const tokenValues = [];
  let invokeCount = 0;
  let rejectionCount = 0;
  const cachedToken = 'cached-token-for-canceled-regression';

  const lm = {
    tools: [{ name: 'icm-get-incident' }],
    getToolInvocationToken: () => cachedToken,
    onTokenRejected: () => { rejectionCount++; },
    invokeTool: async (_name, opts) => {
      invokeCount++;
      tokenValues.push(opts.toolInvocationToken);
      throw new Error('Canceled');
    },
  };
  const bridge = await McpBridge.create(lm, 0, output, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));

  assert.equal(invokeCount, 1, 'old unsafe behavior made two calls; fail closed after the first Canceled');
  assert.deepEqual(tokenValues, [cachedToken],
    'the bridge must not make a second invokeTool call with toolInvocationToken undefined');
  assert.equal(rejectionCount, 1, 'Canceled token path must clear/reject the cached token once');
  assert.ok(body.error, 'Canceled token path must return a coded error');
  assert.equal(body.error.code, 'invocation_token_unavailable');
  assert.equal(body.result, undefined);
  assert.equal(logLines.some((line) => line.includes('retrying without token')), false,
    'logs must not contain the old silent unauthenticated retry message');
});

test('bridge: provider_input_rejected is returned with safe message', async (t) => {
  const lm = makeLm(async () => {
    throw new Error('invalid input: incidentId must be a positive integer');
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.error);
  assert.equal(body.error.code, 'provider_input_rejected');
  assert.equal(body.result, undefined);
});

test('bridge: unrecognised exception → invocation_error (fallback)', async (t) => {
  const lm = makeLm(async () => {
    throw new Error('some completely unknown provider failure XYZZY');
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.error);
  assert.equal(body.error.code, 'invocation_error');
  assert.equal(body.result, undefined);
});

// Regression: existing timeout / cancellation behaviour must still hold.
test('regression: deadline_exceeded still fires on timeout (category tests must not break this)', async (t) => {
  const lm = makeLm(async (_name, _opts, token) => {
    await new Promise((resolve) => {
      token.onCancellationRequested(resolve);
      setTimeout(resolve, 2000);
    });
    throw new Error('cancelled');
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    deadline_unix_ms: Date.now() + 80,
  }));
  assert.ok(body.error);
  assert.equal(body.error.code, 'deadline_exceeded');
});

// ─── Redaction tests ──────────────────────────────────────────────────────────
// Each test throws an error whose message embeds one of the four forbidden
// substrings, then asserts that substring is absent from the full serialized
// response body AND from every output-channel line.
//
// Non-vacuity: each test also asserts the serialized body is non-empty (> 2
// chars) to ensure the redaction scan actually inspects real data.

function assertNoSubstring(haystack, needle, label) {
  assert.ok(haystack.length > 2, `${label}: scanned string must be non-empty (got: "${haystack}")`);
  assert.equal(
    haystack.includes(needle), false,
    `${label}: forbidden substring found — "${needle.slice(0, 20)}…" appears in: ${haystack.slice(0, 120)}`,
  );
}

test('redaction: bearer token in provider error must not appear in response or log', async (t) => {
  const BEARER = 'Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.fakepayload.fakesig';
  const logLines = [];
  const output = { appendLine: (s) => logLines.push(s) };
  const lm = makeLm(async () => { throw new Error(`Provider rejected: ${BEARER}`); });
  const bridge = await McpBridge.create(lm, 0, output, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  const serialized = JSON.stringify(body);
  assertNoSubstring(serialized, BEARER, 'response body');
  for (const line of logLines) assertNoSubstring(line, BEARER, 'output channel');
});

test('redaction: capability secret in provider error must not appear in response or log', async (t) => {
  const logLines = [];
  const output = { appendLine: (s) => logLines.push(s) };
  // We don't know the secret yet; we'll inject it into the error after getting it.
  let secret;
  const lm = makeLm(async () => { throw new Error(`token=${secret} rejected`); });
  const bridge = await McpBridge.create(lm, 0, output, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());
  secret = bridge.bridgeToken;

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  const serialized = JSON.stringify(body);
  assertNoSubstring(serialized, secret, 'response body (secret)');
  for (const line of logLines) assertNoSubstring(line, secret, 'output channel (secret)');
});

test('redaction: tool arguments in provider error must not appear in response or log', async (t) => {
  const SECRET_ARG_VALUE = 'SUPER_SECRET_INCIDENT_ARG_7f3k9x';
  const logLines = [];
  const output = { appendLine: (s) => logLines.push(s) };
  const lm = makeLm(async (_name, opts) => {
    throw new Error(`Provider saw args: ${JSON.stringify(opts.input)} and rejected them because ${SECRET_ARG_VALUE}`);
  });
  const bridge = await McpBridge.create(lm, 0, output, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge, {
    args: { incident_id: SECRET_ARG_VALUE },
  }));
  const serialized = JSON.stringify(body);
  assertNoSubstring(serialized, SECRET_ARG_VALUE, 'response body (args)');
  for (const line of logLines) assertNoSubstring(line, SECRET_ARG_VALUE, 'output channel (args)');
});

test('redaction: provider result content in provider error must not appear in response or log', async (t) => {
  const SECRET_RESULT = 'CONFIDENTIAL_RESULT_CONTENT_9z8y7x6w';
  const logLines = [];
  const output = { appendLine: (s) => logLines.push(s) };
  const lm = makeLm(async () => {
    throw new Error(`Partial result leaked: ${SECRET_RESULT}`);
  });
  const bridge = await McpBridge.create(lm, 0, output, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  const serialized = JSON.stringify(body);
  assertNoSubstring(serialized, SECRET_RESULT, 'response body (result content)');
  for (const line of logLines) assertNoSubstring(line, SECRET_RESULT, 'output channel (result content)');
});

// Non-vacuity control: confirm the redaction scan fails when the secret IS present.
// This test deliberately injects a known string into the response check to prove
// the scanner is not scanning an empty string.
test('redaction non-vacuity: scanner detects injected secret in a constructed response', () => {
  const INJECTED = 'INJECTED_SECRET_CANARY_abc123';
  const fakeBody = { error: { code: 'invocation_error', message: `leaked: ${INJECTED}` } };
  const serialized = JSON.stringify(fakeBody);
  // The serialized body is non-empty.
  assert.ok(serialized.length > 2, 'non-vacuity: body must be non-empty');
  // The scanner DOES find the injected secret (proves the scan works).
  assert.equal(serialized.includes(INJECTED), true,
    'non-vacuity: injected secret must be detectable in a constructed body — if this fails the scanner is broken');
});

// ─── Deliverable F: provider_unavailable classification ───────────────────────
// Tests for the new provider_unavailable category and extractProviderHint.
// Fixtures use the EXACT live strings captured from Cristiano's diagnostic probe.

const { extractProviderHint } = require('../out/mcpBridge');

// ── F1: live strings classify correctly ──────────────────────────────────────

test('classifier: live fixture T1 → provider_unavailable', () => {
  // Exact T1 live string from the probe
  assert.equal(
    classifyInvocationError(new Error('MCP server has stopped')),
    'provider_unavailable',
  );
});

test('classifier: live fixture T2/T3/T4 → provider_unavailable', () => {
  // Exact T2/T3/T4 live string from the probe
  assert.equal(
    classifyInvocationError(new Error(
      'MCP server could not be started: 401 status sending message to https://icm-mcp-prod.azure-api.net/v1/:'
    )),
    'provider_unavailable',
  );
});

test('classifier: server-unavailable phrasings → provider_unavailable', () => {
  assert.equal(classifyInvocationError(new Error('MCP server is not running')),     'provider_unavailable');
  assert.equal(classifyInvocationError(new Error('MCP server unavailable')),         'provider_unavailable');
  assert.equal(classifyInvocationError(new Error('server not running at all')),      'provider_unavailable');
  assert.equal(classifyInvocationError(new Error('server unavailable: timeout')),    'provider_unavailable');
});

// ── F2: startup-401 precedence: provider_unavailable, NOT authorization_unavailable ──

test('classifier: startup 401 → provider_unavailable (not authorization_unavailable)', () => {
  // A message that contains BOTH "MCP server could not be started" AND "Unauthorized".
  // provider_unavailable must win because it is checked first.
  assert.equal(
    classifyInvocationError(new Error(
      'MCP server could not be started: 401 Unauthorized sending message to https://example.com/'
    )),
    'provider_unavailable',
    'startup 401 must classify as provider_unavailable, not authorization_unavailable',
  );
});

// ── F3: unrecognised error still falls back ───────────────────────────────────

test('classifier: unrecognised error → invocation_error fallback (unchanged)', () => {
  assert.equal(classifyInvocationError(new Error('socket hang up')), 'invocation_error');
});

// ── F4: bridge returns provider_unavailable with actionable guidance ──────────

test('bridge: provider_unavailable category with hint and remediation in message', async (t) => {
  const lm = makeLm(async () => {
    throw new Error('MCP server could not be started: 401 status sending message to https://icm-mcp-prod.azure-api.net/v1/:');
  });
  const bridge = await createBridge(lm);
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  assert.ok(body.error, 'error must be present');
  assert.equal(body.error.code, 'provider_unavailable');
  // Message must name the remediation action.
  assert.ok(body.error.message.includes('MCP: List Servers'),
    `message must mention "MCP: List Servers"; got: ${body.error.message}`);
  assert.ok(body.error.message.includes('re-authenticate'),
    `message must mention "re-authenticate"; got: ${body.error.message}`);
});

// ── F5: extractProviderHint builds from allowlist only ────────────────────────

test('extractProviderHint: live fixture T1 → phrase only', () => {
  const hint = extractProviderHint(new Error('MCP server has stopped'));
  assert.ok(hint !== null, 'hint must not be null');
  assert.ok(hint.includes('MCP server has stopped'), 'hint must include the matched phrase');
  assert.ok(hint.length <= 200, 'hint must not exceed 200 chars');
});

test('extractProviderHint: live fixture T2 → phrase + status + origin', () => {
  const hint = extractProviderHint(new Error(
    'MCP server could not be started: 401 status sending message to https://icm-mcp-prod.azure-api.net/v1/:'
  ));
  assert.ok(hint !== null, 'hint must not be null');
  assert.ok(hint.includes('MCP server could not be started'), 'hint must include the matched phrase');
  assert.ok(hint.includes('HTTP 401'),     'hint must include HTTP status');
  assert.ok(hint.includes('https://icm-mcp-prod.azure-api.net'), 'hint must include URL origin');
  assert.ok(!hint.includes('/v1/'),        'hint must NOT include URL path');
  assert.ok(!hint.includes('sending'),     'hint must NOT include arbitrary provider text');
  assert.ok(hint.length <= 200,            'hint must not exceed 200 chars');
});

test('extractProviderHint: unrecognised error → null (no arbitrary text leaks)', () => {
  const SECRET_PROVIDER_TEXT = 'PROVIDER_FREE_TEXT_SENTINEL_xq9z7w4v';
  const hint = extractProviderHint(new Error(`some unknown failure: ${SECRET_PROVIDER_TEXT}`));
  assert.equal(hint, null, 'hint must be null for unrecognised error');
});

// ── F6: hint length cap enforced ─────────────────────────────────────────────

test('extractProviderHint: cap at 200 chars even when all three pieces are present', () => {
  // Build an error whose URL host is pathologically long.
  const longHost = 'a'.repeat(200) + '.example.com';
  const err = new Error(`MCP server has stopped at https://${longHost}/v1/foo`);
  const hint = extractProviderHint(err);
  assert.ok(hint !== null, 'hint must not be null');
  assert.ok(hint.length <= 200, `hint must not exceed 200 chars; got ${hint.length}`);
});

// ── F7: redaction sentinel tests — no forbidden content in hint ───────────────
// Each test embeds a forbidden sentinel in the error message and asserts the hint
// contains NONE of it.  Tests run through the REAL extractProviderHint production
// path so that a mutation to that function can be caught.
//
// Non-vacuity: each test also asserts the hint is non-null (meaning extractProviderHint
// actually ran and returned something), OR asserts null when the whole error is unrecognised.

test('extractProviderHint: bearer token in error message does not appear in hint', () => {
  const SENTINEL_BEARER = 'BEARER_TOKEN_SENTINEL_5e9r2t7y';
  const hint = extractProviderHint(new Error(
    `MCP server has stopped; Authorization: Bearer ${SENTINEL_BEARER}`
  ));
  // hint should be non-null (phrase matched), but the bearer value must not appear.
  assert.ok(hint !== null, 'non-vacuity: extractProviderHint returned null unexpectedly');
  assert.equal(hint.includes(SENTINEL_BEARER), false,
    `Bearer token sentinel must not appear in hint. Hint: ${hint}`);
});

test('extractProviderHint: capability secret does not appear in hint', () => {
  const SENTINEL_SECRET = 'CAPABILITY_SECRET_SENTINEL_3f8h1j6k';
  const hint = extractProviderHint(new Error(
    `MCP server could not be started; capability=${SENTINEL_SECRET}`
  ));
  assert.ok(hint !== null, 'non-vacuity: extractProviderHint returned null unexpectedly');
  assert.equal(hint.includes(SENTINEL_SECRET), false,
    `Capability secret sentinel must not appear in hint. Hint: ${hint}`);
});

test('extractProviderHint: tool arguments do not appear in hint', () => {
  const SENTINEL_ARG = 'TOOL_ARG_SENTINEL_9n4m7p2q';
  const hint = extractProviderHint(new Error(
    `MCP server has stopped; args={"incidentId":"${SENTINEL_ARG}"}`
  ));
  assert.ok(hint !== null, 'non-vacuity: extractProviderHint returned null unexpectedly');
  assert.equal(hint.includes(SENTINEL_ARG), false,
    `Tool arg sentinel must not appear in hint. Hint: ${hint}`);
});

test('extractProviderHint: URL path and query do not appear in hint', () => {
  const SENTINEL_PATH = '/v1/secret-path';
  const SENTINEL_QUERY = 'token=SENTINEL_QUERY_TOKEN_6x3z8w1v';
  const hint = extractProviderHint(new Error(
    `MCP server has stopped; url=https://example.com${SENTINEL_PATH}?${SENTINEL_QUERY}`
  ));
  assert.ok(hint !== null, 'non-vacuity: extractProviderHint returned null unexpectedly');
  assert.equal(hint.includes(SENTINEL_PATH), false,
    `URL path sentinel must not appear in hint. Hint: ${hint}`);
  assert.equal(hint.includes(SENTINEL_QUERY), false,
    `URL query sentinel must not appear in hint. Hint: ${hint}`);
  // But the origin (example.com) IS allowed.
  assert.ok(hint.includes('https://example.com'),
    `URL origin must appear in hint. Hint: ${hint}`);
});

test('extractProviderHint: result content does not appear in hint', () => {
  const SENTINEL_RESULT = 'RESULT_CONTENT_SENTINEL_2p7q4r9s';
  // Error whose message has neither an allowlisted phrase, a 4xx/5xx code, nor a URL:
  const hint = extractProviderHint(new Error(`partial result: ${SENTINEL_RESULT}`));
  // Unrecognised → null (cannot pass text through).
  assert.equal(hint, null, 'Unrecognised error must return null hint, not arbitrary content');
});

// ── F8: bridge: provider_unavailable redaction through full HTTP path ─────────
// Verifies the full bridge code path doesn't leak provider text even for
// provider_unavailable, using a sentinel embedded in the provider error message.

test('bridge: provider_unavailable path: provider free text does not appear in response body', async (t) => {
  const SENTINEL = 'PROVIDER_FREE_TEXT_FULL_PATH_SENTINEL_1a2b3c';
  const logLines = [];
  const output = { appendLine: (s) => logLines.push(s) };
  const lm = makeLm(async () => {
    throw new Error(`MCP server has stopped: internal reason: ${SENTINEL}`);
  });
  const bridge = await McpBridge.create(lm, 0, output, { registry: fixtureRegistry });
  t.after(() => bridge.dispose());

  const { body } = await postBridge(bridge.bridgeUrl, makeRequest(bridge));
  const serialized = JSON.stringify(body);
  assertNoSubstring(serialized, SENTINEL, 'response body (provider_unavailable full path)');
  for (const line of logLines) assertNoSubstring(line, SENTINEL, 'output channel (provider_unavailable full path)');
  // Code must be provider_unavailable (non-vacuity: we actually hit the right path).
  assert.equal(body.error?.code, 'provider_unavailable',
    'non-vacuity: error code must be provider_unavailable');
});
