export const MAX_SOURCE_BYTES = 8 * 1024 * 1024;
export const MAX_CODE_UNITS = 32768;
export const statuses = ['resolved', 'unavailable', 'ambiguous', 'unsupported', 'stale'] as const;
export const reasons = ['missing-dependency', 'missing-descriptor', 'incomplete-identity', 'invalid-descriptor',
  'ambiguous-binding', 'incomplete-source', 'unsupported-language', 'unsupported-version', 'unresolved-dynamic',
  'invalid-source-range', 'stale-request', 'limit-exceeded', 'invalid-request'] as const;
export type Status = typeof statuses[number];
export type Reason = typeof reasons[number];
export interface Descriptor { version: number; kind: 'code'; language: string }
export interface Availability { status: Status; reason?: Reason }
export interface PresentationField extends Availability { name: string; value_type: string; presentation?: Descriptor }
export interface PresentationEnvelope extends Availability {
  version: number; origin: 'current' | 'frozen'; tool_id?: string; tool_digest?: string; action?: string;
  plan_snapshot_digest?: string; arguments: PresentationField[]; outputs: PresentationField[];
}
export interface SourceBuffer { uri: string; path: string; version: number; text: string }
export interface ResolveContext {
  project_root: string; generation: number; package_map_path?: string; entrypoint_path?: string; package_root?: string;
}
export interface ResolveRequest {
  schema_version: 'yawr.presentation-resolve/v1'; request_id: string; context: ResolveContext;
  document: SourceBuffer; overlays: SourceBuffer[];
}
export interface Binding extends Availability {
  id: string; name: string; tool_id?: string; tool_digest?: string; source_uri?: string;
  actions: { name: string; arguments: PresentationField[]; outputs: PresentationField[] }[];
}
export interface Region extends Availability {
  binding_id: string; action: string; direction: 'input'; field: string; yaml_path: string;
  range: { start: number; end: number };
}
export interface Dependency { uri: string; version?: number; digest?: string; missing?: true }
export interface ResolveReply extends Availability {
  schema_version: 'yawr.presentation-resolve/v1'; resolver_version: 'yawr.core-binding/v1'; request_id: string;
  context: ResolveContext; document: { uri: string; version: number };
  bindings: Binding[]; regions: Region[]; dependencies: Dependency[];
}
export type ValueStatus = 'available' | 'absent' | 'redacted' | 'truncated' | 'unavailable';
export interface SafeCodeValue { name: string; text?: string; status: ValueStatus; descriptor?: Descriptor }

export function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('invalid-presentation-object');
  return value as Record<string, unknown>;
}
function closed(value: unknown, keys: string[]): Record<string, unknown> {
  const result = record(value);
  if (Object.keys(result).some(key => !keys.includes(key))) throw new Error('unknown-presentation-field');
  return result;
}
function text(value: unknown): string {
  if (typeof value !== 'string' || !value.length) throw new Error('invalid-presentation-string');
  return value;
}
function integer(value: unknown): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) throw new Error('invalid-presentation-integer');
  return value;
}
function literal<T extends string>(value: unknown, values: readonly T[]): T {
  if (typeof value !== 'string' || !values.includes(value as T)) throw new Error('invalid-presentation-enum');
  return value as T;
}
function list<T>(value: unknown, decode: (item: unknown) => T, cap = 4096): T[] {
  if (!Array.isArray(value) || value.length > cap) throw new Error('invalid-presentation-list');
  return value.map(decode);
}
function optionalText(value: unknown): string | undefined { return value === undefined ? undefined : text(value); }
function availability(value: Record<string, unknown>): Availability {
  const status = literal(value.status, statuses);
  const reason = value.reason === undefined ? undefined : literal(value.reason, reasons);
  if (status !== 'resolved' && !reason) throw new Error('missing-presentation-reason');
  return { status, ...(reason ? { reason } : {}) };
}
export function decodeDescriptor(value: unknown): Descriptor {
  const v = closed(value, ['version', 'kind', 'language']);
  const version = integer(v.version), language = text(v.language);
  if (!version || !/^[a-z][a-z0-9-]{0,31}$/.test(language)) throw new Error('invalid-presentation-descriptor');
  return { version, language, kind: literal(v.kind, ['code']) };
}
export function supportedDescriptor(value: Descriptor | undefined): value is Descriptor {
  return !!value && value.version === 1 && ['sql', 'kql', 'powershell'].includes(value.language);
}
function field(value: unknown): PresentationField {
  const v = closed(value, ['name', 'value_type', 'status', 'reason', 'presentation']);
  const result: PresentationField = { name: text(v.name), value_type: text(v.value_type), ...availability(v),
    ...(v.presentation === undefined ? {} : { presentation: decodeDescriptor(v.presentation) }) };
  if (result.presentation && result.value_type !== 'string') throw new Error('invalid-presentation-type');
  if (result.status === 'resolved' && !supportedDescriptor(result.presentation)) throw new Error('invalid-resolved-descriptor');
  return result;
}
function fields(value: unknown): PresentationField[] {
  const result = list(value, field);
  if (new Set(result.map(f => f.name)).size !== result.length) throw new Error('duplicate-presentation-field');
  return result;
}
function identity(v: Record<string, unknown>, requireAction: boolean): { tool_id?: string; tool_digest?: string; action?: string } {
  const tool_id = optionalText(v.tool_id), tool_digest = optionalText(v.tool_digest), action = optionalText(v.action);
  if (tool_digest && !/^sha256:[a-f0-9]{64}$/.test(tool_digest)) throw new Error('invalid-tool-digest');
  if (v.status === 'resolved' && (!tool_id || !tool_digest || (requireAction && !action))) throw new Error('incomplete-presentation-identity');
  return { ...(tool_id ? { tool_id } : {}), ...(tool_digest ? { tool_digest } : {}), ...(action ? { action } : {}) };
}
export function decodeEnvelope(value: unknown): PresentationEnvelope {
  const v = closed(value, ['version', 'status', 'reason', 'origin', 'tool_id', 'tool_digest', 'action', 'plan_snapshot_digest', 'arguments', 'outputs']);
  const version = integer(v.version);
  if (!version) throw new Error('unsupported-presentation-envelope');
  const origin = literal(v.origin, ['current', 'frozen']);
  const digest = optionalText(v.plan_snapshot_digest);
  if (origin === 'frozen' && !digest) throw new Error('missing-frozen-digest');
  if (digest && !/^sha256:[a-f0-9]{64}$/.test(digest)) throw new Error('invalid-frozen-digest');
  const fallback = (values: unknown): PresentationField[] => fields(values).map(field => version === 1 ? field :
    { ...field, status: 'unsupported', reason: 'unsupported-version' });
  return { version, origin, ...availability(v), ...identity(v, true),
    ...(version === 1 ? {} : { status: 'unsupported', reason: 'unsupported-version' }),
    ...(digest ? { plan_snapshot_digest: digest } : {}), arguments: fallback(v.arguments), outputs: fallback(v.outputs) };
}
function context(value: unknown): ResolveContext {
  const v = closed(value, ['project_root', 'generation', 'package_map_path', 'entrypoint_path', 'package_root']);
  return { project_root: text(v.project_root), generation: integer(v.generation),
    ...(v.package_map_path === undefined ? {} : { package_map_path: text(v.package_map_path) }),
    ...(v.entrypoint_path === undefined ? {} : { entrypoint_path: text(v.entrypoint_path) }),
    ...(v.package_root === undefined ? {} : { package_root: text(v.package_root) }) };
}
function decodeBinding(value: unknown): Binding {
  const v = closed(value, ['id', 'name', 'status', 'reason', 'tool_id', 'tool_digest', 'source_uri', 'actions']);
  const actions = list(v.actions, item => {
    const a = closed(item, ['name', 'arguments', 'outputs']);
    return { name: text(a.name), arguments: fields(a.arguments), outputs: fields(a.outputs) };
  });
  if (new Set(actions.map(a => a.name)).size !== actions.length) throw new Error('duplicate-presentation-action');
  return { id: text(v.id), name: text(v.name), ...availability(v), ...identity(v, false),
    ...(v.source_uri === undefined ? {} : { source_uri: text(v.source_uri) }), actions };
}
function decodeRegion(value: unknown): Region {
  const v = closed(value, ['binding_id', 'action', 'direction', 'field', 'yaml_path', 'range', 'status', 'reason']);
  const r = closed(v.range, ['start', 'end']);
  const start = integer(r.start), end = integer(r.end);
  if (end <= start) throw new Error('invalid-source-range');
  const yaml_path = text(v.yaml_path);
  if (!yaml_path.startsWith('/') || /~(?![01])/u.test(yaml_path)) throw new Error('invalid-yaml-pointer');
  return { binding_id: text(v.binding_id), action: text(v.action), direction: literal(v.direction, ['input']),
    field: text(v.field), yaml_path, range: { start, end }, ...availability(v) };
}
export function decodeReply(value: unknown, request: ResolveRequest): ResolveReply {
  const v = closed(value, ['schema_version', 'resolver_version', 'request_id', 'context', 'document', 'status', 'reason', 'bindings', 'regions', 'dependencies']);
  if (v.schema_version !== request.schema_version || v.resolver_version !== 'yawr.core-binding/v1' || v.request_id !== request.request_id) throw new Error('stale-request');
  const ctx = context(v.context);
  for (const key of ['project_root', 'generation', 'package_map_path', 'entrypoint_path', 'package_root'] as const) {
    if (ctx[key] !== request.context[key]) throw new Error('stale-request');
  }
  const doc = closed(v.document, ['uri', 'version']);
  if (doc.uri !== request.document.uri || doc.version !== request.document.version) throw new Error('stale-request');
  const bindings = list(v.bindings, decodeBinding), regions = list(v.regions, decodeRegion);
  if (new Set(bindings.map(b => b.id)).size !== bindings.length ||
    bindings.some(b => !/^b(?:0|[1-9]\d*)$/.test(b.id) || Number(b.id.slice(1)) >= 4096)) throw new Error('invalid-binding-id');
  let previousEnd = 0;
  for (const region of regions) {
    const { start, end } = region.range;
    const split = (offset: number) => offset > 0 && /[\uD800-\uDBFF]/.test(request.document.text[offset - 1]) && /[\uDC00-\uDFFF]/.test(request.document.text[offset] ?? '');
    if (start < previousEnd || end > request.document.text.length || split(start) || split(end)) throw new Error('invalid-source-range');
    previousEnd = end;
    const binding = bindings.find(b => b.id === region.binding_id);
    const selected = binding?.actions.find(a => a.name === region.action)?.arguments.find(f => f.name === region.field);
    if (!binding || !selected || (region.status === 'resolved' && (binding.status !== 'resolved' || selected.status !== 'resolved'))) throw new Error('invalid-region-identity');
  }
  const dependencies = list(v.dependencies, item => {
    const d = closed(item, ['uri', 'version', 'digest', 'missing']);
    if (d.missing !== undefined && d.missing !== true) throw new Error('invalid-dependency');
    const uri = text(d.uri);
    if (!/^[a-z][a-z0-9+.-]*:/i.test(uri)) throw new Error('invalid-dependency-uri');
    const overlay = [request.document, ...request.overlays].find(b => b.uri === uri);
    const version = d.version === undefined ? undefined : integer(d.version);
    if (overlay && version !== overlay.version) throw new Error('stale-request');
    const digest = d.digest === undefined ? undefined : text(d.digest);
    if (d.missing === true ? version !== undefined || digest !== undefined : !digest || !/^sha256:[a-f0-9]{64}$/.test(digest)) throw new Error('invalid-dependency-digest');
    return { uri, ...(version === undefined ? {} : { version }), ...(d.digest === undefined ? {} : { digest: text(d.digest) }),
      ...(d.missing === true ? { missing: true as const } : {}) };
  }, 16384);
  return { schema_version: 'yawr.presentation-resolve/v1', resolver_version: 'yawr.core-binding/v1', request_id: request.request_id,
    context: ctx, document: { uri: request.document.uri, version: request.document.version }, ...availability(v), bindings, regions, dependencies };
}
export function decodeCapabilities(value: unknown, version: 1 | 3 = 1): void {
  const v = closed(value, ['schema_version', 'resolver_version', 'execution_plan_read', 'execution_plan_write',
    ...(version === 3 ? ['capabilities', 'graph_read', 'authoring_request', 'expression_request', 'stdio_version',
      'stdio_frame_bytes', 'results_chunk_bytes', 'results_max_bytes'] : [])]);
  const schemaVersion = version === 1 ? 'yawr.presentation-capabilities/v1' : `presentation-capabilities/v${version}`;
  const resolverVersion = version === 1 ? 'yawr.core-binding/v1' : `core-binding/v${version}`;
  if (v.schema_version !== schemaVersion || v.resolver_version !== resolverVersion) throw new Error('incompatible-presentation-helper');
  for (const key of ['execution_plan_read', 'execution_plan_write']) {
    const expected = ['execution-plan/v3'];
    const versions = list(v[key], text, expected.length);
    if (versions.length !== expected.length || new Set(versions).size !== expected.length ||
        !expected.every(item => versions.includes(item))) throw new Error('incompatible-execution-runtime');
  }
  if (version === 3) {
    const features = list(v.capabilities, text), graphs = list(v.graph_read, text);
    if (!['yawr.typed-results/v1', 'yawr.run-results-chunks/v1'].every(item => features.includes(item)) ||
        new Set(features).size !== features.length || graphs.length !== 2 || !graphs.includes('1') || !graphs.includes('3') ||
        v.authoring_request !== 'authoring-request/v3' || v.expression_request !== 'yawr.expression-resolve/v1' ||
        v.stdio_version !== 'yawr.stdio/v1' || v.stdio_frame_bytes !== 1048576 ||
        v.results_chunk_bytes !== 65536 || v.results_max_bytes !== 268435456) throw new Error('incompatible-execution-runtime');
  }
}
export function safeOutputs(envelope: PresentationEnvelope, output: unknown, status: unknown): SafeCodeValue[] {
  const values = output && typeof output === 'object' && !Array.isArray(output) ? record(output) : {};
  const states = status && typeof status === 'object' && !Array.isArray(status) ? record(status) : {};
  return envelope.outputs.filter(f => f.presentation).map(f => {
    const raw = states[f.name];
    const state: ValueStatus = raw === 'available' || raw === 'absent' || raw === 'redacted' || raw === 'truncated' ? raw : 'unavailable';
    const value = values[f.name];
    const approved = (state === 'available' || state === 'truncated') && typeof value === 'string';
    return { name: f.name, status: approved ? state : state === 'available' || state === 'truncated' ? 'unavailable' : state,
      ...(approved ? { text: value } : {}), ...(f.status === 'resolved' ? { descriptor: f.presentation } : {}) };
  });
}
export function decodeOutputStatuses(envelope: PresentationEnvelope, value: unknown): Record<string, ValueStatus> {
  if (value === undefined) return {};
  const object = record(value), result: Record<string, ValueStatus> = {};
  const names = new Set(envelope.outputs.filter(f => f.presentation).map(f => f.name));
  for (const [name, state] of Object.entries(object)) {
    if (!names.has(name)) throw new Error('unknown-output-classification');
    Object.defineProperty(result, name, { value: literal(state, ['available', 'absent', 'redacted', 'truncated', 'unavailable']),
      enumerable: true, configurable: true, writable: true });
  }
  return result;
}
