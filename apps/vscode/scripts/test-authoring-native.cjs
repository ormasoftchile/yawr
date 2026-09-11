const fs = require('node:fs');
const path = require('node:path');
const { value, pair } = require('./environment.cjs');
const { runTests } = require('@vscode/test-electron');
const root = path.resolve(__dirname, '..');
const configPath = value('AUTHORING_NATIVE_CONFIG');
if (!configPath || !fs.existsSync(configPath)) throw new Error('Set YAWR_AUTHORING_NATIVE_CONFIG (or its YAWR alias) to the prepared native-config.json; production helper proof never skips.');
const config = JSON.parse(fs.readFileSync(configPath, 'utf8'));
for (const key of ['helper', 'yamlExtension', 'vscodeExecutable', 'callerRunbook', 'callerProjectRoot', 'callerPackageMap']) {
  if (!path.isAbsolute(config[key] ?? '') || !fs.existsSync(config[key])) throw new Error(`Native authoring prerequisite unavailable: ${key}. Supply the real cached artifact; no install, mock or skip.`);
}
const runRoot = path.join(root, '.vscode-test', 'authoring-' + Date.now().toString(36));
for (const dir of ['workspace', 'profile\\User', 'extensions', 'appdata', 'localappdata', 'runtime-data']) fs.mkdirSync(path.join(runRoot, dir), { recursive: true });
fs.writeFileSync(path.join(runRoot, 'profile', 'User', 'settings.json'), JSON.stringify({
  'yawr.highlighting.developmentHelperPath': config.helper, 'chat.disableAIFeatures': true,
  'telemetry.telemetryLevel': 'off', 'extensions.autoCheckUpdates': false, 'extensions.autoUpdate': false,
}));
fs.cpSync(config.yamlExtension, path.join(runRoot, 'extensions', 'redhat.vscode-yaml'), { recursive: true });
fs.writeFileSync(path.join(runRoot, 'workspace', 'schema.json'), JSON.stringify({
  type: 'object', properties: { localMode: { type: 'string', enum: ['offline-schema-only-value'] } },
}));
const workspace = path.join(runRoot, 'authoring.code-workspace');
fs.writeFileSync(workspace, JSON.stringify({
  folders: [{ path: path.join(runRoot, 'workspace') }],
  settings: {
    'yaml.schemas': { [path.join(runRoot, 'workspace', 'schema.json')]: ['*.yaml'] },
    'yaml.schemaStore.enable': false, 'yaml.kubernetesCRDStore.enable': false, 'yaml.completion': true,
    'editor.wordBasedSuggestions': 'off', 'editor.suggestSelection': 'first',
    'yawr.highlighting.enabled': false, 'yawr.autocomplete.enabled': true,
    'yawr.highlighting.developmentHelperPath': config.helper,
    'telemetry.telemetryLevel': 'off', 'extensions.autoCheckUpdates': false, 'extensions.autoUpdate': false,
    'workbench.startupEditor': 'none', 'security.workspace.trust.enabled': false,
    'chat.disableAIFeatures': true, 'chat.mcp.discovery.enabled': {}, 'chat.mcp.access': 'none',
  },
}));
runTests({
  vscodeExecutablePath: config.vscodeExecutable, extensionDevelopmentPath: root,
  extensionTestsPath: path.join(root, 'test', 'authoring-native.cjs'),
  launchArgs: [workspace, `--user-data-dir=${path.join(runRoot, 'profile')}`, `--extensions-dir=${path.join(runRoot, 'extensions')}`,
    '--skip-welcome', '--skip-release-notes', '--disable-workspace-trust', '--disable-telemetry', '--disable-background-networking',
    '--disable-gpu', '--new-window', '--proxy-server=http://127.0.0.1:9', '--proxy-bypass-list=<-loopback>',
    '--disable-extension=vscode.github', '--disable-extension=vscode.github-authentication',
    '--disable-extension=vscode.microsoft-authentication', '--disable-extension=vscode.git', '--disable-extension=GitHub.copilot-chat'],
  extensionTestsEnv: { ...process.env, ...pair('AUTHORING_NATIVE_CONFIG', path.resolve(configPath)), ...pair('AUTHORING_TEST_ROOT', runRoot),
    APPDATA: path.join(runRoot, 'appdata'), LOCALAPPDATA: path.join(runRoot, 'localappdata'),
    TEMP: path.join(runRoot, 'runtime-data'), TMP: path.join(runRoot, 'runtime-data'),
    HTTP_PROXY: 'http://127.0.0.1:9', HTTPS_PROXY: 'http://127.0.0.1:9', ALL_PROXY: 'http://127.0.0.1:9' },
}).then(() => console.log(`Native authoring evidence: ${runRoot}`)).catch(error => { console.error(error); process.exitCode = 1; });
