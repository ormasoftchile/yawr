# Runbook 4: SOC2 Evidence Collection for Audit

**Domain:** Compliance/Audit  
**Complexity:** Nested (invoke sub-runbooks per control) + Artifact collection  
**Interaction:** Automated evidence collection + human attestation

## Summary

Collects evidence for SOC2 audit across 15 controls. Each control invokes a specialized
evidence-collection sub-runbook. Aggregates artifacts (logs, screenshots, configs, reports)
into audit package. Includes compliance officer attestation.

## Steps

1. **Initialize Audit Package** (automated, type: cli)
   - Creates: directory structure for audit evidence
   - Generates: audit_id (timestamp-based UUID)
   - Creates: manifest.json to track collected evidence
   - Fails if: insufficient disk space

2. **Control CC1.1: Access Reviews** (automated, type: invoke)
   - Invokes sub-runbook: `soc2/cc1.1-access-reviews.yaml`
   - Sub-runbook steps:
     - Export user list from Okta (API call)
     - Export access logs for past 90 days (API call)
     - Generate report: users with admin access
     - Take screenshots of access control settings
   - Outputs: `cc1.1-evidence.tar.gz` (uploaded to audit package)
   - Fails if: sub-runbook fails

3. **Control CC1.2: Password Policy** (automated, type: invoke)
   - Invokes sub-runbook: `soc2/cc1.2-password-policy.yaml`
   - Sub-runbook steps:
     - Export Okta password policy settings
     - Export MFA enforcement rules
     - Query audit log: password reset events
   - Outputs: `cc1.2-evidence.tar.gz`

4. **Control CC2.1: Risk Assessment** (human, type: collector)
   - Prompts compliance officer for:
     - Date of most recent risk assessment (date)
     - Risk register file (file upload)
     - Mitigation status (dropdown: Complete, In Progress, Planned)
   - Stores: uploaded risk register in audit package
   - Fails if: risk assessment older than 1 year

5. **Control CC3.1: Security Monitoring** (automated, type: invoke)
   - Invokes sub-runbook: `soc2/cc3.1-security-monitoring.yaml`
   - Sub-runbook steps:
     - Export CloudTrail logs for past 90 days (AWS API)
     - Export Datadog security alerts
     - Export intrusion detection logs
     - Generate summary report: security events by severity
   - Outputs: `cc3.1-evidence.tar.gz`

6. **Control CC4.1: Change Management** (automated, type: invoke)
   - Invokes sub-runbook: `soc2/cc4.1-change-management.yaml`
   - Sub-runbook steps:
     - Export GitHub pull request history (API call)
     - Export deployment logs from CI/CD system
     - Export Jira change tickets
     - Generate report: changes with approval vs. without
   - Outputs: `cc4.1-evidence.tar.gz`

7. **Control CC5.1: Incident Response** (automated, type: invoke)
   - Invokes sub-runbook: `soc2/cc5.1-incident-response.yaml`
   - Sub-runbook steps:
     - Export PagerDuty incident history
     - Export post-incident reports from wiki
     - Export security incident log
     - Generate summary: MTTD, MTTR metrics
   - Outputs: `cc5.1-evidence.tar.gz`

8. **[... 8 more control sub-runbooks, same pattern ...]** (automated, type: invoke)
   - Controls CC6.1 through CC6.8 (logical/physical access, encryption, network security, etc.)
   - Each invokes specialized sub-runbook
   - Each produces evidence artifact

9. **Aggregate Evidence** (automated, type: cli)
   - Combines all evidence files into single archive
   - Generates: audit-evidence-<audit_id>.tar.gz
   - Computes: SHA256 hash of archive
   - Updates: manifest.json with file listing and hashes

10. **Generate Audit Report** (automated, type: extension)
    - Reads manifest.json
    - Generates HTML report:
      - Control ID, status (evidence collected or missing), artifact filename, file hash
    - Includes: collection timestamp, yawr run_id, operator identity
    - Outputs: `audit-report-<audit_id>.html`

11. **Compliance Officer Review** (human, type: approval)
    - Shows: audit report HTML (inline preview)
    - Shows: list of collected artifacts
    - Approver: compliance-officer-id
    - Question: "Attest that evidence is complete and accurate?"
    - Checklist:
      - [ ] All 15 controls have evidence
      - [ ] No evidence files are missing or corrupted
      - [ ] Evidence covers required date ranges
    - Timeout: 5 business days → escalate to CISO
    - Stores: compliance officer signature and timestamp

12. **Security Officer Attestation** (human, type: approval)
    - Approver: security-officer-id
    - Question: "Attest that evidence collection process followed security controls?"
    - Timeout: 3 business days → escalate to CISO

13. **Upload to Secure Vault** (automated, type: extension)
    - Uploads audit package to S3 bucket with encryption
    - S3 path: `s3://compliance-evidence/<year>/<audit_id>/`
    - Enables: object lock (WORM) for 7 years
    - Records: S3 URI in compliance database

14. **Notify Auditor** (automated, type: extension)
    - Sends email to external auditor: auditor-id
    - Includes: S3 presigned URL (expires in 7 days)
    - Includes: audit report HTML
    - Includes: SHA256 hash of evidence archive

15. **Record Audit Completion** (automated, type: extension)
    - Updates compliance tracking system
    - Records: audit_id, completion date, evidence location, attestations
    - Triggers: reminder for next audit (1 year from now)

## Complexity Tags
- Nested (15 sub-runbook invocations)
- Artifact collection (logs, files, screenshots from each control)
- Multi-party (dual attestation)
- Long-running (spans days)

## Key Schema Challenges

1. **Nested runbook invocation** — Steps 2, 3, 5, 6, 7, 8 invoke sub-runbooks. Schema must
   support: (a) sub-runbook path resolution, (b) input passing (audit_id to sub-runbooks),
   (c) output capture (evidence files), (d) failure propagation.

2. **File artifact collection and aggregation** — Each sub-runbook produces tar.gz file. Parent
   runbook aggregates them. Schema must support file output from sub-runbook and artifact metadata
   (filename, size, hash).

3. **Evidence integrity (hashing and signing)** — Step 9 computes SHA256 of evidence archive.
   Step 11 stores compliance officer signature. Schema must support cryptographic operations.

4. **Object lock / WORM storage** — Step 13 uploads to S3 with object lock (immutable for 7
   years). Schema must support storage policies beyond simple upload (retention, versioning,
   encryption).

5. **Presigned URL generation** — Step 14 generates S3 presigned URL with expiration. Schema
   must support dynamic URL generation (not just static URLs).

6. **Bulk invocation with consistent inputs** — 15 sub-runbooks all need audit_id and date
   range. Schema must support input templating or variable scoping.

7. **Partial failure handling** — If step 5 (CC3.1) fails but other controls succeed, is
   partial evidence acceptable? Schema must support failure strategy: fail-fast vs. best-effort.
