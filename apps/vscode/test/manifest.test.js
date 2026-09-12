const assert = require('node:assert/strict');
const test = require('node:test');

const manifest = require('../package.json');

test('autocomplete has an independent toggle and only explicit required argument scaffolding', () => {
  const properties = manifest.contributes.configuration.properties;
  assert.equal(properties['yawr.autocomplete.enabled'].type, 'boolean');
  assert.equal(properties['yawr.autocomplete.enabled'].default, true);
  assert.equal(properties['yawr.highlighting.enabled'].default, true);
  assert.equal(properties['yawr.highlighting.developmentHelperPath'].scope, 'machine');
  assert.equal(properties['yawr.autocomplete.developmentHelperPath'], undefined, 'Shared helper override, not competing binaries');
  assert.ok(manifest.contributes.commands.some(command => command.command === 'yawr.insertRequiredArguments' &&
    command.title === 'Yawr: Insert Missing Required Arguments'));
  assert.ok(manifest.scripts['test:authoring:core'] && manifest.scripts['test:authoring:native']);
});
test('safe highlighting diagnostics are available as one Command Palette action', () => {
  assert.ok(manifest.contributes.commands.some(command => command.command === 'yawr.showHighlightingDiagnostics'));
});

test('React Flow preview is a visible editor title action for runbooks', () => {
  const command = manifest.contributes.commands.find(
    (candidate) => candidate.command === 'yawr.previewGraph',
  );
  assert.ok(command, 'yawr.previewGraph must be contributed');
  assert.ok(command.icon, 'yawr.previewGraph needs an icon to appear in the editor title toolbar');

  const menuItem = manifest.contributes.menus['editor/title'].find(
    (candidate) => candidate.command === 'yawr.previewGraph',
  );
  assert.ok(menuItem, 'yawr.previewGraph must be contributed to editor/title');
  assert.match(menuItem.group, /^navigation(?:@|$)/);
  assert.match(menuItem.when, /resourceFilename/);
  assert.match(menuItem.when, /runbook/);
});

test('React Flow preview placement is configurable and defaults to the runbook group', () => {
  const setting = manifest.contributes.configuration.properties['yawr.preview.openLocation'];

  assert.ok(setting, 'yawr.preview.openLocation must be contributed');
  assert.equal(setting.type, 'string');
  assert.deepEqual(setting.enum, ['beside', 'sameGroup']);
  assert.equal(setting.default, 'sameGroup');
});

test('removed SSE server commands and settings are not contributed', () => {
  const commandIDs = manifest.contributes.commands.map((candidate) => candidate.command);
  const properties = manifest.contributes.configuration.properties;

  assert.ok(!commandIDs.includes('yawr.previewLive'));
  assert.ok(!commandIDs.includes('yawr.restartServer'));
  assert.equal(properties['yawr.serverUrl'], undefined);
  assert.equal(properties['yawr.autoStartServer'], undefined);
});

test('XTS confirmation has no extension-modal preference', () => {
  const properties = manifest.contributes.configuration.properties;

  assert.equal(properties['yawr.xts.showHandoffConfirmation'], undefined);
});

test('runbook server settings are resource-scoped for multi-root workspaces', () => {
  const properties = manifest.contributes.configuration.properties;
  const resourceSettings = [
    'yawr.packageMap',
    'yawr.binaryPath',
    'yawr.preview.nodeStyle',
    'yawr.preview.openLocation',
  ];

  for (const key of resourceSettings) {
    assert.equal(
      properties[key]?.scope,
      'resource',
      `${key} must resolve from the workspace folder containing the active runbook`,
    );
  }
});

test('yawr.validateInputs is contributed as a Command Palette entry', () => {
  const command = manifest.contributes.commands.find(
    (candidate) => candidate.command === 'yawr.validateInputs',
  );
  assert.ok(command, 'yawr.validateInputs must be contributed');
  assert.match(command.title, /yawr/i);
});

// The extension has no `yawr.runbook/v1` JSON Schema, no `jsonValidation`/
// `yamlValidation` contribution. It forwards files to the real `yawr` CLI/server and never
// re-implements structural or enum-membership validation of its own.
// Exception: js-yaml is allowed because it is used to parse *.tool.yaml
// *tool definition* files for the MCP bridge registry — never to validate
// runbook YAML or to substitute for the yawr engine. Approved code presentation
// additionally uses yaml CST only to map exact core-selected scalar ranges.
test('CE-C-02: no bundled schema or editor schema-validation; CST mapping is not a binding resolver', () => {
  assert.equal(manifest.contributes.jsonValidation, undefined, 'no jsonValidation contribution point expected');
  assert.equal(manifest.contributes.yamlValidation, undefined, 'no yamlValidation contribution point expected');

  const deps = { ...(manifest.dependencies ?? {}), ...(manifest.devDependencies ?? {}) };
  // js-yaml is intentionally allowed: used for parsing .tool.yaml definitions
  // in the MCP bridge registry, not for runbook validation.
  const yamlParserAllowlist = new Set(['js-yaml', '@types/js-yaml', 'yaml']);
  assert.equal(deps.yaml, '2.8.1', 'CST mapper uses the vetted pinned YAML package');
  for (const name of Object.keys(deps)) {
    if (yamlParserAllowlist.has(name)) continue;
    assert.doesNotMatch(
      name.toLowerCase(),
      /(^|[^a-z])yaml([^a-z]|$)/,
      `unexpected YAML-parsing dependency "${name}" — this client must not decode runbook YAML itself`,
    );
  }
});

// INVTOKEN-M-01: The manifest must declare the yawr.chat participant so that
// vscode.chat.createChatParticipant('yawr.chat', ...) is reachable. A
// participant registered in code but absent from the manifest is silently
// inert — the exact bug class that has bitten this engagement twelve times.
test('the canonical chat participant is declared exactly once', () => {
  const participants = manifest.contributes.chatParticipants;
  assert.ok(Array.isArray(participants) && participants.length > 0,
    'contributes.chatParticipants must be a non-empty array');

  assert.equal(participants.filter((p) => p.id === 'yawr.chat').length, 1);
});

// INVTOKEN-M-02: The arm-mcp slash command must be declared so VS Code
// surfaces it in the command picker and routes /arm-mcp to the participant.
test('yawr.chat participant declares the arm-mcp command', () => {
  const participants = manifest.contributes.chatParticipants ?? [];
  const yawr = participants.find((p) => p.id === 'yawr.chat');
  assert.ok(yawr);
  assert.ok(yawr.commands.find((c) => c.name === 'arm-mcp'));
});

// INVTOKEN-M-03: The /run slash command must be declared in the manifest so
// VS Code surfaces it in the autocomplete picker. A command handled in code
// but absent from the manifest is silently missing from the UI — the same
// class of defect that has bitten this engagement repeatedly.
test('yawr.chat participant declares the run command', () => {
  const participants = manifest.contributes.chatParticipants ?? [];
  const yawr = participants.find((p) => p.id === 'yawr.chat');
  assert.ok((yawr.commands ?? []).find((c) => c.name === 'run'));
});

test('canonical commands and settings are unique', () => {
  const commands = manifest.contributes.commands.map(command => command.command);
  assert.equal(new Set(commands).size, commands.length);
  const properties = manifest.contributes.configuration.properties;
  for (const suffix of ['autocomplete.enabled', 'highlighting.enabled', 'highlighting.developmentHelperPath',
    'packageMap', 'mcpBridge.toolNameOverrides', 'binaryPath', 'preview.nodeStyle', 'preview.openLocation']) {
    assert.ok(properties[`yawr.${suffix}`]);
    assert.equal(properties[`yawr.${suffix}`]?.deprecationMessage, undefined);
  }
});

test('CE-C-02: no vendored *.schema.json in the extension source tree', () => {
  const fs = require('node:fs');
  const path = require('node:path');
  const root = path.join(__dirname, '..');
  const skipDirs = new Set(['node_modules', 'out', '.git', '.vscode', '.vscode-test']);

  const offenders = [];
  (function walk(dir) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      if (entry.isDirectory()) {
        if (skipDirs.has(entry.name)) continue;
        walk(path.join(dir, entry.name));
      } else if (entry.name.endsWith('.schema.json')) {
        offenders.push(path.join(dir, entry.name));
      }
    }
  })(root);

  assert.deepEqual(offenders, [], `unexpected vendored schema file(s): ${offenders.join(', ')}`);
});
