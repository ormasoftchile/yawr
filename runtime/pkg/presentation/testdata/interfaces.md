# Highlighting implementation interface freeze

**Interface notes for the current implementation.**
Production implementation is authorized; this is not another feasibility gate.
Basis: accepted `highlighting-feasibility\round2\decision.md`, refined `core\contract.md`, and `surfaces\report.md`.
Only this session artifact is changed. Runtime, extension, design, existing binary and inherited changes remain untouched.

## 1. Declaration and propagation

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

Add `schema.ArgDef.Presentation *PresentationDescriptor`, YAML/JSON `presentation,omitempty`; retain through runtime ArgDef conversion, catalog/package mappings, clones and frozen tools/substitutions.
`PresentationDescriptor` is exactly `{version: positive integer, kind: "code", language: string matching ^[a-z][a-z0-9-]{0,31}$}`; no additional keys.
Only `type: string` admits it (not `secret`, objects or arrays). Invalid kind/keys/types are declaration errors. Well-formed future versions/languages survive serialization but are `unsupported`.
Supported: version `1`, languages `sql`, `kql`, `powershell`; Shiki grammar mapping is SQL / kusto / PowerShell. No alias, comments, inference, interpolation changes, execution changes or provider selection.
Tool YAML `actions` remains a **sequence**; existing internal/frozen JSON `ToolDef.actions` remains its existing name-keyed map. Public metadata action/field lists below are arrays; do not change tool YAML to a map.
`outputs` metadata addresses the declared post-`from` slot, not raw transport keys or captures. No descriptor is inserted into dispatched args or tool-returned objects.

## 2. Shared local authoring resolver

**Command: `yawr presentation resolve --stdio`.** One UTF-8 JSON request on stdin, close stdin, one JSON reply on stdout, then exit; no JSON-RPC or persistent server.
Actual `cmd\yawr\main.go` uses a switch and `flag.FlagSet`, **not Cobra**. Add that command there; do not introduce Cobra or route through `run`, `plan`, `serve`, profile loading or bridge startup.
Extension ships the matching platform helper/current binary in its package. Authoring always uses this bundled component, not PATH or a separately installed CLI. Unsupported/missing helper => visible plaintext fallback.
Shared Go implementation refactors buffer-aware reads into `pkg\pkgcatalog.Build` / `BindFile` and tool parsing. Reuse `cmd\yawr\packagemap.go` merge semantics; no TypeScript binding resolver.
Use a metadata-only parse/projection: identities, versions, tiers, exports, action names, argument/output types and descriptors. Do not require runtime transport validation or a complete executable plan to obtain metadata.
Capability boundary: local file reads only; never instantiate providers/auth/MCP discovery, run tools, load profile secrets, read `.env`, expand executable expressions, or use network discovery. Error text must not echo source values.

### Wire types (closed objects; optional fields marked `?`)

```text
Request = {schema_version:"yawr.presentation-resolve/v1", request_id:string,
 context:{project_root:string, generation:integer>=0, package_map_path?:string,
 entrypoint_path?:string, package_root?:string},
 document:Buffer, overlays:Buffer[]}
Buffer = {uri:string, path:string, version:integer>=0, text:string}
Reply = {schema_version:"yawr.presentation-resolve/v1", resolver_version:"yawr.core-binding/v1",
 request_id:string, context:Request.context, document:{uri:string,version:integer>=0},
 status:Status, reason?:Reason, bindings:Binding[], regions:Region[], dependencies:Dependency[]}
Binding = {id:string, name:string, status:Status, reason?:Reason,
 tool_id?:string, tool_digest?:string, source_uri?:string, actions:Action[]}
Action = {name:string, arguments:Field[], outputs:Field[]}
Field = {name:string, value_type:string, status:Status, reason?:Reason,
 presentation?:PresentationDescriptor}
Region = {binding_id:string, action:string, direction:"input", field:string,
 yaml_path:string, range:{start:integer>=0,end:integer>=0}, status:Status, reason?:Reason}
Dependency = {uri:string, version?:integer>=0, digest?:string, missing?:true}
Status = "resolved" | "unavailable" | "ambiguous" | "unsupported" | "stale"
Reason = "missing-dependency" | "missing-descriptor" | "incomplete-identity" |
 "invalid-descriptor" | "ambiguous-binding" | "incomplete-source" | "unsupported-language" |
 "unsupported-version" | "unresolved-dynamic" | "invalid-source-range" | "stale-request" |
 "limit-exceeded" | "invalid-request"
```

All arrays required, possibly empty; numbers must be safe integers in both Go and JS. Field status means descriptor availability, not string-value safety; non-resolved records require `reason`. Bindings/overall reply can resolve with some unavailable fields.
IDs and names are opaque strings, never split on `/`. `Binding.id` is `b` + zero-based `toolRefs` index; response-scoped, not a durable occurrence key.
`tool_id` is the actual core bound definition name (`Binding.Def.Name`), scoped by request/context/binding; in frozen views it is the actual frozen `Plan.Tools` key. Do not claim globally unique names.
`tool_digest` uses the existing core definition digest where available; authoring path-file bindings use `sha256:` + lowercase SHA-256 of the UTF-8 tool-buffer bytes. Resolved bindings require tool ID/digest; resolved graph envelopes also require action. Never compare authoring/file and frozen-definition digests across domains.
`source_uri` identifies the definition file; package-qualified and path/local-name cases must preserve core binding identity. No truncation or invented namespace restriction.
Paths are absolute native paths; URIs are VS Code URI strings, echoed exactly. A file URI must match its canonical path. Untitled runbooks use a virtual absolute `path` under the selected project root, independent of `uri`.
`document` is the current runbook, including unsaved/new files. `overlays` supplies open tools, included runbooks, project/package-map configs and manifests as needed; reject conflicting duplicate paths. Overlay content wins over disk even if invalid.
Root/package-map configuration is immutable for a request; omitted package map means ordinary project config. Extension uses its existing project-root/package-map selection, not a new guessing convention.
`entrypoint_path` defaults to document path; when supplied it identifies the root runbook whose package requirements establish the catalog, not an inherited toolRefs scope. `package_root` is only for package-internal documents and must be verified against core package/export/path facts; it cannot relax containment. Unprovable context => `incomplete-identity`.
Apply existing `pkgpath` bases/containment, including symlinks: workspace external paths retain PKG-W003; package-internal escapes retain PKG-007. Overlay paths outside root are not automatically forbidden or automatically trusted.
Return dependencies read plus missing expected files; overlay dependencies carry their editor version and digest, disk dependencies digest, missing entries `missing:true`. Watch dependencies/config/catalog directories, including external roots.
Snapshot reads consistently; recheck dependencies before publication, returning `stale` if changed. Increment `context.generation` on relevant open/close/edit/disk/config changes; no persistent source/value caches.
Extension matches request ID, document URI/version, generation and every relevant overlay version before publication. Discard obsolete replies, clear old paints, automatically request current state; caches are acceleration only.
Debounce 150 ms, at most one helper per active document, cap helper concurrency at two; terminate only owned obsolete processes. Helper deadline 5 s; worker parse/map/token deadline 250 ms; 32,768 UTF-16 units per code value.
Bound stdin/stdout to 8 MiB each, overlays to 128, published bindings/regions to 4096 each. Limit failures are explicit plaintext, never silent truncation. Valid semantic fallback replies exit 0; malformed envelope exits 2 with a bounded diagnostic, no source dump.
No dependency-manifest generator is needed: use existing local `.tool.yaml`, `yawr-package.yaml`, `.yawr\config.yaml`, and optional `yawr.config/v1` package map. Missing local files / discovery-only dynamic tools return `missing-dependency`.
Do **not** add the earlier hypothetical `yawr.tool-presentation/v1` manifest as a required prerequisite. Materialize genuinely missing packages through existing workflows; ordinary new runbooks, formatting, selectors and valid dirty tool edits refresh without prepare/save/manual refresh.

### Source publication and partial parsing

`range` is **zero-based absolute UTF-16 code-unit offsets**, `[start,end)`, in the exact request text, including CRLF unchanged. It covers the full YAML scalar token (quotes/block header included), not its decoded string.
Go must convert byte/rune positions explicitly; `yaml.Node.Column` is not a JS offset. Reject invalid boundaries/split surrogate pairs. `yaml_path` is RFC6901 over authored YAML, e.g. `/flow/0/step/tool/args/text`.
The extension resolves that exact CST node/path/range, decodes/maps it with `yaml`, and paints content spans only: no key, quote, header, structural indentation or physical EOL. Folded/generated whitespace has no paint span.
Go publishes only verified string argument nodes selected by core binding; it never returns query values. JS validates decoded-map equality; aliases, overlaps, duplicate keys, endpoint parse errors and mismatches fail closed with visible reason.
Bindings/actions/descriptors are available independently of `regions`: valid `toolRefs` and tool metadata still return when an unrelated flow step is incomplete. No full-runbook schema success required.
A core tolerant source pass may retain independently verified nodes before an unrelated error; it must not guess identity or recover through duplicate/invalid binding blocks. Broken tool identity invalidates that binding, never silently uses disk metadata.

## 3. Canonical shared JSON vector

The following single `json` fence is the **shared machine-readable artifact**. Both owners' tests must extract it from this file (or copy it byte-for-byte into their test fixtures with a digest check); no independently hand-shaped producer/consumer fixtures.
`expect` is an exact partial-match assertion, not a claimed complete production reply; extra required reply/dependency/identity fields are validated against §2. Compute/verify tool digest against overlay bytes, not a fixture placeholder.

```json
{
  "request": {
    "schema_version": "yawr.presentation-resolve/v1",
    "request_id": "contract-1",
    "context": {"project_root": "C:\\highlighting-contract", "generation": 7},
    "document": {
      "uri": "file:///C:/highlighting-contract/new.runbook.yaml",
      "path": "C:\\highlighting-contract\\new.runbook.yaml",
      "version": 3,
      "text": "apiVersion: yawr.runbook/v1\nid: example\nname: Example\ntoolRefs:\n  - name: db\n    path: db.tool.yaml\nflow:\n  - step:\n      id: q\n      type: tool\n      tool:\n        name: db\n        action: inspect\n        args:\n          text: 'SELECT 1'\n"
    },
    "overlays": [{
      "uri": "file:///C:/highlighting-contract/db.tool.yaml",
      "path": "C:\\highlighting-contract\\db.tool.yaml",
      "version": 9,
      "text": "apiVersion: yawr.tool/v1\nmeta: {name: db, version: 1.0.0}\ntransport: {type: native, command: never-execute}\nactions:\n  - name: inspect\n    args:\n      text:\n        type: string\n        presentation: {version: 1, kind: code, language: sql}\n    outputs:\n      script:\n        type: string\n        optional: true\n        presentation: {version: 1, kind: code, language: powershell}\n"
    }]
  },
  "invalid_runbook_text": "apiVersion: yawr.runbook/v1\nid: example\nname: Example\n toolRefs: []\n",
  "graph_details_example": {"kind":"tool","tool":"db","action":"inspect","arguments":[{"name":"text","value":"SELECT 1"}],"code_presentation":{"version":1,"status":"resolved","origin":"current","tool_id":"db","tool_digest":"sha256:7e947499578ff5c3edc6437fbd45581765048b892d37a5fcfd1c09ee4dd57423","action":"inspect","arguments":[{"name":"text","value_type":"string","status":"resolved","presentation":{"version":1,"kind":"code","language":"sql"}}],"outputs":[{"name":"script","value_type":"string","status":"resolved","presentation":{"version":1,"kind":"code","language":"powershell"}}]}},
  "expect": {
    "schema_version": "yawr.presentation-resolve/v1",
    "resolver_version": "yawr.core-binding/v1",
    "request_id": "contract-1",
    "context": {"project_root": "C:\\highlighting-contract", "generation": 7},
    "document": {"uri": "file:///C:/highlighting-contract/new.runbook.yaml", "version": 3},
    "status": "resolved",
    "bindings": [{
      "id": "b0", "name": "db", "status": "resolved", "tool_id": "db", "tool_digest": "sha256:7e947499578ff5c3edc6437fbd45581765048b892d37a5fcfd1c09ee4dd57423",
      "actions": [{
        "name": "inspect",
        "arguments": [{"name": "text", "value_type": "string", "status": "resolved", "presentation": {"version": 1, "kind": "code", "language": "sql"}}],
        "outputs": [{"name": "script", "value_type": "string", "status": "resolved", "presentation": {"version": 1, "kind": "code", "language": "powershell"}}]
      }]
    }],
    "regions": [{"binding_id": "b0", "action": "inspect", "direction": "input", "field": "text", "yaml_path": "/flow/0/step/tool/args/text", "range": {"start": 228, "end": 238}, "status": "resolved"}]
  }
}
```

Runnable after build: set `$contract` to this file and `$helper` to the bundled binary; run from an existing empty fixture root. All fixture documents are overlays, not files to create:
```powershell
$v = ([regex]::Match((Get-Content -Raw $contract), '(?s)```json\r?\n(.*?)\r?\n```').Groups[1].Value | ConvertFrom-Json)
$r = $v.request; $r.context.project_root = $PWD.Path
foreach ($b in @($r.document) + @($r.overlays)) { $b.path = Join-Path $PWD (Split-Path -Leaf $b.path); $b.uri = ([uri]$b.path).AbsoluteUri }
$r | ConvertTo-Json -Depth 30 -Compress | & $helper presentation resolve --stdio
```
Tests also replace document text with `invalid_runbook_text`: expect `unavailable/incomplete-source`, no guessed regions. Actions/fields sort by name; bindings by ref index; regions by start offset. Regions/bindings cover the requested document only; included documents get their own lexical requests.

## 4. Graph, runtime and history publication

Add only `code_presentation` to existing `StepDetails` (`nodes[].data.details.code_presentation` in graphjson). Existing `details.arguments[]` retains authored safe values and `redacted`; **do not** add another script-value store.
Exact metadata envelope: `{version:1,status:Status,reason?:Reason,origin:"current"|"frozen",tool_id?:string,tool_digest?:string,action?:string,plan_snapshot_digest?:string,arguments:Field[],outputs:Field[]}`.
Use the same `Field` as §2. Frozen origin requires `plan_snapshot_digest`; unavailable identity may omit tool/action/digest but must give a reason. Descriptor-less known actions retain named fields with `missing-descriptor`.
Direct graph `yawr preview --format graphjson --recurse <path>` and served `/preview/document` populate current metadata using the shared local resolver. Failed metadata must not break an otherwise renderable graph.
Session `segment.graph` chunks and `yawr session graph <session-id> --segment-id <segment-id> --revision <n>` retain frozen metadata inside graph data; existing command's `data` remains base64 graph bytes. Do not change graph hash/revision verification or rebind old blobs.
Runtime terminal `step/completed` / `step/failed` event **payload** adds `code_presentation`; session `run.event` wraps the existing engine event, so location is `frame.payload.payload.code_presentation`. Root, branch and durable frame-commit paths all publish it.
Use actual `payload.output` from those events for output values; `step/output` currently represents stream/log lines, **not** the typed output map. Do not infer a language for stdout or logs.
Add `output_value_status` beside `code_presentation`, a name-keyed map of `"available"|"absent"|"redacted"|"truncated"|"unavailable"` for declared code slots. It carries no values; missing status means unavailable to tokenizer.
Rendering joins metadata output `name` to the existing safe output map. `available` requires a policy-approved string (consumer also checks its type); an empty string is valid. Optional missing outputs stay absent. `truncated` uses only an already-safe excerpt, with a label and matching copy payload.
Before/live/history inputs shown from graph `details.arguments` are labelled **Authored template**. Actual execution-resolved arguments are **Unavailable — not retained**; do not reevaluate templates or advertise them as executed queries.
No `currentStepDetails` Go wire field exists in these source bases. Inspector selection uses existing `details` (`src\stepDetails.ts`, `webview\inspector.tsx`); any current-step UI variable must consume that same object.
Occurrence identity is existing run ID + qualified node ID + call path/frame ID + invocation/retry/occurrence sequence when retained. Persisted dispatch `OccurrenceID`, frame `StepIDs`/child index, definition digest and dynamic resolution pin are authority; UI must not join by bare step ID.
Metadata belongs to the actual frozen action: substituted wrapper and child each have their own tool/action binding. Dynamic children use committed `Pin.ExecutableClosure` plus frame relation; absent linkage => `unresolved-dynamic`, never today's registry.
Redact/classify **before IPC, tokenization, caches and copy** using existing trace/debug/governance protection and graph authored-value redaction. A declaration does not grant read permission; unknown safety => omitted/unavailable. Do not unredact persistence.
For read-only persisted inspection add `yawr preview --format graphjson --run-dir <dir> --run-id <id>` (no positional source). Load original plan/state, never Resume/lease/execute; reject mixed/torn generations, preserve integrity checks.
This and `GET /runs/{id}/document` return frozen graph plus optional root `presentation_state:{run_id,plan_snapshot_digest,checkpoint_sequence,occurrences:[Occurrence]}`. Registry lookup may fall back to the configured run store for completed runs after restart.
`Occurrence={identity:{qualified_node_id,frame_id?,frame_step_index?,invocation?,retry_attempt?,occurrence_sequence?,event_id?,dispatch_occurrence_id?},details:StepDetails,output:object,output_value_status:object}`; all identity counters are integers copied from persisted facts, never fabricated.
`details.code_presentation` pins each occurrence; output is an ephemeral safe projection of retained frame/root results or trace history, not new persistence. Omit unavailable attempts rather than attach the latest output to every retry.
`presentation_state` is a runtime overlay excluded from structural graph hash like existing runstate. Immutable graph metadata remains structural EXCEPT ONLY `StepDetails.code_presentation.plan_snapshot_digest` (`nodes[].data.details.code_presentation.plan_snapshot_digest` in GraphJSON), the final self-reference through snapshot graph hashing. All descriptor/version/origin/tool/action identity fields remain structural. Never recursively exclude arbitrary keys with that name. Inject the actual final session snapshot blob digest through `bindHandoffGraph` after plan finalization; the existing full `bound_content_hash` covers the injected field, and consumers must require equality with the committed `execution_plan_hash`. Runtime events/run-document overlays use the run store's internal `SnapshotDigest`; the session blob binding is a distinct existing digest namespace. SnapshotDigest, serialized revision and full-content hashes remain unchanged. Old metadata-free graphs remain unchanged. SSE checkpoint/event changes trigger refreshing this projection.
Final direct/session stdio, HTTP-event and saved-inspection projections must reconcile output status after existing protection and preview budgets: `available` implies an approved string in that same payload. Rejected/absent/non-string output must never be reconstructed to satisfy this invariant. Approved code strings may be projected into existing output slots within the existing per-entry and total preview budgets.
Dynamic `execution-flow-closure/v3` retains exact action definitions captured by the executable materializer before resolution commit. Readers validate the closure digest and bind its tools through the recorded frame/occurrence/revision; no post-commit catalog fallback.
Current run-state, session frame projections, and plan/frame/result records remain bound to their committed data.

## 5. Snapshot/toolchain contract

Write and read only `execution-plan/v3`. Version preflight precedes lease/state mutation, then rechecks under lease; reject duplicate/unknown versions, trailing data, and corrupt digests.
The extension may author with its bundled helper independently of the configured execution binary. Execution requires a runtime advertising the current plan contract.
Exact verification command: `yawr presentation capabilities` returns `{"schema_version":"yawr.presentation-capabilities/v1","resolver_version":"yawr.core-binding/v1","execution_plan_read":["execution-plan/v3"],"execution_plan_write":["execution-plan/v3"]}`. Exit failure or a missing capability blocks metadata-bearing execution, not editor highlighting.
Tool/provenance/plan digests can change with metadata; dispatched arguments, permissions, capture and interpolation do not. Checkpoints use `run-state/v3`.

## 6. Ownership and integration acceptance

| Owner | Exclusive writes / delivery |
|---|---|
| Runtime | Go/schema/runtime models, shared resolver/IO, CLI, graph projections, snapshot preflight, trace/result safety, docs/specs and Go tests. |
| Extension | Package/dependencies/helper packaging, TS resolver client, worker, YAML mappings/decorations, graph/session inspector, and JS/native/browser tests. |
| Shared | The same JSON fence is consumed in producer/consumer tests; actual Go-produced replies, graphs, and frames enter real parser/rendering tests. |

The extension supplies offline `static\highlighting\highlighter.js` and `worker.js` plus preview HTML integration. The runtime serves the embedded assets with explicit JS MIME and fixed allowlists.
One tokenizer implementation is shared across editor and browser with no CDN or dynamic remote imports. The extension build owns all JS packaging; there is no parallel Go-side frontend toolchain.
Acceptance: actual resolver binding parity (package/path/bare ambiguity, lexical scopes, external roots, unsaved definitions/new buffers), no provider/auth/network/tool startup; invalid descriptor and missing dependencies explicit.
Acceptance: real Go UTF-16 ranges consumed by editor for emoji/CRLF/literal/folded/quoted/escaped scalars; stale/cancel/limit fallbacks; YAML completions/diagnostics and rival semantic provider preserved; no feature-owned semantic provider.
Acceptance: actual native/substituted/nested/dynamic runs publish descriptor fields through direct inspector, session stdio/replay and standalone preview; restart with source deleted/catalog language changed retains frozen colors and safe output.
Acceptance: retries/iterations do not bleed values; unavailable resolved inputs clearly labelled; secret markers absent from token inputs/IPC/copy; unknown language/version/plain fallback, light/dark/high-contrast and disable setting tested.
Acceptance: metadata-free v1 digest regression, metadata v2 round-trip, preflight before mutation, original v1 reads, mixed-version refusal, no mandatory checkpoint upgrade. Coordinator rebuilds/packages only after both deliveries; independent review follows.
