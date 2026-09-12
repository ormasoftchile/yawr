const fs = require('node:fs');
const path = require('node:path');
const { value, pair } = require('./environment.cjs');
const net = require('node:net');
const { runTests } = require('@vscode/test-electron');
const root = path.resolve(__dirname, '..');
const expressionConfigPath = value('EXPRESSION_NATIVE_CONFIG');
const expressionConfig = expressionConfigPath && JSON.parse(fs.readFileSync(expressionConfigPath, 'utf8'));
if (expressionConfig) {
  Object.assign(process.env, pair('PRESENTATION_HELPER', expressionConfig.helper || path.join(root, 'bin', `${process.platform}-${process.arch}`, process.platform === 'win32' ? 'yawr.exe' : 'yawr')));
  if (expressionConfig.yamlExtension) Object.assign(process.env, pair('YAML_EXTENSION', expressionConfig.yamlExtension));
}
require('esbuild').buildSync({ entryPoints: [path.join(root, 'test', 'highlighting-ui.tsx')], bundle: true,
  platform: 'browser', format: 'iife', target: 'es2022', outfile: path.join(root, '.vscode-test', 'highlighting-ui.js') });
const runRoot = path.join(root, '.vscode-test', 'highlighting-' + Date.now().toString(36));
const helper = value('PRESENTATION_HELPER');
const yaml = value('YAML_EXTENSION');
const authored = value('PRESENTATION_AUTHORED_RUNBOOK');
if (!helper || (!authored && !yaml && !expressionConfig)) throw new Error('Supply explicit actual helper and cached YAML extension paths');
for (const dir of ['workspace', 'workspace\\.vscode', 'profile', 'extensions', 'appdata', 'localappdata', 'runtime-data']) fs.mkdirSync(path.join(runRoot, dir), { recursive: true });
if (yaml && (!authored || expressionConfig)) fs.cpSync(yaml, path.join(runRoot, 'extensions', 'redhat.vscode-yaml'), { recursive: true });
fs.writeFileSync(path.join(runRoot, 'workspace', 'schema.json'), JSON.stringify({
  type: 'object', required: ['requiredLocalProperty'], properties: {
    localMode: { type: 'string', enum: ['offline-schema-only-value'] }, requiredLocalProperty: { type: 'string' },
  },
}));
fs.writeFileSync(path.join(runRoot, 'workspace', '.vscode', 'settings.json'), JSON.stringify({
  'yaml.schemas': { './schema.json': ['*.yaml'] },
  'yaml.schemaStore.enable': false, 'yaml.kubernetesCRDStore.enable': false, 'yaml.completion': true,
  'yaml.validate': true, 'yaml.format.enable': true, 'editor.wordBasedSuggestions': 'off',
  'yawr.highlighting.enabled': false, 'yawr.highlighting.developmentHelperPath': path.resolve(helper),
  'telemetry.telemetryLevel': 'off', 'extensions.autoCheckUpdates': false, 'extensions.autoUpdate': false,
  'workbench.startupEditor': 'none', 'security.workspace.trust.enabled': false,
}));
let workspace = path.join(runRoot, 'workspace');
if (expressionConfig?.mode === 'actual') {
  workspace = path.join(runRoot, 'actual.code-workspace');
  fs.writeFileSync(workspace, JSON.stringify({
    folders: [{ path: expressionConfig.projectRoot }],
    settings: JSON.parse(fs.readFileSync(path.join(runRoot, 'workspace', '.vscode', 'settings.json'), 'utf8')),
  }));
}
async function main() {
  const server = net.createServer();
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const port = server.address().port; await new Promise(resolve => server.close(resolve));
  fs.writeFileSync(path.join(runRoot, 'launch.json'), JSON.stringify({ helper, yaml, port, root: runRoot }));
  await runTests({
    vscodeExecutablePath: path.join(root, '.vscode-test', 'vscode-win32-x64-archive-1.136.1', 'Code.exe'),
    extensionDevelopmentPath: root,
    extensionTestsPath: path.join(root, 'test', expressionConfig ? 'expression-highlighting-native.cjs' : authored ? 'highlighting-authored-native.cjs' : 'highlighting-native.cjs'),
    launchArgs: [workspace, `--user-data-dir=${path.join(runRoot, 'profile')}`,
      `--extensions-dir=${path.join(runRoot, 'extensions')}`, '--skip-welcome', '--skip-release-notes',
      '--disable-workspace-trust', '--disable-telemetry', '--disable-background-networking', '--disable-gpu', '--new-window',
      '--proxy-server=http://127.0.0.1:9', '--proxy-bypass-list=<-loopback>',
      '--disable-extension=vscode.github', '--disable-extension=vscode.github-authentication',
      '--disable-extension=vscode.microsoft-authentication', '--disable-extension=vscode.git',
      '--disable-extension=GitHub.copilot-chat', '--disable-extension=TypeScriptTeam.jsts-chat-features',
      `--remote-debugging-port=${port}`, '--remote-debugging-address=127.0.0.1'],
    extensionTestsEnv: { ...process.env, ...pair('HIGHLIGHTING_TEST_ROOT', runRoot), ...pair('HIGHLIGHTING_CDP_PORT', String(port)),
      APPDATA: path.join(runRoot, 'appdata'), LOCALAPPDATA: path.join(runRoot, 'localappdata'),
      TEMP: path.join(runRoot, 'runtime-data'), TMP: path.join(runRoot, 'runtime-data'),
      HTTP_PROXY: 'http://127.0.0.1:9', HTTPS_PROXY: 'http://127.0.0.1:9', ALL_PROXY: 'http://127.0.0.1:9' },
  });
  console.log(`Native production evidence: ${runRoot}`);
}
main().catch(error => { console.error(error); process.exitCode = 1; });
