# Show Routes Through a Step and Test Reaching It

Status: interaction proposal
Date: August 2026

Visual review board: [routes-through-step-sketches.html](routes-through-step-sketches.html)

## The Concept

There is no "focus corridor" in the operator experience.

Selecting a step offers one literal graph command:

```text
Show routes through this step
```

That command temporarily filters the graph to show:

```text
routes that can lead to the selected step
+ the selected step
+ routes that can follow from the selected step
+ visible markers for unresolved or hidden dependencies
```

It does not create a new artifact, run anything, or claim that every displayed
route is feasible. It is simply a reversible graph view.

From that view, the operator may start a separate workflow:

```text
Test reaching this step
```

A route test uses declared test data and choices to check whether the ordinary
Yawr engine reaches the selected step. Every external executor is blocked, and
Yawr stops before running the selected step.

## Product Shape

This proposal has one graph command and one optional product:

1. **Show routes through this step** is a temporary graph filter.
2. **Route test** is a saved, repeatable test that attempts to reach a selected
   step without external dispatch.

They are deliberately not presented as two new product names. The operator sees
verbs that describe the immediate result.

The graph filter is useful by itself. A route test may be created from a filtered
view, a selected gap, or another route test.

## Preceding Step Model

Routes to the selected step may contain three kinds of prerequisite:

| Prerequisite | Actual run | Route test |
|---|---|---|
| Pure Yawr logic | Execute normally | Execute normally |
| Automated CLI/tool/host action | Dispatch through its ordinary executor | Use one exact saved result; do not dispatch |
| Human-assisted check | Open the external view, wait for structured answers, then continue | Do not open the view; use an exact launch response plus saved answers |

Captures and branch conditions always run through the ordinary engine. A route
test substitutes only boundary responses. It must not patch the branch variable
immediately before a decision when an authentic producer exists.

For an automated predecessor, the route test binds a complete declared result to
the exact qualified step, call path, phase, invocation, iteration, and retry. The
ordinary capture logic derives downstream variables from that result.

## Human-Assisted XTS Check

The human is not modelled as a tool. The XTS launch is a host action; the
human's structured observation is a collector response.

An XTS investigation is not modelled as "XTS returned a diagnosis." It has two
separate boundaries and two status layers:

```text
Open XTS view
  -> generic host status:
    completed | failed | timed-out | execution-not-started | unsupported
  -> when generic status is completed, nested XTS result:
    opened | view-not-found | environment-not-found |
    invalid-parameters | execution-not-started

Record findings
  -> structured human answers: primary health, secondary health,
  replication lag, or other runbook-owned fields
```

`outputs.status` carries the generic host status. `outputs.result.status` carries
the nested XTS result on a completed bridge response. The current host-action
executor also places a non-completed generic status at `outputs.result.status`
for convenient branching, but the runbook captures both layers and treats the
view as opened only when:

```text
host_status == "completed" and xts_launch_status == "opened"
```

The launch result says only whether the view opened. The questionnaire answers
are the operator's observations. Downstream routing consumes the answers, gated
by a recorded-finding status, not generic host completion alone.

### Authoring Boundary

The recommended reusable unit is a child runbook titled for the operator task,
for example `Inspect replication in XTS`. The parent graph renders the include as
one collapsed node:

```text
[Inspect replication in XTS]
  Open view, then record findings
```

Expanding the include reveals the host action, launch-status branch, and
collector. The answer-driven business branch remains in the parent after the
include. The parent gives selected child findings stable local aliases and does
not branch on XTS transport fields.

Illustrative child shape:

```yaml
apiVersion: yawr.runbook/v1
id: inspect-replication-in-xts
name: Inspect replication in XTS

inputs:
  server: { type: string, required: true }

flow:
  - step:
      id: initialize_xts_check
      type: noop
      capture:
        xts_check_host_status: execution-not-started
        xts_check_launch_status: execution-not-started
        xts_check_finding_status: blocked
        xts_check_primary_health: unknown
        xts_check_secondary_health: unknown
        xts_check_replication_lag: unknown

  - step:
      id: open_xts_view
      type: host_action
      title: Open XTS view
      host_action:
        capability: xts.open-view
        request:
          view_path: sterling/database replicas.xts
          environment: Production
          parameters: { server: "${server}" }
          focus: true
      capture:
        xts_check_host_status: outputs.status
        xts_check_launch_status: outputs.result.status

  - step:
      id: handle_xts_launch
      type: branch
      branches:
        - condition: >-
            xts_check_host_status == "completed" and
            xts_check_launch_status == "opened"
          label: XTS view opened
          steps:
            - step:
                id: record_findings
                type: collector
                title: Record the XTS findings
                prompt: Review the opened XTS view, then return here.
                fields:
                  - name: xts_check_primary_health
                    type: select
                    label: Primary health
                    required: true
                    options:
                      - { value: healthy, label: Healthy }
                      - { value: unavailable, label: Unavailable }
                      - { value: unknown, label: Unknown }
                  - name: xts_check_secondary_health
                    type: select
                    label: Secondary health
                    required: true
                    options:
                      - { value: healthy, label: Healthy }
                      - { value: unavailable, label: Unavailable }
                      - { value: unknown, label: Unknown }
                  - name: xts_check_replication_lag
                    type: select
                    label: Replication lag
                    required: true
                    options:
                      - { value: within_limit, label: Within limit }
                      - { value: above_limit, label: Above limit }
                      - { value: unknown, label: Unknown }
            - step:
                id: mark_findings_recorded
                type: noop
                capture: { xts_check_finding_status: recorded }
        - else:
          label: XTS view could not be opened
          steps:
            - step:
                id: mark_xts_blocked
                type: noop
                capture: { xts_check_finding_status: blocked }
```

The consumer runbook owns `view_path`, environment, and native XTS parameter
names. Yawr core and the VS Code bridge remain capability-generic.

Illustrative parent boundary:

```yaml
- step:
    id: inspect_replication
    type: include
    title: Inspect replication in XTS
    include:
      runbook: inspect-replication-in-xts.runbook.yaml
      with: { server: "${server}" }
    capture:
      replication_finding_status: xts_check_finding_status
      replication_primary_health: xts_check_primary_health
      replication_secondary_health: xts_check_secondary_health
      replication_lag: xts_check_replication_lag

- step:
    id: route_replication_finding
    type: branch
    branches:
      - condition: >-
          replication_finding_status == "recorded" and
          replication_primary_health == "unavailable"
        label: Primary unavailable
        steps:
          - step:
              id: continue_primary_unavailable
              type: noop
      - else:
        label: Finding blocked or primary not unavailable
        steps:
          - step:
              id: stop_replication_unconfirmed
              type: end
              outcome: { category: blocked, code: replication-unconfirmed }
```

Current static includes are presentation/reuse boundaries, not variable-isolation
boundaries. The include executor propagates every child variable, and a capture
source may fall back to a same-named parent variable when absent. Therefore this
v1 pattern:

- prefixes every child variable with `xts_check_`;
- initializes every captured child value through an executed first step because
  current static includes do not seed the child runbook's top-level `vars:`;
- uses explicit parent aliases for business routing;
- requires collision checks for the namespaced variables; and
- never claims that a child `outputs:` declaration restricts propagation.

An export-only include contract would be cleaner future work, but this proposal
does not assume it already exists.

For generated domain operations, a validated `yawr.regions/v1` overlay may group an
equivalent contiguous inline sequence. A renderer must never infer this grouping
from adjacency alone. The engine ignores regions and continues to trace every
underlying node.

### Actual Run

Visual board states `A Open XTS`, `B Answer questions`, and `C Review answers`
show the live behavior:

1. Yawr reaches `open_xts_view` and asks the operator to confirm `Open XTS`.
2. The VS Code host dispatches the generic `xts.open-view` request.
3. If the acknowledgement is `opened`, VS Code focuses XTS and reminds the
   operator to return.
4. The host-action step completes; Yawr advances to `record_findings` and waits.
5. The operator reviews XTS, returns to Yawr, and completes the questionnaire.
  The values remain an editable client-side draft and have not resumed Yawr.
6. `Review answers` shows every value and its source before submission.
7. `Save answers and continue` sends one collector response to Yawr.
8. Collector validation runs, answers become child variables, the include
  exposes the explicitly aliased findings, and the parent branch chooses the
  next route.

Confirmation has one owner. In this proposal, the Yawr panel's `Open XTS` button
is the sole confirmation and dispatch trigger. The direct graph currently
forwards a pending host action automatically and the extension handler presents
its own modal; that flow must change before implementing this screen. An
already-confirmed panel request must bypass the extension modal through trusted
extension state, never through a runbook-controlled request field. The wire
request remains the closed generic `yawr.host-action/v1` envelope. The panel and modal
must never both ask for confirmation.

Yawr does not attempt to detect when the operator has finished looking at XTS.
`Save answers and continue` is the explicit resume action. The review transition
before it is local presentation state; the engine remains blocked on the same
collector interaction.

The current direct-graph collector posts `interaction.answer` when its form is
submitted. Implementing this review state requires `Review answers` to validate
and retain a local draft without posting that message. Stage C sends the one and
only collector answer. Rerendering between B and C must preserve the local draft;
closing or replacing the run discards it without submitting.

If launch is cancelled or fails, Yawr follows the authored blocked/error route
and does not show the findings form. Cancelling the run while XTS is open cancels
the pending Yawr interaction; a late correlated acknowledgement cannot resume a
replaced run.

Raw XTS data remains in XTS, but every questionnaire value entered into Yawr must
be treated as persisted in run variables, checkpoints, and traces. The current
`ephemeral` field flag only suppresses the echoed interaction answer; it is not
end-to-end non-persistence. The default questionnaire therefore uses only
low-sensitivity categorical findings and does not collect free-text notes, raw
rows, credentials, tokens, or customer data.

If sensitive findings are required, the form records an external evidence
reference rather than the content. End-to-end ephemeral storage and trace
scrubbing are prerequisites before sensitive free text can be accepted.

Current collector execution validates required values and limited string
constraints, but does not fully enforce field types, select membership,
multiplicity, unknown fields, or all numeric constraints at the engine boundary.
Route-test collector answers cannot qualify until one shared validator is used by
both live and scheduled responses. It must enforce the complete resolved field
contract before values can affect routing.

### Navigation and Lifetime

The actual-run states are reached only through engine execution. They are not
ordinary pages in the product:

```text
Run or Debug Run
  -> engine reaches Inspect replication in XTS
  -> A Open XTS
     -> opened
     -> B Answer questions
        -> Review answers (local only)
        -> C Review answers
           -> Edit answers -> B
           -> Save answers and continue -> one interaction.answer -> D Run continues
           -> Stop run -> cancelled
```

Stage D is not a new pause. It shows that the submitted response is immutable and
that the parent decision is now executing.

Current VS Code lifecycle is the v1 boundary:

- switching to another tab preserves the graph webview and the run stays paused;
- returning to the still-open graph panel restores the same pending interaction
  and local answer draft;
- closing the graph panel sends `run.cancel`, disposes pending host actions, and
  kills the child process;
- opening another graph replaces and therefore closes the current panel; and
- there is no webview serializer or detached run session that can reopen a paused
  actual run after panel close or VS Code restart.

Therefore B/C drafts are not durable saves. Stop, panel close, replacement, or
extension shutdown discards the unsubmitted draft. Supporting durable paused
runs would require a detached runner, persisted pending-interaction identity,
draft encryption/redaction policy, reattachment, drift checks, and late-answer
rejection; it is outside v1.

After C submits, the collector answer is persisted as immutable run state and is
reviewable from that run's evidence. It cannot be edited retroactively. A
completed run may offer `Create route test from these answers`; this copies the
answer set into a new route-test draft and requires another review. It never
changes the completed run.

### Answer Sources and Review

Actual-run answers are **collected** from the operator in that run. Their
lifecycle is:

```text
editable local draft
  -> review all answers
  -> Save answers and continue
  -> one persisted, immutable collector response
  -> parent routing
```

Before final submission, `Edit answers` returns to the form. After submission,
the answers and their route effect remain reviewable in run evidence but cannot
be edited retroactively. A correction requires a new run or an explicitly
supported retry; history is never silently rewritten.

Route-test answers are **declared assumptions** for that test. Their source is
one of:

- entered manually for the test; or
- copied from a completed actual run's persisted questionnaire response.

Copied values do not become observations of the route-test run. They remain
assumptions and must be reviewed again. The test records source kind, source run
and interaction identity when applicable, answer-set digest, copy time, reviewer,
review time, and any known omissions.

`Set conditions` shows the source beside the editable values. `Check and run`
shows the source, assumed XTS launch result, every questionnaire value, and an
explicit reviewed state. `Edit conditions` remains available until Run. Starting
the test freezes the reviewed answer set; running and result evidence show the
exact frozen values consumed.

Route-test drafts are durable project artifacts once `Save draft` is selected.
A saved draft or completed route test appears under the selected step's `Saved
route tests` list and in the route-test coverage view. Reopening it returns to
`Set conditions` with its source and values visible; `Check this route` must run
again before execution. Plan or questionnaire drift marks it `Needs review` and
prevents Run until selectors, values, and provenance are reviewed again.

### Route Test

A route test never opens XTS. It schedules two independent exact responses:

```yaml
host_action_responses:
  - at:
      call_path: [inspect_replication]
      step: open_xts_view
      phase: execute
      invocation: 1
      attempt: 1
    response:
      status: completed
      result: { status: opened }
    source:
      kind: prior-run
      run_id: run-4f2a
      interaction_id: open-xts-1
      copied_at: "2026-08-28T12:00:00Z"
    review:
      state: reviewed
      reviewed_by: operator
      reviewed_at: "2026-08-28T12:05:00Z"
      sensitivity_reviewed: true

interaction_answers:
  - at:
      call_path: [inspect_replication, handle_xts_launch]
      step: record_findings
      phase: execute
      invocation: 1
      attempt: 1
    kind: collector
    values:
      xts_check_primary_health: unavailable
      xts_check_secondary_health: healthy
      xts_check_replication_lag: within_limit
    source:
      kind: prior-run
      run_id: run-4f2a
      interaction_id: record-findings-1
      answer_digest: sha256:<answer-set-digest>
      copied_at: "2026-08-28T12:00:00Z"
    review:
      state: reviewed
      reviewed_by: operator
      reviewed_at: "2026-08-28T12:05:00Z"
      sensitivity_reviewed: true
```

The first response exercises launch-status capture and the `opened` branch. The
second exercises collector validation, captures, and answer-driven routing. Both
must resolve and be consumed exactly once.

The collapsed human check counts as one operator-visible route item. Technical
details and evidence retain its two underlying bindings and every qualified
engine node; semantic route counts must not be confused with raw node counts.

The operator UI says `XTS will not open` and labels these values `Assume XTS
launch result` and `saved findings`. A passing route test may prove that Yawr
reaches the target under those declared answers. It does not prove that XTS
opens, that its data is correct, or that a human would make the same
observations.

To exercise the real XTS handoff before a dangerous target, use an ordinary
**Debug Run** with a `before` breakpoint on the target. A breakpoint pauses; it
does not stop or block execution. When Yawr reaches the breakpoint, the operator
must select `Stop`. Selecting Continue may execute the dangerous target. This is
a live, human-assisted run, not a deterministic route test and not qualifying
zero-dispatch evidence.

## Language Contract

Operator language must describe the action or observed result. Internal terms
remain available under `Technical details` and in the durable artifact.

| Internal or rejected term | Operator UI |
|---|---|
| Focus corridor | No corresponding product term |
| Enter focus mode | Show routes through this step |
| Exit focus mode | Show full graph |
| Corridor scope | Through this step / To this step / From this step |
| Scenario | Route test |
| Scenario draft | Route test draft |
| Fixture | Test data |
| Producer-result override | Adjusted result |
| Test authorization | Test approval |
| Host-action response | Assume XTS launch result |
| Collector response | Saved findings |
| Preflight | Check route |
| Deterministic proof | Route test |
| Divergence | Route changed |
| Target reached before dispatch | Step reached - command not run |
| Reachability report | Route test coverage |

Words such as `fixture`, `override`, `selector`, `invocation`, `parity`,
`determinism`, and `evidence unit` must not be required to complete the ordinary
workflow.

## Obviousness Contract

Every state must answer four questions without documentation:

1. **What am I looking at?**
2. **Which step is this about?**
3. **What happens if I use the primary action?**
4. **Can anything external run?**

Interaction rules:

- one visually dominant action per state;
- labels use a verb and a concrete object;
- the selected step remains named in every route-test state;
- safety state remains visible before and during a route test;
- advanced evidence is behind `Technical details`;
- no onboarding paragraph, tooltip tour, or external explanation is required;
- ordinary Run and Debug Run never resemble a route test;
- changes to test data are visible on both the graph and the conditions list.

If a first-time operator cannot predict a primary action's result from its label,
the label or information architecture is wrong. Adding explanatory copy is not
the default remedy.

## Operator Journey

```text
select a step
  -> Show routes through this step
  -> inspect routes to it and from it
  -> Actual or Debug Run (optional live path)
     -> Open XTS
     -> return and record findings
     -> continue from the recorded answers
  -> Test reaching this step (optional)
  -> set conditions
  -> check the expected route and safety boundary
  -> run route test
  -> step reached, command not run
     OR route changed at a named decision
  -> save the route test or fix its conditions
```

Route test coverage is a separate improvement view. It starts from an actionable
gap and returns to `Create route test`.

## Screen Contracts

### 01 Select a Step

The ordinary graph and inspector remain recognizable.

The selected-step inspector has one prominent action:

```text
Show routes through this step
```

Run and Debug Run remain in the runbook toolbar. There is no route count,
feasibility claim, route-test action, or new product terminology before the graph
filter is requested.

### 02 Routes Through This Step

The mode strip says exactly what changed:

```text
Showing routes through: Execute secondary failover
31 steps lead to it, 7 follow it, 1 dependency is unknown
```

The graph directly labels its three regions:

- `TO THIS STEP`;
- `SELECTED STEP`;
- `FROM THIS STEP`.

Scope choices are:

```text
[Through this step] [To this step] [From this step]
```

`Show full graph` restores the previous viewport and selection.

The inspector's one primary action is:

```text
Test reaching this step
```

An unknown dependency remains visible as a boundary marker. The UI does not
claim that hidden nodes are irrelevant or that displayed structural routes are
causal or feasible.

Human-assisted prerequisites render as one collapsed operator task, for example:

```text
Inspect replication in XTS
Open view, then record findings
```

Expanding the node reveals the separate launch and questionnaire steps. Automated
prerequisites remain ordinary tool nodes.

### A Open XTS During an Actual Run

The actual-run panel shows a three-stage progression:

```text
1 Open XTS -> 2 Answer questions -> 3 Review
```

It identifies the exact view, environment, and authored parameters. The primary
action is `Open XTS`. The UI explicitly says this is an actual run and that VS
Code will switch to XTS.

If the view cannot open, the run follows its authored blocked/error route and
does not advance to the questionnaire.

### B Return and Answer Questions

After an `opened` acknowledgement, the panel advances to step two and says:

```text
Waiting for you: record what you found
```

The form contains the runbook-authored questions. Values remain an editable
local draft. `Review answers` advances only the webview to the review stage; it
does not send an interaction answer or resume Yawr.

### C Review Answers Before Continuing

The review stage shows:

- source: `Collected in this actual run`;
- every question and selected value;
- the persistence and routing consequence;
- `Edit answers`; and
- one primary action: `Save answers and continue`.

Only the primary action submits the collector response and resumes Yawr. The
panel says that raw XTS data stays in XTS, the three categorical answers persist,
and the parent decision will use them.

### D Run Continues

After submission, the parent decision is current. The panel shows the immutable
answer summary and source `This actual run`. It states that the response remains
reviewable in run evidence but cannot rewrite the route already taken.

### 03 Set Conditions

The route-test workspace has two ordered steps, not three peer tabs:

```text
1 Set conditions -> 2 Check route
```

The persistent strip says:

```text
Route test - XTS and all external actions are blocked
```

The form uses plain-language groups:

- **Automated step**: an exact saved result such as incident context;
- **Human check**: answer source, assumed XTS launch result, and questionnaire
  answers;
- **Other choices**: ordinary interaction answers and test approvals.

The Human check section begins with:

```text
XTS will not open in this route test.
Choose the launch result and answers Yawr should use.
```

Answer source is visible and selectable. A copied prior-run answer set is still
labelled as an assumption for this route test.

Every condition also appears on its graph node. The primary action is
`Check this route`. Exact call path, invocation, fixture digest, and immutable
actual/effective values are available under `Technical details`.

### 04 Check and Run

The second step shows the route in order and ends at a visible stop point:

```text
Use saved incident result
-> use saved XTS findings
-> take primary unavailable route
-> Confirm failover: Proceed
-> use test approval
-> stop before Execute failover
```

The safety boundary is first in the panel:

```text
No external actions will run.
XTS, commands, tools, transfers, and connectors are blocked.
```

The operator acknowledges:

```text
This tests the runbook route, not Azure or the external service.
```

Before the expected route, the panel shows an `XTS assumptions` review containing
source, assumed launch result, primary health, secondary health, replication lag,
and review status. Nothing is hidden inside the collapsed route row.

The primary action is `Run route test`. Warnings that affect evidence quality
must be acknowledged. Any unresolved safety, binding, repeatability, or semantic
support issue blocks the action.

### 05 Test Running

The mode strip says:

```text
Testing route - XTS and external actions are blocked
4 of 6 steps
```

The panel prioritizes:

1. current step;
2. value or answer being used;
3. next step;
4. selected target and its stop-before boundary.

The only urgent action is `Stop test`. Saved automated results and saved human
answers remain visible on their graph nodes. Normal Run and Debug Run are
unavailable.

### 06 Step Reached

The result says exactly what happened:

```text
Step reached - command not run
```

The panel says:

```text
Route test passed
Yawr reached Execute secondary failover and stopped before running it.
```

The default summary contains only:

- route checks passed;
- external actions: zero;
- XTS answer sets used.

The primary action is `Save route test`. Exact qualified identity, decision
trace, consumption counts, actual/effective audit, parity, and repeatability are
under `View technical evidence`.

This state must never say that the selected command or external behavior passed.
Only the route test passed.

### 07 Route Changed

The result begins with ordinary language:

```text
The route went somewhere else
```

It selects the first decision where expected and actual routes differ. The panel
shows one direct comparison:

```text
Primary health
Expected: Unavailable
Actual:   Healthy
```

The graph uses a dashed expected route and solid actual route. The primary action
is `Fix test conditions`.

Runtime failure, stop, and blocked external dispatch remain distinct terminal
states. They do not reuse route-changed language.

### 08 Find Gaps

The default coverage view starts with work, not taxonomy:

```text
5 steps still need a route test
```

Gaps are ranked and described in ordinary language. Selecting one explains why
it is missing. The primary action is `Create route test`.

Detailed counts for steps, decision routes, checks, and outcomes remain visible
but secondary. The view always says that it measures routes through Yawr, not
external service behavior.

## Graph Filter Semantics

The temporary view is computed from the same route-aware topology as the full
graph:

```text
reverse_reachable(selected)
+ selected
+ forward_reachable(selected)
+ required structural context
```

It must preserve:

- branch arm exits and merge behavior;
- terminal routes that do not continue;
- fallback and no-match routes;
- nested branch ownership;
- exact qualified include identities;
- required parallel siblings and joins;
- unresolved dynamic includes and external event sources as boundary markers.

The filter is structural, not causal analysis. Shared state, external state, and
unresolved dependencies may affect a route without appearing as ordinary incoming
edges.

Selecting another visible node inspects it but does not silently change the step
whose routes are shown. `Show routes through this step` on the new selection is
an explicit retarget action.

For a 500-step graph, indexes are built once, target changes reuse them, and only
the filtered nodes plus boundary markers are laid out.

## Route Test Safety

A route test must dispatch no external operation, including operations labelled
read-only.

Before execution, CLI, tool, host-action, transfer, event, and connector
executors are replaced or blocked. An attempted dispatch transitions immediately
to a safety-failed terminal state, stops execution, and records no qualifying
evidence.

A test approval is plan-bound, non-production authority. It cannot authorize a
real action, become a production token, or satisfy operational sign-off.

A non-read-only target is reached only at its `before` boundary. The engine stops
before governance side effects or executor dispatch and reports `command not
run`.

## Route Test Fidelity

The route test must reuse the ordinary Yawr parser, planner, evaluator, engine
state machine, routing, capture/export logic, includes, governance, and tracing.
It must not become a second interpreter.

Route-test behavior is limited to boundary adapters:

- declared inputs;
- exact automated step results;
- exact host-action responses;
- exact choice, decision, and collector answers;
- adjusted producer results;
- controlled time and events;
- deterministic scheduling where semantics permit it;
- zero-dispatch enforcement;
- test approvals.

Every internal binding resolves to an exact qualified node, call path, phase, and
invocation. Missing, duplicate, out-of-order, cross-invocation, or unused bindings
fail the route test.

A passing route test proves reproducibility under its declared conditions. It does
not prove that the conditions are production-representative or that an external
service behaves correctly.

## End-to-End Test Use

Yes, a saved route-test scenario can drive automated end-to-end testing, but the
test name and claim must identify the boundary.

### 1. Yawr Route E2E (Automated CI)

Use the route-test artifact as a deterministic test vector. Run the real parser,
planner, include expansion, engine, host-action capture logic, collector
validation, child-to-parent capture, branch evaluation, tracing, and target
before-boundary. Replace external boundaries with the artifact's exact responses.

Required assertions include:

- zero external dispatch attempts;
- the host-action response and collector response each match and consume once;
- the `opened` launch branch and expected parent finding branch are selected;
- every scheduled response is consumed and none remains unused;
- the exact qualified target is reached at `before` and not dispatched; and
- the decision-trace digest is stable across repeated runs.

This is an end-to-end test of the Yawr route. It is not an XTS or Azure E2E test.

The existing `internal/replay.Scenario` format is insufficient: it supports
command, tool, and evidence fixtures but not exact host-action and interaction
responses. The proposed route-test schema, scheduler, shared collector validator,
and zero-dispatch adapters must be implemented before this artifact can directly
drive CI.

### 2. VS Code Handoff E2E (Automated CI)

Use the real extension host, direct graph webview, stdio protocol, and generic
host-action bridge with a fake registered `xts.openViewWithParameters` command.
Exercise:

```text
pending host action
  -> Open XTS confirmation
  -> correlated completed/opened acknowledgement
  -> collector pending
  -> local answer draft
  -> review state
  -> exactly one interaction.answer
  -> parent route
```

This proves Yawr/VS Code handoff wiring and review-before-submit behavior. It does
not prove the real XTS extension, authentication, view rendering, or data.

### 3. Real XTS Acceptance (Manual or Controlled Lab)

Use an actual or Debug Run against a non-production environment with the real XTS
extension and a safe terminal target. A human confirms that the view opens with
the expected native parameters, returns, records findings, reviews them, and
observes the expected Yawr route.

This is environment-dependent acceptance evidence, not deterministic CI. It must
not be counted as zero-dispatch route-test evidence. Production-effect testing
and dangerous target execution remain outside this proposal.

## Durable Artifact

The saved operator object is called a route test. Its technical representation
is the strict, versioned `yawr.route-test/v1` schema.

The artifact records:

- subject runbook and plan hash;
- selected target and `before` phase;
- inputs and exact automated results;
- host-action launch responses, separately from human answers;
- choice, decision, and collector answers;
- answer provenance and explicit review state;
- test approvals;
- adjusted producer results with actual/effective audit;
- route and negative assertions;
- zero-dispatch requirement;
- exact-consumption requirement;
- fixture provenance and review state;
- repeatability and parity qualification.

The exact XTS response shape is defined once in `Human-Assisted XTS Check / Route
Test`. It is illustrative, not a ratified public schema. Compilation resolves
both selectors against the frozen plan, verifies their kinds, and requires exact
one-shot consumption. A launch response cannot satisfy a collector binding, and
a collector answer cannot satisfy a host-action binding.

Saved findings record their source (`manual` or copied prior run), source run and
interaction identity when applicable, author/reviewer, copy/review times, answer
set, schema and plan digests, sensitivity review, and known omissions. Raw XTS
tables, credentials, tokens, and arbitrary view payloads are not copied into the
route test.

Operational runbooks and route tests remain separate files with different
authority.

## Responsive Behavior

### Wide

- graph and 360-420px panel are side by side;
- route scope and `Show full graph` remain in one mode strip when space permits;
- one primary panel action remains full width.

### Medium and Narrow

Below 720px:

- graph is the first row;
- panel becomes the second row/bottom region;
- the safety strip remains above both;
- graph height is at least 255px;
- no minimap is shown;
- no document or panel horizontal scrolling is allowed;
- the primary action remains visible without covering graph content.

The visual board includes a `Wide / Narrow` review control independent of browser
width.

## Accessibility

Production UI requirements:

- route-scope choices use radio-group semantics;
- mode changes use a named status region;
- safety failure and step reached use assertive announcements;
- graph nodes and boundary markers remain keyboard selectable;
- route direction, expected/actual, and state badges include text, not color alone;
- `Show full graph` restores focus to the step that opened the filtered view;
- the two setup steps use an ordered stepper, not tab semantics;
- opening Technical details preserves focus and names the selected binding;
- reduced motion removes route pulses and animated transitions.

The HTML review board exposes each product mockup as one described static figure;
only its screen navigation and width switch are interactive.

## Usability Gate

Test the proposal without giving participants the design document.

After selecting a target step, ask a first-time operator to answer:

1. What will `Show routes through this step` do?
2. Which routes are before and after the selected step?
3. During an actual run, what happens after `Open XTS`?
4. Are questionnaire answers used before the operator reviews them?
5. What action submits the collected answers and resumes Yawr?
6. During a route test, will XTS open?
7. Are copied prior-run answers observations or assumptions for the route test?
8. Where will the route test stop?
9. On a changed route, what value caused the first difference?

Pilot acceptance:

- at least 4 of 5 participants answer all nine correctly without help;
- every participant identifies the zero-external-action boundary before starting;
- no participant believes the selected target command was tested or executed;
- median time from step selection to the correct primary action is under 10 seconds.

Failure means revise labels or information architecture. Do not add an onboarding
tour to compensate.

## Open Decisions

- Which review authority qualifies route tests and test data for future policy use?
- Which route-test coverage dimensions, if any, may become enforceable after the
  GeoDR pilot?

Automatic route solving, live production connectors, production-effect testing,
and real escalation transfer/sign-off remain outside this proposal.
