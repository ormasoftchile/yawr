// panelRecovery.ts — workspace-state-based runbook recovery for the preview panel.
//
// Extracted so the path-resolution logic can be unit-tested without a VS Code host.

/** Workspace-state key that persists the last successfully opened runbook path. */
export const WORKSPACE_RUNBOOK_KEY = 'yawr.workspaceRunbook';

/**
 * Resolve the runbook path to use for a panel open or recovery.
 *
 * Resolution order:
 *  1. activeEditorPath — if it ends with .runbook.yaml, use it.
 *  2. savedRunbookPath — durable workspace state from a previous open.
 *  3. undefined — caller must surface an "open a runbook first" warning.
 */
export function resolveRunbookPath(
  activeEditorPath: string | undefined,
  savedRunbookPath: string | undefined,
): string | undefined {
  if (activeEditorPath && activeEditorPath.endsWith('.runbook.yaml')) {
    return activeEditorPath;
  }
  if (savedRunbookPath) {
    return savedRunbookPath;
  }
  return undefined;
}
