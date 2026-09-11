# Investigation Sessions and Cross-Runbook Scenarios

Status: design proposal
Date: August 2026

Related design: [routes-through-step-design.md](routes-through-step-design.md)

## Objective

An operator investigation often starts in one runbook and discovers that a
different runbook owns the next diagnosis. Yawr must let the operator continue
without losing completed work, evidence, graph context, or route analysis.

The product must support two different forms of continuity:

1. Resume an interrupted investigation without rerunning completed work.
2. Start a new investigation from a saved scenario, substituting reviewed saved
   boundary results until a selected live execution point.

The VS Code surface must render every traversed runbook as one connected,
selectable graph. Selecting a node in a later runbook and showing routes to it
must retain the path through earlier runbooks and their handoff edges.

## Operator Example

An operator starts in GEODR0001 and records that an Update SLO workflow is
active. GEODR0001 hands the investigation to GEODR0004.

The session graph becomes:

```text
GEODR0001 executed route
  -> active workflow answer: Update SLO
  -> handoff: investigate active Update SLO
  -> GEODR0004 graph
```

If the operator selects a later GEODR0004 XTS or Kusto step and chooses
`Show routes to this step`, the result includes:

```text
the actual historical route through GEODR0001
+ the committed handoff edge
+ the structurally valid GEODR0004 routes to the selected step
```

The operator may then save this as a scenario. A later `Replay to this step`
uses reviewed saved data for all preceding external boundaries, performs zero
external dispatch, and pauses immediately before the selected step.

## Terms and Identity

### Session

A durable investigation journey. It owns the ordered topology of runbook
segments, transitions, attempts, and final outcome.

`session_id` is stable for resume. Replay and fork create a new session ID and
record provenance to the source session or scenario.

### Segment

One logical invocation of one runbook in a session. Ordinary includes remain
nested inside their owning segment. A first-class handoff creates a new
segment.

Every re-entry into the same runbook creates a different `segment_id`. This
prevents node collisions and represents loops between runbooks without making
identity ambiguous.

### Run attempt

One execution attempt for a segment. It uses the existing `run_id` identity.

- Resume continues the same attempt and run ID.
- Replay creates new session, segment, and run IDs.
- Continuing live after replay keeps the segment but creates a new live run ID
  with `continued_from_run_id` pointing at the replay attempt.

### Transition

An immutable directed edge from an exact source step in one segment to the
entry of a target segment. A transition records the reason, transferred
context, evidence references, and engine-generated provenance.

### Global node identity

Within storage, a node is identified by the tuple:

```text
(session_id, segment_id, qualified_node_id)
```

The derived composite GraphJSON document encodes this tuple into globally
unique node, edge, frame, and group IDs. Authored IDs remain available as
`step_id`. The qualified node ID includes the include call path and remains
distinct when repeated includes contain the same authored step ID.

### Execution occurrence identity

A graph node is an immutable definition. Each replayed or live execution of
that definition is a separate occurrence identified by:

```text
(session_id, segment_id, run_id, qualified_node_id,
 phase, invocation, retry_attempt, occurrence_sequence)
```

`run attempt` means one segment execution identified by `run_id`.
`retry_attempt` means the existing per-step retry number. These are never both
called `attempt` in the session protocol.

The graph renders one definition node with an ordered occurrence history. Its
default status comes from the active run attempt, or the latest occurrence when
the segment is historical. The inspector can select earlier replay and live
occurrences independently.

## Status Models

Session status:

```text
active | paused | resolved | escalated | failed |
cancelled | abandoned | indeterminate
```

Segment status:

```text
prepared | active | paused | handed_off | completed |
failed | cancelled | indeterminate
```

Run-attempt status:

```text
starting | running | waiting | paused_at_boundary | handoff_pending |
completed | failed | cancelled | indeterminate
```

`handed_off` is not `completed`. It means the segment intentionally transferred
control while the containing session remained active.

`handoff_pending` means the source attempt durably requested a transition but
the session journal has not yet committed the target topology.

## Canonical Data Model

### Session record

```json
{
  "schema_version": "yawr.investigation-session/v1",
  "session_id": "...",
  "status": "active",
  "root_segment_id": "...",
  "active_segment_id": "...",
  "active_run_id": "...",
  "sequence": 42,
  "manifest_revision": 8,
  "writer_epoch": 4,
  "creation_command_id": "...",
  "creation_digest": "sha256:...",
  "created_at": "...",
  "updated_at": "...",
  "derived_from_session_id": "...",
  "derived_from_scenario_id": "..."
}
```

Only one segment may be active in v1. Forking creates a new session instead of
adding concurrent mutable branches to one session.

### Segment record

```json
{
  "segment_id": "...",
  "ordinal": 2,
  "runbook_id": "GEODR0004",
  "runbook_name": "GEODR0004 - Stuck update SLO",
  "entry_selector": { "step": "$entry" },
  "status": "active",
  "plan_hash": "sha256:...",
  "graph_hash": "sha256:...",
  "executable_snapshot_hash": "sha256:...",
  "catalog_digest": "sha256:...",
  "package_lock_digest": "sha256:...",
  "profile_digest": "sha256:...",
  "attempt_run_ids": ["..."],
  "executable_revision": 3,
  "graph_revision": 3
}
```

Each segment stores immutable snapshots of:

- its executable plan closure;
- its GraphJSON definition;
- package, catalog, dynamic-include, tool, and profile locks;
- the definition metadata needed by the historical inspector.

Historical definitions never reload from a subsequently edited YAML file.

### Transition record

```json
{
  "transition_id": "...",
  "status": "committed",
  "source_segment_id": "...",
  "source_occurrence": {
    "run_id": "...",
    "qualified_node_id": "route_active/follow_active_update_slo",
    "call_path": [],
    "step": "follow_active_update_slo",
    "phase": "execute",
    "invocation": 1,
    "retry_attempt": 1,
    "occurrence_sequence": 12
  },
  "target_segment_id": "...",
  "target_runbook_id": "GEODR0004",
  "reason_code": "active-update-slo",
  "reason_summary": "Continue with the active Update SLO investigation",
  "context_bindings": {
    "environment": "environment",
    "logical_server": "logical_server",
    "database": "database",
    "incident_id": "incident_id"
  },
  "fact_refs": [
    { "name": "active_workflow", "source": "active_workflow" }
  ],
  "context_digest": "sha256:...",
  "idempotency_key": "...",
  "committed_at": "..."
}
```

The engine supplies source runbook, source step, source run ID, session ID,
timestamp, and transition ID. YAML authors cannot forge these fields.

Only explicitly bound values may enter the target runbook. Target input
declarations and enum constraints are validated before the transition commits.
Secrets are stored as references to a protected source, never as transition or
scenario values.

### Run-attempt record

A run attempt records:

- run ID, mode, status, actor, and client;
- exact executable-plan snapshot;
- exact structured execution cursor set;
- committed event sequence;
- persisted invocation and retry counters;
- pending interaction and idempotent answer state;
- ordered execution occurrences;
- external dispatch intents, results, and counts;
- continuation provenance.

## Durable Execution Foundation

Session work must not build on the current `CurrentStepIndex` checkpoint alone.
The following foundation is required first.

### Executable plan snapshot

Add a versioned, serializable plan DTO containing resolved steps, include
closures, tool definitions, governance, input metadata, and dependency locks.
Runtime-only interfaces and functions are reconstructed from validated types.

Graph snapshots and executable snapshots are separate. GraphJSON is for
inspection; it is never treated as executable state.

Static content is captured in revision 1. A dynamic include cannot execute from
an unresolved snapshot. Before its child starts, the coordinator durably pins
the resolution and appends a new executable-plan revision and graph revision.
Resume and replay use that committed pin and never resolve it again.

### Structured cursor set

Persist every active execution location. A serial flow has one cursor. A
parallel flow has one cursor per active branch plus its deterministic join
barrier state. Each cursor contains:

```text
qualified node ID and call path
branch ID and structural frame stack
iteration index
step ID
phase: before | execute | after
invocation ordinal
retry attempt and delay state
```

Branch result commits are serialized by the commit authority. Resume restores
the cursor set and join state rather than restarting a parallel container.

### Single commit authority

Introduce one required `CommitExecutionMutation` authority. Standalone runs use
a run-local journal implementation. Session-managed attempts use the session
journal implementation exclusively; run snapshots, traces, and manifests are
recoverable projections and are not independently authoritative.

Every state transition that affects execution or control flow is committed,
including completed, skipped, denied, tolerated failure/goto, waiting, paused,
indeterminate, and handoff results. One mutation stores:

```text
settled or waiting step result
+ resulting variables and captures
+ next structured cursor set and join state
+ pending interaction state
+ invocation/retry counters
+ accepted command IDs and answer state
+ dispatch intent or dispatch result
+ committed trace sequence
```

The commit is synced before any corresponding frame or pending interaction is
published. An interaction answer is committed with its command ID before the
engine unblocks. Execution stops safely if a required commit fails.

### Durable dispatch intent

Before any CLI, tool, host action, or other external boundary is dispatched,
Yawr commits an intent containing:

```text
execution occurrence ID
classification and safe endpoint identity
rendered request digest
stable idempotency key
provider deduplication/reconciliation capability
```

After dispatch, Yawr commits the result and next cursor in one mutation. If the
process crashes after the intent but before the result:

- a provider that demonstrably supports the idempotency key may be reconciled
  or invoked idempotently with the same key;
- otherwise the occurrence becomes `indeterminate` and requires operator
  verification before continuation;
- Yawr never assumes the action failed and never blindly redispatches it.

Declared read-only work may be retried only through an explicit recovery
decision. The unmatched intent remains visible and the retry is a new execution
occurrence.

### Resume

Resume acquires a single-writer lease, loads the executable snapshot and latest
committed cursor, and continues the same run ID. It never infers completion from
the current source file and never reruns a committed result.

An indeterminate external action remains blocked until an operator verifies its
state through the existing indeterminate recovery path.

## First-Class Handoff

### Authored shape

```yaml
- step:
    id: follow_active_update_slo
    type: handoff
    title: Continue with GEODR0004 - Stuck update SLO
    handoff:
      runbook: GEODR0004.runbook.yaml
      reason:
        code: active-update-slo
        summary: Continue with the active Update SLO investigation
      with:
        environment: "${environment}"
        logical_server: "${logical_server}"
        database: "${database}"
        incident_id: "${incident_id}"
      facts:
        active_workflow: "${active_workflow}"
```

V1 supports a static runbook target. Catalog-resolved dynamic targets are a
later extension after static transition integrity is proven.

### Runtime semantics

A handoff executor does not start the target itself. It returns a typed control
signal to a session coordinator. The coordinator:

1. Resolves and validates the target under the current catalog/profile policy.
2. Creates the executable and graph snapshots.
3. Preallocates transition, target segment, and target run IDs.
4. Persists a prepared transition and immutable target blobs.
5. Atomically commits the source as `handed_off`, the transition, and the target
   as the active segment.
6. Starts the preallocated target run idempotently.

On restart, a committed target that has not started is started with the same
IDs. **Prepared-transition recovery is deterministic from committed journal
state** and follows one rule:

1. Verify its immutable blobs, source occurrence, source cursor, context
   digest, and preallocated IDs.
2. If they match and no commit exists, commit the same transition and IDs.
3. If they do not match and the source handoff was never committed, append a
   transition-aborted event and leave the source cursor unchanged.
4. If the source handoff or transition commit exists, abort is forbidden; the
   coordinator repairs projections and activates the preallocated target.

No crash point may create two target segments or choose new target IDs.

## Persistence Layout

Standalone run storage remains authoritative for non-session runs:

```text
.runbook/runs/<run_id>/
  plan.v1.json
  trace.jsonl
  snapshots/
  attachments/
```

Session storage is the sole commit authority for session-managed attempts and
adds topology and immutable graph history:

```text
.runbook/sessions/<session_id>/
  events.jsonl
  manifest.json
  blobs/<hash>.json
  segments/<segment_id>/graph-revisions/<revision>.json
  segments/<segment_id>/plan-revisions/<revision>.json
  segments/<segment_id>/metadata.json
  transitions/<transition_id>.json
```

`events.jsonl` is authoritative and append-only for session attempts,
transitions, interactions, dispatch intents, and results. `manifest.json`,
`.runbook/runs/<run_id>/trace.jsonl`, and run snapshots are revisioned
materialized views that can be rebuilt from the journal. Immutable blobs are
written and synced before an event references them. A lease and writer epoch
prevent concurrent resumes.

The writer lease combines an exclusive OS-backed lock with a monotonically
increasing `writer_epoch`. Every execution mutation, dispatch intent, dispatch
result, and transition commit supplies its expected epoch; the store rejects a
stale epoch. Normal takeover is impossible while the prior process still holds
the lock. After a crash releases the lock, acquisition atomically increments
the epoch before recovery begins.

A forced takeover never assumes the old writer stopped before an external
boundary. It marks every unmatched intent indeterminate and blocks further
dispatch until the old process is confirmed stopped or the provider
demonstrably supports fencing/reconciliation. Providers that support fencing
receive the writer epoch in addition to the stable idempotency key.

Session garbage collection must treat referenced runs and attachments as one
retention unit.

## Composite Session Graph

Per-segment GraphJSON snapshots are canonical. A session graph is derived by:

1. Prefixing every qualified node, edge, frame, group, parent, and runtime-state
  key with its segment ID.
2. Wrapping each segment in a labelled runbook frame.
3. Adding an entry node for each segment.
4. Adding each committed transition as an ordinary directed graph edge from the
   exact source node to the target segment entry.
5. Attaching segment, run-attempt, occurrence, execution-source, and snapshot
  metadata to nodes.

When a dynamic include resolves, its committed graph revision appends the newly
materialized nodes and edges. Older revisions remain addressable for historical
inspection and scenario integrity checks.

This preserves the existing graph algorithms. `To`, `From`, and `Through this
step` traverse handoff edges because those edges are part of the derived
topology.

### Route semantics

For a target in the active segment, the default session route view contains:

- the actual executed route in each closed historical segment;
- every committed transition between those segments;
- structurally valid routes in the active segment to, through, or from the
  selected target.

This prevents hypothetical abandoned branches from rewriting history while
preserving route exploration in the current runbook. An explicit `All possible
routes` mode may show structural alternatives in historical segment snapshots.

Selecting a historical node computes routes against its immutable segment
snapshot and its committed session prefix.

### Display and performance

Historical segments may be visually collapsed, but they remain in the canonical
topology. Selecting a historical segment or running a route query expands every
required segment automatically.

The session manifest and executed-route summaries load first. Full segment
graphs load incrementally when active, expanded, selected, or required by a
route projection. This keeps multi-thousand-node sessions usable.

### Inspector

Every node remains selectable. The inspector shows:

- snapshotted definition and owning runbook;
- segment and run-attempt identities;
- status and execution source: `live` or `saved`;
- output, captures, evidence, logs, and outcome;
- the ordered occurrence history and selected occurrence;
- incoming/outgoing transition provenance;
- definition and plan hashes.

Historical properties come from snapshots, never current YAML.

## Session Protocol

Do not silently change `yawr.stdio/v1`. Add `yawr.session-stdio/v1` with a common
envelope:

```json
{
  "version": "yawr.session-stdio/v1",
  "type": "...",
  "frameID": "...",
  "sessionID": "...",
  "sessionSequence": 42,
  "segmentID": "...",
  "runID": "..."
}
```

Core frames:

```text
session.started
session.snapshot
segment.added
segment.graph
segment.graph.available
transition.prepared
transition.committed
attempt.started
run.event
interaction.pending
interaction.resolved
attempt.paused
attempt.finished
segment.finished
session.finished
protocol.error
```

Commands carry a unique command ID and are idempotent:

```text
session.configure
interaction.answer
session.cancel
session.detach
session.resume
session.continue_live
session.close
```

For `session start`, execution does not advance until the client completes the
startup handshake. Core first replays creation and the initial pending run
projection, then emits a one-frame `session.snapshot` at the current journal
head. After accepting that same-sequence snapshot, the client sends exactly one
content-bound command at its advertised writer epoch and sequence:

```json
{
  "version": "yawr.session-stdio/v1",
  "type": "session.configure",
  "commandID": "<client-generated UUID>",
  "sessionID": "<session UUID>",
  "writerEpoch": 3,
  "expectedSequence": 2,
  "payload": { "inputs": { "private_name": "private value" } }
}
```

The empty `inputs` object is valid and releases execution when no private
startup values are supplied. Configuration is accepted only while the run is
pending and only for inputs declared with type `secret`. It is committed as an
execution mutation carrying the command receipt before execution starts.
Retries with the same command ID and content do not add a journal event or
start a second drive loop. Public input overrides and defaults are resolved
before session creation; secret values never appear in process arguments,
session event payloads, or manifest projections.

The v1 CLI process is an attachment and execution owner, not a background
daemon. It holds the session writer lease while running. `session.detach`, stdin
EOF, or transport loss causes it to stop at the next durable boundary, commit
the session as paused, and release the lease. An in-flight unmatched dispatch
intent becomes indeterminate; it is never silently cancelled or retried.

Before spawning Yawr, VS Code generates and persists a UUID `session_id` and a
UUID creation command ID. Session creation is a create-if-absent operation over
that pair and a digest of the entry configuration. Repeating the same request
returns the existing session; reusing either ID with a different digest fails.
Therefore a crash after creation commits but before `session.started` is
delivered cannot orphan or duplicate the session.

VS Code persists the last contiguously accepted session sequence after each
complete frame or graph revision. Reopening starts:

```text
yawr session attach <session_id> --stdio --after-sequence <n>
```

The new process acquires the lease, reconstructs every frame after the durable
sequence from the journal, and remains paused until an explicit resume command.
If another live writer owns the lease, mutable attach fails with a structured
`session-already-active` error.

Command IDs and their accepted outcomes are journaled, so reconnect retries
cannot submit an answer or transition twice. Every outbound frame is derived
from a committed session sequence. Segment graph frames carry graph revision
and chunk metadata so large snapshots need not fit in one frame.

Cold attach emits `segment.graph.available` for historical revisions and sends
graph chunks only for the active segment, or the latest segment of a closed
session. VS Code retrieves an advertised immutable revision with the read-only
`session graph` command when selection or route expansion requires it. The
returned bytes are SHA-256 verified against the advertised segment snapshot.

Execution frame projection content is capped at 4 MiB, leaving bounded
envelope headroom beneath the client's 128 MiB complete-sequence limit.

The journal is the receipt authority. `manifest.json` and `session.snapshot`
are bounded projections whose `accepted_commands` contains at most the receipts
from the latest 256 committed sequences. The server rebuilds the complete
receipt index from the validated journal before idempotency checks, and a
missing, oversized, or failed optional manifest projection does not invalidate
an otherwise valid journal. Cold frame replay keeps event-scoped projection
state rather than one cumulative manifest copy per event.

New `execution.committed` events bind an immutable
`yawr.session-execution-frame-projection/v1` root through the execution mutation's
optional `frame_projection_hash` and the event payload's matching field. The
root contains the run and checkpoint identities, committed trace sequence, and
whole-content digest, and references bounded immutable chunks containing the
current interaction records and that checkpoint's pending trace-event batch.
Readers verify the root, every chunk, the whole-content digest, and those
identities before projecting frames.
Events and mutations that predate this field remain valid and use the
hash-verified cumulative run projection as a bounded legacy fallback; omitting
the optional field preserves their encoded bytes and hash recomputation.

**Graph-chunk acknowledgement is contiguous and hash-verified.** All chunks for
one graph revision share the journal event's session sequence
and include index, count, and whole-blob hash. The client advances its durable
contiguous sequence past that event only after every chunk is assembled and the
hash verifies. If disconnect occurs first, it retains the preceding sequence
and receives the complete revision again. Later frames may be buffered but do
not advance the contiguous acknowledgement across this gap.

## Saved Session Scenario

### Artifact

Use a directory bundle under `.yawr/session-scenarios/<scenario_id>/`:

```text
scenario.yaml
responses.jsonl
graphs/<segment_alias>.graph.json
```

The `yawr.session-scenario/v1` manifest records:

- source session provenance;
- entry runbook;
- ordered segment aliases and plan/graph/dependency hashes;
- exact boundary selectors;
- reviewed saved responses and answer provenance;
- expected transition sequence and reason codes;
- optional replay target;
- bundle digest, sensitivity review, and drift policy.

An exact selector contains:

```text
segment alias
run-attempt ordinal
call path
step
phase
invocation
retry attempt
```

At runtime the selector resolves to a qualified node ID and an execution
occurrence. Route-test `attempt` fields map explicitly to
`retry_attempt`; they do not identify a segment run attempt.

Scenario files are local and ignored by default because outputs may contain
incident context. Explicit export requires sensitivity review and redaction.
Secret values are never saved.

### Replay engine

Do not use the existing broad executor replacement as-is. Generalize the safer
route-test scheduler:

- pure Yawr control flow, includes, and handoffs execute normally;
- exact reviewed fixtures substitute only external and human boundaries;
- captures and branch expressions execute through the ordinary engine;
- missing fixtures and unexpected transitions fail closed;
- external dispatch count must remain zero;
- segment plan, graph, catalog, package, tool, and profile drift fail closed for
  replay-to-step and replay-to-live;
- transitions must arise naturally and match the expected sequence.

Compatibility replay with advisory drift, if retained, is a separate explicit
operation and cannot continue live.

## Product Operations

### Resume session

Continue the same active attempt from its committed cursor. Completed work is
not recomputed or redispatched.

### Save as scenario

Create a reviewed reusable bundle from the session's boundaries, answers,
transitions, and hashes. The source session remains immutable.

### Replay entire scenario

Create a new session and reproduce the saved path entirely from saved boundary
data. All pure control flow and transitions are re-evaluated. No external
system is contacted.

### Replay to step

Create a new session, replay the saved prefix across every required runbook,
and persist `paused_at_boundary` immediately before the exact target.

Preceding nodes are labelled `Saved result`, not `Completed live`.

### Continue live

After explicit operator confirmation, create a new live run attempt for the
same target segment. It references the replay attempt through
`continued_from_run_id` and starts at the persisted boundary.

The replayed target occurrence and subsequent live occurrence remain separate
in history and are both selectable in the inspector.

One journal mutation marks the replay attempt `continued` and no longer
resumable, creates the live attempt at the saved boundary, and changes the
session's `active_run_id`. A crash cannot leave both attempts resumable or make
the replay attempt active again.

V1 allows direct continuation only to an interaction or read-only boundary.
Mutating/destructive work requires fresh live prerequisites and ordinary
governance. Saved data is never silently treated as current evidence.

### Fork from step

Create a new session with immutable provenance to the source prefix. Change one
answer or fixture and explore a different route without altering the original.

### Refresh from step

Create a fork, rerun a selected read-only boundary live, and invalidate every
dependent downstream saved result. Mutating steps cannot be refreshed.

### Validate scenario

Check bundle integrity, hashes, selectors, fixture schemas, expected
transitions, target reachability, and the zero-dispatch invariant.

### Compare with scenario

Show differences in answers, outputs, routes, transitions, outcomes, and
definition versions without executing anything.

### Inspect and isolate routes

Open the composite graph read-only and retain session-wide `To`, `From`, and
`Through` route operations across all committed transition edges.

### Close session

Explicitly record `resolved`, `escalated`, `cancelled`, or `abandoned`. Finishing
or handing off a segment does not implicitly close its session.

Closing at a durable pause marks the unfinished active attempt and segment as
`cancelled`; the chosen session outcome remains independent and explicit.

## CLI Shape

Proposed commands:

```text
yawr session start <runbook> --session-id <uuid> --command-id <uuid>
yawr session attach <session_id> --stdio --after-sequence <n>
yawr session graph <session_id> --segment-id <segment_id> --revision <n>
yawr session resume <session_id>
yawr session inspect <session_id> --format graphjson
yawr session close <session_id> --status <status>

yawr scenario save <session_id> --name <name>
yawr scenario validate <scenario>
yawr scenario replay <scenario>
yawr scenario replay <scenario> --to <segment/selector>
```

`session start` and `session attach` accept the normal `--package-map` and
`--profile` bindings. Package-map selection populates both planning and runtime
tool registries and the dynamic include resolver; the selected catalog/package
digests and runtime profile are frozen into each immutable executable plan.
`--package-map` chooses tool definitions while `--profile` parameterizes those
already-selected definitions.

VS Code uses session stdio for lifecycle, commands, receipts, and graph
availability. It invokes `session graph` only to fetch one advertised immutable
revision on demand; it verifies the returned identity and SHA-256 digest.

## Backward Compatibility

- Existing `yawr run`, `yawr.stdio/v1`, traces, and run directories continue to
  work as single-run execution surfaces.
- Existing includes remain same-run nested execution and do not create session
  segments.
- Existing route tests remain supported. Their exact-selector and reviewed
  fixture contracts become the foundation for session scenarios.
- Session support is opt-in until the coordinator, persistence, and protocol
  are proven. VS Code can later make an implicit single-segment session the
  default without changing runbook behavior.

## Safety Invariants

1. Resume never reruns a committed step.
2. Replay never performs external dispatch before or during its saved prefix.
3. A missing fixture, selector, target, or expected transition fails closed.
4. Replay-to-live requires exact dependency hashes and an explicit mode change.
5. Mutating/destructive actions cannot rely solely on stale saved evidence.
6. Historical definitions and results are immutable.
7. Handoff transfers only allowlisted values into declared target inputs.
8. Secret values do not enter session transitions or scenario artifacts.
9. A handoff is never displayed as successful remediation.
10. Crash recovery creates at most one committed transition and target segment.
11. An unmatched dispatch intent is reconciled or becomes indeterminate; it is
  never blindly redispatched.
12. Pending interactions and accepted answers are committed before publication
  or execution resumes.
13. Session creation is idempotent from a client-persisted ID even when the
  first server frame is lost.
14. Every mutation is fenced by the active writer epoch; stale writers cannot
  commit or dispatch after takeover.

## Implementation Plan

### Phase 0: Freeze contracts

1. Split this proposal into normative specs for session storage, handoff schema,
   session stdio, session GraphJSON, and session-scenario artifacts.
2. Define versioning, limits, redaction, and compatibility rules.
3. Add conformance fixtures before runtime implementation.

Checkpoint: all public contracts have adversarial examples and are approved.

### Phase 1: Make run resume durable

1. Add `ExecutablePlanSnapshotV1` and round-trip tests.
2. Add structured cursor sets, parallel join state, invocation counters, retry
  counters, and pending interaction state to checkpoints.
3. Add the required `CommitExecutionMutation` journal abstraction.
4. Commit pre-dispatch intents and reconcile or mark unmatched intents
  indeterminate after restart.
5. Commit dynamic-include pins and executable/graph revisions before child
  execution.
6. Stop execution on persistence failure and add process-restart resume tests
  through branch, parallel, include, dynamic include, iteration, and pending
  interaction paths.

Checkpoint: killing the process before/after every dispatch and commit boundary
never blindly redispatches work or loses the exact cursor.

### Phase 2: Build strict boundary replay

1. Extract route-test exact selectors and fixture validation into shared core
   types.
2. Execute pure control flow normally while substituting only reviewed boundary
   results.
3. Add strict dependency-drift and expected-transition validation.
4. Add a durable `paused_at_boundary` result and resumable prefix checkpoint.

Checkpoint: a cross-include replay reaches a target with zero dispatches and
fails closed for every missing or mismatched fixture.

### Phase 3: Add session storage and coordinator

1. Add session, segment, run-attempt, occurrence, transition, and lease types.
2. Implement the append-only directory store and atomic manifest projection.
3. Add start, inspect, resume, and close coordinator operations.
4. Associate existing run attempts with session and segment identities.

Checkpoint: a single-segment session survives restart and remains inspectable.

### Phase 4: Add static handoff

1. Add handoff schema, parsing, planning, and validation.
2. Bubble a typed handoff control signal to the session coordinator.
3. Implement prepared/committed transition recovery with preallocated IDs.
4. Add context allowlisting, fact references, and target input validation.

Checkpoint: fault injection at every handoff write boundary creates exactly one
transition and exactly one target segment.

### Phase 5: Add session stdio

1. Implement the new frame and command envelope.
2. Add attach/detach, durable pause on transport loss, reconnect by session
  sequence, writer leases, and idempotent command processing.
3. Add client-generated idempotent session creation and stale-writer epoch
  fencing.
4. Reconstruct all frames from committed journal entries and stream graph
  revisions and transition commits incrementally with contiguous chunk
  acknowledgement.

Checkpoint: disconnect/reconnect during handoff and interaction loses no frames
and creates no duplicate answer or segment.

### Phase 6: Build the VS Code composite graph

1. Parse session GraphJSON and namespace all graph/runtime identities.
2. Render segment frames, handoff edges, current position, and segment status.
3. Load historical segment graphs incrementally.
4. Show immutable historical definition and execution properties.
5. Extend `To`, `From`, and `Through` projections across handoff edges with
   actual-prefix semantics.

Checkpoint: selecting a GEODR0004 node displays a route beginning in GEODR0001,
and every displayed historical node is selectable and inspectable.

### Phase 7: Add saved session scenarios

1. Implement save, parse, integrity, sensitivity, and validation workflows.
2. Implement replay entire scenario across natural handoffs.
3. Implement replay to an exact cross-segment target.
4. Implement explicit live continuation as a new attempt.

Checkpoint: replay GEODR0001 to a GEODR0004 target with saved data, zero external
dispatch, a visible saved/live boundary, and normal live governance afterward.

### Phase 8: Add derived-session operations

1. Add fork from step.
2. Add refresh from a read-only step with downstream invalidation.
3. Add compare with scenario.
4. Add retention and garbage collection for sessions and scenarios.

Checkpoint: forks and refreshes never mutate the source session or reuse a
completed external action.

### Phase 9: Adopt in SQL Livesite runbooks

1. Replace the GEODR0001 generic active-workflow terminal with explicit
   handoffs, starting with Update SLO to GEODR0004.
2. Give every handoff a human title, reason, transferred context, and fact refs.
3. Add real multi-runbook session scenarios and replay-to-step tests.
4. Run the exact Yawr runtime, VS Code Extension Host, and browser graph paths.

Checkpoint: the operator can answer `Update SLO`, continue into GEODR0004,
inspect all prior GEODR0001 steps, isolate a session-wide route, save it, and
replay to a later XTS/Kusto step without contacting external systems.

## Critical Acceptance Tests

1. Crash at every transition commit boundary yields one transition and one
   target run.
2. Crash immediately before dispatch, immediately after dispatch, and before
  result commit either reconciles by provider idempotency or becomes
  indeterminate; it never blindly redispatches.
3. Resume after process restart through include, dynamic include, branch,
  parallel, iteration, and pending interaction never reruns committed work.
4. A failed journal commit or projection write behaves according to the single
  commit authority: commit failure stops execution, projection failure rebuilds.
5. Repeated runbook invocation and duplicate authored step IDs resolve through
  distinct qualified node IDs.
6. Replay reproduces captures, branches, handoffs, and outcomes with zero
   external dispatch.
7. Missing fixture, transition mismatch, plan drift, tampered bundle, or stale
   secret reference fails closed.
8. Replay-to-live creates a new run ID, preserves separate replay/live
  occurrences, and reapplies
   live governance.
9. VS Code detach/reconnect restores the exact cursor, pending interaction,
  current node, and all historical graph
   segments.
10. Duplicate command IDs across reconnect do not duplicate answers, dispatches,
   transitions, or segments.
11. Session-wide `To`, `From`, and `Through` projections cross transition edges
   and auto-expand required segments.
12. Selecting any historical node shows its snapshotted definition and result,
    not the current source file.
13. A crash after session creation commits but before `session.started` is
  delivered reattaches to the one existing session by client-generated ID.
14. A stale writer loses every commit after epoch takeover; takeover during an
  unmatched dispatch blocks as indeterminate unless provider fencing proves
  safety.
15. Recovery completes a valid prepared transition with its original IDs and
  aborts only an uncommitted invalid preparation.
16. Replay-to-live atomically retires replay resumability and selects exactly
  one new active live run.
17. Dropping any graph chunk leaves the client's contiguous sequence before the
  graph event and causes the full verified revision to replay on attach.
18. `session.configure` cannot race the first execution boundary: a valid
  content-bound receipt commits before the first provider call, retry is a
  no-op, and private values are absent from argv and journal event payloads.

## Explicit Non-Goals for V1

- Concurrent active segments in one session.
- Automatic inference of the next TSG from arbitrary output text.
- Dynamic catalog-resolved handoff targets.
- Merging two existing sessions.
- Replaying secret values.
- Continuing a mutating action directly from stale saved evidence.

## Decisions Required Before Implementation

1. Confirm one active segment per session in v1.
2. Confirm forks create a new session with provenance rather than an in-session
   mutable branch.
3. Confirm strict drift refusal for replay-to-step and replay-to-live.
4. Confirm local ignored storage as the default for saved scenarios.
5. Confirm default route semantics: actual historical prefix plus structural
   routes in the active segment.
