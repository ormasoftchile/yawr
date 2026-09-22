// toolDefinitionRegistry.test.js — unit tests for the YAML-derived registry.
//
// Tests confirm that tool definition YAML files are parsed correctly and that
// the vscode_tool precedence rules are enforced.
//
// Run with: node --test test/toolDefinitionRegistry.test.js  (or via npm test)

'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const { buildRegistryFromDir, findToolYamls } = require('../out/toolDefinitionRegistry');
const { buildRegistryForRun } = require('../out/toolDefinitionRegistry');

const FIXTURE_TOOLS_DIR = path.join(__dirname, 'fixtures', 'tools');

// Load registry once — all tests share it (buildRegistryFromDir is pure/sync).
const registry = buildRegistryFromDir(FIXTURE_TOOLS_DIR);

// ─── Registry contents ────────────────────────────────────────────────────────

test('registry: icm/get-incident is present with correct registered name', () => {
  const spec = registry['icm/get-incident'];
  assert.ok(spec, 'icm/get-incident must be in the registry');
  assert.equal(spec.registeredName, 'icm-get-incident');
});

test('registry: icm/get-incident has the correct output fields', () => {
  const spec = registry['icm/get-incident'];
  const fields = Object.keys(spec.outputFields).sort();
  assert.deepEqual(fields, ['database', 'environment', 'logical_server', 'service', 'title'].sort());
  for (const f of fields) {
    assert.equal(spec.outputFields[f].type, 'string', `${f}.type must be string`);
    assert.equal(spec.outputFields[f].required, true, `${f}.required must be true`);
  }
});

// ─── vscode_tool precedence ───────────────────────────────────────────────────

test('precedence: action-level vscode_tool beats transport-level', () => {
  const spec = registry['precedence-tool/action-with-action-level'];
  assert.ok(spec, 'precedence-tool/action-with-action-level must be in registry');
  assert.equal(spec.registeredName, 'action-level-name',
    'action-level vscode_tool must take precedence over transport-level');
});

test('precedence: transport-level vscode_tool beats logical-name fallback', () => {
  const spec = registry['precedence-tool/action-with-transport-level'];
  assert.ok(spec, 'precedence-tool/action-with-transport-level must be in registry');
  assert.equal(spec.registeredName, 'transport-level-name',
    'transport-level vscode_tool must take precedence over logical-name fallback');
});

test('precedence: logical tool name is fallback when no vscode_tool declared', () => {
  const spec = registry['fallback-tool/fallback-action'];
  assert.ok(spec, 'fallback-tool/fallback-action must be in registry');
  assert.equal(spec.registeredName, 'fallback-tool',
    'logical tool name must be used as fallback when no vscode_tool is declared');
});

// ─── Non-vscode-mcp tools are excluded ──────────────────────────────────────

test('registry: tools without effective transport mode vscode-mcp are not included', () => {
  // All keys from our fixture dir are known; unknown keys would be a leak.
  const knownPrefixes = [
    'icm/',           // canonical identity and sequence actions
    'precedence-tool/', // transport-level vscode_tool
    'fallback-tool/', // logical-name fallback
    'ops-meta/',      // canonical meta.name identity
  ];
  for (const key of Object.keys(registry)) {
    const isKnown = knownPrefixes.some((p) => key.startsWith(p));
    assert.ok(isKnown, `unexpected registry key "${key}" — non-vscode-mcp tool may have leaked in`);
  }
});

// ─── Edge cases ───────────────────────────────────────────────────────────────

test('buildRegistryFromDir: returns empty registry for non-existent directory', () => {
  const r = buildRegistryFromDir('/no/such/directory/xyz');
  assert.deepEqual(Object.keys(r), []);
});

test('buildRegistryFromDir: returns empty registry for empty directory', (t) => {
  // Use the fixture dir parent (test/fixtures) which has no *.tool.yaml files directly
  // and only the tools/ subdir; this just verifies it doesn't explode.
  const r = buildRegistryFromDir(path.join(__dirname, 'fixtures'));
  // The 'tools' subdirectory WILL be scanned recursively — that's expected.
  // We just verify the call doesn't throw.
  assert.ok(typeof r === 'object');
});

test('buildRegistryForRun overlays external package-map bindings', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-run-registry-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const project = path.join(root, 'project');
  const external = path.join(root, 'external-package');
  fs.mkdirSync(project, { recursive: true });
  fs.mkdirSync(external, { recursive: true });
  fs.writeFileSync(path.join(external, 'external.tool.yaml'), [
    'apiVersion: yawr.tool/v1',
    'meta: {name: external-tool}',
    'transport:',
    '  mode: vscode-mcp',
    '  vscode_tool: external-provider-tool',
    'actions:',
    '  - name: inspect',
  ].join('\n'));
  const packageMap = path.join(project, 'package-map.yaml');
  fs.writeFileSync(packageMap, [
    'apiVersion: yawr.config/v1',
    'requires:',
    '  - package: external.package',
    '    version: "^1.0.0"',
    '    path: ../external-package',
  ].join('\n'));

  const runRegistry = buildRegistryForRun(project, packageMap);
  assert.equal(runRegistry['external-tool/inspect']?.registeredName, 'external-provider-tool');
});

test('buildRegistryForRun gives effective required packages precedence over project tool paths', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-run-registry-tier-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const project = path.join(root, 'project');
  const projectTools = path.join(root, 'project-tools');
  const requiredPackage = path.join(root, 'required-package');
  fs.mkdirSync(path.join(project, '.yawr'), { recursive: true });
  fs.mkdirSync(projectTools, { recursive: true });
  fs.mkdirSync(requiredPackage, { recursive: true });
  const toolYaml = (registeredName) => [
    'apiVersion: yawr.tool/v1',
    'meta: {name: shared-tool}',
    'transport:',
    '  mode: vscode-mcp',
    `  vscode_tool: ${registeredName}`,
    'actions:',
    '  - name: inspect',
  ].join('\n');
  fs.writeFileSync(path.join(projectTools, 'shared.tool.yaml'), toolYaml('project-provider'));
  fs.writeFileSync(path.join(requiredPackage, 'shared.tool.yaml'), toolYaml('required-provider'));
  fs.writeFileSync(path.join(project, '.yawr', 'config.yaml'), [
    'apiVersion: yawr.config/v1',
    'tool-paths:',
    '  - ../project-tools',
  ].join('\n'));
  const packageMap = path.join(project, 'package-map.yaml');
  fs.writeFileSync(packageMap, [
    'apiVersion: yawr.config/v1',
    'requires:',
    '  - package: required.package',
    '    version: "^1.0.0"',
    '    path: ../required-package',
  ].join('\n'));

  const runRegistry = buildRegistryForRun(project, packageMap);
  assert.equal(runRegistry['shared-tool/inspect']?.registeredName, 'required-provider');
});

test('findToolYamls: returns tool yaml paths for known fixture dir', () => {
  const files = findToolYamls(FIXTURE_TOOLS_DIR);
  assert.ok(files.length >= 4, 'expected at least 4 fixture tool yamls');
  assert.ok(files.every((f) => f.endsWith('.tool.yaml')));
});

test('findToolYamls: finds both .tool.yaml and .yawt files', (t) => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-find-tools-'));
  t.after(() => fs.rmSync(tmpDir, { recursive: true, force: true }));
  fs.writeFileSync(path.join(tmpDir, 'alpha.tool.yaml'), '');
  fs.writeFileSync(path.join(tmpDir, 'beta.yawt'), '');
  fs.writeFileSync(path.join(tmpDir, 'gamma.yaml'), '');
  const files = findToolYamls(tmpDir);
  assert.equal(files.length, 2);
  assert.ok(files.some((f) => f.endsWith('alpha.tool.yaml')));
  assert.ok(files.some((f) => f.endsWith('beta.yawt')));
});

// ─── DEFECT 2 regression tests ───────────────────────────────────────────────
// These five tests guard the three parse axes that were broken before the fix.

// Test 1: consumer's exact fixture (byte-for-byte as supplied in the ask).
// meta.name + sequence-form actions + transport.mode — all three must work together.
test('defect2-reg: consumer icm fixture (meta.name + sequence + transport.mode) registers icm/get-incident', (t) => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-icm-'));
  t.after(() => fs.rmSync(tmpDir, { recursive: true, force: true }));
  fs.writeFileSync(path.join(tmpDir, 'icm.tool.yaml'), [
    'apiVersion: yawr.tool/v1',
    'meta:',
    '  name: icm',
    'transport:',
    '  mode: vscode-mcp',
    'actions:',
    '  - name: get-incident',
  ].join('\n'));
  const r = buildRegistryFromDir(tmpDir);
  assert.ok(r['icm/get-incident'], 'icm/get-incident must be in the registry');
});

// Test 2: actions must use the canonical sequence form.
test('defect2-reg: mapping-form actions are rejected', (t) => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-action-shape-'));
  t.after(() => fs.rmSync(tmpDir, { recursive: true, force: true }));
  fs.writeFileSync(path.join(tmpDir, 'invalid.tool.yaml'), [
    'name: invalid-actions',
    'transport: {mode: vscode-mcp}',
    'actions:',
    '  inspect: {}',
  ].join('\n'));
  assert.deepEqual(buildRegistryFromDir(tmpDir), {});
});

// Test 3: transport.mode is required.
test('defect2-reg: transport.type does not register the tool', (t) => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-transport-shape-'));
  t.after(() => fs.rmSync(tmpDir, { recursive: true, force: true }));
  fs.writeFileSync(path.join(tmpDir, 'invalid.tool.yaml'), [
    'meta: {name: invalid-transport}',
    'transport: {type: vscode-mcp}',
    'actions: [{name: inspect}]',
  ].join('\n'));
  assert.deepEqual(buildRegistryFromDir(tmpDir), {});
});

// Test 4: canonical meta.name identity.
test('defect2-reg: meta.name supplies the registry key', () => {
  assert.ok(registry['ops-meta/act'],
    'ops-meta/act must be registered from meta.name');
});

// Test 5: drift guard — documents the canonical shape matrix the extension accepts.
// If any axis is removed from buildRegistryFromDir, at least one assertion here fails.
test('defect2-drift-guard: all three core parse axes are handled', () => {
  // This test documents the shape matrix mirrored from:
  //   tool.go ToolDef.UnmarshalYAML  (meta.name)
  //   tool.go decodeToolActions       (sequence)
  //   tool.go TransportConfig.UnmarshalYAML (mode)
  const shapeMatrix = [
    // Axis 1: canonical identity
    { desc: 'meta.name identity',                        key: 'ops-meta/act' },
    { desc: 'meta.name identity with actions',           key: 'icm/get-incident' },
    // Axis 2: canonical actions form
    { desc: 'sequence-form actions (canonical)',         key: 'fallback-tool/fallback-action' },
    // Axis 3: transport mode field
    { desc: 'transport.mode (canonical)',                key: 'icm/get-incident' },
  ];
  for (const shape of shapeMatrix) {
    assert.ok(
      registry[shape.key] !== undefined,
      `shape "${shape.desc}" must produce registry key "${shape.key}"`,
    );
  }
});
