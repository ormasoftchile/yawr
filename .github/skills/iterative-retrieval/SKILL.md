---
name: "iterative-retrieval"
description: "Max-3-cycle protocol for agent sub-tasks with WHY context and coordinator validation. Use when spawning sub-agents to complete scoped work."
domain: "agent-coordination"
confidence: "high"
license: MIT
---

# Iterative Retrieval Skill

Squad agents frequently spawn sub-agents to complete scoped work. Without structure, these
handoffs become vague, cycles multiply, and outputs land without being checked. The
**Iterative Retrieval Pattern** caps cycles at 3, mandates WHY context in every spawn, and
requires the coordinator to validate agent output before closing an issue.

---

## Frozen Execution Contract

Before cycle 1, the coordinator MUST freeze and record:

- **Scope:** the concrete deliverable and explicit exclusions.
- **Authorized files:** the exact files or directories the agent may modify.
- **Acceptance criteria:** finite, measurable checks that determine completion.
- **Budgets:** timeout, remaining execution cycles, review rounds, replacement implementers,
  and concurrency slot.
- **Stop behavior:** the evidence to return and the condition that produces
  `status: needs-decision`.

The contract does not expand during execution. New discoveries are recorded as separately
tracked follow-up work; they are never added as completion gates for the current task.

### Hard Limits

| Limit | Rule |
|-------|------|
| Task/agent timeout | 20 minutes by default; an explicit override may never exceed 30 minutes |
| Execution cycles | 3 total, including the initial attempt |
| Review rounds | 2 total |
| Replacement implementers | 1 total |
| Concurrent agents | 4; queue overflow until a slot is available |

Timeout, stall, or exhaustion of any applicable cap returns `status: needs-decision` with the
attempted actions, available evidence, unmet acceptance criteria, and the decision required.
After that status, stop all further automatic spawning for the work item.

Only agents in the current roster may be spawned. Replacement implementers and reviewers may
not recursively spawn successors, create replacement/reviewer chains, or invent new roles.

---

## Spawn Prompt Template

Every agent spawn must include the following sections. Copy and fill in the template:

```
## Task
{What you need done — concrete and bounded}

## Frozen scope and authorized files
Scope: {deliverable and explicit exclusions}
Authorized files: {exact files/directories the agent may modify}

## WHY this matters
{The motivation and context. What system or user goal does this serve? What breaks if skipped?}

## Success criteria
{How you will know the output is correct. Be explicit — list acceptance criteria, not vibes.}
Example:
- [ ] File X exists and contains Y
- [ ] No regressions in existing tests
- [ ] PR is open targeting main with description matching the issue

## Execution limits
Timeout: {20 minutes by default; explicit value, never over 30 minutes}
Cycle: {N of 3 total}
Review round budget: {remaining of 2 total}
Replacement implementer budget: {remaining of 1 total}
Concurrency: {one of 4 active slots; otherwise queued}

## Escalation path
{What the agent should do if uncertain or stuck. "Stop and ask me" is valid.}
Example:
- If requirements are ambiguous, timeout/stall occurs, or a cap is exhausted → stop,
  return `status: needs-decision` with attempted evidence, and do not spawn more agents
- If blocked by a dependency → label status:blocked, explain in a comment
- Record new discoveries as separately tracked follow-up work; do not expand this task's gates
```

---

## 3-Cycle Protocol

| Cycle | Description | Exit condition |
|-------|-------------|----------------|
| **1** | Initial attempt | Done → coordinator validates. Incomplete → surface delta. |
| **2** | Targeted retry with specific corrections | Done → coordinator validates. Incomplete → one more. |
| **3** | Final attempt with all context from cycles 1–2 | Done or escalate — no cycle 4. |

### Rules

1. **After each cycle**, the coordinator evaluates the output against the success criteria
   before accepting it or spawning the next cycle.
2. **Objective context forward**: each subsequent spawn includes a summary of what was tried
   and what is still missing — not just a repeat of the original task.
3. **Cycle 3 exhausted** → return `status: needs-decision`, write a summary to
   `.squad/decisions/inbox/`, label the issue `status:needs-decision`, notify the user, and
   stop all automatic spawning for the item.
4. **Review and replacement caps**: at most 2 review rounds and 1 replacement implementer.
   A second rejection or failed replacement escalates; it does not create another chain.
5. **Concurrency cap**: never run more than 4 agents at once. Queue overflow in original
   priority order.

---

## Coordinator Validation Checklist

Before accepting agent output and closing an issue, the coordinator must check:

- [ ] All success criteria from the spawn prompt are met
- [ ] PR exists and description matches the issue (if code work)
- [ ] No obvious regressions (grep for TODO/FIXME introduced, build passes)
- [ ] Agent did not silently skip parts of the task
- [ ] If the agent reported uncertainty — was it resolved or escalated?

If any item fails → do **not** accept. Spawn cycle N+1 (up to cycle 3) with specific deltas.
Do not change the frozen scope, authorized files, or acceptance criteria to make the cycle pass.

---

## When to Escalate vs Retry

**Retry (cycle N+1)** when:
- Output is structurally correct but missing specific items
- Agent misunderstood scope (provide more context and re-run)
- Partial success — clearly identified remaining delta

**Escalate** when:
- Requirements are fundamentally unclear (decision needed)
- 3 cycles complete without convergence
- 2 review rounds or the single replacement implementer are exhausted
- A task times out, stalls, or cannot be run within the 4-agent concurrency cap
- Agent returned conflicting results across cycles
- Task requires elevated permissions or external action
- The work depends on another issue that isn't done yet

---

## Issue Dedup Check (Mandatory)

Before any agent creates a GitHub issue, it **must** search for existing open issues to avoid
duplicates.

```bash
# Check for existing open issues before creating a new one
gh issue list --search "<keywords from your issue title>" --state open
```

- If an open issue already covers the same problem → **comment on it** instead of creating a new one.
- If no duplicate → proceed to create the issue.
- Use 2–3 representative keywords from the planned issue title as the search query.

---

## Mandatory Output Requirement (Research-Then-Execute)

Every research or analysis task completed under this protocol **MUST** end with at least one
concrete action before the cycle is closed. Acceptable follow-up actions:

- GitHub issue created documenting the findings and next steps
- PR opened implementing a recommendation
- Decision recorded in `.squad/decisions/inbox/`
- Documented recommendation with a named assignee and due date

**Pure analysis reports without actionable follow-up will be rejected during triage.**
If no action is warranted, the agent must explicitly state why and get coordinator sign-off.

---

## Anti-Patterns

- **Spawning without WHY** — agents can't prioritise trade-offs without motivation context.
- **Accepting output without validating** — one failed check avoids merging broken work.
- **Cycle 4+** — if 3 cycles haven't converged, the problem is in the requirements, not the agent.
- **Recursive replacement/reviewer chains** — one replacement and two reviews are the hard stop.
- **Invented roles** — every implementer and reviewer must exist in the current roster.
- **Scope expansion during execution** — discoveries become follow-up work, not new gates.
- **Unbounded spawn prompts** — timeout, budgets, criteria, and stop behavior are mandatory.
- **Vague success criteria** — "looks good" is not a criterion. Use checkboxes.
- **Forwarding WHAT without delta** — cycle 2+ prompts must include what cycle 1 got wrong.
- **Creating issues without dedup check** — always search before creating.
- **Research without action** — delivering analysis with no issue, PR, decision, or assignee is incomplete work.

---

## Examples

### Good spawn prompt
```
## Task
Add an "Iterative Retrieval Protocol" section to `.squad/agents/coordinator/charter.md` explaining
the 3-cycle rule, WHY format, and validation checklist.

## WHY this matters
The coordinator spawns sub-agents on every round. Without a documented protocol, agents run unbounded
cycles and outputs go unvalidated — leading to stale issues and silent failures.

## Success criteria
- [ ] Section "Iterative Retrieval Protocol" exists in charter.md
- [ ] Section documents max-3-cycles rule
- [ ] Section documents WHY format requirement
- [ ] Section contains validation checklist (at least 4 items)
- [ ] No other sections of charter.md are modified

## Escalation path
If the charter.md format is unclear, check another agent charter as a reference.
If uncertain about content, stop and surface to coordinator.
```

### Bad spawn prompt (don't do this)
```
Update the coordinator charter with the iterative retrieval stuff.
```
