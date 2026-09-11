# Work Routing

How to decide who handles what for Lightweight workflow runtime.

## Routing Table

| Work Type | Route To | Role |
|-----------|----------|------|
| Product boundaries, execution model, architecture, review | Lead | Product & Runtime Architect |
| Workflow evaluator, state transitions, persistence interfaces | Runtime | Runtime Engineer |
| CLI, SDK surface, diagnostics, packaging | Tooling | Developer Experience Engineer |
| Monorepo integration, CI reachability, artifact provenance, fidelity repairs | Integrator | Build & Migration Integrity Engineer |
| Cross-platform build isolation, release artifacts, cleanup, final migration repairs | Release | Build & Release Engineer |
| Non-vacuous mutation tests, concurrency isolation, final rejected-test repairs | Quality | Test Integrity Engineer |
| Semantic identity removal, protocol cleanup, isolated package validation | Identity | Identity Purge Engineer |
| Compatibility-code removal, repository tracking, clean provenance | Sanitizer | Repository Sanitization Engineer |
| Remove obsolete format fallbacks and approval bypasses | Modernizer | Legacy Format Purge Implementation Specialist |
| Executable specifications, adversarial cases, non-vacuity | Tester | Conformance Tester |
| Decisions, memory, and session records | Scribe | Session Logger |
| Backlog and continuous work coordination | Ralph | Work Monitor |
| Safety and responsible-release checks | Rai | RAI Reviewer |
| Claims, assumptions, and devil’s-advocate review | Fact Checker | Verifier |

## Coordination Rules

1. Route work to the member whose approved scope is the primary concern.
2. Lead owns product boundaries, execution-model coordination, architecture review, and cross-scope decisions.
3. Tester independently checks executable specifications, adversarial cases, and non-vacuity.
4. Integrator owns fidelity repairs after rejected migration/integration work and must keep retained gates CI-reachable.
5. Release owns final build-isolation and release-artifact repairs when prior migration authors are reviewer-locked.
6. Quality owns non-vacuous mutation and concurrency-isolation repairs after reviewer rejection.
7. Identity owns semantic legacy-identity removal and isolated package validation after purge rejection.
8. Sanitizer owns removal of neutral-named compatibility code and clean repository provenance after final purge rejection.
9. Modernizer owns deletion of executable old-format fallbacks and approval bypasses after final sanitization rejection.
10. Scribe records decisions and propagates durable context without performing domain work.
11. Ralph monitors work continuity and backlog state without inventing backlog items.
12. Rai reviews safety and responsible-release concerns; Fact Checker verifies claims and assumptions.
13. Apply the team principles: fail closed, minimize blast radius, prove real production paths, reject vacuous tests, and separate contracts from implementations.
