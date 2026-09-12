# Evidence and resume

## Durable facts and bounded views

Yawr records execution events in an append-oriented JSONL trace and stores
checkpointed run state. Evidence items can include structured operator data and
attachments with content hashes. These artifacts answer different questions:

- the trace describes ordered execution activity;
- checkpoints contain the state required to continue a durable run;
- published results expose declared, validated outputs;
- UI and protocol projections provide bounded views of those records.

A projection is never the durable authority. Truncated process output, graph
details, terminal summaries, and result references must not be treated as the
complete stored value.

## Ordering and identity

Events are ordered within a run, not globally. Run IDs survive ordinary
pause/resume. Interaction turn IDs identify durable pending work, while a
transport cursor may be local to one connection or server process.

Plan hashes, source identities, publication IDs, digests, checkpoint sequences,
and invocation paths bind recovered data to the execution that produced it.
Clients validate these identities instead of reconstructing a result from trace
previews or current source files.

## Commit boundaries

Effects, captures, trace outbox entries, results, and checkpoint state are
committed through runtime-owned boundaries. A failed over-budget checkpoint does
not replace the prior valid checkpoint. Named results are readable only after
their publication record commits.

Durable state has entry and byte budgets. Copying a value into multiple captures,
frames, or outputs consumes capacity at each retained location even when physical
blob storage deduplicates the bytes. Authors keep public records compact without
discarding required evidence or mislabeling missing observations as success.

## Resume

Resume uses the original run store, working context, bindings, and frozen plan
information. It validates checkpoints and source or package drift according to
the active contract. It does not repair an invalid checkpoint by guessing.

Only one writer may advance a durable run. Before takeover, the previous writer
must stop and release its lease. Lock or epoch files are not deleted to bypass a
live owner.

Served resume first attaches the persisted run, then advances it explicitly.
Clients open the interaction stream and submit answers concurrently with a
blocking advance call. CLI stdio resume drives progression in its own process and
uses the same durable turn identity.

## Evidence quality

Evidence is the declared output of an observed action or operator submission, not
the fact that a screen opened or a request was attempted. Hashing proves content
identity, not correctness. Redaction protects disclosure, not truth. Assertions,
typed output contracts, and explicit domain statuses remain necessary to explain
what an observation means.

Trace and evidence features support review and recovery. They are not, by
themselves, a compliance certification or a guarantee that an external system
performed the intended action.
