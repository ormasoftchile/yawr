'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const root = path.join(__dirname, '..');

test('source Extension Host tests use YAWR variables only', () => {
  const source = fs.readFileSync(path.join(root, 'test', 'suite', 'extension.test.ts'), 'utf8');

  assert.match(source, /environmentValue\(process\.env,\s*['"]E2E_BINARY['"]\)/);
  assert.match(source, /environmentValue\(process\.env,\s*['"]CORE_ROOT['"]\)/);
  assert.match(source, /YAWR_\$\{suffix\}/);
  assert.doesNotMatch(source, /process\.env\.YAWR_(?:E2E_BINARY|CORE_ROOT)/);
  assert.doesNotMatch(source, /Uri\.joinPath\(workspaceFolder\.uri,\s*['"]\.\.['"],\s*['"]yawr['"]\)/);
});

test('VS Code test configuration exposes source and production-surface labels', () => {
  const config = fs.readFileSync(path.join(root, '.vscode-test.mjs'), 'utf8');
  const manifest = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));

  assert.match(config, /label:\s*['"]source['"]/);
  assert.match(config, /label:\s*['"]production-surface['"]/);
  assert.equal((config.match(/version:\s*['"]1\.137\.0['"]/g) || []).length, 2);
  assert.equal(manifest.engines.vscode, '^1.137.0');
  assert.match(manifest.scripts['test:e2e'], /run-vscode-test\.mjs source/);
  assert.match(manifest.scripts['test:e2e:vsix'], /run-vscode-test\.mjs production-surface/);
});

test('component wiring uses the monorepo runtime without a second checkout', () => {
  const powershellBuild = fs.readFileSync(path.join(root, 'scripts', 'build-cli.ps1'), 'utf8');
  const shellBuild = fs.readFileSync(path.join(root, 'scripts', 'build-cli.sh'), 'utf8');
  const highlightingBuild = fs.readFileSync(path.join(root, 'scripts', 'build-highlighting.mjs'), 'utf8');
  const cacheTest = fs.readFileSync(path.join(root, 'test', 'previewDocumentCache.test.js'), 'utf8');
  const wireTest = fs.readFileSync(path.join(root, 'test', 'crossRepoHostActionWire.js'), 'utf8');

  assert.match(powershellBuild, /Join-Path \$ExtensionRoot '\.\.\\\.\.\\runtime'/);
  assert.match(shellBuild, /\$EXTENSION_ROOT\/\.\.\/\.\.\/runtime/);
  assert.match(highlightingBuild, /environmentValue\('CORE_ROOT'\) \|\| resolve\(root, '\.\.', '\.\.', 'runtime'\)/);
  assert.match(cacheTest, /value\('CORE_ROOT'\) \|\| path\.resolve\(__dirname, '\.\.', '\.\.', '\.\.', 'runtime'\)/);
  assert.match(wireTest, /value\('CORE_ROOT'\) \|\| path\.resolve\(__dirname, '\.\.', '\.\.', '\.\.', 'runtime'\)/);
  assert.doesNotMatch(powershellBuild + shellBuild, /yawr-core|Clone https:\/\/github\.com\/ormasoftchile\/yawr/);
});