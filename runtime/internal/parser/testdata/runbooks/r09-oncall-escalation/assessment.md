# Assessment: On-Call Escalation Ladder

**Completeness:** 8/10 (80%)  
**Fidelity:** 7/10 (70%)  
**Verdict:** PASS WITH NOTES

## Gaps Found

| Gap ID | Class | Severity | Description |
|--------|-------|----------|-------------|
| G3-001 | G3 | HIGH | Cross-iterate convergence not supported — once any iterate (steps 3-6) succeeds, remaining should be skipped; workaround uses `when: '!acknowledged'` guard on each iterate block |
| G3-002 | G3 | HIGH | Dynamic escalation chain not expressible — prose says iterate over escalation_chain list from API; workaround uses 4 explicit iterate blocks hardcoded to primary/secondary/manager/director |
| G6-001 | G6 | MEDIUM | Verbose schema — 4 identical escalation iterate patterns; schema lacks a "for each in list" escalation construct |

## Translation Notes

1. **Steps 3-6 (Escalation Loops):** The prose says "step through primary, secondary, manager,
   director" — ideally expressed as `iterate.over: [primary, secondary, manager, director]`.
   The schema's iterate block doesn't support iterating over a dynamic list from a prior step
   output. Workaround: 4 explicit iterate blocks, each targeting one escalation level. Each
   is guarded with `when: '!acknowledged'` to skip if already acknowledged.

2. **Cross-Iterate Convergence:** The key challenge is "once acknowledged, stop escalating."
   The `when:` guard on each iterate provides approximate behavior — if `acknowledged` is set to
   "true" by the break-step inside iterate 3, iterates 4-6 will be skipped. This relies on the
   runtime evaluating `when:` before starting each iterate.

3. **Step 7 (Emergency Broadcast):** Guarded with `when: '!acknowledged'`.
   Correctly skipped if any prior escalation level was acknowledged. The broadcast and war room
   creation are separate steps but both guarded.

4. **Step 8 (Wait for Ack):** Another iterate with the same polling pattern. 60 iterations ×
   1 minute = 60-minute window. Guarded to skip if already acknowledged.

5. **Fully Automated:** This runbook has no human interaction steps — only API calls and polling.
   This is correctly modeled and validates the schema's support for fully automated runbooks.

## Recommended Schema Improvements

- Add `iterate.over:` with dynamic list support for variable-length escalation chains
- Add `iterate.convergence:` field that causes the runtime to skip remaining iterates in a sequence
- Add `type: broadcast` step for simultaneous multi-target notification without wait-all semantics
