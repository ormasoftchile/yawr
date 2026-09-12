# Work Routing

How to decide who handles what.

## Routing Table

| Work Type | Route To | Examples |
|-----------|----------|----------|

Preset installation adds concrete routes for the configured team. Add or edit rows
here only when their agent names also exist in the casting registry.

## Issue Routing

| Label | Action | Who |
|-------|--------|-----|
| `squad` | Triage: analyze issue, assign `squad:{member}` label | Lead |
| `squad:{name}` | Pick up issue and complete the work | Named member |

### How Issue Assignment Works

1. When a GitHub issue gets the `squad` label, the **Lead** triages it — analyzing content, assigning the right `squad:{member}` label, and commenting with triage notes.
2. When a `squad:{member}` label is applied, that member picks up the issue in their next session.
3. Members can reassign by removing their label and adding another member's label.
4. The `squad` label is the "inbox" — untriaged issues waiting for Lead review.

## Rules

1. **Freeze before execution** — record concrete scope and exclusions, exact authorized files,
   and finite measurable acceptance criteria before spawning. New discoveries are separately
   tracked follow-up work, never new completion gates.
2. **Scribe always runs** after substantial work, always as `mode: "background"`. Never blocks.
3. **Quick facts → coordinator answers directly.** Don't spawn an agent for "what port does the server run on?"
4. **When two agents could handle it**, pick the one whose domain is the primary concern.
5. **Bounded fan-out** — run at most 4 agents concurrently and queue overflow. Ralph also
   executes in batches of at most 4; if the executor cannot batch, fail closed.
6. **Bound every spawn** — pass an explicit timeout (20 minutes by default, never over 30),
   cycle budget (3 total), review budget (2 total), replacement budget (1 total), frozen
   acceptance criteria, authorized files, and `status: needs-decision` stop behavior.
7. **Issue-labeled work** — when a `squad:{member}` label is applied to an issue, route to that member. The Lead handles all `squad` (base label) triage.
8. **Roster and chain limits** — use only current-roster agents. Do not invent roles or create
   recursive replacement/reviewer chains.
9. **Fail closed on bounds** — timeout, stall, or cap exhaustion returns
   `status: needs-decision` with attempted evidence and stops all further automatic spawning
   for the work item.
