const test = require('node:test');
const assert = require('node:assert/strict');
const { AuthoringClient } = require('../out/authoringClient');
const { sourceDigest } = require('../out/authoringProtocol');
const wire = require('../out/presentationClient');
const caps = { schema_version: 'yawr.authoring-capabilities/v1', resolver_version: 'yawr.core-authoring/v1',
  grammar_version: 'yawr-expression/v2', operations: ['complete', 'signature', 'required-arguments'],
  discovery_scope: 'explicit-local-catalog', max_bytes: 8388608, max_overlays: 128, max_items: 4096,
  max_value_code_units: 32768, max_depth: 128 };
const request = (id = 'one') => ({ schema_version: 'yawr.authoring-request/v1', request_id: id, operation: 'complete',
  context: { project_root: 'C:\\fixture', generation: 1 },
  document: { uri: `file:///C:/fixture/${id}.runbook.yaml`, path: `C:\\fixture\\${id}.runbook.yaml`, text: '', version: 1 },
  position: 0, overlays: [] });
const reply = r => ({ schema_version: 'yawr.authoring-reply/v1', resolver_version: 'yawr.core-authoring/v1',
  grammar_version: 'yawr-expression/v2', request_id: r.request_id, operation: r.operation, context: r.context,
  document: { uri: r.document.uri, version: r.document.version, digest: sourceDigest(r.document.text) },
  status: 'resolved', discovery: { scope: 'explicit-local-catalog', status: 'not-needed' },
  dependencies: [], site: null, items: [], signature: null, required_edit: null });
const tick = () => new Promise(resolve => setImmediate(resolve));
test('v3 is exact opt-in; malformed/transport failures never become legacy support or cached versions',async t=>{
  identity(t);
  const v3caps={...caps,schema_version:'authoring-capabilities/v3',resolver_version:'core-authoring/v3',
    capabilities:['yawr.typed-results/v1'],capture_roots:['outputs'],
    typed_operations:[{kind:'assign',role:'technical',terminal:false},{kind:'results',title:'Results',role:'operator',terminal:true}]};
  const helper=t.mock.method(wire,'finiteHelper');
  for(const failure of [new Error('helper-unavailable'),new Error('helper-deadline'),new Error('invalid-helper-response'),{...v3caps,extra:true},caps]){
    let fail=true,probes=0,operations=0;
    helper.mock.mockImplementation(async(_binary,args,input)=>{
      if(args[1]==='capabilities'){
        assert.deepEqual(args,['authoring','capabilities','--v3']);probes++;
        if(fail){if(failure instanceof Error)throw failure;return failure;}return v3caps;
      }
      operations++;const r=JSON.parse(input);assert.equal(r.schema_version,'authoring-request/v3');
      return {...reply(r),schema_version:'authoring-reply/v3',resolver_version:'core-authoring/v3'};
    });
    const client=new AuthoringClient(),req={...request(),schema_version:'authoring-request/v3'};
    try{
      await assert.rejects(client.resolve('helper',req));assert.equal(operations,0);
      assert.equal(client.capabilities.get('helper').typedVersion,undefined);
      fail=false;await client.resolve('helper',req);await client.resolve('helper',req);
      assert.equal(probes,2);assert.equal(operations,2);
    }finally{client.dispose();}
  }
});
test('exact historical v3 refusal alone permits authoring v2 compatibility',async t=>{
  identity(t);const client=new AuthoringClient(),calls=[];
  t.mock.method(wire,'finiteHelper',async(_binary,args,input)=>{
    calls.push(args);
    if(args[2]==='--v3')throw Error('unsupported-authoring-v3');
    if(args[1]==='capabilities')return {...caps,schema_version:'authoring-capabilities/v2',resolver_version:'core-authoring/v2'};
    const r=JSON.parse(input);assert.equal(r.schema_version,'authoring-request/v2');
    return {...reply(r),schema_version:'authoring-reply/v2',resolver_version:'core-authoring/v2'};
  });
  try{
    await client.resolve('helper',{...request(),schema_version:'authoring-request/v3'});
    assert.deepEqual(calls.slice(0,2),[['authoring','capabilities','--v3'],['authoring','capabilities','--v2']]);
  }finally{client.dispose();}
});
function identity(t) {
  let size = 1;
  t.mock.method(require('node:fs/promises'), 'stat', async () => ({ dev: 1, ino: 1, size, mtimeMs: size, ctimeMs: size }));
  return () => size++;
}
test('client issues only finite metadata operations, caches matching binary identity, retries malformed capabilities', async t => {
  const replace = identity(t), client = new AuthoringClient(), calls = [];
  let malformed = false;
  t.mock.method(wire, 'finiteHelper', async (binary, args, input, signal, observer, parser) => {
    assert.equal(typeof parser, 'function');
    calls.push(args);
    return args[1] === 'capabilities' ? malformed ? { ...caps, extra: true } : caps : reply(JSON.parse(input));
  });
  try {
    await client.resolve('helper', request()); await client.resolve('helper', request());
    assert.equal(calls.filter(args => args[1] === 'capabilities').length, 1);
    replace(); malformed = true;
    await assert.rejects(client.resolve('helper', request()), /invalid-authoring-response/);
    malformed = false; await client.resolve('helper', request());
    assert.equal(calls.filter(args => args[1] === 'capabilities').length, 3);
    for (const operation of ['signature', 'required-arguments']) await client.resolve('helper', { ...request(), operation });
    assert.ok(calls.every(args => args[0] === 'authoring' &&
      (args.length === 2 && args[1] === 'capabilities' || args.length === 3 && args[2] === '--stdio')));
  } finally { client.dispose(); }
});
test('one request per document/operation, sixteen pending total, disposal cancels owned work', async t => {
  identity(t); const client = new AuthoringClient(), pending = [];
  t.mock.method(wire, 'finiteHelper', async (binary, args, input, signal) => {
    if (args[1] === 'capabilities') return caps;
    return new Promise((resolve, reject) => {
      const entry = { signal, resolve: () => resolve(reply(JSON.parse(input))) }; pending.push(entry);
      signal.addEventListener('abort', () => reject(new Error('stale-request')), { once: true });
    });
  });
  const results = [];
  for (let i = 0; i < 17; i++) { results.push(client.resolve('helper', request(String(i))).catch(e => e.message)); await tick(); }
  assert.equal(pending.filter(p => !p.signal.aborted).length, 16);
  assert.equal(pending[0].signal.aborted, true);
  results.push(client.resolve('helper', request('16')).catch(e => e.message)); await tick();
  assert.equal(pending[16].signal.aborted, true, 'Superseded same document work is aborted');
  client.dispose();
  assert.ok((await Promise.all(results)).every(value => value === 'stale-request'));
});
test('replacement at same path during resolve discards even a correctly echoed response', async t => {
  const replace = identity(t), client = new AuthoringClient();
  t.mock.method(wire, 'finiteHelper', async (binary, args, input) => {
    if (args[1] === 'capabilities') return caps;
    replace(); return reply(JSON.parse(input));
  });
  await assert.rejects(client.resolve('helper', request()), /stale-request/);
  client.dispose();
});
test('end-to-end timeout cancels work while waiting for the shared transport', { timeout: 8000 }, async t => {
  identity(t); const client = new AuthoringClient(); let owned;
  t.mock.method(wire, 'finiteHelper', async (binary, args, input, signal) => {
    owned = signal;
    return new Promise((resolve, reject) => signal.addEventListener('abort', () => reject(new Error('stale-request')), { once: true }));
  });
  const before = Date.now();
  await assert.rejects(client.resolve('helper', request()), /stale-request/);
  assert.ok(owned.aborted); assert.ok(Date.now() - before >= 4900); assert.ok(Date.now() - before < 6500);
  client.dispose();
});
test('capability identity entries stay bounded and cancelled probes cannot publish', async t => {
  identity(t); const client = new AuthoringClient();
  const helper = t.mock.method(wire, 'finiteHelper', async (binary, args, input) => args[1] === 'capabilities' ? caps : reply(JSON.parse(input)));
  for (let i = 0; i < 40; i++) await client.resolve(`helper${i}`, request());
  assert.equal(client.capabilities.size, 32);
  const controller = new AbortController();
  helper.mock.mockImplementation(async () => { controller.abort(); return caps; });
  await assert.rejects(client.resolve('cancelled', request(), controller.signal), /stale-request/);
  assert.equal(client.capabilities.get('cancelled').identity, undefined);
  client.dispose();
});
test('filesystem identity lookup is cancellable before any helper is spawned', async t => {
  t.mock.method(require('node:fs/promises'), 'stat', () => new Promise(() => {}));
  t.mock.method(wire, 'finiteHelper', () => assert.fail('No helper before identity lookup completes'));
  const client = new AuthoringClient(), controller = new AbortController();
  const pending = client.resolve('helper', request(), controller.signal);
  controller.abort();
  await assert.rejects(pending, /stale-request/);
  client.dispose();
});
test('v2 opt-in preserves v1 helpers and caches unsupported negotiation by binary identity', async t => {
  const replace = identity(t), client = new AuthoringClient(), calls = [];
  let supportsV2 = false;
  t.mock.method(wire, 'finiteHelper', async (binary, args, input) => {
    calls.push(args);
    if (args[1] === 'capabilities') {
      if (args[2] === '--v2') {
        if (!supportsV2) throw new Error('unsupported-authoring-v2');
        return { ...caps, schema_version: 'authoring-capabilities/v2', resolver_version: 'core-authoring/v2' };
      }
      return caps;
    }
    const req = JSON.parse(input), v2 = req.schema_version === 'authoring-request/v2';
    assert.equal(v2, supportsV2);
    return { ...reply(req), ...(v2 ? { schema_version: 'authoring-reply/v2', resolver_version: 'core-authoring/v2' } : {}) };
  });
  try {
    const req = { ...request(), schema_version: 'authoring-request/v2' };
    assert.equal((await client.resolve('helper', req)).schema_version, 'yawr.authoring-reply/v1');
    assert.equal((await client.resolve('helper', req)).schema_version, 'yawr.authoring-reply/v1');
    assert.equal(calls.filter(args => args[2] === '--v2').length, 1);
    assert.equal(req.schema_version, 'authoring-request/v2', 'Caller request not mutated');
    replace(); supportsV2 = true;
    assert.equal((await client.resolve('helper', req)).schema_version, 'authoring-reply/v2');
    assert.equal(calls.filter(args => args[2] === '--v2').length, 2);
  } finally { client.dispose(); }
});
test('malformed v2 capability is never cached or downgraded', async t => {
  identity(t); const client = new AuthoringClient();
  t.mock.method(wire, 'finiteHelper', async (binary, args) => {
    assert.equal(args[1], 'capabilities', 'Never send source after malformed capabilities');
    return args[2] === '--v2' ? { ...caps, schema_version: 'authoring-capabilities/v2',
      resolver_version: 'core-authoring/v2', extra: true } : caps;
  });
  try {
    await assert.rejects(client.resolve('helper', { ...request(), schema_version: 'authoring-request/v2' }), /invalid-authoring-response/);
    assert.equal(client.capabilities.get('helper').includeVersion, undefined);
  } finally { client.dispose(); }
});

const cp = require('node:child_process');
const { EventEmitter } = require('node:events');
const { PassThrough } = require('node:stream');
const capsV2 = { ...caps, schema_version: 'authoring-capabilities/v2', resolver_version: 'core-authoring/v2' };
function childProcess() {
  return Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(),
    stderr: new PassThrough(), killed: false, kill() { this.killed = true; return true; } });
}
function runtime(t, failProbe) {
  const calls = [];
  let fail = true;
  t.mock.method(cp, 'spawn', (binary, args, options) => {
    assert.equal(options.shell, false);
    calls.push(args);
    const child = childProcess();
    let input = '';
    child.stdin.on('data', chunk => { input += chunk; });
    setImmediate(() => {
      if (args[2] === '--v2' && fail) { failProbe(child); return; }
      const req = input && JSON.parse(input);
      const value = args[1] === 'capabilities' ? args[2] === '--v2' ? capsV2 : caps :
        { ...reply(req), ...(req.schema_version === 'authoring-request/v2' ?
          { schema_version: 'authoring-reply/v2', resolver_version: 'core-authoring/v2' } : {}) };
      child.stdout.write(JSON.stringify(value)); child.emit('close', 0);
    });
    return child;
  });
  return { calls, recover() { fail = false; } };
}

test('production transport treats every nonzero helper exit as unavailable', async t => {
  const marker = 'authoring: invalid-request\n', args = ['authoring', 'capabilities', '--v2'];
  const cases = [
    { chunks: [marker], code: 2 },
    { chunks: [...Buffer.from(marker)].map(byte => Buffer.from([byte])), code: 2 },
    { chunks: [marker], code: 17 },
    { chunks: [marker], code: null, signal: 'SIGTERM' },
    { chunks: [marker], code: 2, signal: 'SIGTERM' },
    { chunks: [], code: 2 },
    { chunks: [marker.trimEnd()], code: 2 },
    { chunks: ['authoring: invalid-request\r\n'], code: 2 },
    { chunks: ['prefix ', marker], code: 2 },
    { chunks: [marker, 'extra diagnostic'], code: 2 },
    { chunks: [marker], code: 2, stdout: JSON.stringify(caps) },
    { chunks: [marker], code: 2, input: '{}' },
    { chunks: [marker], code: 2, args: ['authoring', 'complete', '--stdio'] },
    { chunks: [marker], code: 2, args: ['presentation', 'capabilities', '--v2'] },
    { chunks: [marker], code: 2, args: [...args, '--extra'] },
    { chunks: [marker, Buffer.alloc(8388608)], code: 2, expected: 'limit-exceeded' },
    { chunks: [marker], code: 2, stdout: Buffer.alloc(8388609), expected: 'limit-exceeded' },
    { chunks: [marker], code: 0, stdout: '{}', expected: 'ok' },
  ];
  const spawn = t.mock.method(cp, 'spawn');
  for (const vector of cases) {
    spawn.mock.mockImplementation(() => {
      const child = childProcess();
      setImmediate(() => {
        for (const chunk of vector.chunks) child.stderr.write(chunk);
        if (vector.stdout) child.stdout.write(vector.stdout);
        child.emit('close', vector.code, vector.signal);
      });
      return child;
    });
    const timings = [];
    const pending = wire.finiteHelper('helper', vector.args ?? args, vector.input ?? '', undefined, timing => timings.push(timing));
    if (vector.expected === 'ok') assert.deepEqual(await pending, {});
    else await assert.rejects(pending, { message: vector.expected ?? 'helper-unavailable' });
    assert.equal(timings[0].reason, vector.expected ?? 'helper-unavailable');
    assert.doesNotMatch(JSON.stringify(timings), /invalid-request|extra diagnostic/);
  }
});

for (const [name, failProbe, error] of [
  ['exit 17', child => child.emit('close', 17), 'helper-unavailable'],
  ['spawn EACCES', child => child.emit('error', Object.assign(new Error('private diagnostic'), { code: 'EACCES' })), 'helper-unavailable'],
  ['stdin EPIPE', child => child.stdin.emit('error', Object.assign(new Error('private source'), { code: 'EPIPE' })), 'helper-unavailable'],
  ['noncategorical exit 2', child => { child.stderr.write('unknown failure\n'); child.emit('close', 2); }, 'helper-unavailable'],
  ['malformed JSON', child => { child.stdout.write('{'); child.emit('close', 0); }, 'invalid-helper-response'],
  ['malformed v2 metadata', child => { child.stdout.write(JSON.stringify({ ...capsV2, extra: true })); child.emit('close', 0); }, 'invalid-authoring-response'],
  ['v1 metadata posing as unsupported', child => { child.stdout.write(JSON.stringify(caps)); child.emit('close', 0); }, 'invalid-authoring-response'],
]) test(`real finiteHelper ${name} cannot downgrade/cache v2 and retries the same identity`, async t => {
  identity(t);
  const transport = runtime(t, failProbe), client = new AuthoringClient();
  const req = { ...request(), schema_version: 'authoring-request/v2' };
  try {
    await assert.rejects(client.resolve('helper', req), { message: error });
    assert.equal(client.capabilities.get('helper').includeVersion, undefined);
    assert.equal(transport.calls.filter(args => args[2] === '--stdio').length, 0, 'No source sent after a failed probe');
    transport.recover();
    assert.equal((await client.resolve('helper', req)).schema_version, 'authoring-reply/v2');
    assert.equal((await client.resolve('helper', req)).schema_version, 'authoring-reply/v2');
    assert.equal(transport.calls.filter(args => args[2] === '--v2').length, 2);
    assert.equal(client.capabilities.get('helper').includeVersion, 2);
  } finally { client.dispose(); }
});

test('v2 probe deadline leaves negotiation retryable through production finiteHelper', { timeout: 8000 }, async t => {
  identity(t);
  let stalled;
  const transport = runtime(t, child => { stalled = child; }), client = new AuthoringClient();
  const req = { ...request(), schema_version: 'authoring-request/v2' };
  try {
    await assert.rejects(client.resolve('helper', req), /stale-request|helper-deadline/);
    assert.ok(stalled.killed);
    assert.equal(client.capabilities.get('helper').includeVersion, undefined);
    transport.recover();
    assert.equal((await client.resolve('helper', req)).schema_version, 'authoring-reply/v2');
    assert.equal(transport.calls.filter(args => args[2] === '--v2').length, 2);
  } finally { client.dispose(); }
});

test('cancelled and superseded v2 process owners cannot publish late unsupported results', async t => {
  identity(t);
  let stalled;
  const transport = runtime(t, child => { stalled = child; }), client = new AuthoringClient();
  const req = { ...request(), schema_version: 'authoring-request/v2' }, controller = new AbortController();
  try {
    const cancelled = client.resolve('helper', req, controller.signal);
    const checked = assert.rejects(cancelled, /stale-request/);
    for (let i = 0; !stalled && i < 20; i++) await tick();
    assert.ok(stalled);
    controller.abort(); await checked;
    stalled.stderr.write('authoring: invalid-request\n'); stalled.emit('close', 2);
    assert.equal(client.capabilities.get('helper').includeVersion, undefined);
    stalled = undefined;
    const superseded = assert.rejects(client.resolve('helper', req), /stale-request/);
    for (let i = 0; !stalled && i < 20; i++) await tick();
    assert.ok(stalled);
    transport.recover();
    const next = client.resolve('helper', req);
    stalled.stderr.write('authoring: invalid-request\n'); stalled.emit('close', 2);
    await superseded;
    assert.equal((await next).schema_version, 'authoring-reply/v2');
    assert.equal(client.capabilities.get('helper').includeVersion, 2);
  } finally { client.dispose(); }
});

test('cancellation in post-v2 identity lookup cannot publish a capability result', async t => {
  const controller = new AbortController(), client = new AuthoringClient();
  let stats = 0;
  t.mock.method(require('node:fs/promises'), 'stat', async () => {
    if (++stats === 3) controller.abort();
    return { dev: 1, ino: 1, size: 1, mtimeMs: 1, ctimeMs: 1 };
  });
  runtime(t, child => { child.stdout.write(JSON.stringify(capsV2)); child.emit('close', 0); });
  try {
    await assert.rejects(client.resolve('helper', { ...request(), schema_version: 'authoring-request/v2' }, controller.signal), /stale-request/);
    assert.equal(client.capabilities.get('helper').includeVersion, undefined);
  } finally { client.dispose(); }
});

test('binary replacement during a failed v2 probe leaves capability state unpublished', async t => {
  const replace = identity(t), client = new AuthoringClient();
  const transport = runtime(t, child => {
    replace(); child.stderr.write('authoring: invalid-request\n'); child.emit('close', 2);
  });
  const req = { ...request(), schema_version: 'authoring-request/v2' };
  try {
    await assert.rejects(client.resolve('helper', req), /helper-unavailable/);
    assert.equal(client.capabilities.get('helper').includeVersion, undefined);
    transport.recover();
    assert.equal((await client.resolve('helper', req)).schema_version, 'authoring-reply/v2');
    assert.equal(transport.calls.filter(args => args[2] === '--v2').length, 2);
    assert.equal(transport.calls.filter(args => args.length === 2).length, 2);
  } finally { client.dispose(); }
});

test('authoring probes still share the two-process budget and queued cancellation releases ownership', async t => {
  identity(t);
  const children = [];
  t.mock.method(cp, 'spawn', () => { const child = childProcess(); children.push(child); return child; });
  const first = wire.finiteHelper('helper', ['presentation', 'capabilities']);
  const second = wire.finiteHelper('helper', ['expression', 'capabilities']);
  await tick(); assert.equal(children.length, 2);
  const client = new AuthoringClient(), controller = new AbortController();
  try {
    const queued = assert.rejects(client.resolve('helper', { ...request(), schema_version: 'authoring-request/v2' }, controller.signal), /stale-request/);
    await tick(); assert.equal(children.length, 2);
    controller.abort(); await queued;
    for (const child of children) { child.stdout.write('{}'); child.emit('close', 0); }
    await Promise.all([first, second]);
    await tick(); assert.equal(children.length, 2, 'Cancelled queue entry never spawns');
    assert.equal(client.capabilities.get('helper').includeVersion, undefined);
  } finally { client.dispose(); }
});
