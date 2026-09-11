# Runbook Debugging v1

Status: implemented (August 2026)
Scope: opt-in interactive debugging and reusable debug profiles for Yawr runbooks.

## Objective

Let a runbook author inspect and deliberately alter one debug run without changing
the runbook or hiding what actually happened. The motivating case is an ICM query
that returns `Mitigated`: the author pauses after that step, changes the effective
result to `Active`, and lets the ordinary captures and branch conditions exercise
the active-incident path.

A normal run never pauses for a debugger and never discovers or applies a debug
profile.

## User Experience

The graph preview offers `Run` and `Debug Run`. In a debug run, a real step can have
one or both breakpoint phases:

- `before`: pause before governance and executor dispatch; runtime variables may be
  edited or execution may continue unchanged.
- `after`: pause after executor dispatch but before capture evaluation, variable
  merge, checkpointing, completion events, failure routing, or terminal routing.
  The actual result is immutable; an effective output, status, and runtime-variable
  patch may be supplied.

The selected node's detail pane shows the paused phase, invocation breadcrumb,
actual result, effective result, variables, and actions:

- Continue unchanged
- Apply and continue
- Step into
- Step over
- Step out
- Stop
- Save as debug profile

An overridden node is marked with an amber `Debug override` badge. Its details and
trace retain both actual and effective values.

## Root Scope and Storage

Shared profiles live at the selected root runbook's project root:

```text
.yawr/debug-profiles/<profile-name>.yaml
```

A profile is explicitly bound to one root runbook. The server never searches for
profiles during a normal run. A client must select and submit a profile when it
starts a debug run.

```yaml
version: yawr.debug-profile/v1
name: Mitigated ICM as active
root:
  ref: runbooks/triage-icm.runbook.yaml
  id: triage-icm
created_against: <plan-hash>
overrides:
  - target:
      call_path:
        - step_id: inspect_primary_icm
      step: get_incident
      phase: after
    set:
      output_patch:
        incident:
          status: Active
```

`created_against` detects drift. A mismatched hash makes the profile stale and
requires explicit confirmation; it is never silently applied.

## Invocation Identity

A debug location is not identified by step ID alone:

```text
root run ID + call path + step ID + invocation ordinal + attempt
```

The call path contains the include/container call sites traversed from the root.
This distinguishes two calls to the same child runbook:

```text
triage/inspect_primary_icm/get_incident
triage/inspect_secondary_icm/get_incident
```

Interactive breakpoints may target either the current invocation or every matching
call path. Saved profiles use the complete call path. Loop/retry selectors are
reserved for `iteration` and `attempt`; v1 records those identities but the first
UI supports only the current invocation and all invocations.

## Engine Contract

Debugging is an optional engine dependency. The engine presents a bounded snapshot
to the debugger at `before` and `after` phases. A controller returns immediately
when no breakpoint/profile matches, or blocks until the client supplies a command.

The `after` hook receives a deep copy of the executor's actual `StepResult`. The
engine applies a validated override to a second effective result and then runs the
existing capture, redaction, evidence, commit, checkpoint, event, failure-routing,
and terminal-routing pipeline against that effective result.

The actual result remains immutable and is emitted in a dedicated audit event when
an override is applied. Existing `step/completed`, `step/failed`, and
`step/skipped` events describe the effective result so existing clients continue
to represent the path the engine actually followed.

Editable v1 state:

- Before: existing runtime-variable values; new variable keys are allowed.
- After: effective output object, effective status (`completed`, `failed`, or
  `skipped`), and runtime-variable patches.
- Captures are recomputed from the effective output.

Immutable states:

- governance `denied`
- transport `indeterminate`
- cancellation

The debugger cannot turn one of these states into success or suppress its required
operator acknowledgment.

Cancellation or deadline expiry while paused terminalizes the debug run as
cancelled in both phases. A before-phase cancellation never dispatches the step;
an after-phase cancellation never retries an already executed step.

## Interaction Wire

The existing interaction SSE/POST lifecycle carries a new `debug_break` kind only
for runs explicitly started in debug mode.

Pending frame:

```json
{
  "type": "pending",
  "kind": "debug_break",
  "runID": "run-1",
  "turnID": "turn-1",
  "stepID": "get_incident",
  "debug": {
    "phase": "after",
    "callPath": [{"step_id":"inspect_primary_icm"}],
    "invocation": 1,
    "attempt": 1,
    "variables": {"incident_id":"123"},
    "actual": {
      "status": "completed",
      "output": {"incident":{"status":"Mitigated"}}
    },
    "watches": [
      {"expression":"incident_status", "value":"Mitigated"}
    ]
  }
}
```

Answer:

```json
{
  "kind": "debug_break",
  "action": "continue",
  "set": {
    "output_patch": {"incident":{"status":"Active"}},
    "vars": {},
    "status": "completed"
  }
}
```

`action` is `continue`, `stop`, `step_into`, `step_over`, or `step_out`.
Stepping actions become one-shot broker filters; the engine receives an ordinary
continue decision. `step_into` targets only the current container invocation path,
so another call to the same child runbook is unaffected.

All debug snapshots use the existing preview bounds and redaction policy. Secret,
credential, and governed-redaction values must never become editable cleartext.
This includes source variables used inside transformed child bindings such as
`with: {api_secret: "Bearer ${root_token}"}` and child-only governance redactions.
Named source dependencies remain protected even when an earlier step creates the
variable at runtime. Whole-map dependencies such as `${vars}` are treated as
unknowable and force fail-closed protection.
Before the first pause, a debug run resolves the static eager/lazy child closure
and applies its protection to the whole run. If any dynamic descendant remains
unknowable, all runtime variables are protected and variable overrides are
disabled for that run.

Browser/SSE reconnect to an unresolved pause in the current server process is
supported. Persisted engine resume with a debugger is rejected in v1 because
ephemeral child secret values and dynamically resolved governance cannot be
safely reconstructed from checkpoints. Operators start a new debug run instead;
non-debug resume behavior is unchanged.

For the same reason, a profile whose static closure contains a dynamic include
cannot persist variable patches in v1. Breakpoints and after-phase result
overrides remain available.

## HTTP Start Contract

`POST /runs` accepts optional debug data:

```json
{
  "runbookPath": "/abs/triage.runbook.yaml",
  "debug": {
    "enabled": true,
    "breakpoints": [
      {"step":"get_incident", "phase":"after", "callPath":[{"step_id":"inspect_primary_icm"}]}
    ],
    "watches": ["incident_status"],
    "profile": {"version":"yawr.debug-profile/v1", "root":{}, "overrides":[]},
    "allowStaleProfile": false
  }
}
```

Absent or `enabled: false` uses the current execution path byte-for-byte.

## Trace and Audit

An applied override emits `debug/override_applied`.

`debug/override_applied` records the location and actual/effective values. The
preview projects these through bounded runstate fields. It never rewrites an
earlier executor event or pretends the external system returned the overridden
value.

## Profile HTTP API

- `GET /debug-profiles?runbookPath=<root>` lists only profiles whose authoritative
  root ref and ID match the selected runbook. Plan-hash drift is returned as
  `stale: true`.
- `PUT /debug-profiles/{id}` writes a slug-validated YAML profile under
  `.yawr/debug-profiles`. The server supplies `version`, `root`, and
  `created_against`; client values cannot redirect storage or rebind a profile.
- Starting with a stale profile requires `allowStaleProfile: true`.

## MVP Boundaries

Always:

- Require explicit debug-run opt-in.
- Keep actual executor results immutable and auditable.
- Validate all client patches and bound their size/depth.
- Match nested targets by complete invocation call path.
- Recompute captures from effective output.

Not in v1:

- Pausing all branches of an already-running parallel step.
- Rewinding or re-executing side effects.
- Overriding governance denial, cancellation, or indeterminate completion.
- Applying profiles to normal runs.
- Source-line gutter breakpoints; graph step IDs are authoritative until parser
  source ranges are available.

## Project Structure

```text
pkg/engine/                         public debug contracts and invocation identity
internal/engine/                    pre/post execution hook and effective-result commit
internal/serve/                     debug controller, wire validation, HTTP integration
internal/serve/static/preview.html  graph breakpoint and paused-state UI
.yawr/debug-profiles/               project-owned reusable profiles
```

## Commands and Testing

```text
Focused engine: go test ./internal/engine -run Debug -count=1
Focused wire:   go test ./internal/serve -run Debug -count=1
Full Go:        go test ./... -count=1
Build:          go build -o yawr.exe ./cmd/yawr/
Manual:         yawr.exe serve --addr 127.0.0.1:<port> ...
```

Tests must prove:

1. Normal runs never invoke the debug controller.
2. A before pause can patch variables before executor evaluation.
3. An after pause can change `Mitigated` to `Active`; captures and the following
   branch observe `Active`, while the actual result remains `Mitigated`.
4. Cancel/stop unblocks the engine without committing an override.
5. Two calls to the same child step are distinguished by call path.
6. An override cannot change denied or indeterminate states.
7. The interaction stream reconnects to an unresolved debug pause.
8. The graph UI can set a breakpoint, edit effective JSON, and continue.
9. Eager, lazy, and dynamic child secrets redact parent and child snapshots,
   including transformed `with` source variables.
10. Debugger callbacks preserve cancellation/deadlines but receive no engine or
  secret-bearing context values; debugger-enabled persisted resume fails closed.

## Implementation Plan

1. Add engine debug contracts, immutable copying, invocation-path context, and unit
   tests for normal/before/after behavior.
2. Propagate the controller and invocation path into nested sub-engines; prove two
   include call sites remain distinct.
3. Add `debug_break` broker frames and answers plus additive `POST /runs` debug
   configuration.
4. Add graph-native breakpoint controls and paused actual/effective/variables UI.
5. Add project-root profile discovery, validation, save, and selection.
6. Run unit, integration, browser, real-runbook, and full-suite validation.

## Success Criteria

The canonical acceptance run queries an ICM whose actual status is `Mitigated`,
pauses after that query, applies effective status `Active`, recomputes captures,
and follows the ordinary active-ICM branch. The UI and trace show both values, and
a saved root-scoped profile can reproduce the same nested call-path-specific run.
