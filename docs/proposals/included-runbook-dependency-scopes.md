# Included-runbook dependencies and lexical tool scopes

**Status: IMPLEMENTED - 0.2.17 Windows review candidate. Not publicly released.**

**Implementation gate:** Owner signoff authorizes the build. Keep `PKG-017`
guards until the replacement execution paths satisfy B5. Artifact publication,
runbook migration, and release acceptance require the remaining delivery gates;
approval to build is not release acceptance.

Source baseline inspected: `e06c0de82b348e7ef805202984ad3a4aacae6b94`.

## 1. Approved decision

Allow every runbook, including an included runbook, to declare its own
`requires:` and `toolRefs:`. A child must mean the same thing when called by
different parents under the same approved project/package configuration.

Replace the current blanket rejection with this rule:

> Resolve the complete eligible dependency set before execution. Bind each
> runbook's tool names in that runbook's own scope. Freeze the selected
> definitions. An include cannot inherit its caller's private tool bindings,
> change an existing binding, or introduce a new dependency during execution.

The owner approved these concrete choices together:

| Decision | Approved first-release behavior |
| --- | --- |
| Child declarations | Supported for static eager/lazy/auto and dynamic catalog includes, transitively |
| Package versions | One selected version/source per package name per run; all constraints must hold |
| Tool names | Same local name may refer to different definitions in different files |
| Dynamic eligibility | If a reachable dynamic include exists, preflight all exported runbooks in the resulting approved catalog, to a fixed point |
| Lazy execution | May defer expansion/execution, but not dependency validation or definition freezing |
| Legacy child inheritance | Reject reliance on an ancestor's private alias with a migration diagnostic; never silently choose another tool |
| Publication | One coordinated runtime/VSIX release, after runtime and installed-editor acceptance |
| Exclusions | No runtime package installation, multi-version package isolation, implicit parent-alias inheritance, or new dynamic debugger promise |

The dynamic-eligibility decision deliberately trades startup work and earlier
errors for a frozen, predictable run. An invalid eligible export can block a run
even if that particular target would not have been selected. Static-only runs
do not preflight unused runbook exports merely because a package exports them.

## 2. Current implementation and reuse

| Surface | Current behavior | Design consequence |
| --- | --- | --- |
| `runtime/cmd/yawr/include_closure.go` | Static include traversal rejects nonempty child `requires`/`toolRefs` with `PKG-017`; skips dynamic includes | Replace the guard only after the shared resolver is wired end-to-end |
| `runtime/cmd/yawr/run.go`, `session.go` | Entrypoint-oriented package construction and binding | Both need the same closure operation |
| `runtime/pkg/pkgcatalog/catalog.go` | Builds tiers 0-3; combines project and root constraints; retains source provenance and digests | Extend to declarations from multiple owning documents, not concatenated path strings |
| `runtime/pkg/pkgcatalog/config.go` | `BindFile` already creates file-local bindings, supports explicit package/path selection and containment rules | Reuse it; do not implement a second tool-name resolver |
| `runtime/pkg/pkgcatalog/bind.go` | Project/package-map source-binding merge | Preserve its explicit override boundary |
| `runtime/internal/adapter/dynamicloader.go` | Checks child package/tool names against catalog membership, not a complete lexical scope | Consume a prevalidated target scope and frozen closure instead |
| `runtime/pkg/run/run.go` | Child loader registers `toolRefs` in a shared registry | Replace mutation with scope propagation; align SDK and CLI |
| `runtime/internal/tool/overlay_registry.go` | Name overrides can affect unrelated consumers | Do not use mutable overrides for scoped child dispatch |
| `runtime/pkg/engine/run.go` | `ExecutionPlan.Tools` is keyed by a name; `ResolvedStep` lacks a binding identity | Add explicit scope/binding identity without changing authored step IDs |
| `runtime/internal/plansnapshot` | Current executable snapshot is `execution-plan/v3` | Introduce a versioned scoped snapshot and explicit legacy handling |
| `runtime/pkg/presentation/resolve.go` | Uses file-local `BindFile`, but catalog requirements come from the entrypoint | Share the new dependency closure and preserve metadata-only operation |
| `runtime/internal/executor/tool_materializer.go` | Freezes tool substitution runbooks using name-based tool lookup | Substitution definitions and their nested tools must carry lexical ownership too |

This is not just removal of two validation errors. The planner, executor,
frozen snapshot, debugger metadata, and authoring resolver must agree on the
same binding.

## 3. Author-facing contract

### 3.1 A child owns its declarations

Illustrative existing YAML syntax; this section is not a runnable fixture:

```yaml
# runbooks/router.runbook.yaml
apiVersion: yawr.runbook/v1
id: router
flow:
  - step:
      id: investigate
      type: include
      include:
        runbook: ../investigations/database.runbook.yaml
```

```yaml
# investigations/database.runbook.yaml
apiVersion: yawr.runbook/v1
id: database-investigation
requires:
  - package: database-tools
    version: "^2.0.0"
toolRefs:
  - name: query
    package: database-tools
flow:
  - step:
      id: inspect
      type: tool
      tool: {name: query, action: inspect}
```

The approved project configuration supplies the location of `database-tools`,
or the runbook author supplies a valid relative `requires[].path`. The router
does not duplicate the child's dependencies. The operator does not edit inputs,
binary paths, or package maps to satisfy a normal packaged example.

Configuration still has a role: a package name is not a download location.
Without an approved source binding or an authored local path, resolution fails
with the existing missing-package diagnostic. This feature does not invent a
package registry or download dependencies.

### 3.2 Scope and name lookup

Each source document has an immutable tool scope, distinct from its execution
invocations and variable scopes.

* A file's explicit `toolRefs` binds names only in that file.
* A sibling or parent can bind `query` differently without altering this file.
* Duplicate declarations of one local name in one file are errors, even if they
  happen to resolve to identical definitions.
* Built-in tools remain available under the existing built-in naming rules.
  An explicit local binding is resolved using existing explicit binding rules.
* A non-built-in, unqualified tool call in an included/substitution file must
  have a `toolRefs` declaration in that file. There is no ancestor fallback.
* An explicit package-qualified tool call can bind directly to that exact
  export in the frozen catalog, subject to existing package/profile authority.
  It is recorded as a binding just like a `toolRefs`-bound call.
* Existing implicit entrypoint tool discovery remains supported, but its
  selections are materialized into the root scope before execution. They
  are not inherited by children.
* Branch/iterate/parallel/compensation bodies authored in the same file retain
  that file's scope. Crossing an include or substitution source-file boundary
  switches to the target file's scope.

Thus a formerly accepted child that relied on a root-only alias must add its
own declaration. Do not synthesize it by copying the parent's binding: doing so
would preserve the very caller-dependent meaning this proposal removes.
For migration safety, also diagnose an unqualified child call that would now
select a built-in while an ancestor privately rebinds that name to a different
definition. Require an explicit local declaration to disambiguate; do not
silently change a previously executing call to the built-in.

### 3.3 Package constraints and paths

Collect all package declarations with their owning document and declaration
location. The merged package set is run-wide; the tool alias map is file-local.

For each package name:

1. Reject duplicates within one declaration list using existing `PKG-005`
   semantics. Repetition across different files is valid.
2. Intersect every version constraint from project configuration and eligible
   documents. Validate the selected manifest version against every constraint;
   there is no network search for a satisfying version.
3. Resolve each authored path relative to its actual declaring file, or the
   established project/package-map base. Preserve `pkgpath` rules rather than
   rebasing all child paths to the root.
4. All nonempty source claims must identify the same source. Different textual
   paths to the same filesystem identity are allowed; conflicting sources fail
   with `PKG-024`, even when manifests claim the same name/version.
5. An explicit package map continues to replace the project binding as it does
   today. It does not silently replace a conflicting path pinned in any
   runbook. All retained version constraints still apply.

A package-owned file cannot escape its owning package through `toolRefs.path`,
static include, substitution, or a nested authored dependency path. A dependency
on another package is expressed by package identity and satisfied through its
approved binding. Workspace-owned external roots retain the existing
`PKG-W003` advisory and provenance treatment. Keep absolute-path restrictions,
symlink containment, case handling, and Unicode normalization consistent with
the existing path-kind implementation.

Report version conflicts with every contributing file, source span, constraint,
selected manifest version, and dependency chain. Do not silently prefer root,
last-loaded child, or first registry entry.

## 4. Shared preflight algorithm

Implement the orchestration in `runtime/pkg/pkgcatalog`, keeping engine/runtime
dependencies out of that package. Use an injected parser/source reader and
existing path, semver, and catalog primitives. CLI, SDK, sessions, preview, and
authoring adapt their source readers to this operation.

### Inputs

Entrypoint identity, workspace identity, effective project/package-map bindings,
approved catalog contributions, source reader, parser, and resource limits.
Authoring may supply unsaved buffers through its existing overlay reader.
Execution uses validated saved sources. Neither reader may silently fall back
to another document version.

### Ordered stages

1. **Discover static closure.** Parse the root and follow all authored static
   includes, including nested branch/loop/compensation bodies and lazy includes.
   Collect declarations and source ownership without executing expressions,
   providers, tools, includes, or approval prompts.
2. **Resolve package sources.** Add newly declared packages using the
   multi-document constraint/path rules. Index exports and retain the exact
   bytes, source provenance, and definition digests read.
3. **Discover dynamic candidates.** When any reachable document has a catalog
   include, add all exported runbook identities in the current approved
   catalog. Do not evaluate the runtime selection expression or search the
   filesystem using runtime values. Candidate declarations may add packages
   before freeze; their exports become candidates as well.
4. **Discover substitutions.** For tools reachable through binding or catalog
   eligibility, collect their authored substitution runbooks and static
   descendants. Include their dependency declarations and source ownership.
   Do not invoke the substituted action.
5. **Reach a fixed point.** Repeat discovery, package aggregation, and
   provisional binding until no new document, package, declaration, or
   substitution is found. Resolve shared constraints from the complete set,
   not traversal order. A provisional result is never executable or published
   as `catalog/frozen`.
6. **Bind every eligible file.** Apply `BindFile` and the explicit-call rules
   against the final catalog. Validate action signatures, defaults, profiles,
   source containment, and frozen substitution closure using that exact scope.
   Previously provisional ambiguities must be re-evaluated after closure.
7. **Seal.** Produce a canonical closure digest, final catalog, immutable
   file scopes, frozen tool definitions, and dynamic target descriptors.
   Preserve existing protected-content validation before durable publication.
8. **Start.** Publish existing package/catalog evidence plus scope provenance,
   persist the scoped execution plan, then allow the first runtime step.
   A failed preflight produces no tool dispatch or partially successful run.

Catalog construction must allow different packages to export the same bare tool
name so that explicit file-local package bindings can disambiguate them.
Qualified identity collisions remain catalog errors. Bare-name ambiguity is
reported at an actual implicit/unqualified binding site, retaining `PKG-006`
and `PKG-022` meanings. This intentionally moves some errors from global
catalog assembly to binding; an unused ambiguous bare name is not a reason to
reject two otherwise explicitly bound packages.

### Termination, consistency, and limits

Use canonical source identities and declaration keys for deduplication. A
diamond dependency is parsed once but records every importing edge. A repeated
execution invocation does not create another dependency scope.

Retain the existing static include/substitution cycle and nesting policies
(the shared walker currently defaults to depth 10). A package dependency cycle
is not by itself an execution recursion: traverse it once and accept it only if
source and constraint consistency can be proven. A dynamic include does not
become a recursively expanded executable call during preflight.

Proposed hard ceilings for the new closure operation: 4,096 unique source
documents, 256 package identities, and 64 MiB total captured source bytes.
Existing stricter parser, nesting, snapshot, and transport limits still apply.
Cancellation is checked between reads and discovery iterations. Reaching a
limit is an explicit error, never permission to run with a partial closure.
These are implementation safety limits, not new operator setup requirements.

Use one captured source snapshot throughout preflight. Reusing a path must
reuse its captured bytes, not reread and accidentally mix versions. Verify
content identities again at the existing dispatch/resume boundaries where
required; changed content cannot replace a frozen definition.

Missing source bindings that might be supplied by another discovered declaration
remain provisional while independent work is pending. Once no discovery can
advance, unresolved identities are fatal. Malformed source, prohibited paths,
or two proven conflicting source claims are not treated as retryable
discovery. This prevents declaration order from deciding which graph can load.

## 5. Dynamic execution after freeze

The eligible target set is the fixed-point set from section 4, not arbitrary
files and not every package installed on the machine.

For an actual dynamic include:

1. Render and validate the existing catalog reference syntax.
2. Resolve the exact eligible target; retain existing missing/ambiguous target
   failures.
3. Verify its frozen source/package identities under the existing drift policy.
4. Select its already-validated lexical scope and executable closure.
5. Apply runtime input bindings and existing governance/approval checks.
6. Commit the dynamic invocation pin with target and scope digests before child
   dispatch; then publish its execution graph before dependent child events.

Child `requires` and `toolRefs` are not ignored and do not trigger new package
discovery here. A missing dependency, unknown scope, mismatched digest, or
unfrozen tool is a hard failure, including with `onError: continue`. Preserve
`DINC-010`/`DINC-011` for their applicable missing-dependency cases and add
specific scope diagnostics rather than claiming success from name membership.

An empty included runbook still has a document scope; it simply has no tool
calls. Multiple calls to the same frozen child share definitions and scope but
retain separate runtime frames, inputs, evidence, and graph occurrence IDs.

## 6. Proposed internal data and execution interfaces

Names below are implementation targets, not existing public APIs:

```text
DependencyClosure
  catalog
  documents[DocumentID] -> FrozenDocument
  scopes[ScopeID] -> FileToolScope
  tools[ToolDefinitionID] -> FrozenToolDefinition
  dynamicTargets[qualified runbook identity] -> TargetDescriptor
  declarationProvenance[]
  digest

FrozenDocument
  sourceIdentity, sourceDigest, packageOwner?, staticEdges[], scopeID

FileToolScope
  documentID
  bindings[local name] -> ToolBinding

ToolBinding
  bindingID, localName, toolDefinitionID, declarationSite, selectionProvenance

TargetDescriptor
  documentID, scopeID, sourceDigest, closureDigest

ResolvedStep additions
  lexicalScopeID
  toolBindingID?       # required for tool steps; absent for non-tool steps
```

The existing `ResolvedStep.Scope` concerns other execution semantics; do not
repurpose it. Carry source ownership through parsed flow nodes and frozen
closures so that eager inlining does not erase the child's scope.

IDs are opaque digests over canonical, versioned records. A document identity
includes its normalized source identity, content digest, and owning package
identity; identical bytes in different owning files are not automatically the
same scope. A tool definition identity includes definition and source/package
provenance. A binding identity includes scope ownership, local name, and tool
definition identity. No ID depends on load order, map iteration, runtime input
values, run ID, or include invocation number.

Compute identities in a non-circular order: source/catalog identities first,
then `ScopeID` from model version, document identity, catalog digest and
effective profile digest, then binding IDs. Definition identity hashes the
captured declaration and source provenance, not generated scope references
inside its materialized substitution. The final closure/artifact digest covers
the complete frozen tables and cross-references. Do not recursively hash
`scope -> binding -> substitution -> scope`.

### Invocation boundary

Add a binding-aware execution interface alongside the legacy
`ToolRuntime.Invoke(name, ...)`, conceptually:

```text
InvokeBound(context, BoundToolCall, arguments)
BoundToolCall = {scopeID, bindingID, definitionID, logicalName, action}
```

Both validation/definition lookup and dispatch resolve the same immutable
binding record. The runtime rejects a missing or mismatched record; it must not
fall back to `Invoke(logicalName, ...)` for a scoped plan.

Do not implement this by temporarily overriding a shared registry, holding a
global "current scope", or rewriting authored YAML names to opaque IDs.
Parallel siblings must be able to execute different `query` bindings at the
same time. Tool defaults, output contracts, profile/governance lookup,
transport selection, substitutions, approval evidence, and inspector
presentation must all use the bound definition. Keep the authored logical name
and canonical package/tool identity available to those existing policies.

Existing custom SDK runtimes lacking bound invocation may continue executing
legacy plans. They must explicitly reject new scoped plans until adapted, not
silently degrade to name-only dispatch.

## 7. Persistence, compatibility, and evidence

### Versioning

Introduce `execution-plan/v4` for new plans with scope tables and binding
references, and a proposed negotiation capability
`yawr.lexical-tool-scopes/v1`. Do not place meaning-changing fields in v3 and
assume old runtimes will honor them.

Version every affected executable closure/pin envelope that cannot safely carry
mandatory scope references in its current format. The build's schema inventory
must include plan snapshots, frozen include/substitution closures, dynamic pins,
and handoff child plans. Package locks keep their existing version if their
meaning is unchanged; scope provenance belongs in the new closure/snapshot
record rather than being smuggled into an unrelated field.

Readers keep an explicit v3 branch and never invent child lexical bindings for
an old saved run. Old runs retain their frozen v3 behavior and existing refusal
rules; a saved artifact without sufficient frozen definitions is rejected
rather than repaired from today's filesystem. A v3-to-v4 automatic migration is
out of scope.

New execution entrypoints and the matching VSIX request the new capability.
An old runtime returns an actionable unsupported-capability error. Read-only
preview may still show authored structure, but must not present unverified
bindings as runnable. No new VS Code engine minimum is justified by this work.

### Resume and replay

Persist scope ownership and bound tool definitions with the authoritative
plan and include/substitution closures before dispatch. Resume/replay restore
them, never call `BindFile` to reinterpret old calls against current source.
Current-source drift checks remain separate from restoring the frozen meaning.
An existing package-drift override must not replace a v4 binding: it may record
accepted external drift only where existing policy allows execution of the
original frozen definition. Incompatible drift still refuses execution.

Snapshot hashes cover all binding identities and closure digests. Mutation,
missing references, cross-scope substitution, or unknown versions fail before
the affected provider call. Preserve writer leases, checkpoint ordering,
Results persistence, and cancellation semantics.

### Evidence and authoring

Evidence records the declaring file, local tool name, selected package/tool
identity, version, definition digest, scope/binding IDs, and selection origin.
Record logical calls and runtime occurrences separately; definition deduplication
must not collapse execution history. Apply existing redaction to all
human-readable metadata and never embed runtime secret values in scope records.

The editor resolves the document being inspected using that document's scope
in the selected entrypoint's closure. Standalone inspection computes its own
closure. Both use the same rules; no parent `toolRefs` leaks into child hovers,
completions, graph code presentation, or validation.

Authoring remains metadata-only: no provider startup, authentication, remote
inventory fetch, package installation, or tool execution. It uses existing
overlay readers and reports incomplete/unavailable dependencies explicitly.
Cache dependencies include the discovered sources, manifests, bindings,
project configuration, and package map. Unsaved changes invalidate the proposed
authoring closure, not an active run's frozen closure.

## 8. Diagnostics and migration

Keep existing package errors for missing packages, invalid constraints,
ambiguous bindings, source conflicts, containment, and drift. Introduce a small
`SCOPE-*` family in `errkit` with the following proposed meanings:

| Code | Meaning |
| --- | --- |
| `SCOPE-001` | Included-file tool call lacks a local or explicitly qualified binding |
| `SCOPE-002` | Binding/scope/definition reference is missing, mismatched, or not frozen |
| `SCOPE-003` | Dependency closure exceeds its declared resource limit |

Before reserving numbers in code, check the current error registry and its
documentation for collisions. Every diagnostic includes safe declaration
locations and include/dependency chains; no raw tool arguments or secret values.

For legacy ancestor-alias use, `SCOPE-001` should say which child call is
unbound, identify a matching ancestor declaration if present, and show the
required local-declaration pattern. It must not mutate runbooks, copy aliases,
or install anything. Do not offer a "use parent bindings" bypass.

Do not require operators to configure paths repeatedly. Repository/package
authors own dependency declarations and one approved set of source bindings;
the normal VS Code run command automatically preflights the closure. Existing
explicit package maps remain a supported development/test override.

## 9. Build sequence and ownership

All stages below are **authorized by owner signoff**. Subsequent implementation changes
must keep existing safety guards until their replacement path passes tests.

| Stage | Deliverable and main files | Depends on | Exit criterion |
| --- | --- | --- | --- |
| B1 | Shared declaration/provenance model and multi-file aggregation in `runtime/pkg/pkgcatalog`; path/source tests | Signoff | Deterministic constraints and source conflicts across nested/diamond graphs |
| B2 | Fixed-point static/dynamic/substitution preflight with snapshot reader and bounds; reuse `flowwalk`/`pkgpath` | B1 | Complete eligible closure without executing providers; finite and cancellable |
| B3 | Scope/binding IDs in planner, `pkg/engine`, frozen flow/include/substitution records, and v4 snapshot codecs | B2 | Lossless round trip; reject missing/tampered references; v3 explicit compatibility |
| B4 | Bound lookup/invocation in `internal/executor`, `internal/tool`, SDK child loader and sub-engine adapters | B3 | Conflicting local aliases execute correctly in parallel; no registry mutation/fallback |
| B5 | Wire CLI `run`, sessions, SDK, dynamic loader, route tests, handoffs, dry-run/replay/resume; replace static rejection | B4 | Same binding result through every supported entrypoint |
| B6 | Shared metadata-only resolution in presentation/authoring, graph and inspection projections, capability negotiation | B3, B5 | Editor definition equals dispatched definition; old helper fails clearly |
| B7 | Self-contained examples, documentation/migration diagnostics, runtime and installed-editor qualification | B5, B6 | Acceptance matrix passes; matching runtime/VSIX and evidence ready for review |

B1-B4 are internal preparation and must not advertise child declarations as
supported. B5 cannot remove the guard while any affected execution path still
uses shared child name registration. Release only when B7 is complete.

### Implementation progress

B1 is implemented: multi-document requirement aggregation retains declaring-file
bases, containment, version constraints, and source provenance. Lexical catalog
mode defers bare-name conflicts to file binding without changing legacy callers.

B2 has an opt-in `pkgcatalog.BuildClosure` implementation and regressions for
static eager/lazy/auto traversal, dynamic-catalog fixed points, substitutions,
file-local bindings, ancestor/built-in migration conflicts, captured source
reads, cancellation, and resource/depth bounds. Its document IDs are discovery
keys, not the final B3 executable identities. A returned closure is preflight
metadata, not authorization to dispatch a tool.

B3-B6 are implemented and qualified on Windows: immutable scopes and bound invocation, versioned snapshots and
closures, shared CLI/SDK/session/serve preparation, scoped replay/inspection,
metadata-only child resolution with dirty overlays, and helper capability
negotiation. New runs use scoped preparation rather than the blanket `PKG-017`
rejection. Explicit legacy saved-run readers remain separate.

The 0.2.17 candidate passes 4,413 runtime tests, with 36 existing/platform skips,
28 source-host tests, and 6 installed-VSIX tests on VS Code 1.136.2.
All 647 extension/webview unit tests pass. Installed frame sampling verifies all 15 scoped execution occurrences,
seven retained runbooks, ordered Results, and a minimum observed dwell of
505.8 ms at the 500 ms setting. Repeated visits to an include share a graph site
but retain distinct execution occurrence identities.

Linux compilation passes; Linux execution/CI and final operator review remain
unverified. Hide/reopen or cancellation during graph expansion, real downstream
external view startup, and final orchestration integration remain manual acceptance cases.
Dynamic debugger Step Into remains outside this feature. The accepted pacing
implementation is unchanged. Five self-contained B7 fixtures, including dynamic
includes inside parallel branches and tool-backed runbooks, are under
[`runtime/examples/dependency-scopes`](../../runtime/examples/dependency-scopes/README.md).
The matching Windows runtime/VSIX pair is a local review candidate, not a
published release.

Handoffs start separately planned child runs and must receive a freshly
validated child closure under the session's approved binding context; they do
not inherit the prior run's mutable aliases. Preserve the existing distinction
between a handoff and an include.

## 10. Acceptance matrix

Use safe fixture tools that return distinct fixed markers and record invocation
counts, with no external services or credentials. Share fixture sources between
CLI, SDK, source-host, and installed-VSIX tests where possible.

| ID | Scenario | Required assertion |
| --- | --- | --- |
| A01 | Static child has both declarations; parent has neither | Successful run; selected child definition is evidenced |
| A02 | Same fixture eager/lazy/auto, including nesting | Identical bound definitions and dependency digests |
| A03 | Child and parent bind `query` to different packages | Each gets its own expected marker, before and after child return |
| A04 | Parallel siblings bind the same name differently | Correct markers in every lane; no ordering dependence over 100 repeats |
| A05 | Repeated child and diamond dependency | Definitions deduplicate; execution occurrences do not |
| A06 | Compatible constraints across three files | One selected package satisfying every constraint; all sites recorded |
| A07 | Incompatible constraints or source paths | Preflight refusal naming all conflict sites; zero tool invocations |
| A08 | Duplicate declaration in one file | Explicit duplicate error; cross-file repetition remains valid |
| A09 | Same relative path string in different files | Correct declaring-file bases; no root rebasing |
| A10 | Package escape, symlink alias, Windows case/Unicode cases | Existing containment rules hold; equivalent identities deduplicate |
| A11 | Explicit package map and child source pin | Override project binding only; conflicting authored pin is not ignored |
| A12 | Same bare export name across packages | Explicit package/path binding succeeds; ambiguous bare binding fails |
| A13 | Child relies on root-only alias | `SCOPE-001` with migration location; no implicit inheritance |
| A14 | Child used standalone and by two parents | Same definition under the same approved source configuration |
| A15 | Dynamic target declares additional preflight dependencies | Dependencies join closure before freeze; no root duplication needed |
| A16 | Nested dynamic targets add packages/exports | Fixed point reached, deterministic ordering/digest |
| A17 | Invalid eligible export never selected at runtime | Run refuses before execution, per approved eligibility policy |
| A18 | New/changed target or tool after freeze | Refusal or original frozen behavior under existing drift policy; never rebinding |
| A19 | Dynamic missing dependency with `onError: continue` | Hard failure; no child provider invocation |
| A20 | Substitution contains nested includes/tools | Exact source-local binding throughout; no name-only materializer lookup |
| A21 | v4 save/resume/replay; v3 fixture; old runtime | Exact scoped restoration; v3 behavior preserved; unsupported v4 refused |
| A22 | Missing/tampered/cross-scope binding reference | Refusal before provider dispatch |
| A23 | Metadata-only authoring and unsaved overlays | Correct child binding, precise invalidation, zero provider/auth/network activity |
| A24 | Child dependency is missing in hover/preview/run | Consistent identity/reason, not contradictory resolved editor metadata |
| A25 | Protected defaults, arguments, outputs, and definitions | No new disclosure in scopes, snapshots, events, graphs, or diagnostics |
| A26 | Closure cycle/limit/cancellation and failed preflight | Bounded termination, explicit error, no partial frozen catalog |
| A27 | CLI, SDK, session, route-test and graphical run | Same selected definition and policy outcome; route-test dispatch rules unchanged |
| A28 | Installed graph with nested/repeated child bindings | One CURRENT stream, canonical order through Results, every 500 ms dwell >=465 ms, no transient gap |
| A29 | Urgent failure/cancellation/blocked state and prompts | Existing immediate bypass, Results persistence, hide/dispose behavior unchanged |
| A30 | Tool defaults, profile rules, native/helper transports | Validation and actual invocation use the same definition; all existing isolation tests pass |

Keep the existing graph examples unchanged as regression inputs. Add a separate
`runtime/examples/dependency-scopes` project containing local package
fixtures, own-dependency child, conflicting local aliases, repeated/parallel
calls, dynamic target, and intentionally failing conflict/inheritance examples.
Include its project bindings so the operator only opens the example and runs it.

Run focused package tests during each stage, then `go -C runtime test ./...`,
`npm run extension:test`, `npm run extension:e2e`, and
`npm run extension:validate:vsix`. Run extension lifecycle commands serially.
Use actual VS Code 1.136.2 and the packaged helper for installed qualification;
frame-sample the graph, not just protocol events. Exercise supported non-Windows
runtime CI as well as Windows path/installed-host cases. No skipped or loosened
assertions substitute for qualification.

Final build delivery must include the source commit, matching runtime/VSIX
paths and SHA-256 values, exact test counts, compatibility results, and any
remaining installed integration/manual-only cases. Real external view acceptance
remains downstream-owned and is not proved by local marker tools.

## 11. Risks and deliberately deferred work

* **Larger preflight:** a dynamic catalog can expose many candidate runbooks.
  The first release bounds and caches metadata discovery; a narrower authored
  target allowlist is a future language proposal, not an implicit optimization.
* **New early failures:** lazy/unselected eligible targets are validated earlier.
  This is intentional and must be documented, not hidden behind silent pruning.
* **Scope propagation:** losing source ownership during eager expansion,
  substitution, resume, or graph projection can silently select the wrong tool.
  A missing scope is always an error in v4.
* **Legacy SDK adapters:** name-only runtimes require an explicit adapter update.
  Do not offer an unsafe scoped-to-global compatibility shim.
* **Multiple versions:** isolated versions of one package within one run require
  a separate package identity/isolation design. This release rejects conflicts.
* **Debugger protection:** a frozen dependency closure may improve analysis, but
  this proposal does not enable dynamic Step Into or weaken protected-content
  checks. That behavior needs its own verified design and acceptance.
* **Runtime install/network discovery:** dependency declarations are not
  permission to fetch packages or authenticate to providers.

## 12. Approval record

Owner decision: **APPROVED** by "do it" on 2026-09-15.

Approved revision/commit: the document reviewed immediately before that signoff,
based on `e06c0de82b348e7ef805202984ad3a4aacae6b94`; the proposal was not yet committed.

Implementation authorized: **YES**

Release/merge authorized: **NO**

Record subsequent owner-approved amendments here. Any change to the decisions
in section 1 requires renewed review rather than being left to implementation.
