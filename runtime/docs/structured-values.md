# Structured runbook composition

Runbook-backed actions can collect structured child results and export them
without converting them to strings. These capabilities are opt-in:
existing `iterate.collect` and output `value` retain their string-template
and capture-path behavior.

## Typed collection

`collect_values` accepts JSON-like YAML values. After each iteration, Yawr
recursively evaluates them against that iteration's variables:

```yaml
- iterate:
    id: configurations
    over: configurations
    as: configuration
    steps:
      - step:
          id: lookup
          type: tool
          tool:
            name: observations
            action: lookup
            args: {id: '${configuration.id}'}
          capture:
            rows: outputs.rows
            lookup_status: outputs.status
    collect_values:
      results:
        configuration: '${configuration}'
        status: '${lookup_status}'
        rows: '${rows}'
        verified: true
```

A string consisting of exactly one `${expression}` keeps the GXL result's
type: array, object, number, boolean, string, or null. Literal YAML collections
are copied recursively. Mixed templates such as `item-${iteration}` remain
strings. Missing required paths and expression errors fail the step.

Every `collect_values` destination produces an array, including `[]` for zero
iterations. All collections preserve input iteration order even with
`concurrency > 1`. A destination cannot occur in both maps.

Collection sees the existing iteration scope, including child captures.
Conditional bodies must explicitly initialize their outputs or use a
per-item substituted runbook to isolate each item's variables; do not interpret
a previous iteration's capture as a new observation. The executable
`gather.runbook.yaml` resets status to `missing` and rows to `[]` before its
conditional branch. It demonstrates skipped work immediately after a nonempty
rowset. Use an explicit `branch` condition in these examples: the existing
generic step `when` dispatch behavior is not a validated execution gate.

## Typed exports

Declare an output using `value_expr`, a **raw GXL expression**, without `${}`:

```yaml
outputs:
  observations:
    type: array
    value_expr: results
```

The output type, enum, optional/required contract, and exact action/runbook
signature are enforced as before. `value` and `value_expr` are mutually exclusive.
This also exports another child's structured output: capture `outputs.rows`
on the child tool step, then use that captured variable in `value_expr`.
It does **not** introduce a `step.<id>.outputs` capture namespace.

The Go `expr.Evaluator` interface is unchanged. Custom evaluators supporting
these opt-in fields implement `expr.ValueEvaluator.EvalValue`; Yawr's existing
`TemplateEvaluator` does so using GXL.

## Date comparison and ordering

- `date.compare(a, b)` returns `-1`, `0`, or `1` by instant.
- `date.diffSeconds(a, b)` returns signed `a - b` seconds, including fractions.

Arguments must be valid RFC3339 timestamps (up to nanosecond precision, with
`Z` or an explicit offset), or UTC `YYYY-MM-DD HH:MM:SS`. Impossible dates,
missing RFC3339 offsets, year zero, nulls, and wrong argument types fail closed.
Neither function uses the host timezone or clock.

`list.order(items, comparator)` returns an object:

```yaml
outputs:
  ordering:
    type: object
    value_expr: 'list.order(observations, "date.compare(left.timestamp, right.timestamp) < 0")'
```

The comparator is a literal GXL string expression that means "`left` precedes `right`".
It may reference only `left` and `right`, use ordinary pure GXL functions, and
must produce a boolean. `now()` and nested `list.order` calls are forbidden.
Dynamic comparator strings are not supported. Comparator syntax and restrictions,
including expressions nested in collection maps/arrays, are checked before
dispatch, even for empty lists and short-circuited expressions.

Yawr checks both directions for every pair rather than trusting a comparator
that may be non-transitive. The result contains:

| `status` | `items` |
|---|---|
| `ordered` | The uniquely ordered input values |
| `ambiguous` | Original input order; at least one pair has neither direction |
| `inconsistent` | Original input order; contradictory directions or a cycle prevent a strict total order |

Contradictory directions and cycles take precedence over ambiguity, including
cycles among a subset of otherwise ambiguous observations. Never choose a "latest" item unless
the status is `ordered`. Ties are not broken by input order. Invalid expressions,
missing data, and non-boolean comparisons are errors, not successful ordering.
The operation is bounded to **256 items**, with quadratic comparison cost.
It does not relax checkpoint/storage limits.

Closeness thresholds and tie-breaker rules belong in YAML, not in the engine.
For example:

```yaml
value_expr: >-
  list.order(observations,
  'date.diffSeconds(left.min, right.min) < -1800 or
  (date.diffSeconds(left.min, right.min) >= -1800 and
  date.diffSeconds(left.min, right.min) <= 1800 and
  date.compare(left.max, right.max) < 0)')
```

This example uses an inclusive 30-minute threshold. The author must select the
threshold semantics required by their source. Pairwise closeness rules can
produce cycles even when every timestamp is valid; equal maxima can leave
pairs ambiguous. Both outcomes remain explicit and preserve all observations.

## Executable examples

From `examples\structured-values`, using a freshly built CLI:

```powershell
& ..\..\yawr.exe run .\root.runbook.yaml --package-map .\package-map.yaml --profile .\profile.yaml
& ..\..\yawr.exe run .\ordering-root.runbook.yaml --package-map .\package-map.yaml --profile .\profile.yaml
```

The first example asserts four configuration associations, child rowsets of
sizes two/one/zero/zero, associated `observed`/`missing`/`no-data` statuses,
nested exports, and numeric/boolean/null preservation.
The second asserts an authored date rule, equal observations, and a cycle.
All behavior is YAML-backed and uses no external command or diagnostic query.

Durable runs execute their frozen substitution bodies, including nested static
includes, rather than reparsing unresolved source during dispatch. Package
bindings remain root-owned. Explicit child returns checkpoint the unexecuted
tail as `skipped` with reason `terminal`, so resumption cannot execute it.
A single `execution/frame-returned` trace event records the skipped step IDs
without exceeding the bounded checkpoint trace outbox.

## Dry-run coverage

`yawr dry-run` crosses no transport and expands no substituted action, so no
step produces a declared payload. To keep the capture surface identical to a
real run, a dry-run stands in for each action's declared `outputs:` contract:
every non-optional declared name takes its declared type's zero value, an
enum-constrained string takes its first declared member, and nested
`outputs.<a>.<b>` capture paths are materialized with `null` leaves. Without
the stand-in, every legal capture from a tool step fails with
`GCP-RESOLVE-002`, which leaves no static gate for a tool-bearing runbook.

The stand-in is not a dispatch. Each tool step still reports `dry_run: true`
and `would_execute: tool`, and the synthesized values carry no information
about what the action would return: a dry-run checks binding, resolution, and
capture wiring, never results. Capture paths whose shape the declaration does
not describe — GDP indexing such as `outputs.rows[0].name`, optional segments,
or a path descending into a declared scalar — are left alone and still report
`GCP-RESOLVE-002`.

## Failure and operator boundaries

A failed substituted child remains a failed tool result. Available declared
outputs are still type/enum checked and can be captured, including an explicit
`failed` status with typed rows. Missing required outputs add a contract error;
absent/invalid outputs are not invented. These rules apply at each public
nesting boundary. Only declared values, not private child variables, escape.
The example `rows` action deliberately fails its final assertion for
`name: failed` (use `count: "0"` for zero rows). The gather example tolerates
that lookup branch long enough to retain all associations, but the containing
iteration and public call still fail. Execution completion is never a
replacement for domain statuses such as no-data, blocked, negative, unsupported,
or missing.

A failed **native** dispatch produced no payload at all, so there is nothing to
type-check. Its captures are still bound, so an `on_error: continue` successor
can reference every name without a template error, but each source is bound to
the value that source actually holds:

| capture source | value after a failed native dispatch |
|---|---|
| `stdout`, `stderr` | `""` |
| `exit_code` | `-1` |
| `outputs.<name>` | `null` |

`null` rather than `""` is what makes the failure guard writable. Comparing a
declared `boolean` output against `true` raises `GXL-TYPE-001` when the capture
holds a string, which makes the only branch reachable on that path dead code.
`null` compares cleanly against any scalar (`spec/grammar/gxl.ebnf` §5.2), so
both `condition: 'ok != true'` and `condition: 'ok == null'` evaluate.
Capturing `exit_code` remains the most direct success test for a native action.

An operator collector inside a substituted tool produces no final public output
while waiting. Durable restart republishes the same pending turn; only the
actual structured submission is exported. UI opening is not evidence of a
diagnosis. Required missing evidence fails rather than becoming a successful
empty finding.

For read-only caller checkouts, direct **both** artifacts outside the source:
`--run-dir <artifact-directory>\runs --trace <artifact-directory>\trace.jsonl`.
The default run store is unchanged. No checkpoint limit or approval gate is
relaxed by these options.
