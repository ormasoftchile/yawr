# Assessment: SOC2 Evidence Collection for Audit

**Completeness:** 9/10 (90%)  
**Fidelity:** 9/10 (90%)  
**Verdict:** PASS WITH NOTES

## Gaps Closed (field-types-p0)

| Gap ID | Class | Severity | Resolution |
|--------|-------|----------|------------|
| G2-002 | G2 | ~~MEDIUM~~ | **FIXED** — `risk_assessment_date` now uses `type: date`. `mitigation_status` now uses `type: select` with static options (Complete, In Progress, Planned). Boolean attestation yes/no fields now use `type: boolean`. |

## Remaining Gaps

| Gap ID | Class | Severity | Description |
|--------|-------|----------|-------------|
| G6-001 | G6 | HIGH | Bulk invocation verbosity — 15 sequential invoke steps for 15 controls; schema lacks `invoke_many:` or `iterate.over: imports.*` syntax |
| G2-001 | G2 | HIGH | Business-day timeout not supported — compliance officer review (5 days), security officer (3 days) expressed as wall-clock hours |
| G2-003 | G2 | MEDIUM | WORM/object-lock storage policy not expressible — S3 object lock requires separate API call outside schema |
| G2-004 | G2 | MEDIUM | Presigned URL generation not natively supported — would require an additional cli step or extension |

## Translation Notes

1. **Sub-runbook invocations (steps 2-8):** Used `type: invoke` for each control sub-runbook.
   This is the correct v2 pattern for nested runbook composition. Input propagation (audit_id,
   period_start, period_end) works correctly. The 15-step verbosity is a limitation but valid.

2. **CC6.1-CC6.8 consolidation:** Bundled the 8 CC6 controls into a single invoke step pointing
   to a hypothetical `cc6-access-controls-bundle.yaml` to reduce verbosity. In a real
   implementation, these would each be separate invoke steps.

3. **Compliance officer review (step 11):** Boolean attestation fields now use `type: boolean`.
   Business-day timeouts are still approximated as wall-clock hours.

4. **risk_assessment_date (step CC2.1):** Now `type: date` — proper ISO 8601 date with date
   picker in UI and date string stored in variable.

5. **mitigation_status (step CC2.1):** Now `type: select` with three static options.
   No more text + hint workaround.

6. **Evidence aggregation (step 9):** Used a bash cli step combining tar and sha256sum. The
   schema doesn't have a native artifact aggregation primitive.

7. **WORM storage:** The `aws s3 cp` step with `--sse` handles encryption but not object lock.
   Object lock would require a separate `aws s3api put-object-legal-hold` call. Marked with
   `# GAP:` comment.

## Recommended Schema Improvements

- Add `invoke_many:` syntax for bulk sub-runbook invocation with shared inputs
- Add calendar-aware timeout syntax
- Add `storage_policy:` field to artifact capture for retention, versioning, and encryption
- Add `generate_presigned_url:` action or `type: artifact` step
