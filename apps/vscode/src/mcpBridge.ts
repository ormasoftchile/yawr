// mcpBridge.ts — loopback HTTP server that lets Yawr invoke VS Code's
// already-authenticated MCP tools without touching any credential.
//
// Wire contract: yawr.vscode-mcp-bridge/v1
// Default port:  7779, loopback (127.0.0.1) ONLY
//
// Security invariant: nothing that reaches the Yawr process (env vars,
// run state, results, error text) may contain the capability secret or any
// MCP credential. The capability secret is minted HERE and passed TO Yawr;
// Yawr never generates it. Yawr must echo it back on every request for
// timing-safe comparison before any MCP call is made.

import * as http from 'http';
import * as crypto from 'crypto';
import type * as vscode from 'vscode';
import { buildRegistryFromDir } from './toolDefinitionRegistry';

// ─── Wire types ──────────────────────────────────────────────────────────────

export interface BridgeRequest {
  version: string;
  request_id: string;
  tool: string;
  action: string;
  args: Record<string, unknown>;
  deadline_unix_ms?: number;
  capability_proof: string;
}

export interface BridgeErrorBody {
  code: string;
  message: string;
}

export interface BridgeResponse {
  version: string;
  request_id: string;
  result?: Record<string, unknown>;
  error?: BridgeErrorBody;
}

// ─── Dependency-injected LM interface ────────────────────────────────────────
// Production code passes vscode.lm; tests pass a plain stub object.

export type FieldType = 'string' | 'number' | 'boolean' | 'object' | 'array';

/** Per-field output specification: declared type and whether the field is required. */
export interface OutputFieldSpec {
  type: FieldType;
  /** When false, an absent field is allowed. Present-but-null/wrong-type still fails. */
  required: boolean;
}

export interface LmToolInfo {
  name: string;
  /**
   * JSON Schema object describing the tool's accepted inputs.  When present,
   * the bridge validates adapted args against it before calling invokeTool.
   * Shape mirrors VS Code's LanguageModelToolInformation.inputSchema.
   */
  inputSchema?: {
    type?: string;
    properties?: Record<string, { type?: string }>;
    required?: string[];
  };
}

export interface LmToolResult {
  content: Array<{ value?: string } | { text?: string } | unknown>;
}

export interface LmCancellationToken {
  isCancellationRequested: boolean;
  onCancellationRequested: (listener: () => void) => { dispose: () => void };
}

export interface LmInterface {
  readonly tools: readonly LmToolInfo[];
  invokeTool(
    name: string,
    options: { input: Record<string, unknown>; toolInvocationToken?: unknown },
    token: LmCancellationToken,
  ): Promise<LmToolResult>;
  /**
   * Returns the toolInvocationToken captured from the current chat handler.
   * The bridge passes this to invokeTool as VS Code's tool-invocation consent
   * token. It is NOT an MCP authorization credential.
   *
   * Safety invariant: absence or rejection of this token must fail closed.
   * The bridge must not silently retry without it because that can trigger an
   * interactive authentication/consent flow outside the armed path.
   */
  getToolInvocationToken(): unknown;
  /**
   * Called when VS Code rejects the supplied token (stale session or
   * re-authentication required). Implementations must clear the token store
   * so the operator must re-arm.
   */
  onTokenRejected?(): void;
}

// ─── Tool / action spec ────────────────────────────────────────────────────────
// The registry is built at runtime from workspace *.tool.yaml definitions.
// resolveSpec() is the single lookup surface: callers pass the registry and
// optional name-override map rather than relying on a hard-coded constant.

export interface ToolActionSpec {
  registeredName: string;
  /** Declared output fields with type and required flag. */
  outputFields: Record<string, OutputFieldSpec>;
}

/**
 * resolveSpec returns the spec for a (tool, action) pair, consulting the
 * provided registry and applying any name overrides.
 *
 * @param tool        Logical tool name (e.g. "icm")
 * @param action      Action name (e.g. "get-incident")
 * @param registry    Registry built from *.tool.yaml definitions
 * @param overrides   Optional map of "tool/action" → registered name, sourced
 *                    from the yawr.mcpBridge.toolNameOverrides VS Code setting
 */
export function resolveSpec(
  tool: string,
  action: string,
  registry: Record<string, ToolActionSpec>,
  overrides?: Record<string, string>,
): ToolActionSpec | undefined {
  const key = `${tool}/${action}`;
  const spec = registry[key];
  if (!spec) return undefined;
  const overrideName = overrides?.[key];
  if (typeof overrideName === 'string' && overrideName) {
    return { ...spec, registeredName: overrideName };
  }
  return spec;
}

// ─── Result normalizer ────────────────────────────────────────────────────────
// Validate the declared output contract while tolerating additive MCP result
// fields. A declared field marked required: false may be absent (optional
// absent → OK). These remain errors:
//   • present-but-null/undefined → error
//   • present-but-wrong-type    → error
//   • required field absent     → error
// Unknown extra fields are ignored; MCP Result is an extensible object.

export function normalizeResult(
  raw: unknown,
  spec: ToolActionSpec,
): { ok: true; value: Record<string, unknown> } | { ok: false; reason: string } {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    return { ok: false, reason: 'MCP result is not a JSON object' };
  }
  const obj = raw as Record<string, unknown>;

  const out: Record<string, unknown> = {};
  for (const [field, fieldSpec] of Object.entries(spec.outputFields)) {
    if (!(field in obj)) {
      if (!fieldSpec.required) {
        // Optional field absent — this is a valid outcome; omit from output.
        continue;
      }
      return { ok: false, reason: `declared output field "${field}" is missing from MCP result` };
    }
    const v = obj[field];
    // present-but-null/undefined is always an error, even for optional fields.
    if (v === null || v === undefined) {
      return { ok: false, reason: `declared output field "${field}" is null/undefined` };
    }
    const actual = Array.isArray(v) ? 'array' : typeof v;
    if (actual !== fieldSpec.type) {
      return {
        ok: false,
        reason: `field "${field}": expected ${fieldSpec.type}, got ${actual}`,
      };
    }
    out[field] = v;
  }
  return { ok: true, value: out };
}

// extractTextFromResult pulls text from a LanguageModelToolResult content
// array, concatenating all text parts.
export function extractTextFromResult(result: LmToolResult): string {
  const parts: string[] = [];
  for (const part of result.content) {
    if (part && typeof part === 'object') {
      const p = part as Record<string, unknown>;
      if (typeof p['value'] === 'string') parts.push(p['value']);
      else if (typeof p['text'] === 'string') parts.push(p['text']);
    }
  }
  return parts.join('');
}

// ─── Redaction guard ─────────────────────────────────────────────────────────
// Scrubs the capability secret from any string before it leaves the bridge.
// Used on every outbound message and log line.

function redact(secret: string, text: string): string {
  if (!secret) return text;
  // Use a global replace; the secret is 64 hex chars so no regex special chars.
  return text.split(secret).join('[REDACTED]');
}

// ─── Invocation error classifier ─────────────────────────────────────────────
// Allowlisted categories for vscode.lm.invokeTool failures.  Provider
// exception text is NEVER forwarded; we classify from it and then drop it.
//
// invocation_token_unavailable — VS Code rejected the call because the
//   toolInvocationToken was rejected (stale session or re-auth required).
//   Produced only when VS Code throws a token-related error during invocation.
// provider_unavailable         — the MCP server itself is not running, could
//   not be started, or is not reachable (including a 401 during MCP startup).
//   Operator action: fix the MCP server's authentication/configuration, not Yawr.
//   PRECEDENCE NOTE: provider_unavailable is checked BEFORE authorization_unavailable.
//   A startup failure that mentions "401" classifies here, not as authorization_unavailable,
//   because the 401 came from the MCP server's own start-up, not from a Yawr credential.
//   The operator's required action (re-authenticate the MCP provider) is completely
//   different from an authorization_unavailable action (check Yawr/VS Code credentials).
// authorization_unavailable    — auth/credential/forbidden failure from the
//   provider or VS Code auth layer (after a successful MCP server start).
// provider_input_rejected      — the provider rejected the supplied arguments
//   (input-validation failure raised by the MCP server itself, not our schema
//   guard which fires earlier as input_validation_error).
// invocation_error             — any other or unrecognised exception; the safe
//   conservative fallback.

export type InvocationErrorCategory =
  | 'invocation_token_unavailable'
  | 'provider_unavailable'
  | 'authorization_unavailable'
  | 'provider_input_rejected'
  | 'invocation_error';

export function classifyInvocationError(err: unknown): InvocationErrorCategory {
  const msg = err instanceof Error ? err.message : String(err);
  // Token-unavailable: VS Code requires a toolInvocationToken; the call was
  // rejected because the token was stale or absent.
  if (/invocation.?token|toolInvocationToken|no.*token.*invocation|token.*required/i.test(msg)) {
    return 'invocation_token_unavailable';
  }
  // Provider unavailable: the MCP server itself did not start / has stopped /
  // is not reachable.  Checked BEFORE authorization_unavailable because a
  // server that returned 401 during startup is a provider-availability problem
  // (fix the MCP server's sign-in), not a Yawr credential problem.
  if (/MCP server has stopped|MCP server could not be started|MCP server is not running|MCP server unavailable|server not running|server unavailable/i.test(msg)) {
    return 'provider_unavailable';
  }
  // Authorization / credential failure — preserve the existing category meaning.
  if (/auth|credential|unauthorized|forbidden/i.test(msg)) {
    return 'authorization_unavailable';
  }
  // Provider rejected the supplied arguments at its own validation layer.
  if (/invalid.?input|input.?invalid|invalid.?param|bad.?request|schema.?error|validation.?error/i.test(msg)) {
    return 'provider_input_rejected';
  }
  // Conservative fallback — unknown or ambiguous exception.
  return 'invocation_error';
}

// ─── Provider hint extractor ──────────────────────────────────────────────────
// Builds a SHORT, SAFE, strictly allowlisted hint from a provider error for
// operator display.  This is an allowlist, NOT a denylist: the hint is assembled
// only from recognised pieces; arbitrary provider text is never passed through.
//
// Allowed pieces (in order):
//   1. A recognised fixed phrase from PROVIDER_HINT_PHRASES (canonical casing used).
//   2. An HTTP error status code (4xx/5xx only) if present in the message.
//   3. The ORIGIN ONLY of any URL (scheme + host — never path, query, or fragment).
//
// Hard cap: total hint length ≤ PROVIDER_HINT_MAX_LEN characters.
// Never contains: tokens, secrets, tool arguments, result content, stack traces,
// URL paths/queries, or arbitrary free text.

/** Allowlisted phrases that are safe to surface verbatim in operator-facing hints. */
const PROVIDER_HINT_PHRASES = [
  'MCP server has stopped',
  'MCP server could not be started',
  'MCP server is not running',
  'MCP server unavailable',
  'server not running',
  'server unavailable',
] as const;

const PROVIDER_HINT_MAX_LEN = 200;

export function extractProviderHint(err: unknown): string | null {
  const msg = err instanceof Error ? err.message : String(err);
  const parts: string[] = [];

  // 1. Recognised fixed phrase (use canonical casing from the allowlist).
  for (const phrase of PROVIDER_HINT_PHRASES) {
    if (new RegExp(phrase, 'i').test(msg)) {
      parts.push(phrase);
      break; // only one phrase per hint
    }
  }

  // 2. HTTP error status code (4xx/5xx only — informative, not credential-bearing).
  const statusMatch = msg.match(/\b([45]\d{2})\b/);
  if (statusMatch) {
    parts.push(`HTTP ${statusMatch[1]}`);
  }

  // 3. URL origin only (scheme + host — strip path, query, fragment).
  const urlMatch = msg.match(/https?:\/\/[^\s/?#]*/);
  if (urlMatch) {
    try {
      const u = new URL(urlMatch[0] + '/');
      parts.push(`${u.protocol}//${u.host}`);
    } catch {
      // Unparseable URL fragment — skip entirely.
    }
  }

  if (parts.length === 0) return null;

  const hint = parts.join('; ');
  return hint.length > PROVIDER_HINT_MAX_LEN ? hint.slice(0, PROVIDER_HINT_MAX_LEN) : hint;
}

// ─── Canceled-error predicate ─────────────────────────────────────────────────
// VS Code raises an error whose message contains "Canceled" (capital C) when a
// tool invocation is canceled by the VS Code LM/tool layer. Historically the
// bridge treated this as permission to retry without toolInvocationToken; that
// unsafe downgrade is now forbidden. Canceled is still recognized so the bridge
// can fail closed with invocation_token_unavailable and clear the cached token.
//
// Exported so tests can verify the predicate without a VS Code host.
export function isCanceledError(err: unknown): boolean {
  const msg = err instanceof Error ? err.message : String(err);
  return /\bCanceled\b/.test(msg) || /\bcancelled\b/i.test(msg);
}

// ─── Input schema validation ──────────────────────────────────────────────────
// Validates adapted args against a tool's live inputSchema BEFORE invoking the
// tool. Fail-closed: any missing required parameter, unknown parameter, or type
// mismatch returns an error string; undefined means the args are valid.

function checkJsonSchemaType(param: string, value: unknown, expectedType: string): string | undefined {
  switch (expectedType) {
    case 'integer':
      if (typeof value !== 'number' || !Number.isInteger(value))
        return `parameter "${param}": expected integer, got ${typeof value}`;
      break;
    case 'number':
      if (typeof value !== 'number')
        return `parameter "${param}": expected number, got ${typeof value}`;
      break;
    case 'string':
      if (typeof value !== 'string')
        return `parameter "${param}": expected string, got ${typeof value}`;
      break;
    case 'boolean':
      if (typeof value !== 'boolean')
        return `parameter "${param}": expected boolean, got ${typeof value}`;
      break;
    case 'array':
      if (!Array.isArray(value))
        return `parameter "${param}": expected array, got ${typeof value}`;
      break;
    case 'object':
      if (typeof value !== 'object' || Array.isArray(value) || value === null)
        return `parameter "${param}": expected object, got ${Array.isArray(value) ? 'array' : typeof value}`;
      break;
  }
  return undefined;
}

export function validateArgsAgainstSchema(
  args: Record<string, unknown>,
  schema: NonNullable<LmToolInfo['inputSchema']>,
): string | undefined {
  const props = schema.properties ?? {};
  const required = schema.required ?? [];

  // Required params must be present.
  for (const param of required) {
    if (!(param in args))
      return `required parameter "${param}" is missing`;
  }

  // No extra params allowed (fail-closed).
  for (const key of Object.keys(args)) {
    if (!(key in props))
      return `parameter "${key}" is not declared in the tool's input schema`;
  }

  // Type check each supplied param.
  for (const [key, value] of Object.entries(args)) {
    const propSchema = props[key];
    if (!propSchema?.type) continue;
    const err = checkJsonSchemaType(key, value, propSchema.type);
    if (err) return err;
  }

  return undefined;
}

// ─── McpBridge class ─────────────────────────────────────────────────────────

const BRIDGE_VERSION = 'yawr.vscode-mcp-bridge/v1';

function httpStatusForBridgeResponse(response: BridgeResponse): number {
  return response.error?.code === 'bridge_disconnected' ? 503 : 200;
}

export interface McpBridgeOptions {
  /** Pre-built registry. Takes precedence over registryDir when both are given. */
  registry?: Record<string, ToolActionSpec>;
  /** Directory to scan for *.tool.yaml definitions. Used when registry is not provided. */
  registryDir?: string;
  /**
   * Tool name overrides keyed by "tool/action". Sourced from the
   * yawr.mcpBridge.toolNameOverrides VS Code setting. Overrides the
   * YAML-derived registered name for live-session corrections without a
   * code change or release.
   */
  overrides?: Record<string, string>;
}

export class McpBridge {
  private readonly secret: string;
  private readonly server: http.Server;
  private readonly inflight = new Map<string, Promise<BridgeResponse>>();
  private readonly activeCancellations = new Map<string, { cancel(): void }>();
  private readonly completed = new Map<string, BridgeResponse>();
  private _port: number;
  private disposed = false;
  private readonly output?: { appendLine(s: string): void };
  private registry: Record<string, ToolActionSpec>;
  private readonly overrides: Record<string, string>;

  private constructor(
    private readonly lm: LmInterface,
    port: number,
    output: { appendLine(s: string): void } | undefined,
    registry: Record<string, ToolActionSpec>,
    overrides: Record<string, string>,
  ) {
    this.secret = crypto.randomBytes(32).toString('hex');
    this._port = port;
    this.output = output;
    this.registry = registry;
    this.overrides = overrides;
    this.server = http.createServer((req, res) => {
      this.dispatch(req, res).catch((err) => {
        if (!res.headersSent) {
          res.writeHead(500);
          res.end();
        }
        this.log(`[mcpBridge] unhandled error in dispatch: ${err instanceof Error ? err.message : String(err)}`);
      });
    });
  }

  // create starts the listener and resolves once it is bound.
  static create(
    lm: LmInterface,
    port = 7779,
    output?: { appendLine(s: string): void },
    options?: McpBridgeOptions,
  ): Promise<McpBridge> {
    // Resolve registry: explicit > dir scan > empty
    let registry: Record<string, ToolActionSpec>;
    if (options?.registry) {
      registry = options.registry;
    } else if (options?.registryDir) {
      registry = buildRegistryFromDir(options.registryDir);
    } else {
      registry = {};
    }
    const overrides = options?.overrides ?? {};

    return new Promise((resolve, reject) => {
      const bridge = new McpBridge(lm, port, output, registry, overrides);
      bridge.server.on('error', reject);
      bridge.server.listen(port, '127.0.0.1', () => {
        const addr = bridge.server.address();
        if (addr && typeof addr === 'object') {
          bridge._port = addr.port;
        }
        resolve(bridge);
      });
    });
  }

  get bridgeToken(): string {
    return this.secret;
  }

  get bridgeUrl(): string {
    return `http://127.0.0.1:${this._port}`;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const source of this.activeCancellations.values()) source.cancel();
    this.activeCancellations.clear();
    this.server.close();
    this.inflight.clear();
    this.completed.clear();
  }

  /**
   * Replace the registry used for all subsequent requests. Call this whenever
   * the active runbook changes so bridge dispatches from the correct project's
   * tool definitions, not the stale snapshot taken at bridge creation.
   */
  updateRegistry(registry: Record<string, ToolActionSpec>): void {
    this.registry = registry;
  }

  private log(msg: string): void {
    this.output?.appendLine(redact(this.secret, msg));
  }

  // dispatch handles one HTTP request: parse, validate, route to handle().
  private async dispatch(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
    if (this.disposed) {
      return this.sendError(res, 503, 'bridge_disconnected', 'Bridge is shutting down', '');
    }

    let bodyText: string;
    try {
      bodyText = await readBody(req);
    } catch {
      return this.sendError(res, 400, 'malformed_request', 'Failed to read request body', '');
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(bodyText);
    } catch {
      return this.sendError(res, 400, 'malformed_request', 'Request body is not valid JSON', '');
    }

    if (this.disposed) {
      return this.sendError(res, 503, 'bridge_disconnected', 'Bridge is shutting down', '');
    }

    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return this.sendError(res, 400, 'malformed_request', 'Request body must be a JSON object', '');
    }

    const body = parsed as Partial<BridgeRequest>;

    if (body.version !== BRIDGE_VERSION) {
      return this.sendError(
        res,
        400,
        'version_mismatch',
        `expected version "${BRIDGE_VERSION}", got "${body.version ?? '(missing)'}"`,
        body.request_id ?? '',
      );
    }

    if (!body.request_id || typeof body.request_id !== 'string') {
      return this.sendError(res, 400, 'malformed_request', 'request_id is required', '');
    }

    const request_id = body.request_id;

    if (!this.checkCapability(body.capability_proof)) {
      return this.sendError(res, 403, 'capability_rejected', 'capability_proof is invalid', request_id);
    }

    if (!body.tool || typeof body.tool !== 'string' ||
        !body.action || typeof body.action !== 'string') {
      return this.sendError(res, 400, 'malformed_request', 'tool and action are required strings', request_id);
    }

    if (body.args !== undefined && (typeof body.args !== 'object' || Array.isArray(body.args))) {
      return this.sendError(res, 400, 'malformed_request', 'args must be a JSON object', request_id);
    }

    const args = (body.args ?? {}) as Record<string, unknown>;

    const cached = this.completed.get(request_id);
    if (cached) {
      return this.sendJson(res, httpStatusForBridgeResponse(cached), cached);
    }

    const inflight = this.inflight.get(request_id);
    if (inflight) {
      const result = await inflight;
      return this.sendJson(res, httpStatusForBridgeResponse(result), result);
    }

    const promise = this.handle({
      version: BRIDGE_VERSION,
      request_id,
      tool: body.tool,
      action: body.action,
      args,
      deadline_unix_ms: typeof body.deadline_unix_ms === 'number' ? body.deadline_unix_ms : undefined,
      capability_proof: body.capability_proof as string,
    });

    this.inflight.set(request_id, promise);
    let result: BridgeResponse;
    try {
      result = await promise;
    } finally {
      this.inflight.delete(request_id);
    }
    this.completed.set(request_id, result);
    return this.sendJson(res, httpStatusForBridgeResponse(result), result);
  }

  // handle performs the authorized MCP invocation for a validated request.
  private async handle(req: BridgeRequest): Promise<BridgeResponse> {
    const spec = resolveSpec(req.tool, req.action, this.registry, this.overrides);
    if (!spec) {
      return this.errorResponse(req.request_id, 'tool_not_found',
        `no tool action registered for "${req.tool}/${req.action}"`);
    }

    // Verify the tool is registered in vscode.lm.tools; name available names
    // so a mismatch is diagnosable without reading source code.
    const toolInfo = this.lm.tools.find((t) => t.name === spec.registeredName);
    if (!toolInfo) {
      const available = this.lm.tools.map((t) => t.name).join(', ') || '(none)';
      return this.errorResponse(req.request_id, 'tool_unavailable',
        `logical action "${req.tool}/${req.action}" resolved to registered name "${spec.registeredName}", ` +
        `which is not present in vscode.lm.tools; available: [${available}]`);
    }

    // Validate adapted args against the tool's live inputSchema before any
    // invocation. Fail-closed: missing required, unknown, or wrong-type params
    // all produce a coded error; no partial arg set is ever sent downstream.
    if (toolInfo.inputSchema) {
      const schemaError = validateArgsAgainstSchema(req.args, toolInfo.inputSchema);
      if (schemaError) {
        return this.errorResponse(req.request_id, 'input_validation_error', schemaError);
      }
    }

    // Set up deadline / cancellation.
    const cts = makeCancellationSource();
    this.activeCancellations.set(req.request_id, cts);
    let deadlineTimer: ReturnType<typeof setTimeout> | undefined;
    let timedOut = false;

    if (req.deadline_unix_ms) {
      const remaining = req.deadline_unix_ms - Date.now();
      if (remaining <= 0) {
        cts.cancel();
        timedOut = true;
      } else {
        deadlineTimer = setTimeout(() => {
          timedOut = true;
          cts.cancel();
        }, remaining);
      }
    }

    // safeMessage: per-category text that is safe to return to Yawr Core.
    // Provider exception text is never forwarded; only the safe string is returned.
    // For provider_unavailable the message is augmented with the allowlisted hint below.
    const safeMessage: Record<InvocationErrorCategory, string> = {
      invocation_token_unavailable: 'MCP tool authorization is not available (token expired or rejected)',
      provider_unavailable:         'the MCP provider is not running',
      authorization_unavailable:    'MCP tool authorization is not available',
      provider_input_rejected:      'MCP tool provider rejected the supplied input',
      invocation_error:             'MCP invocation failed',
    };

    try {
      if (timedOut) {
        return this.errorResponse(req.request_id, 'deadline_exceeded', 'deadline already passed');
      }

      // Fail-closed invocation model:
      //
      //   Attempt 1: invoke with the cached toolInvocationToken.
      //   No attempt 2: absence or rejection of the token is surfaced loudly.
      //
      // The previous fallback retried Canceled failures without
      // a token, which can trigger an interactive VS Code/MCP prompt. That is
      // unsafe for live ICM use and must remain opt-in-only in any future design.
      const cachedToken = this.lm.getToolInvocationToken();
      if (cachedToken === undefined) {
        this.log(`[mcpBridge] invokeTool "${spec.registeredName}": no toolInvocationToken cached — refusing unsafe unauthenticated invocation`);
        return this.errorResponse(req.request_id, 'invocation_token_unavailable', safeMessage.invocation_token_unavailable);
      }

      let raw: LmToolResult;
      try {
        if (this.disposed || cts.token.isCancellationRequested) {
          return this.errorResponse(
            req.request_id,
            this.disposed ? 'bridge_disconnected' : 'deadline_exceeded',
            this.disposed ? 'Bridge is shutting down' : 'invocation timed out',
          );
        }
        raw = await this.lm.invokeTool(
          spec.registeredName,
          { input: req.args, toolInvocationToken: cachedToken },
          cts.token,
        );
        if (this.disposed || cts.token.isCancellationRequested) {
          return this.errorResponse(
            req.request_id,
            this.disposed ? 'bridge_disconnected' : 'deadline_exceeded',
            this.disposed ? 'Bridge is shutting down' : 'invocation timed out',
          );
        }
        this.log(`[mcpBridge] invokeTool "${spec.registeredName}": attempt 1 succeeded`);
      } catch (err: unknown) {
        if (this.disposed) {
          return this.errorResponse(req.request_id, 'bridge_disconnected', 'Bridge is shutting down');
        }
        if (timedOut || cts.token.isCancellationRequested) {
          return this.errorResponse(req.request_id, 'deadline_exceeded', 'invocation timed out');
        }
        const category = isCanceledError(err) ? 'invocation_token_unavailable' : classifyInvocationError(err);
        // If VS Code rejected/canceled the token path, clear it so the operator
        // must re-arm instead of silently falling back to an interactive prompt.
        if (category === 'invocation_token_unavailable') {
          this.lm.onTokenRejected?.();
        }
        // Log safe metadata only: class name + chosen category + request id.
        // Never log the exception message, args, or any result content.
        const className = err instanceof Error ? err.constructor.name : typeof err;
        this.log(`[mcpBridge] invocation error: category=${category} class=${className} request_id=${req.request_id}`);
        // For provider_unavailable, build an actionable message with the allowlisted hint.
        // The hint is assembled from a fixed-phrase allowlist, HTTP status code, and URL
        // origin only — no arbitrary provider text passes through.
        let errMsg = safeMessage[category];
        if (category === 'provider_unavailable') {
          const hint = extractProviderHint(err);
          if (hint) {
            errMsg = `${errMsg} (${hint}). Check \`MCP: List Servers\` in VS Code and re-authenticate the provider.`;
          } else {
            errMsg = `${errMsg}. Check \`MCP: List Servers\` in VS Code and re-authenticate the provider.`;
          }
        }
        return this.errorResponse(req.request_id, category, errMsg);
      }

      const text = extractTextFromResult(raw);
      let parsedResult: unknown;
      try {
        parsedResult = JSON.parse(text);
      } catch {
        return this.errorResponse(req.request_id, 'result_parse_error',
          'MCP result content is not valid JSON');
      }

      const normalized = normalizeResult(parsedResult, spec);
      if (!normalized.ok) {
        return this.errorResponse(req.request_id, 'result_normalization_error', normalized.reason);
      }

      return {
        version: BRIDGE_VERSION,
        request_id: req.request_id,
        result: normalized.value,
      };
    } finally {
      if (deadlineTimer !== undefined) clearTimeout(deadlineTimer);
      if (this.activeCancellations.get(req.request_id) === cts) {
        this.activeCancellations.delete(req.request_id);
      }
    }
  }

  private checkCapability(proof: unknown): boolean {
    if (typeof proof !== 'string') return false;
    const expected = Buffer.from(this.secret, 'utf8');
    const actual = Buffer.from(proof, 'utf8');
    if (actual.length !== expected.length) return false;
    return crypto.timingSafeEqual(actual, expected);
  }

  private errorResponse(request_id: string, code: string, message: string): BridgeResponse {
    return {
      version: BRIDGE_VERSION,
      request_id,
      error: { code, message: redact(this.secret, message) },
    };
  }

  private sendError(
    res: http.ServerResponse,
    status: number,
    code: string,
    message: string,
    request_id: string,
  ): void {
    const body: BridgeResponse = {
      version: BRIDGE_VERSION,
      request_id,
      error: { code, message: redact(this.secret, message) },
    };
    this.sendJson(res, status, body);
  }

  private sendJson(res: http.ServerResponse, status: number, body: BridgeResponse): void {
    const payload = JSON.stringify(body);
    res.writeHead(status, { 'Content-Type': 'application/json' });
    res.end(payload);
  }
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

function readBody(req: http.IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

function makeCancellationSource(): {
  token: LmCancellationToken;
  cancel: () => void;
} {
  let cancelled = false;
  const listeners: Array<() => void> = [];
  const token: LmCancellationToken = {
    get isCancellationRequested() { return cancelled; },
    onCancellationRequested(listener) {
      if (cancelled) { listener(); return { dispose: () => {} }; }
      listeners.push(listener);
      return { dispose: () => { const i = listeners.indexOf(listener); if (i >= 0) listeners.splice(i, 1); } };
    },
  };
  return {
    token,
    cancel() {
      if (cancelled) return;
      cancelled = true;
      for (const l of listeners) l();
    },
  };
}

