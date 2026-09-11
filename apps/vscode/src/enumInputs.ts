// enumInputs.ts — pure (no `vscode` import) helpers for AR-CE-2/3 client
// obligations: deciding how to collect a value for one declared runbook
// input, given whatever metadata the engine surfaces.
//
// This module deliberately owns NO schema, NO member-validation logic, and
// NO runbook parsing. It only shapes UI affordances from a DTO the engine
// already produced. The engine remains the sole enum authority
// (AR-CE-4 §1); nothing here decides whether a value is valid.
//
// DTO shape (normative, AR-CE-2 in
// .squad/decisions/inbox/barbara-client-enum-compatibility-ruling.md):
// an `inputs[]` array, each entry carrying `name`, `type`, `required`,
// `default`, `description`, and — when the input is enum-constrained —
// either `enum: string[]` (declared order, verbatim) or, for a redacted
// declaration, `enumRedacted: true` plus `enumMemberCount`. This DTO now
// ships from `yawr preview --format graphjson`
// (pkg/preview/render/graphjson.Document.Inputs, F-1/F-2,
// barbara-client-enum-parity-gate-review.md) via extension.ts's
// `validateInputs`. extractInputDecls still tolerates its absence (an
// older engine binary without the field) per the forward-compatibility
// rule (AR-CE-7 §1) — see enumRuntimeRegression.test.js's
// `TestExtractInputDecls_RealGraphJSON_*` coverage for the live-binary
// path.

/** One declared runbook input, as (eventually) carried by the preview
 * document's `inputs[]` array. All fields are optional so this stays
 * forward-compatible with an engine that has not shipped the DTO yet. */
export interface InputDecl {
  name: string;
  type?: string;
  required?: boolean;
  default?: unknown;
  description?: string;
  enum?: string[];
  enumRedacted?: boolean;
  enumMemberCount?: number;
}

/** Sentinel returned by a prompt when the operator cancelled it. */
export const CANCELLED = Symbol('yawr.input.cancelled');
/** Sentinel returned when the operator explicitly left an optional input
 * unset. This is distinct from the empty string (AR-CE-6 §1). */
export const UNSET = Symbol('yawr.input.unset');

export type Affordance =
  | { kind: 'redacted-freetext'; hint: string }
  | { kind: 'selector'; members: string[]; allowUnset: boolean; preselect?: string }
  | { kind: 'freetext'; defaultValue?: string };

/** nfcEquals compares two strings after NFC normalisation. Used only for
 * selector preselect/echo matching (AR-CE-5 §3) — never for membership
 * validation, and never applied to a value before submission. */
export function nfcEquals(a: string, b: string): boolean {
  return a.normalize('NFC') === b.normalize('NFC');
}

/** chooseAffordance decides how to collect one input's value.
 *
 * - `enumRedacted` (C1): free text only; no member, not even the marker
 *   string, is ever exposed as a value here.
 * - non-redacted `enum` with at least one member: a closed selector over
 *   the members in declared order (AR-CE-3 §1). `default` preselects only
 *   when it is itself a member (AR-CE-6 §2); otherwise nothing preselects.
 * - anything else (no `enum` key at all — the absence rule, AR-CE-2):
 *   mandatory free-text fallback (AR-CE-3 §3).
 */
export function chooseAffordance(decl: InputDecl): Affordance {
  if (decl.enumRedacted) {
    const hint =
      typeof decl.enumMemberCount === 'number'
        ? `one of ${decl.enumMemberCount} permitted values`
        : 'a permitted value';
    return { kind: 'redacted-freetext', hint };
  }
  if (Array.isArray(decl.enum) && decl.enum.length > 0) {
    const preselect =
      typeof decl.default === 'string' && decl.enum.includes(decl.default) ? decl.default : undefined;
    return { kind: 'selector', members: decl.enum.slice(), allowUnset: !decl.required, preselect };
  }
  return { kind: 'freetext', defaultValue: typeof decl.default === 'string' ? decl.default : undefined };
}

/** extractInputDecls reads the `inputs[]` array off a parsed preview
 * document (now shipped by `yawr preview --format graphjson`, F-1/F-2),
 * tolerating its absence (an older engine binary without the field) per
 * the forward-compatibility rule (AR-CE-7 §1): absence means "no metadata
 * available", never "unconstrained". */
export function extractInputDecls(doc: unknown): InputDecl[] | undefined {
  if (!doc || typeof doc !== 'object') return undefined;
  const inputs = (doc as Record<string, unknown>).inputs;
  if (!Array.isArray(inputs)) return undefined;
  return inputs.filter((i): i is InputDecl => !!i && typeof i === 'object' && typeof (i as InputDecl).name === 'string');
}

/** parseVarPairs parses the free-text `key=value,key2=value2` fallback
 * surface used when no input declarations are available at all. Only the
 * separators (`,` between pairs, first `=` inside a pair) are interpreted;
 * the value itself is never trimmed, case-folded, or otherwise normalised
 * (AR-CE-5 §1) — it is passed through to `-var` verbatim. */
export function parseVarPairs(raw: string): Record<string, string> {
  const out: Record<string, string> = {};
  if (!raw) return out;
  for (const pair of raw.split(',')) {
    const idx = pair.indexOf('=');
    if (idx <= 0) continue;
    const key = pair.slice(0, idx).trim();
    const value = pair.slice(idx + 1);
    if (key) out[key] = value;
  }
  return out;
}

/** Shape of the (subset of) fields Node's `child_process` attaches to a
 * rejected `execFile`/`promisify(execFile)` error that we care about. */
export interface ExecFileErrorLike {
  message?: string;
  stderr?: unknown;
}

/** stderrOf extracts the raw stderr text captured by `execFile`, if any. */
export function stderrOf(err: unknown): string {
  if (err && typeof err === 'object' && 'stderr' in err) {
    const s = (err as ExecFileErrorLike).stderr;
    if (typeof s === 'string') return s;
  }
  return '';
}

/** deriveFailureMessage picks the text a client should show for a failed
 * CLI invocation: the engine's own stderr, verbatim, in preference to
 * Node's combined `err.message` (which prefixes the full argv as
 * "Command failed: ..." and can bury a coded error under it). This is the
 * fix for D-3-shaped bugs on the client side — an ENUM-0xx (or any other
 * coded) error must never be flattened into a generic "<command> failed"
 * string (AR-CE-4 §4/§5). */
export function deriveFailureMessage(err: unknown): string {
  const stderrText = stderrOf(err).trim();
  if (stderrText) return stderrText;
  if (err instanceof Error) return err.message;
  return String(err);
}

/** filterRequiredInputs returns only the declared inputs that are required
 * (required === true). Optional inputs (false/absent) are excluded — the
 * caller should only prompt the operator for inputs whose absence would
 * block the run. An input with required: undefined or required: false is
 * treated as optional and excluded. */
export function filterRequiredInputs(decls: InputDecl[]): InputDecl[] {
  return decls.filter((d) => d.required === true);
}

/** firstLine returns the first non-empty line of text, for compact
 * surfacing in a modal/toast while the full text still goes to a log. */
export function firstLine(text: string): string {
  for (const line of text.split(/\r?\n/)) {
    if (line.trim()) return line;
  }
  return text;
}

/** warningLines extracts every `yawr: warning: ...` line (e.g. ENUM-W001)
 * from a CLI stderr capture, verbatim, code included. */
export function warningLines(stderrText: string): string[] {
  if (!stderrText) return [];
  return stderrText.split(/\r?\n/).filter((line) => line.startsWith('yawr: warning:'));
}
