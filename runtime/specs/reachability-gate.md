# Reachability Gate

## The problem it solves

Four separate times a schema field was declared, schema-validated, and
unit-tested — and never actually read by production code:

1. `AllowedEnvironments` — declared and dead for months.
2. `RequiresCapabilities` — same.
3. `ProfileToolOverride.Endpoint` — parsed, unit-tested, never read at execution.
4. `Contract.Idempotent` — parsed, read in unit tests, never read by production logic.

In every case the test suite was green. Schema tests pass while the feature is
unreachable from the CLI. Phase 1B adds new fields on exactly this pattern, so
without a gate a fifth dead field is inevitable.

## Convention

A **reachability test** proves that a declared schema field or CLI flag changes
**observable CLI behavior** — exit code, stdout/stderr, or trace events — when
exercised through the real `cmd/yawr/run.go` code path (`runRun()`).

**What counts:**
- A test that calls `runRun()` or equivalent CLI entry point and asserts a
  behavioral difference (different exit code, different output) caused solely
  by the field under test.

**What does NOT count:**
- A unit test that constructs a struct directly and checks the field value.
- A schema validation test that parses YAML and asserts the field is non-zero.
- A planner/engine test that builds `planner.Config` directly and bypasses
  `cmd/yawr/run.go`.

**Why this matters:** David's Tier 0 preflight checks (PLAN-010/011/012) were
complete, unit-tested, and schema-validated — but `cmd/yawr/run.go` never
passed `Profile` to the planner config. All three checks were dead in
production. The only test that would have caught this is one that calls
`runRun()`. That is the test this gate requires.

## Naming convention

Reachability probe functions are unexported and live in `cmd/yawr/`:

```
testCLI_<FeatureName>_Reachable(t *testing.T)
```

They are called by `TestCLI_ReachabilityGate` in
`cmd/yawr/reachability_registry_test.go`.

## Adding a new field to the registry

When you add a schema field, CLI flag, or config key that has production
behavior, add an entry to `reachabilityRegistry` in
`cmd/yawr/reachability_registry_test.go`:

```go
{
    Feature:  "MyField",
    Status:   statusReachable,
    TestFunc: testCLI_MyField_Reachable,
},
```

If the field is not yet wired (known-dead), record it explicitly instead:

```go
{
    Feature:    "MyField",
    Status:     statusKnownDead,
    DeadReason: "KNOWN-DEAD: describe why; cite the Phase/Item that will wire it.",
},
```

An entry with `statusReachable` and a `nil` TestFunc fails CI immediately.
An entry with `statusKnownDead` and an empty `DeadReason` also fails CI.
There is no silent option — every field must be declared one way or the other.

## Graduating a KNOWN-DEAD entry

When the wiring ships:
1. Write `testCLI_<Feature>_Reachable` in `cmd/yawr/reachability_probes_test.go`.
2. Change the registry entry to `statusReachable` with the new `TestFunc`.
3. Remove the `DeadReason`.
4. The gate confirms the probe actually fires by running it.
