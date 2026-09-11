# Offline authoring

The finite `yawr authoring` helper supplies native-editor completion, GXL
signature help, and an explicit missing-required-arguments edit. It does not run
or plan runbooks, initialize profiles/authentication, contact tool providers, or
discover live MCP inventories.

Commands:

```text
yawr authoring capabilities
yawr authoring complete --stdio
yawr authoring signature --stdio
yawr authoring required-arguments --stdio
```

Each stdio operation consumes one closed `yawr.authoring-request/v1` JSON document and
returns one closed `yawr.authoring-reply/v1` document. Unknown/duplicate keys, wrong
operations, inconsistent buffer identities, and invalid UTF-16 positions are
rejected with exit code 2 and categorical stderr. Unavailable replies exit 0.
Capabilities identify `yawr.core-authoring/v1` and `yawr-expression/v2`.

## Include mappings (opt-in authoring v2)

`include:` in a runtime `type: include` step expects a **mapping**, not a
filename scalar. Completion immediately after the colon offers an explicitly
selected static `{runbook: ""}` or catalog
`{runbook_ref: "", resolve_from: catalog}` scaffold. Empty strings are
placeholders for the author to fill, not executable target guesses. On an
indented next line, completion offers actual mapping keys instead. It also
supports existing single-line plain/quoted/explicit keys and flow mappings;
comments, values, newline style and the rest of the document remain unchanged.
The same separator safeguards apply as for argument keys.

Keys and enum values are projected from the runtime's embedded
`schemas/runbook.schema.json` `IncludeConfig` definition. Existing keys narrow
its mutually exclusive static/dynamic arms: static `runbook` excludes
`runbook_ref`/`resolve_from`/`on_not_found`, while dynamic `runbook_ref` excludes
`runbook`/`expand`. `gate.stop_if` is an authored string list, not a guessed
outcome inventory. `with` keys and arbitrary data named `include` have no
structural suggestions. The shared runtime-flow visitor selects nested steps.

This slice offers structure and declared enum values, **not target-path,
imports-alias, package-export, child-input or output discovery**. Author
`runbook`'s path/import alias or `runbook_ref`'s catalog identity explicitly.
No directory scan, target-file read or provider operation is needed. Existing
GXL/GIS completion remains available at its separately admitted sites.

Use `yawr authoring capabilities --v2` to negotiate
`authoring-capabilities/v2` / `core-authoring/v2`. Send
`authoring-request/v2` to receive `authoring-reply/v2`; it adds the closed site
and item kinds `include-mapping`, `include-key`, and `include-value` and changes
no other fields, operations, limits or expression grammar. Unflagged
capabilities and v1 requests retain their historical exact wire and behavior.
The editor probes v2 only after validating v1 capabilities; an older helper
rejecting the opt-in flag keeps its existing v1 completions. Invalid v2 replies
are not downgraded. Capability support is cached only for the same binary
identity, and cancellation/deadline checks still apply.

## Available tools means a bounded local scope

At `toolRefs[].name`, candidates come from the existing explicit local catalog:
compiled built-ins, project configuration and package maps, entrypoint
requirements, declared tool paths, and the conventional project `tools`
directory. A reference with an explicit file path offers only that definition's
actual name. Every candidate is checked by the same `Build`/`BindFile` authority
used by tool presentation.

At a tool step's `tool.name`, only successfully bound **current-file references**
are offered. Requirements do not inherit another runbook's references, and
selecting a tool does not synthesize a reference. This is not an inventory of all
tools installed on the machine. Missing local manifests/exports or invalid dirty
overlays remain unavailable; there is no fallback from an invalid overlay to disk.
There is no new recursive workspace/runbook-directory discovery, history
exclusion, or increase to the existing 4096-entry limit. Independently proven
explicit-file bindings can survive limited catalog discovery, with the limitation
reported explicitly.

Tool/action selection changes only the selected name. Argument completion offers
missing direct argument keys, not nested value keys. Neither operation populates
other arguments, defaults, transport settings, or values. Descriptions are
untrusted plain documentation (at most 2048 UTF-16 units); descriptions of secret
or sensitive-name arguments are omitted. Default **values never cross the wire**:
`default_info` reports only `absent` or `declared-redacted`, including explicit
null defaults.

## Expressions and explicit placeholders

Core selects the same runtime-owned expression sites as presentation. Ordinary
SQL/KQL/PowerShell/prose and regex-pattern strings are not GXL sources. Within
GIS, only real interpolation expressions are eligible. The exact second literal
argument of `list.order` is GXL even when its outer call is unfinished; computed,
parenthesized and concatenated comparator values are not promoted. Comparator
completion excludes `now` and nested `list.order`.

The dependency-neutral Go function registry defines the 18 runtime functions,
their display signatures, parameter spans, and supported namespaces (`str`,
`list`, `regex`, `date`). It is shared with the parser and verified against the
evaluator. Signature help counts commas only at the selected call's own depth;
zero-arity and out-of-arity positions have a null active parameter.

Only the explicit `required-arguments` operation inserts argument placeholders. It inserts
one block-mapping edit with `null` literals and relative placeholder ranges.
Declared required names are included even if they have defaults; an existing key,
including an existing null, is never overwritten. Names are sorted. The editor
should apply this as one undoable native operation using escaped plain segments
and placeholder APIs, not by interpreting metadata as snippet syntax.

## Source safety and limits

Requests carry the full document, caret, context generation, and all dirty
overlays. Source digest and dependency versions/digests are revalidated before
publication. Offsets are absolute UTF-16, while placeholder/label ranges address
the newly inserted text/signature label. Core owns insertion offsets and encoding
through YAML, GIS and comparator-string layers. Mid-token completion replaces
the entire identifier, including its suffix.

The current recovery is intentionally narrow: an incomplete direct block
argument key or empty insertion line can be repaired locally and admitted only
after parsing and proving its ancestry. Complete earlier flow entries can survive
a later broken entry only when the discarded tail stays inside that flow; later
root declarations cannot be hidden by prefix recovery. Duplicate keys and merges
remain rejected throughout the parsed source. Anchors/aliases in unrelated values
(such as an earlier UTC regex
assertion) do not suppress a proven tool or expression site. They are never
expanded: addressed ancestry, step discriminators, tool/reference subtrees and
binding declarations remain strict. Malformed ancestry and unprovable ranges
are rejected. Flow-style argument scaffolding and multiline
quoted scalars whose physical mapping is ambiguous are unavailable. Literal and
folded block-scalar edits must remain within one proven physical content run.
Argument keys without an original separator acquire `: ` only at an
AST-proven local repair slot. Explicit YAML keys (`? key`) without a separator
are unavailable for completion; author their separate `: value` line first.
Explicit single-line scalar keys with a separator preserve that separator,
value and comments. Multiline key-token replacement is unavailable, as are
complex, aliased, anchored and merge keys. A quoted `?` in an ordinary key name
is not an explicit-key indicator. The required-arguments command still counts
existing explicit null keys as present and preserves their values.
There are no variable/capture/output-property suggestions, diagnostics or fixes.

Request/reply size is 8 MiB; at most 128 overlays, 4096 catalog/read entries or
results, 32 MiB total disk reads, 32768 decoded expression UTF-16 units, 65536
lexer tokens, and depth 128. No completion list is silently truncated. The
finite CLI has a five-second processing deadline; editor transport cancellation
and its end-to-end queue deadline remain the client's responsibility.
