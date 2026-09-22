# Edge Cases

> Minimal runbooks for testing edge cases and boundary conditions

## What this example shows

This collection demonstrates edge cases and minimal runbooks: single-step runbooks, timeout-only steps, and branching to multiple targets. Useful for testing parser robustness, rendering edge cases, and minimal valid runbook structures.

## Files

| File | Role |
|------|------|
| `edge-case-single-step.yawr` | Single collector step — minimal valid runbook |
| `edge-case-single-step-timeout.yawr` | Single noop step with delay — tests timeout rendering |
| `edge-case-branch.yawr` | Entry point for branching — single step that branches to two targets |
| `edge-case-branch-target-1.yawr` | First branch target |
| `edge-case-branch-target-2.yawr` | Second branch target |

## How to run

```bash
# Single step runbook
yawr run edge-case-single-step.yawr

# Single step with timeout
yawr run edge-case-single-step-timeout.yawr

# Branching to multiple targets
yawr run edge-case-branch.yawr
```

## Key concepts

- **Minimal runbooks**: Testing the smallest valid runbook structure
- **Single-step execution**: Edge case of runbooks with exactly one step
- **Timeout-only step**: Using `type: noop` with `delay:` for pure timing
- **Multi-target branching**: Demonstrating a step that can branch to multiple included runbooks
- **Boundary testing**: Useful for parser, validator, and renderer edge case testing

