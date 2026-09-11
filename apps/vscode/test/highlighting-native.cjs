const vscode = require('vscode');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const root = value('HIGHLIGHTING_TEST_ROOT');
const workspace = path.join(root, 'workspace');
const results = [];
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(fn, label) {
  for (let i = 0; i < 160; i++) { const result = await fn(); if (result) return result; await sleep(250); }
  throw new Error('Timeout: ' + label);
}
async function check(name, fn) {
  try { results.push({ name, status: 'PASS', evidence: await fn() }); }
  catch (error) { results.push({ name, status: 'FAIL', error: error.stack }); throw error; }
  finally { fs.writeFileSync(path.join(root, 'results.json'), JSON.stringify(results, null, 2)); }
}
exports.run = async () => {
  const disposables = [];
  let cdp;
  try {
    const extension = vscode.extensions.getExtension('ormasoftchile.yawr-preview');
    assert.ok(extension); await extension.activate();
    assert.equal(path.resolve(extension.extensionPath).toLowerCase(), path.resolve(__dirname, '..').toLowerCase());
    const yaml = vscode.extensions.getExtension('redhat.vscode-yaml');
    assert.ok(yaml); await yaml.activate();
    const completionURI = vscode.Uri.file(path.join(workspace, 'completion.yaml'));
    fs.writeFileSync(completionURI.fsPath, 'localMode: \n\n');
    const completionDoc = await vscode.workspace.openTextDocument(completionURI);
    await vscode.window.showTextDocument(completionDoc);
    fs.writeFileSync(path.join(root, 'baseline-context.json'), JSON.stringify({
      language: completionDoc.languageId, uri: completionURI.toString(), workspace: vscode.workspace.workspaceFolders?.map(f => f.uri.toString()),
      schemas: vscode.workspace.getConfiguration('yaml', completionURI).get('schemas'), yaml: yaml.packageJSON.version,
    }, null, 2));
    const completions = async () => {
      const list = await vscode.commands.executeCommand('vscode.executeCompletionItemProvider', completionURI, new vscode.Position(0, 11));
      const labels = list?.items.map(item => typeof item.label === 'string' ? item.label : item.label.label) ?? [];
      fs.writeFileSync(path.join(root, 'completion-poll.json'), JSON.stringify({ labels, diagnostics: vscode.languages.getDiagnostics(completionURI).map(d => d.message) }));
      return labels;
    };
    let baseline;
    await check('actual YAML local schema completion and diagnostics before feature', async () => {
      baseline = await until(async () => { const labels = await completions(); return labels.some(l => l.includes('offline-schema-only-value')) ? labels : false; }, 'schema completion');
      await until(() => vscode.languages.getDiagnostics(completionURI).some(d => d.message.includes('requiredLocalProperty')), 'schema diagnostic');
      return { yaml: yaml.packageJSON.version, extensionPath: extension.extensionPath, completions: baseline };
    });
    cdp = await require('./helpers/highlighting-cdp.cjs').connect(Number(value('HIGHLIGHTING_CDP_PORT')));
    const vector = JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', 'presentation-contract.json')));
    const toolURI = vscode.Uri.file(path.join(workspace, 'db.tool.yaml'));
    const runURI = vscode.Uri.file(path.join(workspace, 'new.runbook.yaml'));
    fs.writeFileSync(toolURI.fsPath, vector.request.overlays[0].text);
    fs.writeFileSync(runURI.fsPath, vector.request.document.text);
    const tool = await vscode.workspace.openTextDocument(toolURI);
    const document = await vscode.workspace.openTextDocument(runURI);
    const editor = await vscode.window.showTextDocument(document);
    await vscode.workspace.getConfiguration('yawr').update('highlighting.enabled', true, vscode.ConfigurationTarget.Workspace);
    const status = () => cdp.evaluate(`document.querySelector('.statusbar')?.textContent || ''`);
    const waitHighlighted = () => until(async () => (await status()).includes('code highlighted'), 'production editor decorations');
    let rivalCalls = 0;
    const legend = new vscode.SemanticTokensLegend(['comment']);
    disposables.push(vscode.languages.registerDocumentSemanticTokensProvider({ language: 'yaml', scheme: 'file' }, {
      provideDocumentSemanticTokens(doc) {
        rivalCalls++; const builder = new vscode.SemanticTokensBuilder(legend);
        const offset = doc.getText().indexOf('SELECT');
        if (offset >= 0) { const pos = doc.positionAt(offset); builder.push(pos.line, pos.character, 6, 0, 0); }
        return builder.build();
      },
    }, legend));
    await check('actual helper metadata paints production editor; rival remains selected', async () => {
      await waitHighlighted();
      const semantic = await vscode.commands.executeCommand('vscode.provideDocumentSemanticTokens', runURI);
      assert.equal(semantic.data.length, 5); assert.ok(rivalCalls); assert.equal(document.languageId, 'yaml');
      const rendered = await cdp.evaluate(`Array.from(document.querySelectorAll('.view-line span')).filter(e=>e.textContent==='SELECT').map(e=>({text:e.textContent,color:getComputedStyle(e).color,className:e.className}))`);
      assert.ok(rendered.length);
      await cdp.screenshot(path.join(root, 'production-editor.png'));
      return { rendered, rivalCalls, status: await status() };
    });
    await check('production light dark and high-contrast palettes preserve rival tokens', async () => {
      const appearances = [];
      for (const name of ['Default Light Modern', 'Default Dark Modern', 'Default High Contrast', 'Default High Contrast Light']) {
        await vscode.workspace.getConfiguration('workbench').update('colorTheme', name, vscode.ConfigurationTarget.Workspace);
        await sleep(300); await waitHighlighted();
        const rows = await cdp.evaluate(`Array.from(document.querySelectorAll('.view-line span')).filter(e=>e.textContent==='SELECT').map(e=>({text:e.textContent,color:getComputedStyle(e).color,className:e.className}))`);
        assert.ok(rows.some(row => row.className.includes('ced-')), 'Production decoration survives theme change');
        appearances.push({ name, rows });
        assert.equal((await vscode.commands.executeCommand('vscode.provideDocumentSemanticTokens', runURI)).data.length, 5);
      }
      return appearances;
    });
    await check('dirty tool descriptor overlays refresh without save; folded quoted Unicode CRLF', async () => {
      const outcomes = [];
      for (const [language, value] of [['kql', '>-\r\n            let x = "🚀";\r\n            x\r\n'],
        ['powershell', '"Write-Output \\U0001F680"'], ['sql', "'SELECT ''x'';'"]]) {
        const editTool = new vscode.WorkspaceEdit();
        editTool.replace(toolURI, new vscode.Range(tool.positionAt(0), tool.positionAt(tool.getText().length)),
          vector.request.overlays[0].text.replace('language: sql', `language: ${language}`));
        await vscode.workspace.applyEdit(editTool);
        const source = vector.request.document.text.replace("'SELECT 1'\n", value.endsWith('\n') ? value : value + '\n');
        await editor.edit(builder => builder.setEndOfLine(value.includes('\r\n') ? vscode.EndOfLine.CRLF : vscode.EndOfLine.LF));
        await editor.edit(builder => builder.replace(new vscode.Range(document.positionAt(0), document.positionAt(document.getText().length)), source));
        await waitHighlighted();
        assert.ok(tool.isDirty); assert.ok(document.isDirty); assert.equal(document.languageId, 'yaml');
        outcomes.push({ language, version: document.version, status: await status() });
      }
      return outcomes;
    });
    await check('rapid obsolete edits are canceled and latest document version wins', async () => {
      await editor.edit(builder => builder.replace(new vscode.Range(document.positionAt(0), document.positionAt(document.getText().length)), vector.invalid_runbook_text));
      await editor.edit(builder => builder.replace(new vscode.Range(document.positionAt(0), document.positionAt(document.getText().length)), vector.request.document.text));
      await waitHighlighted();
      return { version: document.version, status: await status() };
    });
    await check('unknown metadata and invalid identity clear old paints automatically', async () => {
      const change = new vscode.WorkspaceEdit();
      change.replace(toolURI, new vscode.Range(tool.positionAt(0), tool.positionAt(tool.getText().length)),
        vector.request.overlays[0].text.replace('language: sql', 'language: future-language'));
      await vscode.workspace.applyEdit(change);
      await until(async () => (await status()).includes('unsupported-language'), 'unknown metadata fallback');
      await editor.edit(builder => builder.replace(new vscode.Range(document.positionAt(0), document.positionAt(document.getText().length)), vector.invalid_runbook_text));
      await until(async () => (await status()).includes('incomplete-source'), 'identity clears');
      return { status: await status() };
    });
    await check('feature disabled and baseline YAML language services preserved', async () => {
      await vscode.workspace.getConfiguration('yawr').update('highlighting.enabled', false, vscode.ConfigurationTarget.Workspace);
      await until(async () => (await status()).includes('highlighting disabled'), 'disabled');
      assert.deepEqual(await completions(), baseline);
      assert.ok(vscode.languages.getDiagnostics(completionURI).some(d => d.message.includes('requiredLocalProperty')));
      return { status: await status(), completionCount: baseline.length };
    });
    await check('production execution inspector renders actual source-deleted frozen outputs with offline browser workers', async () => {
      const frozenGraph = value('PRESENTATION_FROZEN_GRAPH');
      assert.ok(frozenGraph, 'Supply actual core-produced source-deleted frozen graph');
      const graph = require('../out/directGraphPreview').parseGraphDocument(fs.readFileSync(frozenGraph, 'utf8'));
      const assetRoot = vscode.Uri.file(path.resolve(__dirname, '..'));
      const panel = vscode.window.createWebviewPanel('yawrHighlightingAcceptance', 'Production frozen inspector acceptance',
        vscode.ViewColumn.Beside, { enableScripts: true, localResourceRoots: [assetRoot] });
      disposables.push(panel);
      const url = relative => panel.webview.asWebviewUri(vscode.Uri.joinPath(assetRoot, ...relative.split('/'))).toString();
      const nonce = 'isolatedProductionHighlightingAcceptance';
      const result = new Promise((resolve, reject) => {
        const timeout = setTimeout(() => reject(new Error('Production inspector worker did not render')), 15000);
        panel.webview.onDidReceiveMessage(message => {
          if (message.type === 'ready') void panel.webview.postMessage({ type: 'graph', graph });
          if (message.type === 'rendered') { clearTimeout(timeout); resolve(message); }
        }, undefined, disposables);
      });
      panel.webview.html = `<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}'; style-src ${panel.webview.cspSource}; style-src-attr 'unsafe-inline'; connect-src ${panel.webview.cspSource}; worker-src blob:;"><link rel="stylesheet" href="${url('.vscode-test/highlighting-ui.css')}"></head><body data-worker="${url('media/highlighting-worker.js')}"><div id="root"></div><script nonce="${nonce}" src="${url('.vscode-test/highlighting-ui.js')}"></script></body></html>`;
      const rendered = await result;
      fs.writeFileSync(path.join(root, 'production-inspector.json'), JSON.stringify(rendered, null, 2));
      const outputs = graph.presentation_state.occurrences.map(o => o.output.code);
      assert.equal(outputs.length, 3);
      for (const text of outputs) {
        assert.ok(rendered.code.some(code => code.text === text && code.spans > 0), 'Actual retained string tokenized in production component');
        assert.ok(rendered.copies.includes(text), 'Copy contains exact safe text');
      }
      assert.match(rendered.text, /Authored template/);
      assert.match(rendered.text, /Unavailable — not retained/);
      assert.ok(rendered.code.every(code => !code.label.includes('Executed query')));
      await cdp.screenshot(path.join(root, 'production-inspector.png'));
      return { actualRunID: graph.presentation_state.run_id, code: rendered.code, copies: rendered.copies };
    });
    await check('actual standalone preview renders frozen code outputs with fully local vendor assets', async () => {
      const graph = require('../out/directGraphPreview').parseGraphDocument(fs.readFileSync(value('PRESENTATION_FROZEN_GRAPH'), 'utf8'));
      return require('./helpers/standalone-presentation.cjs').verify(vscode, graph, value('CORE_ROOT'), root);
    });
  } finally {
    disposables.forEach(d => d.dispose()); cdp?.close();
    fs.writeFileSync(path.join(root, 'results.json'), JSON.stringify(results, null, 2));
  }
};
