import { MAX_CODE_UNITS, MAX_SOURCE_BYTES, record, type ResolveRequest, type ResolveContext } from './presentationProtocol';

export const EXPRESSION_GRAMMAR = 'yawr-expression/v2';
export type ExpressionGrammar = typeof EXPRESSION_GRAMMAR;
export const EXPRESSION_CLASSES = ['variable', 'property', 'namespace', 'function', 'keyword', 'string', 'number',
  'boolean', 'null', 'operator', 'delimiter', 'interpolation', 'comment'] as const;
export type ExpressionClass = typeof EXPRESSION_CLASSES[number];
export type ExpressionMode = 'gxl' | 'gis' | 'regex';
export interface ExpressionToken { start: number; end: number; class: ExpressionClass }
export interface ExpressionValue { mode: ExpressionMode; text_length: number; text_digest: string; tokens: ExpressionToken[] }
export interface ExpressionRegion extends ExpressionValue { yaml_path: string; range: { start: number; end: number } }
export interface ExpressionDetailValue extends ExpressionValue { path: string }
export interface ExpressionPresentation { version: 1; grammar_version: ExpressionGrammar; values: ExpressionDetailValue[] }
export type ExpressionResolveRequest = Omit<ResolveRequest, 'schema_version'> & { schema_version: 'yawr.expression-resolve/v1' };
export interface ExpressionResolveReply {
  schema_version: 'yawr.expression-resolve/v1'; resolver_version: 'yawr.core-expression/v1'; grammar_version: ExpressionGrammar;
  request_id: string; context: ResolveContext; document: { uri: string; version: number };
  status: 'resolved' | 'unavailable'; reason?: 'incomplete-source' | 'invalid-request' | 'limit-exceeded'; regions: ExpressionRegion[];
}
function closed(value: unknown, keys: string[]) {
  const v = record(value);
  if (Object.keys(v).some(k => !keys.includes(k))) throw new Error('unknown-expression-field');
  return v;
}
function integer(value: unknown): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) throw new Error('invalid-expression-offset');
  return value;
}
export function expressionPointer(value: unknown): string {
  if (typeof value !== 'string' || !value.startsWith('/') || /~(?![01])/u.test(value) || value.split('/').length > 129) throw new Error('invalid-expression-pointer');
  return value;
}
function bounded(value: unknown): void {
  if (new TextEncoder().encode(JSON.stringify(value)).length > MAX_SOURCE_BYTES) throw new Error('limit-exceeded');
}
function array(value: unknown, cap: number): unknown[] {
  if (!Array.isArray(value) || value.length > cap) throw new Error('limit-exceeded');
  return value;
}
const valueKeys = ['mode', 'text_length', 'text_digest', 'tokens'];
function supportedGrammar(value: unknown): value is ExpressionGrammar {
  return value === EXPRESSION_GRAMMAR;
}
function decodeValue(v: Record<string, unknown>, grammar: ExpressionGrammar): ExpressionValue {
  const text_length = integer(v.text_length);
  if (text_length > MAX_CODE_UNITS || (v.mode !== 'gxl' && v.mode !== 'gis' && !(grammar === EXPRESSION_GRAMMAR && v.mode === 'regex')) ||
    typeof v.text_digest !== 'string' || !/^sha256:[a-f0-9]{64}$/.test(v.text_digest)) throw new Error('invalid-expression-value');
  let end = 0;
  const tokens = array(v.tokens, 65536).map(item => {
    const token = closed(item, ['start', 'end', 'class']);
    const start = integer(token.start), stop = integer(token.end);
    if (start < end || stop <= start || stop > text_length || !EXPRESSION_CLASSES.includes(token.class as ExpressionClass)) throw new Error('invalid-expression-token');
    end = stop;
    return { start, end: stop, class: token.class as ExpressionClass };
  });
  return { mode: v.mode, text_length, text_digest: v.text_digest, tokens };
}
function tokenBudget(values: ExpressionValue[]): void {
  if (values.reduce((sum, v) => sum + v.tokens.length, 0) > 65536) throw new Error('limit-exceeded');
}
export function decodeExpressionPresentation(value: unknown): ExpressionPresentation | undefined {
  try {
    bounded(value);
    const v = closed(value, ['version', 'grammar_version', 'values']);
    if (v.version !== 1 || !supportedGrammar(v.grammar_version)) return undefined;
    const grammar = v.grammar_version;
    const values = array(v.values, 4096).map(item => {
      const entry = closed(item, [...valueKeys, 'path']);
      return { ...decodeValue(entry, grammar), path: expressionPointer(entry.path) };
    });
    tokenBudget(values);
    if (new Set(values.map(v => v.path)).size !== values.length) return undefined;
    return { version: 1, grammar_version: grammar, values };
  } catch { return undefined; }
}
export function decodeExpressionCapabilities(value: unknown): void {
  bounded(value);
  const v = closed(value, ['schema_version', 'resolver_version', 'grammar_version', 'modes', 'max_value_code_units', 'max_regions', 'max_tokens']);
  if (typeof v.schema_version !== 'string' || typeof v.resolver_version !== 'string' || typeof v.grammar_version !== 'string' ||
    !Array.isArray(v.modes) || !v.modes.every(mode => typeof mode === 'string') ||
    ![v.max_value_code_units, v.max_regions, v.max_tokens].every(limit => typeof limit === 'number' && Number.isSafeInteger(limit) && limit > 0)) {
    throw new Error('invalid-expression-capabilities');
  }
  const suppliedModes = v.modes;
  const modes = ['gxl', 'gis', 'regex'];
  if (v.schema_version !== 'yawr.expression-capabilities/v1' || v.resolver_version !== 'yawr.core-expression/v1') {
    throw new Error('invalid-expression-capabilities');
  }
  if (!supportedGrammar(v.grammar_version) || suppliedModes.length !== modes.length ||
    !modes.every(mode => suppliedModes.includes(mode)) || v.max_value_code_units !== 32768 ||
    v.max_regions !== 4096 || v.max_tokens !== 65536) throw new Error('unsupported-expression-capabilities');
}
export function decodeExpressionReply(value: unknown, request: ExpressionResolveRequest): ExpressionResolveReply {
  bounded(value);
  const v = closed(value, ['schema_version', 'resolver_version', 'grammar_version', 'request_id', 'context', 'document', 'status', 'reason', 'regions']);
  if (v.schema_version !== request.schema_version || v.resolver_version !== 'yawr.core-expression/v1' || !supportedGrammar(v.grammar_version)) throw new Error('unsupported-expression-protocol');
  const grammar = v.grammar_version;
  const context = closed(v.context, ['project_root', 'generation', 'package_map_path', 'entrypoint_path', 'package_root']);
  const document = closed(v.document, ['uri', 'version']);
  if (v.request_id !== request.request_id || document.uri !== request.document.uri || document.version !== request.document.version ||
    Object.keys(context).length !== Object.keys(request.context).length ||
    Object.entries(request.context).some(([k, val]) => context[k] !== val)) throw new Error('stale-request');
  if (v.status !== 'resolved' && v.status !== 'unavailable' ||
    v.reason !== undefined && !['incomplete-source', 'invalid-request', 'limit-exceeded'].includes(v.reason as string) ||
    v.status === 'unavailable' && v.reason === undefined) throw new Error('invalid-expression-status');
  let end = 0;
  const regions = array(v.regions, 4096).map(item => {
    const region = closed(item, [...valueKeys, 'yaml_path', 'range']);
    const range = closed(region.range, ['start', 'end']);
    const start = integer(range.start), stop = integer(range.end);
    if (start < end || stop <= start || stop > request.document.text.length) throw new Error('invalid-expression-range');
    end = stop;
    return { ...decodeValue(region, grammar), yaml_path: expressionPointer(region.yaml_path), range: { start, end: stop } };
  });
  tokenBudget(regions);
  if (v.status === 'unavailable' && regions.length || new Set(regions.map(r => r.yaml_path)).size !== regions.length) throw new Error('invalid-expression-regions');
  return { schema_version: request.schema_version, resolver_version: 'yawr.core-expression/v1', grammar_version: grammar,
    request_id: request.request_id, context: request.context, document: { uri: request.document.uri, version: request.document.version },
    status: v.status, ...(v.reason ? { reason: v.reason as ExpressionResolveReply['reason'] } : {}), regions };
}
export function expressionTextMatches(text: string, value: ExpressionValue, digest: string): boolean {
  const boundary = (offset: number) => !(offset > 0 && offset < text.length &&
    /[\uD800-\uDBFF]/.test(text[offset - 1]) && /[\uDC00-\uDFFF]/.test(text[offset]));
  return text.length === value.text_length && digest === value.text_digest &&
    value.tokens.every(t => boundary(t.start) && boundary(t.end));
}
export function pointerParts(pointer: string): string[] {
  return expressionPointer(pointer).slice(1).split('/').map(p => p.replace(/~1/g, '/').replace(/~0/g, '~'));
}
export function safeExpressionText(root: unknown, pointer: string): string | undefined {
  try {
    const parts = pointerParts(pointer);
    let value = root;
    for (const [index, key] of parts.entries()) {
      if (!value || typeof value !== 'object' || !Object.hasOwn(value, key)) return undefined;
      if (Array.isArray(value) && !/^(?:0|[1-9]\d*)$/.test(key)) return undefined;
      // Only StepDetails' serialized NamedDetailValue arrays own this flag.
      // Their value descendants are arbitrary authored data, not typed records.
      if (index === 2 && ['arguments', 'bindings', 'request', 'filter', 'collect'].includes(parts[0]) &&
        Array.isArray((root as Record<string, unknown>)[parts[0]]) &&
        (value as Record<string, unknown>).redacted === true) return undefined;
      value = (value as Record<string, unknown>)[key];
    }
    return typeof value === 'string' && !/(?:\[(?:redacted|sensitive|truncated)\]|<(?:redacted|sensitive|truncated)>)/i.test(value) ? value : undefined;
  } catch { return undefined; }
}
