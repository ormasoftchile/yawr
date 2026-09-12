'use strict';

const assert = require('node:assert/strict');
const { once } = require('node:events');
const path = require('node:path');
const test = require('node:test');
const { pathToFileURL } = require('node:url');

const modulePromise = import(pathToFileURL(path.join(
  __dirname,
  '..',
  'scripts',
  'bounded-process.mjs',
)).href);
const quiet = { write() {} };

test('bounded process terminates on overall timeout', async () => {
  const { runBoundedProcess } = await modulePromise;
  await assert.rejects(
    runBoundedProcess(process.execPath, ['-e', 'setInterval(() => console.log("alive"), 10)'], {
      timeoutMs: 150,
      stallMs: 1000,
      stdout: quiet,
      stderr: quiet,
    }),
    (error) => error.kind === 'timeout',
  );
});

test('bounded process terminates after a no-output stall', async () => {
  const { runBoundedProcess } = await modulePromise;
  await assert.rejects(
    runBoundedProcess(process.execPath, ['-e', 'console.log("ready"); setInterval(() => {}, 1000)'], {
      timeoutMs: 2000,
      stallMs: 150,
      stdout: quiet,
      stderr: quiet,
    }),
    (error) => error.kind === 'stall',
  );
});

test('bounded process reports the failed phase and preserves empty output', async () => {
  const { runBoundedProcess } = await modulePromise;
  await assert.rejects(
    runBoundedProcess(process.execPath, ['-e', 'process.exit(7)'], {
      timeoutMs: 2000,
      stallMs: 1000,
      label: 'VSIX package',
      stdout: quiet,
      stderr: quiet,
    }),
    (error) => error.kind === 'exit'
      && error.label === 'VSIX package'
      && error.code === 7
      && error.output === '',
  );
});

test('retry helper enforces the single retry cap', async () => {
  const { runWithRetry } = await modulePromise;
  let attempts = 0;
  await assert.rejects(
    runWithRetry(async () => {
      attempts += 1;
      throw new Error('transient');
    }, { retries: 1, shouldRetry: () => true }),
    /transient/,
  );
  assert.equal(attempts, 2);
});

test('timeout terminates the spawned process tree', async () => {
  const { runBoundedProcess } = await modulePromise;
  let grandchildPid;
  const capture = {
    write(chunk) {
      const match = String(chunk).match(/grandchild=(\d+)/);
      if (match) grandchildPid = Number(match[1]);
    },
  };
  const script = [
    "const {spawn}=require('node:child_process')",
    "const child=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'})",
    "console.log('grandchild='+child.pid)",
    "setInterval(()=>console.log('alive'),25)",
  ].join(';');
  await assert.rejects(
    runBoundedProcess(process.execPath, ['-e', script], {
      timeoutMs: 300,
      stallMs: 1000,
      stdout: capture,
      stderr: quiet,
    }),
    (error) => error.kind === 'timeout',
  );
  assert.ok(grandchildPid, 'the child must report its descendant PID');
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.throws(() => process.kill(grandchildPid, 0), (error) => error.code === 'ESRCH');
});

test('cleanup always runs and cleanup failure fails validation', async () => {
  const { runWithCleanup } = await modulePromise;
  let cleaned = false;
  await assert.rejects(
    runWithCleanup(async () => 'ok', async () => {
      cleaned = true;
      throw new Error('cleanup failed');
    }),
    /cleanup failed/,
  );
  assert.equal(cleaned, true);

  await assert.rejects(
    runWithCleanup(async () => { throw new Error('operation failed'); }, async () => {
      throw new Error('cleanup failed');
    }),
    (error) => error instanceof AggregateError && error.errors.length === 2,
  );
});

test('failed validation evidence allowlists and redacts a diagnostic summary outside the repository', async () => {
  const { mkdir, readFile, readdir, rm, writeFile } = require('node:fs/promises');
  const testRoot = path.join(__dirname, '.validation-evidence-test');
  const repositoryRoot = path.join(testRoot, 'repository');
  const runRoot = path.join(repositoryRoot, 'run');
  const evidenceRoot = path.join(testRoot, 'evidence');
  await rm(testRoot, { recursive: true, force: true });
  await mkdir(runRoot, { recursive: true });
  const privatePath = path.join(require('node:os').homedir(), 'private', 'extension');
  await writeFile(path.join(runRoot, 'diagnostic-state.json'), JSON.stringify({
    label: 'production-surface',
    installedSource: 'vsix',
    installedExtensionDirectory: 'ormasoftchile.yawr-preview-1.2.3',
    installComplete: true,
    extensionFound: true,
    extensionActiveAfterActivation: false,
    activationError: `TypeError: failed at ${privatePath}\n    at activate (${privatePath}\\extension.js:1:1)`,
    previewGraphCommandPresent: true,
    previewGraphCommandExecuted: false,
    previewGraphCommandErrorCategory: 'CommandError',
    yawrCommands: ['yawr.previewGraph'],
    tabs: [{ label: privatePath, inputType: 'TabInputWebview', viewType: 'yawrPreviewGraph' }],
  }));
  await writeFile(path.join(runRoot, 'arbitrary.log'), `secret profile log ${privatePath}\n`);
  const evidenceModule = await import(pathToFileURL(path.join(
    __dirname,
    '..',
    'scripts',
    'validation-evidence.mjs',
  )).href);

  try {
    const evidence = await evidenceModule.preserveFailureEvidence(runRoot, {
      environment: { YAWR_FAILURE_EVIDENCE_ROOT: evidenceRoot },
      repositoryRoot,
      failure: {
        phase: 'installed VSIX validation',
        reason: 'exit-code-1',
        stdout: `stdout from ${privatePath}`,
        stderr: `Error: failed\n    at run (${privatePath}\\runner.js:1:1)`,
      },
    });
    assert.match(evidence.artifactId, /^yawr-validation-[0-9a-f-]+$/);
    assert.equal(path.dirname(evidence.destination), evidenceRoot);
    assert.deepEqual(await readdir(evidence.destination), ['diagnostic-summary.json']);
    const summaryText = await readFile(path.join(evidence.destination, 'diagnostic-summary.json'), 'utf8');
    const summary = JSON.parse(summaryText);
    assert.equal(summary.installedSource.extensionDirectory, 'ormasoftchile.yawr-preview-1.2.3');
    assert.equal(summary.activation.errorCategory, 'TypeError');
    assert.equal(summary.command.previewGraphPresent, true);
    assert.equal(summary.command.previewGraphExecuted, false);
    assert.equal(summary.tabs[0].viewType, 'yawrPreviewGraph');
    assert.equal(summary.failure.phase, 'installed VSIX validation');
    assert.match(summary.failure.stdoutExcerpt, /<HOME>|<ABSOLUTE_PATH>/);
    assert.doesNotMatch(summaryText, /private|extension\.js|runner\.js|arbitrary\.log|secret profile log/);
    assert.equal((await readdir(runRoot)).includes('arbitrary.log'), true);
  } finally {
    await rm(testRoot, { recursive: true, force: true });
  }
});

test('diagnostic summaries redact adversarial absolute paths without hiding safe relative identifiers', async () => {
  const evidenceModule = await import(pathToFileURL(path.join(
    __dirname,
    '..',
    'scripts',
    'validation-evidence.mjs',
  )).href);
  const adversarialPaths = [
    '/private/file.js:1:2',
    '/mnt/c/private/file.js:3:4',
    String.raw`C:\Users\private\file.js:5:6`,
    String.raw`\\server\share\private.js:7:8`,
    String.raw`D:/private\mixed\file.js:9:10`,
    '"/opt/private folder/quoted.js:11:12"',
    String.raw`'C:\private folder\quoted.js:13:14'`,
  ];
  const summary = evidenceModule.createDiagnosticSummary({
    label: 'TypeError: production surface',
    installedExtensionDirectory: 'ormasoftchile.yawr-preview-1.2.3',
    activationErrorCategory: 'TypeError',
    yawrCommands: ['yawr.previewGraph', 'src/commands/preview.js:1:2'],
    tabs: [{ label: `safe relative/path.js and ${adversarialPaths.join(' then ')}` }],
  }, {
    phase: 'installed VSIX validation',
    reason: 'CommandError',
    stdout: `ordinary message; safe src/runtime/file.js:2:3; ${adversarialPaths.join(' | ')}`,
    stderr: `Error: ${adversarialPaths.join(', ')}\n    at run (${adversarialPaths[0]})`,
  });
  const summaryText = JSON.stringify(summary);

  assert.equal(summary.activation.errorCategory, 'TypeError');
  assert.equal(summary.failure.reason, 'CommandError');
  assert.equal(summary.command.registeredYawrCommands[1], 'src/commands/preview.js:1:2');
  assert.match(summary.failure.stdoutExcerpt, /ordinary message/);
  assert.match(summary.failure.stdoutExcerpt, /src\/runtime\/file\.js:2:3/);
  assert.match(summaryText, /<ABSOLUTE_PATH>/);
  for (const absolutePath of adversarialPaths) {
    const unquotedPath = absolutePath.replace(/^["']|["']$/g, '');
    assert.equal(summaryText.includes(unquotedPath), false, `must redact ${absolutePath}`);
  }
  assert.doesNotMatch(summary.failure.stderrExcerpt, /^\s*at\s/m);
});

test('failure evidence rejects arbitrary files and repository destinations without debris', async () => {
  const { mkdir, readdir, rm, writeFile } = require('node:fs/promises');
  const testRoot = path.join(__dirname, '.validation-evidence-rejection-test');
  const repositoryRoot = path.join(testRoot, 'repository');
  const runRoot = path.join(repositoryRoot, 'run');
  await rm(testRoot, { recursive: true, force: true });
  await mkdir(runRoot, { recursive: true });
  await writeFile(path.join(runRoot, 'diagnostic-state.json'), '{}\n');
  const evidenceModule = await import(pathToFileURL(path.join(
    __dirname,
    '..',
    'scripts',
    'validation-evidence.mjs',
  )).href);

  try {
    await assert.rejects(
      evidenceModule.preserveFailureEvidence(runRoot, {
        environment: { YAWR_FAILURE_EVIDENCE_ROOT: path.join(testRoot, 'evidence') },
        repositoryRoot,
        sourceFiles: ['profile/logs/window.log'],
      }),
      /not allowlisted/,
    );
    await assert.rejects(
      evidenceModule.preserveFailureEvidence(runRoot, {
        environment: { YAWR_FAILURE_EVIDENCE_ROOT: path.join(repositoryRoot, 'evidence') },
        repositoryRoot,
      }),
      /outside the repository/,
    );
    assert.deepEqual(await readdir(repositoryRoot), ['run']);
  } finally {
    await rm(testRoot, { recursive: true, force: true });
  }
});
