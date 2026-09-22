# Service Health Branching

> DNS and HTTP health check with nested conditional branching

## What this example shows

This runbook demonstrates multi-level branching based on tool output. It performs DNS resolution, then conditionally checks HTTP status, with different paths for success and failure at each level.

## Files

| File | Role |
|------|------|
| `service-health-branching.yawr` | Entry point — run this one |

## How to run

```bash
yawr run service-health-branching.yawr
```

## Key concepts

- **Conditional branching**: Using `type: branch` with `condition:` expressions
- **Nested branches**: Branches within branches for multi-level decision trees
- **Boolean expressions**: Using `str.contains(variable, "text")` for conditions
- **Multiple outcomes**: Different terminal states (resolved, escalated) based on path taken
- **Required evidence**: Collecting evidence with `required_evidence:` on collector steps
