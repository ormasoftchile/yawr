const test = require('node:test');
const assert = require('node:assert/strict');
const { decodeAuthoringReply, sourceDigest } = require('../out/authoringProtocol');

test('current authoring contract accepts include completion kinds', () => {
  const request = {
    schema_version: 'authoring-request/v3', request_id: 'include', operation: 'complete',
    context: { project_root: 'C:\\fixture', generation: 1 },
    document: { uri: 'file:///C:/fixture/runbook.yaml', path: 'C:\\fixture\\runbook.yaml', text: 'include:\n  ', version: 1 },
    overlays: [], position: 11,
  };
  const range = { start: 11, end: 11 };
  const reply = {
    schema_version: 'authoring-reply/v3', resolver_version: 'core-authoring/v3',
    grammar_version: 'yawr-expression/v2', operation: 'complete', request_id: 'include',
    context: request.context, document: { uri: request.document.uri, version: 1, digest: sourceDigest(request.document.text) },
    status: 'resolved', discovery: { scope: 'explicit-local-catalog', status: 'not-needed' },
    dependencies: [], site: { kind: 'include-key', yaml_path: '/flow/0/step/include', range },
    items: [{ id: 'include-key:runbook', kind: 'include-key', name: 'runbook', edit: { range, new_text: 'runbook' } }],
    signature: null, required_edit: null,
  };
  assert.equal(decodeAuthoringReply(reply, request).schema_version, 'authoring-reply/v3');
  assert.throws(() => decodeAuthoringReply({ ...reply, schema_version: 'authoring-reply/v2' }, request), /invalid-authoring-response/);
});
