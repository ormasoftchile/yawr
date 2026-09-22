'use strict';

// panelRecovery.test.js
//
// Focused unit tests for workspace-state-based panel recovery after an
// Extension Host restart.
//
// Scenario: the Extension Host restarts, clearing all module-level state
// (graphPanel, graphPanelRunbookPath). yawr.restartServer / yawr.previewGraph
// must still open a panel deterministically by resolving the runbook from
// durable workspace state — no active .runbook.yaml editor required.
//
// Tests:
//  1. resolveRunbookPath — active editor takes precedence over saved path
//  2. resolveRunbookPath — falls back to workspace state when no qualifying editor
//  3. resolveRunbookPath — returns undefined when neither is available
//  4. WORKSPACE_RUNBOOK_KEY is the stable constant "yawr.workspaceRunbook"
//  5. Compiled extension.js contains the workspace-state recovery path
//     (key lookup, workspaceState.get, workspaceState.update, resolveRunbookPath)

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const { resolveRunbookPath, WORKSPACE_RUNBOOK_KEY } = require('../out/panelRecovery');

// ─── 1: active editor wins ───────────────────────────────────────────────────

test('resolveRunbookPath: active .runbook.yaml editor takes precedence over saved workspace path', () => {
  const active = 'C:\\work\\my-incident.runbook.yaml';
  const saved  = 'C:\\work\\other.runbook.yaml';
  assert.strictEqual(
    resolveRunbookPath(active, saved),
    active,
    'active editor path must win over saved workspace path',
  );
});

test('resolveRunbookPath: active .yawr editor takes precedence over saved workspace path', () => {
  const active = 'C:\\work\\my-incident.yawr';
  const saved  = 'C:\\work\\other.runbook.yaml';
  assert.strictEqual(
    resolveRunbookPath(active, saved),
    active,
    'active .yawr editor path must win over saved workspace path',
  );
});

// ─── 2: workspace state fallback ─────────────────────────────────────────────

test('resolveRunbookPath: falls back to saved path when no qualifying editor is focused (post-restart recovery)', () => {
  const saved = 'C:\\work\\release-readiness.runbook.yaml';

  assert.strictEqual(
    resolveRunbookPath(undefined, saved),
    saved,
    'must return saved path when activeEditorPath is undefined',
  );

  assert.strictEqual(
    resolveRunbookPath('C:\\work\\README.md', saved),
    saved,
    'must return saved path when active file is not a .runbook.yaml',
  );

  assert.strictEqual(
    resolveRunbookPath('C:\\work\\notes.ts', saved),
    saved,
    'must return saved path when active file is a TypeScript file',
  );
});

// ─── 3: neither available ────────────────────────────────────────────────────

test('resolveRunbookPath: returns undefined when neither active editor nor saved path is available', () => {
  assert.strictEqual(
    resolveRunbookPath(undefined, undefined),
    undefined,
    'must return undefined when no runbook can be resolved',
  );
  assert.strictEqual(
    resolveRunbookPath('C:\\work\\notes.txt', undefined),
    undefined,
    'must return undefined when active file is not a runbook and no saved path',
  );
  assert.strictEqual(
    resolveRunbookPath('C:\\work\\runbook.yaml', undefined),
    undefined,
    'must return undefined for .yaml files that lack the .runbook.yaml suffix',
  );
});

// ─── 4: WORKSPACE_RUNBOOK_KEY stability ──────────────────────────────────────

test('WORKSPACE_RUNBOOK_KEY uses the canonical Yawr namespace', () => {
  assert.strictEqual(
    WORKSPACE_RUNBOOK_KEY,
    'yawr.workspaceRunbook',
    'WORKSPACE_RUNBOOK_KEY must use the canonical Yawr namespace.',
  );
});

// ─── 5: compiled extension.js contains workspace-state recovery ───────────────

test('compiled extension.js resolves the preview runbook from workspace state when no editor is active', () => {
  const extensionSrc = fs.readFileSync(
    path.join(__dirname, '..', 'out', 'extension.js'),
    'utf8',
  );
  const panelRecoverySrc = fs.readFileSync(
    path.join(__dirname, '..', 'out', 'panelRecovery.js'),
    'utf8',
  );

  // The string key must be defined in the panelRecovery module.
  assert.ok(
    panelRecoverySrc.includes(WORKSPACE_RUNBOOK_KEY),
    `panelRecovery.js must contain the workspace state key "${WORKSPACE_RUNBOOK_KEY}". ` +
    'This key must never be changed — changing it silently drops persisted runbook paths for existing users.',
  );

  // The extension module must reference the key constant by name (via require/import).
  assert.ok(
    extensionSrc.includes('WORKSPACE_RUNBOOK_KEY'),
    'extension.js must reference the WORKSPACE_RUNBOOK_KEY constant from panelRecovery. ' +
    'Without this the restart command cannot look up the persisted runbook path.',
  );

  assert.ok(
    extensionSrc.includes('workspaceState') &&
    extensionSrc.includes('.get('),
    'extension.js must call workspaceState.get() to read the persisted runbook path. ' +
    'The extension must consult durable state, not just ephemeral module-level vars.',
  );

  assert.ok(
    extensionSrc.includes('workspaceState') &&
    extensionSrc.includes('.update('),
    'extension.js must call workspaceState.update() to persist the runbook path when a panel is opened. ' +
    'This is what makes subsequent restarts recovery-capable.',
  );

  assert.ok(
    extensionSrc.includes('resolveRunbookPath'),
    'extension.js must call resolveRunbookPath() — the deterministic resolution helper. ' +
    'Direct editor-only checks bypass the workspace state fallback.',
  );
});
