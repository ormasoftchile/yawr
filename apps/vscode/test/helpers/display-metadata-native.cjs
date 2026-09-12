const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../../scripts/environment.cjs');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const metadata = require('../fixtures/workflow-markdown-contract.json').displayMetadata;
const marker = 'SYNTHETIC_WITHHELD_REPORT';
const failure = 'original synthetic MCP failure';
const malformed = [
  ['array-status', { ...metadata, value_status: ['redacted'] }],
  ...Object.keys(metadata).map(key => [`array-${key}`, { ...metadata, [key]: [metadata[key]] }]),
  ['null', null], ['object', {}], ['prototype-key', { ...metadata, constructor: 'untrusted' }],
];
function graph() {
  return { schema_version: '1', hash: 'synthetic-display-revision', runbook: { id: 'revision', name: 'Synthetic revision' },
    frames: [], groups: [], edges: [], nodes: [{ id: 'report', type: 'note', position: { x: 0, y: 0 },
      data: { id: 'report', kind: 'display', group_id: '', frame_id: '',
        details: { kind: 'display', format: 'markdown', content: 'AUTHORED_TEMPLATE_NOT_RUNTIME' } } }] };
}
function history(value) {
  return { run_id: 'revision', plan_snapshot_digest: metadata.plan_snapshot_digest, checkpoint_sequence: 1,
    occurrences: [{ identity: { qualified_node_id: 'report', invocation: 1 },
      details: { kind: 'display', format: 'markdown' }, display_presentation: value,
      output: { content: marker }, output_value_status: {} }] };
}
exports.verify = async vscode => {
  const before = value('DISPLAY_REVISION_MODE') === 'before';
  const cases = before ? malformed.slice(0, 1) : malformed;
  const root = value('WORKFLOW_NATIVE_ROOT');
  const extension = vscode.extensions.getExtension('ormasoftchile.yawr-preview');
  await extension.activate();
  const file = path.join(root, 'synthetic.runbook.yaml');
  fs.writeFileSync(file, 'apiVersion: yawr.runbook/v1\nid: synthetic\nname: Synthetic\nflow: []\n');
  const panel = await vscode.commands.executeCommand('yawr.test.openDirectGraphPanel', file, {
    documentLoader: async () => graph(),
    spawnRun: () => { throw new Error('No execution permitted'); },
    spawnSession: () => { throw new Error('No execution permitted'); },
  });
  const messages = [], results = [];
  panel.webview.onDidReceiveMessage(message => messages.push(message));
  const post = async message => { await panel.webview.postMessage(message); await sleep(90); };
  const action = async (action, name) => post({ type: 'test.action', action, name });
  async function state(predicate) {
    for (let i = 0; i < 100; i++) {
      const offset = messages.length;
      await action('inspect-workflow-markdown');
      const value = messages.slice(offset).find(message => message.type === 'workflow-markdown.state');
      if (value && predicate(value)) return value;
    }
    throw new Error('Revision native DOM did not settle');
  }
  try {
    await state(value => value.visible.length > 0);
    for (const surface of ['direct', 'history']) for (const [name, value] of cases) {
      const document = graph();
      document.hash += `-${surface}-${name}`;
      if (surface === 'history') {
        document.presentation_state = require('../../out/presentationHistory').decodePresentationState(history(value));
      }
      await post({ type: 'graph', document, style: 'smooth-curves', testMode: true });
      if (surface === 'direct') await post({ type: 'run.frame', frame: {
        type: 'run.event', version: 'yawr.stdio/v1', runID: 'revision',
        event: { kind: 'step/failed', run_id: 'revision', sequence: 1, payload: {
          qualified_node_id: 'report', invocation: 1, display_presentation: value,
          output: { content: marker }, error: failure,
        } },
      } });
      await action('select-node', 'report');
      await action('click-button', 'Rendered');
      let current = await state(value => value.markdown);
      assert.equal(current.markdown.text.includes(marker), before, `${surface}/${name} rendered`);
      await action('click-button', 'Raw');
      current = await state(value => value.markdown && (before ? value.markdown.raw === marker : value.markdown.copyDisabled));
      assert.equal(current.markdown.raw, before ? marker : undefined, `${surface}/${name} Raw`);
      await vscode.env.clipboard.writeText('revision clipboard sentinel');
      await action('click-button', 'Copy Markdown');
      assert.equal(await vscode.env.clipboard.readText(), before ? marker : 'revision clipboard sentinel');
      if (!before) assert.ok(!JSON.stringify(current.runtimeNodes).includes(marker));
      if (surface === 'direct') assert.equal(current.runtimeNodes.report.error, failure);
      results.push({ surface, name, raw: current.markdown.raw, copyDisabled: current.markdown.copyDisabled,
        contentRetained: JSON.stringify(current.runtimeNodes).includes(marker) });
    }
    assert.ok(!messages.some(message => /^(run|session)\.(start|command)$/.test(message.type)));
  } finally { panel.dispose(); }
  const standalone = await standaloneVerify(vscode, cases, before);
  const evidence = { mode: before ? 'before' : 'after', results, standalone };
  fs.writeFileSync(path.join(root, 'display-metadata-native-results.json'), JSON.stringify(evidence, null, 2));
  return evidence;
};
async function standaloneVerify(vscode, cases, before) {
  const staticRoot = vscode.Uri.file(path.join(value('CORE_ROOT'), 'internal', 'serve', 'static'));
  const panel = vscode.window.createWebviewPanel('displayMetadataStandalone', 'Synthetic display metadata',
    vscode.ViewColumn.Active, { enableScripts: true, localResourceRoots: [staticRoot] });
  let html = fs.readFileSync(path.join(staticRoot.fsPath, 'preview.html'), 'utf8');
  const mapping = {};
  for (const asset of ['vendor/preview.js', 'highlighting/highlighter.js', 'highlighting/worker.js']) {
    mapping['/preview/assets/' + asset] = panel.webview.asWebviewUri(vscode.Uri.joinPath(staticRoot, ...asset.split('/'))).toString();
    html = html.replaceAll(`from '/preview/assets/${asset}'`, `from '${mapping['/preview/assets/' + asset]}'`);
  }
  const document = graph();
  document.nodes = cases.map(([name]) => ({ ...graph().nodes[0], id: name,
    data: { ...graph().nodes[0].data, id: name } }));
  document.presentation_state = history(metadata);
  document.presentation_state.occurrences = cases.map(([name, value]) => ({
    ...history(value).occurrences[0], identity: { qualified_node_id: name, invocation: 1 },
  }));
  const fixture = JSON.stringify({ document, mapping, names: cases.map(([name]) => name), marker, before }).replace(/</g, '\\u003c');
  const nonce = 'displayMetadataNative';
  const shim = `<script nonce="${nonce}">
const fixture=${fixture},api=acquireVsCodeApi(),requests=[],copies=[];
const originalFetch=globalThis.fetch.bind(globalThis);
globalThis.fetch=async(input,options)=>{
 const url=typeof input==='string'?input:String(input.url||input);requests.push({url,method:options?.method||'GET'});
 if(fixture.mapping[url])return originalFetch(fixture.mapping[url],options);
 if(url.startsWith('/runs/')&&url.endsWith('/document'))return new Response(JSON.stringify(fixture.document),{headers:{'Content-Type':'application/json',ETag:'"synthetic"'}});
 if(url.startsWith('/debug-profiles'))return new Response('{"profiles":[]}');
 throw new Error('Unexpected request '+url);
};
Object.defineProperty(navigator,'clipboard',{value:{writeText:async text=>copies.push(text)}});
globalThis.EventSource=class{constructor(){this.readyState=1;setTimeout(()=>this.onopen?.(),20)}addEventListener(){}removeEventListener(){}close(){}};
const url=new URL(location.href);url.searchParams.set('runID','revision');url.searchParams.delete('runbookPath');history.replaceState(null,'',url);
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const wait=async fn=>{for(let i=0;i<150;i++){const v=fn();if(v)return v;await sleep(60)}throw new Error('Standalone revision DOM timeout')};
(async()=>{try{
 const results=[];
 for(const name of fixture.names){
  const node=await wait(()=>Array.from(document.querySelectorAll('.react-flow__node')).find(n=>n.getAttribute('data-id')===name));
  node.dispatchEvent(new MouseEvent('click',{bubbles:true}));
  const output=await wait(()=>document.querySelector('.panel .markdown-output'));await sleep(100);
  const button=text=>Array.from(output.querySelectorAll('button')).find(b=>b.textContent===text);
  button('Rendered').click();await sleep(100);
  const rendered=output.textContent.includes(fixture.marker);
  button('Raw').click();await sleep(100);button('Copy Markdown').click();await sleep(50);
  results.push({name,rendered,raw:output.querySelector('.markdown-raw')?.textContent,copyDisabled:button('Copy Markdown').disabled,
   genericLeak:document.querySelector('.panel').textContent.includes(fixture.marker)});
 }
 api.postMessage({type:'display-revision',results,copies,requests});
}catch(error){api.postMessage({type:'display-revision',error:String(error),text:document.body.textContent})}})();
</script>`;
  html = html.replace('<head>', `<head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}' ${panel.webview.cspSource}; style-src 'unsafe-inline' ${panel.webview.cspSource}; connect-src ${panel.webview.cspSource}; worker-src blob:;">`);
  html = html.replace('<script type="module">', shim + `<script type="module" nonce="${nonce}">`);
  try {
    const result = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('Standalone revision timeout')), 40000);
      panel.webview.onDidReceiveMessage(value => { if (value.type === 'display-revision') { clearTimeout(timer); resolve(value); } });
      panel.webview.html = html;
    });
    assert.equal(result.error, undefined);
    for (const item of result.results) {
      assert.equal(item.rendered, before, item.name);
      assert.equal(item.raw, before ? marker : undefined, item.name);
      assert.equal(item.copyDisabled, !before, item.name);
      assert.equal(item.genericLeak, before, item.name);
    }
    assert.deepEqual(result.copies, before ? cases.map(() => marker) : []);
    assert.ok(result.requests.every(request => request.method === 'GET'));
    return result;
  } finally { panel.dispose(); }
}
