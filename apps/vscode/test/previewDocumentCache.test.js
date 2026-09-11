const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const vm = require('node:vm');
const http = require('node:http');

const html = fs.readFileSync(path.join(value('CORE_ROOT') || path.resolve(__dirname, '..', '..', '..', 'runtime'),
  'internal', 'serve', 'static', 'preview.html'), 'utf8');
const code = html.slice(html.indexOf('async function fetchPreviewDocument('), html.indexOf('// ---------- main app ----------'));
function loader(fetcher = fetch) {
  return vm.runInNewContext(code + '\nfetchPreviewDocument', {
    fetch: fetcher, validateDocumentPresentation: value => value,
    parseDisplayJSON: require('../out/displayPresentationJSON').parseDisplayJSON,
  });
}
test('production HTTP cache follows 200 -> 304 -> new checkpoint 200 without structural overlay aliasing', async () => {
  let checkpoint = 1;
  const requests = [];
  const server = http.createServer((req, res) => {
    const etag = `"same-structural-hash:checkpoint-${checkpoint}"`;
    requests.push(req.headers['if-none-match']);
    if (req.headers['if-none-match'] === etag) { res.writeHead(304).end(); return; }
    res.writeHead(200, { ETag: etag, 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ hash: 'same-structural-hash', presentation_state: { checkpoint_sequence: checkpoint } }));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    const load = loader(), cache = {}, signal = new AbortController().signal;
    const url = `http://127.0.0.1:${server.address().port}/runs/run-a/document`;
    const first = await load('run-a/source-a', url, cache, signal);
    assert.equal(await load('run-a/source-a', url, cache, signal), first);
    checkpoint++;
    const second = await load('run-a/source-a', url, cache, signal);
    assert.equal(first.hash, second.hash);
    assert.equal(first.presentation_state.checkpoint_sequence, 1);
    assert.equal(second.presentation_state.checkpoint_sequence, 2);
    await load('run-a/source-b', url, cache, signal);
    await load('run-b/source-b', url, cache, signal);
    assert.deepEqual(requests, [undefined, '"same-structural-hash:checkpoint-1"', '"same-structural-hash:checkpoint-1"', undefined, undefined]);
  } finally { await new Promise(resolve => server.close(resolve)); }
});
test('production document cache never commits late, aborted or invalid document validators', async () => {
  let complete;
  const load = loader(() => new Promise(resolve => { complete = resolve; }));
  const cache = {}, controller = new AbortController();
  const pending = load('old', '/old', cache, controller.signal);
  controller.abort();
  cache.identity = 'new'; cache.etag = '"new"'; cache.document = { run: 'new' };
  complete(new Response('{"run":"old"}', { headers: { ETag: '"old"' } }));
  assert.equal(await pending, null);
  assert.equal(cache.document.run, 'new'); assert.equal(cache.etag, '"new"');
  const noDocument = loader(async () => new Response(null, { status: 304 }));
  await assert.rejects(noDocument('missing', '/missing', {}, new AbortController().signal), /without a matching document/);
});
test('terminal production polling queues exactly one final refresh behind an active request', () => {
  assert.match(html, /if \(!runID \|\| isTerminal\) return/);
  assert.match(html, /if \(!manual && isTerminalRef\.current\) return/);
  assert.match(html, /if \(opts\.final\) docFinalRefreshRef\.current = true/);
  assert.match(html, /docFinalRefreshRef\.current = false;\s+loadDoc\(\{ manual: true \}\)/);
  assert.match(html, /loadDoc\(\{ manual: true, final: true \}\)/);
});
