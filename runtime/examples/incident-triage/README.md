# Incident Triage

> Multi-level incident classification and routing with nested runbook includes

## What this example shows

This is a comprehensive incident response runbook that demonstrates large-scale fan-out branching, choice-driven routing, nested runbook composition (parent → child → grandchild), and multi-file runbook organization. It routes incidents to specialized sub-runbooks based on category.

## Files

| File | Role |
|------|------|
| `incident-triage.yawr` | Entry point — run this one |
| `app-crash.yawr` | Included sub-runbook — application crash investigation |
| `network.yawr` | Included sub-runbook — network issue investigation |
| `connectivity-test.yawr` | Included by network.yawr — deep connectivity diagnostics |
| `resource-exhaustion.yawr` | Included sub-runbook — resource exhaustion investigation |

## How to run

```bash
yawr run incident-triage.yawr
```

## Key concepts

- **Choice steps**: Using `type: choice` with `variable:` and `options:` for user-driven routing
- **Include steps**: Using `type: include` with `include.runbook:` and `include.with:` to compose runbooks
- **Gate semantics**: Using `gate.stop_if:` on include steps to short-circuit parent execution
- **Multi-file composition**: Organizing complex workflows across multiple files with `imports:`
- **Nested includes**: Three-level nesting (parent → network → connectivity-test)
- **Inline vs included branches**: Some paths are inline (database, security), others include separate runbooks
- **Evidence collection**: Multiple evidence types (text, checklist) on different steps
- **Approval gates**: Separate `type: approve` steps for governance
- **Iterate with branching**: Resource exhaustion example iterates over instances with branching inside the loop

