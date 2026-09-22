// yawr.host-action/v1 generic bridge.
//
// This module implements the VS Code extension side of the yawr.host-action/v1
// contract as defined in design/yawr/sections/18-host-action-bridge.tex.
// It contains no external-view-specific logic.
// Capability handlers are registered externally and injected at construction.
//
// Wire protocol versions: the bridge only accepts messages with
//   version: "yawr.host-action/v1"
// Messages with any other version are silently dropped.

// ─── Wire envelope types ─────────────────────────────────────────────────────

export const WIRE_VERSION = 'yawr.host-action/v1' as const;
export const REQUEST_TYPE = 'yawr.host-action.request' as const;
export const ACK_TYPE = 'yawr.host-action.ack' as const;
export const CANCEL_TYPE = 'yawr.host-action.cancel' as const;

// ─── Error codes (§2.8 canonical codes) ──────────────────────────────────────

export const ERROR_CAPABILITY_NOT_REGISTERED = 'CAPABILITY_NOT_REGISTERED' as const;
export const ERROR_HANDLER_ERROR = 'HANDLER_ERROR' as const;
export const ERROR_HANDLER_TIMEOUT = 'HANDLER_TIMEOUT' as const;

// ─── Public interfaces ────────────────────────────────────────────────────────

/** Minimal CancellationToken — structurally compatible with vscode.CancellationToken. */
export interface CancellationToken {
  readonly isCancellationRequested: boolean;
  onCancellationRequested(listener: () => void): { dispose(): void };
}

/** Arguments passed to every registered capability handler (§2.6 handler contract). */
export interface HostActionHandlerArgs {
  capability: string;
  request: unknown;
  correlationId: string;
  previewSessionId: string;
  requestId: string;
  runId: string;
  turnId: string;
  cancellationToken: CancellationToken;
}

/** Return value from a capability handler (§2.6 handler contract). */
export interface HostActionResult {
  status: 'completed' | 'failed' | 'timed-out' | 'execution-not-started';
  result?: unknown;
  error?: { code: string; message: string };
}

/** A registered capability handler function. */
export type HostActionHandler = (args: HostActionHandlerArgs) => Promise<HostActionResult>;

/** A static registry entry: capability name → handler + optional timeout override. */
export interface HostActionRegistration {
  handler: HostActionHandler;
  /** Timeout in milliseconds; overrides the bridge default when set. */
  timeoutMs?: number;
}

/** Ack envelope sent from extension to preview (§2.1 ack envelope). */
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
  result: unknown | null;
  error: { code: string; message: string } | null;
}

/** Cancel envelope sent from extension to preview (§2.2 cancel envelope). */
export interface HostActionCancelEnvelope {
  type: typeof CANCEL_TYPE;
  version: typeof WIRE_VERSION;
  correlationId: string;
  previewSessionId: string;
  requestId: string;
  status: 'execution-not-started';
  reason: 'panel-disposed' | 'run-replaced' | 'reload';
}

/** Injectable transport seam — implement this for tests (use webviewPanelTransport in production). */
export interface HostActionTransport {
  sendAck(ack: HostActionAckEnvelope): void;
  sendCancel(cancel: HostActionCancelEnvelope): void;
}

// ─── Constants ───────────────────────────────────────────────────────────────

export const DEFAULT_TIMEOUT_MS = 30_000;
const MAX_ENVELOPE_STRING_LENGTH = 1024;
const MAX_PAYLOAD_DEPTH = 8;
const MAX_PAYLOAD_NODES = 512;
const MAX_PAYLOAD_STRING_BYTES = 4096;
const MAX_PAYLOAD_KEY_BYTES = 256;
const MAX_PAYLOAD_ARRAY_ITEMS = 64;
const MAX_PAYLOAD_OBJECT_PROPERTIES = 64;
const MAX_PAYLOAD_JSON_BYTES = 64 * 1024;

const REQUEST_ENVELOPE_FIELDS = new Set([
  'type',
  'version',
  'capability',
  'request',
  'correlationId',
  'previewSessionId',
  'requestId',
  'runId',
  'turnId',
]);

const CANCEL_ENVELOPE_FIELDS = new Set([
  'type',
  'version',
  'correlationId',
  'previewSessionId',
  'requestId',
  'status',
  'reason',
]);

// ─── Built-in test capability: test.echo ─────────────────────────────────────

/** test.echo: echoes request.echo back in result.echo. Used in CI (AC-HA-1, AC-HA-10). */
export const testEchoHandler: HostActionHandler = async (args) => {
  const req = args.request as Record<string, unknown> | null | undefined;
  return {
    status: 'completed',
    result: { echo: req != null && typeof req === 'object' ? req.echo : undefined },
  };
};

// ─── Internal helpers ─────────────────────────────────────────────────────────

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function nonEmptyString(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 && value.length <= MAX_ENVELOPE_STRING_LENGTH
    ? value
    : undefined;
}

function hasExactFields(message: Record<string, unknown>, fields: ReadonlySet<string>): boolean {
  const keys = Object.keys(message);
  return keys.length === fields.size && keys.every((key) => fields.has(key));
}

function isBoundedJsonObject(value: unknown): value is Record<string, unknown> {
  if (!isRecord(value)) return false;
  try {
    if (Buffer.byteLength(JSON.stringify(value), 'utf8') > MAX_PAYLOAD_JSON_BYTES) return false;
  } catch {
    return false;
  }

  let nodes = 0;
  const visit = (item: unknown, depth: number): boolean => {
    if (depth > MAX_PAYLOAD_DEPTH || ++nodes > MAX_PAYLOAD_NODES) return false;
    if (item === null || typeof item === 'boolean') return true;
    if (typeof item === 'number') return Number.isFinite(item);
    if (typeof item === 'string') return Buffer.byteLength(item, 'utf8') <= MAX_PAYLOAD_STRING_BYTES;
    if (Array.isArray(item)) {
      return item.length <= MAX_PAYLOAD_ARRAY_ITEMS && item.every((child) => visit(child, depth + 1));
    }
    if (isRecord(item)) {
      const entries = Object.entries(item);
      return entries.length <= MAX_PAYLOAD_OBJECT_PROPERTIES && entries.every(([key, child]) =>
        Buffer.byteLength(key, 'utf8') <= MAX_PAYLOAD_KEY_BYTES && visit(child, depth + 1));
    }
    return false;
  };
  return visit(value, 1);
}

// Redact handler error messages: strip stack frames, truncate to 200 chars (§2.8).
function redactError(_err: unknown): string {
  return 'Handler execution failed.';
}

/** Parse and validate a yawr.host-action/v1 request envelope. Returns undefined for malformed/silent-drop cases. */
interface ParsedRequestEnvelope {
  capability: string;
  request: unknown;
  correlationId: string;
  previewSessionId: string;
  requestId: string;
  runId: string;
  turnId: string;
}

function parseRequestEnvelope(message: Record<string, unknown>): ParsedRequestEnvelope | undefined {
  // §2.7: type, version, capability, request, and all five correlation fields are required.
  // Missing any → silent drop. Wrong version → silent drop. Wrong type → already filtered upstream.
  if (!hasExactFields(message, REQUEST_ENVELOPE_FIELDS) ||
      message.type !== REQUEST_TYPE || message.version !== WIRE_VERSION) return undefined;

  const correlationId = nonEmptyString(message.correlationId);
  const previewSessionId = nonEmptyString(message.previewSessionId);
  const requestId = nonEmptyString(message.requestId);
  const runId = nonEmptyString(message.runId);
  const turnId = nonEmptyString(message.turnId);

  if (
    correlationId === undefined ||
    previewSessionId === undefined ||
    requestId === undefined ||
    runId === undefined ||
    turnId === undefined
  ) return undefined;

  const capability = nonEmptyString(message.capability);
  if (capability === undefined) return undefined;

  // request must be present as an object (§2.1: REQUIRED, MAY be empty object)
  if (!('request' in message) || !isBoundedJsonObject(message.request)) return undefined;

  return { capability, request: message.request, correlationId, previewSessionId, requestId, runId, turnId };
}

interface ParsedCancelEnvelope {
  correlationId: string;
  previewSessionId: string;
  requestId: string;
}

function parseCancelEnvelope(message: Record<string, unknown>): ParsedCancelEnvelope | undefined {
  if (!hasExactFields(message, CANCEL_ENVELOPE_FIELDS) ||
      message.type !== CANCEL_TYPE || message.version !== WIRE_VERSION ||
      message.status !== 'execution-not-started' ||
      typeof message.reason !== 'string' ||
      !['panel-disposed', 'run-replaced', 'reload'].includes(message.reason)) return undefined;

  const correlationId = nonEmptyString(message.correlationId);
  const previewSessionId = nonEmptyString(message.previewSessionId);
  const requestId = nonEmptyString(message.requestId);
  if (correlationId === undefined || previewSessionId === undefined || requestId === undefined) return undefined;
  return { correlationId, previewSessionId, requestId };
}

function buildAck(
  env: ParsedRequestEnvelope,
  status: HostActionAckEnvelope['status'],
  result: unknown | null,
  error: { code: string; message: string } | null,
): HostActionAckEnvelope {
  return {
    type: ACK_TYPE,
    version: WIRE_VERSION,
    capability: env.capability,
    correlationId: env.correlationId,
    previewSessionId: env.previewSessionId,
    requestId: env.requestId,
    runId: env.runId,
    turnId: env.turnId,
    status,
    result,
    error,
  };
}

// ─── CancellationTokenSource ──────────────────────────────────────────────────

/** Internal mutable source for a CancellationToken — no vscode dependency. */
class CancellationTokenSource {
  private _cancelled = false;
  private _listeners: Array<() => void> = [];

  readonly token: CancellationToken;

  constructor() {
    // Capture `this` explicitly so the token getter always refers to this source.
    const self = this;
    this.token = {
      get isCancellationRequested() { return self._cancelled; },
      onCancellationRequested(listener) {
        if (self._cancelled) {
          listener();
          return { dispose() {} };
        }
        self._listeners.push(listener);
        return {
          dispose() {
            const i = self._listeners.indexOf(listener);
            if (i !== -1) self._listeners.splice(i, 1);
          },
        };
      },
    };
  }

  cancel(): void {
    if (this._cancelled) return;
    this._cancelled = true;
    const snapshot = this._listeners.slice();
    this._listeners = [];
    for (const l of snapshot) l();
  }

  dispose(): void {
    this._listeners = [];
  }
}

// ─── Timeout helper ───────────────────────────────────────────────────────────

class HandlerTimeoutError extends Error {
  constructor() { super('Handler exceeded deadline'); }
}

class HandlerCancelledError extends Error {
  constructor() { super('Handler was cancelled'); }
}

/**
 * Races `p` against a deadline. When the deadline fires, `onTimeout` is called
 * (use it to cancel the handler's CancellationToken), then the promise rejects
 * with HandlerTimeoutError. The internal `settled` flag ensures at-most-once.
 */
function raceWithDeadline<T>(
  p: Promise<T>,
  timeoutMs: number,
  onTimeout: () => void,
  cancellationToken: CancellationToken,
): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    let done = false;
    let cancellationSubscription: { dispose(): void } | undefined;
    const finish = (complete: () => void) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      cancellationSubscription?.dispose();
      complete();
    };
    const timer = setTimeout(() => {
      if (done) return;
      done = true;
      onTimeout();
      cancellationSubscription?.dispose();
      reject(new HandlerTimeoutError());
    }, timeoutMs);
    cancellationSubscription = cancellationToken.onCancellationRequested(() => {
      finish(() => reject(new HandlerCancelledError()));
    });
    p.then(
      (value) => finish(() => resolve(value)),
      (error) => finish(() => reject(error)),
    );
  });
}

// ─── Bridge ───────────────────────────────────────────────────────────────────

interface PendingEntry {
  env: ParsedRequestEnvelope;
  cts: CancellationTokenSource;
}

/** Generic yawr.host-action/v1 bridge. Zero external-view-specific logic. */
export class HostActionBridge {
  // Key = requestId (CI-2: at-most-once is per requestId).
  private readonly pending = new Map<string, PendingEntry>();
  private readonly settled = new Set<string>();

  constructor(
    private readonly registry: ReadonlyMap<string, HostActionRegistration>,
    private readonly transport: HostActionTransport,
    private readonly defaultTimeoutMs: number,
  ) {}

  /**
   * Process an incoming postMessage from the webview. Silently drops anything
   * that is not a well-formed yawr.host-action/v1 request.
   */
  async receive(message: unknown): Promise<void> {
    if (!isRecord(message)) return;
    if (message.type === CANCEL_TYPE) {
      const cancel = parseCancelEnvelope(message);
      if (cancel === undefined) return;
      const entry = this.pending.get(cancel.requestId);
      if (entry === undefined ||
          entry.env.correlationId !== cancel.correlationId ||
          entry.env.previewSessionId !== cancel.previewSessionId) return;

      this.pending.delete(cancel.requestId);
      this.settled.add(cancel.requestId);
      entry.cts.cancel();
      entry.cts.dispose();
      return;
    }

    if (message.type !== REQUEST_TYPE) return;

    const env = parseRequestEnvelope(message);
    if (env === undefined) return; // silent drop: malformed envelope

    const { requestId } = env;

    // At-most-once (CI-2): drop if already settled or in-flight.
    if (this.settled.has(requestId) || this.pending.has(requestId)) return;

    // Capability lookup (§2.5 allowlisting).
    const registration = this.registry.get(env.capability);
    if (registration === undefined) {
      // Emit unsupported synchronously within this event-loop turn (AC-HA-3).
      this.settled.add(requestId);
      this.transport.sendAck(buildAck(
        env, 'unsupported', null,
        { code: ERROR_CAPABILITY_NOT_REGISTERED, message: 'Capability is not registered.' },
      ));
      return;
    }

    // Add to pending set before any async work.
    const cts = new CancellationTokenSource();
    this.pending.set(requestId, { env, cts });

    const timeoutMs = registration.timeoutMs ?? this.defaultTimeoutMs;
    let handlerPromise: Promise<HostActionResult>;
    try {
      handlerPromise = registration.handler({
        capability: env.capability,
        request: env.request,
        correlationId: env.correlationId,
        previewSessionId: env.previewSessionId,
        requestId: env.requestId,
        runId: env.runId,
        turnId: env.turnId,
        cancellationToken: cts.token,
      });
    } catch (syncErr) {
      // Handler threw synchronously (H-3: bridge wraps in try/catch).
      if (!this.pending.delete(requestId)) return;
      this.settled.add(requestId);
      cts.dispose();
      this.transport.sendAck(buildAck(
        env, 'failed', null,
        { code: ERROR_HANDLER_ERROR, message: redactError(syncErr) },
      ));
      return;
    }

    // Race handler against deadline (B-2: timeout enforcement).
    let result: HostActionResult;
    try {
      result = await raceWithDeadline(handlerPromise, timeoutMs, () => cts.cancel(), cts.token);
    } catch (err) {
      if (!this.pending.delete(requestId)) return; // cancelled externally; do not double-ack
      this.settled.add(requestId);
      cts.dispose();
      if (err instanceof HandlerTimeoutError) {
        this.transport.sendAck(buildAck(
          env, 'timed-out', null,
          { code: ERROR_HANDLER_TIMEOUT, message: 'Handler exceeded the configured deadline.' },
        ));
      } else {
        this.transport.sendAck(buildAck(
          env, 'failed', null,
          { code: ERROR_HANDLER_ERROR, message: redactError(err) },
        ));
      }
      return;
    }

    // Handler succeeded or returned a structured failure.
    if (!this.pending.delete(requestId)) return; // cancelled externally
    this.settled.add(requestId);
    cts.dispose();

    if (!isRecord(result) || !['completed', 'failed', 'timed-out', 'execution-not-started'].includes(String(result.status))) {
      this.transport.sendAck(buildAck(
        env, 'failed', null,
        { code: ERROR_HANDLER_ERROR, message: 'Handler returned an invalid result.' },
      ));
      return;
    }

    if (result.status === 'completed') {
      if (!isBoundedJsonObject(result.result)) {
        this.transport.sendAck(buildAck(
          env, 'failed', null,
          { code: ERROR_HANDLER_ERROR, message: 'Handler completed without a non-null object result.' },
        ));
        return;
      }
      this.transport.sendAck(buildAck(env, 'completed', result.result, null));
      return;
    }

    const ackError = isRecord(result.error) &&
      typeof result.error.code === 'string' && typeof result.error.message === 'string'
      ? { code: result.error.code, message: 'Handler reported an error.' }
      : { code: ERROR_HANDLER_ERROR, message: 'Handler returned a non-completed status without an error.' };
    this.transport.sendAck(buildAck(env, result.status, null, ackError));
  }

  /**
   * Cancel all pending requests with the given lifecycle reason (§2.8).
   * Signals each handler's CancellationToken, emits cancel envelopes, and
   * removes requests from the pending set so any in-flight handler resolution
   * is silently discarded (CI-4: monotonic terminal state).
   */
  cancelAllPending(reason: 'panel-disposed' | 'run-replaced' | 'reload'): void {
    const entries = [...this.pending.entries()];
    this.pending.clear();
    for (const [requestId, entry] of entries) {
      entry.cts.cancel();
      entry.cts.dispose();
      this.settled.add(requestId);
      this.transport.sendCancel({
        type: CANCEL_TYPE,
        version: WIRE_VERSION,
        correlationId: entry.env.correlationId,
        previewSessionId: entry.env.previewSessionId,
        requestId: entry.env.requestId,
        status: 'execution-not-started',
        reason,
      });
    }
  }

  /** Alias for cancelAllPending('panel-disposed') — call on WebviewPanel.onDidDispose. */
  dispose(): void {
    this.cancelAllPending('panel-disposed');
  }
}

// ─── Factory ─────────────────────────────────────────────────────────────────

/**
 * Create a HostActionBridge from a static registry and an injectable transport.
 *
 * @param registry  Static capability allowlist (compile-time only; must not be mutated after construction).
 * @param transport Seam for sending ack/cancel envelopes. Use webviewPanelTransport in production.
 * @param defaultTimeoutMs  Global handler timeout in ms (default: 30 000).
 */
export function createHostActionBridge(
  registry: ReadonlyMap<string, HostActionRegistration>,
  transport: HostActionTransport,
  defaultTimeoutMs = DEFAULT_TIMEOUT_MS,
): HostActionBridge {
  return new HostActionBridge(registry, transport, defaultTimeoutMs);
}

// ─── Production transport adapter ────────────────────────────────────────────

/**
 * Build a HostActionTransport that posts messages through a VS Code WebviewPanel.
 * Pass a guard function to suppress messages after the panel is disposed.
 */
export function webviewPanelTransport(
  panel: { webview: { postMessage(msg: unknown): void } },
  isActive?: () => boolean,
): HostActionTransport {
  return {
    sendAck(ack) {
      if (isActive && !isActive()) return;
      panel.webview.postMessage(ack);
    },
    sendCancel(cancel) {
      if (isActive && !isActive()) return;
      panel.webview.postMessage(cancel);
    },
  };
}
