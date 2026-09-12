# Checkpoint value budget

The current `run-state/v3` checkpoint has a **4096-entry inclusive limit**:
4096 is accepted; 4097 is rejected. Both save and load use this accounting:

```text
entries =
  len(run.Vars)
  + sum(root StepResults, len(Output) + len(Vars))
  + sum(all ExecutionFrames,
      len(WorkingVars) + sum(frame Results, len(Output) + len(Vars)))
  + sum(PendingTraceEvents, len(Payload))
```

This is not recursive JSON-node counting. A scalar, null, object or array costs
one entry at each retained top-level map location. A 256-row array is one entry
per retained occurrence, not 256 entries. Nested object keys and array elements
are not additional slots. Identical data is charged again when copied into
another result, capture or frame.

## Trace metadata

The **pending trace outbox payload** participates in the same quota; it does not
have a free metadata allowance. For example, 4094 execution-value entries plus
two pending payload entries fit; 4095 plus two do not. The separate outbox bound
is 16 events, with existing identity, ordering, timestamp and final execution
commit validation. This does not charge every committed trace event: only
payloads retained in the checkpoint's pending outbox.

Save and load apply identical accounting to pending payload entries and expanded
bytes. Save rejects over-budget states **before writing blobs or publishing the
checkpoint**, leaving the previous checkpoint available. `run-state/v3`
checkpoints must satisfy the cap and blob-integrity checks when written and read.

Step identity/status/timing, invocation maps, cursor metadata, and interaction
records are not independently walked by the value-slot counter. They still have
their own relationship and size limits and occupy snapshot bytes. Metadata
inserted into a counted Vars/Output/Payload map costs entries normally.

## Independent byte limits

Each occurrence also incurs its complete marshalled JSON value byte length,
including nested contents:

| Limit | Value |
|---|---:|
| Expanded bytes across counted values, including pending payloads | 256 MiB |
| One state value / uncompressed blob | 256 MiB |
| Snapshot document | 16 MiB |
| Inline-value threshold | 64 KiB |
| Compressed blob | 64 MiB |

Blob deduplication reduces physical storage, not entry or expanded-byte charges.
Two references to one 128 MiB blob consume 256 MiB of expanded budget. Another
one-byte JSON value then exceeds it. A large nested array can therefore fit the
entry quota but fail the byte quota.

## What composition does not reclaim

The budget applies to the **current snapshot**, not the sum of prior
snapshot files. However, that snapshot retains completed execution frames and
their results. Captured values can exist in raw Output, StepResult.Vars and
working/root Vars simultaneously. Sequential iterations and branches can retain
copies of inherited working values as well.

Substituted tools isolate their argument namespace and export declared outputs;
they do not garbage-collect durable child history. Include capture mappings are
not export whitelists. Replacing a variable does not remove older result/frame
copies; assigning null still occupies a slot. There is no general YAML
unset/discard/frame-compaction operation, nor a promise that scope/export
annotations release retained values.

Keep complete typed domain records compact at boundaries and avoid unnecessary
duplicate exports, but measure the entire retained snapshot. For example, this
complete synthetic four-perspective result preserves every perspective and
false/zero/empty-array values:

```json
{"observations":[
  {"procedure":"user","status":"synthetic-only","rows":[],"confirmed":false,"count":0},
  {"procedure":"kernel","status":"synthetic-only","rows":[],"confirmed":false,"count":0},
  {"procedure":"system","status":"synthetic-only","rows":[],"confirmed":false,"count":0},
  {"procedure":"node","status":"synthetic-only","rows":[],"confirmed":false,"count":0}
]}
```

This corresponds to the investigation's `followup-ken\bounded.runbook.yaml` and
`bounded-child.runbook.yaml` fixtures; it is not evidence from a live CPU
investigation. Retain all required checks and real rows/evidence in actual
results. Do not relabel missing/failed observations as success. Compact output
alone cannot guarantee a larger composition fits because internal history is
also counted. A legitimate high-CPU composition exceeding 4096 can still fail;
this change neither raises the allowance nor implements lifetime cleanup.

Boundary and atomicity coverage is in `internal\runstore\budget_test.go`;
existing store tests also cover deduplicated-blob expansion and integrity.
