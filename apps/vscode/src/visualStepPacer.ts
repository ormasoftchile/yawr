export const DEFAULT_MINIMUM_STEP_DISPLAY_MS = 200;

export function minimumStepDisplayMs(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value)
    ? Math.min(Number.MAX_SAFE_INTEGER, Math.max(0, Math.floor(value)))
    : DEFAULT_MINIMUM_STEP_DISPLAY_MS;
}

export interface VisualStep {
  nodeID: string;
  progressing: boolean;
}

export interface PacingClock {
  now(): number;
  setTimeout(callback: () => void, delay: number): number;
  clearTimeout(timer: number): void;
}

/** Only the visual cursor is queued; runtime state and evidence never enter this queue. */
export class VisualStepPacer {
  private queue: VisualStep[] = [];
  private displayed?: VisualStep;
  private latest?: VisualStep;
  private displayedAt = 0;
  private timer?: number;
  private generation = 0;
  private visible = true;
  private disposed = false;
  private interval = DEFAULT_MINIMUM_STEP_DISPLAY_MS;
  private awaitingCommit = false;
  private completing = false;

  constructor(
    private readonly publish: (step: VisualStep | undefined) => void,
    private readonly clock: PacingClock,
    private readonly requireCommit = false,
  ) {}

  acknowledge(step: VisualStep): void {
    if (this.disposed || !this.visible || !this.awaitingCommit || step !== this.displayed) return;
    this.awaitingCommit = false;
    this.displayedAt = this.clock.now();
    this.schedule();
  }

  start(interval: unknown): void {
    if (this.disposed) return;
    this.interval = minimumStepDisplayMs(interval);
    this.bypass();
  }

  ordinary(nodeID: string): void {
    if (this.disposed) return;
    if (this.completing && !this.queue.length) this.cancelTimer();
    this.completing = false;
    const next = { nodeID, progressing: true };
    this.latest = next;
    if (!this.visible) return;
    if (!this.interval || !this.displayed?.progressing) {
      this.bypass(next);
      return;
    }
    const tail = this.queue.at(-1) ?? this.displayed;
    if (tail.nodeID === nodeID) return;
    this.queue.push(next);
    this.schedule();
  }

  bypass(step?: VisualStep): void {
    if (this.disposed) return;
    this.cancelTimer();
    this.queue = [];
    this.completing = false;
    this.latest = step;
    if (this.visible) this.show(step);
  }

  complete(): void {
    if (this.disposed || this.completing) return;
    if (!this.visible || !this.interval || !this.displayed?.progressing) {
      this.bypass();
      return;
    }
    this.completing = true;
    this.schedule();
  }

  setVisible(visible: boolean): void {
    if (this.disposed || visible === this.visible) return;
    this.visible = visible;
    this.cancelTimer();
    this.queue = [];
    if (this.completing) {
      this.completing = false;
      this.latest = undefined;
    }
    if (visible) this.show(this.latest ? { ...this.latest } : undefined);
  }

  dispose(): void {
    this.cancelTimer();
    this.queue = [];
    this.disposed = true;
  }

  private show(step: VisualStep | undefined): void {
    this.displayed = step;
    this.displayedAt = this.clock.now();
    this.awaitingCommit = this.requireCommit && step !== undefined;
    this.publish(step);
  }

  private cancelTimer(): void {
    this.generation++;
    if (this.timer !== undefined) this.clock.clearTimeout(this.timer);
    this.timer = undefined;
  }

  private schedule(): void {
    if (this.awaitingCommit || this.timer !== undefined || (!this.queue.length && !this.completing)) return;
    const remaining = Math.max(0, this.interval - (this.clock.now() - this.displayedAt));
    const generation = this.generation;
    this.timer = this.clock.setTimeout(() => {
      if (this.disposed || !this.visible || generation !== this.generation) return;
      this.timer = undefined;
      if (this.clock.now() - this.displayedAt >= this.interval) {
        if (this.queue.length) this.show(this.queue.shift());
        else if (this.completing) {
          this.completing = false;
          this.latest = undefined;
          this.show(undefined);
        }
      }
      this.schedule();
    }, Math.min(remaining, 2_147_483_647));
  }
}
