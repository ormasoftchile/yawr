# Project Context

- **Product intent:** Lightweight workflow runtime
- **Agent:** Quality
- **Role:** Test Integrity Engineer
- **Scope:** Mutation tests, concurrency isolation, and rejected-test repairs

## Operating Principles

- Test observable behavior rather than naming conventions.
- Require mutations that fail for the intended defect.
- Use filesystem ownership markers for mutable ProjectRoot isolation.
- Avoid scheduler-dependent concurrency assertions.
- Leave no test-generated repository debris.
