import { spawn } from 'node:child_process';
import { mkdir, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const mode = process.argv[2];
const supported = new Set(['unit', 'source', 'installed', 'package']);
if (!supported.has(mode)) {
  throw new Error(`expected lifecycle mode: ${[...supported].join(', ')}`);
}

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const repositoryRoot = resolve(root, '..', '..');
const disposableRoot = join(root, '.vscode-test', 'lifecycle');
const vsixPath = mode === 'package'
  ? join(root, 'yawr-preview.vsix')
  : join(disposableRoot, 'yawr-preview.vsix');
const trackedGeneratedFiles = [
  'media/graph.css',
  'media/graph.js',
  'media/highlighting-worker.js',
  'media/highlighting-NOTICES.txt',
];

async function snapshotFiles() {
  return new Map(await Promise.all(trackedGeneratedFiles.map(async (relativePath) => {
    const path = join(root, relativePath);
    try {
      return [path, await readFile(path)];
    } catch (error) {
      if (error?.code === 'ENOENT') return [path, undefined];
      throw error;
    }
  })));
}

async function restoreFiles(snapshot) {
  for (const [path, contents] of snapshot) {
    if (contents === undefined) await rm(path, { force: true });
    else await writeFile(path, contents);
  }
}

function run(command, args, extraEnvironment = {}) {
  return new Promise((resolveRun, rejectRun) => {
    const useShell = process.platform === 'win32' && ['npm', 'npx'].includes(command);
    const child = spawn(command, args, {
      cwd: root,
      env: { ...process.env, ...extraEnvironment },
      stdio: 'inherit',
      shell: useShell,
    });
    child.once('error', rejectRun);
    child.once('close', (code) => {
      if (code === 0) resolveRun();
      else rejectRun(new Error(`${command} exited with code ${code ?? 'unknown'}`));
    });
  });
}

async function packageVsix() {
  await mkdir(dirname(vsixPath), { recursive: true });
  await rm(vsixPath, { force: true });
  await run('npx', ['vsce', 'package', '--no-dependencies', '--out', vsixPath]);
  await run(process.execPath, ['scripts/normalize-vsix.mjs', vsixPath]);
  if ((await stat(vsixPath)).size === 0) throw new Error('packaged VSIX is empty');
}

const snapshot = await snapshotFiles();
try {
  await rm(disposableRoot, { recursive: true, force: true });
  if (mode === 'unit') {
    await run('npm', ['run', 'compile']);
    const tests = (await readdir(join(root, 'test')))
      .filter((name) => name.endsWith('.test.js'))
      .sort()
      .map((name) => join('test', name));
    await run(process.execPath, ['--test', '--test-timeout=5000', ...tests]);
  } else if (mode === 'source') {
    await run('npm', ['run', 'compile']);
    await run('npm', ['run', 'compile:test']);
    await run(process.execPath, ['scripts/run-vscode-test.mjs', 'source']);
  } else if (mode === 'installed') {
    await run('npm', ['run', 'compile']);
    await run('npm', ['run', 'compile:test']);
    await packageVsix();
    await run(process.execPath, ['scripts/run-vscode-test.mjs', 'production-surface'], {
      YAWR_VSIX_PATH: vsixPath,
    });
  } else {
    await packageVsix();
  }
} finally {
  await restoreFiles(snapshot);
  try {
    const repositoryPaths = trackedGeneratedFiles.map((path) => join('apps', 'vscode', path));
    await run('git', ['-C', repositoryRoot, 'add', '--refresh', '--', ...repositoryPaths]);
  } catch {}
  await rm(join(root, 'out'), { recursive: true, force: true });
  await rm(join(root, '.vscode-test'), { recursive: true, force: true });
  if (mode !== 'package') await rm(vsixPath, { force: true });
}
