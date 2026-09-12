const test = require('node:test');
const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const protocol = require('../out/authoringProtocol');
const raw = fs.readFileSync(path.join(__dirname, 'fixtures', 'authoring-contract.json'), 'utf8');
const vectors = JSON.parse(raw);
const digest = text => 'sha256:' + createHash('sha256').update(text).digest('hex');
function pair() {
  const vector = vectors.cases[0], template = vectors.prefix + vector.source_template;
  const position = template.indexOf('|CURSOR|'), text = template.replace('|CURSOR|', '');
  const request = { ...vectors.base_request, operation: vector.operation, position,
    document: { ...vectors.document_identity, text } };
  const range = { start: position - 1, end: position };
  const reply = { schema_version: 'authoring-reply/v3', resolver_version: 'core-authoring/v3',
    grammar_version: 'yawr-expression/v2', operation: request.operation, request_id: request.request_id,
    context: { ...request.context }, document: { uri: request.document.uri, version: request.document.version, digest: digest(text) },
    status: 'resolved', discovery: { scope: 'explicit-local-catalog', status: 'complete' },
    dependencies: [], site: { kind: 'tool', yaml_path: '/flow/0/step/tool/name', range },
    items: [{ id: 'opaque', kind: 'tool', name: 'db', edit: { range, new_text: 'db' } }],
    signature: null, required_edit: null };
  return { request, reply };
}
test('canonical fixture bytes pinned, strict edit retains entire surrounding source', () => {
  assert.equal(createHash('sha256').update(raw).digest('hex'), '08500d8713f80a93e218a1fe8a9ce11953eae9eb0a079af0e86caf4301fb5c4e');
  const { request, reply } = pair();
  const decoded = protocol.decodeAuthoringReply(reply, request);
  const { range, new_text } = decoded.items[0].edit;
  assert.equal(request.document.text.slice(range.start, range.end), 'd');
  assert.equal(request.document.text.slice(0, range.start) + new_text + request.document.text.slice(range.end),
    request.document.text.replace('name: d\n', 'name: db\n'));
});
test('closed reply rejects identity, modes, bounds, IDs, dependency and metadata violations', () => {
  const mutations = [
    r => { r.extra = true; }, r => { delete r.signature; }, r => { r.operation = 'signature'; },
    r => { r.document.digest = digest('stale'); }, r => { r.context.generation++; },
    r => { r.document.version++; }, r => { r.items[0].default = 'SECRET'; },
    r => { r.items[0].description = 'x'.repeat(2049); },
    r => { r.items[0].edit.range = { start: 0, end: 1 }; },
    r => { r.items.push(r.items[0]); }, r => { r.items[0].edit.range.end = 1.5; },
    r => { r.dependencies = [{ uri: 'file:///unknown' }]; },
    r => { r.dependencies = [{ uri: 'file:///unknown', missing: true, version: 1 }]; },
    r => { r.status = 'unavailable'; r.reason = 'missing-dependency'; },
    r => { r.discovery.status = 'limited'; }, r => { r.reason = 'missing-dependency'; },
    r => { r.items[0].name = '\ud800'; },
  ];
  for (const mutate of mutations) {
    const { request, reply } = pair(); mutate(reply);
    assert.throws(() => protocol.decodeAuthoringReply(reply, request), undefined, mutate.toString());
  }
});
test('strict JSON detects duplicate escaped keys, malformed UTF8, deep objects and extra documents', () => {
  for (const raw of ['{"x":1,"\\u0078":2}', '{}{}', '{"nested":{"x":1,"x":2}}',
    '{"x":"\\ud800"}', '['.repeat(130) + '0' + ']'.repeat(130)]) {
    assert.throws(() => protocol.parseAuthoringJSON(Buffer.from(raw)));
  }
  assert.throws(() => protocol.parseAuthoringJSON(Buffer.from([0x22, 0xc0, 0xaf, 0x22])));
  assert.deepEqual(protocol.parseAuthoringJSON(Buffer.from('{"valid":[1,true,null,"😀"]}')), { valid: [1, true, null, '😀'] });
});
test('required placeholder ranges select literal null and never unsafe snippet syntax', () => {
  const { request, reply } = pair(); request.operation = reply.operation = 'required-arguments';
  reply.items = [];
  reply.required_edit = { edit: { range: reply.site.range, new_text: '"a${x}\\\\": null\n' }, placeholders: [{ start: 11, end: 15 }] };
  const text = reply.required_edit.edit.new_text, start = text.indexOf('null');
  reply.required_edit.placeholders = [{ start, end: start + 4 }];
  assert.deepEqual(protocol.decodeAuthoringReply(reply, request).required_edit, reply.required_edit);
  reply.required_edit.placeholders[0].start--;
  assert.throws(() => protocol.decodeAuthoringReply(reply, request));
});
module.exports = { pair, vectors };
