import { defineConfig } from '@vscode/test-cli';
import { resolve, dirname, join } from 'path';
import { fileURLToPath } from 'url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const environmentValue = (suffix) => process.env[`YAWR_${suffix}`];
const testGrep = environmentValue('TEST_GREP');

// Extension-host integration test configuration.
//
// `npm run test:e2e` compiles the test suite (tsconfig.test.json → out/test/suite/)
// then launches a real VS Code extension host (via @vscode/test-electron) with
// the extension loaded from the workspace root.
//
// The host uses the workspace folder so the extension can resolve workspace-
// relative paths during activation.  Mocha timeout is generous to allow the
// Electron process to start on slow CI machines.
//
// YAWR_TEST_GREP limits Mocha to a focused acceptance check.
const vsixHarnessPath = resolve(__dirname, 'test', 'vsix-harness');

// Per-invocation isolated user-data directory for the production-surface run.
//
// The default @vscode/test-electron profile dir (.vscode-test/user-data/) is
// shared across all runs. If a previous invocation was aborted, its Electron
// process tree may still be alive and holding an IPC server at that path. A
// new invocation connecting to that IPC server triggers VS Code's singleton
// check:
//
//   "Running extension tests from the command line is currently only supported
//   if no other instance of Code is running."
//
// Using a timestamp-based run ID produces a fresh --user-data-dir per
// invocation. A lingering process tree holds a different IPC handle (different
// MD5 hash of the user-data path) and causes no conflict.
const vsixRunId = environmentValue('TEST_RUN_ID') ?? `${Date.now().toString(36)}-${process.pid}`;
const testStateRoot = environmentValue('TEST_STATE_ROOT') ?? join(__dirname, '.vscode-test', 'runs', vsixRunId);
const productionWorkspace = join(testStateRoot, 'workspace');
const vsixUserDataDir = join(testStateRoot, 'profile');
const sourceUserDataDir = join(testStateRoot, 'profile');
const sourceExtensionsDir = join(testStateRoot, 'extensions');
const vsixExtensionsDir = join(testStateRoot, 'extensions');

const hermeticLaunchArgs = (userDataDir) => [
  `--user-data-dir=${userDataDir}`,
  '--disable-extension=GitHub.copilot-chat',
  '--disable-extension=vscode.github',
  '--disable-extension=vscode.github-authentication',
  '--disable-extension=vscode.microsoft-authentication',
  '--disable-workspace-trust',
  '--skip-welcome',
  '--skip-release-notes',
];

export default defineConfig([
  {
    // Default configuration: loads the extension from the source worktree via
    // extensionDevelopmentPath (the package root). Used by HAB-E2E-02 and all
    // other suite tests that do not require an installed VSIX.
    label: 'source',
    version: '1.137.0',
    files: [
      'out/test/suite/extension.test.js',
      'out/test/suite/hostActionBridge.test.js',
      'out/test/suite/sessionGraph.test.js',
    ],
    workspaceFolder: '.',
    launchArgs: [
      ...hermeticLaunchArgs(sourceUserDataDir),
      `--extensions-dir=${sourceExtensionsDir}`,
    ],
    mocha: {
      timeout: 90000,
      ...(testGrep ? { grep: testGrep } : {}),
    },
  },
  {
    // Installed-VSIX production-surface configuration.
    //
    // This configuration proves that the extension host loads yawr-preview from
    // the INSTALLED VSIX (not from the source worktree via extensionDevelopmentPath).
    //
    // Key differences from the default configuration:
    //   • extensionDevelopmentPath: the no-op VSIX test harness (plus optional
    //     XTS). yawr-preview source is NOT loaded as a development extension.
    //   • run-vscode-test.mjs installs yawr-preview.vsix into this invocation's
    //     isolated extensions directory before starting the host.
    //   • files: only productionSurface.test.js — the installed-VSIX acceptance test.
    //
    // Prerequisites: yawr-preview.vsix must exist at the workspace root.
    // Run `npm run package` to produce it before invoking this configuration.
    //
    // Run via: vscode-test --label production-surface
    label: 'production-surface',
    version: '1.137.0',
    files: 'out/test/suite/productionSurface.test.js',
    extensionDevelopmentPath: [vsixHarnessPath],
    workspaceFolder: productionWorkspace,
    // Unique per-invocation user-data-dir — see comment above vsixRunId.
    launchArgs: [
      ...hermeticLaunchArgs(vsixUserDataDir),
      `--extensions-dir=${vsixExtensionsDir}`,
      ...(environmentValue('TEST_CDP_PORT') ? [`--remote-debugging-port=${environmentValue('TEST_CDP_PORT')}`, '--remote-debugging-address=127.0.0.1'] : []),
    ],
    mocha: {
      timeout: 120000,
      ...(testGrep ? { grep: testGrep } : {}),
    },
  },
]);

// VSIX-only acceptance remains isolated in the production-surface profile.
