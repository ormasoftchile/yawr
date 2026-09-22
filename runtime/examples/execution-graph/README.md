# Included-runbook execution in VS Code

These examples reproduce the router -> catalog investigation -> classification
shape without an incident service, XTS, MCP calls, credentials, or external
commands. All work is local `noop`, assertion, Results, or operator-choice steps.
The relative catalog binding is included in `.yawr/config.yaml`.

Install the matching YAWR VSIX, open one of the files below, and use **Yawr: Run
Current Runbook**. No path, package-map, or input configuration is required.

| Runbook in `runbooks` | Expected behavior |
| --- | --- |
| `dynamic-router.yawr` | Runtime-selected child and dynamic grandchild appear. CURRENT visits nine steps, returns to the investigation and router, then reaches Results. Three runbooks remain navigable. |
| `repeated-dynamic.yawr` | The same include site calls the same investigation twice. Both child/grandchild invocations remain distinct: five runbook frames, with two separate `collect_evidence` nodes. |
| `static-eager.yawr` | Three runbooks are visible before execution. Every executed step is followed, then execution returns to the root. |
| `static-lazy.yawr` | Same coverage with lazy runtime expansion; static preview already knows the filenames. |
| `nested-failure.yawr` | A dynamic child includes a grandchild, then deliberately fails at `deliberate_failure`. That exact node is last reached; `must_not_run` never executes. |
| `operator-review.yawr` | The real choice appears at the child `operator_classification`. The run waits until you classify the sample data. Only then does `parent_continues` execute. Cancelling instead must stop immediately. |

## What to verify

1. During execution there is one CURRENT marker. Child steps do not execute
   invisibly behind an ancestor marker.
2. After a child finishes, dashed **Return** edges show the observed transition
   back to its parent. Results stays in the ordinary visual sequence.
3. After completion, use **Runbooks in this run** to revisit every runbook,
   including both copies in the repeated example. Use **Execution history**
   to inspect the exact call/step/return order. Inspection must not restart CURRENT.
4. In the failure example, locate the exact failed child. In the operator
   example, verify the prompt and its exact node before answering or cancelling.
5. Reset removes the previous execution's dynamic frames. Running again must
   not accumulate stale frames. While inspecting a completed run, editing source
   must not rewrite its frozen child definitions; Reset applies deferred refresh.

The default pacing is 200 ms. Automated source-host and installed-VSIX cases
also exercise 500 ms with a 465 ms minimum observed dwell, preserving the
accepted 35 ms measurement tolerance. They use these same example sources.
No runtime sleeps are added for visualization.

History is retained for the life of this graph run, not automatically reopened
after disposing the panel or restarting VS Code. Durable investigation sessions
have a separate recovery model.

## Debugger boundary

The automatic CURRENT stream above is supported for static and dynamic includes.
Debugger **Step Into** is supported for static eager/lazy includes, but the
runtime still explicitly disables it for dynamic includes. Trying to enable that
flag alone also exposes a pre-existing debug protection failure:
`engine: dynamic include resolution contains protected content`. This change
does not bypass that protection or claim dynamic debugger support.
