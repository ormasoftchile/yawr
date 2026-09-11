const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createHash } = require('node:crypto');
const { spawn, spawnSync } = require('node:child_process');
const { value } = require('../scripts/environment.cjs');
const artifacts = value('WORKFLOW_ARTIFACTS');
const candidate = value('WORKFLOW_RUNTIME_BINARY');
assert.ok(artifacts && candidate, 'Explicit task runtime candidate and artifacts required');
const inspection = JSON.parse(fs.readFileSync(path.join(artifacts, 'inspection.json'), 'utf8'));
const root = inspection.fixtureRoot;
for (const file of inspection.inspectedFixtureHashes) {
  assert.equal(createHash('sha256').update(fs.readFileSync(path.join(root, file.file))).digest('hex'), file.sha256,
    `STOP: synthetic binding changed: ${file.file}`);
}
const yaml = require('yaml');
const transport = yaml.parse(fs.readFileSync(path.join(root, 'tests', 'performance-runbook-tools', 'query-mock', 'tools', 'query-sterling-kusto.tool.yaml'), 'utf8'));
assert.equal(transport.transport.command, 'curl.exe');
assert.equal(transport.transport.mode, 'native');
assert.deepEqual(transport.actions[0].argv.slice(0, 6), ['--silent', '--show-error', '--fail', '--globoff', '--proto', '=file']);
assert.equal(transport.actions[0].argv.length, 7);
assert.ok(transport.actions[0].argv[6].startsWith('file:///C:/One/yawr-sqllivesite/tests/performance-runbook-tools/payloads/'));
const runtimeRoot = path.join(artifacts, 'native-fixture-runs');
fs.mkdirSync(runtimeRoot, { recursive: true });
const home = path.join(runtimeRoot, 'isolated-home');
fs.mkdirSync(home, { recursive: true });
// An explicit first-found empty curl configuration prevents any private/user config lookup.
for (const file of ['.curlrc', '_curlrc']) fs.writeFileSync(path.join(home, file), '');
const system32 = path.join(process.env.SystemRoot, 'System32');
const env = { SystemRoot: process.env.SystemRoot, WINDIR: process.env.SystemRoot, SystemDrive: process.env.SystemDrive,
  ComSpec: process.env.ComSpec, PATHEXT: '.COM;.EXE;.BAT;.CMD', PATH: system32,
  CURL_HOME: home, HOME: home, USERPROFILE: home, APPDATA: home, LOCALAPPDATA: home,
  TEMP: home, TMP: home, HTTP_PROXY: 'http://127.0.0.1:9', HTTPS_PROXY: 'http://127.0.0.1:9',
  ALL_PROXY: 'http://127.0.0.1:9', YAWR_TELEMETRY_DISABLED: '1' };
async function run(name, expectedFailure) {
  const runDir = path.join(runtimeRoot, `${name}-${Date.now()}.runs`);
  const trace = path.join(runtimeRoot, `${name}-${Date.now()}.trace.jsonl`);
  const args = ['run', '--stdio', '--package-map', path.join(root, 'tests', 'icm-db-info-probe', 'synthetic.package-map.yaml'),
    '--run-dir', runDir, '--trace', trace, path.join(root, 'tests', 'icm-db-info-probe', name + '.runbook.yaml')];
  const child = spawn(candidate, args, { cwd: root, env, windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'] });
  const frames = []; let buffer = '', errors = '', unsafe = '';
  const timer = setTimeout(() => { unsafe = 'fixture deadline exceeded'; child.kill(); }, 60000);
  child.stdout.setEncoding('utf8'); child.stderr.setEncoding('utf8');
  child.stderr.on('data', text => { errors += text; });
  child.stdout.on('data', text => {
    buffer += text;
    while (buffer.includes('\n')) {
      const index = buffer.indexOf('\n'), line = buffer.slice(0, index); buffer = buffer.slice(index + 1);
      if (!line.trim()) continue;
      let frame;
      try { frame = JSON.parse(line); } catch { unsafe = 'non-protocol output'; child.kill(); return; }
      frames.push(frame);
      if (frame.type === 'run.finished') child.stdin.end();
      if (frame.type === 'interaction.pending' || frame.type === 'pending' || frame.type?.includes('host-action')) {
        unsafe = 'unexpected interaction/auth/host action — fixture stopped without answering'; child.kill(); return;
      }
    }
  });
  const exit = await new Promise((resolve, reject) => { child.on('error', reject); child.on('close', resolve); });
  clearTimeout(timer);
  assert.equal(unsafe, '', unsafe);
  const events = frames.filter(frame => frame.type === 'run.event').map(frame => frame.event);
  const summary = frames.find(frame => frame.type === 'run.finished');
  assert.ok(summary, errors || 'missing terminal summary');
  assert.equal(summary.status, expectedFailure ? 'failed' : 'completed', JSON.stringify(summary));
  const reached = events.filter(event => event.kind === 'step/started').map(event => event.payload.qualified_node_id);
  if (expectedFailure) {
    assert.ok(events.some(event => event.kind === 'step/failed' && event.payload.qualified_node_id === 'launch_scenario/validate_query_scope'));
    assert.ok(!reached.some(id => id.includes('get_database_info') || id.includes('results/summary')));
  } else {
    const report = events.find(event => event.kind === 'step/completed' && event.payload.qualified_node_id === 'launch_scenario/results/summary');
    assert.ok(report?.payload.display_presentation, 'actual executed Database results metadata');
    const selected = require('../out/displayPresentation').selectDisplayPresentation(report.payload.display_presentation, report.payload.output);
    assert.match(selected.text, /^# Database results/);
    assert.match(selected.text, /```json/);
    assert.ok(!selected.text.includes('${'));
    assert.ok(reached.some(id => id.endsWith('/configuration')));
  }
  const graph = spawnSync(candidate, ['preview', '--run-dir', runDir, '--run-id', summary.runID, '--format', 'graphjson'],
    { cwd: root, env, encoding: 'utf8', timeout: 60000, maxBuffer: 16 * 1024 * 1024 });
  const capture = { name, args, exit, summary, events, frames, trace, runDir,
    historyPreview: graph.status === 0 ? JSON.parse(graph.stdout) : undefined,
    historyPreviewError: graph.status !== 0 ? graph.stderr : undefined };
  fs.writeFileSync(path.join(runtimeRoot, name + '.capture.json'), JSON.stringify(capture, null, 2));
  return { name, exit, status: summary.status, reached: reached.length, trace, runDir, historyPreviewAvailable: graph.status === 0 };
}
(async () => {
  const results = [await run('prefilled-singleton', false), await run('prefilled-invalid-window', true)];
  fs.writeFileSync(path.join(runtimeRoot, 'results.json'), JSON.stringify({ results,
    candidate, candidateSHA256: createHash('sha256').update(fs.readFileSync(candidate)).digest('hex'),
    curl: path.join(system32, 'curl.exe'), curlSHA256: createHash('sha256').update(fs.readFileSync(path.join(system32, 'curl.exe'))).digest('hex'),
    environment: 'allowlisted child environment, isolated empty curl config; System32-only PATH; no inherited business credentials',
    bindings: inspection.inspectedFixtureHashes }, null, 2));
  console.log(JSON.stringify(results));
})().catch(error => { console.error(error); process.exitCode = 1; });
