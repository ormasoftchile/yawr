const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const { pickProjectRoot } = require('../out/projectRoot');

test('selects the active runbook project over a broad workspace', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-project-root-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));

  const project = path.join(root, 'sql-livesite');
  const runbooks = path.join(project, 'runbooks');
  fs.mkdirSync(path.join(project, 'packages'), { recursive: true });
  fs.mkdirSync(runbooks, { recursive: true });
  const runbook = path.join(runbooks, 'proof.runbook.yaml');
  fs.writeFileSync(runbook, 'apiVersion: yawr.runbook/v1\n');

  assert.equal(pickProjectRoot(runbook, [root], root), project);
});

test('falls back to a matching workspace project when the runbook is external', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-project-root-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));

  const project = path.join(root, 'project');
  fs.mkdirSync(path.join(project, 'tools'), { recursive: true });
  fs.mkdirSync(path.join(project, 'runbooks'), { recursive: true });
  const externalRunbook = path.join(root, 'external.runbook.yaml');
  fs.writeFileSync(externalRunbook, 'apiVersion: yawr.runbook/v1\n');

  assert.equal(pickProjectRoot(externalRunbook, [project], root), project);
});

test('recognizes a .yawr and runbooks project without tools or packages', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'yawr-project-root-marker-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const project = path.join(root, 'project');
  const runbooks = path.join(project, 'runbooks');
  fs.mkdirSync(path.join(project, '.yawr'), { recursive: true });
  fs.mkdirSync(runbooks, { recursive: true });
  const runbook = path.join(runbooks, 'incident.runbook.yaml');
  fs.writeFileSync(runbook, 'apiVersion: yawr.runbook/v1\n');

  assert.equal(pickProjectRoot(runbook, [project], root), project);
});
