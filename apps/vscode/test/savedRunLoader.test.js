'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const {
  savedRunPreviewArgs,
} = require('../out/directGraphPreview');

const {
  discoverSavedRuns,
  resolveRunIdentityFromPath,
  loadSavedRunState,
} = require('../out/savedRunLoader');

const repoRoot = path.resolve(__dirname, '..', '..', '..');
const codePresentationRunsDir = path.join(
  repoRoot,
  'runtime',
  'examples',
  'code-presentation',
  '.runbook',
  'runs',
);
const sampleRunID = '188052b4-b7b3-480a-bbbb-a390ec3ae450';
const sampleRunDir = path.join(codePresentationRunsDir, sampleRunID);

test('savedRunPreviewArgs formats CLI flags correctly', () => {
  const args = savedRunPreviewArgs('/path/to/runs', 'run-abc-123');
  assert.deepEqual(args, [
    'preview',
    '--format',
    'graphjson',
    '--run-dir',
    '/path/to/runs',
    '--run-id',
    'run-abc-123',
  ]);
});

test('resolveRunIdentityFromPath resolves from run directory', () => {
  if (!fs.existsSync(sampleRunDir)) return;
  const identity = resolveRunIdentityFromPath(sampleRunDir);
  assert.ok(identity, 'Identity should be resolved from run directory');
  assert.equal(identity.runID, sampleRunID);
  assert.equal(identity.runDir, codePresentationRunsDir);
});

test('resolveRunIdentityFromPath resolves from trace.jsonl', () => {
  const traceFile = path.join(sampleRunDir, 'trace.jsonl');
  if (!fs.existsSync(traceFile)) return;
  const identity = resolveRunIdentityFromPath(traceFile);
  assert.ok(identity, 'Identity should be resolved from trace.jsonl');
  assert.equal(identity.runID, sampleRunID);
  assert.equal(identity.runDir, codePresentationRunsDir);
});

test('resolveRunIdentityFromPath resolves from checkpoint snapshot file', () => {
  const snapDir = path.join(sampleRunDir, 'snapshots');
  if (!fs.existsSync(snapDir)) return;
  const files = fs.readdirSync(snapDir).filter(f => f.startsWith('checkpoint-'));
  if (files.length === 0) return;
  const checkpointFile = path.join(snapDir, files[0]);

  const identity = resolveRunIdentityFromPath(checkpointFile);
  assert.ok(identity, 'Identity should be resolved from checkpoint file');
  assert.equal(identity.runID, sampleRunID);
  assert.equal(identity.runDir, codePresentationRunsDir);
});

test('resolveRunIdentityFromPath resolves from parent runs directory', () => {
  if (!fs.existsSync(codePresentationRunsDir)) return;
  const identity = resolveRunIdentityFromPath(codePresentationRunsDir);
  assert.ok(identity, 'Identity should be resolved from parent runs directory');
  assert.ok(identity.runID);
  assert.equal(identity.runDir, codePresentationRunsDir);
});

test('resolveRunIdentityFromPath returns undefined for non-existent or invalid paths', () => {
  assert.equal(resolveRunIdentityFromPath('/non/existent/path/here'), undefined);
  assert.equal(resolveRunIdentityFromPath(''), undefined);
  assert.equal(resolveRunIdentityFromPath(__dirname), undefined);
});

test('discoverSavedRuns discovers runs and extracts metadata', async () => {
  if (!fs.existsSync(codePresentationRunsDir)) return;
  const runs = await discoverSavedRuns([codePresentationRunsDir, '/non/existent/dir']);
  assert.ok(runs.length >= 1, 'Should find at least 1 run');
  const target = runs.find(r => r.runID === sampleRunID);
  assert.ok(target, `Should find run ${sampleRunID}`);
  assert.equal(target.status, 'completed');
  assert.equal(target.hasTrace, true);
  assert.equal(target.hasSnapshots, true);
  assert.equal(target.stepCount, 6);
  assert.ok(target.startedAt, 'startedAt should be populated');
  assert.ok(target.completedAt, 'completedAt should be populated');
});

test('loadSavedRunState loads state with mock executor', async () => {
  if (!fs.existsSync(sampleRunDir)) return;
  const mockDocument = {
    schema_version: '3',
    hash: 'sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef',
    nodes: [
      { id: 'sql', position: { x: 0, y: 0 }, data: { id: 'sql', kind: 'step' } },
      { id: 'kql', position: { x: 0, y: 0 }, data: { id: 'kql', kind: 'step' } },
    ],
    edges: [],
    frames: [],
    groups: [],
    runbook: { id: 'root', name: 'root' },
    presentation_state: {
      run_id: sampleRunID,
      plan_snapshot_digest: 'sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef',
      checkpoint_sequence: 10,
      occurrences: [],
    },
  };

  const loaded = await loadSavedRunState(
    'yawr',
    codePresentationRunsDir,
    sampleRunID,
    async (binary, args) => {
      assert.deepEqual(args, savedRunPreviewArgs(codePresentationRunsDir, sampleRunID));
      return { stdout: JSON.stringify(mockDocument) };
    },
  );

  assert.equal(loaded.runID, sampleRunID);
  assert.equal(loaded.status, 'completed');
  assert.equal(loaded.error, undefined);
  assert.ok(loaded.events.length > 0, 'Should load trace events');
  assert.equal(loaded.steps.length, 6, 'Should reconstruct 6 terminal steps');

  // Check individual step attributes
  const sqlStep = loaded.steps.find(s => s.node_id === 'sql');
  assert.ok(sqlStep, 'sql step should exist');
  assert.equal(sqlStep.status, 'completed');
  assert.equal(sqlStep.duration_ms, 98);
  assert.deepEqual(sqlStep.output, { code: 'SELECT name\nFROM synthetic_table\nWHERE enabled = 1;' });

  const kqlStep = loaded.steps.find(s => s.node_id === 'kql');
  assert.ok(kqlStep, 'kql step should exist');
  assert.equal(kqlStep.status, 'completed');
  assert.equal(kqlStep.duration_ms, 107);
});

test('loadSavedRunState works when runDir points directly to run folder', async () => {
  if (!fs.existsSync(sampleRunDir)) return;
  const mockDocument = {
    schema_version: '3',
    hash: 'sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef',
    nodes: [],
    edges: [],
    frames: [],
    groups: [],
    runbook: { id: 'root', name: 'root' },
  };

  const loaded = await loadSavedRunState(
    'yawr',
    sampleRunDir, // Passing full run folder instead of runs parent
    sampleRunID,
    async (binary, args) => {
      // Must have normalized to codePresentationRunsDir
      assert.deepEqual(args, savedRunPreviewArgs(codePresentationRunsDir, sampleRunID));
      return { stdout: JSON.stringify(mockDocument) };
    },
  );

  assert.equal(loaded.runID, sampleRunID);
  assert.equal(loaded.status, 'completed');
  assert.equal(loaded.steps.length, 6);
});

test('loadSavedRunState works end-to-end with real yawr binary when available', async () => {
  const binaryPath = path.join(repoRoot, 'runtime', 'yawr');
  if (!fs.existsSync(binaryPath) || !fs.existsSync(sampleRunDir)) return;

  const { execFile } = require('node:child_process');
  const { promisify } = require('node:util');
  const pexec = promisify(execFile);

  const loaded = await loadSavedRunState(
    binaryPath,
    codePresentationRunsDir,
    sampleRunID,
    (bin, args) => pexec(bin, args, { cwd: repoRoot, maxBuffer: 32 * 1024 * 1024 }),
  );

  assert.equal(loaded.runID, sampleRunID);
  assert.equal(loaded.status, 'completed');
  assert.ok(loaded.document, 'Document should be loaded from yawr preview');
  assert.ok(loaded.document.nodes.length > 0, 'Document should have nodes');
  assert.ok(loaded.document.presentation_state, 'Document should have presentation_state');
  assert.equal(loaded.steps.length, 6);
  assert.ok(loaded.events.length > 0);
});
