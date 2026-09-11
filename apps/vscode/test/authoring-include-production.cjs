const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const { pathToFileURL } = require('node:url');
const { AuthoringClient } = require('../out/authoringClient');
const { parseAuthoringJSON, decodeAuthoringReply } = require('../out/authoringProtocol');
const fixture = require('./fixtures/authoring-include.json');
const YAML = require('yaml');
const helper = value('AUTHORING_HELPER');
if (!helper || !path.isAbsolute(helper) || !fs.existsSync(helper)) throw new Error('Set YAWR_AUTHORING_HELPER to the actual candidate.');

test('include v2 actual CLI and strict decoder preserve exact LF/CRLF YAML edits', { timeout: 60000 }, async () => {
  const root = path.join(__dirname, '..', '.vscode-test', `include-core-${process.pid}`);
  fs.mkdirSync(root, { recursive: true });
  const client = new AuthoringClient(), documentPath = path.join(root, 'new.runbook.yaml');
  const request = source => ({
    schema_version: 'authoring-request/v2', request_id: 'include', operation: 'complete',
    context: { project_root: root, generation: 7 }, position: source.indexOf('|CURSOR|'), overlays: [],
    document: { path: documentPath, uri: pathToFileURL(documentPath).href, version: 1, text: source.replace('|CURSOR|', '') },
  });
  try {
    for (const vector of fixture.cases) for (const crlf of [false, true]) {
      const nl = text => crlf ? text.replace(/\n/g, '\r\n') : text;
      const req = request(nl(fixture.prefix + vector.source));
      const reply = await client.resolve(helper, req);
      assert.equal(reply.schema_version, 'authoring-reply/v2');
      assert.equal(reply.status, 'resolved', vector.name + ': ' + reply.reason);
      assert.equal(reply.discovery.status, 'not-needed', 'Structural metadata never discovers files');
      assert.deepEqual(reply.dependencies, []);
      const item = reply.items.find(i => i.name === vector.item && i.kind === vector.kind);
      assert.ok(item, vector.name);
      const edited = req.document.text.slice(0, item.edit.range.start) + item.edit.new_text + req.document.text.slice(item.edit.range.end);
      assert.equal(edited, nl(fixture.prefix + vector.expected), vector.name);
      const doc = YAML.parseDocument(edited);
      assert.deepEqual(doc.errors, []);
      const include = doc.getIn(['flow', 0, 'step', 'include'], true);
      assert.ok(YAML.isMap(include));
      assert.ok(include.items.every(pair => YAML.isScalar(pair.key)));
      assert.equal(reply.signature, null); assert.equal(reply.required_edit, null);
      const cloned = parseAuthoringJSON(Buffer.from(JSON.stringify(reply)));
      assert.deepEqual(decodeAuthoringReply(cloned, req), reply);
      assert.throws(() => decodeAuthoringReply({ ...reply, schema_version: 'yawr.authoring-reply/v1', resolver_version: 'yawr.core-authoring/v1' },
        { ...req, schema_version: 'yawr.authoring-request/v1' }), /invalid-authoring-response/, 'v1 stays closed to include enums');
    }
    for (const source of [
      'data:\n  include: |CURSOR|\n',
      fixture.prefix + "\n        runbook: child.yaml\n        with:\n          include: |CURSOR|\n",
      fixture.prefix + "\n        ? ru|CURSOR| # no separator\n        with: {}\n",
      fixture.prefix + " '|CURSOR|'\n",
      fixture.prefix + " # |CURSOR|\n",
      fixture.prefix + "\n        runbook: child.yaml\n        runbook_ref: child\n        ex|CURSOR|:\n",
    ]) {
      const reply = await client.resolve(helper, request(source));
      assert.deepEqual(reply.items, [], source);
    }
    const old = await client.resolve(helper, { ...request(fixture.prefix + '|CURSOR|\n'), schema_version: 'yawr.authoring-request/v1' });
    assert.equal(old.schema_version, 'yawr.authoring-reply/v1');
    assert.equal(old.site, null); assert.deepEqual(old.items, []);
  } finally { client.dispose(); fs.rmSync(root, { recursive: true, force: true }); }
});

test('actual candidate recovers include items after exit 17 and spawn EACCES without binary replacement', async t => {
  const cp = require('node:child_process'), { EventEmitter } = require('node:events'), { PassThrough } = require('node:stream');
  const spawn = cp.spawn;
  const text = fixture.prefix + '\n', documentPath = path.join(__dirname, 'synthetic-include.runbook.yaml');
  const req = { schema_version: 'authoring-request/v2', request_id: 'recovery', operation: 'complete',
    context: { project_root: __dirname, generation: 1 }, overlays: [], position: fixture.prefix.length,
    document: { path: documentPath, uri: pathToFileURL(documentPath).href, version: 1, text } };
  const mockedSpawn = t.mock.method(cp, 'spawn');
  for (const failure of ['exit-17', 'spawn-EACCES']) {
    let fail = true, probes = 0, completions = 0;
    mockedSpawn.mock.mockImplementation((binary, args, options) => {
      if (args[2] === '--stdio') completions++;
      if (args[2] !== '--v2') return spawn(binary, args, options);
      probes++;
      if (!fail) return spawn(binary, args, options);
      const child = Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(),
        stderr: new PassThrough(), kill: () => true });
      setImmediate(() => failure === 'exit-17' ? child.emit('close', 17) :
        child.emit('error', Object.assign(new Error('access denied'), { code: 'EACCES' })));
      return child;
    });
    const client = new AuthoringClient();
    try {
      await assert.rejects(client.resolve(helper, req), { message: 'helper-unavailable' });
      assert.equal(client.capabilities.get(helper).includeVersion, undefined);
      assert.equal(completions, 0);
      fail = false;
      for (let i = 0; i < 2; i++) {
        const reply = await client.resolve(helper, req);
        assert.equal(reply.schema_version, 'authoring-reply/v2', failure);
        assert.deepEqual(reply.items.map(item => item.name), ['runbook', 'runbook_ref']);
      }
      assert.equal(probes, 2, 'One failed probe, one successful probe, then the v2 cache');
    } finally { client.dispose(); }
  }
});
