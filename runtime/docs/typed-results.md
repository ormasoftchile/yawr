# Typed bindings and durable named Results (P0 candidate)

This opt-in feature replaces assignment-only noops and fake one-item publication
loops. It uses the existing YAML, GIS, GXL and GCP languages. It does not enable
Markdown rendering, introduce a report DSL, or implement P1 reusable cross-field
contracts or default child-variable isolation.

## Authoring

`bindings` is an **ordered sequence**, evaluated once per runbook invocation from
configured inputs. Each declaration requires `name`, `type`, and a present `value`.
`mutable` defaults to false. A resumed suffix does not reinitialize bindings.

```yaml
apiVersion: yawr.runbook/v1
id: typed-example
name: Typed example
bindings:
  - {name: phase, type: string, mutable: true, value: pending}
  - {name: observations, type: array, mutable: true, value: []}
outputs:
  status: {type: string, value_expr: phase}
  result:
    type: object
    value_tree:
      observations: '${observations}'
      empty: ""
      zero: 0
      flag: false
      nullable: null
flow:
  - step:
      id: settle
      type: assign
      assign:
        - {name: phase, value: observed}
  - step: {id: results, type: results, title: Results}
```

Supported types: `any`, `string`, `bool`/`boolean`, `int`/`integer`,
`number`/`float`, `array`, `object`. New typed values do not coerce numeric or
Boolean strings. Null requires `any` at the declared port; null nested inside an
object or array is retained. Integer precision remains the existing ±2^53 range.
String enums use the existing enum validator.

An output selects exactly one of `value`, GXL `value_expr`, or typed
`value_tree`. Tree string leaves use GIS; a sole `${expression}` returns a native
value. Mixed literal/interpolation text returns a string. `\${...}` is the GIS
literal escape; `$${...}` and `$expr` wrappers are not supported.
An omitted initializer is invalid; explicit null, false, zero, empty text,
empty arrays and empty objects are not defaults or missing values.
`optional: true` omits a genuinely missing output key only. It never suppresses
parse, type, evaluation or constraint errors, nor substitutes null.

Entry declarations may reference admitted inputs and earlier
bindings only. Duplicate names, reserved evaluator roots, self/cyclic and forward
dependencies fail admission, including a forward name that exists in ambient
variables. Declarations are not reordered. New expressions are pure: no `now()`,
including nested/comparator expressions; dynamically supplied comparators are
rejected. Existing GXL evaluation is bounded at 32,768 UTF-16 units per expression,
128 nesting, and 65,536 syntax/evaluation visits per construction. `list.order`
retains its 256-element bound. These are expression budgets, not table-row previews.

`assign` writes declared mutable bindings, in order, into scratch state. All
writes succeed atomically or none become visible. Captures into declarations use
the same mutability and strict type checks. Conflicting parallel/concurrent
iteration writes and unresolved mutable closures that cannot prove disjointness
are rejected. Atomic operations do not accept retries, delays, continue-on-fail
or goto error handling.

## Database-info contract

The admitted database-info probe contains these exact declaration and assignment
forms (excerpts, not a replacement for its full business runbook):

```yaml
bindings:
  - {name: probe_status, type: string, mutable: true, value: incident-read-failed}
  - {name: probe_incident_id, type: string, value: '${icm_id}'}
  - {name: db_observations, type: array, mutable: true, value: []}
# In flow:
# - step: {id: incident_loaded, type: assign, assign: [{name: probe_status, value: scope-invalid}]}
```

The real database tool capture uses `database_info_public: outputs` to copy its
**complete declared semantic map**, including all 18 database-info fields.
It is not a new manual shortlist and not the provider's private payload.
Undeclared extras, process stdout/stderr/exit code, child locals and control
sentinels are excluded. Bare `outputs` requires a declared tool output contract
and successful validation before capture; unconstrained tools cannot authorize
it. Named `outputs.name` captures retain their defined behavior.

The probe's actual `outputs.result.value_tree` is:

```yaml
status: '${probe_status}'
incident:
  id: '${probe_incident_id}'
  title: '${incident_title}'
  service: '${incident_service}'
  state: '${incident_state}'
  environment: '${incident_environment}'
  impact_start: '${incident_impact_start}'
  created: '${incident_created}'
  mitigated: '${incident_mitigated}'
scope:
  logical_server: '${incident_server}'
  logical_database: '${incident_database}'
  query_environment: '${probe_query_environment}'
  start_time: '${probe_start_time}'
  end_time: '${probe_end_time}'
database_info: '${database_info_public}'
```

Its genuine configuration iteration, assertions, scope checks, collector, blocked
end, and failure handling remain. It ends with
`{id: results, type: results, title: Results}`. Existing display strings are not
the result transport and do not authorize the paused renderer.

## Publication, forwarding and recovery

`results` is the last top-level operation of its declaring runbook. It evaluates
that runbook's named outputs and terminates only that invocation. A parent uses
explicit include captures such as `probe_result: outputs.result`, then declares
and publishes its own root outputs. Runbook-backed tools use their child's
committed publication, not a later re-evaluation of its variables. There is no
implicit promotion of child results and no synthetic `probe_results[0]` wrapper.
Includes pass their defined variable scope; full isolation is deferred.

### Forwarding with an early-stop gate

An include's `gate.stop_if` tests the child's existing declared outcome after
child settlement, **before** either include captures or engine GCP captures.
A matching gate terminates the parent with that same category and code. It
skips the entire capture map and all later parent operations, including Results
and provider calls. This also applies if the child already published: its
committed record remains its own, and no parent publication is implied.
Eager, lazy, dynamic and resumed include invocations share this behavior.

The synthetic caller uses this pattern (retain its existing `requires` and
`toolRefs`, and run only with its synthetic profile and package map):

```yaml
inputs:
  incident_id: {type: string, required: true}
outputs:
  status: {type: string, value_expr: probe_status}
  result: {type: object, optional: true, value_expr: probe_result}
flow:
  - step:
      id: invoke
      type: include
      capture:
        probe_result: outputs.result
      include:
        runbook: ../../hands-on-tests/lib/icm-db-info-probe.runbook.yaml
        expand: eager
        with:
          icm_id: '${incident_id}'
          query_environment: dbinfo-singleton
          start_time: "2026-01-15 10:00:00"
          end_time: "2026-01-15 12:00:00"
          expected_logical_server: synthetic-sql
          expected_logical_database: synthetic-db
        gate: {stop_if: [blocked]}
  - step: {id: results, type: results, title: Results}
```

Ordinary, non-stopping success captures the exact committed native child
`result`, then the parent publishes its declared outputs. An unpublished
blocked return has **no result**, not `{}` or a captured null. The optional root
declaration does not publish at an early end or make an absent child output
capturable. Without a matching gate, a requested unpublished child public output
still diagnoses a missing publication. A gate cannot turn failed, denied,
indeterminate or unfinished required child work into a successful blocked return;
the execution failure remains distinct from the domain outcome.

Failed, denied, indeterminate, unfinished, pending-interaction or tolerated
required descendant work cannot become a successful complete root publication.
Domain `no-data` and `boundary` results are valid successful outcomes; an early
blocked end has no publication. Execution status and Results availability are
different facts.

Publication shares the engine's existing checkpoint/lease/trace transaction.
Only committed records are readable. Origin, publication ID, digest and original
checkpoint sequence survive reopening; transport errors do not authorize another
producer invocation. Resume recovers already committed output and original
pending event IDs. It never reconstructs Results from trace previews or Vars.

## Canonical record and transport

`yawr.run-results/v1` contains:

| Field | Type / meaning |
|---|---|
| `schema_version` | `"yawr.run-results/v1"` |
| `publication_id` | Stable publication identity string |
| `plan_snapshot_digest` | Frozen-plan SHA-256 identity |
| `checkpoint_sequence` | Positive committed publication sequence, not GET sequence |
| `origin` | `{node_id: string, invocation: positive integer, frame_id?: string}` |
| `outputs` | Map of declared names to `{type: string, value: native JSON}` |
| `digest` | `"sha256:"` plus digest of canonical record **excluding `digest`** |

Root `frame_id` is absent. Qualified node IDs retain the runtime's existing
slash-qualified execution identity. Canonical JSON sorts object keys by UTF-8
bytes, preserves array order and native values, uses Go JSON string escaping
(including `<`, `>`, `&`, U+2028/U+2029), and preserves numeric serialization
including negative zero. A present null always has a `value` member.
PJVM evaluation normalizes authored -0 to +0; transport preserves -0 when already present in the canonical record.

### Direct CLI

First negotiate v3 as described below. Start with:

```powershell
yawr run --stdio --require-capabilities yawr.typed-results/v1,yawr.run-results-chunks/v1 `
  --run-dir <isolated-run-store> --profile <authorized-profile> <runbook>
```

Keep stdin open until the single actual **`run.finished`** frame. There is no
`run.completed`. Existing interaction/event streams drain before result chunks
and terminal output. Terminal payloads without the feature retain their defined fields.

If the complete terminal, its original summaries/errors and `results` fit the
existing **1 MiB including LF** limit, `run.finished.results` is the complete
canonical record. Otherwise canonical JSON is constructed as one bounded document
and sent before terminal in frames with **exactly** these fields:

```json
{"version":"yawr.stdio/v1","type":"run.results.chunk","runID":"...","publicationID":"...","digest":"sha256:...","offset":0,"totalBytes":1435856,"data":"...base64..."}
```

Each decoded payload is at most **65,536 bytes**. Offsets count bytes, not
characters; a UTF-8 code point may cross chunks. Terminal then has
`results_ref: {schema_version, publication_id, digest, total_bytes}`, not an
inline or partial `results`. Validate identity, contiguous offsets, byte count,
strict UTF-8, canonical serialization and the record digest excluding `digest`.
Missing/conflicting/overlapping transport is unavailable, not a rerun request.
Do not validate the reference by hashing wire bytes including the digest field.

For a feature run without deliverable publication, terminal has `results: null`
and `results_unavailable: {status, reason}`. Status is `unavailable` or `redacted`;
reasons include `no-publication`, `execution-not-completed`,
`invalid-publication`, `protected-content`. Execution `status` remains authoritative;
the original execution error is retained separately as `error`.
Its message uses the existing step-error preview bound, with
`messageTruncated: true` when shortened; this never changes execution status
or applies truncation to canonical Results.
A write failure returns a transport error/nonzero CLI exit; it never sends an
invented successful terminal or discards the committed durable record.

`--configure` uses the current command shape
`{"type":"run.configure","inputs":{"declared_secret":"..."}}` with **no command
version field**; values are strings and only declared private inputs are accepted.
It is the current private-input configuration protocol.

### Authorized active and persisted HTTP

The existing bearer/JWT middleware authorizes `POST /rpc` before any result lookup:

```json
{"jsonrpc":"2.0","id":1,"method":"run.get","params":{"runID":"..."}}
```

The JSON-RPC `result` contains `state`, `source`, timestamps, cursor and
`results`, the identical canonical root record or null. Active
and reopened persisted reads do not execute any steps. No private RunState
envelope, TypedState slots, child publications or trace-derived reconstruction is
returned as Results. The same unavailability object is additive when needed;
`protection-unavailable` also applies if the frozen protection plan cannot be read.
Protected canonical values are withheld as a whole, never replaced under their
original digest. Protected Vars in typed runs are withheld with
`vars_unavailable`, rather than creating a second leak through that field.

The existing HTTP `writeRPC` uses a JSON encoder and has **no response frame-byte
ceiling**; the existing server write timeout defaults to 60 seconds. Therefore
`run.get` returns the bounded **full** document, including values above 64 KiB
and 1 MiB; no new artifact store or HTTP chunk-reference protocol is necessary.
Clients must bound their full-response reader, handle HTTP/JSON-RPC/timeout
errors independently, and never display incomplete data. No HTTP response
budget or timeout was raised.

All existing durable limits remain: plan 64 MiB, state envelope 16 MiB,
inline stored value 64 KiB, expanded checkpoint/value 256 MiB, compressed blob
64 MiB, 4096 stored entries. Canonical Results construction also stops at
256 MiB before allocating an oversized escaped string or terminal frame.
Ordinary direct stdio's 1 MiB protocol is **not** investigation-session stdio's
8 MiB frame / 4 MiB projection protocol.

## Capability and editor protocol

Actual queries, each a one-shot command:

```text
yawr presentation capabilities --v3
yawr authoring capabilities --v3
yawr presentation expressions capabilities --v3
```

The first returns `presentation-capabilities/v3`, `resolver_version:
core-binding/v3`, v1/v2/v3 execution-plan read/write arrays, `graph_read: ["1","3"]`,
`authoring_request: authoring-request/v3`,
`expression_request: yawr.expression-resolve/v1`, stdio version/limits, and:

```json
{"capabilities":["yawr.typed-results/v1","yawr.run-results-chunks/v1","yawr.run-get-results/v1"]}
```

Require all transport capabilities used by the client before execution. The
`--require-capabilities` comma-list provides a second fail-closed runtime gate:
an unknown capability exits 2 before parsing/executing the runbook. Runtimes that reject `--v3` or lack the required versions/capabilities are
refused. Spawn errors, timeouts, broken pipes, malformed replies and
nonconforming bodies are failures and never trigger another execution mode.

The editor queries the current v3 capability envelopes. Unknown capability
query versions fail explicitly.

`authoring complete|signature|required-arguments --stdio` retains the exact
seven-field request shape, selecting `schema_version: authoring-request/v3`.
Responses use `authoring-reply/v3`, `core-authoring/v3`, and the existing
`yawr-expression/v2` grammar. `typed-key`/`typed-value` sites and items are
schema-derived; declaration type and `assign`/`results` discriminators use the
embedded runtime schema, not a client semantic scanner. Expression completion
includes earlier typed bindings with their `value_type`. V3 capabilities also
describe bare `outputs` capture and assign/Results operator roles.

Expression resolve requests use `yawr.expression-resolve/v1`; replies use the
same schema and `yawr.core-expression/v1`.
New binding/assignment/value-tree string sites use the existing bounded GIS/GXL
visitor. Native include, nested-comparator and regex readers use their current protocols.

Feature-bearing graph documents use `schema_version: "3"`. Frames carry
`invocation` declaration metadata (ordered bindings,
types/mutability/presence, outputs and Results terminal flag). Assign details carry
ordered `assign: [{name,value,value_present}]`, `role: technical`, and expression
descriptors. Results carries `role: operator` and default title `Results`.
IDs, qualified IDs and content hashes are not rewritten for UI convenience.
The same model is produced by read-only saved inspection
(`yawr preview --format graphjson --run-dir DIR --run-id ID`), without reloading
current YAML. Results has the generic GraphJSON `terminal` renderer type while
retaining semantic kind `results`; explicit authored titles are preserved.
Plans, flow closures and state use the engine's feature-bearing v3 formats.

This runtime is a **candidate**. Real consumer integration and independent review
must pass before promotion or another helper integration. Markdown and P1 remain
deferred.
