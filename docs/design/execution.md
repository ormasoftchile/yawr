# Execution

## From source to run

1. Yawr loads the runbook and current schema.
2. Semantic validation rejects invalid references, illegal combinations, and
   unsafe relationships.
3. Project configuration, package maps, requirements, and tool references build
   the explicit catalog used for the run.
4. The planner resolves the executable structure and records the identities
   needed for durable execution.
5. The engine advances one operation at a time, applying governance before an
   effect and committing results, captures, events, and checkpoints after it.

Dry-run, direct CLI, stdio, and served execution share these runtime stages.
Transport-specific framing does not create a second execution model.

## Flow and composition

Runbooks combine effectful steps with runtime-owned control flow. Current
capabilities include commands and tools, branching, iteration and parallel
composition, static or catalog-resolved includes, operator choices and
collectors, approvals, host actions, assertions, assignments, handoffs, and
declared results.

Composition is explicit:

- Child inputs are supplied through declared bindings.
- Captures name the values that cross a step boundary.
- Public tool and runbook outputs are validated against their declarations.
- Dynamic includes resolve an exported identity from an approved catalog, never
  an arbitrary runtime path.
- Include gates can stop a parent on a declared child outcome without inventing
  a successful publication.
- Structured values retain native arrays, objects, numbers, booleans, strings,
  and null where the selected contract supports them.

## Status and failure

Execution status and domain outcome are separate facts. A transport can complete
while a domain result is blocked or contains no data; a step can expose declared
failure output without making the containing run successful.

Yawr does not coerce missing observations into empty success. Required outputs,
captures, evidence, and interactions fail when they are absent or invalid.
Indeterminate effects, governance denial, cancellation, and ordinary execution
failure remain distinct terminal conditions.

Retries and continue-on-failure are authored policies, not permission to erase
an uncertain side effect. Callers must design idempotency and compensation around
the behavior of the selected tool.

## Interactive execution

Interactive steps produce a pending turn with opaque field tokens. The client
renders the prompt and submits a typed answer for the same run and turn. Display
labels are not protocol identifiers. Duplicate, stale, wrong-kind, or malformed
answers are rejected.

An advancing served request may remain open while waiting for an answer, so
clients listen and answer concurrently. Opening a panel or receiving a pending
frame is not evidence that the requested work was completed.

## Debug execution

Debugging is opt-in and never applies to a normal run. Breakpoints use full
invocation identity rather than a step name alone. The actual executor result is
immutable; a validated effective result may drive captures and downstream flow,
and both values remain visible in the debug audit. Debugging cannot override
denial, indeterminate completion, or cancellation.
