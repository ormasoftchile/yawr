# r21-yawr-run — `yawr run` CLI Adapter Test Fixture

**Phase:** Phase 10 — Adapters  
**Purpose:** Integration test for standalone `yawr run` CLI execution  
**Created:** 2026-04-20  
---

## Overview

This fixture exercises the `yawr run` CLI adapter in standalone mode (no `yawr serve`, no EventDispatcher). It validates:

1. ✅ **Variable overrides** via `--var` flag
2. ✅ **Infix condition evaluation** (`environment == "prod"`)
3. ✅ **CLI step execution** (echo command)
4. ✅ **Variable capture and flow** (captured vars used in subsequent steps)
5. ✅ **Trace event generation** (JSONL output to `trace.jsonl`)
6. ✅ **Serve-only constraint** (commented-out `wait_for_event` scenario)

---

## Test Scenarios

### Scenario 1: Development Deployment (Default)

```bash
yawr run testdata/runbooks/r21-yawr-run/schema.yaml
```

**Expected Behavior:**

- Input `environment` defaults to `"dev"`
- Executes branch condition: `true` (fallback path)
- Runs step: `dev_deploy`
- Output: `"Deploying to DEVELOPMENT environment"`
- Exit code: `0`

**Expected Trace Events:**

```
run/started
step/started (id: capture_env)
step/completed (id: capture_env, captured: {environment: "dev"})
step/started (id: branch_by_env)
step/completed (id: branch_by_env, branch: "Development path")
step/started (id: dev_deploy)
step/completed (id: dev_deploy, captured: {deploy_result: "Deploying to DEVELOPMENT..."})
step/started (id: complete)
step/completed (id: complete)
run/completed
```

---

### Scenario 2: Production Deployment (Variable Override)

```bash
yawr run testdata/runbooks/r21-yawr-run/schema.yaml --var environment=prod
```

**Expected Behavior:**

- Input `environment` overridden to `"prod"`
- Executes branch condition: `environment == "prod"` (first matching branch)
- Runs steps: `prod_check`, `prod_confirm`
- Output: `"PROD deployment — running safety checks"`, `"Production checks passed"`
- Exit code: `0`

**Expected Trace Events:**

```
run/started
step/started (id: capture_env)
step/completed (id: capture_env, captured: {environment: "prod"})
step/started (id: branch_by_env)
step/completed (id: branch_by_env, branch: "Production path")
step/started (id: prod_check)
step/completed (id: prod_check, captured: {check_result: "PROD deployment..."})
step/started (id: prod_confirm)
step/completed (id: prod_confirm)
step/started (id: complete)
step/completed (id: complete)
run/completed
```

---

### Scenario 3: Staging Deployment

```bash
yawr run testdata/runbooks/r21-yawr-run/schema.yaml --var environment=staging
```

**Expected Behavior:**

- Input `environment` overridden to `"staging"`
- Executes branch condition: `environment == "staging"` (second matching branch)
- Runs step: `staging_deploy`
- Output: `"Deploying to STAGING environment"`
- Exit code: `0`

---

### Scenario 4: Serve-Only Constraint (wait_for_event Rejection)

**Test Setup:**

1. Uncomment the `wait_for_event` step in `schema.yaml` (lines 110-126)
2. Run: `yawr run testdata/runbooks/r21-yawr-run/schema.yaml`

**Expected Behavior:**

- Parser accepts the `wait_for_event` step (valid schema)
- Planner resolves the step (no tool dependencies)
- Runtime Core **rejects** at dispatch time
- Error message: `"step type 'wait_for_event' requires yawr serve mode"`
- Exit code: `2` (validation error)

**Rationale:**

The `yawr run` adapter passes `nil` as the `EventDispatcher` handle to the Runtime Core. When a `wait_for_event` step is dispatched, the executor checks:

```go
if step.Type == "wait_for_event" && dispatcher == nil {
    return fmt.Errorf("step type 'wait_for_event' requires yawr serve mode")
}
```

This enforces the serve-only constraint specified in §02-architecture.tex:524.

---

## Integration Test Pattern

### Go Test Example

```go
func TestYawrRunStandalone(t *testing.T) {
    tests := []struct {
        name      string
        vars      map[string]string
        wantExit  int
        wantSteps []string
    }{
        {
            name:     "default (dev)",
            vars:     nil,
            wantExit: 0,
            wantSteps: []string{"capture_env", "branch_by_env", "dev_deploy", "complete"},
        },
        {
            name:     "production override",
            vars:     map[string]string{"environment": "prod"},
            wantExit: 0,
            wantSteps: []string{"capture_env", "branch_by_env", "prod_check", "prod_confirm", "complete"},
        },
        {
            name:     "staging override",
            vars:     map[string]string{"environment": "staging"},
            wantExit: 0,
            wantSteps: []string{"capture_env", "branch_by_env", "staging_deploy", "complete"},
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Build CLI args
            args := []string{"run", "testdata/runbooks/r21-yawr-run/schema.yaml"}
            for k, v := range tt.vars {
                args = append(args, "--var", fmt.Sprintf("%s=%s", k, v))
            }

            // Execute
            cmd := exec.Command("yawr", args...)
            output, err := cmd.CombinedOutput()

            // Assert exit code
            if exitErr, ok := err.(*exec.ExitError); ok {
                assert.Equal(t, tt.wantExit, exitErr.ExitCode())
            } else {
                assert.NoError(t, err) // exit 0
            }

            // Parse trace.jsonl
            trace := parseTrace(t, "trace.jsonl")

            // Assert step execution order
            stepIDs := extractStepIDs(trace)
            assert.Equal(t, tt.wantSteps, stepIDs)
        })
    }
}

func TestYawrRunRejectsWaitForEvent(t *testing.T) {
    // Load fixture
    rb := loadYAML(t, "testdata/runbooks/r21-yawr-run/schema.yaml")

    // Inject wait_for_event step
    waitStep := map[string]any{
        "step": map[string]any{
            "id":    "wait_approval",
            "type":  "wait_for_event",
            "title": "Wait for approval",
            "event": map[string]any{
                "source": "webhook",
                "id":     "deployment_approved",
            },
            "timeout":    "60s",
            "on_timeout": "fail",
        },
    }
    rb["flow"] = append(rb["flow"].([]any), waitStep)

    // Write modified runbook
    tmpPath := writeTempRunbook(t, rb)

    // Execute
    cmd := exec.Command("yawr", "run", tmpPath)
    output, err := cmd.CombinedOutput()

    // Assert exit code 2 (validation error)
    exitErr, ok := err.(*exec.ExitError)
    require.True(t, ok, "expected error exit")
    assert.Equal(t, 2, exitErr.ExitCode())

    // Assert error message
    assert.Contains(t, string(output), "step type 'wait_for_event' requires yawr serve mode")
}
```

---

## Trace Validation

The fixture is designed to produce deterministic trace output (modulo timestamps, run IDs, event IDs).

**Golden Trace Pattern:**

```jsonl
{"event_id":"...","run_id":"...","runbook_id":"r21-yawr-run","timestamp":"...","kind":"run/started","sequence":0,"payload":{...}}
{"event_id":"...","run_id":"...","runbook_id":"r21-yawr-run","timestamp":"...","kind":"step/started","sequence":1,"payload":{"step_id":"capture_env"}}
{"event_id":"...","run_id":"...","runbook_id":"r21-yawr-run","timestamp":"...","kind":"step/completed","sequence":2,"payload":{"step_id":"capture_env","status":"success","captured":{"environment":"dev"}}}
...
{"event_id":"...","run_id":"...","runbook_id":"r21-yawr-run","timestamp":"...","kind":"run/completed","sequence":N,"payload":{"status":"success"}}
```

**Normalization for Golden Trace Comparison:**

- Replace `event_id` with `"EVENT_ID"`
- Replace `run_id` with `"RUN_ID"`
- Replace `timestamp` with `"TIMESTAMP"`
- Replace variable durations with `"DURATION"`

This allows deterministic comparison in tests:

```go
golden := normalizeTrace(t, "testdata/golden/r21-dev.trace.jsonl")
actual := normalizeTrace(t, "trace.jsonl")
assert.Equal(t, golden, actual)
```

---

## Files

```
testdata/runbooks/r21-yawr-run/
├── schema.yaml          Main fixture runbook
├── README.md            This file
└── golden/              Golden trace files (Phase 13)
    ├── dev.trace.jsonl
    ├── prod.trace.jsonl
    └── staging.trace.jsonl
```

---

## Phase 10 Acceptance Criteria

This fixture validates the following Phase 10 deliverables:

- [ ] `yawr run` CLI parses `--var` flags correctly
- [ ] Variables flow from CLI → inputs → steps → captured vars
- [ ] Infix condition evaluation works (`environment == "prod"`)
- [ ] CLI steps execute and capture stdout
- [ ] Trace events written to `trace.jsonl` in JSONL format
- [ ] `wait_for_event` rejected with correct error message in `yawr run` mode
- [ ] Exit code 0 on success, 2 on validation error

---

## Notes

- **No mocking required:** Uses real `echo` command (available on all platforms)
- **No external dependencies:** Pure YAML + CLI execution
- **Fast execution:** Completes in <1 second
- **Extensible:** Can add more scenarios for Phase 11+ (resume, dry-run)

**See Also:**

- Phase 10 Audit: `/Volumes/Projects/yawr/.squad/tmp/barbara-phase10-audit.md`
- Architectural Decisions: `/Volumes/Projects/yawr/.squad/decisions/inbox/barbara-phase10-audit.md`
