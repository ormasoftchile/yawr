# Assessment: Kubernetes Pod Incident Response

**Completeness:** 9/10 (90%)  
**Fidelity:** 8/10 (80%)  
**Verdict:** PASS WITH NOTES

## Corrections Applied (2026-04-19)

### type:extension → type:tool / GAP stubs

Four steps in this runbook originally used `type: extension` incorrectly. `type: extension` is **not
a step type in yawr v2** — it is a field-annotation convention (`x-<namespace>:` prefix) for attaching
metadata to runbooks and steps. Using it as a step type was wrong.

| Step ID | Was | Now | Reason |
|---------|-----|-----|--------|
| `detect_alert` | `type: extension` (prometheus-webhook receive) | `type: cli` GAP stub | Inbound event receive — no `wait_for_event` step type exists yet (GAP-3) |
| `create_pagerduty_incident` | `type: extension` (pagerduty create-incident) | `type: tool` (pagerduty-notify) | Outbound notify — correctly expressed as a tool call |
| `close_alert` | `type: extension` (prometheus-webhook resolve) | `type: tool` (alertmanager-notify) | Outbound API call — correctly expressed as a tool call |
| `notify_slack` | `type: extension` (slack send-message) | `type: tool` (slack-notify) | Outbound notify — correctly expressed as a tool call |

### The Real Gap: wait_for_event (GAP-3)

`detect_alert` is a genuine schema gap. The step's intent is to block runbook execution until an
inbound webhook payload arrives and binds its fields (timestamp, alert_id) to runbook state. The
correct step type for this — `type: wait_for_event` — does not yet exist in the yawr v2 schema.
The placeholder uses `type: cli` with a stub echo command and a `# GAP:` comment.

The `toolRefs` section was expanded to include:
- `slack-notify` (builtin://slack)
- `pagerduty-notify` (builtin://pagerduty)
- `alertmanager-notify` (builtin://alertmanager)

## Gaps Found

| Gap ID | Class | Severity | Description |
|--------|-------|----------|-------------|
| GAP-3  | G1 | HIGH | No `wait_for_event` step type — inbound webhook/event receive has no native representation; `detect_alert` uses a cli stub placeholder |
| G2-001 | G2 | HIGH | No JSON path query language for conditions beyond basic template functions; conditions use string matching (`contains`) rather than structured JSON path queries |
| G1-002 | G1 | MEDIUM | No terminal outcome steps in individual branches — each branch ends without explicit outcome declaration (only one `type: end` at the very end) |

## Translation Notes

1. **Step 1 (Detect Alert):** Originally `type: extension` with a prometheus-webhook receive action.
   Corrected to a `type: cli` GAP stub. The real implementation requires `type: wait_for_event`
   (not yet specified). In practice, webhook payloads may arrive as runbook trigger inputs — but the
   schema has no step-level mechanism to pause and wait for an inbound event mid-flow.

2. **Step 2 (Gather Diagnostics):** Used `type: parallel` with three branches for concurrent
   kubectl commands. Independent diagnostics can run simultaneously.

3. **Step 3 (Analyze Failure Mode):** Used `type: branch` with automatic condition evaluation.
   Conditions evaluate JSON output from step 2 using Go template `contains` function. The schema
   doesn't have a native JSON path query language beyond basic template functions, so string
   matching is used.

4. **Approval Gates:** Used `type: collector` with `approvals:` field for all human approval
   steps. This is the v2 pattern. Each approval has timeout + escalation.

5. **Retry Logic:** Used `retry:` field on wait steps with max=3, interval=10s, backoff=linear.

6. **Compensation:** The prose mentions potential config loss if rollback fails. No `type: compensate`
   was added because the prose doesn't explicitly describe a compensating action — this is a design
   gap in the source runbook, not the schema.

7. **Evidence Collection:** Used `cli` steps for tar + aws upload. The schema doesn't have a
   native artifact storage primitive beyond captures.

8. **Slack/PagerDuty Notifications:** Corrected from `type: extension` to `type: tool` using
   `slack-notify` and `pagerduty-notify` toolRefs.

## Recommended Schema Improvements

- Add `type: wait_for_event` step for blocking on inbound webhook/event (GAP-3)
- Add `jsonpath()` template function or native JSON path syntax in conditions
- Clarify whether every branch arm should have a `type: end` step or if convergence to a
  single end step is acceptable

