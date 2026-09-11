const vscode = require('vscode');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { execFile } = require('node:child_process');
const { promisify } = require('node:util');
const { loadGraphDocument } = require('../out/directGraphPreview');
const { resolveRunPackageMapPath } = require('../out/runHandoff');
const { pickProjectRoot } = require('../out/projectRoot');
const { value } = require('../scripts/environment.cjs');

exports.run = async () => {
  const root = value('HIGHLIGHTING_TEST_ROOT');
  const runbook = value('PRESENTATION_AUTHORED_RUNBOOK');
  const project = pickProjectRoot(runbook, [value('PRESENTATION_PROJECT_ROOT')], path.dirname(runbook));
  const packageMap = resolveRunPackageMapPath(project, value('PRESENTATION_PACKAGE_MAP')).path;
  const execute = (binary, args) => promisify(execFile)(binary, args, { cwd: project, maxBuffer: 16 * 1024 * 1024 });
  const helper = value('PRESENTATION_HELPER');
  const baseline = await loadGraphDocument(helper, runbook, execute);
  const current = await loadGraphDocument(helper, runbook, execute, packageMap);
  const nodeID = value('PRESENTATION_NODE_ID') || 'rbwrite_summary';
  const node = graph => graph.nodes.find(node => node.id === nodeID);
  assert.ok(node(current), 'Actual selected authored node exists');
  assert.equal(node(baseline).data.details.code_presentation.reason, 'missing-dependency');
  assert.equal(node(current).data.details.code_presentation.origin, 'current');
  assert.equal(node(current).data.details.code_presentation.arguments.find(field => field.name === 'query').presentation.language, 'kql');
  assert.deepEqual(node(baseline).data.details.arguments, node(current).data.details.arguments);
  const query = node(current).data.details.arguments.find(item => item.name === 'query').value;
  assert.match(query, /\n\| where/);
  fs.writeFileSync(path.join(root, 'authored-baseline.graph.json'), JSON.stringify(baseline, null, 2));
  fs.writeFileSync(path.join(root, 'authored-current.graph.json'), JSON.stringify(current, null, 2));
  const extension = vscode.extensions.getExtension('ormasoftchile.yawr-preview');
  assert.ok(extension);
  await extension.activate();
  const config = vscode.workspace.getConfiguration('yawr');
  await config.update('packageMap', packageMap, vscode.ConfigurationTarget.Workspace);
  await config.update('binaryPath', helper, vscode.ConfigurationTarget.Workspace);
  await config.update('highlighting.enabled', true, vscode.ConfigurationTarget.Workspace);
  const production = await vscode.commands.executeCommand('yawr.test.openDirectGraphPanel', runbook);
  let publishedGraph;
  const originalPost = production.webview.postMessage.bind(production.webview);
  production.webview.postMessage = message => {
    if (message.type === 'graph') publishedGraph = message.document;
    return originalPost(message);
  };
  let productionSubscription;
  try {
    const inspector = await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('Production graph command did not reach authored inspector')), 25000);
      productionSubscription = production.webview.onDidReceiveMessage(message => {
        if (message.type === 'rendered') void production.webview.postMessage({ type: 'test.action', action: 'select-node', name: nodeID });
        if (message.type === 'inspector.state' && message.nodeID === nodeID) { clearTimeout(timeout); resolve(message); }
      });
    });
    assert.match(inspector.sections.join(' '), /Authored template/);
    assert.deepEqual(node(publishedGraph).data.details, node(current).data.details,
      'Unmocked production graph command forwards configured package map and actual authored metadata');
    fs.writeFileSync(path.join(root, 'production-command-results.json'), JSON.stringify({
      graphHash: publishedGraph.hash, envelope: node(publishedGraph).data.details.code_presentation, inspector,
    }, null, 2));
  } finally {
    productionSubscription?.dispose();
    production.dispose();
  }
  const assetRoot = vscode.Uri.file(extension.extensionPath);
  const panel = vscode.window.createWebviewPanel('yawrAuthoredAcceptance', 'Actual authored template acceptance',
    vscode.ViewColumn.One, { enableScripts: true, localResourceRoots: [assetRoot] });
  const url = relative => panel.webview.asWebviewUri(vscode.Uri.joinPath(assetRoot, ...relative.split('/'))).toString();
  let receiver;
  const rendered = graph => new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('Authored inspector did not render')), 20000);
    receiver = message => {
      if (message.type === 'ready') void panel.webview.postMessage({ type: 'graph', graph });
      if (message.type === 'rendered') { clearTimeout(timeout); resolve(message); }
    };
    const nonce = `isolatedActualAuthoredInspector${graph.hash}`;
    panel.webview.html = `<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}'; style-src ${panel.webview.cspSource}; style-src-attr 'unsafe-inline'; connect-src ${panel.webview.cspSource}; worker-src blob:;"><link rel="stylesheet" href="${url('.vscode-test/highlighting-ui.css')}"></head><body data-worker="${url('media/highlighting-worker.js')}"><div id="root"></div><script nonce="${nonce}" src="${url('.vscode-test/highlighting-ui.js')}"></script></body></html>`;
  });
  const subscription = panel.webview.onDidReceiveMessage(message => receiver?.(message));
  try {
    const before = await rendered(baseline);
    assert.match(before.text, /Authored template/);
    assert.equal(before.code.length, 0);
    const after = await rendered(current);
    const code = after.code.find(code => code.text === query);
    assert.ok(code?.spans > 0, 'Actual authored query rendered by production StepInspector and browser worker');
    assert.ok(code.colors.length > 1);
    assert.equal(code.whiteSpace, 'pre');
    assert.ok(after.copies.includes(query), 'Copy preserves original authored linebreaks and template values');
    const evidence = { project, packageMap, nodeID, beforeHash: baseline.hash, afterHash: current.hash,
      before: { reason: node(baseline).data.details.code_presentation.reason, code: before.code },
      after: { envelope: node(current).data.details.code_presentation, code, exactCopy: after.copies.includes(query) } };
    fs.writeFileSync(path.join(root, 'authored-panel-results.json'), JSON.stringify(evidence, null, 2));
    console.log(JSON.stringify(evidence, null, 2));
  } finally {
    subscription.dispose();
    panel.dispose();
  }
};
