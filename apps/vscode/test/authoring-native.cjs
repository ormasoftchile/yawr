const vscode = require('vscode');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { value } = require('../scripts/environment.cjs');
const { createHash } = require('node:crypto');
const fixture = require('./fixtures/authoring-contract.json');
const editRegressions = require('./fixtures/authoring-edit-regressions.json');
const includeFixture = require('./fixtures/authoring-include.json');
const YAML = require('yaml');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const label = item => typeof item.label === 'string' ? item.label : item.label.label;
const digest = text => createHash('sha256').update(text).digest('hex');
const yawrItems = list => list?.items.filter(item => item.detail?.startsWith('Yawr · ')) ?? [];
exports.run = async () => {
  const config = JSON.parse(fs.readFileSync(value('AUTHORING_NATIVE_CONFIG'), 'utf8'));
  const root = value('AUTHORING_TEST_ROOT'), workspace = path.join(root, 'workspace');
  const extension = vscode.extensions.getExtension('ormasoftchile.yawr-preview');
  assert.ok(extension); await extension.activate();
  const yaml = vscode.extensions.getExtension('redhat.vscode-yaml');
  assert.ok(yaml, 'Existing YAML provider is required, not replaced by a mock'); await yaml.activate();
  const { AuthoringClient } = require('../out/authoringClient');
  const { captureAuthoringContext, setPresentationEntrypoint } = require('../out/authoringContext');
  const client = new AuthoringClient();
  const evidence = { helperSHA256: digest(fs.readFileSync(config.helper)),
    callerFixture: config.callerFixture ?? 'actual-read-only-caller', cases: [] };
  let sequence = 0;
  const buffers = [];
  const until = async (fn, name) => {
    for (let n = 0; n < 40; n++) { const value = await fn(); if (value) return value; await sleep(150); }
    throw new Error(`Native authoring assertion timed out: ${name}`);
  };
  const unsaved = async (template, filename = path.join(workspace, `probe-${++sequence}.runbook.yaml`)) => {
    const offset = template.indexOf('|CURSOR|');
    assert.ok(offset >= 0);
    const source = template.replace('|CURSOR|', '');
    const uri = vscode.Uri.file(filename).with({ scheme: 'untitled' });
    let doc = await vscode.workspace.openTextDocument(uri);
    doc = await vscode.languages.setTextDocumentLanguage(doc, 'yaml');
    const editor = await vscode.window.showTextDocument(doc);
    await editor.edit(edit => {
      edit.setEndOfLine(source.includes('\r\n') ? vscode.EndOfLine.CRLF : vscode.EndOfLine.LF);
      edit.insert(new vscode.Position(0, 0), source);
    });
    assert.ok(doc.getText() === source, 'Native buffer preserves exact request newline bytes');
    editor.selection = new vscode.Selection(doc.positionAt(offset), doc.positionAt(offset));
    buffers.push(doc); await sleep(100);
    return { doc, editor, position: doc.positionAt(offset), source };
  };
  const completion = probe => vscode.commands.executeCommand('vscode.executeCompletionItemProvider', probe.doc.uri, probe.position);
  const signature = probe => vscode.commands.executeCommand('vscode.executeSignatureHelpProvider', probe.doc.uri, probe.position);
  const rawReply = async (probe, operation) => {
    const captured = captureAuthoringContext(probe.doc, extension.extensionPath, 700);
    return client.resolve(config.helper, { schema_version: 'authoring-request/v3', request_id: `native-${++sequence}`,
      operation, context: captured.context, document: captured.document, overlays: captured.overlays, position: probe.doc.offsetAt(probe.position) });
  };
  const accept = async (probe, expectedItem) => {
    const range = expectedItem.range;
    assert.ok(range instanceof vscode.Range, 'Native exact replacement range');
    const start = probe.doc.offsetAt(range.start), end = probe.doc.offsetAt(range.end);
    const expected = probe.source.slice(0, start) + expectedItem.insertText + probe.source.slice(end);
    assert.equal(typeof expectedItem.insertText, 'string');
    assert.equal(expectedItem.command, undefined); assert.equal(expectedItem.additionalTextEdits, undefined);
    await vscode.commands.executeCommand('editor.action.triggerSuggest');
    await sleep(700);
    await vscode.commands.executeCommand('acceptSelectedSuggestion');
    await until(() => probe.doc.getText() === expected, 'native completion acceptance changes only exact token');
    await vscode.commands.executeCommand('undo');
    assert.ok(probe.doc.getText() === probe.source, 'One native completion undo');
  };
  const setting = async (key, value) => {
    await vscode.workspace.getConfiguration('yawr').update(key, value, vscode.ConfigurationTarget.Workspace);
    await sleep(150);
  };
  try {
    fs.writeFileSync(path.join(workspace, 'db.tool.yaml'), fixture.base_request.overlays[0].text);
    const yamlProbe = await unsaved('localMode: |CURSOR|', path.join(workspace, 'yaml-coexistence.yaml'));
    const yamlBaseline = await until(async () => (await completion(yamlProbe))?.items.some(i => label(i).includes('offline-schema-only-value')),
      'YAML provider completion before Yawr probes');
    assert.ok(yamlBaseline);
    for (const vector of includeFixture.cases.filter(v => v.native)) {
      for (const crlf of [false, true]) {
        const nl = text => crlf ? text.replace(/\n/g, '\r\n') : text;
        const probe = await unsaved(nl(includeFixture.prefix + vector.source));
        const reply = await rawReply(probe, 'complete');
        const coreItem = reply.items.find(i => i.name === vector.item && i.kind === vector.kind);
        assert.ok(coreItem, vector.name + ': core item');
        const nativeItems = yawrItems(await completion(probe)), item = nativeItems.find(i => label(i) === vector.item);
        assert.ok(item, vector.name + ': native item');
        const start = probe.doc.offsetAt(item.range.start), end = probe.doc.offsetAt(item.range.end);
        assert.deepEqual({ start, end }, coreItem.edit.range);
        assert.equal(item.insertText, coreItem.edit.new_text);
        const edited = probe.source.slice(0, start) + item.insertText + probe.source.slice(end);
        assert.equal(edited, nl(includeFixture.prefix + vector.expected));
        const doc = YAML.parseDocument(edited);
        assert.deepEqual(doc.errors, []);
        const include = doc.getIn(['flow', 0, 'step', 'include'], true);
        assert.ok(YAML.isMap(include)); assert.ok(include.items.every(pair => YAML.isScalar(pair.key)));
        await accept(probe, item);
        evidence.cases.push({ name: vector.name + (crlf ? '-CRLF' : '-LF'), nativeCompletion: true,
          nativeAcceptance: true, exactGoEdit: true, scalarKeyIdentity: true, oneUndo: true });
      }
    }
    for (const source of ['data:\n  include: |CURSOR|\n',
      includeFixture.prefix + '\n        runbook: child.yaml\n        with:\n          include: |CURSOR|\n']) {
      const probe = await unsaved(source);
      assert.deepEqual(yawrItems(await completion(probe)), []);
      assert.equal(probe.doc.getText(), probe.source);
    }
    await vscode.window.showTextDocument(yamlProbe.doc);
    assert.ok((await completion(yamlProbe))?.items.some(i => label(i).includes('offline-schema-only-value')));
    evidence.cases.push({ name: 'include-arbitrary-data-excluded', nativeItems: 0, unchanged: true, yamlProviderContinues: true });
    for (const name of ['tool', 'action', 'argument']) {
      const vector = fixture.cases.find(c => c.name === name);
      const probe = await unsaved(fixture.prefix + vector.source_template);
      assert.equal(vscode.workspace.getConfiguration('yawr', probe.doc.uri).get('highlighting.developmentHelperPath'), config.helper,
        'Native machine-scoped helper uses the explicit candidate');
      const items = yawrItems(await completion(probe)), item = items.find(i => label(i) === vector.expect.item.name);
      if (!item) {
        const raw = await rawReply(probe, 'complete');
        assert.fail(name + ': real native completion missing; core=' + JSON.stringify({ status: raw.status, reason: raw.reason,
          site: raw.site?.kind, itemCount: raw.items.length, context: captureAuthoringContext(probe.doc, extension.extensionPath, 700).context }));
      }
      assert.equal(probe.doc.getText(item.range), vector.edit_target);
      assert.equal(item.insertText, vector.expect.item.edit.new_text);
      assert.equal(item.command, undefined); assert.equal(item.additionalTextEdits, undefined);
      await accept(probe, item);
      evidence.cases.push({ name, nativeCompletion: true, exactRange: true, oneUndo: true });
    }
    for (const vector of editRegressions.filter(v => v.native)) {
      const probe = await unsaved(fixture.prefix + vector.source_template);
      const reply = await rawReply(probe, 'complete');
      const coreItem = reply.items.find(i => i.name === vector.item && i.kind === vector.kind);
      assert.ok(coreItem, vector.name + ': Go item');
      const nativeItems = yawrItems(await completion(probe));
      const item = nativeItems.find(i => label(i) === vector.item);
      assert.ok(item, vector.name + ': native item');
      const start = probe.doc.offsetAt(item.range.start), end = probe.doc.offsetAt(item.range.end);
      assert.deepEqual({ start, end }, coreItem.edit.range, 'Native range is exactly the Go range');
      assert.equal(item.insertText, coreItem.edit.new_text, 'No frontend reconstruction');
      const expected = fixture.prefix + vector.expected_source_template;
      const edited = probe.source.slice(0, start) + item.insertText + probe.source.slice(end);
      assert.equal(edited, expected, vector.name + ': intended identifier and unchanged surroundings');
      if (vector.kind === 'namespace') assert.ok(reply.items.every(i => i.kind === 'namespace'));
      if (vector.kind === 'argument') assert.deepEqual(YAML.parse(edited).flow[0].step.tool.args, { text: 5, limit: 9 });
      if (edited === probe.source) {
        await vscode.commands.executeCommand('editor.action.triggerSuggest');
        await sleep(700);
        await vscode.commands.executeCommand('acceptSelectedSuggestion');
        await sleep(150);
        assert.equal(probe.doc.getText(), probe.source, 'Accepting an already-correct namespace is a no-op');
      } else {
        await accept(probe, item);
      }
      evidence.cases.push({ name: 'edit-regression-' + vector.name, nativeCompletion: true,
        nativeAcceptance: true, exactGoEdit: true, semanticIdentifier: true,
        noOp: edited === probe.source, oneUndo: edited !== probe.source });
    }
    for (const crlf of [false, true]) {
      for (const separator of [false, true]) {
        let template = fixture.prefix + '        name: db\n        action: inspect\n        args:\n' +
          '          ? te|CURSOR| # keep\n' + (separator ? '          : 5\n' : '') +
          '          limit: 9 # sibling\n';
        if (crlf) template = template.replace(/\n/g, '\r\n');
        const probe = await unsaved(template);
        const reply = await rawReply(probe, 'complete');
        const nativeItems = yawrItems(await completion(probe));
        if (!separator) {
          assert.equal(reply.status, 'unavailable');
          assert.equal(reply.reason, 'invalid-source-range');
          assert.deepEqual(reply.items, []);
          assert.deepEqual(nativeItems, []);
          assert.equal(probe.doc.getText(), probe.source);
          await vscode.window.showTextDocument(yamlProbe.doc);
          assert.ok((await completion(yamlProbe))?.items.some(i => label(i).includes('offline-schema-only-value')));
          evidence.cases.push({ name: 'explicit-no-separator-' + (crlf ? 'CRLF' : 'LF'),
            status: reply.status, reason: reply.reason, nativeItems: 0, unchanged: true,
            yamlProviderContinues: true, nativeAcceptance: false, oneUndo: false });
        } else {
          const coreItem = reply.items.find(i => i.name === 'text');
          const item = nativeItems.find(i => label(i) === 'text');
          assert.ok(coreItem); assert.ok(item);
          const start = probe.doc.offsetAt(item.range.start), end = probe.doc.offsetAt(item.range.end);
          assert.deepEqual({ start, end }, coreItem.edit.range);
          assert.equal(item.insertText, coreItem.edit.new_text);
          const edited = probe.source.slice(0, start) + item.insertText + probe.source.slice(end);
          assert.equal(edited, probe.source.replace('? te # keep', '? text # keep'));
          const doc = YAML.parseDocument(edited);
          assert.deepEqual(doc.errors, []);
          const args = doc.getIn(['flow', 0, 'step', 'tool', 'args'], true);
          assert.equal(args.items.length, 2);
          assert.ok(args.items.every(pair => YAML.isScalar(pair.key)));
          assert.equal(args.items[0].key.value, 'text');
          assert.equal(args.items[0].value.value, 5);
          assert.equal(args.items[1].key.value, 'limit');
          assert.equal(args.items[1].value.value, 9);
          await accept(probe, item);
          evidence.cases.push({ name: 'explicit-separated-' + (crlf ? 'CRLF' : 'LF'),
            nativeAcceptance: true, exactGoEdit: true, scalarKeyIdentity: true, oneUndo: true });
        }
      }
    }
    const reference = await unsaved(fixture.prefix.replace('  - name: db\n', '  - name: d|CURSOR|\n') +
      '        name: db\n        action: inspect\n');
    const referenceItem = yawrItems(await completion(reference)).find(i => label(i) === 'db');
    assert.ok(referenceItem, 'Path-pinned reference declaration completion');
    await accept(reference, referenceItem);
    evidence.cases.push({ name: 'tool-reference', nativeCompletion: true, exactRange: true });
    const mid = await unsaved(fixture.prefix + '        name: d|CURSOR|zz\n        action: inspect\n');
    const midItem = yawrItems(await completion(mid)).find(i => label(i) === 'db');
    assert.ok(midItem); assert.equal(mid.doc.getText(midItem.range), 'dzz');
    await accept(mid, midItem);
    evidence.cases.push({ name: 'mid-token-suffix', nativeCompletion: true });
    const crlf = await unsaved((fixture.prefix.replace('name: Example', 'name: Example 😀') +
      '        name: d|CURSOR|zz\n        action: inspect\n').replace(/\n/g, '\r\n'));
    const crlfItem = yawrItems(await completion(crlf)).find(i => label(i) === 'db');
    assert.ok(crlfItem); assert.equal(crlf.doc.getText(crlfItem.range), 'dzz');
    await accept(crlf, crlfItem);
    evidence.cases.push({ name: 'CRLF-astral-prefix', nativeCompletion: true, exactRange: true });
    const expressionPrefix = 'apiVersion: yawr.runbook/v1\nid: expressions\nname: Expressions\nflow:\n  - step:\n      id: condition\n      type: noop\n      when: ';
    const date = await unsaved(expressionPrefix + 'date.dif|CURSOR|Wrong\n');
    const dateItem = yawrItems(await completion(date)).find(i => label(i) === 'date.diffSeconds' || label(i) === 'diffSeconds');
    assert.ok(dateItem, 'Go date function inventory reaches native completion');
    assert.ok(date.doc.getText(dateItem.range).endsWith('difWrong'), 'Exact core range includes the suffix after the caret');
    await accept(date, dateItem);
    const dateSignature = await unsaved(expressionPrefix + "date.compare('2026-09-06', |CURSOR|)\n");
    const dateHelp = await signature(dateSignature);
    assert.equal(dateHelp?.signatures[0].label, 'date.compare(left: string, right: string) -> number');
    assert.equal(dateHelp.activeParameter, 1);
    assert.deepEqual(dateHelp.signatures[0].parameters.map(p => p.label), [[13, 25], [27, 40]]);
    const nested = await unsaved(expressionPrefix + "str.contains(str.trim('x,y'), |CURSOR|)\n");
    const nestedHelp = await signature(nested);
    assert.equal(nestedHelp?.signatures[0].label, 'str.contains(text: string, substring: string) -> boolean');
    assert.equal(nestedHelp.activeParameter, 1);
    const comparator = await unsaved(expressionPrefix + "list.order(rows, 'date.com|CURSOR|')\n");
    assert.ok(yawrItems(await completion(comparator)).some(i => /compare/.test(label(i))), 'Nested comparator completion');
    const comparatorSignature = await unsaved(expressionPrefix + 'list.order(rows, "date.compare(left, |CURSOR|)")\n');
    assert.equal((await signature(comparatorSignature))?.signatures[0].label, 'date.compare(left: string, right: string) -> number');
    evidence.cases.push({ name: 'date-and-nested-signatures', nativeCompletion: true, nativeSignatures: true,
      quotedCommaDepth: true, comparator: true, exactParameterSpans: true });
    for (const [name, template] of [
      ['regex-pattern', expressionPrefix + "regex.match(text, 'date.|CURSOR|')\n"],
      ['ordinary-kql', fixture.prefix + "        name: db\n        action: inspect\n        args:\n          text: 'T | where date.|CURSOR|'\n"],
    ]) {
      const probe = await unsaved(template);
      assert.deepEqual(yawrItems(await completion(probe)), []);
      assert.equal((await signature(probe))?.signatures?.length ?? 0, 0);
      evidence.cases.push({ name, noYawrFunctions: true });
    }
    const explicit = fixture.cases.find(c => c.name === 'explicit-required');
    const required = await unsaved(fixture.prefix + explicit.source_template);
    const expected = (await rawReply(required, 'required-arguments')).required_edit;
    assert.ok(expected);
    assert.equal(await vscode.commands.executeCommand('yawr.insertRequiredArguments'), true);
    const range = expected.edit.range;
    assert.equal(required.doc.getText(), required.source.slice(0, range.start) + expected.edit.new_text + required.source.slice(range.end));
    assert.ok(required.doc.getText().includes('          limit: 5\n'));
    await vscode.commands.executeCommand('leaveSnippet');
    await vscode.commands.executeCommand('undo');
    assert.equal(required.doc.getText(), required.source, 'One native required snippet undo');
    evidence.cases.push({ name: 'required-command', oneUndo: true, explicitOnly: true });
    const explicitNull = await unsaved(fixture.prefix +
      '        name: db\n        action: inspect\n        args:|CURSOR|\n          ? limit # keep explicit null\n');
    const explicitNullEdit = (await rawReply(explicitNull, 'required-arguments')).required_edit;
    assert.ok(explicitNullEdit);
    assert.equal(await vscode.commands.executeCommand('yawr.insertRequiredArguments'), true);
    const explicitRange = explicitNullEdit.edit.range;
    assert.equal(explicitNull.doc.getText(), explicitNull.source.slice(0, explicitRange.start) +
      explicitNullEdit.edit.new_text + explicitNull.source.slice(explicitRange.end));
    const explicitDoc = YAML.parseDocument(explicitNull.doc.getText());
    assert.deepEqual(explicitDoc.errors, []);
    const explicitArgs = explicitDoc.getIn(['flow', 0, 'step', 'tool', 'args'], true);
    assert.ok(explicitArgs.items.every(pair => YAML.isScalar(pair.key)));
    assert.deepEqual(explicitArgs.items.map(pair => [pair.key.value, pair.value?.value ?? null]),
      [['limit', null], ['text', null]]);
    await vscode.commands.executeCommand('leaveSnippet');
    await vscode.commands.executeCommand('undo');
    assert.equal(explicitNull.doc.getText(), explicitNull.source);
    evidence.cases.push({ name: 'required-explicit-null-preserved', exactGoEdit: true,
      scalarKeyIdentity: true, oneUndo: true });
    fs.writeFileSync(path.join(workspace, 'db.tool.yaml'), fixture.base_request.overlays[0].text +
      "      'price${x}\\\\': {type: string, required: true}\n");
    const adversarial = await unsaved(fixture.prefix + explicit.source_template);
    const adversarialEdit = (await rawReply(adversarial, 'required-arguments')).required_edit;
    assert.ok(adversarialEdit);
    assert.equal(adversarialEdit.placeholders.length, 2);
    assert.equal(await vscode.commands.executeCommand('yawr.insertRequiredArguments'), true);
    const adverseRange = adversarialEdit.edit.range;
    assert.ok(adversarial.doc.getText() === adversarial.source.slice(0, adverseRange.start) +
      adversarialEdit.edit.new_text + adversarial.source.slice(adverseRange.end), 'Dollar/brace/backslash keys remain literal native snippet text');
    await vscode.commands.executeCommand('leaveSnippet'); await vscode.commands.executeCommand('undo');
    assert.ok(adversarial.doc.getText() === adversarial.source);
    evidence.cases.push({ name: 'adversarial-native-snippet', escapedLiteralKeys: true, oneUndo: true });
    await setting('autocomplete.enabled', false); await setting('highlighting.enabled', true);
    const toggled = await unsaved(fixture.prefix + fixture.cases[0].source_template);
    assert.deepEqual(yawrItems(await completion(toggled)), []);
    assert.equal(toggled.doc.languageId, 'yaml');
    await setting('highlighting.enabled', false); await setting('autocomplete.enabled', true);
    assert.ok(yawrItems(await completion(toggled)).length, 'Autocomplete remains available with highlighting off');
    evidence.cases.push({ name: 'independent-toggles', passed: true });
    await vscode.window.showTextDocument(yamlProbe.doc);
    assert.ok((await completion(yamlProbe))?.items.some(i => label(i).includes('offline-schema-only-value')), 'YAML provider remains registered');

    const before = fs.readFileSync(config.callerRunbook), originalDigest = digest(before);
    setPresentationEntrypoint(config.callerProjectRoot, config.callerRunbook, []);
    const caller = await vscode.workspace.openTextDocument(vscode.Uri.file(config.callerRunbook));
    await vscode.window.showTextDocument(caller);
    await setting('packageMap', config.callerPackageMap);
    const original = caller.getText();
    const action = /^\s*action:\s*(\S*)/m.exec(original);
    assert.ok(action, 'Actual caller must have an authored action probe');
    const actionEnd = action.index + action[0].length;
    const existingProbe = { doc: caller, position: caller.positionAt(actionEnd) };
    const existingReply = await rawReply(existingProbe, 'complete');
    const existingNative = yawrItems(await completion(existingProbe));
    const callerContext = captureAuthoringContext(caller, extension.extensionPath, 700).context;
    assert.equal(callerContext.project_root, config.callerProjectRoot);
    assert.equal(callerContext.package_map_path, config.callerPackageMap);
    assert.equal(callerContext.entrypoint_path, config.callerRunbook);
    assert.equal(existingReply.status, 'resolved', 'Complete real caller source admits its bound query action');
    assert.equal(existingReply.site?.kind, 'action');
    assert.equal(existingReply.site?.tool_id, config.callerToolID ?? 'query-sterling-kusto');
    assert.ok(existingReply.items.some(i => i.name === 'query'));
    assert.ok(existingNative.some(i => label(i) === 'query'), 'Actual caller action reaches native completion');
    evidence.cases.push({ name: 'caller-existing', status: existingReply.status, reason: existingReply.reason,
      discovery: existingReply.discovery, nativeItems: existingNative.length, context: callerContext,
      position: actionEnd, sourceSHA256: originalDigest, itemNames: existingNative.map(label) });
    const copyPath = path.join(path.dirname(config.callerRunbook), 'autocomplete-unsaved-probe.runbook.yaml');
    setPresentationEntrypoint(config.callerProjectRoot, config.callerRunbook, [copyPath]);
    const partialStart = actionEnd - action[1].length;
    const partial = await unsaved(original.slice(0, partialStart) + action[1].slice(0, 1) + '|CURSOR|' + original.slice(actionEnd), copyPath);
    const partialReply = await rawReply(partial, 'complete');
    const partialItems = yawrItems(await completion(partial));
    assert.equal(partialReply.status, 'resolved', 'Unsaved real caller partial action must resolve');
    const queryItem = partialItems.find(i => label(i) === 'query');
    assert.ok(queryItem, 'Unsaved real caller has a native query completion, not merely a resolved status');
    assert.equal(partial.doc.getText(queryItem.range), action[1].slice(0, 1));
    evidence.cases.push({ name: 'caller-unsaved-partial', status: partialReply.status, reason: partialReply.reason,
      discovery: partialReply.discovery, nativeItems: partialItems.length, itemNames: partialItems.map(label) });
    await accept(partial, queryItem);
    const argsPath = path.join(path.dirname(config.callerRunbook), 'autocomplete-unsaved-args-probe.runbook.yaml');
    setPresentationEntrypoint(config.callerProjectRoot, config.callerRunbook, [copyPath, argsPath]);
    assert.ok(original.includes('timeout: "120"'));
    const args = await unsaved(original.replace('timeout: "120"', 'ti|CURSOR|: "120"'), argsPath);
    const argsReply = await rawReply(args, 'complete');
    assert.equal(argsReply.status, 'resolved');
    assert.equal(argsReply.site?.kind, 'argument');
    assert.equal(argsReply.site?.tool_id, config.callerToolID ?? 'query-sterling-kusto');
    const timeoutItem = yawrItems(await completion(args)).find(i => label(i) === 'timeout');
    assert.ok(timeoutItem, 'Real bound query argument metadata reaches native completion');
    assert.equal(args.doc.getText(timeoutItem.range), 'ti');
    await accept(args, timeoutItem);
    evidence.cases.push({ name: 'caller-unsaved-argument', nativeCompletion: true, item: label(timeoutItem),
      exactRange: true, oneUndo: true, discovery: argsReply.discovery });
    for (const [name, token] of [['date-subject', 'date.compare(start_time'], ['date-comparator', 'date.diffSeconds(left']]) {
      const start = original.indexOf(token);
      assert.ok(start >= 0);
      const functionName = token.slice(0, token.indexOf('('));
      const probe = { doc: caller, position: caller.positionAt(start + 'date.'.length + 3) };
      const reply = await rawReply(probe, 'complete');
      assert.equal(reply.status, 'resolved');
      const item = yawrItems(await completion(probe)).find(i => label(i) === functionName || label(i) === functionName.slice(5));
      assert.ok(item, 'Real caller ' + name + ' function reaches native completion');
      const comma = original.indexOf(',', start);
      const help = await signature({ doc: caller, position: caller.positionAt(comma + 1) });
      assert.ok(help?.signatures[0].label.startsWith(functionName + '('), 'Real caller date signature');
      assert.equal(help.activeParameter, 1);
      evidence.cases.push({ name: 'caller-' + name, nativeCompletion: true, nativeSignature: true,
        item: label(item), position: probe.doc.offsetAt(probe.position), yamlPath: reply.site?.yaml_path });
    }
    const unboundToken = 'action: ' + (config.callerUnboundAction ?? 'get-pool-info');
    const unboundAction = original.indexOf(unboundToken);
    assert.ok(unboundAction >= 0);
    const unboundProbe = { doc: caller, position: caller.positionAt(unboundAction + unboundToken.length) };
    const unboundReply = await rawReply(unboundProbe, 'complete');
    assert.equal(unboundReply.status, 'unavailable', 'No implicit reference synthesis for unrelated unbound tool');
    assert.equal(yawrItems(await completion(unboundProbe)).length, 0);
    evidence.cases.push({ name: 'caller-unbound-tool', status: unboundReply.status, reason: unboundReply.reason,
      discovery: unboundReply.discovery, nativeItems: 0 });
    assert.equal(digest(fs.readFileSync(config.callerRunbook)), originalDigest, 'Actual caller remains byte-for-byte read-only');
    assert.equal(fs.existsSync(copyPath), false, 'Unsaved probe is never written to caller');
    assert.equal(fs.existsSync(argsPath), false, 'Unsaved argument probe is never written to caller');
    evidence.cases.push({ name: 'caller-read-only', passed: true, sourceSHA256: originalDigest });
  } finally {
    client.dispose();
    for (const doc of buffers) if (!doc.isClosed) {
      await vscode.window.showTextDocument(doc);
      await vscode.commands.executeCommand('workbench.action.revertAndCloseActiveEditor');
    }
    fs.writeFileSync(path.join(root, 'results.json'), JSON.stringify(evidence, null, 2));
  }
};
