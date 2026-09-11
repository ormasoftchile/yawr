const test = require('node:test');
const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const { Worker } = require('node:worker_threads');
const { parseDocument } = require('yaml');
const wire = require('../out/expressionPresentationProtocol');
const { resolveExpressionScalars } = require('../out/expressionPresentationScalar');
const { expressionColor, expressionSpans, overlayExpressionSpans } = require('../out/expressionPresentationColors');
const { expressionHelperAvailable, resolveExpressionPresentation } = require('../out/expressionPresentationClient');
const { parseStepDetails } = require('../out/stepDetails');
const { sourceSpans } = require('../out/presentationScalar');
const { bundledPresentationHelper } = require('../out/presentationClient');
const fixture = require('./fixtures/expression-contract.json');
const regexFixture = require('./fixtures/expression-contract-v2.json');
const codeFixture = require('./fixtures/presentation-contract.json');
const digest = text => 'sha256:' + createHash('sha256').update(text).digest('hex');
const envelope = values => ({ version: 1, grammar_version: wire.EXPRESSION_GRAMMAR, values });
const projection = (text, tokens, mode = 'gis') => ({ mode, text_length: text.length, text_digest: digest(text), tokens });
const capabilities = { schema_version: 'yawr.expression-capabilities/v1', resolver_version: 'yawr.core-expression/v1',
  grammar_version: wire.EXPRESSION_GRAMMAR, modes: ['gxl', 'gis', 'regex'], max_value_code_units: 32768, max_regions: 4096, max_tokens: 65536 };
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function productionModule(file) {
  const { buildSync } = require('esbuild');
  const Module = require('node:module');
  const compiled = buildSync({ entryPoints: [path.join(__dirname, '..', file)], bundle: true, write: false,
    platform: 'node', format: 'cjs', packages: 'external', logLevel: 'silent' });
  const loaded = new Module(path.join(__dirname, 'expression-production.cjs'), module);
  loaded.filename = loaded.id;
  loaded.paths = module.paths;
  loaded._compile(compiled.outputFiles[0].text, loaded.filename);
  return loaded.exports;
}
function request(text) {
  return { schema_version: 'yawr.expression-resolve/v1', request_id: 'expr:1', document: { uri: 'file:///test.runbook.yaml', path: 'C:\\test.runbook.yaml', version: 1, text },
    context: { project_root: 'C:\\project', generation: 1, package_map_path: 'C:\\configured.yaml', entrypoint_path: 'C:\\entry.runbook.yaml' }, overlays: [] };
}
function reply(req, regions) {
  return { schema_version: req.schema_version, resolver_version: 'yawr.core-expression/v1', grammar_version: wire.EXPRESSION_GRAMMAR,
    request_id: req.request_id, context: req.context, document: { uri: req.document.uri, version: req.document.version }, status: 'resolved', regions };
}
function region(source, pointer, value) {
  const node = parseDocument(source, { keepSourceTokens: true }).getIn(wire.pointerParts(pointer), true);
  return { ...value, yaml_path: pointer, range: { start: node.range[0], end: node.range[1] } };
}
function workerTokens(message) {
  return new Promise((resolve, reject) => {
    const worker = new Worker(path.join(__dirname, '..', 'out', 'highlighting-worker.cjs'));
    const timeout = setTimeout(() => { void worker.terminate(); reject(new Error('worker-timeout')); }, 5000);
    worker.on('error', reject);
    worker.on('message', result => {
      if (result.ready) worker.postMessage({ id: 1, palette: 'dark', ...message });
      else { clearTimeout(timeout); void worker.terminate(); resolve(result); }
    });
  });
}
test('canonical shared values decode unchanged and have exact UTF16 lengths and UTF8 digests', () => {
  for (const { text, expected } of fixture) {
    assert.equal(expected.text_length, text.length);
    assert.equal(expected.text_digest, digest(text));
    const value = { ...expected, path: '/common/when' };
    assert.deepEqual(wire.decodeExpressionPresentation(envelope([value])).values[0], value);
    for (const palette of ['dark', 'light', 'hc', 'hc-light']) {
      assert.ok(new Set(expressionSpans(expected, palette).map(t => t.color)).size >= 2);
    }
  }
});
test('current expression capability, detail and editor protocols are closed', () => {
  const grammar = wire.EXPRESSION_GRAMMAR;
  const modes = ['gxl', 'gis', 'regex'];
  const caps = { ...capabilities, grammar_version: grammar, modes };
  assert.doesNotThrow(() => wire.decodeExpressionCapabilities(caps));
  assert.doesNotThrow(() => wire.decodeExpressionCapabilities({ ...caps, modes: [...modes].reverse() }));
  for (const bad of [
    { ...caps, modes: [...modes, modes[0]] }, { ...caps, modes: modes.slice(1) },
    { ...caps, modes: modes.map(() => 'gxl') }, { ...caps, modes: [...modes.slice(1), 'future'] },
    { ...caps, grammar_version: 'yawr-expression/v3' }, { ...caps, extra: true }, { ...caps, max_tokens: 65537 },
  ]) assert.throws(() => wire.decodeExpressionCapabilities(bad));
  for (const item of [...fixture, ...regexFixture]) {
    const source = `expected: &pattern '${item.text}'\n`, req = request(source);
    const detail = { ...item.expected, path: '/assertions/0/expected' };
    const metadata = { version: 1, grammar_version: grammar, values: [detail] };
    const response = { ...reply(req, [region(source, '/expected', item.expected)]), grammar_version: grammar };
    assert.deepEqual(wire.decodeExpressionPresentation(metadata), metadata);
    assert.deepEqual(wire.decodeExpressionReply(response, req), response);
  }
  const value = { ...regexFixture[0].expected, path: '/assertions/0/expected' };
  assert.equal(value.text_digest, digest(regexFixture[0].text));
  for (const mutate of [
    v => { v.values[0].mode = 'javascript'; }, v => { v.values[0].tokens[0].class = 'regexp'; },
    v => { v.values[0].tokens[0].extra = true; }, v => { v.values.push(v.values[0]); },
    v => { v.values[0].path = '/bad~2'; }, v => { v.grammar_version = 'yawr-expression/v0'; },
  ]) {
    const bad = envelope([structuredClone(value)]); mutate(bad);
    assert.equal(wire.decodeExpressionPresentation(bad), undefined);
  }
});
test('v2 regex scalar mapping paints exact classes on all YAML forms, excluding anchors, tags, quotes and aliases', async () => {
  const { expected, text } = regexFixture[0];
  for (const scalar of ["^ab+$", "'^ab+$'", '"^ab+$"', '|-\n  ^ab+$\n', '>-\n  ^ab+$\n',
    '&db_utc \'^ab+$\'', '!!str &db_utc "^ab+$"', '"\\x5eab+$"']) {
    const source = `expected: ${scalar}\nalias: *db_utc\nplain: ^ab+$\n`;
    const req = request(source), response = wire.decodeExpressionReply(reply(req, [region(source, '/expected', expected)]), req);
    const mapped = resolveExpressionScalars(source, response);
    assert.equal(mapped.length, 1, scalar);
    assert.equal(mapped[0].text, text);
    for (const palette of ['dark', 'light', 'hc', 'hc-light']) {
      const spans = sourceSpans(mapped[0], expressionSpans(expected, palette), source);
      for (const token of expected.tokens) {
        const visible = spans.filter(span => span.expressionClass === token.class);
        assert.ok(visible.some(span => span.color === expressionColor(token.class, palette)));
      }
      for (const span of spans) assert.doesNotMatch(source.slice(span.start, span.end), /db_utc|!!str|['"\r\n]/);
      assert.equal(spans.filter(span => span.expressionClass === 'keyword').map(span => source.slice(span.start, span.end)).join(''), scalar.includes('\\x5e') ? '\\x5e$' : '^$');
    }
    const result = await workerTokens({ source, expressions: response });
    assert.deepEqual(result.spans.map(span => [source.slice(span.start, span.end), span.expressionClass]),
      [[scalar.includes('\\x5e') ? '\\x5e' : '^', 'keyword'], ['ab', 'string'], ['+', 'operator'], ['$', 'keyword']]);
    const widened = structuredClone(response);
    widened.regions[0].range.start = source.indexOf(' ') + 1;
    if (scalar.startsWith('&') || scalar.startsWith('!!')) assert.deepEqual(resolveExpressionScalars(source, widened), []);
    const alias = parseDocument(source).getIn(['alias'], true);
    const aliasReply = reply(req, [{ ...expected, yaml_path: '/alias', range: { start: alias.range[0], end: alias.range[1] } }]);
    assert.deepEqual(resolveExpressionScalars(source, aliasReply), []);
  }
});
test('v2 safe authored recursion and DOM rendering consume core regex tokens without guessing strings', async () => {
  const { verifiedExpressionValues, appendTokenText } = productionModule('webview\\highlighting\\expressions.ts');
  const { text, expected } = regexFixture[0];
  const pointers = ['/assertions/0/expected', '/arguments/0/value/a~0~1/0/pattern'];
  const details = {
    assertions: [{ type: 'matches', expected: text }, { type: 'eq', expected: text }],
    arguments: [{ name: 'payload', value: { 'a~/': [{ redacted: true, pattern: text }] } },
      { name: 'secret', redacted: true, value: text }],
    expression_presentation: envelope([...pointers, '/arguments/1/value'].map(path => ({ ...expected, path }))),
  };
  const values = await verifiedExpressionValues(details, new AbortController().signal);
  assert.deepEqual([...values.keys()], pointers);
  assert.equal(values.has('/assertions/1/expected'), false);
  for (const mutate of [
    v => { v.expression_presentation.grammar_version = 'yawr-expression/v0'; },
    v => { v.expression_presentation.values[0].text_digest = 'sha256:' + '0'.repeat(64); },
    v => { v.expression_presentation.values[0].path = '/missing'; },
  ]) {
    const bad = structuredClone(details); mutate(bad);
    assert.equal((await verifiedExpressionValues(bad, new AbortController().signal)).has(pointers[0]), false);
  }
  const controller = new AbortController(); controller.abort();
  assert.equal((await verifiedExpressionValues(details, controller.signal)).size, 0);
  const saved = globalThis.document;
  const element = () => ({ dataset: {}, style: {}, textContent: '', replaceChildren(...nodes) { this.children = nodes; } });
  globalThis.document = { createElement: element, createTextNode: text => ({ textContent: text }) };
  try {
    const container = element(), hostile = '<img src=x onerror=alert(1)> ' + text;
    const prefix = hostile.length - text.length;
    appendTokenText(container, hostile, expressionSpans(expected, 'dark').map(span => ({ ...span, start: span.start + prefix, end: span.end + prefix })));
    assert.equal(container.children.map(child => child.textContent).join(''), hostile);
    assert.equal(container.children.filter(child => child.dataset?.expressionClass).length, 4);
    assert.ok(container.children.every(child => !Object.hasOwn(child, 'innerHTML')));
  } finally { globalThis.document = saved; }
});
test('v2 regex and GIS spans preserve their supplied boundaries through folding and astral YAML escapes', async () => {
  const text = '^🚀${name}+$';
  const tokens = [
    { start: 0, end: 1, class: 'keyword' }, { start: 1, end: 3, class: 'string' },
    ...fixture[1].expected.tokens,
    { start: 10, end: 11, class: 'operator' }, { start: 11, end: 12, class: 'keyword' },
  ];
  // Reuse core's canonical "Hi ${name}!" GIS token offsets; the prefix
  // "^🚀" occupies the same three UTF-16 units, and regex owns the suffix.
  const value = projection(text, tokens, 'regex');
  assert.equal(text.length, 12);
  const source = 'expected: &pattern "^\\U0001F680${name}+$"\n';
  const req = request(source), response = wire.decodeExpressionReply(reply(req, [region(source, '/expected', value)]), req);
  const spans = (await workerTokens({ source, expressions: response })).spans;
  assert.ok(spans.some(span => source.slice(span.start, span.end) === '\\U0001F680' && span.expressionClass === 'string'));
  assert.ok(spans.some(span => source.slice(span.start, span.end) === '${' && span.expressionClass === 'interpolation'));
  assert.ok(spans.some(span => source.slice(span.start, span.end) === 'name' && span.expressionClass === 'variable'));
  assert.ok(spans.some(span => source.slice(span.start, span.end) === '+' && span.expressionClass === 'operator'));
});
test('expression reply is separate, closed and echoes full source context; unsupported data degrades only expressions', () => {
  const req = request('when: count >= 2\n');
  const value = reply(req, [region(req.document.text, '/when', fixture[0].expected)]);
  assert.deepEqual(wire.decodeExpressionReply(value, req), value);
  for (const mutate of [
    v => { v.schema_version = 'yawr.presentation-resolve/v1'; }, v => { v.grammar_version = 'future'; },
    v => { v.context.generation++; }, v => { v.context.package_map_path = 'C:\\other.yaml'; },
    v => { v.document.version++; }, v => { v.extra = true; }, v => { v.regions[0].tokens[0].class = 'future'; },
    v => { v.regions[0].tokens[0].end = 99; }, v => { v.regions[0].tokens[1].start = 1; },
    v => { v.regions[0].yaml_path = '/bad~2path'; }, v => { v.regions[0].range.end = 999; },
    v => { v.status = 'unavailable'; v.reason = 'incomplete-source'; },
  ]) {
    const bad = structuredClone(value); mutate(bad); assert.throws(() => wire.decodeExpressionReply(bad, req));
  }
  for (const metadata of [null, {}, { version: 2 }, envelope([{ ...fixture[0].expected, path: '/common/when', extra: 1 }]),
    { ...envelope([]), grammar_version: 'future' }]) {
    const details = parseStepDetails({ kind: 'branch', common: { when: fixture[0].text }, expression_presentation: metadata }, 'branch', 'node');
    assert.equal(details.expression_presentation, undefined);
    assert.equal(details.common.when, fixture[0].text);
  }
});
test('CST mapping uses core pointer, exact range, decoded length/digest and indivisible escape units', () => {
  for (const scalar of ['count >= 2', "'count >= 2'", '"count >= 2"', '>-\r\n  count\r\n  >= 2\r\n',
    '|-\n  count >= 2\n', '"\\U0001F680 >= 2"', "'''x'' >= 2'"]) {
    const source = 'flow:\n  - step:\n      when: ' + scalar + '\n      title: untouched\n';
    // Block scalars need indentation relative to their actual owner.
    const fixed = source.replace(/\r?\n  (count|>=)/g, '\n        $1');
    const pointer = '/flow/0/step/when';
    const node = parseDocument(fixed, { keepSourceTokens: true }).getIn(wire.pointerParts(pointer), true);
    const expected = projection(node.value, [{ start: 0, end: node.value.length, class: 'variable' }], 'gxl');
    const req = request(fixed), res = wire.decodeExpressionReply(reply(req, [region(fixed, pointer, expected)]), req);
    const mapped = resolveExpressionScalars(fixed, res);
    assert.equal(mapped.length, 1, scalar);
    assert.equal(mapped[0].text, node.value);
    const spans = sourceSpans(mapped[0], expressionSpans(expected, 'dark'), fixed);
    assert.ok(spans.length);
    for (const span of spans) assert.doesNotMatch(fixed.slice(span.start, span.end), /[\r\n]/);
    for (const mutate of [
      v => { v.regions[0].text_digest = 'sha256:' + '0'.repeat(64); },
      v => { v.regions[0].text_length++; }, v => { v.regions[0].range.start++; },
      v => { v.regions[0].yaml_path = '/flow/0/step/title'; },
    ]) {
      const bad = structuredClone(res); mutate(bad);
      assert.equal(resolveExpressionScalars(fixed, bad).length, 0);
    }
  }
});
test('surrogate boundaries, malformed spans, envelope budgets and redacted descendants fail closed', () => {
  const text = '🚀 ${x}', value = projection(text, [{ start: 1, end: 2, class: 'string' }]);
  assert.equal(wire.expressionTextMatches(text, value, digest(text)), false);
  assert.equal(wire.decodeExpressionPresentation(envelope([{ ...value, text_length: 32769, path: '/x' }])), undefined);
  assert.equal(wire.decodeExpressionPresentation(envelope(Array.from({ length: 4097 }, (_, i) => ({ ...value, path: '/' + i })))), undefined);
  const root = { arguments: [{ name: 'a', redacted: true, value: { text: 'never render' } }, { name: 'b', value: { 'a~/': ['safe'] } }] };
  assert.equal(wire.safeExpressionText(root, '/arguments/0/value/text'), undefined);
  assert.equal(wire.safeExpressionText(root, '/arguments/1/value/a~0~1/0'), 'safe');
  assert.equal(wire.safeExpressionText({ value: '[REDACTED]' }, '/value'), undefined);
  assert.equal(wire.safeExpressionText({ value: 'safe prefix <redacted> suffix' }, '/value'), undefined);
  assert.equal(wire.safeExpressionText({ value: '<truncated>' }, '/value'), undefined);
  assert.equal(wire.safeExpressionText({}, '/__proto__/x'), undefined);
});
test('only serialized named-entry boundaries protect values, not arbitrary payload redacted keys', async t => {
  const { verifiedExpressionValues } = productionModule('webview\\highlighting\\expressions.ts');
  const digestInputs = [];
  const originalDigest = globalThis.crypto.subtle.digest.bind(globalThis.crypto.subtle);
  t.mock.method(globalThis.crypto.subtle, 'digest', (algorithm, bytes) => {
    digestInputs.push(new TextDecoder().decode(bytes));
    return originalDigest(algorithm, bytes);
  });
  for (const [kind, field] of [['tool', 'arguments'], ['host_action', 'request'], ['include', 'bindings'],
    ['wait_for_event', 'filter'], ['iterate', 'collect']]) {
    const payload = { redacted: true, code: fixture[1].text, other: 'must remain visible',
      'a~/': [{ name: 'not a typed entry', redacted: true, value: { code: fixture[1].text } }],
      arguments: [{ name: 'also not a typed entry', redacted: true, value: fixture[1].text }] };
    const pointers = [`/${field}/0/value/code`, `/${field}/0/value/a~0~1/0/value/code`, `/${field}/0/value/arguments/0/value`];
    const secret = 'NEVER_DIGEST_OR_RENDER_SECRET ${name}';
    const hidden = `/${field}/1/value/a~0~1/0/code`;
    const details = { kind, [field]: [{ name: 'payload', value: payload },
      { name: 'protected', redacted: true, value: { 'a~/': [{ code: secret }] } }],
      expression_presentation: envelope([...pointers.map(path => ({ ...fixture[1].expected, path })),
        { ...projection(secret, [{ start: 0, end: secret.length, class: 'variable' }]), path: hidden }]) };
    for (const pointer of pointers) assert.equal(wire.safeExpressionText(details, pointer), fixture[1].text);
    assert.equal(wire.safeExpressionText(details, hidden), undefined);
    assert.equal(wire.safeExpressionText(details, `/${field}/01/value/a~0~1/0/code`), undefined);
    const verified = await verifiedExpressionValues(details, new AbortController().signal);
    assert.deepEqual([...verified.keys()], pointers);
    assert.doesNotMatch(JSON.stringify([...verified.values()]), /NEVER_DIGEST_OR_RENDER_SECRET/);
    const parsed = parseStepDetails(structuredClone(details), kind, 'node');
    assert.deepEqual(parsed[field][0].value, payload);
    assert.equal(parsed[field][1].value, undefined);
  }
  assert.equal(digestInputs.length, 15);
  assert.ok(digestInputs.every(text => text === fixture[1].text), 'Protected descendants never reach digest computation');
  assert.equal(wire.safeExpressionText({ redacted: true, code: fixture[1].text }, '/code'), fixture[1].text);
  assert.throws(() => parseStepDetails({ kind: 'tool', arguments: { 0: { name: 'fake', redacted: true, value: 'data' } } }, 'tool', 'node'));
});
test('production authored component and named rows preserve all safe payload fields, including legacy display', () => {
  const React = require('react');
  const { renderToStaticMarkup } = require('react-dom/server');
  const { AuthoredValue } = productionModule('webview\\highlighting\\AuthoredValue.tsx');
  const { NamedValueRows } = productionModule('webview\\inspector.tsx');
  const value = { redacted: true, code: fixture[1].text, other: 'must remain visible',
    'a~/': [{ name: 'ordinary map', redacted: true, value: fixture[1].text }] };
  const render = (component, props) => renderToStaticMarkup(React.createElement(component, props));
  const check = html => {
    assert.match(html, /redacted/);
    assert.match(html, /true/);
    assert.ok(html.includes(fixture[1].text));
    assert.match(html, /must remain visible/);
    assert.match(html, /ordinary map/);
    assert.doesNotMatch(html, /data-expression-class|NEVER_RENDER_SECRET/);
  };
  check(render(AuthoredValue, { value, path: '/arguments/0/value' }));
  for (const path of ['/arguments', '/request', '/bindings', '/filter', '/collect', undefined]) {
    check(render(NamedValueRows, { path, values: [{ name: 'payload', value },
      { name: 'protected', redacted: true, value: { code: 'NEVER_RENDER_SECRET', nested: [value] } }] }));
  }
});
test('production worker paints independent ordinary conditions and tolerates incomplete GIS without touching adjacent YAML', async () => {
  const source = 'flow:\n  - step:\n      when: count >= 2\n      display:\n        content: "Hi ${name"\n      title: "${not.selected}"\n';
  const pointer = '/flow/0/step/display/content';
  const text = 'Hi ${name';
  const req = request(source);
  const res = reply(req, [region(source, '/flow/0/step/when', fixture[0].expected),
    region(source, pointer, projection(text, fixture[1].expected.tokens.slice(0, 2)))]);
  const result = await workerTokens({ source, expressions: wire.decodeExpressionReply(res, req) });
  assert.ok(result.spans.length > 0);
  assert.equal(result.spans.find(t => source.slice(t.start, t.end) === '>=').color, expressionColor('operator', 'dark'));
  assert.ok(result.spans.every(t => !source.slice(t.start, t.end).includes('not.selected')));
});
test('detailed GIS overrides KQL tokens without a blanket macro and invalid dollar-prefix does not suppress host colors', async () => {
  const coreReply = structuredClone(codeFixture.expect);
  const selected = coreReply.regions.find(r => r.status === 'resolved');
  assert.ok(selected);
  const doc = parseDocument(codeFixture.request.document.text);
  doc.setIn(wire.pointerParts(selected.yaml_path), "T | where x == '${name}' and y > 2");
  const source = doc.toString();
  const node = parseDocument(source, { keepSourceTokens: true }).getIn(wire.pointerParts(selected.yaml_path), true);
  selected.range = { start: node.range[0], end: node.range[1] };
  coreReply.regions = [selected];
  coreReply.bindings.find(b => b.id === selected.binding_id).actions.find(a => a.name === selected.action)
    .arguments.find(f => f.name === selected.field).presentation.language = 'kql';
  const start = node.value.indexOf('${');
  assert.ok(start >= 0, 'Existing mixed-language fixture contains GIS');
  const end = node.value.indexOf('}', start);
  const value = projection(node.value, [{ start, end: start + 2, class: 'interpolation' },
    { start: start + 2, end, class: 'variable' }, { start: end, end: end + 1, class: 'interpolation' }]);
  const req = request(source);
  const expressions = reply(req, [region(source, selected.yaml_path, value)]);
  const result = await workerTokens({ source, reply: coreReply, expressions });
  const mapped = resolveExpressionScalars(source, expressions)[0];
  const expectedSpans = sourceSpans(mapped, expressionSpans(value, 'dark'), source);
  for (const token of expectedSpans) assert.ok(result.spans.some(t => t.start === token.start && t.end === token.end && t.color === token.color));
  assert.ok(result.spans.some(t => !t.macro), 'Host tokens remain outside GIS');
  doc.setIn(wire.pointerParts(selected.yaml_path), "T | where x == '$${invalid}' and y > 2");
  const invalidSource = doc.toString(), invalidNode = parseDocument(invalidSource, { keepSourceTokens: true }).getIn(wire.pointerParts(selected.yaml_path), true);
  selected.range = { start: invalidNode.range[0], end: invalidNode.range[1] };
  const invalid = await workerTokens({ source: invalidSource, reply: coreReply });
  assert.ok(invalid.spans.length > 0);
  assert.ok(invalid.spans.every(t => !t.macro));
  const host = [{ start: 0, end: 20, color: '#123456' }];
  assert.deepEqual(overlayExpressionSpans(host, [{ start: 5, end: 8, color: '#abcdef', macro: true }]),
    [{ start: 0, end: 5, color: '#123456' }, { start: 5, end: 8, color: '#abcdef', macro: true }, { start: 8, end: 20, color: '#123456' }]);
});
test('missing helper and cancelled requests fall back without altering old code protocol', async () => {
  assert.equal(await expressionHelperAvailable(path.join(__dirname, 'missing-helper.exe')), false);
  const controller = new AbortController(); controller.abort();
  assert.equal(await resolveExpressionPresentation(process.execPath, { ...request(''), schema_version: 'yawr.presentation-resolve/v1' }, controller.signal), undefined);
  assert.equal(await expressionHelperAvailable(process.execPath), false);
  assert.equal(await expressionHelperAvailable(process.execPath), false);
});
test('host failures stay actionable and expression-only success never claims code highlighting', async () => {
  const { resolveCodePresentation } = require('../out/presentationClient');
  const { presentationStatus } = require('../out/presentationStatus');
  const missing = await resolveCodePresentation(path.join(__dirname, 'missing-code-helper.exe'), request(''));
  assert.equal(missing.reason, 'helper-unavailable');
  const expression = [{ start: 0, end: 2, color: '#FFAB70', macro: true }];
  assert.equal(presentationStatus(expression, undefined, [], missing.reason),
    'expressions highlighted · code unavailable: helper-unavailable');
  assert.equal(presentationStatus(expression, { bindings: [] }, []), 'expressions highlighted · no declared code');
  assert.equal(presentationStatus([...expression, { start: 3, end: 4, color: '#123456' }],
    { bindings: [] }, []), 'code highlighted · expressions highlighted');
  assert.match(presentationStatus(expression, undefined, [], 'invalid-source-range'), /code unavailable: invalid-source-range/);
});
test('editor uses the containing project owning an explicit map, not a nested package tools directory', t => {
  const { presentationProjectRoot } = require('../out/presentationContext');
  const project = path.resolve('synthetic-project'), pkg = path.join(project, 'packages', 'demo');
  const document = path.join(pkg, 'runbooks', 'mixed.runbook.yaml');
  const directories = new Set([path.join(project, 'packages'), path.join(project, 'runbooks'),
    path.join(pkg, 'tools'), path.join(pkg, 'runbooks')]);
  const map = path.join('packages', 'local.serve-package-map.yaml'), files = new Set([path.join(project, map)]);
  t.mock.method(fs, 'statSync', file => {
    if (!directories.has(file) && !files.has(file)) throw new Error('ENOENT');
    return { isDirectory: () => directories.has(file), isFile: () => files.has(file) };
  });
  t.mock.method(fs, 'existsSync', file => directories.has(file) || files.has(file));
  assert.equal(presentationProjectRoot(document, [project], project, map), project);
  assert.equal(presentationProjectRoot(document, [project], project, path.join(project, map)), project);
  assert.equal(presentationProjectRoot(document, [project], project, ''), pkg);
  assert.equal(presentationProjectRoot(document, [project], project, 'absent.yaml'), pkg);
  assert.equal(presentationProjectRoot(document, [path.resolve('unrelated')], project, map), pkg);
  files.add(path.join(pkg, map));
  assert.equal(presentationProjectRoot(document, [project], project, map), pkg, 'An actual package-local map remains authoritative');
});
test('KQL, SQL and PowerShell preserve every host color outside GIS including raw strings and comments', async () => {
  const { colorAt } = require('./helpers/mixed-highlighting.cjs');
  for (const [language, text, keyword, literal, comment] of [
    ['kql', "let StartTime=datetime(${start});\nEvents | where name == '${name}' and kind != 'excluded' // outside\n", 'let', "'excluded'", '// outside'],
    ['sql', "SELECT '${name}' AS name, 'excluded' AS kind -- outside\nFROM Events WHERE id = ${id}\n", 'SELECT', "'excluded'", '-- outside'],
    ['powershell', '$name = "${name}"; Write-Output \'excluded\' # outside\nif (${enabled}) { $true }\n', 'if', "'excluded'", '# outside'],
  ]) {
    const coreReply = structuredClone(codeFixture.expect);
    const selected = coreReply.regions.find(region => region.status === 'resolved');
    const doc = parseDocument(codeFixture.request.document.text);
    doc.setIn(wire.pointerParts(selected.yaml_path), text);
    doc.getIn(wire.pointerParts(selected.yaml_path), true).type = 'BLOCK_LITERAL';
    const source = doc.toString();
    const node = parseDocument(source, { keepSourceTokens: true }).getIn(wire.pointerParts(selected.yaml_path), true);
    selected.range = { start: node.range[0], end: node.range[1] };
    coreReply.regions = [selected];
    coreReply.bindings.find(binding => binding.id === selected.binding_id).actions.find(action => action.name === selected.action)
      .arguments.find(field => field.name === selected.field).presentation.language = language;
    const tokens = [...text.matchAll(/\$\{vars\.([a-z]+)\}/g)].flatMap(match => {
      const start = match.index, end = start + match[0].length;
      return [{ start, end: start + 2, class: 'interpolation' }, { start: start + 2, end: start + 6, class: 'variable' },
        { start: start + 6, end: start + 7, class: 'delimiter' }, { start: start + 7, end: end - 1, class: 'property' },
        { start: end - 1, end, class: 'interpolation' }];
    });
    const req = request(source), value = projection(text, tokens);
    const expressions = reply(req, [region(source, selected.yaml_path, value)]);
    const [baseline, combined] = await Promise.all([
      workerTokens({ source, reply: coreReply }), workerTokens({ source, reply: coreReply, expressions }),
    ]);
    assert.deepEqual(combined.reasons, []);
    const expressionTokens = sourceSpans(resolveExpressionScalars(source, expressions)[0], expressionSpans(value, 'dark'), source);
    for (const span of baseline.spans) {
      for (let offset = span.start; offset < span.end; offset++) {
        if (!expressionTokens.some(token => token.start <= offset && offset < token.end)) {
          assert.equal(colorAt(combined.spans, offset), span.color, `${language} host ${offset}`);
        }
      }
    }
    for (const expected of [keyword, literal, comment]) {
      const offset = source.indexOf(expected);
      assert.ok(offset >= 0 && colorAt(combined.spans, offset), `${language}: ${expected}`);
      assert.equal(colorAt(combined.spans, offset), colorAt(baseline.spans, offset));
    }
    for (const token of expressionTokens) {
      assert.equal(colorAt(combined.spans, token.start), token.color, `${language}: ${token.expressionClass}`);
    }
  }
});
test('capability success and proven unsupported cache invalidate on identity change or expiry, not cancellation', async t => {
  const client = require('../out/presentationClient');
  const original = client.finiteHelper;
  const helper = path.join(__dirname, '..', '.vscode-test', `expression-cache-${process.pid}.helper`);
  fs.mkdirSync(path.dirname(helper), { recursive: true });
  fs.writeFileSync(helper, 'identity-one');
  let calls = 0, available = true, now = Date.now();
  t.mock.method(Date, 'now', () => now);
  client.finiteHelper = async () => { calls++; return available ? capabilities : { ...capabilities, grammar_version: 'future/v2' }; };
  try {
    assert.equal(await expressionHelperAvailable(helper), true);
    assert.equal(await expressionHelperAvailable(helper), true);
    assert.equal(calls, 1);
    fs.writeFileSync(helper, 'identity-two-with-different-size');
    available = false;
    assert.equal(await expressionHelperAvailable(helper), false);
    assert.equal(await expressionHelperAvailable(helper), false);
    assert.equal(calls, 2);
    available = true;
    now += 29_999;
    assert.equal(await expressionHelperAvailable(helper), false);
    assert.equal(calls, 2);
    now++;
    assert.equal(await expressionHelperAvailable(helper), true);
    assert.equal(calls, 3, 'Unchanged unsupported helper is reprobed after a bounded negative cache');
    const cancelled = new AbortController(); cancelled.abort();
    assert.equal(await expressionHelperAvailable(helper, cancelled.signal), false);
    assert.equal(calls, 3, 'Even a cached success respects an aborted caller');
    fs.writeFileSync(helper, 'identity-three');
    const controller = new AbortController();
    client.finiteHelper = async () => { calls++; controller.abort(); throw new Error('stale-request'); };
    assert.equal(await expressionHelperAvailable(helper, controller.signal), false);
    client.finiteHelper = async () => { calls++; return capabilities; };
    assert.equal(await expressionHelperAvailable(helper), true);
    assert.equal(calls, 5);
  } finally { client.finiteHelper = original; fs.rmSync(helper, { force: true }); }
});
test('transient capabilities failures recover on the next request with the same helper identity', async () => {
  const client = require('../out/presentationClient'), original = client.finiteHelper;
  const helper = path.join(__dirname, '..', '.vscode-test', `expression-recovery-${process.pid}.helper`);
  fs.mkdirSync(path.dirname(helper), { recursive: true });
  try {
    const failures = [new Error('helper-deadline'), new Error('helper-unavailable'), new Error('invalid-helper-response'),
      new Error('limit-exceeded'), null, {}, { ...capabilities, max_tokens: undefined }, { ...capabilities, extra: true }];
    for (const [index, failure] of failures.entries()) {
      fs.writeFileSync(helper, 'identity-'.repeat(index + 1));
      const before = fs.statSync(helper);
      let calls = 0;
      client.finiteHelper = async () => {
        if (++calls !== 1) return capabilities;
        if (failure instanceof Error) throw failure;
        return failure;
      };
      assert.equal(await expressionHelperAvailable(helper), false);
      assert.equal(calls, 1, 'No internal retries');
      assert.equal(await expressionHelperAvailable(helper), true);
      assert.equal(await expressionHelperAvailable(helper), true);
      assert.equal(calls, 2, 'Recovery is cached only after a valid capabilities response');
      assert.equal(fs.statSync(helper).mtimeMs, before.mtimeMs);
    }
    fs.writeFileSync(helper, 'cancelled-success');
    const controller = new AbortController();
    client.finiteHelper = async () => { controller.abort(); return capabilities; };
    assert.equal(await expressionHelperAvailable(helper, controller.signal), false);
    let calls = 0;
    client.finiteHelper = async () => { calls++; return capabilities; };
    assert.equal(await expressionHelperAvailable(helper), true);
    assert.equal(calls, 1, 'An aborted successful probe must not populate the shared cache');
  } finally { client.finiteHelper = original; fs.rmSync(helper, { force: true }); }
});
test('capability cache belongs to the latest arriving probe, not the latest completion', async t => {
  const unsupported = { ...capabilities, grammar_version: 'future/v2' };
  const orders = [
    { name: 'older unsupported after newer success', older: unsupported, newer: capabilities, newerFirst: true },
    { name: 'older transport exception after newer success', older: new Error('helper-deadline'), newer: capabilities, newerFirst: true },
    { name: 'older unsupported before newer success', older: unsupported, newer: capabilities, newerFirst: false },
    { name: 'older success after newer unsupported', older: capabilities, newer: unsupported, newerFirst: true },
    { name: 'older success before newer unsupported', older: capabilities, newer: unsupported, newerFirst: false },
    { name: 'older unsupported after newer transport exception', older: unsupported, newer: new Error('helper-deadline'), newerFirst: true },
  ];
  for (const [index, scenario] of orders.entries()) await t.test(scenario.name, async t => {
    t.mock.method(require('node:fs/promises'), 'stat', async () => ({ dev: 1, ino: index, size: 1, mtimeMs: 1, ctimeMs: 1 }));
    const pending = [deferred(), deferred()], started = [deferred(), deferred()];
    let calls = 0;
    t.mock.method(require('../out/presentationClient'), 'finiteHelper', async () => {
      const index = calls++;
      if (index >= pending.length) return capabilities;
      started[index].resolve();
      return pending[index].promise;
    });
    const older = expressionHelperAvailable(process.execPath);
    await started[0].promise;
    const newer = expressionHelperAvailable(process.execPath);
    await started[1].promise;
    const complete = async (index, response, result) => {
      if (response instanceof Error) pending[index].reject(response);
      else pending[index].resolve(response);
      assert.equal(await result, response === capabilities);
    };
    if (scenario.newerFirst) {
      await complete(1, scenario.newer, newer);
      await complete(0, scenario.older, older);
    } else {
      await complete(0, scenario.older, older);
      await complete(1, scenario.newer, newer);
    }
    assert.equal(await expressionHelperAvailable(process.execPath), scenario.newer !== unsupported);
    assert.equal(calls, scenario.newer instanceof Error ? 3 : 2, 'Only a transient newest result requires a new caller probe');
  });
});
test('late probes cannot restore a replaced filesystem identity', async t => {
  const helper = path.join(__dirname, 'expression-identity-order.helper');
  let identity = 1, calls = 0;
  t.mock.method(require('node:fs/promises'), 'stat', async () => ({ dev: 1, ino: 1, size: identity, mtimeMs: identity, ctimeMs: identity }));
  const pending = deferred(), started = deferred();
  t.mock.method(require('../out/presentationClient'), 'finiteHelper', async () => {
    if (++calls > 1) return capabilities;
    started.resolve();
    return pending.promise;
  });
  const older = expressionHelperAvailable(helper);
  await started.promise;
  identity++;
  assert.equal(await expressionHelperAvailable(helper), true);
  pending.resolve({ ...capabilities, grammar_version: 'future/v2' });
  assert.equal(await older, false);
  assert.equal(await expressionHelperAvailable(helper), true);
  assert.equal(calls, 2, 'New identity remains cached despite old completion');
});
test('cache ownership is claimed before asynchronous stat, including an old identity returning late', async t => {
  for (const changedIdentity of [false, true]) await t.test(`changed identity: ${changedIdentity}`, async t => {
    const helper = path.join(__dirname, `expression-stat-order-${changedIdentity}.helper`);
    const pendingStat = deferred(), statStarted = deferred();
    const info = { dev: 1, ino: 1, size: 1, mtimeMs: 1, ctimeMs: 1 };
    let stats = 0, calls = 0;
    t.mock.method(require('node:fs/promises'), 'stat', async () => {
      if (++stats > 1) return { ...info, size: changedIdentity ? 2 : 1 };
      statStarted.resolve();
      return pendingStat.promise;
    });
    t.mock.method(require('../out/presentationClient'), 'finiteHelper', async () =>
      ++calls === 1 ? capabilities : { ...capabilities, grammar_version: 'future/v2' });
    const older = expressionHelperAvailable(helper);
    await statStarted.promise;
    assert.equal(await expressionHelperAvailable(helper), true);
    pendingStat.resolve(info);
    assert.equal(await older, false);
    assert.equal(await expressionHelperAvailable(helper), true);
    assert.equal(calls, 2, 'Delayed stat must not transfer commit ownership back to the old request');
  });
});
test('cancelled overlapping probes cannot publish or revive an older cache owner', async t => {
  for (const cancelNewer of [false, true]) await t.test(`cancel newer: ${cancelNewer}`, async t => {
    const helper = path.join(__dirname, `expression-cancel-order-${cancelNewer}.helper`);
    t.mock.method(require('node:fs/promises'), 'stat', async () => ({ dev: 1, ino: 1, size: 1, mtimeMs: 1, ctimeMs: 1 }));
    const pending = [deferred(), deferred()], started = [deferred(), deferred()];
    const controllers = [new AbortController(), new AbortController()];
    let calls = 0;
    t.mock.method(require('../out/presentationClient'), 'finiteHelper', async () => {
      const index = calls++;
      if (index >= pending.length) return capabilities;
      started[index].resolve();
      return pending[index].promise;
    });
    const older = expressionHelperAvailable(helper, controllers[0].signal);
    await started[0].promise;
    const newer = expressionHelperAvailable(helper, controllers[1].signal);
    await started[1].promise;
    controllers[cancelNewer ? 1 : 0].abort();
    pending[1].resolve(capabilities);
    assert.equal(await newer, !cancelNewer);
    pending[0].resolve({ ...capabilities, grammar_version: 'future/v2' });
    assert.equal(await older, false);
    assert.equal(await expressionHelperAvailable(helper), true);
    assert.equal(calls, cancelNewer ? 3 : 2, 'Cancellation must neither cache success nor reactivate an obsolete negative result');
  });
});
test('a cancelled or failed newest stat cannot let an older probe publish', async t => {
  for (const cancel of [false, true]) await t.test(`cancel stat: ${cancel}`, async t => {
    const helper = path.join(__dirname, `expression-stat-failure-${cancel}.helper`);
    const pendingStat = deferred(), statStarted = deferred(), pendingProbe = deferred(), probeStarted = deferred();
    const info = { dev: 1, ino: 1, size: 1, mtimeMs: 1, ctimeMs: 1 };
    let stats = 0, calls = 0;
    t.mock.method(require('node:fs/promises'), 'stat', async () => {
      if (++stats !== 2) return info;
      statStarted.resolve();
      return pendingStat.promise;
    });
    t.mock.method(require('../out/presentationClient'), 'finiteHelper', async () => {
      if (++calls > 1) return capabilities;
      probeStarted.resolve();
      return pendingProbe.promise;
    });
    const older = expressionHelperAvailable(helper);
    await probeStarted.promise;
    const controller = new AbortController();
    const newer = expressionHelperAvailable(helper, controller.signal);
    await statStarted.promise;
    if (cancel) { controller.abort(); pendingStat.resolve(info); }
    else pendingStat.reject(new Error('ENOENT'));
    assert.equal(await newer, false);
    assert.equal(calls, 1, 'Cancelled or failed stat never starts a helper');
    pendingProbe.resolve({ ...capabilities, grammar_version: 'future/v2' });
    assert.equal(await older, false);
    assert.equal(await expressionHelperAvailable(helper), true);
    assert.equal(calls, 2, 'Older unsupported result cannot replace newest stat failure with a negative cache');
  });
});
test('capability cache bounds pending paths and ignores completions after eviction and readmission', async t => {
  for (const readmitFirst of [false, true]) await t.test(`readmit before old completion: ${readmitFirst}`, async t => {
    t.mock.method(require('node:fs/promises'), 'stat', async () => ({ dev: 1, ino: 1, size: 1, mtimeMs: 1, ctimeMs: 1 }));
    const helpers = Array.from({ length: 33 }, (_, i) => path.join(__dirname, `expression-eviction-${readmitFirst}-${i}.helper`));
    const pending = deferred(), started = deferred(), calls = [];
    t.mock.method(require('../out/presentationClient'), 'finiteHelper', async binary => {
      calls.push(binary);
      if (calls.length > 1) return capabilities;
      started.resolve();
      return pending.promise;
    });
    const older = expressionHelperAvailable(helpers[0]);
    await started.promise;
    for (const helper of helpers.slice(1)) assert.equal(await expressionHelperAvailable(helper), true);
    for (const helper of helpers.slice(1)) assert.equal(await expressionHelperAvailable(helper), true);
    assert.equal(calls.length, 33, 'Exactly 32 completed paths fit while the evicted first probe is pending');
    if (readmitFirst) {
      assert.equal(await expressionHelperAvailable(helpers[0]), true);
      assert.equal(calls.length, 34, 'Evicted path is reprobed on readmission');
    }
    pending.resolve({ ...capabilities, grammar_version: 'future/v2' });
    assert.equal(await older, false);
    assert.equal(await expressionHelperAvailable(helpers[0]), true);
    assert.equal(calls.length, 34, 'Old owner cannot overwrite the readmitted path');
    assert.equal(await expressionHelperAvailable(helpers[1]), true);
    assert.equal(calls.length, 35, 'Readmission evicted the oldest remaining path at the 32-path bound');
  });
});
test('expression probes share the two-helper transport cap and isolate queued cancellation', async t => {
  const childProcess = require('node:child_process');
  const { finiteHelper } = require('../out/presentationClient');
  const originalSpawn = childProcess.spawn;
  let active = 0, maximum = 0;
  const script = `setTimeout(() => process.stdout.write(${JSON.stringify(JSON.stringify(capabilities))}), 100)`;
  t.mock.method(childProcess, 'spawn', (binary, args, options) => {
    const child = originalSpawn(binary, args[0] === 'presentation' ? ['-e', script] : args, options);
    maximum = Math.max(maximum, ++active);
    child.once('close', () => active--);
    return child;
  });
  const controller = new AbortController();
  const pending = [finiteHelper(process.execPath, ['-e', script]), finiteHelper(process.execPath, ['-e', script]),
    expressionHelperAvailable(process.execPath), expressionHelperAvailable(process.execPath),
    expressionHelperAvailable(process.execPath, controller.signal)];
  const timer = setTimeout(() => controller.abort(), 25);
  try {
    const results = await Promise.all(pending);
    assert.deepEqual(results.slice(0, 2), [capabilities, capabilities]);
    assert.deepEqual(results.slice(2), [true, true, false]);
    assert.equal(maximum, 2);
    assert.equal(active, 0);
  } finally { clearTimeout(timer); }
});
test('actual new helper emits canonical fixture through the separate production client', async () => {
  const helper = value('EXPRESSION_HELPER') || bundledPresentationHelper(path.join(__dirname, '..'));
  const prerequisite = 'Build/package a matching expression-capable helper, or set YAWR_EXPRESSION_HELPER to that executable.';
  assert.ok(fs.existsSync(helper), prerequisite);
  assert.equal(await expressionHelperAvailable(helper), true, prerequisite);
  const source = `apiVersion: yawr.runbook/v1\nid: expression-vector\nflow:\n  - step:\n      id: condition\n      type: choice\n      when: ${fixture[0].text}\n      prompt: "${fixture[1].text}"\n      options: []\n`;
  const req = request(source);
  const sourcePath = path.join(__dirname, '..', '.vscode-test', 'expression-vector.runbook.yaml');
  req.document.path = sourcePath;
  req.document.uri = require('node:url').pathToFileURL(sourcePath).toString();
  req.context = { project_root: path.resolve(__dirname, '..'), entrypoint_path: sourcePath, generation: 1 };
  const actual = await resolveExpressionPresentation(helper, { ...req, schema_version: 'yawr.presentation-resolve/v1' });
  assert.equal(actual?.status, 'resolved');
  for (const [pointer, expected] of [['/flow/0/step/when', fixture[0].expected], ['/flow/0/step/prompt', fixture[1].expected]]) {
    const value = actual.regions.find(r => r.yaml_path === pointer);
    assert.ok(value, pointer);
    const { yaml_path, range, ...projection } = value;
    assert.deepEqual(projection, expected);
  }
});
