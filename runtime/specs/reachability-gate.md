# Reachability Gate

Schema fields and CLI flags must have an executable test proving they affect
observable behavior through the production `cmd/yawr` path.

## Required coverage

A reachability test calls `runRun()` or the equivalent CLI entry point and
asserts a change in exit code, stdout, stderr, or trace events caused by the
field or flag under test.

Tests that only inspect parsed structs, schema validation, planner configuration,
or engine internals do not establish CLI reachability.

## Registry

`runtime/cmd/yawr/reachability_registry_test.go` lists each governed feature and
its decisive production-path test. Every registry entry must identify an
executable probe. There is no silent or documentation-only exception.

## Review rule

Any change that adds a schema field or CLI flag must add its reachability probe
in the same change. The probe must fail when the production wiring is removed.
