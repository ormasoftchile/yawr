# Assessment: Security Breach Containment and Forensics

**Completeness:** 8/10 (80%)  
**Fidelity:** 8/10 (80%)  
**Verdict:** PASS WITH NOTES

## Gaps Closed (field-types-p1)

| Gap ID | Class | Severity | Resolution |
|--------|-------|----------|------------|
| G2-002 | G2 | ~~MEDIUM~~ | **PARTIALLY FIXED** — `responsible_team` field in `remediation_planning` now carries `validation.pattern` restricting input to `security\|infrastructure\|engineering\|compliance`, plus `pattern_hint` for operator guidance. Full fix (dropdown) still requires `type: select`; pattern validation is a P1 interim. |
| G2-003 | G2 | ~~LOW~~ | **FIXED** — `legal_notes` field in `legal_assessment` step now carries `when: 'notification_required == "yes"'`. The field is hidden and its variable skipped when notification is not required, which matches the actual business logic. |
| G2-004 | G2 | ~~LOW~~ | **FIXED** — `target_completion_date` field in `remediation_planning` now carries `validation.pattern` enforcing `YYYY-MM-DD` format, with `pattern_hint` shown on validation failure. |

## Corrections Applied (2026-04-19)

### type:extension → type:tool / GAP stubs

Twenty-three steps in this runbook originally used `type: extension` incorrectly. `type: extension`
is **not a step type in yawr v2** — it is a field-annotation convention (`x-<namespace>:` prefix)
for attaching metadata to runbooks and steps. Using it as a step type was wrong.

**Inbound event receive → GAP stub:**

| Step ID | Was | Now | Reason |
|---------|-----|-----|--------|
| `receive_alert` | `type: extension` (siem-webhook receive) | `type: cli` GAP stub | Inbound event receive — no `wait_for_event` step type exists yet (GAP-3) |

**Outbound tool calls → type:tool:**

| Step ID | Tool | Action |
|---------|------|--------|
| `restore_network_access` | aws | restore-security-groups |
| `unsuspend_accounts` | okta | unsuspend-all-users |
| `disable_sg_ingress` | aws | revoke-sg-ingress |
| `shutdown_instances` | aws | stop-instances |
| `block_ips` | palo-alto | block-ips |
| `disable_vpn` | okta | disable-vpn-policy |
| `rotate_iam` | aws | rotate-iam-keys |
| `invalidate_jwt` | auth-service | invalidate-all-tokens |
| `force_password_reset` | okta | force-password-reset-all |
| `revoke_api_keys` | internal-api | revoke-all-keys |
| `ebs_snapshots` | aws | create-ebs-snapshots |
| `export_cloudtrail` | aws | export-cloudtrail |
| `export_app_logs` | log-exporter | export |
| `suspend_accounts_high` | okta | suspend-accounts |
| `revoke_creds_high` | credential-revoker | revoke-for-alert |
| `isolate_hosts_high` | aws | restrict-sg-egress |
| `suspend_accounts_medium_escalated` | okta | suspend-accounts |
| `close_pagerduty_low` | pagerduty-notify | resolve-alert |
| `analyze_iocs` | splunk | search-iocs |
| `stakeholder_notification` | email-notify | send |
| `close_incident` | incident-tracker | close |
| `notify_security_team` | slack-notify | send-message |

### The Real Gap: wait_for_event (GAP-3)

`receive_alert` is a genuine schema gap. The step's intent is to block runbook execution until an
inbound SIEM webhook payload arrives and binds its fields (alert_timestamp, incident_id) to runbook
state. The correct step type — `type: wait_for_event` — does not yet exist in the yawr v2 schema.
The placeholder uses `type: cli` with a stub echo command and a `# GAP:` comment.

The `toolRefs` section was expanded to include all tools referenced by corrected steps:
`aws`, `okta`, `palo-alto`, `auth-service`, `internal-api`, `credential-revoker`, `log-exporter`,
`splunk`, `email-notify`, `pagerduty-notify`, `slack-notify`, `incident-tracker`.

## Gaps Found

| Gap ID | Class | Severity | Description |
|--------|-------|----------|-------------|
| GAP-3  | G1 | HIGH | No `wait_for_event` step type — inbound webhook/event receive has no native representation; `receive_alert` uses a cli stub placeholder |
| G3-001 | G3 | CRITICAL | Cross-branch parallelism not expressible — forensics (step 8) should run concurrently with containment branches (4, 5, 6); schema's `type: parallel` cannot span branch arms |
| G3-002 | G3 | HIGH | Mid-runbook cross-branch goto not supported — step 6b (Medium Containment) needs to jump to "High Containment" branch; workaround inlines duplicate steps |
| G2-001 | G2 | HIGH | `choice` step default on timeout — prose specifies "default to High on 10-min timeout"; workaround uses `on_timeout: skip` which proceeds with pre-set default variable |
| G2-002 | G2 | LOW | Responsible team still `type: text` — pattern validation added (P1-B) constrains valid values but a `type: select` dropdown would be more ergonomic; full fix deferred |

## Translation Notes

1. **Step 1 (Receive Alert):** Originally `type: extension` with a siem-webhook receive action.
   Corrected to a `type: cli` GAP stub. The real implementation requires `type: wait_for_event`
   (not yet specified). In practice, SIEM alerts may arrive as runbook trigger inputs — but the
   schema has no step-level mechanism to pause and wait for an inbound event mid-flow.

2. **Step 2 (Triage Severity):** Used `type: choice` correctly for human severity selection.
   The "default to High on timeout" is approximated via `on_timeout: skip` with the default
   variable value set to `high`. This is a partial workaround — the skip behavior with default
   depends on runtime behavior not fully specified in the schema.

3. **Step 3-7 (Branching):** Used `type: branch` with four conditions. Critical containment
   includes nested parallel steps for isolation, credential revocation, and forensic snapshots.
   This correctly models nested parallel within a branch.

4. **Cross-Branch Parallelism (step 8):** Prose says forensics runs IN PARALLEL WITH
   containment. This is the most significant gap — the schema's `type: parallel` cannot wrap
   branch arms. Workaround: forensics runs sequentially AFTER containment. Marked with
   `# GAP:` comment.

5. **Step 6b (Medium → High escalation):** Prose says "if compromise confirmed, route to High
   Containment". Cross-branch goto is not expressible. Workaround: duplicated the essential High
   Containment steps inline within the Medium branch. Marked with `# GAP:` comment.

6. **Compensation (step 4b):** Used `type: compensate` to register rollback. The `on: failure`
   trigger is correct but note this triggers on ANY subsequent step failure, not specifically
   step 4f failure. This is a known limitation.

7. **Steps 9 and 13 (Stakeholder notification, Close incident):** Corrected from `type: extension`
   to `type: tool` using `email-notify`, `slack-notify`, and `incident-tracker` toolRefs.

## Recommended Schema Improvements

- Add `type: wait_for_event` step for blocking on inbound webhook/event (GAP-3)
- Add `async: true` step attribute for background execution (enables forensics alongside containment)
- Add cross-branch goto or `type: goto` step for mid-runbook routing
- Add `on: step_id_failure` to `type: compensate` for targeted compensation triggers
- Extend `type: choice` to support `default:` value when timeout occurs without explicit skip
