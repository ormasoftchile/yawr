const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const { safeExpressionText } = require('../../out/expressionPresentationProtocol');
exports.verify = async function verify(vscode, graph, nodeIDs, coreRoot, evidenceRoot, options = {}) {
  const expected = Object.fromEntries(nodeIDs.map(id => {
    const details = graph.nodes.find(node => node.id === id)?.data.details;
    return [id, (details?.expression_presentation?.values ?? [])
      .filter(value => value.tokens.length && safeExpressionText(details, value.path) !== undefined).map(value => value.path)];
  }));
  const staticRoot = vscode.Uri.file(path.join(coreRoot, 'internal', 'serve', 'static'));
  const panel = vscode.window.createWebviewPanel('yawrStandaloneExpressions', 'Offline authored expression preview',
    vscode.ViewColumn.One, { enableScripts: true, localResourceRoots: [staticRoot] });
  const asset = relative => panel.webview.asWebviewUri(vscode.Uri.file(path.join(staticRoot.fsPath, relative))).toString();
  const mapping = Object.fromEntries(['vendor\\preview.js', 'highlighting\\highlighter.js', 'highlighting\\worker.js'].map(file =>
    ['/preview/assets/' + file.replaceAll('\\', '/'), asset(file)]));
  let html = fs.readFileSync(path.join(staticRoot.fsPath, 'preview.html'), 'utf8');
  for (const [source, target] of Object.entries(mapping)) html = html.replaceAll(`from '${source}'`, `from '${target}'`);
  const nonce = 'isolatedStaticExpressions';
  html = html.replace('<script type="module">', `<script type="module" nonce="${nonce}">`);
  const shim = `<script nonce="${nonce}">
const testApi=acquireVsCodeApi(), fixture=${JSON.stringify(graph).replaceAll('<', '\\u003c')}, ids=${JSON.stringify(nodeIDs)}, expected=${JSON.stringify(expected)}, assets=${JSON.stringify(mapping)}, requests=[], observations=[];
const savedFetch=globalThis.fetch.bind(globalThis);
globalThis.fetch=async(input,options)=>{
  const url=typeof input==='string'?input:String(input.url||input);requests.push(url);
  if(assets[url])return savedFetch(assets[url],options);
  if(url.startsWith('/preview/document?format=prose'))return new Response('<p>Authored source prose not used by expression renderer.</p>');
  if(url.startsWith('/preview/document?'))return new Response(JSON.stringify(fixture),{headers:{'Content-Type':'application/json'}});
  if(url.startsWith('/debug-profiles'))return new Response('{"profiles":[]}');
  if(url.startsWith('/'))throw new Error('Unexpected API; expression preview must never run tools');
  return savedFetch(input,options);
};
const url=new URL(location.href);url.searchParams.delete('runID');url.searchParams.set('runbookPath','offline-authored.runbook.yaml');history.replaceState(null,'',url);
let index=0,attempts=0;
const timer=setInterval(()=>{
  if(index===ids.length){clearInterval(timer);testApi.postMessage({type:'static-expressions',observations,requests});return}
  const node=[...document.querySelectorAll('.react-flow__node')].find(el=>el.getAttribute('data-id')===ids[index]);
  node?.dispatchEvent(new MouseEvent('click',{bubbles:true}));
  const values=[...document.querySelectorAll('.panel [data-expression-path]')].map(value=>({path:value.dataset.expressionPath,text:value.textContent,
    tokens:[...value.querySelectorAll('[data-expression-class]')].map(token=>({class:token.dataset.expressionClass,text:token.textContent,color:getComputedStyle(token).color}))}));
  if(expected[ids[index]].length && expected[ids[index]].every(path=>values.some(value=>value.path===path&&value.tokens.length))){
    observations.push({nodeID:ids[index],values,text:document.querySelector('.panel')?.textContent||''});index++;attempts=0}
  else if(attempts++>100){clearInterval(timer);testApi.postMessage({type:'static-expressions',error:'No static expression tokens',nodeID:ids[index],requests})}
},100);
</script>`;
  html = html.replace('<head>', `<head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}' ${panel.webview.cspSource}; style-src 'unsafe-inline' ${panel.webview.cspSource}; connect-src ${panel.webview.cspSource}; worker-src blob:;">`);
  html = html.replace('<script type="module"', shim + '<script type="module"');
  try {
    const result = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('Static expression preview timed out')), 20000);
      panel.webview.onDidReceiveMessage(message => {
        if (message.type !== 'static-expressions') return;
        clearTimeout(timer); resolve(message);
      });
      panel.webview.html = html;
    });
    const evidence = options.safeEvidence ? { error: result.error, observations: result.observations?.map(item => ({
      nodeID: item.nodeID, values: item.values.map(value => ({ path: value.path, tokenCount: value.tokens.length,
        classes: [...new Set(value.tokens.map(token => token.class))] })),
    })) } : result;
    fs.writeFileSync(path.join(evidenceRoot, `static-expressions-${nodeIDs[0]}.json`), JSON.stringify(evidence, null, 2));
    assert.equal(result.error, undefined);
    assert.equal(result.observations.length, nodeIDs.length);
    assert.ok(result.observations.every(o => o.values.some(v => new Set(v.tokens.map(t => t.color)).size > 1)));
    assert.ok(result.requests.every(url => !url.startsWith('/runs')), 'No execution calls');
    return result;
  } finally { panel.dispose(); }
};
