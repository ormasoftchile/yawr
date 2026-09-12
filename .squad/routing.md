# Yawr Work Routing

| Work Type | Route To |
| --- | --- |
| Product boundaries, execution model, architecture, review | Lead |
| Workflow evaluator, state transitions, persistence interfaces | Runtime |
| CLI, SDK surface, diagnostics, packaging | Tooling |
| Test harnesses, Extension Host orchestration, CI reliability, timeout enforcement, diagnostic artifact security | Automation |
| Executable specifications, adversarial cases, non-vacuity | Tester |
| Current decisions and concise project context | Scribe |
| Backlog and continuous work coordination | Ralph |
| Safety and responsible-release checks | Rai |
| Claims, assumptions, and evidence review | Fact Checker |

Lead coordinates cross-scope work. Tester independently verifies executable
behavior. Rai and Fact Checker review release claims. All members fail closed,
limit changes to their approved scope, and use current Yawr contracts.

## Execution Rules

1. Before execution, freeze concrete scope and exclusions, exact authorized files,
   and finite measurable acceptance criteria. New discoveries are separately tracked
   follow-up work, never new completion gates.
2. Every spawn receives an explicit timeout (20 minutes by default, never over 30),
   a maximum of 3 total execution cycles, 2 review rounds, 1 replacement implementer,
   its acceptance criteria, authorized files, and stop behavior.
3. Run at most 4 agents concurrently and queue overflow. Ralph executes in batches of
   at most 4; if the executor cannot batch, fail closed.
4. Use only current-roster agents. Do not invent roles or create recursive
   replacement/reviewer chains.
5. Timeout, stall, or cap exhaustion returns `status: needs-decision` with attempted
   actions and evidence, then stops all further automatic spawning for that work item.
