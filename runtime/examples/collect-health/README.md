# Collect Health

> Sequential service health checks with result accumulation

## What this example shows

This runbook demonstrates iteration with child runbook invocation and result accumulation. It loops over a list of services, invokes a child runbook for each, captures the results, and builds a consolidated report.

## Files

| File | Role |
|------|------|
| `collect-health.yawr` | Entry point — run this one |
| `check-service.yawr` | Included sub-runbook — checks a single service and returns status |

## How to run

```bash
yawr run collect-health.yawr
```

## Key concepts

- **Iterate with include**: Using `iterate:` to loop over a collection and invoke a child runbook each iteration
- **Result capture**: Capturing outputs from included runbooks with `capture:` on the include step
- **Accumulation pattern**: Building a running report by capturing and appending to a variable
- **Noop steps**: Using `type: noop` for variable manipulation without side effects
- **Continue on error**: Using `on_error: continue` to tolerate failures in child runbooks
