const test = require('node:test');
const assert = require('node:assert/strict');
const { AuthoringClient } = require('../out/authoringClient');
const { sourceDigest } = require('../out/authoringProtocol');
const wire = require('../out/presentationClient');

const caps = {
  schema_version: 'authoring-capabilities/v3', resolver_version: 'core-authoring/v3',
  grammar_version: 'yawr-expression/v2', operations: ['complete', 'signature', 'required-arguments'],
  discovery_scope: 'explicit-local-catalog', max_bytes: 8388608, max_overlays: 128, max_items: 4096,
  max_value_code_units: 32768, max_depth: 128, capabilities: ['yawr.typed-results/v1'],
  capture_roots: ['outputs'], typed_operations: [
    { kind: 'assign', role: 'technical', terminal: false },
    { kind: 'results', title: 'Results', role: 'operator', terminal: true },
  ],
};
const request = () => ({
  schema_version: 'authoring-request/v3', request_id: 'current', operation: 'complete',
  context: { project_root: 'C:\\fixture', generation: 1 },
  document: { uri: 'file:///C:/fixture/runbook.yaml', path: 'C:\\fixture\\runbook.yaml', text: '', version: 1 },
  position: 0, overlays: [],
});
const reply = r => ({
  schema_version: 'authoring-reply/v3', resolver_version: 'core-authoring/v3',
  grammar_version: 'yawr-expression/v2', request_id: r.request_id, operation: r.operation, context: r.context,
  document: { uri: r.document.uri, version: r.document.version, digest: sourceDigest(r.document.text) },
  status: 'resolved', discovery: { scope: 'explicit-local-catalog', status: 'not-needed' },
  dependencies: [], site: null, items: [], signature: null, required_edit: null,
});

function identity(t) {
  t.mock.method(require('node:fs/promises'), 'stat', async () => ({ dev: 1, ino: 1, size: 1, mtimeMs: 1, ctimeMs: 1 }));
}

test('client probes only current authoring capabilities and sends v3', async t => {
  identity(t);
  const calls = [];
  t.mock.method(wire, 'finiteHelper', async (_binary, args, input) => {
    calls.push(args);
    if (args[1] === 'capabilities') return caps;
    const req = JSON.parse(input);
    assert.equal(req.schema_version, 'authoring-request/v3');
    return reply(req);
  });
  const client = new AuthoringClient();
  try {
    await client.resolve('helper', request());
    await client.resolve('helper', request());
    assert.deepEqual(calls[0], ['authoring', 'capabilities', '--v3']);
    assert.equal(calls.filter(c => c[1] === 'capabilities').length, 1);
  } finally { client.dispose(); }
});

test('unsupported current capabilities fail without downgrade', async t => {
  identity(t);
  const calls = [];
  t.mock.method(wire, 'finiteHelper', async (_binary, args) => {
    calls.push(args);
    throw new Error('unsupported-authoring-v3');
  });
  const client = new AuthoringClient();
  try {
    await assert.rejects(client.resolve('helper', request()), /unsupported-authoring-v3/);
    assert.deepEqual(calls, [['authoring', 'capabilities', '--v3']]);
  } finally { client.dispose(); }
});

test('malformed v3 capabilities are rejected', async t => {
  identity(t);
  t.mock.method(wire, 'finiteHelper', async () => ({ ...caps, schema_version: 'authoring-capabilities/v2' }));
  const client = new AuthoringClient();
  try {
    await assert.rejects(client.resolve('helper', request()), /invalid-authoring-response/);
  } finally { client.dispose(); }
});
