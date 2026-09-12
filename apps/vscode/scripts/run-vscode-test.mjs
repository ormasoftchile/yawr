import { mkdir, readFile, readdir, writeFile } from 'node:fs/promises';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import {
  BoundedProcessError,
  runBoundedProcess,
  runWithRetry,
} from './bounded-process.mjs';
import { preserveFailureEvidence } from './validation-evidence.mjs';

const label = process.argv[2];
if (!label || !['source', 'production-surface', 'download'].includes(label)) {
  throw new Error('expected VS Code test label: source, production-surface, or download');
}

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const repositoryRoot = resolve(root, '..', '..');
const stallMs = 120_000;
const runID = `${Date.now().toString(36)}-${process.pid}-${Math.random().toString(16).slice(2)}`;
const runRoot = join(root, '.vscode-test', 'runs', runID);
const vscodePathFile = join(runRoot, 'vscode-path.txt');
const diagnosticStateFile = join(runRoot, 'diagnostic-state.json');
const executable = join(root, '..', '..', 'node_modules', '@vscode', 'test-cli', 'out', 'bin.mjs');
const portableGoBin = join(root, '..', '..', '.tools', 'go1.25.7', 'go', 'bin');
const packagedHelper = join(root, 'bin', `${process.platform}-${process.arch}`, process.platform === 'win32' ? 'yawr.exe' : 'yawr');
const require = createRequire(import.meta.url);
const { downloadAndUnzipVSCode, resolveCliPathFromVSCodeExecutablePath } = require('@vscode/test-electron');
const environment = {
  ...process.env,
  PATH: `${portableGoBin}${process.platform === 'win32' ? ';' : ':'}${process.env.PATH ?? ''}`,
  YAWR_TEST_RUN_ID: runID,
  YAWR_TEST_STATE_ROOT: runRoot,
  YAWR_E2E_BINARY: packagedHelper,
  YAWR_EXPRESSION_HELPER: packagedHelper,
};

const transient = (error) => error instanceof BoundedProcessError && (
  error.kind === 'timeout'
  || error.kind === 'stall'
  || /\b(?:ECONNRESET|ECONNREFUSED|ETIMEDOUT|EAI_AGAIN|ENOTFOUND|EPIPE|EBUSY|EPERM|429|50[234])\b/i.test(error.output ?? '')
);
const retryTransient = (operation, phase) => runWithRetry(operation, {
  retries: 1,
  shouldRetry: transient,
  onRetry: (_error, attempt) => console.error(`[extension:validate:vsix] retry ${attempt}/1: ${phase}`),
});

async function writeDiagnosticState(update) {
  let current = {};
  try {
    current = JSON.parse(await readFile(diagnosticStateFile, 'utf8'));
  } catch (error) {
    if (error?.code !== 'ENOENT') throw error;
  }
  await writeFile(diagnosticStateFile, `${JSON.stringify({ ...current, ...update }, null, 2)}\n`);
}

async function findInstalledExtension() {
  const extensionsRoot = join(runRoot, 'extensions');
  const entries = await readdir(extensionsRoot, { withFileTypes: true });
  const installed = entries.find((entry) => entry.isDirectory() && entry.name.startsWith('ormasoftchile.yawr-preview-'));
  return installed ? join(extensionsRoot, installed.name) : undefined;
}

let failure;
try {
  await mkdir(join(runRoot, 'workspace'), { recursive: true });
  await writeDiagnosticState({ label });
  if (label === 'download') {
    const vscodeExecutable = await downloadAndUnzipVSCode({
      version: '1.137.0',
      cachePath: join(root, '.vscode-test'),
    });
    await writeFile(process.env.YAWR_VSCODE_PATH_FILE || vscodePathFile, vscodeExecutable);
  } else {
    if (label === 'production-surface') {
      const pathFile = process.env.YAWR_VSCODE_PATH_FILE || vscodePathFile;
      await retryTransient(() => runBoundedProcess(process.execPath, [
        fileURLToPath(import.meta.url),
        'download',
      ], {
        cwd: root,
        env: { ...environment, YAWR_VSCODE_PATH_FILE: pathFile },
        timeoutMs: 180_000,
        stallMs,
        label: 'VS Code download',
      }), 'VS Code download');
      const vscodeExecutable = (await readFile(pathFile, 'utf8')).trim();
      const cli = resolveCliPathFromVSCodeExecutablePath(vscodeExecutable);
      await retryTransient(() => runBoundedProcess(cli, [
        `--user-data-dir=${join(runRoot, 'profile')}`,
        `--extensions-dir=${join(runRoot, 'extensions')}`,
        '--install-extension',
        process.env.YAWR_VSIX_PATH || join(root, 'yawr-preview.vsix'),
      ], {
        cwd: root,
        env: environment,
        timeoutMs: 180_000,
        stallMs,
        label: 'VSIX install',
        shell: process.platform === 'win32',
      }), 'VSIX install');
      const installedExtensionPath = await findInstalledExtension();
      await writeDiagnosticState({
        installedSource: 'vsix',
        installedExtensionDirectory: installedExtensionPath ? basename(installedExtensionPath) : null,
        installComplete: true,
      });
    }
    await runBoundedProcess(process.execPath, [executable, '--label', label], {
      cwd: root,
      env: environment,
      timeoutMs: 240_000,
      stallMs,
      label: `${label} Extension Host`,
    });
  }
} catch (error) {
  failure = error;
  process.exitCode = 1;
  if (error instanceof BoundedProcessError) {
    const reason = error.kind === 'exit' ? `exit-code-${error.code ?? 'unknown'}` : error.kind;
    console.error(`extension:validate:vsix FAIL phase="${error.label ?? label}" reason="${reason}"`);
    console.error(`extension:validate:vsix OUTPUT\n${error.output?.trim() || '(no output captured)'}`);
  } else {
    console.error(error);
  }
}

if (failure) {
  try {
    const evidence = await preserveFailureEvidence(runRoot, { failure, repositoryRoot });
    console.error(`extension:validate:vsix failure evidence artifact: ${evidence.artifactId}`);
  } catch (evidenceError) {
    console.error(`extension:validate:vsix FAIL phase="failure evidence" reason="${evidenceError?.message ?? evidenceError}"`);
  }
}
