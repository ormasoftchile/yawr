'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const { executeRunHandoff, resolveRunPackageMapPath, buildRunArgs } = require('../out/runHandoff');

const SCRATCH_ROOT = path.join(__dirname, '.tmp-runhandoff');

function resetScratch() {
  fs.rmSync(SCRATCH_ROOT, { recursive: true, force: true });
  fs.mkdirSync(SCRATCH_ROOT, { recursive: true });
}

function writeFile(rel, text = '# fixture\n') {
  const full = path.join(SCRATCH_ROOT, rel);
  fs.mkdirSync(path.dirname(full), { recursive: true });
  fs.writeFileSync(full, text);
  return full;
}

test.after(() => {
  fs.rmSync(SCRATCH_ROOT, { recursive: true, force: true });
});

test('executeRunHandoff: serve package-map setting passes sibling requires map to spawned yawr run', async () => {
  resetScratch();
  const runMap = writeFile('packages/incident-routing.vscode-mcp.package-map.yaml');
  const serveMap = writeFile('packages/incident-routing.vscode-mcp.serve-package-map.yaml');
  const runbook = writeFile('runbooks/icm-tsg-router.runbook.yaml');

  let captured = null;
  const fakeExecFile = (file, args, options, callback) => {
    captured = { file, args: [...args], options };
    callback(null, 'ok\n', '');
  };

  const result = await executeRunHandoff({
    bin: 'yawr.exe',
    runbookPath: runbook,
    varPairArgs: ['icm_id=12345'],
    projectRoot: SCRATCH_ROOT,
    packageMapSetting: path.relative(SCRATCH_ROOT, serveMap),
    bridgeVars: { YAWR_VSCODE_BRIDGE_URL: 'http://127.0.0.1:7', YAWR_VSCODE_BRIDGE_TOKEN: 'capability-only' },
    baseEnv: { PATH: 'base-path' },
    execFile: fakeExecFile,
  });

  assert.ok(captured, 'non-vacuity: executeRunHandoff must call execFile');
  assert.equal(captured.file, 'yawr.exe');
  assert.deepEqual(captured.args, [
    'run',
    '--package-map', runMap,
    '--var', 'icm_id=12345',
    runbook,
  ], 'spawned yawr run argv must include the requires-capable package map, not just a helper return value');
  assert.equal(captured.args.includes(serveMap), false,
    'chat /run must not pass the serve-only map when the requires-capable sibling exists');
  assert.equal(captured.options.env.YAWR_VSCODE_BRIDGE_URL, 'http://127.0.0.1:7');
  assert.equal(captured.options.env.YAWR_VSCODE_BRIDGE_TOKEN, 'capability-only');
  assert.equal(result.packageMap.path, runMap);
  assert.equal(result.packageMap.source, 'run-sibling');
});

test('executeRunHandoff: explicit run package-map setting is passed through to spawned yawr run', async () => {
  resetScratch();
  const runMap = writeFile('packages/incident-routing.vscode-mcp.package-map.yaml');
  const runbook = writeFile('runbooks/icm-tsg-router.runbook.yaml');

  let capturedArgs = null;
  const fakeExecFile = (_file, args, _options, callback) => {
    capturedArgs = [...args];
    callback(null, '', '');
  };

  await executeRunHandoff({
    bin: 'yawr',
    runbookPath: runbook,
    varPairArgs: [],
    projectRoot: SCRATCH_ROOT,
    packageMapSetting: path.relative(SCRATCH_ROOT, runMap),
    bridgeVars: {},
    baseEnv: {},
    execFile: fakeExecFile,
  });

  assert.deepEqual(capturedArgs, ['run', '--package-map', runMap, runbook],
    'actual spawned argv must carry the explicit run package-map');
});

test('resolveRunPackageMapPath: serve map without sibling falls back with warning', () => {
  resetScratch();
  const serveMap = writeFile('packages/only.serve-package-map.yaml');

  const result = resolveRunPackageMapPath(SCRATCH_ROOT, path.relative(SCRATCH_ROOT, serveMap));

  assert.equal(result.path, serveMap);
  assert.equal(result.source, 'setting');
  assert.match(result.warning, /no run package-map sibling/);
});

test('buildRunArgs: omits --package-map when no run map is resolved', () => {
  const args = buildRunArgs('runbooks/a.runbook.yaml', ['x=1'], undefined);
  assert.deepEqual(args, ['run', '--var', 'x=1', 'runbooks/a.runbook.yaml']);
  assert.equal(args.includes('--package-map'), false);
});
