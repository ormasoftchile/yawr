# Runbook 5: Security Breach Containment and Forensics

**Domain:** Security Incident Response  
**Complexity:** Branching (breach severity) + Parallel (containment + forensics) + Compensating actions  
**Interaction:** Human decision + automated containment

## Summary

Responds to detected security breach. Assesses breach severity, executes containment actions
(isolate systems, revoke credentials), runs forensics in parallel, notifies stakeholders, and
initiates remediation. Includes rollback if containment causes production outage.

## Steps

1. **Receive Security Alert** (automated, type: extension)
   - Triggered by: SIEM (Splunk, Datadog Security) webhook
   - Collects: alert ID, severity, affected systems, alert description, timestamp
   - Creates: PagerDuty incident (high urgency)
   - Fails if: webhook payload invalid

2. **Security Officer Triage** (human, type: choice)
   - Shows: alert details, affected systems, initial indicators
   - Prompt: "Assess breach severity:"
   - Options:
     - **Critical** — Active data exfiltration, ransomware, or RCE
     - **High** — Compromised credentials, unauthorized access to production
     - **Medium** — Suspicious activity, potential phishing
     - **Low** — False positive, benign anomaly
   - Assignee: security-oncall-id
   - Timeout: 10 minutes → default to High (err on side of caution)
   - Stores: severity assessment and triage notes

3. **Branch by Severity** (automated, type: decision)
   - Routes based on step 2 choice:
     - If Critical → "Critical Containment"
     - If High → "High Containment"
     - If Medium → "Medium Containment"
     - If Low → "Low Containment" (just log and close)

4. **Branch: Critical Containment** (if Critical)
   - Step 4a: Exec approval: "Initiate full lockdown?" (type: approval)
     - Approver: CISO (escalate to CEO if unavailable within 5 minutes)
     - Warning: "This will cause production outage"
   - Step 4b: Register rollback compensation (automated, internal)
     - Registers: "Restore network access, unsuspend accounts" if step 4c/4d/4e fail
   - Step 4c: Isolate affected systems (automated, type: extension, parallel)
     - Parallel tasks:
       - Disable AWS security group ingress (AWS API)
       - Shut down compromised EC2 instances (AWS API)
       - Block attacker IPs at firewall (Palo Alto API)
       - Disable VPN access (Okta API)
     - Wait-all before proceeding
   - Step 4d: Revoke all production credentials (automated, type: extension, parallel)
     - Parallel tasks:
       - Rotate AWS IAM keys (AWS API)
       - Invalidate JWT tokens (auth service API)
       - Force password reset for all users (Okta API)
       - Revoke API keys (internal API)
     - Wait-all before proceeding
   - Step 4e: Snapshot affected systems for forensics (automated, type: extension, parallel)
     - Parallel tasks:
       - Create EBS snapshots of affected EC2 volumes
       - Export CloudTrail logs (past 7 days)
       - Export application logs (past 24 hours)
       - Take memory dumps of running processes
     - Store: snapshots in isolated forensics S3 bucket
   - Step 4f: Verify production health (automated, type: cli, retry)
     - Checks: API endpoints responding, no service degradation
     - Retries: 5 times with 30s backoff
     - If fails: trigger rollback compensation (restore access)

5. **Branch: High Containment** (if High)
   - Step 5a: Suspend compromised accounts (automated, type: extension)
     - Reads: compromised user IDs from alert
     - Calls Okta API: suspend accounts
   - Step 5b: Revoke credentials for affected users (automated, type: extension)
     - Rotates: AWS keys, GitHub tokens, API keys for affected users
   - Step 5c: Isolate affected hosts (automated, type: extension)
     - Modifies security groups to block external traffic
     - Does NOT shut down instances (minimize production impact)
   - Step 5d: Snapshot for forensics (same as 4e but only affected hosts)

6. **Branch: Medium Containment** (if Medium)
   - Step 6a: Human investigation (human, type: manual)
     - Instructions: "Investigate suspicious activity in SIEM"
     - Checklist:
       - [ ] Reviewed user login history
       - [ ] Checked for lateral movement
       - [ ] Determined if compromise occurred
     - Assignee: security analyst
     - Timeout: 1 hour → escalate to senior analyst
   - Step 6b: Decision: "Compromise confirmed?" (human, type: choice)
     - Options: Yes (route to High Containment), No (route to Low Containment)

7. **Branch: Low Containment** (if Low)
   - Step 7a: Log false positive (automated, type: cli)
     - Appends to false positive log
   - Step 7b: Close PagerDuty incident (automated, type: extension)
   - Step 7c: End runbook (automated, type: end)

8. **Forensics Investigation** (automated + human, parallel with containment)
   - Runs in parallel with containment steps (4, 5, 6)
   - Step 8a: Analyze logs for IOCs (automated, type: extension)
     - Queries Splunk: search for known IOCs (IP addresses, file hashes, domains)
     - Generates: IOC match report
   - Step 8b: Malware analysis (human, type: manual)
     - Instructions: "Submit suspicious files to VirusTotal and internal sandbox"
     - Assignee: malware analyst
   - Step 8c: Timeline reconstruction (human, type: manual)
     - Instructions: "Build attack timeline from logs"
     - Assignee: forensics lead

9. **Stakeholder Notification** (automated, type: extension)
   - Sends email to: CISO, CTO, CEO, legal counsel
   - Includes: severity, affected systems, containment actions, initial findings
   - If severity == Critical: also send to board of directors

10. **Legal/Compliance Assessment** (human, type: approval)
    - Approver: legal counsel
    - Question: "Is breach notification required (GDPR, CCPA, state laws)?"
    - Shows: affected systems, user data types, jurisdictions
    - Timeout: 4 hours → escalate to outside counsel

11. **Customer Notification** (conditional, human, type: approval)
    - Only runs if step 10 == "Yes"
    - Approver: CEO AND legal counsel (both must approve)
    - Shows: draft customer notification email
    - Timeout: 24 hours (regulatory deadline)

12. **Remediation Planning** (human, type: collector)
    - Prompts security team for:
      - Root cause (text)
      - Remediation steps (multi-line text)
      - Responsible team (dropdown)
      - Target completion date (date)
    - Stores: remediation plan in incident database

13. **Close Incident** (automated, type: extension)
    - Updates incident status: contained (not resolved)
    - Creates: follow-up Jira ticket for remediation
    - Archives: all forensics evidence to S3 with 7-year retention
    - Sends: Slack notification to security team

## Complexity Tags
- Branching (4-way by severity)
- Parallel (containment + forensics run concurrently)
- Compensation/rollback (restore access if containment breaks production)
- Multi-party (exec approval, dual approval for customer notification)

## Key Schema Challenges

1. **Human choice with timeout default** — Step 2 asks security officer to choose severity,
   defaults to "High" on 10-minute timeout. Schema must support choice step with default option.

2. **Escalating approval path** — Step 4a approver is CISO, but if unavailable within 5
   minutes, escalates to CEO. Schema must support approval with primary/backup approvers.

3. **Compensation registration with conditional trigger** — Step 4b registers rollback actions
   to execute if step 4f fails (production health check). Schema must support conditional
   compensation: only execute if specific step fails, not all failures.

4. **Parallel containment + forensics** — Steps 4c-4f (containment) run in parallel with step
   8 (forensics). Two independent parallel flows, not fan-out/fan-in. Schema must support
   concurrent branches.

5. **Nested parallel within branch** — Step 4c is parallel (4 tasks), nested inside branch
   "Critical Containment". Schema must support parallel blocks within conditional branches.

6. **Mid-runbook decision with branch routing** — Step 6b (human decision) routes to either
   High Containment (step 5) or Low Containment (step 7). Schema must support jumping to
   different branches mid-flow.

7. **Conditional step execution** — Step 11 (customer notification) only runs if step 10 ==
   "Yes". Schema must support step-level conditional execution.

8. **High-stakes approval with production impact warning** — Step 4a shows warning about
   production outage. Schema must support rich approval prompts: warnings, impact descriptions.
