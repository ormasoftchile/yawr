const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { pathToFileURL } = require('node:url');
const Module = require('node:module');
class Emitter {
  listeners = new Set();
  event = callback => { this.listeners.add(callback); return { dispose: () => this.listeners.delete(callback) }; };
  fire = value => { for (const callback of [...this.listeners]) callback(value); };
}
class Position { constructor(line, character) { Object.assign(this, { line, character }); } }
class Range {
  constructor(start, end) { Object.assign(this, { start, end }); }
  get isSingleLine() { return this.start.line === this.end.line; }
  contains(p) { return this.isSingleLine && p.line === this.start.line && p.character >= this.start.character && p.character <= this.end.character; }
}
class SnippetString {
  segments = [];
  appendText(text) { this.segments.push(['text', text]); return this; }
  appendPlaceholder(text) { this.segments.push(['placeholder', text]); return this; }
  appendTabstop(n) { this.segments.push(['tabstop', n]); return this; }
}
function harness(t) {
  const events = Object.fromEntries(['text', 'open', 'close', 'config', 'active'].map(key => [key, new Emitter()]));
  const root = path.join(__dirname, '..', '.vscode-test'), file = path.join(root, 'authoring.runbook.yaml');
  const uri = { fsPath: file, scheme: 'file', toString: () => pathToFileURL(file).href };
  let source = 'name: d', handler, inserted;
  const doc = { uri, fileName: file, version: 1, languageId: 'yaml', isClosed: false, isUntitled: false,
    getText: () => source, positionAt: offset => new Position(0, offset), offsetAt: p => p.character };
  const selection = { active: new Position(0, 7), isEqual: other => other === selection };
  const editor = { document: doc, selection, insertSnippet: async (snippet, range, options) => { inserted = { snippet, range, options }; return true; } };
  const registrations = [], commands = {}, watchers = [], statuses = [], messages = [];
  const settings = { 'highlighting.developmentHelperPath': process.execPath, 'highlighting.enabled': false, 'autocomplete.enabled': true };
  const disposable = () => ({ disposed: false, dispose() { this.disposed = true; } });
  const vscode = {
    EventEmitter: Emitter, Range, SnippetString, StatusBarAlignment: { Right: 1 },
    CompletionItemKind: { Module: 1, Method: 2, Property: 3, Function: 4 },
    CompletionItem: class { constructor(label, kind) { Object.assign(this, { label, kind }); } },
    CompletionList: class { constructor(items, isIncomplete) { Object.assign(this, { items, isIncomplete }); } },
    SignatureInformation: class { constructor(label, documentation) { Object.assign(this, { label, documentation }); } },
    ParameterInformation: class { constructor(label, documentation) { Object.assign(this, { label, documentation }); } },
    SignatureHelp: class {},
    Uri: { file: filename => ({ toString: () => pathToFileURL(filename).href }), parse: text => ({ scheme: 'file', fsPath: require('node:url').fileURLToPath(text) }) },
    RelativePattern: class { constructor(root, pattern) { Object.assign(this, { root, pattern }); } },
    workspace: {
      textDocuments: [doc], workspaceFolders: [{ uri: { fsPath: root } }],
      getConfiguration: () => ({ get: (key, fallback) => settings[key] ?? fallback }),
      onDidChangeTextDocument: events.text.event, onDidOpenTextDocument: events.open.event,
      onDidCloseTextDocument: events.close.event, onDidChangeConfiguration: events.config.event,
      createFileSystemWatcher: pattern => {
        const changes = new Emitter(), watcher = { ...disposable(), pattern, changes,
          onDidChange: changes.event, onDidCreate: changes.event, onDidDelete: changes.event };
        watchers.push(watcher); return watcher;
      },
    },
    window: { activeTextEditor: editor, onDidChangeActiveTextEditor: events.active.event,
      showInformationMessage: text => { messages.push(text); },
      createStatusBarItem: () => { const status = { ...disposable(), show() {}, hide() {} }; statuses.push(status); return status; } },
    languages: {
      registerCompletionItemProvider: (selector, provider, ...triggers) => { registrations.push({ type: 'complete', selector, provider, triggers }); return disposable(); },
      registerSignatureHelpProvider: (selector, provider, ...triggers) => { registrations.push({ type: 'signature', selector, provider, triggers }); return disposable(); },
    },
    commands: { registerCommand: (id, callback) => { commands[id] = callback; return disposable(); } },
  };
  const original = Module._load;
  t.mock.method(Module, '_load', function (id, ...rest) { return id === 'vscode' ? vscode : original.call(this, id, ...rest); });
  for (const name of ['authoringEditor', 'authoringContext']) delete require.cache[require.resolve('../out/' + name)];
  const { AuthoringClient } = require('../out/authoringClient');
  t.mock.method(AuthoringClient.prototype, 'resolve', async (binary, request, signal) => handler(request, signal));
  const module = require('../out/authoringEditor'), registration = module.registerAuthoringEditor({ extensionPath: path.resolve(__dirname, '..') });
  t.after(() => registration.dispose());
  const token = { isCancellationRequested: false, onCancellationRequested: () => disposable() };
  return { doc, editor, events, registrations, settings, commands, watchers, statuses, messages, token,
    setHandler: fn => { handler = fn; }, inserted: () => inserted,
    edit: text => { source = text; doc.version++; events.text.fire({ document: doc }); },
    complete: () => registrations[0].provider.provideCompletionItems(doc, selection.active, token),
    signature: () => registrations[1].provider.provideSignatureHelp(doc, selection.active, token),
    snippet: module.requiredArgumentsSnippet, registration,
  };
}
const result = extra => ({ status: 'resolved', discovery: { scope: 'explicit-local-catalog', status: 'complete' },
  dependencies: [], site: { kind: 'tool', range: { start: 6, end: 7 } }, items: [], signature: null, required_edit: null, ...extra });
test('native provider publishes only exact core plain text and no implicit command or args', async t => {
  const h = harness(t);
  h.setHandler(() => result({ items: [{ kind: 'tool', name: 'db', description: '<b>[command](command:evil)</b>',
    edit: { range: { start: 6, end: 7 }, new_text: 'db' } }] }));
  const list = await h.complete(), item = list.items[0];
  assert.equal(list.isIncomplete, false);
  assert.equal(item.documentation, '<b>[command](command:evil)</b>');
  assert.equal(typeof item.documentation, 'string'); assert.equal(item.insertText, 'db');
  assert.deepEqual(item.range, new Range(new Position(0, 6), new Position(0, 7)));
  assert.equal(item.command, undefined); assert.equal(item.additionalTextEdits, undefined);
  assert.deepEqual(h.registrations.map(r => r.triggers), [['.', ':'], ['(', ',']]);
  assert.ok(h.registrations.every(r => r.selector.every(s => s.language === 'yaml')));
  assert.equal(h.settings['highlighting.enabled'], false);
});
test('signature uses core labels and UTF16 parameter spans without TypeScript inventory', async t => {
  const h = harness(t);
  h.setHandler(() => result({ signature: { label: 'sample(value)', description: 'plain', active_parameter: null,
    parameters: [{ name: 'value', label_range: { start: 7, end: 12 }, description: 'plain parameter' }] } }));
  const help = await h.signature();
  assert.equal(help.signatures[0].label, 'sample(value)');
  assert.deepEqual(help.signatures[0].parameters[0].label, [7, 12]);
  assert.equal(help.activeParameter, 1, 'Null is not clamped to parameter zero');
});
test('explicit snippet uses literal segments and only core null placeholders with one undo pair', async t => {
  const h = harness(t);
  const text = '"a${x}\\\\": null\n', start = text.indexOf('null');
  const required_edit = { edit: { range: { start: 7, end: 7 }, new_text: text }, placeholders: [{ start, end: start + 4 }] };
  h.setHandler(() => result({ required_edit }));
  assert.equal(h.inserted(), undefined);
  assert.equal(await h.commands['yawr.insertRequiredArguments'](), true);
  assert.deepEqual(h.inserted().snippet.segments, [['text', text.slice(0, start)], ['placeholder', 'null'], ['text', '\n'], ['tabstop', 0]]);
  assert.deepEqual(h.inserted().options, { undoStopBefore: true, undoStopAfter: true });
});
test('document/config/dependency/entrypoint invalidation discards late metadata independently of painting', async t => {
  for (const change of ['text', 'config', 'dependency', 'entrypoint', 'close', 'active']) {
    const h = harness(t);
    let release, started;
    const ready = new Promise(resolve => { started = resolve; });
    h.setHandler(() => { started(); return new Promise(resolve => { release = resolve; }); });
    const pending = h.complete(); await ready;
    if (change === 'text') h.edit('name: e');
    if (change === 'config') h.events.config.fire();
    if (change === 'dependency') h.watchers[0].changes.fire();
    if (change === 'entrypoint') require('../out/authoringContext').setPresentationEntrypoint(path.dirname(h.doc.fileName), h.doc.fileName, []);
    if (change === 'close') h.events.close.fire(h.doc);
    if (change === 'active') h.events.active.fire();
    release(result({ items: [{ kind: 'tool', name: 'db', edit: { range: { start: 6, end: 7 }, new_text: 'db' } }] }));
    assert.equal(await pending, undefined, change);
    h.registration.dispose();
  }
});
test('dependency invalidation coverage is capped rather than silently dropped', async t => {
  const h = harness(t);
  h.setHandler(() => result({ dependencies: Array.from({ length: 130 }, (_, i) => ({
    uri: pathToFileURL(path.join(path.parse(__dirname).root, `authoring-unavailable-${i}`, 'missing.tool.yaml')).href,
    missing: true,
  })) }));
  assert.equal(await h.complete(), undefined);
  assert.ok(h.statuses[0].text.includes('limit-exceeded'));
  assert.equal(h.watchers.filter(w => !w.disposed).length, 128);
  assert.deepEqual(h.messages, []);
});
test('toggle disables only authoring, availability is bounded status not keystroke popup', async t => {
  const h = harness(t);
  h.setHandler(() => result({ status: 'unavailable', reason: 'missing-dependency' }));
  assert.equal(await h.complete(), undefined);
  assert.ok(h.statuses[0].text.includes('missing-dependency'));
  assert.deepEqual(h.messages, []);
  h.settings['autocomplete.enabled'] = false; h.events.config.fire();
  h.setHandler(() => assert.fail('Disabled provider must not request helper metadata'));
  assert.equal(await h.complete(), undefined);
  assert.equal(h.registrations.length, 2, 'Providers register once across toggles');
  assert.ok(h.watchers.every(w => w.disposed));
});
