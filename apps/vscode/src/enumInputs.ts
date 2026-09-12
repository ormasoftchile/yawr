// enumInputs.ts — pure (no `vscode` import) helpers for deciding how to
// collect a value for one declared runbook input from current engine metadata.
//
// This module deliberately owns NO schema, NO member-validation logic, and
// NO runbook parsing. It only shapes UI affordances from a DTO the engine
// already produced. The engine remains the sole enum authority; nothing here
// decides whether a value is valid.
//
// The preview document carries an `inputs[]` array. Each entry has `name`,
// `type`, `required`,
// `default`, `description`, and — when the input is enum-constrained —
// either `enum: string[]` (declared order, verbatim) or, for a redacted
// declaration, `enumRedacted: true` plus `enumMemberCount`.

/** One declared runbook input carried by the preview document's `inputs[]` array. */
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

/** Reads the required `inputs[]` array from a current graphjson document. */
export function extractInputDecls(doc: unknown): InputDecl[] {
  if (!doc || typeof doc !== 'object') throw new Error('invalid-preview-inputs');
  const inputs = (doc as Record<string, unknown>).inputs;
  if (!Array.isArray(inputs)) throw new Error('invalid-preview-inputs');
  for (const input of inputs) {
    if (!input || typeof input !== 'object' || typeof (input as InputDecl).name !== 'string') {
      throw new Error('invalid-preview-inputs');
    }
  }
  return inputs as InputDecl[];
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
