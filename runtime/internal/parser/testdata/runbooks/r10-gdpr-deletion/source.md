# Runbook 10: Customer Data Deletion (GDPR/CCPA Compliance)

**Domain:** Compliance (GDPR/CCPA)  
**Complexity:** Parallel (deletion across systems) + Verification loop + Attestation  
**Interaction:** Human approval + automated deletion

## Summary

Processes customer data deletion request (GDPR "right to be forgotten"). Verifies request
authenticity, obtains legal approval, deletes data from all systems (database, S3, logs,
backups, analytics), verifies deletion, and generates deletion certificate.

## Steps

1. **Receive Deletion Request** (automated, type: extension)
   - Triggered by: customer submits deletion request via web form
   - Collects: customer email, request reason, request timestamp
   - Generates: deletion_request_id
   - Stores: request in compliance database

2. **Identity Verification** (human, type: manual)
   - Instructions: "Verify customer identity per data protection policy"
   - Checklist:
     - [ ] Customer responded to verification email
     - [ ] Customer provided identity document (if high-value account)
     - [ ] Verification completed within 30 days
   - Assignee: privacy-team-id
   - Timeout: 30 days (GDPR deadline)
   - Stores: verification attestation

3. **Legal Review** (human, type: approval)
   - Shows: deletion request, customer account details
   - Approver: legal-counsel-id
   - Question: "Approve data deletion? Check for legal holds."
   - Checklist:
     - [ ] No active litigation involving customer
     - [ ] No regulatory investigation requiring data retention
     - [ ] No other legal basis to retain data
   - Timeout: 5 business days
   - Stores: legal approval

4. **Search All Data Stores** (automated, type: extension, parallel)
   - Parallel searches across all systems:
     - Step 4a: Search production database (type: cli)
       - Executes: `SELECT * FROM users WHERE email = '<customer_email>'`
       - Executes: `SELECT * FROM orders WHERE user_id = '<user_id>'`
       - Stores: list of tables containing customer data
     - Step 4b: Search S3 buckets (type: cli)
       - Executes: `aws s3api list-objects --query "Contents[?contains(Key, '<user_id>')]"`
       - Stores: list of S3 keys
     - Step 4c: Search application logs (type: extension)
       - Queries Elasticsearch: `email:<customer_email>`
       - Stores: log entries with customer data
     - Step 4d: Search analytics (type: extension)
       - Queries Mixpanel API: get user profile
       - Stores: analytics events
     - Step 4e: Search CRM (type: extension)
       - Queries Salesforce API: get account records
       - Stores: CRM records
     - Step 4f: Search support tickets (type: extension)
       - Queries Zendesk API: search tickets by email
       - Stores: ticket IDs
   - Wait-all before proceeding
   - Stores: aggregated inventory of customer data

5. **DPO Review: Deletion Scope** (human, type: approval)
   - Shows: inventory of customer data from step 4
   - Approver: data-protection-officer-id
   - Question: "Confirm deletion scope is complete?"
   - Warning: "Deletion is permanent and cannot be undone"
   - Timeout: 3 business days

6. **Parallel Deletion** (automated, type: extension, parallel)
   - Execute deletions concurrently across all systems:
     - Step 6a: Delete from production database (type: cli)
       - Executes: `DELETE FROM users WHERE user_id = '<user_id>'`
       - Executes: `DELETE FROM orders WHERE user_id = '<user_id>'`
     - Step 6b: Delete from S3 (type: cli)
       - Executes: `aws s3 rm <s3_key>` for each key from step 4b
     - Step 6c: Delete from logs (type: extension)
       - Redacts customer email/PII from Elasticsearch (replace with "[DELETED]")
     - Step 6d: Delete from analytics (type: extension)
       - Calls Mixpanel API: delete user profile
     - Step 6e: Delete from CRM (type: extension)
       - Calls Salesforce API: delete account records
     - Step 6f: Delete from support tickets (type: extension)
       - Calls Zendesk API: redact customer PII from tickets
   - Failure handling: best-effort deletion (log failures, continue with other systems)

7. **Delete from Backups** (automated, type: cli, iterate)
   - Loop through all backup generations (daily for 30 days):
     - Identify: backups containing customer data
     - Recreate: backup without customer data (or mark records as deleted)
   - Max passes: 30 (daily backups)
   - Stores: list of modified backups

8. **Verification Loop** (automated, type: iterate)
   - Max passes: 5
   - Interval: 1 hour (allow replication lag)
   - Step 8a: Verify database deletion (type: cli)
     - Executes: `SELECT * FROM users WHERE user_id = '<user_id>'`
     - Asserts: no rows returned
   - Step 8b: Verify S3 deletion (type: cli)
     - Executes: `aws s3api head-object --key <s3_key>`
     - Asserts: object not found
   - Step 8c: Verify analytics deletion (type: extension)
     - Queries Mixpanel API: get user profile
     - Asserts: user not found
   - Convergence: all verifications pass
   - Fails if: data still present after 5 passes → manual investigation required

9. **Generate Deletion Certificate** (automated, type: extension)
   - Generates PDF certificate:
     - Deletion request ID
     - Customer email (redacted: first 2 chars + ***)
     - Deletion timestamp
     - Systems deleted from (list)
     - DPO signature
     - Company signature
   - Signs: PDF with company digital signature
   - Stores: certificate in compliance archive (7-year retention)

10. **DPO Attestation** (human, type: approval)
    - Shows: deletion certificate, verification results
    - Approver: data-protection-officer-id
    - Question: "Attest that data deletion is complete and verified?"
    - Stores: DPO signature

11. **Notify Customer** (automated, type: extension)
    - Sends: email to customer confirming deletion
    - Includes: deletion certificate (PDF attachment)
    - Records: notification sent timestamp (GDPR requires confirmation)

12. **Update Compliance Log** (automated, type: extension)
    - Records deletion in GDPR compliance log:
      - Request date, completion date, duration
      - Data inventory, systems deleted from
      - Verification results, DPO attestation
    - Flags: if deletion took >30 days (GDPR violation)

13. **Close Deletion Request** (automated, type: extension)
    - Updates: deletion request status to "completed"
    - Archives: all evidence (inventory, deletion logs, certificate)
    - Sends: Slack notification to privacy team

## Complexity Tags
- Parallel (search across 6 systems, delete across 6 systems)
- Looping (backup deletion, verification loop)
- Multi-party (legal, DPO attestations)
- Long-running (30-day deadline)
- Best-effort failure handling (some deletions may fail)

## Key Schema Challenges

1. **Dynamic data inventory** — Step 4 searches 6+ systems and stores inventory. Inventory
   size unknown (could be 0 records or 1000s). Schema must support variable-length data
   structures as step outputs.

2. **Best-effort parallel deletion** — Step 6 attempts deletion across all systems but
   continues even if some fail. Schema must support failure mode: continue-on-failure vs.
   fail-fast for parallel blocks.

3. **Backup iteration over date range** — Step 7 loops through 30 daily backups. Schema must
   support iteration over date ranges (not just numeric counter).

4. **Verification with eventual consistency** — Step 8 verifies deletion but allows 1-hour
   retry (replication lag). Schema must support retry with generous backoff.

5. **Digital signature of certificate** — Step 9 signs PDF with company certificate. Schema
   must support cryptographic signing with certificate management.

6. **Compliance deadline tracking** — Step 12 flags if deletion took >30 days (GDPR deadline).
   Schema must support SLA tracking and violation reporting.

7. **Partial failure reporting** — If step 6c (log redaction) fails but other deletions
   succeed, report partial success. Schema must capture detailed failure reasons per sub-task.

8. **Data redaction vs. deletion** — Steps 6c and 6f redact (not delete) data from logs and
   support tickets (can't delete logs, only redact PII). Schema must distinguish deletion
   operations (remove record) from redaction operations (replace sensitive fields).
