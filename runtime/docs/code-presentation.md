# Tool-owned code presentation

Declare syntax on string action arguments and declared output slots:

```yaml
actions:
  - name: inspect
    args:
      text:
        type: string
        presentation: {version: 1, kind: code, language: sql}
    outputs:
      script:
        type: string
        optional: true
        presentation: {version: 1, kind: code, language: powershell}
```

Version 1 supports `sql`, `kql`, and `powershell`. A descriptor is display
metadata, not permission to read a value or an instruction to execute it.
Secret/non-string declarations, malformed descriptors, and additional keys
are rejected. Well-formed future versions/languages remain in snapshots and
fall back to plaintext. Outputs refer to declared slots after `from` mapping.
There is no automatic language detection for stdout or logs.

## Offline authoring

`yawr presentation resolve --stdio` reads one bounded JSON request, returns
one JSON reply, and exits. It uses the same local package catalog and lexical
binding rules as execution, but never starts tools, authentication, or discovery.
Open-buffer overlays override disk, including invalid buffers. Replies contain
metadata and UTF-16 YAML scalar ranges, not query values. Dependencies are
rechecked before publication. No server, credentials, or manual refresh is needed.

The wire contract and shared producer/consumer vector are checked into
`pkg/presentation/testdata/interfaces.md`. Consumers must verify document
version, request ID, generation, overlay versions, and scalar mapping before
painting. Unsupported, missing, stale, or invalid metadata is plaintext.

## Executed values and saved inspection

Current preview graph details carry `code_presentation`. Terminal engine events
carry the frozen tool/action descriptor and a separate `output_value_status`
map. Only approved string outputs are available; secret/unclassified output
is not tokenizable. Long code values use bounded, already-redacted excerpts.
Inputs in graph details remain **Authored template**, not reconstructed executed
queries. Actual resolved inputs are **Unavailable — not retained**.

Read saved plans without resuming or reading the original source:

```powershell
yawr preview --format graphjson --run-dir .runbook\runs --run-id RUN_ID
```

The same frozen projection is served by `GET /runs/{id}/document`, including
completed runs after server restart when its run store is configured.
`presentation_state.occurrences` uses retained event/qualified-node/frame
identities, not a bare step-ID/latest-output join. Missing retained terminal
events do not invent historical values. Mixed checkpoint generations fail closed.

## Runtime contract

`yawr presentation capabilities` advertises `execution-plan/v3` as the sole
readable and writable plan format. Authoring with the bundled helper remains
independent of the configured execution binary; execution verifies that the
binary supports the current format.

`examples/code-presentation` demonstrates all three languages using noop-backed
substitutions. Its strings are returned as data; no SQL, KQL, PowerShell,
provider, or native command executes:

```powershell
yawr run examples\code-presentation\root.runbook.yaml --profile examples\code-presentation\profile.yaml
```

Immutable session graphs retain frozen descriptors. Structural graph hashing
excludes **only** `nodes[].details.code_presentation.plan_snapshot_digest`
(`nodes[].data.details` on the GraphJSON wire): this final binding would otherwise
hash itself through the plan's graph hash. Descriptor/version/origin/tool/action
identity remains structural. The final graph's existing `bound_content_hash`
covers the entire document, including the injected binding; the binding must
equal `execution_plan_hash`, the committed session snapshot blob digest.
Runtime events and read-only run documents use the run store's internal
`SnapshotDigest` instead; these two existing digest namespaces are not interchangeable.
Neither snapshot hashing nor serialized graph revision integrity is weakened.
Dynamic resolutions capture exact action definitions using the existing
executable materializer before the resolution commit. Metadata-bearing closures
use `execution-flow-closure/v3` and retain their own frozen tool table. Display
and restart inspection read only that committed
closure and its frame/occurrence/revision relation, never today's catalog.
Missing/uncommitted linkage remains `unresolved-dynamic`.

The final direct-stdio, session-stdio, HTTP-event and saved-inspection projections
apply existing preview entry/aggregate budgets to already-approved, protected
strings. `available` always requires the exact string in that same payload.
Masking, omission and excerpting cannot preserve a false `available` status.
Presentation never grants permission or restores rejected data.

Terminal preview metadata is atomic: `code_presentation` has a separate 64 KiB
serialized budget and at most 4096 fields in each of `arguments` and `outputs`.
Generic ten-item preview truncation and its omission sentinels never apply to
these typed arrays or to output classifications. If this budget is exceeded,
the preview omits metadata, classifications and output together and emits
`presentation_diagnostic: "limit-exceeded"` outside the typed envelope.
Malformed sentinel-bearing metadata is omitted with
`presentation_diagnostic: "invalid-metadata"` rather than repaired into a partial
identity. The original event kind, error, status and occurrence identity remain
available. Consumers must treat these as optional-decoration diagnostics, not
tool failures or success. Valid metadata remains strictly decoded; missing or
invalid safety classifications never authorize plaintext output fallback.

## Authored GXL and GIS highlighting

Expression presentation is a separate, opt-in lexical protocol:

```powershell
yawr presentation expressions capabilities
yawr presentation expressions resolve --stdio
```

Requests use the same five keys (`schema_version`, `request_id`, `context`,
`document`, `overlays`) as code presentation, with schema
`yawr.expression-resolve/v1`. Replies identify `yawr.core-expression/v1` and grammar
`yawr-expression/v2`, and contain `regions` rather than tool bindings or
dependencies. The existing code resolver and capabilities are unchanged.
The expression resolver reads only the submitted document; absent catalogs,
tools, providers, package maps and credentials cannot block highlighting.

Core selects runtime-consumed scalar sites structurally: boolean GXL conditions
and output `value_expr`, plus GIS templates in CLI/tool/host-action requests,
input prompts and labels, includes, iterates, assertions, displays, event
filters and the noop/include capture exception. Ordinary GCP captures, option
values, capture defaults, arbitrary JSON/YAML data and runtime outputs are not
expression sites. A nested `condition` key in ordinary data is not a boolean
site. Aliases are left plain; unrelated input aliases do not prevent independent
authored sites from being selected. A literal `matches` assertion's `expected`
is a regex site, including an anchored scalar at that exact destination. Its
source range excludes anchor/tag properties, but includes YAML scalar quotes.
An arbitrary anchor definition elsewhere does not acquire this role.

Each region includes the actual RFC 6901 scalar pointer, its source range, and
decoded `mode`, `text_length`, `text_digest`, and lexical `tokens`. All spans are
half-open UTF-16 code units; the SHA-256 digest covers the exact UTF-8 decoded
string. Clients must verify scalar ownership, document identity, decoded length
and digest, then map decoded spans through their YAML CST (not by adding the
source start). Folded/chomped scalars and YAML escapes require that mapping.
The canonical cross-client examples are in
`pkg/presentation/testdata/expression-values.json`.
The v2 regex examples are in `pkg/presentation/testdata/regex-values.json` and
the canonical authored YAML is `pkg/presentation/testdata/regex-source.yaml`.

The existing GXL lexer and GIS escape/brace scanner produce tokens without
evaluation or diagnostics. Lexically valid prefixes remain usable while an
expression is incomplete. `\${` is literal, `$${` is invalid rather than an
escape, and sequential backslashes follow the runtime scanner. Boolean sites
accept bare GXL only; wrapper syntax is rejected.

The same grammar version also colors literal arguments explicitly defined by
core as GXL source: currently only the second argument of `list.order` (its
comparator). It does not infer code from ordinary strings, object properties,
unknown functions, variables or computed string values. The core lexer decodes
eligible literals and maps nested tokens back through GXL escapes; outer quotes
and malformed suffixes remain strings. Recursive lexical presentation is bounded
to 16 literal levels and 65,536 scanned tokens per expression. This does not
permit nested ordering at runtime or change strict parsing/evaluation.

Grammar v2 also colors the complete literal second argument of the exact builtin
`regex.match(text, pattern)`. Computed/concatenated/parenthesized arguments,
`obj.regex.match`, variables, ordinary strings and host regex syntax are not
admitted. Regex argument roles are distinct from GXL comparator roles. Nested
regex tokens compose through comparator literals and GIS/YAML source maps while
the outer value keeps its `gxl` or `gis` mode. Regex literals passed to the builtin
do not undergo a second GIS expansion.

Assertion expected values use `regex` mode: literal pieces are decoded by the
actual GIS scanner, lexed independently, and mapped back to the original authored
value. Unknown interpolation values are never joined into a guessed pattern.
GIS interpolation overrides regex only at actual template boundaries; escaped
delimiters remain literal, and forbidden `$${` stops the recognizable prefix.
The Go-only bounded scanner covers Go regexp/RE2 groups, classes, flags, escapes,
anchors and repetition, using the existing closed token classes. Unsupported
lookaround/backreferences and incomplete forms fall back to string/plain text;
coloring is not regex validation or matching.

Readers accept the current `gxl,gis,regex` capability set only. Values are
checked against that envelope grammar, and stale or modified spans are rejected.

Safe authored graph details may carry the optional sibling
`expression_presentation` (version 1, grammar `yawr-expression/v2`). Each entry
points to an existing displayed string with a relative JSON pointer; no code
is duplicated. Secret masking removes affected entries; final rendering drops
entries whose string or digest changed. All retained step kinds use this
projection, independent of tool descriptor availability. Missing retained
fields (including stdin/env text, output declarations, typed `collect_values`,
handoff bindings and node titles) are not reconstructed for panel highlighting.

Frozen and dynamic inspection uses only its committed plan/spec/closure.
Expression metadata introduces no snapshot version or self-binding field.
Both structural and full-bound hashes include it normally. Existing tool-only
occurrence authority remains unchanged.

Limits: 32,768 UTF-16 units per string, 4,096 regions/values, 65,536 total tokens,
8 MiB request/reply, 128 overlays, and structural depth 128. Oversized strings
are skipped; aggregate overflow produces an empty unavailable reply or omitted
panel envelope. Unknown versions, invalid spans, stale identities, unsupported
sources and cancellations fall back to plaintext. This feature does not add
evaluation, completion or diagnostics.
