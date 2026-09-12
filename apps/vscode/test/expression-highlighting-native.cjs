const vscode = require('vscode');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { execFile } = require('node:child_process');
const { promisify } = require('node:util');
const { createHash } = require('node:crypto');
const { value: environmentValue } = require('../scripts/environment.cjs');
const { loadGraphDocument, parseGraphDocument } = require('../out/directGraphPreview');
const { resolveRunPackageMapPath } = require('../out/runHandoff');
const { resolveExpressionPresentation } = require('../out/expressionPresentationClient');
const fixture = require('./fixtures/expression-contract.json');
const regexFixture = require('./fixtures/expression-contract-v2.json');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(fn, label) {
  for (let i = 0; i < 60; i++) { const result = await fn(); if (result) return result; await sleep(250); }
  throw new Error('Timeout: ' + label);
}
const metadata = values => ({ version: 1, grammar_version: 'yawr-expression/v2', values });
const value = (index, pointer) => ({ ...fixture[index].expected, path: pointer });
exports.run = async () => {
  const config = JSON.parse(fs.readFileSync(environmentValue('EXPRESSION_NATIVE_CONFIG'), 'utf8'));
  const root = environmentValue('HIGHLIGHTING_TEST_ROOT');
  const extension = vscode.extensions.getExtension('ormasoftchile.yawr-preview');
  assert.ok(extension); await extension.activate();
  config.helper ||= require('../out/presentationClient').bundledPresentationHelper(extension.extensionPath);
  const results = { mode: config.mode, cases: [] };
  let cdp;
  let yamlBaseline, yamlURI;
  const yamlCompletions = async () => {
    const list = await vscode.commands.executeCommand('vscode.executeCompletionItemProvider', yamlURI, new vscode.Position(0, 11));
    return list?.items.map(item => typeof item.label === 'string' ? item.label : item.label.label) ?? [];
  };
  try {
    const capabilities = JSON.parse((await promisify(execFile)(config.helper,
      ['presentation', 'expressions', 'capabilities'], { maxBuffer: 1024 * 1024 })).stdout);
    require('../out/expressionPresentationProtocol').decodeExpressionCapabilities(capabilities);
    if (config.requiredGrammar) assert.equal(capabilities.grammar_version, config.requiredGrammar);
    results.grammar = capabilities.grammar_version;
    if (config.yamlExtension) {
      const yaml = vscode.extensions.getExtension('redhat.vscode-yaml');
      assert.ok(yaml); await yaml.activate();
      yamlURI = vscode.Uri.file(path.join(root, 'workspace', 'completion.yaml'));
      fs.writeFileSync(yamlURI.fsPath, 'localMode: \n');
      await vscode.window.showTextDocument(await vscode.workspace.openTextDocument(yamlURI));
      yamlBaseline = await until(async () => {
        const labels = await yamlCompletions();
        return labels.some(label => label.includes('offline-schema-only-value')) ? labels : undefined;
      }, 'existing YAML provider baseline');
    }
    if (config.mode === 'contract') {
      // This mode exercises the production renderer with the agreed wire fixture, not a new producer.
      const source = path.join(root, 'workspace', 'contract.runbook.yaml');
      fs.writeFileSync(source, 'apiVersion: yawr.runbook/v1\nid: expression-contract\nname: Expression contract\nflow:\n  - step:\n      id: ordinary\n      type: noop\n');
      const execute = (binary, args) => promisify(execFile)(binary, args, { cwd: path.dirname(source), maxBuffer: 16 * 1024 * 1024 });
      const baseline = await loadGraphDocument(config.helper, source, execute);
      const original = baseline.nodes.find(node => node.data.details);
      assert.ok(original);
      const htmlPrefix = '<img src=x onerror=alert(1)> ';
      const nestedPayload = { redacted: true, code: fixture[1].text, other: 'must remain visible',
        'x~/': [{ name: 'authored data', redacted: true, value: fixture[1].text }] };
      const namedDetails = (kind, field) => ({ kind, [field]: [
        { name: 'nested', value: nestedPayload },
        { name: 'secret', redacted: true, value: { 'x~/': [fixture[1].text], code: 'NEVER_DISPLAY_SECRET' } },
      ], expression_presentation: metadata([value(1, `/${field}/0/value/code`), value(1, `/${field}/0/value/x~0~1/0/value`),
        value(1, `/${field}/1/value/x~0~1/0`)]) });
      const specs = [
        { kind: 'branch', common: { when: fixture[0].text }, arms: [{ condition: fixture[0].text, steps: 1 }],
          expression_presentation: metadata([value(0, '/common/when'), value(0, '/arms/0/condition')]) },
        { kind: 'choice', prompt: fixture[1].text, options: [{ label: fixture[1].text, value: '${not.an.expression}', hint: fixture[1].text }],
          expression_presentation: metadata([value(1, '/prompt'), value(1, '/options/0/label'), value(1, '/options/0/hint')]) },
        { ...namedDetails('tool', 'arguments'), tool: 'missing', action: 'query' },
        { kind: 'display', content: htmlPrefix + fixture[1].text,
          expression_presentation: metadata([{
            ...value(1, '/content'), text_length: htmlPrefix.length + fixture[1].text.length,
            text_digest: 'sha256:' + createHash('sha256').update(htmlPrefix + fixture[1].text).digest('hex'),
            tokens: fixture[1].expected.tokens.map(t => ({ ...t, start: t.start + htmlPrefix.length, end: t.end + htmlPrefix.length })),
          }]) },
        { ...namedDetails('host_action', 'request'), capability: 'offline.contract' },
        { ...namedDetails('include', 'bindings'), reference: 'offline-contract.runbook.yaml' },
        { kind: 'assert', assertions: [
          { type: 'matches', subject: 'plain', expected: regexFixture[0].text },
          { type: 'eq', subject: regexFixture[0].text, expected: regexFixture[0].text },
        ], expression_presentation: { version: 1, grammar_version: 'yawr-expression/v2',
          values: [{ ...regexFixture[0].expected, path: '/assertions/0/expected' }] } },
      ];
      baseline.nodes = specs.map((details, index) => ({ ...structuredClone(original), id: `contract-${index}`,
        data: { ...structuredClone(original.data), id: `contract-${index}`, kind: details.kind, details } }));
      baseline.edges = [];
      const graph = parseGraphDocument(JSON.stringify(baseline));
      const panel = vscode.window.createWebviewPanel('yawrExpressionContract', 'Expression contract renderer', vscode.ViewColumn.One,
        { enableScripts: true, localResourceRoots: [vscode.Uri.file(extension.extensionPath)] });
      const asset = relative => panel.webview.asWebviewUri(vscode.Uri.file(path.join(extension.extensionPath, relative))).toString();
      let receiver;
      const subscription = panel.webview.onDidReceiveMessage(message => receiver?.(message));
      try {
        for (const theme of ['vscode-light', 'vscode-dark', 'vscode-high-contrast', 'vscode-high-contrast-light']) {
          const result = new Promise((resolve, reject) => {
            const timer = setTimeout(() => reject(new Error('Contract renderer timed out')), 15000);
            receiver = message => {
              if (message.type === 'ready') void panel.webview.postMessage({ type: 'graph', graph, theme });
              if (message.type === 'rendered') { clearTimeout(timer); resolve(message); }
            };
          });
          const nonce = 'isolatedExpressionContract';
          panel.webview.html = `<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-${nonce}'; style-src ${panel.webview.cspSource}; style-src-attr 'unsafe-inline'; connect-src ${panel.webview.cspSource}; worker-src blob:;"><link rel="stylesheet" href="${asset('.vscode-test\\highlighting-ui.css')}"></head><body class="${theme}" data-worker="${asset('media\\highlighting-worker.js')}"><div id="root"></div><script nonce="${nonce}" src="${asset('.vscode-test\\highlighting-ui.js')}"></script></body></html>`;
          const rendered = await result;
          for (const pointer of ['/common/when', '/arms/0/condition', '/prompt', '/options/0/label', '/options/0/hint', '/content',
            '/assertions/0/expected',
            ...['arguments', 'request', 'bindings'].flatMap(field => [`/${field}/0/value/code`, `/${field}/0/value/x~0~1/0/value`])]) {
            const expression = rendered.expressions.find(item => item.path === pointer);
            assert.ok(expression?.tokens.length, pointer);
            assert.ok(new Set(expression.tokens.map(t => t.color)).size >= (pointer === '/assertions/0/expected' ? 2 : 3),
              pointer + ' existing core class colors');
          }
          assert.doesNotMatch(rendered.text, /NEVER_DISPLAY_SECRET/);
          assert.match(rendered.text, /must remain visible/);
          assert.match(rendered.text, /authored data/);
          assert.ok(!rendered.expressions.some(item => /^\/(?:arguments|request|bindings)\/1\//.test(item.path)));
          assert.doesNotMatch(JSON.stringify(rendered.copies), /NEVER_DISPLAY_SECRET/);
          assert.ok(!rendered.expressions.some(item => item.path === '/options/0/value'));
          assert.ok(!rendered.expressions.some(item => item.path === '/assertions/1/expected'));
          const regex = rendered.expressions.find(item => item.path === '/assertions/0/expected');
          assert.deepEqual(regex.tokens.map(token => ({ text: token.text, class: token.class })),
            regexFixture[0].expected.tokens.map(token => ({ text: regexFixture[0].text.slice(token.start, token.end), class: token.class })));
          const palette = { 'vscode-light': 'light', 'vscode-dark': 'dark', 'vscode-high-contrast': 'hc', 'vscode-high-contrast-light': 'hc-light' }[theme];
          const rgb = hex => `rgb(${[1, 3, 5].map(start => parseInt(hex.slice(start, start + 2), 16)).join(', ')})`;
          for (const token of regex.tokens) assert.equal(token.color,
            rgb(require('../out/expressionPresentationColors').expressionColor(token.class, palette)));
          assert.ok(!rendered.html.includes('<img src='), 'Authored HTML is escaped text');
          results.cases.push({ theme, expressions: rendered.expressions });
        }
      } finally { subscription.dispose(); panel.dispose(); }
      results.static = await require('./helpers/standalone-expressions.cjs').verify(vscode, graph, ['contract-0', 'contract-1', 'contract-2', 'contract-4', 'contract-5', 'contract-6'],
        config.coreRoot || path.resolve(extension.extensionPath, '..', 'yawr'), root);
      for (const [field, nodeID] of [['arguments', 'contract-2'], ['request', 'contract-4'], ['bindings', 'contract-5']]) {
        const observation = results.static.observations.find(item => item.nodeID === nodeID);
        assert.ok(observation.values.some(item => item.path === `/${field}/0/value/code` && item.tokens.length));
        assert.ok(observation.values.some(item => item.path === `/${field}/0/value/x~0~1/0/value` && item.tokens.length));
        assert.ok(!observation.values.some(item => item.path.startsWith(`/${field}/1/`)));
        assert.doesNotMatch(observation.text, /NEVER_DISPLAY_SECRET/);
      }
      const regex = results.static.observations.find(item => item.nodeID === 'contract-6').values.find(item => item.path === '/assertions/0/expected');
      assert.deepEqual(regex.tokens.map(token => ({ text: token.text, class: token.class })),
        regexFixture[0].expected.tokens.map(token => ({ text: regexFixture[0].text.slice(token.start, token.end), class: token.class })));
    } else {
      assert.equal(config.mode, 'actual');
      const settings = vscode.workspace.getConfiguration('yawr');
      const packageMap = resolveRunPackageMapPath(config.projectRoot, config.packageMap).path;
      await settings.update('highlighting.developmentHelperPath', config.helper, vscode.ConfigurationTarget.Global);
      await settings.update('binaryPath', config.helper, vscode.ConfigurationTarget.Workspace);
      await settings.update('packageMap', config.relativePackageMap ? config.packageMap : packageMap, vscode.ConfigurationTarget.Workspace);
      await settings.update('highlighting.enabled', true, vscode.ConfigurationTarget.Workspace);
      cdp = await require('./helpers/highlighting-cdp.cjs').connect(Number(environmentValue('HIGHLIGHTING_CDP_PORT')));
      const execute = (binary, args) => promisify(execFile)(binary, args, { cwd: config.projectRoot, maxBuffer: 16 * 1024 * 1024 });
      for (const entry of config.cases) {
        const graph = await loadGraphDocument(config.helper, entry.runbook, execute, packageMap);
        const node = graph.nodes.find(node => node.id === entry.nodeID);
        assert.ok(node?.data.details?.expression_presentation?.values.length, 'Actual producer emitted expression detail metadata');
        const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(entry.runbook));
        const request = { schema_version: 'yawr.presentation-resolve/v1', request_id: 'native-actual', context: {
          project_root: config.projectRoot, package_map_path: packageMap, entrypoint_path: entry.runbook, generation: 1,
        }, document: { uri: doc.uri.toString(), path: entry.runbook, version: doc.version, text: doc.getText() }, overlays: [] };
        const expressions = await resolveExpressionPresentation(config.helper, request);
        assert.equal(expressions?.status, 'resolved', 'New expression command must be exercised, never a stale helper');
        if (config.requiredGrammar) {
          assert.equal(expressions.grammar_version, config.requiredGrammar);
          assert.equal(node.data.details.expression_presentation.grammar_version, config.requiredGrammar);
        }
        assert.ok(expressions.regions.some(r => r.tokens.length));
        const editor = await vscode.window.showTextDocument(doc);
        const mixed = entry.mixed ? await require('./helpers/mixed-highlighting.cjs').inspectMixed(config.helper, request, entry.nodeID) : undefined;
        if (mixed) {
          assert.equal(mixed.status, 'resolved');
          assert.ok(mixed.region && mixed.expression?.tokens.length, 'Actual metadata binds both channels to the same source scalar');
          assert.ok(mixed.baseline.hostCount > 0 && mixed.combined.hostCount > 0 && mixed.comparedCodeUnits > 0);
          assert.ok(mixed.hostTokens.some(token => token.text.includes('let')));
          assert.ok(mixed.hostTokens.some(token => token.text.includes('fabric:/')));
          assert.ok(mixed.expressionTokens.some(token => token.expressionClass === 'interpolation' && token.text === '${'));
          assert.ok(mixed.expressionTokens.some(token => token.expressionClass === 'variable' && token.text === 'start_time'));
          if (entry.expectedHostCount !== undefined) assert.equal(mixed.combined.hostCount, entry.expectedHostCount);
          if (entry.expectedCodeUnits !== undefined) assert.equal(mixed.comparedCodeUnits, entry.expectedCodeUnits);
        }
        const selected = entry.expressionPath
          ? expressions.regions.find(r => r.yaml_path === entry.expressionPath)
          : mixed?.region ?? expressions.regions.find(r => r.yaml_path.endsWith('/when') || r.yaml_path.endsWith('/condition')) ?? expressions.regions[0];
        assert.ok(selected, 'Requested authored expression region exists');
        if (entry.requiredExpressionMode) assert.equal(selected.mode, entry.requiredExpressionMode);
        for (const pointer of entry.forbiddenExpressionPaths ?? []) {
          assert.ok(!expressions.regions.some(region => region.yaml_path === pointer), `No editor region for ${pointer}`);
        }
        const requiredExpressionTokens = [];
        if (entry.requiredExpressionTokens) {
          const mapped = require('../out/expressionPresentationScalar').resolveExpressionScalars(doc.getText(), expressions)
            .find(item => item.expression.yaml_path === selected.yaml_path);
          assert.ok(mapped, 'Nested expression uses the verified production YAML mapping');
          if (entry.scalarAnchor) {
            const source = doc.getText(), anchor = source.indexOf('&' + entry.scalarAnchor);
            assert.ok(anchor >= 0 && selected.range.start > anchor + entry.scalarAnchor.length);
            const spans = require('../out/presentationScalar').sourceSpans(mapped,
              require('../out/expressionPresentationColors').expressionSpans(selected, 'dark'), source);
            assert.ok(spans.length && spans.every(span => span.start > selected.range.start && span.end < selected.range.end),
              'Quoted anchored pattern paints body only, never anchor or YAML quotes');
          }
          const colored = require('../out/expressionPresentationColors').expressionSpans(selected, 'dark');
          for (const expected of entry.requiredExpressionTokens) {
            const token = colored.find(item => item.expressionClass === expected.class &&
              mapped.text.slice(item.start, item.end) === expected.text);
            assert.ok(token, `Core emits nested ${expected.class}: ${expected.text}`);
            requiredExpressionTokens.push({ text: expected.text, class: expected.class, color: token.color });
          }
        }
        editor.revealRange(new vscode.Range(doc.positionAt(selected.range.start), doc.positionAt(selected.range.end)), vscode.TextEditorRevealType.InCenter);
        await until(async () => /(?:code (?:partially )?highlighted|expressions highlighted)/.test(await cdp.evaluate(`document.querySelector('.statusbar')?.textContent || ''`)), 'actual expression editor');
        const editorTokens = await until(async () => {
          const rows = await cdp.evaluate(`Array.from(document.querySelectorAll('.view-line span')).filter(e=>String(e.className).includes('ced-')).map(e=>({text:e.textContent,color:getComputedStyle(e).color}))`);
          results.editorProbe = { runbook: entry.runbook,
            projectRoot: require('../out/presentationContext').presentationProjectRoot(entry.runbook,
              vscode.workspace.workspaceFolders.map(folder => folder.uri.fsPath), extension.extensionPath,
              vscode.workspace.getConfiguration('yawr', doc.uri).get('packageMap', '')),
            status: await cdp.evaluate(`document.querySelector('.statusbar')?.textContent || ''`), rows };
          if (mixed) {
            const rgb = hex => `rgb(${[1, 3, 5].map(start => parseInt(hex.slice(start, start + 2), 16)).join(', ')})`;
            const required = [
              mixed.hostTokens.find(token => token.text === 'let'),
              mixed.hostTokens.find(token => token.text.includes('fabric:/')),
              mixed.expressionTokens.find(token => token.text === 'start_time'),
              mixed.expressionTokens.find(token => token.text === '${'),
            ];
            if (!required.every(token => token && rows.some(row => row.text.replace(/\u00a0/g, ' ').includes(token.text) && row.color === rgb(token.color)))) return undefined;
          }
          if (requiredExpressionTokens.length) {
            const rgb = hex => `rgb(${[1, 3, 5].map(start => parseInt(hex.slice(start, start + 2), 16)).join(', ')})`;
            if (!requiredExpressionTokens.every(token => rows.some(row =>
              row.text.replace(/\u00a0/g, ' ').includes(token.text) && row.color === rgb(token.color)))) return undefined;
          }
          return rows.length ? rows : undefined;
        }, 'actual decorated editor text');
        const panel = await vscode.commands.executeCommand('yawr.test.openDirectGraphPanel', entry.runbook);
        let inspector;
        const observation = { runbook: entry.runbook, nodeID: entry.nodeID, editorTokens, mixed, requiredExpressionTokens, messages: {}, states: [],
          editorStatus: results.editorProbe.status, editorProjectRoot: results.editorProbe.projectRoot,
          configuredHelper: vscode.workspace.getConfiguration('yawr', doc.uri).get('highlighting.developmentHelperPath') };
        results.cases.push(observation);
        const originalPost = panel.webview.postMessage.bind(panel.webview);
        panel.webview.postMessage = message => {
          if (message.type === 'graph') observation.publishedExpressions = message.document.nodes.find(n => n.id === entry.nodeID)?.data.details?.expression_presentation;
          return originalPost(message);
        };
        const subscription = panel.webview.onDidReceiveMessage(message => {
          observation.messages[message.type] = (observation.messages[message.type] || 0) + 1;
          if (message.type === 'inspector.state') observation.states.push(message);
          if (message.type === 'rendered') void panel.webview.postMessage({ type: 'test.action', action: 'select-node', name: entry.nodeID });
          if (message.type === 'inspector.expressions') observation.inspector = inspector = message.values;
        });
        try {
          await until(async () => {
            await panel.webview.postMessage({ type: 'test.action', action: 'select-node', name: entry.nodeID });
            await panel.webview.postMessage({ type: 'test.action', action: 'inspect-expressions' });
            return inspector?.some(value => value.tokens.length) ? inspector : undefined;
          }, 'actual production ordinary/code inspector');
          const expectedPaths = node.data.details.expression_presentation.values.filter(v => v.tokens.length).map(v => v.path);
          assert.ok(inspector.some(v => expectedPaths.includes(v.path) && new Set(v.tokens.map(t => t.color)).size > 1));
          if (entry.detailPath) {
            const rendered = inspector.find(value => value.path === entry.detailPath);
            assert.ok(rendered, 'Actual expected pattern is visible in the production inspector');
            const rgb = hex => `rgb(${[1, 3, 5].map(start => parseInt(hex.slice(start, start + 2), 16)).join(', ')})`;
            for (const token of requiredExpressionTokens) assert.ok(rendered.tokens.some(actual =>
              actual.class === token.class && actual.text === token.text && actual.color === rgb(token.color)),
            `Inspector expected pattern ${token.class}: ${token.text}`);
          }
          observation.hash = graph.hash;
        } finally { subscription.dispose(); panel.dispose(); }
        if (config.relativePackageMap) {
          await vscode.window.showTextDocument(doc);
          await until(async () => /code highlighted.*expressions highlighted/.test(
            await cdp.evaluate(`document.querySelector('.statusbar')?.textContent || ''`)), 'code and expressions after graph established editor context');
          const diagnostic = await vscode.commands.executeCommand('yawr.showHighlightingDiagnostics');
          const samePath = (actual, expected) => assert.equal(vscode.Uri.file(actual).fsPath, vscode.Uri.file(expected).fsPath);
          samePath(diagnostic.context.projectRoot, config.projectRoot);
          samePath(diagnostic.context.entrypoint, entry.runbook);
          samePath(diagnostic.context.packageMap, packageMap);
          assert.equal(diagnostic.context.knownEntrypoint, true);
          assert.equal(diagnostic.code.status, 'resolved');
          assert.equal(diagnostic.code.timings.capabilities.reason, 'ok');
          assert.equal(diagnostic.code.timings.resolve.reason, 'ok');
          assert.equal(diagnostic.expressions.status, 'resolved');
          assert.equal(diagnostic.helper.path, config.helper);
          observation.afterGraphDiagnostic = diagnostic;
          await vscode.window.showTextDocument(doc);
        }
        if (!config.editorContextOnly) {
          observation.static = await require('./helpers/standalone-expressions.cjs').verify(vscode, graph, [entry.nodeID],
            config.coreRoot || path.resolve(extension.extensionPath, '..', 'yawr'), root, { safeEvidence: config.safeEvidence });
          if (entry.detailPath) {
            const rendered = observation.static.observations[0].values.find(value => value.path === entry.detailPath);
            assert.ok(rendered, 'Actual expected pattern is visible in standalone');
            const rgb = hex => `rgb(${[1, 3, 5].map(start => parseInt(hex.slice(start, start + 2), 16)).join(', ')})`;
            for (const token of requiredExpressionTokens) assert.ok(rendered.tokens.some(actual =>
              actual.class === token.class && actual.text === token.text && actual.color === rgb(token.color)),
            `Standalone expected pattern ${token.class}: ${token.text}`);
          }
        }
      }
    }
    if (yamlBaseline) {
      assert.deepEqual(await yamlCompletions(), yamlBaseline, 'YAML provider still owns YAML completion');
      assert.ok(vscode.languages.getDiagnostics(yamlURI).some(d => d.message.includes('requiredLocalProperty')));
      results.yamlProviders = { preserved: true, completions: yamlBaseline };
    }
  } finally {
    cdp?.close();
    const evidence = config.safeEvidence ? { mode: results.mode, grammar: results.grammar, cases: results.cases.map(item => ({
      runbook: item.runbook, nodeID: item.nodeID, editorStatus: item.editorStatus, editorProjectRoot: item.editorProjectRoot,
      configuredHelper: item.configuredHelper, afterGraphDiagnostic: item.afterGraphDiagnostic,
      hostCount: item.mixed?.combined.hostCount, comparedCodeUnits: item.mixed?.comparedCodeUnits,
      expressionStatus: item.mixed?.expressionStatus,
      requiredExpressionTokens: item.requiredExpressionTokens,
      inspectorTokenChecks: !!item.inspector, standaloneTokenChecks: !!item.static,
    })) } : results;
    fs.writeFileSync(path.join(root, 'expression-native-results.json'), JSON.stringify(evidence, null, 2));
  }
  console.log(`Expression native ${config.mode}: ${results.cases.length} cases passed`);
};
