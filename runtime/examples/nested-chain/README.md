# Nested Chain

> Five-level deep runbook include chain for testing nesting

## What this example shows

This example demonstrates deep nesting of runbook includes (5 levels). Each level includes the next level with a delay, useful for testing rendering, execution tracing, and nested include behavior.

## Files

| File | Role |
|------|------|
| `chain-level-1.yawr` | Entry point — run this one |
| `chain-level-2.yawr` | Included by level 1 |
| `chain-level-3.yawr` | Included by level 2 |
| `chain-level-4.yawr` | Included by level 3 |
| `chain-level-5.yawr` | Included by level 4 — terminal leaf |

## How to run

```bash
yawr run chain-level-1.yawr
```

## Key concepts

- **Deep nesting**: Demonstrates 5 levels of include depth
- **Include chain**: Each runbook includes exactly one other runbook
- **Delay visualization**: Each level has a delay to show temporal progression
- **Execution tracing**: Useful for testing execution history and breadcrumb rendering
- **Minimal steps**: Each level has minimal logic to focus on nesting mechanics

