// panelRecovery.ts — workspace-state-based runbook recovery for the preview panel.
//
// Extracted so the path-resolution logic can be unit-tested without a VS Code host.

/** Workspace-state key that persists the last successfully opened runbook path. */
export const WORKSPACE_RUNBOOK_KEY = 'yawr.workspaceRunbook';

/** Check whether a file path has a runbook extension (.runbook.yaml, .runbook.yml, or .yawr). */
export function isRunbookPath(filePath: string | undefined): boolean {
  return typeof filePath === 'string' && /\.(?:runbook\.ya?ml|yawr)$/i.test(filePath);
}

/**
 * Resolve the runbook path to use for a panel open or recovery.
 *
 * Resolution order:
 *  1. activeEditorPath — if it is a runbook file (.runbook.yaml, .runbook.yml, .yawr), use it.
 *  2. savedRunbookPath — durable workspace state from a previous open.
 *  3. undefined — caller must surface an "open a runbook first" warning.
 */
export function resolveRunbookPath(
  activeEditorPath: string | undefined,
  savedRunbookPath: string | undefined,
): string | undefined {
  if (isRunbookPath(activeEditorPath)) {
    return activeEditorPath;
  }
  if (savedRunbookPath) {
    return savedRunbookPath;
  }
  return undefined;
}
