# Ceremonies

## Design Review

- **Trigger:** Before work that changes product boundaries, execution behavior, or shared contracts.
- **Facilitator:** Lead
- **Participants:** Relevant owners, with Tester for conformance implications.
- **Outcome:** Clarified scope and interfaces; record a decision only when the team actually chooses a direction.

## Conformance Review

- **Trigger:** Before declaring runtime or tooling behavior complete.
- **Facilitator:** Tester
- **Participants:** Implementer and relevant contract owner.
- **Outcome:** Evidence that real production paths were exercised and tests are non-vacuous.

## Responsible Release Review

- **Trigger:** Before user-facing release when safety, privacy, or factual claims are in scope.
- **Facilitator:** Rai or Fact Checker according to concern.
- **Outcome:** Actionable findings with evidence; fail closed on blocking findings.

## Retrospective

- **Trigger:** After a material failure, rejected review, or completed milestone.
- **Facilitator:** Lead
- **Participants:** Involved members; Scribe records durable outcomes.
- **Outcome:** Facts, root cause, and follow-up work. Do not invent backlog items outside an actual retrospective.
