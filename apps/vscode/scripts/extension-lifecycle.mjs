import { mkdir, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { BoundedProcessError, runBoundedProcess } from './bounded-process.mjs';

const mode = process.argv[2];
const supported = new Set(['unit', 'source', 'installed', 'package', 'package-check', 'validate-vsix']);
if (!supported.has(mode)) {
  throw new Error(`expected lifecycle mode: ${[...supported].join(', ')}`);
}

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const repositoryRoot = resolve(root, '..', '..');
const require = createRequire(import.meta.url);
const vsceCli = require.resolve('@vscode/vsce/vsce');
const disposableRoot = join(root, '.vscode-test', 'lifecycle');
const vsixPath = mode === 'package'
  ? join(root, 'yawr-preview.vsix')
  : join(disposableRoot, 'yawr-preview.vsix');
const validationDeadline = mode === 'validate-vsix' ? Date.now() + 600_000 : undefined;
const cleanupReserveMs = 30_000;
const stallMs = 120_000;
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

function boundedTimeout(requestedMs, reserveCleanup = true) {
  if (!validationDeadline) return requestedMs;
  const remaining = validationDeadline - Date.now() - (reserveCleanup ? cleanupReserveMs : 0);
  if (remaining <= 0) throw new Error('extension validation exhausted its 10 minute overall limit');
  return Math.min(requestedMs, remaining);
}

function run(command, args, extraEnvironment = {}, timeoutMs = 240_000, label = command, reserveCleanup = true) {
  return runBoundedProcess(command, args, {
    cwd: root,
    env: { ...process.env, ...extraEnvironment },
    timeoutMs: boundedTimeout(timeoutMs, reserveCleanup),
    stallMs,
    label,
  }).catch((error) => {
    if (error instanceof BoundedProcessError) {
      error.phase = label;
      error.reason = error.kind === 'exit' ? `exit-code-${error.code ?? 'unknown'}` : error.kind;
    }
    throw error;
  });
}

async function packageVsix() {
  await mkdir(dirname(vsixPath), { recursive: true });
  await rm(vsixPath, { force: true });
  const deadline = Date.now() + boundedTimeout(180_000);
  await run(process.execPath, [vsceCli, 'package', '--no-dependencies', '--out', vsixPath], {},
    Math.max(1, deadline - Date.now()), 'VSIX package');
  await run(process.execPath, ['scripts/normalize-vsix.mjs', vsixPath], {},
    Math.max(1, deadline - Date.now()), 'VSIX normalization');
  if ((await stat(vsixPath)).size === 0) throw new Error('packaged VSIX is empty');
}

const snapshot = await snapshotFiles();
let failure;
try {
  await rm(disposableRoot, { recursive: true, force: true });
  if (mode === 'unit') {
    await run('npm', ['run', 'compile'], {}, 120_000, 'extension compile');
    const tests = (await readdir(join(root, 'test')))
      .filter((name) => name.endsWith('.test.js'))
      .sort()
      .map((name) => join('test', name));
    await run(process.execPath, ['--test', '--test-timeout=5000', ...tests], {}, 240_000, 'extension unit tests');
  } else if (mode === 'source') {
    await run('npm', ['run', 'compile'], {}, 120_000, 'extension compile');
    await run('npm', ['run', 'compile:test'], {}, 120_000, 'Extension Host test compile');
    await run(process.execPath, ['scripts/run-vscode-test.mjs', 'source']);
  } else if (mode === 'installed') {
    await run('npm', ['run', 'compile'], {}, 120_000, 'extension compile');
    await run('npm', ['run', 'compile:test'], {}, 120_000, 'Extension Host test compile');
    await packageVsix();
    await run(process.execPath, ['scripts/run-vscode-test.mjs', 'production-surface'], {
      YAWR_VSIX_PATH: vsixPath,
    });
  } else if (mode === 'validate-vsix') {
    const compileDeadline = Date.now() + boundedTimeout(120_000);
    await run('npm', ['run', 'compile'], {}, Math.max(1, compileDeadline - Date.now()), 'extension compile');
    await run('npm', ['run', 'compile:test'], {}, Math.max(1, compileDeadline - Date.now()), 'Extension Host test compile');
    await packageVsix();
    await run(process.execPath, ['scripts/run-vscode-test.mjs', 'production-surface'], {
      YAWR_VSIX_PATH: vsixPath,
    }, 570_000, 'installed VSIX validation');
  } else {
    await packageVsix();
  }
} catch (error) {
  failure = error;
} finally {
  try {
    const cleanup = async () => {
      await restoreFiles(snapshot);
      const repositoryPaths = trackedGeneratedFiles.map((path) => join('apps', 'vscode', path));
      await run('git', ['-C', repositoryRoot, 'add', '--refresh', '--', ...repositoryPaths], {},
        10_000, 'generated-file index refresh', false);
      await rm(join(root, 'out'), { recursive: true, force: true, maxRetries: 10, retryDelay: 250 });
      await rm(join(root, '.vscode-test'), { recursive: true, force: true, maxRetries: 10, retryDelay: 250 });
      if (mode !== 'package') await rm(vsixPath, { force: true });
    };
    await Promise.race([
      cleanup(),
      new Promise((_, reject) => {
        const timer = setTimeout(() => reject(new Error('extension validation cleanup exceeded 30 seconds')), 30_000);
        timer.unref?.();
      }),
    ]);
  } catch (cleanupError) {
    failure = failure
      ? new AggregateError([failure, cleanupError], 'validation and cleanup failed')
      : cleanupError;
  }
}

if (failure) {
  if (mode === 'validate-vsix') {
    const phase = failure.phase ?? 'lifecycle';
    const reason = failure.reason ?? failure.code ?? failure.name ?? 'unknown';
    const output = failure.output?.trim() || '(no output captured)';
    console.error(`extension:validate:vsix FAIL phase="${phase}" reason="${reason}"`);
    console.error(`extension:validate:vsix OUTPUT\n${output}`);
  }
  throw failure;
}
if (mode === 'validate-vsix') console.log('extension:validate:vsix PASS installed-vsix');
