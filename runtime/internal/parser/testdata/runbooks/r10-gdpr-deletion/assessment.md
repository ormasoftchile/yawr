# Assessment: Customer Data Deletion (GDPR/CCPA Compliance)

**Completeness:** 8/10 (80%)  
**Fidelity:** 7/10 (70%)  
**Verdict:** PASS WITH NOTES

## Gaps Found

| Gap ID | Class | Severity | Description |
|--------|-------|----------|-------------|
| G3-001 | G3 | HIGH | Best-effort parallel — step 6 deletes from 6 systems but prose says "continue on individual failures and report which failed"; `join.on_failure: continue` proceeds but doesn't capture per-branch failure details |
| G3-002 | G3 | HIGH | Iterate over date range — step 7 (30 daily backups) ideally expressed as iterate over date range; workaround uses numeric counter 1-30 without actual date computation |
| G2-001 | G2 | HIGH | Business-day timeout not supported — legal review (5 days), DPO review (3 days) expressed as wall-clock hours |
| G2-002 | G2 | MEDIUM | Cryptographic signing of deletion certificate not natively supported in schema |
| G2-003 | G2 | MEDIUM | SLA deadline tracking (flag if deletion >30 days) not expressible — requires runtime duration computation against GDPR deadline |

## Translation Notes

1. **Step 4 (Search All Data):** Used `type: parallel` with 6 branches for concurrent search.
   `join.on_failure: continue` allows partial results if some searches fail. Variable captures
   from each branch populate the data inventory.

2. **Step 6 (Parallel Deletion):** Used `type: parallel` with `join.on_failure: continue` for
   best-effort deletion. This is the correct behavior — proceed even if some deletions fail.
   However, the schema doesn't provide a way to capture which specific branches failed and
   report partial success details.

3. **Step 7 (Backup Deletion):** The prose says "loop through 30 daily backups". Used
   `iterate` with `max: 30`. The `iteration` counter provides 1-30 but the actual backup
   filename/date would need to be computed from the iteration number by the invoked extension.
   No native date-range iterate exists. Marked with `# GAP:` comments.

4. **Step 8 (Verification Loop):** Used `iterate` with `max: 5` and 1-hour wait between
   attempts. The convergence check uses a branch with `continue_on_fail: false` to exit
   iterate when both DB and analytics deletions are confirmed.

5. **Step 9 (Certificate):** The extension call generates the PDF but cryptographic signing
   (with company certificate) is not expressible in the schema. Marked with `# GAP:` comment.

6. **Step 12 (Compliance Log):** SLA tracking (GDPR 30-day deadline) requires computing
   elapsed time from request_timestamp to now and comparing to 30 days. The schema has no
   native time arithmetic. This would need to be handled by the extension itself.

## Recommended Schema Improvements

- Add per-branch failure capture in `type: parallel`: `branches[].capture_on_failure: [key: value]`
- Add `iterate.over: date_range(start, end, step)` for iterating over date/time ranges
- Add calendar-aware timeout support
- Add `type: sign` step for cryptographic artifact signing
- Add `elapsed_since:` template function for SLA deadline comparisons
