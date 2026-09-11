# Runbook 1: Kubernetes Pod Incident Response

**Domain:** SRE/Operations  
**Complexity:** Branching + Looping  
**Interaction:** Human approval gates + automated remediation

## Summary

Automated incident response for unhealthy Kubernetes pods detected by monitoring. Includes
automated diagnostics, decision-based remediation (restart vs. scale vs. escalate), and
evidence collection for post-incident review.

## Steps

1. **Detect Alert** (automated, type: extension)
   - Triggered by Prometheus AlertManager webhook
   - Collects: pod name, namespace, cluster, alert severity, timestamp
   - Fails if: webhook payload malformed or missing required fields

2. **Gather Pod Diagnostics** (automated, type: cli)
   - Executes: `kubectl get pod <pod> -n <namespace> -o json`
   - Executes: `kubectl describe pod <pod> -n <namespace>`
   - Executes: `kubectl logs <pod> -n <namespace> --tail=100`
   - Stores: pod manifest, events, recent logs as evidence artifacts
   - Fails if: kubectl not configured, cluster unreachable, pod not found

3. **Analyze Failure Mode** (automated, type: decision)
   - Evaluates pod status JSON:
     - If CrashLoopBackOff or ImagePullBackOff → route to "Fix Config"
     - If OOMKilled or resource limits hit → route to "Scale Resources"
     - If NodeNotReady → route to "Check Node Health"
     - Else → route to "Escalate to On-Call"
   - Decision logic uses expressions on pod.status.conditions
   - Fails if: pod status JSON invalid or missing required fields

4. **Branch: Fix Config** (if CrashLoopBackOff)
   - Step 4a: Collect recent config changes from Git (type: cli)
     - Executes: `git log --oneline --since="1 hour ago" -- k8s/<namespace>/<pod>.yaml`
   - Step 4b: Human approval: "Restart pod with previous config?" (type: approval)
     - Shows: diff of current vs. previous config
     - Timeout: 5 minutes → escalate to senior SRE
   - Step 4c: Rollback config (type: cli)
     - Executes: `kubectl apply -f <previous-config>.yaml`
   - Step 4d: Wait for pod healthy (type: cli, retry loop)
     - Executes: `kubectl wait --for=condition=Ready pod/<pod> -n <namespace> --timeout=120s`
     - Retries: 3 times with 10s backoff
     - Fails if: pod never becomes ready

5. **Branch: Scale Resources** (if OOMKilled)
   - Step 5a: Human approval: "Double memory limit for pod?" (type: approval)
     - Shows: current resource usage, limits, and requested increase
   - Step 5b: Patch deployment (type: cli)
     - Executes: `kubectl patch deployment <deploy> -n <namespace> --patch='...'`
   - Step 5c: Wait for new pod ready (type: cli, retry loop)

6. **Branch: Check Node Health** (if NodeNotReady)
   - Step 6a: SSH to node (type: cli)
     - Executes: `ssh <node> 'uptime && df -h && free -m'`
   - Step 6b: Collect node logs (type: cli)
     - Executes: `ssh <node> 'journalctl -u kubelet --since "10 minutes ago"'`
   - Step 6c: Escalate to infrastructure team (type: approval)
     - Approvers: infra-oncall-id
     - Timeout: 10 minutes

7. **Branch: Escalate to On-Call** (if unknown failure)
   - Step 7a: Create PagerDuty incident (type: extension)
     - Calls PagerDuty API: create incident with high severity
     - Attaches: all collected diagnostics
   - Step 7b: Wait for acknowledgment (type: approval)
     - Approver: on-call engineer (from PagerDuty)
     - Timeout: 15 minutes → escalate to manager

8. **Post-Incident Evidence** (automated, type: cli)
   - Bundle all logs, configs, decisions into tar.gz
   - Upload to S3: `s3://incidents/<timestamp>-<pod>.tar.gz`
   - Record incident summary in incident database

9. **Close Alert** (automated, type: extension)
   - Calls AlertManager API: resolve alert
   - Sends Slack notification: "Incident resolved for pod <pod>"

## Complexity Tags
- Branching (4-way decision based on failure mode)
- Looping (retry on pod health check)
- Multi-party (different approvers for different branches)
- Timeout/SLA (escalation on approval timeout)

## Key Schema Challenges

1. **Dynamic branching based on JSON evaluation** — Step 3 needs to parse kubectl JSON output
   and route to one of four branches. Schema must support expression language powerful enough
   to navigate nested JSON (pod.status.conditions[?(@.type=="Ready")].status).

2. **Retry loops with backoff** — Steps 4d and 5c retry kubectl wait commands. Schema needs
   retry policy (max attempts, backoff strategy) and failure handling (fail runbook vs. continue).

3. **Approval timeout with escalation** — Steps 4b, 6c, 7b have timeouts. Schema must define
   what happens on timeout: escalate to different approver, fail step, or skip. Escalation may
   change approver list dynamically.

4. **Evidence artifact collection** — Steps 2, 8 collect files (logs, configs, diagnostics).
   Schema must support artifact capture with metadata (filename, size, hash) and storage
   destination (local, S3, trace attachment).

5. **Cross-branch resumption** — If runbook is interrupted (yawr crashes, operator cancels),
   resume must work regardless of which branch was taken. Trace must record decision history.

6. **Compensation/rollback** — If Step 4c (config rollback) fails, original config is lost.
   Schema needs compensating action (restore original config) registered at step 4b.
