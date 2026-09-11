# yawr v2 — Stress-Test Runbook Corpus

These 10 runbooks are the stress-test corpus used to validate the yawr v2 schema design.
Use them as test fixtures during parser and executor development.

Each runbook directory contains:
- `source.md` — Plain-English runbook description (from Dennis's research corpus)
- `schema.yaml` — yawr v2 YAML translation
- `assessment.md` — Completeness %, Fidelity %, Verdict, Gaps found

> **Note on `# GAP:` comments in YAML files:**  
> YAML files for R4–R10 may contain inline comments of the form `# GAP: description`.
> These mark schema limitations identified during design validation. They indicate where
> the yawr v2 schema could not fully express the intended semantics of the source runbook.
> All such files remain syntactically valid YAML; the comments are informational only.

---

## Runbook Index

| # | Runbook | Domain | Completeness | Fidelity | Verdict | Schema Gaps |
|---|---------|--------|-------------|---------|---------|-------------|
| R1 | K8s Pod Incident Response | SRE/Operations | 90% | 80% | PASS WITH NOTES | No native webhook type; weak JSON querying |
| R2 | Production Canary Deployment | DevOps/Deployment | 100% | 90% | PASS | No native early-exit from iterate |
| R3 | New Employee Onboarding | HR/IT | 80% | 70% | PASS WITH NOTES | No business-day timeout; no self-service task type |
| R4 | SOC2 Evidence Collection | Compliance/Audit | 90% | 80% | PASS WITH NOTES | Bulk invocation verbosity; business-day timeout |
| R5 | Security Breach Containment | Security | 80% | 70% | PASS WITH NOTES | Cross-branch parallelism not expressible |
| R6 | Database Migration | Data Engineering | 80% | 70% | PASS WITH NOTES | No datetime-based wait; convergence-based iterate |
| R7 | Financial Approval Workflow | Finance | 70% | 60% | FAIL | Dynamic approver lookup; business-day timeouts |
| R8 | FDA Medical Device Release | Regulated Industry | 70% | 60% | FAIL | No external event wait; no 21 CFR Part 11 signatures |
| R9 | On-Call Escalation Ladder | SRE/Operations | 80% | 70% | PASS WITH NOTES | Dynamic escalation chain; cross-iterate convergence |
| R10 | GDPR Data Deletion | Compliance/GDPR | 80% | 70% | PASS WITH NOTES | Best-effort parallel; iterate over date range |

---

## Schema Readiness by Domain

*(Reproduced from validation executive summary)*

### Schema Strengths (What Works Well)

1. **Basic control flow:** Sequential steps, branching, iterate — all expressible
2. **Nested runbooks:** `invoke` with input/output passing works correctly (R4)
3. **Saga pattern:** `type: compensate` correctly models rollback (R2, R6)
4. **Parallel fan-out:** `type: parallel` works for independent concurrent tasks (R2, R3, R10)
5. **Approval gates:** `approvals:` with min/roles/timeout correctly models basic approval patterns
6. **Human interaction:** `choice`, `decision`, `collector` types cover most interactive patterns
7. **Retry with backoff:** `retry:` field works correctly (R1, R6)
8. **Conditional execution:** `when:` guards work for skip-on-condition patterns

### Schema Weaknesses (Critical Gaps)

1. **Dynamic data lookups** — Dynamic approver resolution from HR/org-chart systems (G2)
2. **Advanced parallelism** — Cross-branch parallel execution, background tasks (G3)
3. **External integrations** — Pause/resume on webhook, async event triggers (G4)
4. **Time semantics** — Business-day timeouts, datetime waits, timezone-aware scheduling (G2)

### Production Readiness Summary

| Metric | Target | Actual | Status |
|--------|--------|--------|--------|
| Runbooks translated | ≥10 | 10 | ✅ |
| Average completeness | ≥95% | 82% | ❌ |
| Average fidelity | ≥95% | 73% | ❌ |
| CRITICAL gaps | 0 | 4 | ❌ |

**Conclusion:** Schema is 80% production-ready. Address P0 gaps before declaring stable for parser implementation.

### P0 Gaps (Block Production Use)

1. Add dynamic approver resolution (`roles_from:` with provider lookup)
2. Add external event triggers (`type: wait_for_event` with webhook resume)
3. Add cross-branch parallelism (`async: true` step attribute)

### P1 Gaps (Significant Friction)

4. Calendar-aware timeouts (`timeout: {value: 2, unit: business_days}`)
5. Bulk invocation syntax (`invoke_many:` or `iterate.over: imports.*`)
6. Datetime-based wait (`wait_until: "2026-04-20T02:00:00Z"`)

---

## Methodology

Validation methodology (scoring rubric, translation protocol, gap classification):
`.squad/tmp/john-validation-methodology.md`

---

*Corpus authored by Dennis (CS Researcher). Schema translations by John (YAML/Schema Specialist).  
Date: 2026-04-18*
