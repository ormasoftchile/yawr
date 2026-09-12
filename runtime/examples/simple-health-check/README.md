# Simple Health Check

> Basic service reachability check using ping and HTTP

## What this example shows

This is a minimal runbook that demonstrates tool steps, variable capture, and basic collector input. It pings a hostname and checks an HTTP endpoint, then asks the user to review the results.

## Files

| File | Role |
|------|------|
| `simple-health-check.runbook.yaml` | Entry point — run this one |

## How to run

```bash
yawr run simple-health-check.runbook.yaml
```

## Key concepts

- **Tool steps**: Using `type: tool` to invoke external tools (ping, curl)
- **Variable capture**: Capturing output with `capture:` for later use
- **Collector step**: Using `type: collector` to gather user notes and observations
- **End step**: Declaring terminal outcomes with `type: end` and `outcome:`

