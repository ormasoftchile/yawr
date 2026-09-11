import { isScalar, parseDocument, type Scalar } from 'yaml';
import { MAX_CODE_UNITS, type Descriptor, type ResolveReply } from './presentationProtocol';

export interface SourceSpan { start: number; end: number }
export interface MappingUnit { from: number; to: number; spans: SourceSpan[] }
export interface ScalarMapping { text: string; units: MappingUnit[] }
export interface CodeRegion extends ScalarMapping { descriptor: Descriptor }
export interface ColoredSpan extends SourceSpan { color: string; macro?: boolean; expressionClass?: string }

// YAML-generated folding and chomping whitespace has no paintable source unit.
// Escapes stay indivisible, including escapes decoding to surrogate pairs.
export function mapScalar(node: Scalar<string>): ScalarMapping {
  const cst = node.srcToken;
  if (!cst || cst.type === 'block-scalar' && !cst.props.length) throw new Error('incomplete-source');
  const units: MappingUnit[] = [];
  let decoded = '';
  const emit = (value: string, start?: number, end?: number) => {
    if (!value) return;
    units.push({ from: decoded.length, to: decoded.length + value.length,
      spans: start === undefined || end === undefined ? [] : [{ start, end }] });
    decoded += value;
  };
  const direct = (value: string, offset: number) => {
    for (let i = 0; i < value.length;) {
      const char = String.fromCodePoint(value.codePointAt(i)!);
      emit(char, offset + i, offset + i + char.length);
      i += char.length;
    }
  };
  if (cst.type === 'block-scalar') {
    const prop = cst.props[cst.props.length - 1];
    if (!('source' in prop) || !('source' in cst.props[0])) throw new Error('incomplete-source');
    const bodyStart = prop.offset + prop.source.length;
    const header = cst.props[0].source;
    const indicator = Number(header.match(/[1-9]/)?.[0] ?? 0);
    const chomp = header.includes('-') ? '-' : header.includes('+') ? '+' : '';
    let offset = bodyStart;
    const lines = cst.source.split('\n').map(raw => {
      const value = raw.endsWith('\r') ? raw.slice(0, -1) : raw;
      const line = { text: value, offset, indent: value.match(/^ */)![0].length };
      offset += raw.length + 1;
      return line;
    });
    const first = lines.findIndex(line => line.text.trim().length);
    const indent = indicator ? cst.indent + indicator : first < 0 ? cst.indent : lines[first].indent;
    let last = lines.length - 1;
    while (last >= 0 && lines[last].text.slice(indent) === '') last--;
    if (last < 0) {
      if (chomp === '+' && cst.source) emit('\n'.repeat(Math.max(1, lines.length - 1)));
    } else {
      for (let i = 0; i < Math.max(first, 0); i++) {
        direct(lines[i].text.slice(indent), lines[i].offset + indent); emit('\n');
      }
      let separator = '', moreBefore = false;
      for (let i = Math.max(first, 0); i <= last; i++) {
        const line = lines[i], content = line.text.slice(Math.min(indent, line.text.length));
        if (line.text.trim() && line.indent < indent) throw new Error('invalid-source-range');
        const more = line.indent > indent || content.startsWith('\t');
        if (node.type === 'BLOCK_LITERAL') {
          emit(separator); direct(content, line.offset + Math.min(indent, line.text.length)); separator = '\n';
        } else if (more) {
          emit(separator === ' ' ? '\n' : separator === '\n' && !moreBefore ? '\n\n' : separator);
          direct(content, line.offset + indent); separator = '\n'; moreBefore = true;
        } else if (!content) {
          if (separator === '\n') emit('\n'); else separator = '\n';
        } else {
          emit(separator); direct(content, line.offset + indent); separator = ' '; moreBefore = false;
        }
      }
      if (chomp === '+') {
        for (let i = last + 1; i < lines.length; i++) { emit('\n'); direct(lines[i].text.slice(indent), lines[i].offset + indent); }
        if (!decoded.endsWith('\n')) emit('\n');
      } else if (chomp !== '-') emit('\n');
    }
  } else if (cst.type === 'single-quoted-scalar' || cst.type === 'double-quoted-scalar' || cst.type === 'scalar') {
    const raw = cst.source, quoted = cst.type !== 'scalar', double = cst.type === 'double-quoted-scalar';
    const begin = quoted ? 1 : 0, end = raw.length - (quoted ? 1 : 0);
    const escapes: Record<string, string> = { '0': '\0', a: '\x07', b: '\b', e: '\x1b', f: '\f', n: '\n', r: '\r', t: '\t', v: '\v',
      N: '\u0085', _: '\u00a0', L: '\u2028', P: '\u2029', ' ': ' ', '"': '"', '/': '/', '\\': '\\', '\t': '\t' };
    for (let i = begin; i < end;) {
      const start = i;
      if (double && raw[i] === '\\') {
        const next = raw[i + 1];
        if (next === '\n' || next === '\r' && raw[i + 2] === '\n') {
          i += next === '\n' ? 2 : 3;
          while (i < end && /[ \t]/.test(raw[i])) i++;
          continue;
        }
        const digits = ({ x: 2, u: 4, U: 8 } as Record<string, number>)[next];
        if (digits) {
          const hex = raw.slice(i + 2, i + 2 + digits);
          if (!new RegExp(`^[0-9a-fA-F]{${digits}}$`).test(hex) || parseInt(hex, 16) > 0x10ffff) throw new Error('invalid-source-range');
          i += digits + 2; emit(String.fromCodePoint(parseInt(hex, 16)), cst.offset + start, cst.offset + i);
        } else if (Object.hasOwn(escapes, next)) { i += 2; emit(escapes[next], cst.offset + start, cst.offset + i); }
        else throw new Error('invalid-source-range');
      } else if (cst.type === 'single-quoted-scalar' && raw.slice(i, i + 2) === "''") {
        i += 2; emit("'", cst.offset + start, cst.offset + i);
      } else if (raw[i] === '\n' || raw[i] === '\r' && raw[i + 1] === '\n') {
        let breaks = 0;
        while (i < end && /[ \t\r\n]/.test(raw[i])) { if (raw[i] === '\n') breaks++; i++; }
        emit(breaks > 1 ? '\n'.repeat(breaks - 1) : ' ');
      } else if (/[ \t]/.test(raw[i])) {
        while (i < end && /[ \t]/.test(raw[i])) i++;
        if (raw[i] !== '\n' && !(raw[i] === '\r' && raw[i + 1] === '\n')) direct(raw.slice(start, i), cst.offset + start);
      } else {
        const char = String.fromCodePoint(raw.codePointAt(i)!);
        i += char.length; emit(char, cst.offset + start, cst.offset + i);
      }
    }
  } else throw new Error('invalid-source-range');
  if (decoded !== node.value) throw new Error('decoded-source-mismatch');
  return { text: decoded, units };
}

export function resolveScalars(source: string, reply: ResolveReply): { regions: CodeRegion[]; reasons: string[] } {
  const doc = parseDocument(source, { keepSourceTokens: true, prettyErrors: false, strict: true, uniqueKeys: true });
  const regions: CodeRegion[] = [], reasons: string[] = [];
  for (const region of reply.regions) {
    if (region.status !== 'resolved') { reasons.push(region.reason ?? region.status); continue; }
    try {
      const pointer = region.yaml_path.slice(1).split('/').map(part => part.replace(/~1/g, '/').replace(/~0/g, '~'));
      const binding = reply.bindings.find(b => b.id === region.binding_id);
      const toolPath = pointer.slice(0, -2);
      if (pointer[pointer.length - 2] !== 'args' || pointer[pointer.length - 3] !== 'tool' ||
        pointer[pointer.length - 1] !== region.field || doc.getIn([...toolPath, 'name']) !== binding?.name ||
        doc.getIn([...toolPath, 'action']) !== region.action) throw new Error('incomplete-identity');
      const node = doc.getIn(pointer, true);
      if (!isScalar(node) || typeof node.value !== 'string' || !node.range) throw new Error('invalid-source-range');
      if (node.range[0] !== region.range.start || node.range[1] !== region.range.end) throw new Error('invalid-source-range');
      if (doc.errors.some(error => error.code === 'DUPLICATE_KEY' || error.pos[0] <= node.range![2])) throw new Error('incomplete-source');
      const descriptor = binding?.actions.find(a => a.name === region.action)?.arguments.find(f => f.name === region.field)?.presentation;
      if (!descriptor) throw new Error('missing-descriptor');
      if (node.value.length > MAX_CODE_UNITS) throw new Error('limit-exceeded');
      regions.push({ ...mapScalar(node as Scalar<string>), descriptor });
    } catch (error) { reasons.push(error instanceof Error ? error.message : 'invalid-source-range'); }
  }
  return { regions, reasons };
}
export function sourceSpans(region: ScalarMapping, tokens: ColoredSpan[], source: string): ColoredSpan[] {
  const cells = new Map<string, ColoredSpan>();
  for (const token of tokens) {
    let low = 0, high = region.units.length;
    while (low < high) {
      const middle = (low + high) >>> 1;
      if (region.units[middle].to <= token.start) low = middle + 1;
      else high = middle;
    }
    for (let index = low; index < region.units.length && region.units[index].from < token.end; index++) {
      const unit = region.units[index];
      for (const span of unit.spans) {
        const key = `${span.start}:${span.end}`;
        if (!cells.has(key) || token.macro) cells.set(key, { ...token, ...span });
      }
    }
  }
  const output: ColoredSpan[] = [];
  for (const span of [...cells.values()].sort((a, b) => a.start - b.start)) {
    if (/[\r\n]/.test(source.slice(span.start, span.end))) throw new Error('invalid-source-range');
    const previous = output[output.length - 1];
    if (previous?.end === span.start && previous.color === span.color && previous.macro === span.macro && previous.expressionClass === span.expressionClass) previous.end = span.end;
    else output.push({ ...span });
  }
  return output;
}
