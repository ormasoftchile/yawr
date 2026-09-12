# Assessment: Financial Approval for Large Purchase

**Completeness:** 9/10 (90%)
**Fidelity:** 10/10 (100%)
**Verdict:** PASS WITH NOTES

## Gaps Fixed (field-types-p1)

| Gap ID | Class | Severity | Resolution |
|--------|-------|----------|------------|
| G2-005 | G2 | ~~LOW~~ | **FIXED** — `item_description` now uses `validation.max_length: 500` to enforce the prose requirement "max 500 chars". The redundant parenthetical was removed from the label. |

## Gaps Fixed (field-types-p0)

| Gap ID | Class | Severity | Resolution |
|--------|-------|----------|------------|
| G2-002 | G2 | ~~MEDIUM~~ | **FIXED** — `budget_line_item` now uses `type: select` with `options_from` pointing to the `finance-system` provider. `department` now uses `type: select` with static options. All approval yes/no fields now use `type: boolean`. `amount` now uses `type: number` with `validation.min: 50000`. `requested_delivery_date` now uses `type: date`. `business_justification` now uses `type: text` with `multiline: true`. |
| G2-004 | G2 | ~~MEDIUM~~ | **FIXED** — same as G2-002 above. |

## Previously Fixed Gaps

| Gap ID | Class | Severity | Resolution |
|--------|-------|----------|------------|
| G2-002 | G2 | ~~HIGH~~ | **FIXED (prev)** — All 6 approval steps now use `timeout_business_days` + `timezone: "America/New_York"` instead of approximate wall-clock hours |
| G2-003 | G2 | ~~HIGH~~ | **FIXED (prev)** — Board approval (step 7b) now uses `approvals.mode: quorum` + `pool: [board-member-1 … board-member-5]` + `required: 3` |

## Remaining Gaps

| Gap ID | Class | Severity | Description |
|--------|-------|----------|-------------|
| G2-001 | G2 | CRITICAL | Dynamic approver resolution not supported — step 2 requires looking up manager from HR system; `approvals.roles:` is a static string list; cannot use template expression or provider query |
| G2-005 | G2 | MEDIUM | Dynamic approver at each tier (director/VP from org chart) falls back to static hardcoded role names |

## Translation Notes

1. **Step 1 (Purchase Request Form):** `amount` is now `type: number` with `validation.min: 50000`
   — the schema enforces that only purchases over $50,000 can be submitted. `budget_line_item`
   uses `type: select` with `options_from` for dynamic lookup from the finance system at
   runtime. `department` uses `type: select` with static options.

2. **Amount in conditions (Step 4):** Now that `.amount` is a proper number (not a string),
   Go template conditions using `gt`/`lt`/`ge` will perform numeric comparisons correctly.
   No more fragile string comparisons.

3. **Step 2 (Manager Approval):** The prose says approver = "requester's manager (lookup from
   HR system)". This is a CRITICAL gap — the schema cannot resolve approvers dynamically.
   Workaround: hardcoded `roles: [manager]` which assumes a static role name.

4. **Step 4 (Approval Tier Decision):** Used `type: branch` with numeric range conditions.
   With `.amount` as a real number, `gt`/`lt` comparisons are now semantically correct.

5. **Step 5-7 (Tier Branches):** Director/VP approvers are hardcoded role names. Still
   requires a future `roles_from:` provider-based resolution feature.

6. **Step 7b (Board Approval):** Uses `mode: quorum` with `pool` of 5 named board-member
   roles and `required: 3`. Enforces exactly 3-of-5 semantics.

7. **Step 9 (Contract Execution):** Uses `type: file` for PDF uploads.

## Recommended Schema Improvements (remaining)

- Add `roles_from:` field with provider-based resolution: `roles_from: {provider: hr-system, query: manager_for_email, params: {email: "{{ .requester_email }}"}}`
