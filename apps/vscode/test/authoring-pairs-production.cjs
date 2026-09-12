const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const { pathToFileURL } = require('node:url');
const { AuthoringClient } = require('../out/authoringClient');
const fixture = require('./fixtures/authoring-contract.json');
const YAML = require('yaml');
const helper = value('AUTHORING_HELPER');
if (!helper || !path.isAbsolute(helper) || !fs.existsSync(helper)) {
  throw new Error('Set YAWR_AUTHORING_HELPER to the actual candidate; no skip or mock fallback.');
}
test('actual CLI/production decoder preserves every offered argument pair edit by AST identity', { timeout: 60000 }, async () => {
  const root = path.join(__dirname, '..', '.vscode-test', `authoring-pairs-${process.pid}`);
  fs.mkdirSync(root, { recursive: true });
  const client = new AuthoringClient();
  const documentPath = path.join(root, 'new.runbook.yaml'), toolPath = path.join(root, 'db.tool.yaml');
  const names = ['text', '? text', 'true', 'a: b', `quote'"\\\${x}`];
  const cases = [
    ['plain', 'te|CURSOR|: 5', true],
    ['spaced', 'te|CURSOR| \t: 5 # keep', true],
    ['empty', 'te|CURSOR|: # keep', true],
    ['bare', 'te|CURSOR|', true, 'te: null'],
    ['blank', '|CURSOR|', true, '__yawr_cursor__: null'],
    ['double', '"te|CURSOR|" : 5', true],
    ['single', "'te|CURSOR|' : 5", true],
    ['question-plain-name', '?te|CURSOR|: 5', true],
    ['question-quoted-name', '"? te|CURSOR|" : 5', true],
    ['explicit', '? te|CURSOR|\n          : 5', true],
    ['explicit-comment', '? te|CURSOR| # keep\n          # between\n          : 5', true],
    ['explicit-double', '? "te|CURSOR|" # keep\n          : 5', true],
    ['explicit-single', "? 'te|CURSOR|'\n          : # keep\n            5", true],
    ['explicit-empty', '? te|CURSOR|\n          :', true],
    ['explicit-map-value', "? te|CURSOR|\n          : {nested: [5, null, 'kept']}", true],
    ['explicit-newline-key', '?\n            te|CURSOR|\n          : 5', true],
    ['explicit-no-separator', '? te|CURSOR|', false],
    ['explicit-comment-no-separator', '? te|CURSOR| # keep', false],
    ['explicit-double-no-separator', '? "te|CURSOR|" # keep', false],
    ['explicit-single-no-separator', "? 'te|CURSOR|'", false],
    ['explicit-inline-mapping-key', '? te|CURSOR| : 5', false],
    ['explicit-inline-quoted-mapping-key', '? "te|CURSOR|" : 5', false],
    ['explicit-inline-empty-mapping-key', '? te|CURSOR| :', false],
    ['explicit-nonseparating-colon', '? te|CURSOR|\n            :value', false],
    ['explicit-empty-key', '? |CURSOR|\n          : 5', false],
    ['multiline-plain', '? te|CURSOR|\n            continued\n          : 5', false],
    ['multiline-double', '? "te|CURSOR|\n            continued"\n          : 5', false],
    ['multiline-single', "? 'te|CURSOR|\n            continued'\n          : 5", false],
    ['literal-key', '? |-\n            te|CURSOR|\n          : 5', false],
    ['folded-key', '? >-\n            te|CURSOR|\n          : 5', false],
    ['tagged-key', '!!str te|CURSOR|: 5', false],
    ['sequence-key', '? [te|CURSOR|]\n          : 5', false],
    ['mapping-key', '? {te|CURSOR|: null}\n          : 5', false],
    ['alias-key', '? *te|CURSOR|\n          : 5', false],
    ['anchored-key', '&key te|CURSOR|: 5', false],
    ['merge-key', '<<|CURSOR|: {te: 5}', false],
    ['flow-map', '{te|CURSOR|: 5, limit: 9}', false],
  ];
  const identity = (node, replaced, name) => {
    if (!node) return ['scalar', null];
    if (YAML.isScalar(node)) return ['scalar', node === replaced ? name : node.value];
    if (YAML.isMap(node)) return ['map', node.items.map(pair =>
      [identity(pair.key, replaced, name), identity(pair.value, replaced, name)])];
    if (YAML.isSeq(node)) return ['seq', node.items.map(item => identity(item, replaced, name))];
    throw new Error('Unsupported AST node must not acquire an edit');
  };
  let edits = 0;
  try {
    for (const [name, body, available, repaired] of cases) {
      for (const crlf of [false, true]) {
        const normalize = text => crlf ? text.replace(/\n/g, '\r\n') : text;
        const container = name === 'flow-map' ? '        args: ' + body + '\n' :
          '        args:\n          ' + body + '\n          limit: 9 # sibling\n';
        const template = normalize(fixture.prefix + '        name: db\n        action: inspect\n' + container);
        const request = { ...fixture.base_request, request_id: name, operation: 'complete',
          context: { project_root: root, generation: 7 }, position: template.indexOf('|CURSOR|'),
          overlays: fixture.base_request.overlays.map(b => ({ ...b, path: toolPath, uri: pathToFileURL(toolPath).href,
            text: b.text + names.slice(1).map(key => `      ${JSON.stringify(key)}: {type: string}\n`).join('') })),
          document: { ...fixture.document_identity, path: documentPath, uri: pathToFileURL(documentPath).href,
            text: template.replace('|CURSOR|', '') } };
        const reply = await client.resolve(helper, request);
        if (!available) {
          assert.deepEqual(reply.items, [], name);
          assert.equal(reply.required_edit, null);
          if (name.includes('no-separator')) {
            assert.equal(reply.status, 'unavailable');
            assert.equal(reply.reason, 'invalid-source-range');
          }
          continue;
        }
        assert.equal(reply.status, 'resolved', name);
        assert.deepEqual(reply.items.map(item => item.name).sort(), [...names].sort(), name);
        const beforeSource = repaired ? template.replace(body, repaired).replace('|CURSOR|', '') : request.document.text;
        const before = YAML.parseDocument(beforeSource);
        assert.deepEqual(before.errors, [], name);
        const originalKey = before.getIn(['flow', 0, 'step', 'tool', 'args'], true).items[0].key;
        assert.ok(YAML.isScalar(originalKey));
        for (const item of reply.items) {
          assert.equal(item.kind, 'argument');
          const { range, new_text } = item.edit;
          assert.ok(range.start <= request.position && request.position <= range.end);
          const edited = request.document.text.slice(0, range.start) + new_text + request.document.text.slice(range.end);
          const after = YAML.parseDocument(edited);
          assert.deepEqual(after.errors, [], name);
          assert.deepEqual(identity(after.contents), identity(before.contents, originalKey, item.name),
            `${name}: scalar key must equal ${item.name}, with all values/other nodes unchanged`);
          edits++;
        }
      }
    }
    assert.equal(edits, 160);
  } finally { client.dispose(); fs.rmSync(root, { recursive: true, force: true }); }
});
