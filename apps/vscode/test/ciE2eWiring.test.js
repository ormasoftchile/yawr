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
  assert.match(manifest.scripts.test, /extension-lifecycle\.mjs unit test\/\*\.test\.js/);
  assert.match(manifest.scripts['test:e2e'], /extension-lifecycle\.mjs source/);
  assert.match(manifest.scripts['test:e2e:vsix'], /extension-lifecycle\.mjs installed/);
  assert.match(manifest.scripts['validate:vsix'], /extension-lifecycle\.mjs validate-vsix/);
  assert.match(manifest.scripts.package, /extension-lifecycle\.mjs package/);
  assert.match(manifest.scripts['package:validate'], /extension-lifecycle\.mjs package-check/);
  const lifecycle = fs.readFileSync(path.join(root, 'scripts', 'extension-lifecycle.mjs'), 'utf8');
  assert.match(lifecycle, /finally\s*\{/);
  assert.match(lifecycle, /media\/graph\.css/);
  assert.match(lifecycle, /restoreFiles\(snapshot\)/);
  assert.match(lifecycle, /rm\(join\(root, 'out'\)/);
  assert.match(lifecycle, /rm\(join\(root, '\.vscode-test'\)/);
  assert.match(lifecycle, /if \(mode !== 'package'\) await rm\(vsixPath/);
  assert.match(lifecycle, /600_000/);
  assert.match(lifecycle, /cleanupReserveMs = 30_000/);
  assert.match(lifecycle, /stallMs = 120_000/);
  assert.match(lifecycle, /require\.resolve\(['"]@vscode\/vsce\/vsce['"]\)/);
  assert.match(lifecycle, /FAIL phase=.*reason=/);
  assert.match(lifecycle, /PASS installed-vsix/);
  assert.match(lifecycle, /\(no output captured\)/);
  assert.doesNotMatch(lifecycle, /run\(['"]npx['"], \[['"]vsce['"]/);
  const bounded = fs.readFileSync(path.join(root, 'scripts', 'bounded-process.mjs'), 'utf8');
  assert.match(bounded, /terminateProcessTree/);
  assert.match(bounded, /taskkill\.exe/);
  assert.match(bounded, /process\.kill\(-pid, 'SIGKILL'\)/);
  const runner = fs.readFileSync(path.join(root, 'scripts', 'run-vscode-test.mjs'), 'utf8');
  assert.match(runner, /preserveFailureEvidence\(runRoot,\s*\{\s*failure,\s*repositoryRoot\s*\}\)/);
  assert.match(runner, /failure evidence artifact: \$\{evidence\.artifactId\}/);
  assert.match(runner, /installedExtensionDirectory/);
  assert.match(runner, /diagnostic-state\.json/);
  assert.doesNotMatch(runner, /failure evidence: \$\{evidencePath\}/);
  const productionSurface = fs.readFileSync(path.join(root, 'test', 'suite', 'productionSurface.test.ts'), 'utf8');
  assert.match(productionSurface, /replace\(\/\^mainThreadWebview-\/,\s*['"]{2}\)/);
  assert.match(productionSurface, /canonicalWebviewViewType\(preview\.input\.viewType\)/);
});

test('active and template coordinator agents share the bounded safeguard contract', () => {
  const repositoryRoot = path.resolve(root, '..', '..');
  const active = fs.readFileSync(path.join(repositoryRoot, '.github', 'agents', 'squad.agent.md'), 'utf8');
  const template = fs.readFileSync(path.join(repositoryRoot, '.squad', 'templates', 'squad.agent.md.template'), 'utf8');
  const boundedSection = (source) => source.match(
    /### Bounded Execution Safeguards\r?\n[\s\S]*?(?=\r?\n\*\*On every session start:\*\*)/,
  )?.[0].replace(/\r\n/g, '\n');

  assert.ok(boundedSection(active), 'active coordinator must contain bounded safeguards');
  assert.equal(boundedSection(template), boundedSection(active));
  assert.match(template, /0\.0\.0-source/);
});

test('root, task, and CI wiring expose the bounded installed-VSIX validator', () => {
  const repositoryRoot = path.resolve(root, '..', '..');
  const rootManifest = JSON.parse(fs.readFileSync(path.join(repositoryRoot, 'package.json'), 'utf8'));
  const tasks = JSON.parse(fs.readFileSync(path.join(repositoryRoot, '.vscode', 'tasks.json'), 'utf8'));
  const workflow = fs.readFileSync(path.join(repositoryRoot, '.github', 'workflows', 'ci.yml'), 'utf8');

  assert.equal(rootManifest.scripts['extension:validate:vsix'], 'npm run validate:vsix --workspace apps/vscode');
  assert.equal(rootManifest.scripts['extension:e2e:vsix'], undefined);
  assert.ok(tasks.tasks.some((task) => task.script === 'extension:validate:vsix'));
  assert.match(workflow, /extension-installed-vsix:[\s\S]*timeout-minutes:\s*15/);
  assert.match(workflow, /xvfb-run -a npm run extension:validate:vsix/);
  assert.doesNotMatch(workflow, /extension-installed-vsix:[\s\S]*npm run extension:e2e:vsix/);
});

test('component wiring uses the monorepo runtime without a second checkout', () => {
  const powershellBuild = fs.readFileSync(path.join(root, 'scripts', 'build-cli.ps1'), 'utf8');
  const shellBuild = fs.readFileSync(path.join(root, 'scripts', 'build-cli.sh'), 'utf8');
  const highlightingBuild = fs.readFileSync(path.join(root, 'scripts', 'build-highlighting.mjs'), 'utf8');
  const helperPackaging = fs.readFileSync(path.join(root, 'scripts', 'package-helper.mjs'), 'utf8');
  const cacheTest = fs.readFileSync(path.join(root, 'test', 'previewDocumentCache.test.js'), 'utf8');
  const wireTest = fs.readFileSync(path.join(root, 'test', 'crossRepoHostActionWire.js'), 'utf8');

  assert.match(powershellBuild, /Join-Path \$ExtensionRoot '\.\.\\\.\.\\runtime'/);
  assert.match(shellBuild, /\$EXTENSION_ROOT\/\.\.\/\.\.\/runtime/);
  assert.match(highlightingBuild, /environmentValue\('CORE_ROOT'\) \|\| resolve\(root, '\.\.', '\.\.', 'runtime'\)/);
  assert.match(helperPackaging, /\['authoring', 'capabilities', '--v3'\]/);
  assert.match(helperPackaging, /assertAuthoringParity\(sourceAuthoring, packagedAuthoring\)/);
  assert.match(cacheTest, /value\('CORE_ROOT'\) \|\| path\.resolve\(__dirname, '\.\.', '\.\.', '\.\.', 'runtime'\)/);
  assert.match(wireTest, /value\('CORE_ROOT'\) \|\| path\.resolve\(__dirname, '\.\.', '\.\.', '\.\.', 'runtime'\)/);
  assert.doesNotMatch(powershellBuild + shellBuild, /Clone https:\/\/github\.com\/ormasoftchile\/yawr/);
});