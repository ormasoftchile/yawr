const TERMINAL_RUN_STATUSES = new Set(['completed', 'failed', 'cancelled', 'indeterminate', 'blocked', 'denied']);
const SETTLED_STEP_STATUSES = new Set(['completed', 'failed', 'skipped', 'denied', 'indeterminate', 'cancelled', 'blocked']);
const ISSUE_STEP_STATUSES = new Set(['failed', 'denied', 'indeterminate', 'cancelled', 'blocked']);

export function isTerminalRunStatus(status: string): boolean {
  return TERMINAL_RUN_STATUSES.has(status);
}

export function isSettledStepStatus(status: string): boolean {
  return SETTLED_STEP_STATUSES.has(status);
}

export function isIssueStepStatus(status: string): boolean {
  return ISSUE_STEP_STATUSES.has(status);
}