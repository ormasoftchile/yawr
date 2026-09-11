export type DirectDebugPhase = 'before' | 'after';

export interface DirectDebugCallFrame {
  step_id: string;
  runbook_path?: string;
}

export interface DirectDebugBreakpoint {
  step: string;
  phase: DirectDebugPhase;
  callPath?: DirectDebugCallFrame[];
}

export interface DirectDebugRunConfig {
  enabled: true;
  breakpoints: DirectDebugBreakpoint[];
  watches?: string[];
}

const MAX_DEBUG_CONFIG_BYTES = 1024 * 1024;
const MAX_BREAKPOINTS = 256;
const MAX_CALL_PATH = 32;
const MAX_WATCHES = 32;
const MAX_TEXT_LENGTH = 1024;

export function parseDirectDebugConfig(value: unknown): DirectDebugRunConfig | undefined {
  if (value === undefined) return undefined;
  try {
    const raw = record(value);
    exactKeys(raw, ['enabled', 'breakpoints', 'watches']);
    if (raw.enabled !== true) throw new Error('enabled must be true');
    const serialized = JSON.stringify(raw);
    if (Buffer.byteLength(serialized, 'utf8') > MAX_DEBUG_CONFIG_BYTES) {
      throw new Error('payload is too large');
    }

    const breakpoints = array(raw.breakpoints ?? []);
    if (breakpoints.length === 0) throw new Error('at least one breakpoint is required');
    if (breakpoints.length > MAX_BREAKPOINTS) throw new Error('too many breakpoints');
    const parsedBreakpoints = breakpoints.map((candidate) => {
      const breakpoint = record(candidate);
      exactKeys(breakpoint, ['step', 'phase', 'callPath']);
      const step = boundedText(breakpoint.step, 'breakpoint step');
      if (breakpoint.phase !== 'before' && breakpoint.phase !== 'after') {
        throw new Error('breakpoint phase must be before or after');
      }
      const callPath = array(breakpoint.callPath ?? []);
      if (callPath.length > MAX_CALL_PATH) throw new Error('call path is too deep');
      const parsedCallPath = callPath.map((candidateFrame) => {
        const frame = record(candidateFrame);
        exactKeys(frame, ['step_id', 'runbook_path']);
        const parsed: DirectDebugCallFrame = {
          step_id: boundedText(frame.step_id, 'call path step'),
        };
        if (frame.runbook_path !== undefined) {
          parsed.runbook_path = boundedText(frame.runbook_path, 'call path runbook');
        }
        return parsed;
      });
      return {
        step,
        phase: breakpoint.phase,
        ...(parsedCallPath.length > 0 ? { callPath: parsedCallPath } : {}),
      } as DirectDebugBreakpoint;
    });

    const watches = array(raw.watches ?? []);
    if (watches.length > MAX_WATCHES) throw new Error('too many watches');
    const parsedWatches = watches.map((watch) => boundedText(watch, 'watch'));
    return {
      enabled: true,
      breakpoints: parsedBreakpoints,
      ...(parsedWatches.length > 0 ? { watches: parsedWatches } : {}),
    };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new Error(`invalid debug configuration: ${message}`);
  }
}

export function validateDirectDebugTargets(
  config: DirectDebugRunConfig | undefined,
  nodes: readonly { id: string; data: Record<string, unknown> }[],
): void {
  if (!config) return;
  const targets = new Set(nodes.map((node) => {
    const step = typeof node.data.step_id === 'string' && node.data.step_id ? node.data.step_id : node.id;
    const callPath = Array.isArray(node.data.call_path)
      ? node.data.call_path.filter((value): value is string => typeof value === 'string')
      : [];
    return targetKey(step, callPath);
  }));
  for (const breakpoint of config.breakpoints) {
    const callPath = (breakpoint.callPath ?? []).map((frame) => frame.step_id);
    if (!targets.has(targetKey(breakpoint.step, callPath))) {
      throw new Error(`debug breakpoint ${[...callPath, breakpoint.step].join('/')} does not exist in the loaded graph`);
    }
  }
}

function targetKey(step: string, callPath: readonly string[]): string {
  return JSON.stringify([step, callPath]);
}

function record(value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error('expected an object');
  }
  return value as Record<string, unknown>;
}

function array(value: unknown): unknown[] {
  if (!Array.isArray(value)) throw new Error('expected an array');
  return value;
}

function exactKeys(value: Record<string, unknown>, allowed: readonly string[]): void {
  const allowedKeys = new Set(allowed);
  if (Object.keys(value).some((key) => !allowedKeys.has(key))) {
    throw new Error('unknown field');
  }
}

function boundedText(value: unknown, label: string): string {
  if (typeof value !== 'string' || value.trim() === '' || value.length > MAX_TEXT_LENGTH) {
    throw new Error(`${label} must be a non-empty bounded string`);
  }
  return value;
}
