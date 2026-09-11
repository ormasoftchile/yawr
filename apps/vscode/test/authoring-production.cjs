const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const { pathToFileURL } = require('node:url');
const { AuthoringClient } = require('../out/authoringClient');
const fixture = require('./fixtures/authoring-contract.json');
const editRegressions = require('./fixtures/authoring-edit-regressions.json');
const YAML = require('yaml');
const helper = value('AUTHORING_HELPER');
if (!helper || !path.isAbsolute(helper) || !fs.existsSync(helper)) {
  throw new Error('Production authoring proof requires YAWR_AUTHORING_HELPER pointing to the actual candidate. No mock fallback or automatic skip.');
}
test('actual Go CLI -> strict production decoder, all canonical operations', { timeout: 60000 }, async () => {
  const root = path.join(__dirname, '..', '.vscode-test', `authoring-core-${process.pid}`);
  fs.mkdirSync(root, { recursive: true });
  const client = new AuthoringClient();
  const documentPath = path.join(root, 'new.runbook.yaml'), toolPath = path.join(root, 'db.tool.yaml');
  const overlays = fixture.base_request.overlays.map(b => ({ ...b, path: toolPath, uri: pathToFileURL(toolPath).href }));
  try {
    for (const vector of fixture.cases) {
      const template = fixture.prefix + vector.source_template, position = template.indexOf('|CURSOR|');
      const request = { ...fixture.base_request, request_id: vector.name, operation: vector.operation,
        context: { project_root: root, generation: 7 }, overlays, position,
        document: { ...fixture.document_identity, path: documentPath, uri: pathToFileURL(documentPath).href, text: template.replace('|CURSOR|', '') } };
      const reply = await client.resolve(helper, request);
      assert.equal(reply.status, vector.expect.status, vector.name + ': ' + reply.reason);
      if (vector.expect.item) {
        const expected = vector.expect.item, item = reply.items.find(i => i.name === expected.name && i.kind === expected.kind);
        assert.ok(item, vector.name + ': expected item ' + expected.name);
        for (const [key, value] of Object.entries(expected)) if (key !== 'edit') assert.equal(item[key], value, `${vector.name}.${key}`);
        assert.equal(item.edit.new_text, expected.edit.new_text);
        assert.equal(request.document.text.slice(item.edit.range.start, item.edit.range.end), vector.edit_target);
        assert.equal(item.edit.range.end, position);
      }
      if (vector.expect.signature) for (const [key, value] of Object.entries(vector.expect.signature)) {
        assert.equal(reply.signature?.[key], value, `${vector.name}.${key}`);
      }
      if (vector.expect.items) assert.deepEqual(reply.items, vector.expect.items);
      if (vector.expect.required_missing_names) {
        assert.ok(reply.required_edit);
        assert.ok(reply.required_edit.placeholders.length);
        const { range, new_text } = reply.required_edit.edit;
        const edited = request.document.text.slice(0, range.start) + new_text + request.document.text.slice(range.end);
        for (const name of vector.expect.required_missing_names) assert.match(edited, new RegExp(`${name}: null`));
        assert.ok(edited.includes(vector.expect.preserved_source));
      }
      assert.ok(!JSON.stringify(reply).includes('"default":'), 'No raw defaults');
    }
  } finally { client.dispose(); fs.rmSync(root, { recursive: true, force: true }); }
});

test('actual Go CLI edits preserve selected identifiers, separators and all other fields', { timeout: 60000 }, async () => {
  const root = path.join(__dirname, '..', '.vscode-test', `authoring-edits-${process.pid}`);
  fs.mkdirSync(root, { recursive: true });
  const client = new AuthoringClient();
  const documentPath = path.join(root, 'new.runbook.yaml'), toolPath = path.join(root, 'db.tool.yaml');
  try {
    for (const vector of editRegressions) {
      for (const crlf of [false, true]) {
        const normalize = text => crlf ? text.replace(/\n/g, '\r\n') : text;
        const template = normalize(fixture.prefix + vector.source_template);
        const request = { ...fixture.base_request, request_id: vector.name, operation: 'complete',
          context: { project_root: root, generation: 7 }, position: template.indexOf('|CURSOR|'),
          overlays: fixture.base_request.overlays.map(b => ({ ...b, path: toolPath, uri: pathToFileURL(toolPath).href })),
          document: { ...fixture.document_identity, path: documentPath, uri: pathToFileURL(documentPath).href, text: template.replace('|CURSOR|', '') } };
        const reply = await client.resolve(helper, request);
        assert.equal(reply.status, 'resolved', vector.name);
        const item = reply.items.find(i => i.name === vector.item && i.kind === vector.kind);
        assert.ok(item, vector.name);
        if (vector.kind === 'namespace') assert.ok(reply.items.every(i => i.kind === 'namespace'));
        const { range, new_text } = item.edit;
        assert.ok(range.start <= request.position && request.position <= range.end);
        const edited = request.document.text.slice(0, range.start) + new_text + request.document.text.slice(range.end);
        const expected = normalize(fixture.prefix + vector.expected_source_template);
        assert.equal(edited, expected, vector.name + ': exact edit, not just valid YAML');
        const decoded = YAML.parse(edited);
        assert.deepEqual(decoded, YAML.parse(expected));
        if (vector.kind === 'argument') assert.deepEqual(decoded.flow[0].step.tool.args, { text: 5, limit: 9 });
      }
    }
    for (const expression of [
      "unknown.co|CURSOR|ntains('x', 'y')",
      "obj.st|CURSOR|r.contains('x', 'y')",
      "obj?.st|CURSOR|r.contains('x', 'y')",
      "st|CURSOR|r?.contains('x', 'y')",
      "str.co|CURSOR|ntains.more",
      "str.trim('st|CURSOR|r.contains')",
      "regex.match(x, 'st|CURSOR|r.contains')",
      `list.order(rows, 'regex.match(left.x, "st|CURSOR|r.contains")')`,
    ]) {
      const template = fixture.prefix + "        name: db\n        action: inspect\n      when: " + expression + "\n";
      const request = { ...fixture.base_request, request_id: 'no-invented-member', operation: 'complete',
        context: { project_root: root, generation: 7 }, position: template.indexOf('|CURSOR|'),
        overlays: fixture.base_request.overlays.map(b => ({ ...b, path: toolPath, uri: pathToFileURL(toolPath).href })),
        document: { ...fixture.document_identity, path: documentPath, uri: pathToFileURL(documentPath).href, text: template.replace('|CURSOR|', '') } };
      const reply = await client.resolve(helper, request);
      assert.equal(reply.status, 'resolved', expression);
      assert.deepEqual(reply.items, [], expression + ': no invented functions/members');
    }
    for (const name of ['true', 'a: b', `quote'"\\\${x}`, '😀']) {
      const vector = editRegressions.find(v => v.name === 'quoted-key-original');
      const template = fixture.prefix + vector.source_template;
      const request = { ...fixture.base_request, request_id: 'encoded-key', operation: 'complete',
        context: { project_root: root, generation: 7 }, position: template.indexOf('|CURSOR|'),
        overlays: fixture.base_request.overlays.map(b => ({ ...b, path: toolPath, uri: pathToFileURL(toolPath).href,
          text: b.text.replace('      text:', '      ' + JSON.stringify(name) + ':') })),
        document: { ...fixture.document_identity, path: documentPath, uri: pathToFileURL(documentPath).href, text: template.replace('|CURSOR|', '') } };
      const reply = await client.resolve(helper, request);
      assert.equal(reply.status, 'resolved');
      const item = reply.items.find(i => i.name === name);
      assert.ok(item);
      const { range, new_text } = item.edit;
      const edited = request.document.text.slice(0, range.start) + new_text + request.document.text.slice(range.end);
      assert.equal(edited, request.document.text.replace('"te"', JSON.stringify(name)));
      assert.deepEqual(YAML.parse(edited).flow[0].step.tool.args, { [name]: 5, limit: 9 });
    }
  } finally { client.dispose(); fs.rmSync(root, { recursive: true, force: true }); }
});
