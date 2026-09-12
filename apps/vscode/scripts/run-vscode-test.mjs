import { spawn } from 'node:child_process';
import { mkdir, rm } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';

const label = process.argv[2];
if (!label || !['source', 'production-surface'].includes(label)) {
  throw new Error('expected VS Code test label: source or production-surface');
}

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const runID = `${Date.now().toString(36)}-${process.pid}-${Math.random().toString(16).slice(2)}`;
const runRoot = join(root, '.vscode-test', 'runs', runID);
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

const run = (command, args, shell = false) => new Promise((resolve, reject) => {
  const child = spawn(command, args, { cwd: root, env: environment, stdio: 'inherit', shell });
  child.once('error', reject);
  child.once('close', (code) => code === 0 ? resolve() : reject(new Error(`${command} exited with code ${code ?? 'unknown'}`)));
});

try {
  await mkdir(join(runRoot, 'workspace'), { recursive: true });
  if (label === 'production-surface') {
    const vscodeExecutable = await downloadAndUnzipVSCode({
      version: '1.137.0',
      cachePath: join(runRoot, 'cache'),
    });
    const cli = resolveCliPathFromVSCodeExecutablePath(vscodeExecutable);
    await run(cli, [
      `--user-data-dir=${join(runRoot, 'profile')}`,
      `--extensions-dir=${join(runRoot, 'extensions')}`,
      '--install-extension',
      process.env.YAWR_VSIX_PATH || join(root, 'yawr-preview.vsix'),
    ], process.platform === 'win32');
  }
  await run(process.execPath, [executable, '--label', label]);
} catch (error) {
  process.exitCode = 1;
  console.error(error);
} finally {
  await rm(runRoot, {
    recursive: true,
    force: true,
    maxRetries: 20,
    retryDelay: 500,
  });
  await rm(join(root, '.vscode-test'), {
    recursive: true,
    force: true,
    maxRetries: 20,
    retryDelay: 500,
  });
}
