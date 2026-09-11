# Multi-Region Rollout

> Rolling health check across regions with iterate, governance, and approvals

## What this example shows

This runbook demonstrates iteration over a collection (regions), governance controls, approval gates, and evidence collection. It validates deployment health across multiple regions with governance policies enforced.

## Files

| File | Role |
|------|------|
| `multi-region-rollout.runbook.yaml` | Entry point — run this one |

## How to run

```bash
yawr run multi-region-rollout.runbook.yaml
```

## Key concepts

- **Iteration**: Using `iterate:` with `over:`, `as:`, and nested steps
- **Governance**: Top-level `governance:` block with `allow_commands`, `deny_env_vars`, and `redact` rules
- **Approval gates**: `type: approve` step with role-based approvals
- **Defaults**: Setting default `timeout` for all steps via `defaults:` block
- **Assert steps**: Using `type: assert` to validate conditions
- **Evidence checklists**: Pre-deployment checklist with `required_evidence: kind: checklist`

