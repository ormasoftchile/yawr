// activeProjectRegistry.test.js — load-bearing tests for deliverable C.
//
// Verifies that the bridge registry is derived from the ACTIVE RUNBOOK's
// resolved project, not from workspaceFolders[0] or directory traversal order.
//
// Two fixture projects (multi-root-a and multi-root-b) both declare
// "shared-tool/run" but with different registered MCP tool names:
//   root-a → "tool-from-root-a"
//   root-b → "tool-from-root-b"
//
// The load-bearing property: given a runbook inside root-b, the correct
// registered name is "tool-from-root-b" regardless of which folder appears
// first in the workspace-folders list. If the code fell back to traversal
// order and root-a appeared first, it would return "tool-from-root-a" and
// the assertions below would fail.

'use strict';

const assert = require('node:assert/strict');
const path = require('node:path');
const test = require('node:test');

const { buildRegistryFromDir } = require('../out/toolDefinitionRegistry');
const { pickProjectRoot } = require('../out/projectRoot');
const { McpBridge } = require('../out/mcpBridge');

const ROOT_A = path.join(__dirname, 'fixtures', 'multi-root-a');
const ROOT_B = path.join(__dirname, 'fixtures', 'multi-root-b');
// Runbook physically inside root-b's runbooks dir.
const RUNBOOK_IN_B = path.join(ROOT_B, 'runbooks', 'test.runbook.yaml');

// ─── pickProjectRoot selects the active runbook's project ────────────────────

test('pickProjectRoot: runbook in root-b selects root-b even when root-a is first workspace folder', () => {
  // Workspace folders: [root-a, root-b] — root-a is first, which is the wrong choice.
  const selected = pickProjectRoot(RUNBOOK_IN_B, [ROOT_A, ROOT_B], ROOT_A);
  assert.equal(selected, ROOT_B,
    'expected root-b because the runbook lives there, not the first workspace folder');
});

test('pickProjectRoot: folder order is irrelevant — root-b is still chosen when [root-b, root-a]', () => {
  // Reversed folder order: root-b first, root-a second.
  // This test WOULD pass for the wrong reason if the code picked the first
  // folder (it would accidentally pick root-b). Combining both ordering tests
  // makes the suite load-bearing: it must pass for BOTH orderings, and the
  // only way to satisfy both is to use the runbook path, not folder order.
  const selected = pickProjectRoot(RUNBOOK_IN_B, [ROOT_B, ROOT_A], ROOT_A);
  assert.equal(selected, ROOT_B,
    'root-b must still be chosen when folder list is reversed — must not be traversal-order-dependent');
});

// ─── Registry reflects the active project, not the traversal winner ──────────

test('registry from root-b has tool-from-root-b, not tool-from-root-a', () => {
  const registry = buildRegistryFromDir(ROOT_B);
  const spec = registry['shared-tool/run'];
  assert.ok(spec, 'shared-tool/run must be in the root-b registry');
  assert.equal(spec.registeredName, 'tool-from-root-b',
    'tool-from-root-b must win — not tool-from-root-a from the other folder');
});

test('registry from root-a has tool-from-root-a (control check)', () => {
  const registry = buildRegistryFromDir(ROOT_A);
  const spec = registry['shared-tool/run'];
  assert.ok(spec, 'shared-tool/run must be in the root-a registry');
  assert.equal(spec.registeredName, 'tool-from-root-a');
});

// ─── End-to-end: active-project selection feeds correct name into bridge ──────

test('end-to-end: runbook-in-b chain yields tool-from-root-b in the bridge registry', () => {
  // This is the integration path: pickProjectRoot → buildRegistryFromDir → registry.
  // If any step used root-a (workspace[0]) instead of root-b, registeredName would be wrong.
  const projectRoot = pickProjectRoot(RUNBOOK_IN_B, [ROOT_A, ROOT_B], ROOT_A);
  const registry = buildRegistryFromDir(projectRoot);
  const spec = registry['shared-tool/run'];
  assert.ok(spec, 'shared-tool/run must be found after the full chain');
  assert.equal(spec.registeredName, 'tool-from-root-b',
    'full chain must produce root-b\'s binding — tool-from-root-a means workspaceFolders[0] was used');
});

// ─── McpBridge.updateRegistry: live registry switch ─────────────────────────

test('McpBridge.updateRegistry: switching registry changes dispatch target', async (t) => {
  // Registry A maps shared-tool/run → tool-from-root-a.
  // Registry B maps shared-tool/run → tool-from-root-b.
  // The test verifies that after updateRegistry(B), a new request routes to
  // tool-from-root-b, not tool-from-root-a.
  const registryA = buildRegistryFromDir(ROOT_A);
  const registryB = buildRegistryFromDir(ROOT_B);

  const callLog = [];
  // Both tools are present in vscode.lm.tools; result is an empty object
  // (no required outputFields in the fixture).
  const lm = {
    tools: [
      { name: 'tool-from-root-a' },
      { name: 'tool-from-root-b' },
    ],
    getToolInvocationToken: () => 'test-invocation-token',
    invokeTool: async (name) => {
      callLog.push(name);
      return { content: [{ value: '{"result":"ok"}' }] };
    },
  };

  const bridge = await McpBridge.create(lm, 0, undefined, { registry: registryA });
  t.after(() => bridge.dispose());

  // Helper: POST one request to the bridge.
  const http = require('node:http');
  async function post(payload) {
    return new Promise((resolve, reject) => {
      const data = JSON.stringify(payload);
      const u = new URL(bridge.bridgeUrl);
      const req = http.request(
        { hostname: u.hostname, port: Number(u.port), path: '/', method: 'POST',
          headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) } },
        (res) => {
          const chunks = [];
          res.on('data', (c) => chunks.push(c));
          res.on('end', () => resolve(JSON.parse(Buffer.concat(chunks).toString())));
        },
      );
      req.on('error', reject);
      req.end(data);
    });
  }

  const base = {
    version: 'yawr.vscode-mcp-bridge/v1',
    tool: 'shared-tool',
    action: 'run',
    args: {},
    capability_proof: bridge.bridgeToken,
  };

  // Request 1: registry A — expects tool-from-root-a.
  const r1 = await post({ ...base, request_id: 'req-a-1' });
  assert.equal(r1.error, undefined, `unexpected error with registry A: ${JSON.stringify(r1.error)}`);
  assert.equal(callLog.at(-1), 'tool-from-root-a',
    'with registry A, dispatch must use tool-from-root-a');

  // Switch to registry B (simulates user switching to runbook-in-b).
  bridge.updateRegistry(registryB);

  // Request 2: registry B — expects tool-from-root-b.
  const r2 = await post({ ...base, request_id: 'req-b-1' });
  assert.equal(r2.error, undefined, `unexpected error with registry B: ${JSON.stringify(r2.error)}`);
  assert.equal(callLog.at(-1), 'tool-from-root-b',
    'after updateRegistry(B), dispatch must use tool-from-root-b');

  // Confirm dispatching root-a first was not a coincidence: the pre-switch
  // call used tool-from-root-a and the post-switch call used tool-from-root-b.
  assert.deepEqual(callLog, ['tool-from-root-a', 'tool-from-root-b'],
    'call sequence must exactly match the registry switch');
});
