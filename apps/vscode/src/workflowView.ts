export interface WorkflowPreference {
  version: 1;
  workflowMode: 'workflow' | 'all';
}
export function decodeWorkflowPreference(value: unknown): WorkflowPreference {
  const item = value as Partial<WorkflowPreference> | undefined;
  return { version: 1, workflowMode: item?.version === 1 && item.workflowMode === 'all' ? 'all' : 'workflow' };
}
export function mergeWorkflowPreference(state: unknown, preference: unknown): Record<string, unknown> {
  return { ...(state && typeof state === 'object' && !Array.isArray(state) ? state : {}),
    workflowView: decodeWorkflowPreference(preference) };
}
