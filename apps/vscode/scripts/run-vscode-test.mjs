import { copyFile, mkdir, readFile, readdir, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { createServer } from 'node:net';
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
const packagedFixture = join(root, 'fixtures', 'file-only-subprocess', 'win32-x64', 'fixture.exe');
const hashFile = async (path) => createHash('sha256').update(await readFile(path)).digest('hex');
const expectedExtensionVersion = JSON.parse(await readFile(join(root, 'package.json'), 'utf8')).version;
const require = createRequire(import.meta.url);
const { downloadAndUnzipVSCode, resolveCliPathFromVSCodeExecutablePath } = require('@vscode/test-electron');
const JSZip = require('jszip');
const vsixPath = process.env.YAWR_VSIX_PATH || join(root, 'yawr-preview.vsix');
let archiveIdentities = {};
if (label === 'production-surface' && process.platform === 'win32' && process.arch === 'x64') {
  console.log(`Installed VSIX SHA256: ${await hashFile(vsixPath)}`);
  const archive = await JSZip.loadAsync(await readFile(vsixPath));
  const helperEntry = archive.file('extension/bin/win32-x64/yawr.exe');
  const fixtureEntry = archive.file('extension/fixtures/file-only-subprocess/win32-x64/fixture.exe');
  if (!helperEntry || !fixtureEntry) throw new Error('final VSIX is missing the packaged helper or file-only fixture');
  const hashBytes = (bytes) => createHash('sha256').update(bytes).digest('hex');
  const helperSHA256 = hashBytes(await helperEntry.async('nodebuffer'));
  const fixtureSHA256 = hashBytes(await fixtureEntry.async('nodebuffer'));
  const standaloneSHA256 = await hashFile(packagedHelper);
  if (helperSHA256 !== standaloneSHA256) {
    throw new Error(`final VSIX helper ${helperSHA256} differs from standalone runtime ${standaloneSHA256}`);
  }
  archiveIdentities = {
    YAWR_EXPECTED_HELPER_SHA256: helperSHA256,
    YAWR_EXPECTED_FIXTURE_SHA256: fixtureSHA256,
    YAWR_EXPECTED_STANDALONE_SHA256: standaloneSHA256,
  };
}
const environment = {
  ...process.env,
  PATH: `${portableGoBin}${process.platform === 'win32' ? ';' : ':'}${process.env.PATH ?? ''}`,
  YAWR_TEST_RUN_ID: runID,
  YAWR_TEST_STATE_ROOT: runRoot,
  YAWR_E2E_BINARY: packagedHelper,
  YAWR_EXPRESSION_HELPER: packagedHelper,
  YAWR_EXPECTED_EXTENSION_VERSION: expectedExtensionVersion,
  ...archiveIdentities,
};
if (label === 'production-surface') {
  const server = createServer();
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  environment.YAWR_TEST_CDP_PORT = String(server.address().port);
  await new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
}

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
  if (label === 'production-surface') {
    await mkdir(join(runRoot, 'workspace', 'test', 'fixtures'), { recursive: true });
    await copyFile(
      join(root, 'test', 'fixtures', 'enum.runbook.yaml'),
      join(runRoot, 'workspace', 'test', 'fixtures', 'enum.runbook.yaml'),
    );
  }
  await writeDiagnosticState({ label });
  if (label === 'download') {
    const vscodeExecutable = await downloadAndUnzipVSCode({
      version: '1.136.2',
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
        vsixPath,
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
    await runBoundedProcess(process.execPath, [
      executable,
      '--label',
      label,
      '--fail-zero',
      '--forbid-only',
      '--forbid-pending',
    ], {
      cwd: root,
      env: environment,
      timeoutMs: 240_000,
      stallMs,
      label: `${label} Extension Host`,
    });
    if (label === 'production-surface' && process.platform === 'win32' && process.arch === 'x64') {
      const state = JSON.parse(await readFile(diagnosticStateFile, 'utf8'));
      if (state.fileOnlyQualificationExecuted !== true) {
        throw new Error('installed VSIX file-only qualification was skipped');
      }
      if (state.installedPackageSHA256Equality !== true) {
        throw new Error('installed VSIX package SHA-256 equality was not proved');
      }
    }
    if (label === 'production-surface') {
      await writeDiagnosticState({ installedTestSkipCount: 0 });
      console.log('extension:validate:vsix installed-tests skips=0');
    }
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
