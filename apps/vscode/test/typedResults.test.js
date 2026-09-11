const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { EventEmitter } = require('node:events');
const { PassThrough } = require('node:stream');
const { ResultsAssembly, canonicalResultsJSON, publicationDigest, validateResults, MAX_RESULTS_BYTES } = require('../out/typedResults');
const { DirectRunSession } = require('../out/directRunSession');
const { parseStepDetails } = require('../out/stepDetails');
const { projectWorkflow } = require('../out/workflowProjection');
const { requireTypedPlanVersion, requireCompatibleExecution } = require('../out/presentationClient');

const runID = 'fixture-run', version = 'yawr.stdio/v1';
function publication(value = { nullable: null, boolean: false, integer: 0, array: [], object: {}, string: '', unicode: 'á😀<&>\u2028' }) {
  const document = { schema_version: 'yawr.run-results/v1', publication_id: 'fixture-run/root/results/1',
    plan_snapshot_digest: `sha256:${'a'.repeat(64)}`, checkpoint_sequence: 42,
    origin: { node_id: 'results', invocation: 1 },
    outputs: { result: { type: 'object', value }, explicit_null: { type: 'any', value: null } } };
  return { ...document, digest: publicationDigest(document) };
}
function wire(value, size = 65536) {
  const bytes = Buffer.from(canonicalResultsJSON(value));
  return { chunks: Array.from({ length: Math.ceil(bytes.length / size) }, (_, i) => ({
    version, type: 'run.results.chunk', runID, publicationID: value.publication_id, digest: value.digest,
    offset: i * size, totalBytes: bytes.length, data: bytes.subarray(i * size, (i + 1) * size).toString('base64'),
  })), terminal: { version, type: 'run.finished', runID, status: 'completed',
    results_ref: { schema_version: 'yawr.run-results/v1', publication_id: value.publication_id, digest: value.digest, total_bytes: bytes.length } } };
}
test('named Results preserve native null/false/zero/empty and publication digest excludes digest', () => {
  const value = publication();
  assert.deepEqual(validateResults(value), value);
  assert.equal(new ResultsAssembly(runID).complete({ runID, status: 'completed', results: value }).state, 'available');
  assert.equal(value.digest, publicationDigest({ ...value, digest: 'ignored' }));
  assert.ok(!Object.hasOwn(value.origin, 'frame_id'));
  assert.equal(value.outputs.explicit_null.value, null);
  assert.throws(() => validateResults({ ...value, outputs: { value: { type: 'any' } } }), /fields/);
  assert.throws(() => validateResults({ ...value, outputs: { value: { type: 'boolean', value: 'false' } } }), /type/);
  assert.throws(() => validateResults({ ...value, outputs: { value: { type: 'secret', value: 'protected' } } }), /protected/);
  assert.throws(() => validateResults({ ...value, digest: `sha256:${'0'.repeat(64)}` }), /digest/);
});
test('over 1MiB UTF-8 contiguous chunks preserve every row and identical duplicate chunks', () => {
  const rows = Array.from({length:12000}, (_,i)=>({i, text:'á😀'.repeat(24), empty:[], null:null, false:false, zero:0}));
  const value = publication({rows}), {chunks, terminal} = wire(value);
  assert.ok(terminal.results_ref.total_bytes > 1024*1024);
  const assembly = new ResultsAssembly(runID);
  for(const chunk of chunks) {assembly.acceptChunk(chunk); assembly.acceptChunk(chunk);}
  const result=assembly.complete(terminal);
  assert.equal(result.state,'available');
  assert.deepEqual(result.publication.outputs.result.value.rows, rows);
});
test('missing/overlapping/conflicting/wrong identity and expired partial assemblies never expose success', () => {
  const {chunks,terminal}=wire(publication(),64);
  const variants = [
    [chunks[1]], [chunks[0],chunks[2]], [chunks[0],{...chunks[0],data:Buffer.from('x').toString('base64')}],
    [{...chunks[0],runID:'stale'}], [{...chunks[0],totalBytes:MAX_RESULTS_BYTES+1}],
    [chunks[0],{...chunks[1],publicationID:'other'}], [chunks[0],{...chunks[1],offset:32}],
    [{...chunks[0],data:'not-base64'}],
    [chunks[0],{...chunks[0],offset:1,data:Buffer.from(chunks[0].data,'base64').subarray(1).toString('base64')}],
  ];
  for(const frames of variants) {
    const assembly=new ResultsAssembly(runID);frames.forEach(f=>assembly.acceptChunk(f));
    assert.equal(assembly.complete(terminal).state,'unavailable');
  }
  const partial=new ResultsAssembly(runID);partial.acceptChunk(chunks[0]);
  const failed={...terminal,status:'failed'};
  assert.equal(partial.complete(failed).reason,'execution-not-completed');
  assert.equal(failed.status,'failed');
  const referenceMismatch = new ResultsAssembly(runID);chunks.forEach(f=>referenceMismatch.acceptChunk(f));
  assert.equal(referenceMismatch.complete({...terminal,results_ref:{...terminal.results_ref,digest:`sha256:${'b'.repeat(64)}`}}).state,'unavailable');
  const oldDuplicate = new ResultsAssembly(runID);
  chunks.forEach(f=>oldDuplicate.acceptChunk(f));oldDuplicate.acceptChunk(chunks[0]);
  assert.equal(oldDuplicate.complete(terminal).state,'available');
});
test('noncanonical transport and redacted/missing publication are explicitly unavailable', () => {
  const doc=publication(), raw=Buffer.from(JSON.stringify(doc,null,2)), assembly=new ResultsAssembly(runID);
  const {terminal}=wire(doc);terminal.results_ref.total_bytes=raw.length;
  assembly.acceptChunk({version,type:'run.results.chunk',runID,publicationID:doc.publication_id,digest:doc.digest,offset:0,totalBytes:raw.length,data:raw.toString('base64')});
  assert.equal(assembly.complete(terminal).reason,'results-noncanonical-transport');
  assert.equal(new ResultsAssembly(runID).complete({runID,status:'completed',results:null}).reason,'no-results-publication');
  assert.equal(new ResultsAssembly(runID).complete({runID,status:'completed',results_unavailable:{reason:'redacted'}}).state,'unavailable');
});
test('actual DirectRunSession assembles coalesced chunk frames, keeps failed status, never repeats execution', () => {
  function run(frames) {
    const child = new EventEmitter();
    Object.assign(child,{stdin:new PassThrough(),stdout:new PassThrough(),stderr:new PassThrough(),exitCode:null,killed:false,
      kill(){this.killed=true;return true;}});
    const received=[],errors=[];
    new DirectRunSession(child,{onFrame:f=>received.push(f),onError:e=>errors.push(e),onExit(){}});
    child.stdout.write(frames.map(frame=>JSON.stringify(frame)+'\n').join(''));
    assert.deepEqual(errors,[]);
    assert.equal(child.stdin.read(),null,'view/transport must never invoke or answer anything');
    return received;
  }
  const doc=publication({rows:Array.from({length:9000},(_,i)=>({i,text:'á😀'.repeat(30)}))});
  const {chunks,terminal}=wire(doc);
  const frames=run([{version,type:'run.started',runID},...chunks,terminal]);
  assert.equal(frames.length,2,'raw chunks do not enter webview/history');
  assert.equal(frames[1].resultsAvailability.state,'available');
  assert.deepEqual(frames[1].resultsAvailability.publication,doc);
  const failure=run([{version,type:'run.started',runID},chunks[0],{...terminal,status:'failed'}]).at(-1);
  assert.equal(failure.status,'failed');assert.equal(failure.resultsAvailability.state,'unavailable');
});
test('Results is operational in both views; assignment is inspectable technical plumbing with canonical identity unchanged', () => {
  const nodes=['tool','assign','results'].map((kind,i)=>({id:`id${i}`,position:{x:0,y:0},data:{kind,details:{kind}}}));
  const graph={schema_version:'1',hash:'original-hash',runbook:{id:'fixture'},nodes,edges:[
    {id:'e1',source:'id0',target:'id1'},{id:'e2',source:'id1',target:'id2'}],frames:[],groups:[]};
  const before=JSON.stringify(graph);
  for(const mode of ['workflow','all']) {
    const projection=projectWorkflow(graph,{}, {mode,expandedNodeIDs:new Set(),pinnedNodeIDs:new Set(),collapsedGroupIDs:new Set()});
    assert.ok(projection.document.nodes.some(node=>node.id==='id2'&&node.data.kind==='results'));
    assert.equal(JSON.stringify(graph),before);
  }
  assert.equal(parseStepDetails({kind:'results'},'results','node').kind,'results');
  assert.equal(parseStepDetails({kind:'assign',assign:[{name:'x',value:null}]},'assign','node').assign[0].value,null);
  assert.throws(()=>parseStepDetails({kind:'assign',assign:[{name:'x'}]},'assign','node'),/required/);
});
test('release bundle contains no paused Markdown renderer or host copy/link routes', () => {
  const bundle=fs.readFileSync(path.join(__dirname,'..','media','graph.js'),'utf8');
  const host=fs.readFileSync(path.join(__dirname,'..','out','extension.js'),'utf8');
  for(const pattern of [/MarkdownOutput/,/MarkdownViewContext/,/markdown\.copy/,/markdown\.open-link/,/renderMarkdown/,/DOMPurify/]) {
    assert.doesNotMatch(bundle,pattern);assert.doesNotMatch(host,pattern);
  }
  assert.ok(fs.readFileSync(path.join(__dirname,'..','webview','ResultsViewer.tsx'),'utf8').includes('canonicalResultsJSON(output.value, true)'));
  const disabled=require('../out/displayPresentation');
  const metadata={version:1,format:'markdown',output_field:'content',origin:'frozen',plan_snapshot_digest:`sha256:${'a'.repeat(64)}`,value_status:'available'};
  assert.equal(disabled.decodeDisplayPresentation(metadata),undefined);
  assert.equal(disabled.selectDisplayPresentation(metadata,{content:'not authorized'}).text,undefined);
  const legacy={output:{content:'# legacy plain text',count:0}};disabled.sanitizeDisplayPayload(legacy);
  assert.deepEqual(legacy.output,{content:'# legacy plain text',count:0});
  const decorated={display_presentation:metadata,output:{content:'withheld',count:0}};disabled.sanitizeDisplayPayload(decorated);
  assert.deepEqual(decorated.output,{count:0});
  assert.equal(disabled.mergeDisplayPayload(decorated,{output:{content:'late',count:0}}).output.content,undefined);
});

function rawSession(lines) {
  const child = new EventEmitter();
  Object.assign(child, { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
    exitCode: null, killed: false, kill() { this.killed = true; this.emit('close', 1, null); return true; } });
  const frames = [], errors = [], exits = [];
  const session = new DirectRunSession(child, { onFrame: f => frames.push(f), onError: e => errors.push(e), onExit: c => exits.push(c) });
  child.stdout.write(JSON.stringify({ version, type: 'run.started', runID }) + '\n');
  for (const line of lines) child.stdout.write(line + '\n');
  session.dispose();
  return { frames, errors, exits, child };
}

test('raw typed terminal duplicate decorations are unavailable without hiding execution failure', () => {
  const doc = publication({ nested: { key: 1 }, unicode: 'á😀', negativeZero: -0 });
  for (const status of ['completed', 'failed']) {
    const line = canonicalResultsJSON({ version, type: 'run.finished', runID, status,
      error: { code: 'synthetic-failure', message: 'underlying tool failed' }, results: doc });
    const variants = [
      line.replace('"checkpoint_sequence":', '"checkpoint_sequence":0,"checkpoint_sequence":'),
      line.replace('"checkpoint_sequence":', '"checkpoint_\\u0073equence":0,"checkpoint_sequence":'),
      line.replace('"results":', '"results":null,"results":'),
      line.replace('"results":', '"re\\u0073ults":null,"results":'),
      line.replace('"origin":', '"origin":{},"origin":'),
      line.replace('"result":', '"result":{},"result":'),
      line.replace('"key":', '"key":0,"key":'),
      line.replace('"value":', '"value":null,"value":'),
      line.replace('"digest":', '"digest":"invalid","digest":'),
    ];
    for (const malformed of variants) {
      assert.notEqual(malformed, line);
      const got = rawSession([malformed]), terminal = got.frames.at(-1);
      assert.deepEqual(got.errors, []);
      assert.equal(terminal.type, 'run.finished');
      assert.equal(terminal.status, status);
      assert.deepEqual(terminal.error, { code: 'synthetic-failure', message: 'underlying tool failed' });
      assert.equal(terminal.resultsAvailability.state, 'unavailable');
      assert.equal(terminal.results, undefined);
      assert.equal(terminal.resultsAvailability.publication, undefined);
    }
  }
});

test('raw typed identity and chunk metadata duplicates terminate explicitly, never remain Running', () => {
  const doc = publication(), line = canonicalResultsJSON({ version, type: 'run.finished', runID, status: 'failed', results: doc });
  for (const key of ['version', 'type', 'runID', 'status']) {
    const position = line.lastIndexOf(`"${key}":`);
    const got = rawSession([line.slice(0, position) + `"${key}":"wrong",` + line.slice(position)]);
    assert.equal(got.errors.length, 1);
    assert.equal(got.child.killed, true);
    assert.equal(got.exits.length, 1);
    assert.equal(got.frames.length, 1);
  }
  const { chunks, terminal } = wire(doc);
  for (const key of ['publicationID', 'digest', 'offset', 'totalBytes', 'data']) {
    const raw = canonicalResultsJSON(chunks[0]);
    const got = rawSession([raw.replace(`"${key}":`, `"${key}":null,"${key}":`), canonicalResultsJSON(terminal)]);
    assert.equal(got.errors.length, 1);
    assert.equal(got.child.killed, true);
    assert.equal(got.exits.length, 1);
    assert.equal(got.frames.length, 1);
  }
});

test('raw duplicate references poison completed chunk assembly; identical chunks still pass', () => {
  const doc = publication({ text: 'á😀'.repeat(20000) }), { chunks, terminal } = wire(doc);
  const chunkLines = chunks.flatMap(chunk => [canonicalResultsJSON(chunk), canonicalResultsJSON(chunk)]);
  const positive = rawSession([...chunkLines, canonicalResultsJSON(terminal)]);
  assert.deepEqual(positive.errors, []);
  assert.equal(positive.frames.at(-1).resultsAvailability.state, 'available');
  const line = canonicalResultsJSON(terminal);
  for (const key of ['results_ref', 'schema_version', 'publication_id', 'digest', 'total_bytes']) {
    const got = rawSession([...chunkLines, line.replace(`"${key}":`, `"${key}":null,"${key}":`)]);
    assert.deepEqual(got.errors, []);
    assert.equal(got.frames.at(-1).resultsAvailability.state, 'unavailable');
    assert.equal(got.frames.at(-1).resultsAvailability.publication, undefined);
  }
});

test('raw native numeric Unicode escaped content and exact 1MiB terminal retain values', () => {
  const value = { zero: -0, numbers: [1e-7, 1e20, -1.25, 9007199254740991], unicode: 'á😀\u2028',
    escaped: '"checkpoint_sequence":0,\\n\\u0073{}[]', pad: '' };
  let doc = publication(value);
  const terminal = () => canonicalResultsJSON({ version, type: 'run.finished', runID, status: 'completed', results: doc });
  value.pad = 'x'.repeat(1024 * 1024 - Buffer.byteLength(terminal()) - 1);
  doc = publication(value);
  const line = terminal();
  assert.equal(Buffer.byteLength(line) + 1, 1024 * 1024);
  const got = rawSession([line]);
  assert.deepEqual(got.errors, []);
  const available = got.frames.at(-1).resultsAvailability;
  assert.equal(available.state, 'available');
  assert.deepEqual(available.publication.outputs.result.value, value);
  assert.ok(Object.is(available.publication.outputs.result.value.zero, -0));
});

test('legacy undecorated duplicate output and terminal policy is unchanged', () => {
  const got = rawSession([`{"version":"${version}","type":"run.finished","runID":"${runID}","status":"failed","status":"completed","output":{"key":0,"key":1}}`]);
  assert.deepEqual(got.errors, []);
  assert.equal(got.frames.at(-1).status, 'completed');
  assert.deepEqual(got.frames.at(-1).output, { key: 1 });
});
