import { isExecutionEnded, type CurrentActivity } from './executionProgress';
import type { WorkflowMode } from './workflowProjection';

export function executionViewMode(preference: WorkflowMode, runStatus: string): WorkflowMode {
  // Keep technical steps expanded through the terminal result, not just while active.
  return ['idle', 'not-started'].includes(runStatus) ? preference : 'all';
}

export function currentExecutionNode(
  activities: readonly CurrentActivity[], runStatus: string, graphIDs: ReadonlySet<string>,
  reachedID?: string, previousID?: string, pendingID?: string,
): string | undefined {
  if (['idle', 'not-started', 'starting'].includes(runStatus)) return undefined;
  const known = (id: string | undefined) => id && graphIDs.has(id) ? id : undefined;
  if (isExecutionEnded(runStatus)) return known(reachedID) ?? known(previousID);
  const visible = activities.filter(activity => activity.inGraph && graphIDs.has(activity.nodeID));
  const pending = pendingID ? visible.find(activity => activity.nodeID === pendingID || activity.path === pendingID) : undefined;
  if (pending) return pending.nodeID;
  const leaves = visible.filter(activity => !activity.container);
  const candidates = leaves.length ? leaves : visible;
  // A concurrent lane starting must not steal the cursor from a still-active leaf.
  return candidates.find(activity => activity.nodeID === previousID)?.nodeID ??
    candidates[0]?.nodeID ?? known(reachedID) ?? known(previousID);
}

interface Rect {
  left: number;
  top: number;
  right: number;
  bottom: number;
}
interface Viewport {
  x: number;
  y: number;
  zoom: number;
}

export const EXECUTION_PAN_DURATION = 280;

interface AnimationClock {
  now(): number;
  requestFrame(callback: (time: number) => void): number;
  cancelFrame(frame: number): void;
}

export function animateExecutionViewport(
  from: Viewport, to: Viewport, write: (viewport: Viewport) => void,
  clock: AnimationClock, reducedMotion: boolean,
): () => void {
  if (reducedMotion) {
    write(to);
    return () => {};
  }
  const start = clock.now();
  let frame = 0;
  let cancelled = false;
  const advance = (time: number) => {
    if (cancelled) return;
    const elapsed = Math.min(1, Math.max(0, (time - start) / EXECUTION_PAN_DURATION));
    const eased = 1 - (1 - elapsed) ** 3;
    // React Flow's built-in zoom interpolation can zoom out even for a pure pan.
    write({ x: from.x + (to.x - from.x) * eased, y: from.y + (to.y - from.y) * eased, zoom: from.zoom });
    if (elapsed < 1) frame = clock.requestFrame(advance);
  };
  frame = clock.requestFrame(advance);
  return () => {
    cancelled = true;
    clock.cancelFrame(frame);
  };
}

export function executionViewport(viewport: Viewport, canvas: Rect, node: Rect): Viewport | undefined {
  if (canvas.right <= canvas.left || canvas.bottom <= canvas.top ||
      node.right <= node.left || node.bottom <= node.top) return undefined;
  const shift = (start: number, end: number, near: number, far: number) => {
    if (start >= near && end <= far || start <= near && end >= far) return 0;
    const margin = Math.min(24, (far - near) / 10);
    if (end - start > far - near - margin * 2) {
      return start > near ? near + margin - start : far - margin - end;
    }
    return start < near ? near + margin - start : end > far ? far - margin - end : 0;
  };
  const x = shift(node.left, node.right, canvas.left, canvas.right);
  const y = shift(node.top, node.bottom, canvas.top, canvas.bottom);
  return x || y ? { x: viewport.x + x, y: viewport.y + y, zoom: viewport.zoom } : undefined;
}
