// Release-only containment. Paused decoration cannot authorize content or rendering.
// Undecorated output and independent code-presentation protections remain unchanged.
export const DISPLAY_MAX_UTF16 = 32768;
export interface DisplayPresentationV1 {
  version: 1;
  format: 'markdown';
  output_field: 'content';
  origin: 'frozen';
  plan_snapshot_digest: string;
  value_status: 'available' | 'truncated' | 'redacted' | 'unavailable';
}
export interface DisplaySelection {
  status: DisplayPresentationV1['value_status'];
  text?: string;
  reason: string;
}
export function decodeDisplayPresentation(_value: unknown, _snapshotDigest?: string): DisplayPresentationV1 | undefined {
  return undefined;
}
export function selectDisplayPresentation(_metadata: unknown, _output: unknown, _snapshotDigest?: string, _diagnostic?: string): DisplaySelection {
  return { status: 'unavailable', reason: 'Display decoration is paused in this release' };
}
export function hasDisplayPayload(value: Record<string, unknown>): boolean {
  return 'display_presentation' in value || 'display_presentation_diagnostic' in value;
}
export function sanitizeDisplayPayload(payload: Record<string, unknown>, _snapshotDigest?: string): void {
  if (!hasDisplayPayload(payload)) return;
  if (payload.output && typeof payload.output === 'object' && !Array.isArray(payload.output)) {
    const { content: _, ...output } = payload.output as Record<string, unknown>;
    payload.output = output;
  }
  delete payload.display_presentation;
  payload.display_presentation_diagnostic = 'feature-paused';
}
export function mergeDisplayPayload(previous: Record<string, unknown>, incoming: Record<string, unknown>,
  snapshotDigest?: string): Record<string, unknown> {
  const result = { ...previous, ...incoming };
  // A paused/withheld observation cannot regain its content through a late plain preview.
  if (hasDisplayPayload(previous) || hasDisplayPayload(incoming)) result.display_presentation_diagnostic = 'feature-paused';
  sanitizeDisplayPayload(result, snapshotDigest);
  return result;
}
