# After Agent Reference

### After Agent Work

<!-- KNOWN PLATFORM BUGS: (1) "Silent Success" — ~7-10% of background spawns complete
     file writes but return no text. Mitigated by RESPONSE ORDER + filesystem checks.
     (2) "Server Error Retry Loop" — context overflow after fan-out. Mitigated by lean
     post-work turn + Scribe delegation + compact result presentation. -->

**⚡ Keep the post-work turn LEAN.** Coordinator's job: (1) present compact results, (2) spawn Scribe. That's ALL. No orchestration logs, no decision consolidation, no heavy file I/O.

**⚡ Context budget rule:** After collecting results from 3+ agents, use compact format (agent + 1-line outcome). Full details go in orchestration log via Scribe.

After each batch of agent work:

1. **Collect results** against each task's frozen execution contract. The task deadline is
   20 minutes by default and never more than 30 minutes. A shorter `read_agent` wait may be
   repeated only while the task remains within that deadline; collection does not reset it.

2. **Silent success detection** — when `read_agent` returns empty/no response:
   - Check filesystem: history.md modified? New decision inbox files? Output files created?
   - Files found → `"⚠️ {Name} completed (files verified) but response lost."` Treat as DONE.
   - No files → `"❌ {Name} failed — no work product."` Retry only if the work item remains
     within its 3-cycle and single-replacement budgets.

3. **Show compact results:** `{emoji} {Name} — {1-line summary of what they did}`

4. **Spawn Scribe** (background, never wait). Only if agents ran or inbox has files:

```
agent_type: "general-purpose"
model: "claude-haiku-4.5"
mode: "background"
name: "scribe"
description: "📋 Scribe: Log session & merge decisions"
prompt: |
  You are the Scribe. Read .squad/agents/scribe/charter.md.
  TEAM ROOT: {team_root}
  CURRENT_DATETIME: <resolved CURRENT_DATETIME literal>
  STATE_BACKEND: {state_backend}

  SPAWN MANIFEST: {spawn_manifest}

  FROZEN EXECUTION CONTRACT:
  - Scope: Record this completed batch only; do not expand or initiate follow-up work.
  - Authorized files: Runtime-managed Scribe state paths listed in the tasks below.
  - Acceptance criteria: Complete the measured tasks below, report counts, and make no
    unverified archival deletion.
  - Timeout: 20 minutes.
  - Execution cycle: 1 of 3 total. Review budget: 0 of 2 used.
    Replacement implementer budget: 0 of 1 used.
  - Stop behavior: On timeout, stall, ambiguity, or cap exhaustion, return
    `status: needs-decision` with attempted actions and evidence. Do not spawn another agent.

  Tasks (in order):
  0. PRE-CHECK: Run `squad_state_health` when available. If state tools are unavailable,
     stop without mutating files or git state.
  0b. PRE-CHECK: Read `decisions.md` and list `decisions/inbox` with state tools.
     Record measurements.
  1. DECISIONS ARCHIVE [HARD GATE]: If decisions.md >= 20480 bytes, archive entries older than 30 days NOW. If >= 51200 bytes, archive entries older than 7 days. Do not skip this step. Follow the ARCHIVAL SAFETY RULES below — they are not optional.
  2. DECISION INBOX: Use `squad_state_list` and `squad_state_read` on `decisions/inbox`,
     merge entries into `decisions.md` with `squad_state_write`, delete processed inbox
     entries with `squad_state_delete`, and deduplicate. Before splicing an inbox body
     beneath an `###` entry, DEMOTE its headings so its shallowest heading lands at
     `####` (`##` -> `####`). Preserve relative structure. Never emit an `##` under an `###`.
  3. ORCHESTRATION LOG: Write `orchestration-log/{timestamp}-{agent}.md` with `squad_state_write` per agent. Use ISO 8601 UTC timestamp. Replace `:` with `-` in `{timestamp}` so filenames are valid on all platforms (e.g. `2026-06-02T21-15-30Z`).
  4. SESSION LOG: Write `log/{timestamp}-{topic}.md` with `squad_state_write`. Brief. Use ISO 8601 UTC timestamp. Replace `:` with `-` in `{timestamp}` so filenames are valid on all platforms.
  5. CROSS-AGENT: Append team updates to affected agents' `agents/{agent}/history.md` with `squad_state_append`.
  6. HISTORY SUMMARIZATION [HARD GATE]: If any history.md >= 15360 bytes (15KB), summarize now. The ARCHIVAL SAFETY RULES apply here too — summarization moves content out of a file exactly like decision archival does.
  7. HEALTH REPORT: Report ENTRY COUNTS, never file sizes: `N removed from source / N added to destination` for every archival, plus inbox count processed and history files summarized. Write with `squad_state_write` or `squad_state_append`.

  ARCHIVAL SAFETY RULES (apply to every operation that moves content out of a file):
  A. DESTINATION MUST BE TRACKED. Before writing, run `git ls-files --error-unmatch <destination>`.
     Exit 0 -> proceed. Non-zero -> redirect to an existing tracked archive file, or ABORT with a
     clear error. `.squad/` is git-excluded in many checkouts: already-tracked files still commit,
     but NEW files silently never do. Moving content into an untracked destination is a DELETION,
     not an archive. Never create a new timestamped archive file and assume it will commit.
  B. APPEND FIRST, VERIFY, THEN DELETE. Append to the destination. Re-read the destination and
     confirm every moved heading is literally present AND the entry count grew by exactly the
     number moved. Only then remove from the source. If the append cannot be verified, DO NOT
     trim — leave the source intact and report the failure. Losing history is far worse than
     leaving a file over its size gate.
  C. COUNT ENTRIES, NOT BYTES. File size is not a valid integrity signal: a merge and an archive
     in the same pass move size in opposite directions, so a size delta proves nothing.
  D. NEVER REPORT A GATE OUTCOME YOU DID NOT MEASURE. "No archival required" must come from an
     actual measurement. A gate that reports without measuring is worse than no gate.
  E. If a state tool cannot perform these checks, STOP and report rather than proceeding with an
     unverified move.

  Runtime state tools own persistence. Never switch branches, push note refs, reset
  `.squad/`, or commit mutable squad state from this prompt.

  Never speak to user. ⚠️ End with plain text summary after all tool calls.
```

5. **Assess bounded follow-up:** Validate only against the frozen acceptance criteria.
   New discoveries become separately tracked follow-up work and never new gates for the current
   item. Spawn a retry/replacement/reviewer only when all applicable budgets remain:
   maximum 3 execution cycles, 2 review rounds, and 1 replacement implementer. Never create
   recursive replacement/reviewer chains or use a role outside the current roster.

6. **Stop or continue:** Timeout, stall, or cap exhaustion returns
   `status: needs-decision` with attempted actions, available evidence, unmet criteria, and the
   decision required. Stop all further automatic spawning for that item. Otherwise, if Ralph is
   active, run the next work-check cycle in batches of at most 4. Queue overflow; if the executor
   cannot batch, fail closed with `status: needs-decision`.
