# Assessment: New Employee Onboarding

**Completeness:** 10/10 (100%)  
**Fidelity:** 10/10 (100%)  
**Verdict:** PASS

## Gaps Closed (field-types-p2)

| Gap ID | Class | Severity | Resolution |
|--------|-------|----------|------------|
| G2-005 | G2 | ~~MEDIUM~~ | **FIXED** — `manager` field in `collect_employee_info` converted from `type: text` to `type: autocomplete` with `options_from.provider: hr-directory`. Operator now gets live search against the employee directory rather than having to type the manager name free-form. |

## Gaps Closed (field-types-p1)

| Gap ID | Class | Severity | Resolution |
|--------|-------|----------|------------|
| G2-003 | G2 | ~~LOW~~ | **FIXED** — `email` field in `collect_employee_info` now carries `validation.pattern` enforcing `@company.com` domain. `pattern_hint` gives the operator a clear error message. |
| G2-004 | G2 | ~~LOW~~ | **FIXED** — `shipping_address` field in `order_laptop` step now carries `when: 'office_location == "Remote"'`. Remote employees see and must fill the field; in-office employees see neither the field nor the prompt. |

## Gaps Closed (field-types-p0)

| Gap ID | Class | Severity | Resolution |
|--------|-------|----------|------------|
| G2-002 | G2 | ~~MEDIUM~~ | **FIXED** — `department` and `office_location` now use `type: select` with static options lists. Boolean yes/no fields now use `type: boolean`. Delivery date uses `type: date`. Start date uses `type: date`. |

## Remaining Gaps

| Gap ID | Class | Severity | Description |
|--------|-------|----------|-------------|
| G2-001 | G2 | HIGH | Timeout field lacks business day support — schema uses wall-clock duration strings only; prose specifies "2 business days", "5 business days", etc. |
| G4-001 | G4 | HIGH | No self-service task pattern — step 6 (employee self-service training) forced into approval pattern with `roles: [employee]`, which is semantically incorrect |
| G2-003 | G2 | MEDIUM | `on_timeout: approve` (auto-approve on timeout, step 8) not defined in schema spec; workaround uses `on_timeout: skip` |

## Translation Notes

1. **Step 1 (Collect Info):** Used `type: collector` with 7 fields. Department and office
   location use `type: select` with static option lists. Start date uses `type: date`.
   Manager field now uses `type: autocomplete` with `options_from.provider: hr-directory` —
   the P2 live-search field type resolves the gap noted in earlier passes.

2. **Boolean fields:** All yes/no fields (background check, compliance booleans, training
   confirmations, access confirmation, manager approval) now use `type: boolean`.
   Runtime stores a proper boolean; templates can use `{{ if .field_name }}` directly.

3. **Business Day Timeouts:** Still expressed as wall-clock hours with `# GAP:` comments.
   Not resolved by field-types-p0.

4. **Step 4 (Parallel Provisioning):** Used `type: parallel` with 5 branches (3 automated tool
   calls + 2 manual collector steps). Correctly models heterogeneous parallel tasks.

5. **Step 6 (Security Training):** The prose says "assignee: employee (self-service)". Used
   `roles: [employee]` in the approvals field. This is awkward — it's not really an "approval"
   in the traditional sense. The schema doesn't distinguish self-service tasks from approval
   gates. Marked with `# GAP:` comment.

6. **Step 7 (Access Provisioning):** Used `type: branch` with 4 conditional arms based on
   department. Now that `.department` stores a canonical value string (e.g. `"Engineering"`)
   from the select field, template comparisons are reliable.

7. **Step 8 (Manager Confirm Access):** The prose says "timeout: 1 business day → auto-approve".
   Used `on_timeout: skip` as closest available option. `on_timeout: approve` is not defined in
   the schema spec.

8. **Step 10 (Dual Attestation):** Used `approvals.min: 2` with `roles: [hr-director, it-security-officer]`.
   Correctly models M-of-N approval (both must approve).

## Recommended Schema Improvements

- Add calendar-aware timeout support: `timeout: {value: 2, unit: business_days, calendar: us_federal_holidays}`
- Add `type: task` step for self-service assignments (assignee completes task, not approves)
- Add `on_timeout: auto_approve` option for approval gates
