const test = require('node:test');
const assert = require('node:assert/strict');
const { decodeCapabilities } = require('../out/presentationProtocol');
const { buildStdioRunArgs } = require('../out/directRunSession');

function capabilities() {
  return {
    schema_version: 'presentation-capabilities/v3',
    resolver_version: 'core-binding/v3',
    execution_plan_read: ['execution-plan/v3', 'execution-plan/v4'],
    execution_plan_write: ['execution-plan/v3', 'execution-plan/v4'],
    capabilities: ['yawr.typed-results/v1', 'yawr.run-results-chunks/v1', 'yawr.lexical-tool-scopes/v1'],
    graph_read: ['1', '3'],
    authoring_request: 'authoring-request/v3',
    expression_request: 'yawr.expression-resolve/v1',
    stdio_version: 'yawr.stdio/v1',
    stdio_frame_bytes: 1048576,
    results_chunk_bytes: 65536,
    results_max_bytes: 268435456,
  };
}

test('matching runtime advertises scoped plans and explicit legacy snapshot support', () => {
  assert.doesNotThrow(() => decodeCapabilities(capabilities(), 3));
});

test('old helpers produce an actionable lexical-scope compatibility error', () => {
  const old = capabilities();
  old.execution_plan_read = ['execution-plan/v3'];
  old.execution_plan_write = ['execution-plan/v3'];
  assert.throws(() => decodeCapabilities(old, 3), /install the matching YAWR runtime/);
  const missingFeature = capabilities();
  missingFeature.capabilities.pop();
  assert.throws(() => decodeCapabilities(missingFeature, 3), /lexical tool scope support/);
});

test('every direct run requires lexical scopes before opening runtime sources', () => {
  for (const typedResults of [false, true]) {
    const args = buildStdioRunArgs('root.runbook.yaml', {}, undefined, false, new Set(), undefined, typedResults, true);
    assert.ok(args[args.indexOf('--require-capabilities') + 1].split(',').includes('yawr.lexical-tool-scopes/v1'));
  }
});
