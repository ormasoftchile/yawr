export const MAX_RESULTS_BYTES = 256 * 1024 * 1024;
function stringJSON(value: string): string {
  for (const char of value) {
    const unit = char.charCodeAt(0);
    if (char.length === 1 && unit >= 0xd800 && unit <= 0xdfff) throw new Error('invalid-results-unicode');
  }
  return JSON.stringify(value).replace(/[<>&\u2028\u2029]/g, char =>
    `\\u${char.charCodeAt(0).toString(16).padStart(4, '0')}`);
}
/** UTF-8 (not UTF-16) key ordering and Go JSON escaping on both hosts. */
export function canonicalResultsJSON(value: unknown, pretty = false): string {
  const utf8 = new TextEncoder();
  let bytes = 0;
  const budget = (size: number) => {
    bytes += size;
    if (bytes > MAX_RESULTS_BYTES) throw new Error('results-budget-exceeded');
  };
  const literal = (text: string): string => { budget(utf8.encode(text).length); return text; };
  const compare = (a: string, b: string): number => {
    const x = utf8.encode(a), y = utf8.encode(b);
    for (let i = 0; i < Math.min(x.length, y.length); i++) if (x[i] !== y[i]) return x[i] - y[i];
    return x.length - y.length;
  };
  function encode(value: unknown, depth: number): string {
    if (depth > 128) throw new Error('results-depth-exceeded');
    if (value === null) return literal('null');
    if (typeof value === 'string') return literal(stringJSON(value));
    if (typeof value === 'boolean') return literal(value ? 'true' : 'false');
    if (typeof value === 'number') {
      if (!Number.isFinite(value)) throw new Error('invalid-results-number');
      return literal(Object.is(value, -0) ? '-0' : JSON.stringify(value));
    }
    const pad = pretty ? '\n' + '  '.repeat(depth + 1) : '';
    const close = pretty ? '\n' + '  '.repeat(depth) : '';
    if (Array.isArray(value)) {
      budget(value.length ? 2 + pad.length + close.length + (value.length - 1) * (1 + pad.length) : 2);
      return value.length ? `[${pad}${value.map(item => encode(item, depth + 1)).join(',' + pad)}${close}]` : '[]';
    }
    if (!value || typeof value !== 'object') throw new Error('invalid-results-object');
    const object = value as Record<string, unknown>;
    const keys = Object.keys(object).sort(compare);
    budget(keys.length ? 2 + pad.length + close.length + (keys.length - 1) * (1 + pad.length) + keys.length * (pretty ? 2 : 1) : 2);
    return keys.length ? `{${pad}${keys.map(key => `${literal(stringJSON(key))}:${pretty ? ' ' : ''}${encode(object[key], depth + 1)}`).join(',' + pad)}${close}}` : '{}';
  }
  const result = encode(value, 0);
  return result;
}
