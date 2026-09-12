'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const manifest = require('../package.json');

function compiledExtension() {
  return fs.readFileSync(path.join(__dirname, '..', 'out', 'extension.js'), 'utf8');
}

function functionSlice(source, startMarker, endMarker) {
  const start = source.indexOf(startMarker);
  const end = source.indexOf(endMarker, start + startMarker.length);
  assert.ok(start >= 0, `missing start marker ${startMarker}`);
  assert.ok(end > start, `missing end marker ${endMarker}`);
  return source.slice(start, end);
}

function directPanelSlice(source) {
  const start = source.indexOf('async function openDirectGraphPanelForRunbook');
  assert.ok(start >= 0, 'missing direct graph panel function');
  return source.slice(start);
}

test('manifest exposes the full direct graph with no legacy live-server command', () => {
  const commands = manifest.contributes.commands;
  assert.ok(commands.some((item) => item.command === 'yawr.previewGraph'));
  assert.ok(!commands.some((item) => item.command === 'yawr.previewLive'));
  assert.ok(!commands.some((item) => item.command === 'yawr.restartServer'));
  assert.equal(manifest.contributes.configuration.properties['yawr.serverUrl'], undefined);
  assert.equal(manifest.contributes.configuration.properties['yawr.autoStartServer'], undefined);
});

test('extension registers a test seam for the real direct webview', () => {
  const source = compiledExtension();
  const guard = source.indexOf('context.extensionMode === vscode.ExtensionMode.Test');
  const seam = source.indexOf("'yawr.test.openDirectGraphPanel'", guard);
  const productionCommands = source.indexOf("'yawr.validateInputs'", guard);
  assert.ok(guard >= 0, 'missing ExtensionMode.Test guard');
  assert.ok(seam > guard, 'direct graph injection seam must follow the test-mode guard');
  assert.ok(productionCommands > seam, 'test-only command block must close before production commands resume');
});

test('extension activation and static graph do not start the MCP listener', () => {
  const source = compiledExtension();
  const activation = functionSlice(source, 'function activate(context)', 'const HOST_ACTION_TEST_HTML');
  const directPanel = directPanelSlice(source);

  assert.doesNotMatch(activation, /McpBridge\.create/);
  assert.doesNotMatch(directPanel, /McpBridge\.create/);
  assert.match(source, /function ensureMcpBridge/);
});

test('previewGraph routes to the direct panel without starting a server', () => {
  const source = compiledExtension();
  const previewGraph = functionSlice(source, 'async function previewGraph()', 'function findRunbookViewColumn');

  assert.match(previewGraph, /openDirectGraphPanelForRunbook/);
  assert.doesNotMatch(previewGraph, /ensureRunning|createPreviewWebviewHtml/);
});

test('direct panel loads graphjson into local webview assets', () => {
  const source = compiledExtension();
  const directPanel = directPanelSlice(source);

  assert.match(directPanel, /createDirectGraphWebviewHtml/);
  assert.match(directPanel, /loadGraphDocument/);
  assert.match(directPanel, /localResourceRoots/);
  assert.match(directPanel, /onDidSaveTextDocument/);
  assert.match(directPanel, /onDidChangeConfiguration/);
  assert.match(directPanel, /message.*type.*ready/s);
  assert.match(directPanel, /DirectRunSession/);
  assert.match(directPanel, /buildStdioRunArgs/);
  assert.match(directPanel, /resolveRunPackageMapPath/);
  assert.match(directPanel, /child_process_1\.spawn/);
  assert.match(directPanel, /run\.start/);
  assert.match(directPanel, /run\.command/);
  assert.match(directPanel, /parseDirectDebugConfig/);
  assert.match(directPanel, /run\.configure/);
  assert.match(directPanel, /createHostActionBridge/);
  assert.match(directPanel, /graphMayRequireMcpBridge/);
  assert.match(directPanel, /buildRegistryForRun/);
  assert.match(directPanel, /createMcpBridge/);
  assert.match(directPanel, /bridge\s*\?/);
  assert.doesNotMatch(directPanel, /bridge\.updateRegistry/);
  assert.match(directPanel, /pickProjectRoot/);
  assert.match(directPanel, /resolveBinary/);
  assert.match(directPanel, /cwd:\s*projectRoot/);
  assert.match(directPanel, /signal:\s*controller\.signal/);
  assert.match(directPanel, /maxBuffer/);
  assert.match(directPanel, /\.abort\(\)/);
  assert.doesNotMatch(directPanel, /ensureRunning|createPreviewWebviewHtml|iframe/i);
});

test('style changes retain the last graph for webview recovery', () => {
  const source = compiledExtension();
  const directPanel = directPanelSlice(source);

  assert.match(directPanel, /latestMessage\.type === 'graph'/);
  assert.match(directPanel, /latestMessage = \{ \.\.\.latestMessage, style: updatedStyle \}/);
  assert.match(directPanel, /currentStyle/);
  assert.doesNotMatch(directPanel, /publish\(\{ type: 'graph', document, style \}\)/);
});

test('direct graph panel persists and launches route tests through the reviewed artifact boundary', () => {
  const source = compiledExtension();
  const directPanel = directPanelSlice(source);

  assert.match(directPanel, /loadRouteTestArtifacts/);
  assert.match(directPanel, /saveRouteTestArtifact/);
  assert.match(directPanel, /candidate\.type === 'route-test\.save'/);
  assert.match(directPanel, /candidate\.type === 'route-test\.run'/);
  assert.match(directPanel, /buildStdioRunArgs/);
  assert.match(directPanel, /routeTestPath/);
  assert.match(directPanel, /typeof requested\.plan_hash !== 'string'/);
  assert.match(directPanel, /requested\.plan_hash !== currentPlanHash/);
  assert.doesNotMatch(directPanel, /plan_hash:\s*currentPlanHash/);
});

test('compiled extension registers no legacy live-server commands', () => {
  const source = compiledExtension();
  assert.doesNotMatch(source, /registerCommand\(['"]yawr\.previewLive/);
  assert.doesNotMatch(source, /registerCommand\(['"]yawr\.restartServer/);
  assert.doesNotMatch(source, /yawr\.test\.openServedPreviewPanel/);
  assert.doesNotMatch(source, /createPreviewWebviewHtml|ensureRunning|EventSource|<iframe/i);
  assert.doesNotMatch(source, /require\(['"]\.\/serverManager['"]\)|require\(['"]\.\/serverLaunch['"]\)/);
});