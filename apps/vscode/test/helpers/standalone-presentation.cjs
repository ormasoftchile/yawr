const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
exports.verify = async function verify(vscode, graph, coreRoot, evidenceRoot) {
  assert.ok(coreRoot, 'Supply YAWR_CORE_ROOT for current production standalone assets');
  const staticRoot = vscode.Uri.file(path.join(coreRoot, 'internal', 'serve', 'static'));
  const panel = vscode.window.createWebviewPanel('yawrStandaloneAcceptance', 'Production standalone preview acceptance',
    vscode.ViewColumn.Beside, { enableScripts: true, localResourceRoots: [staticRoot] });
  const asset = relative => panel.webview.asWebviewUri(vscode.Uri.joinPath(staticRoot, ...relative.split('/'))).toString();
  let html = fs.readFileSync(path.join(staticRoot.fsPath, 'preview.html'), 'utf8');
  const nonce = 'isolatedStandaloneAcceptance';
  const encoded = JSON.stringify(graph).replace(/</g, '\\u003c');
  const mapping = {
    '/preview/assets/vendor/preview.js': asset('vendor/preview.js'),
    '/preview/assets/highlighting/highlighter.js': asset('highlighting/highlighter.js'),
    '/preview/assets/highlighting/worker.js': asset('highlighting/worker.js'),
  };
  for (const [source, target] of Object.entries(mapping)) html = html.replaceAll(`from '${source}'`, `from '${target}'`);
  html = html.replace('<script type="module">', `<script type="module" nonce="${nonce}">`);
  const shim = `<script nonce="${nonce}">
const testApi=acquireVsCodeApi(), frozen=${encoded}, assetMap=${JSON.stringify(mapping)}, requests=[], copies=[], documentResponses=[];
let finishRun, documentRequests=0;
const savedFetch=globalThis.fetch.bind(globalThis);
globalThis.fetch=async(input,options)=>{
  const url=typeof input==='string'?input:String(input.url||input); requests.push(url);
  if(assetMap[url]) return savedFetch(assetMap[url],options);
  if(url.startsWith('/runs/')&&url.endsWith('/document')) {
    const count=++documentRequests, conditional=options?.headers?.['If-None-Match'];
    const status=count===2?304:200, etag=count<3?'"checkpoint-before"':'"checkpoint-final"';
    documentResponses.push({status,conditional,etag});
    if(count===1)setTimeout(()=>[...document.querySelectorAll('button')].find(button=>button.textContent==='Load')?.click(),250);
    if(count===2)setTimeout(()=>finishRun?.(),100);
    return new Response(status===304?null:JSON.stringify(frozen),{status,headers:{'Content-Type':'application/json',ETag:etag}});
  }
  if(url.startsWith('/debug-profiles')) return new Response('{"profiles":[]}');
  if(url.startsWith('/')) throw new Error('Unexpected offline API request');
  return savedFetch(input,options);
};
Object.defineProperty(navigator,'clipboard',{value:{writeText:async text=>{copies.push(text)}}});
globalThis.EventSource=class {constructor(url){this.readyState=1;this.listeners=[];setTimeout(()=>this.onopen?.(),50);finishRun=()=>{const event={data:JSON.stringify({status:'completed',nodes:{}})};this.listeners.forEach(fn=>fn(event));this.onmessage?.(event)}}addEventListener(name,fn){if(name==='state')this.listeners.push(fn)}removeEventListener(){}close(){this.readyState=2}};
const url=new URL(location.href);url.searchParams.delete('runbookPath');url.searchParams.set('runID',frozen.presentation_state.run_id);history.replaceState(null,'',url);
let index=0,attempts=0;const observations=[],started=Date.now();
const timer=setInterval(()=>{
  const occurrence=frozen.presentation_state.occurrences[index];
  if(!occurrence){if(Date.now()-started<4000)return;clearInterval(timer);testApi.postMessage({type:'standalone',observations,copies,requests,documentResponses,text:document.body.textContent});return}
  const node=[...document.querySelectorAll('.react-flow__node')].find(el=>el.getAttribute('data-id')===occurrence.identity.qualified_node_id);
  if(node)node.dispatchEvent(new MouseEvent('click',{bubbles:true}));
  const codes=[...document.querySelectorAll('.panel .yawr-code pre code')],expected=occurrence.output.code;
  const output=codes.find(el=>el.textContent===expected&&el.querySelector('span'));
  if(output){document.querySelectorAll('.panel .yawr-code button').forEach(button=>button.click());observations.push({node:occurrence.identity.qualified_node_id,text:output.textContent,spans:output.querySelectorAll('span').length});index++;attempts=0}
  else if(attempts++>100){clearInterval(timer);testApi.postMessage({type:'standalone',error:'No tokenized standalone output',html:document.body.innerHTML,requests})}
},100);
</script>`;
  html = html.replace('<head>', `<head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}' ${panel.webview.cspSource}; style-src 'unsafe-inline' ${panel.webview.cspSource}; connect-src ${panel.webview.cspSource}; worker-src blob:;">`);
  html = html.replace('<script type="module"', shim + '<script type="module"');
  try {
    const result = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('Standalone production preview timed out')), 20000);
      panel.webview.onDidReceiveMessage(message => {
        if (message.type !== 'standalone') return;
        clearTimeout(timer); resolve(message);
      });
      panel.webview.html = html;
    });
    fs.writeFileSync(path.join(evidenceRoot, 'standalone-results.json'), JSON.stringify(result, null, 2));
    assert.equal(result.error, undefined);
    assert.equal(result.observations.length, 3);
    assert.ok(result.observations.every(item => item.spans > 0));
    assert.ok(result.observations.every(item => result.copies.includes(item.text)));
    assert.ok(!result.requests.some(request => request.includes('format=prose')), 'Frozen history never reads original source prose');
    assert.match(result.text, /Unavailable — not retained/);
    assert.deepEqual(result.documentResponses.map(response => response.status), [200, 304, 200]);
    assert.equal(result.documentResponses[0].conditional, undefined);
    assert.equal(result.documentResponses[1].conditional, '"checkpoint-before"');
    assert.equal(result.documentResponses[2].conditional, '"checkpoint-before"');
    return result;
  } finally { panel.dispose(); }
};
