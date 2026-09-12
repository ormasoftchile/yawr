export type PreviewPanelTarget = 'active' | 'beside' | number;

export function resolvePreviewPanelTarget(
  openLocation: string | undefined,
  runbookViewColumn: number | undefined,
): PreviewPanelTarget {
  if (openLocation === 'beside') {
    return 'beside';
  }
  return runbookViewColumn ?? 'active';
}