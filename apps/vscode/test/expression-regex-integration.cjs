const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const { pathToFileURL } = require('node:url');
const { parseDocument, stringify } = require('yaml');
const { finiteHelper } = require('../out/presentationClient');
const { resolveExpressionPresentation } = require('../out/expressionPresentationClient');
const { resolveExpressionScalars } = require('../out/expressionPresentationScalar');
const { expressionSpans, expressionColor } = require('../out/expressionPresentationColors');
const { sourceSpans } = require('../out/presentationScalar');
const wire = require('../out/expressionPresentationProtocol');
const fixture = require('./fixtures/expression-contract-v2.json')[0];
const helper = value('EXPRESSION_HELPER');
async function resolve(source) {
  assert.ok(helper && fs.existsSync(helper), 'Set YAWR_EXPRESSION_HELPER (or its YAWR alias) to the isolated v2 candidate; this integration never skips');
  const caps = await finiteHelper(helper, ['presentation', 'expressions', 'capabilities']);
  wire.decodeExpressionCapabilities(caps);
  assert.equal(caps.grammar_version, 'yawr-expression/v2', 'An approved v1 helper is not v2 integration evidence');
  const file = path.join(__dirname, '..', '.vscode-test', 'regex-lexical.runbook.yaml');
  const request = { schema_version: 'yawr.presentation-resolve/v1', request_id: 'regex-lexical', context: {
    project_root: path.resolve(__dirname, '..'), entrypoint_path: file, generation: 1,
  }, document: { uri: pathToFileURL(file).toString(), path: file, version: 1, text: source }, overlays: [] };
  const reply = await resolveExpressionPresentation(helper, request);
  assert.equal(reply?.status, 'resolved');
  assert.equal(reply.grammar_version, 'yawr-expression/v2');
  return { reply, mapped: resolveExpressionScalars(source, reply) };
}
function token(value, text, cls, expected) {
  assert.ok(value.tokens.some(t => t.class === cls && text.slice(t.start, t.end) === expected), `${cls}: ${expected}`);
}
test('actual v2 producer emits canonical anchored assertion, excludes alias and preserves semantic negatives', async () => {
  const source = fs.readFileSync(path.join(__dirname, 'fixtures', 'expression-regex-authored.yaml'), 'utf8');
  const { reply, mapped } = await resolve(source);
  const base = '/flow/0/step/assert';
  const pattern = mapped.find(value => value.expression.yaml_path === `${base}/0/expected`);
  assert.ok(pattern);
  assert.equal(pattern.expression.mode, 'regex');
  const paint = sourceSpans(pattern, expressionSpans(pattern.expression, 'dark'), source);
  assert.ok(paint.length);
  assert.ok(paint.every(span => span.start > pattern.expression.range.start && span.end < pattern.expression.range.end));
  assert.ok(!reply.regions.some(value => value.yaml_path === `${base}/1/expected`));
  assert.ok(reply.regions.filter(value => value.mode === 'regex').every(value => value.yaml_path === `${base}/0/expected`));
  const nested = mapped.find(value => value.expression.yaml_path === `${base}/2/subject`);
  assert.ok(nested); assert.equal(nested.expression.mode, 'gis');
  for (const expected of fixture.expected.tokens) token(nested.expression, nested.text, expected.class,
    fixture.text.slice(expected.start, expected.end));
  for (const value of reply.regions.filter(value => value.yaml_path.startsWith(`${base}/3/`))) {
    assert.equal(value.mode, 'gis'); assert.equal(value.tokens.length, 0);
  }
});
test('actual canonical regex fixture maps plain, single, double, literal, folded, escaped and anchored scalar bodies', async () => {
  for (const scalar of ['^ab+$', "'^ab+$'", '"^ab+$"', '&db_utc \'^ab+$\'', '"\\x5eab+$"',
    '|-\n            ^ab+$', '>-\n            ^ab+$']) {
    const source = `flow:\n  - step:\n      type: assert\n      assert:\n        - type: matches\n          subject: text\n          expected: ${scalar}\n`;
    const { mapped } = await resolve(source);
    const value = mapped.find(value => value.expression.mode === 'regex');
    assert.ok(value, scalar);
    const { yaml_path, range, ...actual } = value.expression;
    assert.deepEqual(actual, fixture.expected, scalar);
    for (const palette of ['dark', 'light', 'hc', 'hc-light']) {
      const spans = expressionSpans(actual, palette);
      assert.ok(spans.every(span => span.color === expressionColor(span.expressionClass, palette)));
    }
  }
});
test('actual literal argument semantics preserve comparator composition, RE2 classes and computed/property negatives', async () => {
  const cases = [
    { expr: 'regex.match(name, "^ab+$")', regex: true },
    { expr: 'list.order(items, "regex.match(a.name, \\"^ab+$\\")")', regex: true },
    { expr: 'obj.regex.match(name, "^ab+$")', regex: false },
    { expr: 'regex.match(name, "^ab" + "+$")', regex: false },
    { expr: 'regex.match(name, ("^ab+$"))', regex: false },
    { expr: 'regex.match(name, pattern)', regex: false },
  ];
  for (const entry of cases) {
    const source = stringify({ flow: [{ step: { type: 'noop', when: entry.expr } }] });
    const { mapped } = await resolve(source);
    const value = mapped.find(value => value.expression.yaml_path.endsWith('/when'));
    assert.ok(value);
    assert.equal(value.expression.mode, 'gxl');
    if (entry.regex) for (const expected of fixture.expected.tokens) token(value.expression, value.text, expected.class,
      fixture.text.slice(expected.start, expected.end));
    else assert.ok(!value.expression.tokens.some(t => t.class === 'keyword' && ['^', '$'].includes(value.text.slice(t.start, t.end))));
  }
  const pattern = '^(?i:(?P<word>\\p{Greek}+|[[:digit:]]{2,4}?|\\Q🚀.\\E))$';
  const source = stringify({ flow: [{ step: { type: 'assert', assert: [{ type: 'matches', subject: 'x', expected: pattern }] } }] });
  const { mapped } = await resolve(source), value = mapped.find(value => value.expression.mode === 'regex');
  assert.ok(value);
  for (const [cls, text] of [['keyword', '^'], ['keyword', '$'], ['keyword', 'i'], ['property', 'word'], ['number', '2'], ['number', '4']]) {
    token(value.expression, value.text, cls, text);
  }
  assert.ok(wire.expressionTextMatches(value.text, value.expression, value.expression.text_digest));
});
test('neutral authored fixture maps exact pattern classes while an alias remains YAML', async () => {
  const source = fs.readFileSync(path.join(__dirname, 'fixtures', 'expression-regex-authored.yaml'), 'utf8');
  const { reply, mapped } = await resolve(source);
  const value = mapped.find(value => value.expression.yaml_path === '/flow/0/step/assert/0/expected');
  assert.ok(value); assert.equal(value.expression.mode, 'regex');
  for (const [cls, text] of [['keyword', '^'], ['keyword', '$'], ['number', '0'], ['number', '62']]) token(value.expression, value.text, cls, text);
  assert.ok(!reply.regions.some(value => value.yaml_path === '/flow/0/step/assert/1/expected'));
  const node = parseDocument(source, { keepSourceTokens: true }).getIn(['flow', 0, 'step', 'assert', 0, 'expected'], true);
  assert.equal(value.expression.range.start, node.range[0]);
  assert.equal(value.expression.range.end, node.range[1]);
  assert.ok(value.expression.range.start > source.indexOf('&db_name') + '&db_name'.length);
});
