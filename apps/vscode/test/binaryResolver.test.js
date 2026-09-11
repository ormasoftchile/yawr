'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const { resolveBinary } = require('../out/binaryResolver');

function createExecutable(directory, command = 'yawr') {
  const filename = process.platform === 'win32' ? `${command}.exe` : command;
  const executable = path.join(directory, filename);
  fs.mkdirSync(path.dirname(executable), { recursive: true });
  fs.writeFileSync(executable, '');
  fs.chmodSync(executable, 0o755);
  return executable;
}

const output = { appendLine() {} };

test('an absent explicit absolute path does not fall back to a discovered yawr binary', async (t) => {
  const projectRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-binary-resolver-'));
  t.after(() => fs.rmSync(projectRoot, { recursive: true, force: true }));
  createExecutable(projectRoot);

  const configured = path.join(projectRoot, 'missing', 'configured-yawr');

  await assert.rejects(resolveBinary(configured, output, projectRoot, []));
});

test('an absent explicit command does not fall back to a discovered yawr binary', async (t) => {
  const projectRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-binary-resolver-'));
  t.after(() => fs.rmSync(projectRoot, { recursive: true, force: true }));
  createExecutable(projectRoot);

  await assert.rejects(resolveBinary('configured-yawr', output, projectRoot, []));
});

test('the canonical yawr command prefers a yawr binary from the active project', async (t) => {
  const projectRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-binary-resolver-'));
  t.after(() => fs.rmSync(projectRoot, { recursive: true, force: true }));
  const discovered = createExecutable(projectRoot, 'yawr');

  assert.equal(await resolveBinary('yawr', output, projectRoot, []), discovered);
});
