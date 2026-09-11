import { createHash } from 'crypto';
import { TextDecoder } from 'util';
import type { ResolveContext, SourceBuffer, Dependency } from './presentationProtocol';

export const AUTHORING_MAX_BYTES = 8388608;
export const operations = ['complete', 'signature', 'required-arguments'] as const;
export type AuthoringOperation = typeof operations[number];
export interface AuthoringRange { start: number; end: number }
export interface AuthoringEdit { range: AuthoringRange; new_text: string }
export interface AuthoringRequest {
  schema_version: 'authoring-request/v3'; request_id: string; operation: AuthoringOperation;
  context: ResolveContext; document: SourceBuffer; overlays: SourceBuffer[]; position: number;
}
export interface AuthoringItem {
  id: string; kind: 'tool' | 'action' | 'argument' | 'namespace' | 'function' | 'variable' | 'typed-key' | 'typed-value' | 'include-mapping' | 'include-key' | 'include-value'; name: string;
  description?: string; value_type?: string; required?: boolean; default_info?: 'absent' | 'declared-redacted';
  edit: AuthoringEdit;
}
export interface AuthoringSignature {
  name: string; label: string; description?: string;
  parameters: { name: string; label_range: AuthoringRange; description?: string }[];
  active_parameter: number | null; call_range: AuthoringRange;
}
export interface RequiredEdit { edit: AuthoringEdit; placeholders: AuthoringRange[] }
export interface AuthoringReply {
  schema_version: 'authoring-reply/v3'; resolver_version: 'core-authoring/v3'; grammar_version: 'yawr-expression/v2';
  operation: AuthoringOperation; request_id: string; context: ResolveContext;
  document: { uri: string; version: number; digest: string };
  status: 'resolved' | 'unavailable' | 'stale'; reason?: string;
  discovery: { scope: 'explicit-local-catalog'; status: 'complete' | 'limited' | 'unavailable' | 'not-needed'; reason?: string };
  dependencies: Dependency[];
  site: { kind: 'tool-reference' | 'tool' | 'action' | 'argument' | 'expression' | 'typed-key' | 'typed-value' | 'include-mapping' | 'include-key' | 'include-value'; yaml_path: string; range: AuthoringRange;
    binding_id?: string; tool_id?: string; tool_digest?: string; action?: string } | null;
  items: AuthoringItem[]; signature: AuthoringSignature | null; required_edit: RequiredEdit | null;
}
const reasons = ['invalid-request', 'incomplete-source', 'incomplete-identity', 'missing-dependency', 'ambiguous-binding',
  'unresolved-dynamic', 'invalid-source-range', 'unsupported-context', 'limit-exceeded', 'stale-request'] as const;
function invalid(): never { throw new Error('invalid-authoring-response'); }
function closed(value: unknown, required: string[], optional: string[] = []): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return invalid();
  const result = value as Record<string, unknown>;
  if (required.some(k => !Object.hasOwn(result, k)) || Object.keys(result).some(k => !required.includes(k) && !optional.includes(k))) return invalid();
  return result;
}
export function validUnicode(value: string): boolean {
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(++i);
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false;
    } else if (code >= 0xdc00 && code <= 0xdfff) return false;
  }
  return true;
}
function text(value: unknown, cap = AUTHORING_MAX_BYTES, empty = false): string {
  if (typeof value !== 'string' || (!empty && !value.length) || value.length > cap || !validUnicode(value)) return invalid();
  return value;
}
function integer(value: unknown): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) return invalid();
  return value;
}
function oneOf<T extends string>(value: unknown, options: readonly T[]): T {
  if (typeof value !== 'string' || !options.includes(value as T)) return invalid();
  return value as T;
}
function list<T>(value: unknown, decode: (item: unknown) => T, cap = 4096): T[] {
  if (!Array.isArray(value) || value.length > cap) return invalid();
  return value.map(decode);
}
function description(v: Record<string, unknown>): { description?: string } {
  return v.description === undefined ? {} : { description: text(v.description, 2048, true) };
}
export const sourceDigest = (source: string): string => 'sha256:' + createHash('sha256').update(source, 'utf8').digest('hex');
function digest(value: unknown): string {
  const result = text(value);
  if (!/^sha256:[a-f0-9]{64}$/.test(result)) return invalid();
  return result;
}
export function validBoundary(source: string, offset: number): boolean {
  return Number.isSafeInteger(offset) && offset >= 0 && offset <= source.length &&
    !(offset > 0 && /[\uD800-\uDBFF]/.test(source[offset - 1]) && /[\uDC00-\uDFFF]/.test(source[offset] ?? ''));
}
function range(value: unknown, source: string, container?: AuthoringRange): AuthoringRange {
  const v = closed(value, ['start', 'end']), start = integer(v.start), end = integer(v.end);
  if (start > end || !validBoundary(source, start) || !validBoundary(source, end) ||
    (container && (start < container.start || end > container.end))) return invalid();
  return { start, end };
}
function edit(value: unknown, source: string, site: AuthoringReply['site']): AuthoringEdit {
  if (!site) return invalid();
  const v = closed(value, ['range', 'new_text']);
  return { range: range(v.range, source, site.range), new_text: text(v.new_text, AUTHORING_MAX_BYTES, true) };
}
function reason(v: Record<string, unknown>, required: boolean): { reason?: string } {
  if (required) return { reason: oneOf(v.reason, reasons) };
  if (v.reason !== undefined) return invalid();
  return {};
}
// Parse the wire before JSON.parse can silently discard duplicate object keys.
export function parseAuthoringJSON(bytes: Buffer): unknown {
  if (bytes.length > AUTHORING_MAX_BYTES) return invalid();
  let source: string;
  try { source = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes); } catch { return invalid(); }
  visitJSONWire(source);
  try { return JSON.parse(source); } catch { return invalid(); }
}

/** Shared bounded wire visitor: decoded keys are checked before object construction. */
export function visitJSONWire(source: string, options: {
  maxDepth?: number;
  onDuplicate?: (key: string, rootKey: string | undefined) => void;
  onRootValue?: (key: string, value: unknown) => void;
} = {}): void {
  let cursor = 0;
  let visits = 0;
  if (source.length > AUTHORING_MAX_BYTES) return invalid();
  const whitespace = () => { while (/[ \t\r\n]/.test(source[cursor] ?? '\0')) cursor++; };
  const string = (): string => {
    const start = cursor++;
    while (cursor < source.length) {
      if (source[cursor] === '\\') { cursor += 2; continue; }
      if (source[cursor++] === '"') {
        try { return text(JSON.parse(source.slice(start, cursor)), AUTHORING_MAX_BYTES, true); } catch { return invalid(); }
      }
    }
    return invalid();
  };
  const primitive = /(?:true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)/y;
  const value = (depth: number, rootKey?: string): void => {
    if (depth > (options.maxDepth ?? 128) || ++visits > source.length) return invalid();
    whitespace();
    if (source[cursor] === '"') {
      const literal = string();
      if (depth === 1 && rootKey !== undefined) options.onRootValue?.(rootKey, literal);
      return;
    }
    const opener = source[cursor];
    if (opener === '{' || opener === '[') {
      cursor++; whitespace();
      const closer = opener === '{' ? '}' : ']', keys = new Set<string>();
      if (source[cursor] === closer) { cursor++; return; }
      for (;;) {
        whitespace();
        let key: string | undefined;
        if (opener === '{') {
          if (source[cursor] !== '"') return invalid();
          key = string();
          if (keys.has(key)) {
            if (!options.onDuplicate) return invalid();
            options.onDuplicate(key, rootKey);
          }
          keys.add(key);
          whitespace(); if (source[cursor++] !== ':') return invalid();
        }
        if (depth === 0 && key !== undefined) options.onRootValue?.(key, undefined);
        value(depth + 1, depth === 0 ? key : rootKey); whitespace();
        if (source[cursor] === closer) { cursor++; return; }
        if (source[cursor++] !== ',') return invalid();
      }
    }
    primitive.lastIndex = cursor;
    const token = primitive.exec(source);
    if (!token) return invalid();
    cursor += token[0].length;
  };
  value(0); whitespace(); if (cursor !== source.length) return invalid();
}
export function decodeAuthoringCapabilities(value: unknown): void {
  const v = closed(value, ['schema_version', 'resolver_version', 'grammar_version', 'operations', 'discovery_scope',
    'max_bytes', 'max_overlays', 'max_items', 'max_value_code_units', 'max_depth',
    'capabilities', 'capture_roots', 'typed_operations']);
  if (v.schema_version !== 'authoring-capabilities/v3' || v.resolver_version !== 'core-authoring/v3' ||
    v.grammar_version !== 'yawr-expression/v2' || v.discovery_scope !== 'explicit-local-catalog' ||
    JSON.stringify(v.operations) !== JSON.stringify(operations) || v.max_bytes !== AUTHORING_MAX_BYTES ||
    v.max_overlays !== 128 || v.max_items !== 4096 || v.max_value_code_units !== 32768 || v.max_depth !== 128) return invalid();
  if (JSON.stringify(v.capabilities) !== '["yawr.typed-results/v1"]' || JSON.stringify(v.capture_roots) !== '["outputs"]') return invalid();
  const typed = list(v.typed_operations, raw => closed(raw, ['kind', 'role', 'terminal'], ['title']));
  if (typed.length !== 2 || typed[0].kind !== 'assign' || typed[0].role !== 'technical' || typed[0].terminal !== false ||
      typed[0].title !== undefined || typed[1].kind !== 'results' || typed[1].role !== 'operator' ||
      typed[1].terminal !== true || typed[1].title !== 'Results') return invalid();
}
export function decodeAuthoringReply(value: unknown, request: AuthoringRequest): AuthoringReply {
  if (Buffer.byteLength(JSON.stringify(value), 'utf8') > AUTHORING_MAX_BYTES) return invalid();
  const v = closed(value, ['schema_version', 'resolver_version', 'grammar_version', 'operation', 'request_id',
    'context', 'document', 'status', 'discovery', 'dependencies', 'site', 'items', 'signature', 'required_edit'], ['reason']);
  if (v.schema_version !== 'authoring-reply/v3' || v.resolver_version !== 'core-authoring/v3' ||
    v.grammar_version !== 'yawr-expression/v2' || v.operation !== request.operation || v.request_id !== request.request_id) return invalid();
  const ctx = closed(v.context, ['project_root', 'generation'], ['package_map_path', 'entrypoint_path', 'package_root']);
  for (const key of ['project_root', 'generation', 'package_map_path', 'entrypoint_path', 'package_root'] as const) {
    if (ctx[key] !== request.context[key]) return invalid();
    if (ctx[key] !== undefined) key === 'generation' ? integer(ctx[key]) : text(ctx[key]);
  }
  const doc = closed(v.document, ['uri', 'version', 'digest']);
  if (doc.uri !== request.document.uri || doc.version !== request.document.version || doc.digest !== sourceDigest(request.document.text)) return invalid();
  const status = oneOf(v.status, ['resolved', 'unavailable', 'stale']);
  reason(v, status !== 'resolved');
  const discovery = closed(v.discovery, ['scope', 'status'], ['reason']);
  oneOf(discovery.scope, ['explicit-local-catalog']);
  const discoveryStatus = oneOf(discovery.status, ['complete', 'limited', 'unavailable', 'not-needed']);
  reason(discovery, discoveryStatus === 'limited' || discoveryStatus === 'unavailable');
  const source = request.document.text;
  let site: AuthoringReply['site'] = null;
  if (v.site !== null) {
    const s = closed(v.site, ['kind', 'yaml_path', 'range'], ['binding_id', 'tool_id', 'tool_digest', 'action']);
    const pointer = text(s.yaml_path);
    if (!pointer.startsWith('/') || /~(?![01])/.test(pointer)) return invalid();
    site = { kind: oneOf(s.kind, ['tool-reference', 'tool', 'action', 'argument', 'expression',
      'include-mapping', 'include-key', 'include-value', 'typed-key', 'typed-value']), yaml_path: pointer, range: range(s.range, source) };
    for (const key of ['binding_id', 'tool_id', 'action'] as const) if (s[key] !== undefined) site[key] = text(s[key]);
    if (s.tool_digest !== undefined) site.tool_digest = digest(s.tool_digest);
  }
  const ids = new Set<string>();
  const items = list(v.items, raw => {
    const i = closed(raw, ['id', 'kind', 'name', 'edit'], ['description', 'value_type', 'required', 'default_info']);
    const id = text(i.id); if (ids.has(id)) return invalid(); ids.add(id);
    const item: AuthoringItem = { id, kind: oneOf(i.kind, ['tool', 'action', 'argument', 'namespace', 'function',
      'include-mapping', 'include-key', 'include-value', 'variable', 'typed-key', 'typed-value']),
      name: text(i.name), edit: edit(i.edit, source, site), ...description(i) };
    if (i.required !== undefined) { if (typeof i.required !== 'boolean') return invalid(); item.required = i.required; }
    if (i.value_type !== undefined) item.value_type = text(i.value_type);
    if (i.default_info !== undefined) item.default_info = oneOf(i.default_info, ['absent', 'declared-redacted'] as const);
    if (site && !({ 'tool-reference': ['tool'], tool: ['tool'], action: ['action'], argument: ['argument'],
      expression: ['namespace', 'function', 'variable'], 'typed-key': ['typed-key'], 'typed-value': ['typed-value'],
      'include-mapping': ['include-mapping'], 'include-key': ['include-key'],
      'include-value': ['include-value'] }[site.kind].includes(item.kind))) return invalid();
    return item;
  });
  for (let i = 1; i < items.length; i++) {
    const a = items[i - 1], b = items[i];
    if (Buffer.compare(Buffer.from(a.name), Buffer.from(b.name)) > 0 ||
      (a.name === b.name && Buffer.compare(Buffer.from(a.id), Buffer.from(b.id)) >= 0)) return invalid();
  }
  let signature: AuthoringSignature | null = null;
  if (v.signature !== null) {
    if (!site || site.kind !== 'expression') return invalid();
    const s = closed(v.signature, ['name', 'label', 'parameters', 'active_parameter', 'call_range'], ['description']);
    const label = text(s.label);
    let end = 0;
    const parameters = list(s.parameters, raw => {
      const p = closed(raw, ['name', 'label_range'], ['description']);
      const label_range = range(p.label_range, label);
      if (label_range.start < end || label_range.start === label_range.end) return invalid();
      end = label_range.end;
      return { name: text(p.name), label_range, ...description(p) };
    });
    const active_parameter = s.active_parameter === null ? null : integer(s.active_parameter);
    if (active_parameter !== null && active_parameter >= parameters.length) return invalid();
    signature = { name: text(s.name), label, parameters, active_parameter, call_range: range(s.call_range, source, site.range), ...description(s) };
  }
  let required_edit: RequiredEdit | null = null;
  if (v.required_edit !== null) {
    if (!site || !['argument', 'tool', 'action'].includes(site.kind)) return invalid();
    const r = closed(v.required_edit, ['edit', 'placeholders']), e = edit(r.edit, source, site);
    let end = 0;
    const placeholders = list(r.placeholders, raw => {
      const p = range(raw, e.new_text);
      if (p.start < end || e.new_text.slice(p.start, p.end) !== 'null') return invalid();
      end = p.end; return p;
    });
    if (!placeholders.length) return invalid();
    required_edit = { edit: e, placeholders };
  }
  if ((request.operation !== 'complete' && items.length) || (request.operation !== 'signature' && signature) ||
    (request.operation !== 'required-arguments' && required_edit) ||
    ((status !== 'resolved' || !site) && (items.length || signature || required_edit))) return invalid();
  const dependencies = list(v.dependencies, raw => {
    const d = closed(raw, ['uri'], ['version', 'digest', 'missing']), uri = text(d.uri);
    if (!/^[a-z][a-z0-9+.-]*:/i.test(uri)) return invalid();
    const result: Dependency = { uri };
    if (d.missing !== undefined) { if (d.missing !== true || d.version !== undefined || d.digest !== undefined) return invalid(); result.missing = true; }
    else result.digest = digest(d.digest);
    if (d.version !== undefined) result.version = integer(d.version);
    const overlay = [request.document, ...request.overlays].find(b => b.uri === uri);
    if (!overlay && result.version !== undefined) return invalid();
    if (overlay && (result.missing || result.version !== overlay.version || result.digest !== sourceDigest(overlay.text))) return invalid();
    return result;
  });
  if (new Set(dependencies.map(d => d.uri)).size !== dependencies.length) return invalid();
  return { ...(v as unknown as AuthoringReply), site, items, signature, required_edit, dependencies };
}
