# Runbook 2: Production Deployment with Canary + Rollback

**Domain:** DevOps/Deployment  
**Complexity:** Linear → Parallel → Branching (rollback on failure)  
**Interaction:** Human approval + automated monitoring

## Summary

Deploys new application version to production using canary strategy: deploy to 10% traffic,
monitor metrics for 10 minutes, then promote to 100% or rollback. Includes automated health
checks, metric queries, and rollback compensation.

## Steps

1. **Pre-Deployment Check** (automated, type: assert)
   - Asserts:
     - Git tag exists: `git rev-parse <tag>`
     - Docker image exists: `docker manifest inspect <image>:<tag>`
     - Staging tests passed: query CI system for test results
   - Fails if: any assertion fails (abort deployment)

2. **Human Approval: Deploy to Production** (type: approval)
   - Shows: Git changelog between current and new version
   - Shows: Docker image digest
   - Approvers: release-managers-id
   - Timeout: 30 minutes (deployment window closes)

3. **Tag Deployment Start** (automated, type: extension)
   - Calls observability API: create deployment marker with timestamp, version, actor
   - Stores: deployment_id for later correlation

4. **Register Rollback Compensation** (automated, internal)
   - Registers compensating action: "Rollback to version <current>" to be invoked if any
     subsequent step fails
   - Compensation steps: scale canary to 0%, wait 30s, delete canary deployment

5. **Deploy Canary (10% traffic)** (automated, parallel fan-out)
   - Step 5a: Create canary deployment (type: cli)
     - Executes: `kubectl apply -f canary-deployment.yaml`
   - Step 5b: Update service weights (type: cli)
     - Executes: `kubectl patch service <svc> --patch='{"spec":{"weights":[{"name":"prod","weight":90},{"name":"canary","weight":10}]}}'`
   - Parallel execution: both must succeed before proceeding
   - Fails if: deployment times out, service patch fails

6. **Wait for Canary Healthy** (automated, type: cli, retry loop)
   - Executes: `kubectl wait --for=condition=Available deployment/canary --timeout=120s`
   - Retries: 5 times with 15s backoff
   - Fails if: canary never becomes available → triggers rollback

7. **Monitor Canary Metrics (10 minutes)** (automated, type: iterate)
   - Loop for 10 minutes (60 iterations, 10s interval):
     - Query Prometheus: `rate(http_requests_total{deployment="canary",status=~"5.."}[1m])`
     - Query Prometheus: `histogram_quantile(0.95, http_request_duration_seconds{deployment="canary"})`
     - Collect: error rate, p95 latency
   - Convergence condition: error rate < 1%, p95 latency < 200ms for 5 consecutive checks
   - Early exit: if error rate > 5% or p95 > 500ms → fail step → triggers rollback
   - Fails if: metrics unreachable, query syntax error

8. **Decision: Promote or Rollback** (automated, type: decision)
   - Evaluates collected metrics:
     - If all metrics within SLO → route to "Promote"
     - If any metric outside SLO → route to "Rollback"
   - Decision logged to trace with full metric snapshot

9. **Branch: Promote to 100%** (if metrics healthy)
   - Step 9a: Human approval: "Promote canary to 100%?" (type: approval)
     - Shows: canary metrics summary (error rate, latency, throughput)
     - Approvers: release-managers-id
     - Timeout: 10 minutes → auto-rollback
   - Step 9b: Scale canary to 100% (type: cli)
     - Executes: `kubectl patch service <svc> --patch='{"spec":{"weights":[{"name":"canary","weight":100}]}}'`
   - Step 9c: Delete old prod deployment (type: cli)
     - Executes: `kubectl delete deployment prod`
   - Step 9d: Rename canary → prod (type: cli)
     - Executes: `kubectl patch deployment canary --patch='{"metadata":{"name":"prod"}}'`
   - Step 9e: Tag deployment success (type: extension)
     - Calls observability API: mark deployment as successful

10. **Branch: Rollback** (if metrics unhealthy OR step 9a timeout)
    - Step 10a: Scale canary to 0% (type: cli)
      - Executes: `kubectl patch service <svc> --patch='{"spec":{"weights":[{"name":"prod","weight":100},{"name":"canary","weight":0}]}}'`
    - Step 10b: Wait 30 seconds (type: cli)
      - Executes: `sleep 30`
    - Step 10c: Delete canary deployment (type: cli)
      - Executes: `kubectl delete deployment canary`
    - Step 10d: Tag deployment failure (type: extension)
      - Calls observability API: mark deployment as rolled back
    - Step 10e: Send alert (type: extension)
      - Sends Slack message: "Deployment rolled back due to unhealthy metrics"

11. **Post-Deployment Verification** (automated, convergent on both branches)
    - Verify prod deployment healthy (type: cli)
    - Verify service endpoints responding (type: cli)
    - Run smoke tests (type: cli): `curl https://api.company.com/health`

12. **Clear Rollback Compensation** (automated, internal)
    - Unregisters rollback compensation (deployment succeeded or already rolled back)

## Complexity Tags
- Linear → Parallel → Branching
- Looping (metrics monitoring with early exit)
- Compensation/Saga (rollback registered at step 4, executed on failure)
- Timeout/SLA (approval timeout triggers rollback)

## Key Schema Challenges

1. **Saga pattern / compensation registration** — Step 4 registers rollback actions to be
   invoked if *any* later step fails. Schema must support compensating action registration with
   scope (applies to steps 5-9) and trigger condition (any failure, specific failures, timeout).

2. **Parallel fan-out with wait-all** — Step 5 executes two kubectl commands in parallel and
   waits for both to succeed. Schema must support parallel execution with synchronization
   (wait-all vs. wait-any) and failure handling (fail if any fail, fail if all fail).

3. **Iterate with early exit and convergence** — Step 7 loops for 10 minutes but exits early if
   metrics degrade. Schema needs iterate block with: max iterations, interval, convergence
   condition (5 consecutive healthy checks), and failure condition (error rate > 5%).

4. **Conditional rollback trigger** — Rollback (step 10) is triggered by: (a) step 7 metrics
   failure, (b) step 8 decision, OR (c) step 9a approval timeout. Schema must support multiple
   trigger sources for a branch.

5. **Approval timeout with default action** — Step 9a timeout causes rollback (not escalation or
   manual intervention). Schema needs timeout action: escalate, skip, fail, or default-choice.

6. **Cross-branch convergence** — Step 11 runs after both promote and rollback branches. Schema
   must support join point (fan-in) where execution resumes on the main path regardless of which
   branch was taken.

7. **Evidence correlation** — Deployment_id from step 3 must propagate to all subsequent steps
   (OpenTelemetry trace context). Schema must support context propagation across steps and branches.
