# Region Manifest v1

Status: current contract
Scope: a structural overlay on a yawr runbook that lets renderers group
yawr nodes into domain-shaped units without changing engine semantics.

## Goal

Domain kits compile higher-level operations (e.g. `ops.approval`,
`ops.incident`) into one or more yawr steps. Renderers today see only
the resulting yawr structure — `human_task`, `branch`, `iterate`, etc.
A region manifest lets a kit declare "these N yawr nodes belong to one
domain operation." Renderers can then collapse them into a single
domain node, expandable on demand to reveal the underlying yawr.

The engine is unaware of regions. Manifest presence does not change
execution.

## Non-goals

- v1 does not translate yawr vocabulary. Pure yawr nodes (iterate,
  branch, parallel, …) outside any region render as yawr.
- v1 does not specify renderer styling or interaction. It defines only
  the data contract.
- v1 does not let regions modify trace events. Events remain keyed by
  the underlying yawr node IDs.

## Embedding

The manifest is embedded in the runbook YAML as a top-level `regions:`
block. Single artifact, single source of truth.

```yaml
runbook: my-runbook
steps:
  - id: step_a
    type: human_task
    ...
  - id: step_b
    type: tool
    ...

regions:
  schema_version: "1"
  regions:
    - id: approve_change_42
      kit: dri
      op_type: ops.approval
      op_id: approve_change_42
      label: "Approve change 42"
      members: [step_a, step_b]
      entries: [step_a]
      exits: [step_b]
      display: collapsed
      status_rule: default
```

Parsers that don't understand `regions:` ignore it; the engine is one
such parser. Tools that do understand it (renderers, report generator)
read it and validate it.

## Region

| Field           | Type            | Required | Notes |
|-----------------|-----------------|----------|-------|
| `id`            | string          | yes      | Unique within the manifest. |
| `kit`           | string          | yes      | Source kit slug (`dri`, `home`, …). |
| `op_type`       | string          | yes      | Kit-defined op type (`ops.approval`). |
| `op_id`         | string          | yes      | Kit-side identifier of the source op. |
| `label`         | string          | yes      | Human-readable, kit-localized. |
| `members`       | []string (node IDs) | yes  | All yawr nodes belonging to the region. |
| `entries`       | []string (node IDs) | yes  | Members with at least one inbound edge from outside the region (or the runbook entry point). May be empty if region is dead code. |
| `exits`         | []string (node IDs) | yes  | Members with at least one outbound edge leaving the region (or the runbook end). Plural is normal — branches commonly produce multiple exits. |
| `display`       | enum            | no       | `collapsed` (default), `expanded`, `always-expanded`. |
| `status_rule`   | enum            | no       | `default` (see below) or a kit-defined rule name. |
| `skip_reason`   | string          | no       | Set if the region is intentionally empty (`members: []`). |

## Constraints (validated at compile/parse time)

A manifest is valid only if all four hold. A renderer may assume them.

1. **No node sharing across regions.** Every `node_id` appears in at
   most one region's `members`. If two ops do "the same thing," the
   compiler emits the action twice.
2. **Structural contiguity.** The subgraph induced by `members` must be
   weakly connected through edges that stay inside the region or pass
   through declared `entries`/`exits`. No interleaving of regions.
3. **Entries and exits are explicit and accurate.**
   `entries ⊆ members`, `exits ⊆ members`, and every cross-region edge
   must terminate at a declared entry or originate at a declared exit.
4. **Status rule is recognized.** Either `default` or a name the kit's
   renderer registers.

Additionally:

- `members` may be empty only if `skip_reason` is set.
- Regions may nest implicitly (a member of region A may itself be a
  yawr structural node — iterate/branch — whose body contains
  members of region B). Nesting through `members` is not declared
  separately; renderers infer it from graphdoc structure.

## Status aggregation

Given the live `runstate.State`, each region has a status derived from
its members:

| Members include                        | Region status |
|----------------------------------------|---------------|
| any `failed`                           | `failed`      |
| any `running`                          | `running`     |
| all `skipped`                          | `skipped`     |
| all `completed` or `skipped`           | `completed`   |
| any `pending`, none of the above       | `pending`     |
| any `cancelled`, none of the above     | `cancelled`   |

This is `status_rule: default`. A kit may register a custom rule by
name (e.g. `dri.approval_quorum`) that consumes member states and
returns one of the standard statuses. Custom rules live in the kit's
renderer code, not in the manifest.

## Edges crossing regions

A renderer collapsing region R draws:

- one inbound edge for each unique source-region of edges terminating
  at any node in `entries`;
- one outbound edge for each unique target-region of edges originating
  at any node in `exits`.

Edges between two members of R are hidden when R is collapsed and
shown when R is expanded.

## Skipped regions

If every member ended `skipped` (e.g. a `compensate` block that didn't
fire), the region's status is `skipped`. Renderers should grey it out
but not hide it; auditability requires the region remain visible.

## Empty regions

A region with `members: []` and a `skip_reason` records that an op was
elided at compile time (e.g. severity-gated). Renderers show a single
greyed marker "elided: <skip_reason>". The op is still present in the
report.

## Authoring

Domain authors do not write `regions:` by hand. Kit compilers emit it.
A hand-authored runbook may omit `regions:` entirely; it then renders
as pure yawr.

## Validation

`yawr validate <runbook>` runs the four constraints when `regions:` is
present. Failures block compilation.

## Versioning

`schema_version: "1"`. Breaking changes bump the major version.
Renderers receiving a version they don't understand fall back to pure
yawr rendering and surface a one-line notice.

## What this enables

- VS Code workflow view shows DRI ops as collapsed domain nodes,
  expandable to reveal yawr.
- ASCII and TUI renderers do the same in their own idioms.
- The markdown report groups trace events under their op of origin
  rather than per-step.
- Future kits inherit the rendering and reporting story for free.
