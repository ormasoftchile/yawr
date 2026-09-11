const WIRE_VERSION = 'yawr.host-action/v1' as const;
const ACK_TYPE = 'yawr.host-action.ack' as const;
const CANCEL_TYPE = 'yawr.host-action.cancel' as const;
const MAX_ENVELOPE_STRING_LENGTH = 1024;

const ACK_FIELDS = new Set([
  'type', 'version', 'capability', 'correlationId', 'previewSessionId', 'requestId',
  'runId', 'turnId', 'status', 'result', 'error',
]);
const CANCEL_FIELDS = new Set([
  'type', 'version', 'correlationId', 'previewSessionId', 'requestId', 'status', 'reason',
]);
const ACK_STATUSES = new Set(['completed', 'failed', 'unsupported', 'timed-out', 'execution-not-started']);
const CANCEL_REASONS = new Set(['panel-disposed', 'run-replaced', 'reload']);

export interface HostActionAckEnvelope {
  type: typeof ACK_TYPE;
  version: typeof WIRE_VERSION;
  capability: string;
  correlationId: string;
  previewSessionId: string;
  requestId: string;
  runId: string;
  turnId: string;
  status: 'completed' | 'failed' | 'unsupported' | 'timed-out' | 'execution-not-started';
  result: Record<string, unknown> | null;
  error: { code: string; message: string } | null;
}

export interface HostActionCancelEnvelope {
  type: typeof CANCEL_TYPE;
  version: typeof WIRE_VERSION;
  correlationId: string;
  previewSessionId: string;
  requestId: string;
  status: 'execution-not-started';
  reason: 'panel-disposed' | 'run-replaced' | 'reload';
}

export type HostActionResponseEnvelope = HostActionAckEnvelope | HostActionCancelEnvelope;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function hasExactFields(value: Record<string, unknown>, fields: ReadonlySet<string>): boolean {
  const keys = Object.keys(value);
  return keys.length === fields.size && keys.every((key) => fields.has(key));
}

function isWireString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= MAX_ENVELOPE_STRING_LENGTH;
}

function isAckError(value: unknown): value is { code: string; message: string } {
  return isRecord(value) && hasExactFields(value, new Set(['code', 'message'])) &&
    isWireString(value.code) && isWireString(value.message);
}

export function parseHostActionResponse(value: unknown): HostActionResponseEnvelope | undefined {
  if (!isRecord(value) || value.version !== WIRE_VERSION) return undefined;
  if (value.type === CANCEL_TYPE) {
    if (!hasExactFields(value, CANCEL_FIELDS) || value.status !== 'execution-not-started' ||
        !CANCEL_REASONS.has(String(value.reason)) || !isWireString(value.correlationId) ||
        !isWireString(value.previewSessionId) || !isWireString(value.requestId)) return undefined;
    return value as unknown as HostActionCancelEnvelope;
  }
  if (value.type !== ACK_TYPE || !hasExactFields(value, ACK_FIELDS) ||
      !ACK_STATUSES.has(String(value.status)) || !isWireString(value.capability) ||
      !isWireString(value.correlationId) || !isWireString(value.previewSessionId) ||
      !isWireString(value.requestId) || !isWireString(value.runId) || !isWireString(value.turnId)) return undefined;
  if (value.status === 'completed') {
    if (!isRecord(value.result) || value.error !== null) return undefined;
  } else if (value.result !== null || !isAckError(value.error)) {
    return undefined;
  }
  return value as unknown as HostActionAckEnvelope;
}