# Collect Health Parallel

> Parallel service health checks with concurrent execution

## What this example shows

This is the parallel version of collect-health. It demonstrates concurrent iteration with the `concurrency:` field, collecting results into an array, and using template functions to render the consolidated report.

## Files

| File | Role |
|------|------|
| `collect-health-parallel.runbook.yaml` | Entry point — run this one |
| `check-service.runbook.yaml` | Included sub-runbook — checks a single service and returns status |

## How to run

```bash
yawr run collect-health-parallel.runbook.yaml
```

## Key concepts

- **Concurrent iteration**: Using `concurrency: 3` on iterate to run iterations in parallel
- **Collect field**: Using `collect:` to gather per-iteration results into an array
- **Template functions**: Using `{{ join .results "\n" }}` to render collected results
- **Parallel composition**: Each iteration invokes a child runbook concurrently
- **Efficiency**: Demonstrates performance optimization for independent work

