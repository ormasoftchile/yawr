# Y2 Results contract gap closure

Status: PASS — independently reviewed by Tester on
2026-09-12T11:48:43.2743672-07:00.

## Closed unavailable vocabulary

`runtime/internal/resultsdelivery` owns construction and strict JSON decoding of
the Results availability envelope. The only valid wire pairs are:

| status | reason |
|---|---|
| `unavailable` | `no-publication` |
| `unavailable` | `invalid-publication` |
| `redacted` | `protected-content` |
| `unavailable` | `execution-not-completed` |
| `unavailable` | `protection-unavailable` |

There is no public generic string constructor. Unknown, missing, extra,
duplicate, trailing, and status/reason-mismatched input is rejected without
changing the decode receiver. Invalid values cannot be serialized. Delivery and
serve boundaries use typed factories; durable stores continue to contain only
validated `RunResults`, never availability envelopes.

Before production edits, the former API was reproduced with:

```text
go test -C runtime ./internal/resultsdelivery -run '^TestY2ReproduceUnknownUnavailableReason$' -count=1 -v
```

It passed while logging:

```text
constructed=&{Status:redacted Reason:unknown-reason} serialized={"status":"redacted","reason":"unknown-reason"}
```

This demonstrated both unrestricted construction and serialization of an
unknown, mismatched pair.

## Independent digest vectors

`runtime/specs/run-results-digest-v1.schema.json` defines the fixed vector
document. `runtime/specs/run-results-digest-v1.json` contains exactly:

`root-false`, `root-zero`, `root-empty-string`, `root-array`,
`root-object-order-a`, `root-object-order-b`, `root-output-absent`,
`root-output-present-null`, `root-nested-null`, `parent-results`, and
`child-results`.

Each case includes the semantic record excluding `digest`, deliberately ordered
source UTF-8 and base64, canonical UTF-8 and base64, literal SHA-256 hex and
prefixed digest, canonical-with-digest UTF-8 and base64, and metadata. The
literals were generated once with a disposable Node.js standard-library-only
script that was removed afterward. Tests first verify bytes, base64, JSON,
hashes, equivalence, and distinctions using Go's standard library. Separate
parity tests compare `CanonicalResultsJSON`, `Seal`, and `Validate` to the fixed
literals.

## Production-path evidence

Tests cover direct JSON, real open-stdin stdio unavailable and chunk transport,
active served retrieval, reopened persisted retrieval, and existing successful
resume/no-replay behavior. Unit boundary tests exhaust every naturally
constructible reason and strict negative decoding. Existing authentication,
execution status, inline/chunk/reference formats, and durable publication
behavior remain unchanged.

Native authoring is externally gated and was not run or claimed. The current
handoff drop was not accessed or modified. No replacement artifact, caller
acceptance, publication, signing, or release is claimed. Review remains pending.

## Fail-closed outcome mapping

Availability is additive and never replaces execution state:

| condition | public outcome |
|---|---|
| no committed publication | `results: null`, `unavailable/no-publication` |
| pending or failed execution with no publication | original execution state plus `unavailable/no-publication` |
| non-completed execution with a committed record | original execution state plus `unavailable/execution-not-completed` |
| invalid, corrupt, or Results-over-256-MiB record | `unavailable/invalid-publication` |
| protected output or failed protection validation | `redacted/protected-content` |
| run exists but its frozen protection plan is missing/unreadable | `unavailable/protection-unavailable`; protected Vars are also withheld |
| missing run state | JSON-RPC `-32010 Run not found`; no availability object is invented |
| corrupt or unreadable run-state store | JSON-RPC `-32603 Internal error`; it is not presented as a missing run or availability reason |
| malformed JSON-RPC/params | existing parse/invalid-params protocol error |
| stdio encoding, frame-budget, or write failure | transport error/nonzero exit; no successful terminal or replacement availability state |

Unknown or contradictory availability pairs fail construction, decoding, and
serialization. An execution denied before Results normally has no committed
publication, so its failed state remains authoritative and availability is
`no-publication`; denial is not a sixth availability reason.

## Applicable limits

Units below are bytes unless stated otherwise.

| boundary | current limit |
|---|---|
| canonical Results document | 256 MiB |
| ordinary stdio frame | 1 MiB including terminating LF |
| decoded Results chunk | 65,536 bytes |
| persisted plan snapshot | 64 MiB |
| persisted state snapshot | 16 MiB |
| inline stored value threshold | 64 KiB |
| expanded stored value | 256 MiB |
| compressed stored blob | 64 MiB |
| expanded checkpoint aggregate | 256 MiB |
| stored value entries | 4,096 entries |
| served HTTP request read timeout | 30 seconds by default |
| served HTTP response write timeout | 60 seconds by default |
| served `run.get` response byte limit | **NOT SUPPORTED**; clients/proxies must impose a complete-response byte ceiling below the 256-MiB Results maximum and reject truncation |
| CLI subprocess stdout/stderr aggregate limit | **NOT SUPPORTED**; an external process supervisor must cap each stream and the aggregate before buffering |
| CLI subprocess argument, environment, stdin, or cwd size limit | **NOT SUPPORTED**; an external supervisor/admission layer must impose explicit byte/count limits |
| general CLI-step deadline | **NOT SUPPORTED**; parent cancellation is honored, but an external supervisor must impose a deadline and terminate the process tree |

## R2 file-only curl subprocess

Native support under a supplied Yawr execution profile is **NOT SUPPORTED**.
Runtime profiles describe context, attendance, approval scope, test subprocess
affordance, and tool overrides; they do not sandbox a `cli` step or define a
file-only curl policy.

This conclusion is grounded in `runtime/pkg/schema/profile.go`,
`runtime/internal/executor/cli.go`, `runtime/pkg/platform/real.go`, and the
standard-step dispatch in `runtime/internal/engine/engine.go`. Results and
persistence limits come from `runtime/pkg/engine/results.go`,
`runtime/cmd/yawr/stdio_protocol.go`, `runtime/cmd/yawr/stdio_results.go`,
`runtime/internal/runstore/dir_store.go`, and
`runtime/internal/serve/server.go`.

Current CLI execution resolves the authored command through normal OS executable
lookup (or accepts an authored path), accepts authored/interpolated arguments,
inherits the host environment and overlays declared variables, accepts arbitrary
working directory and string stdin, captures stdout/stderr into unbounded memory
buffers, and reports the exit code. Nonzero exit marks the step failed; spawn
errors and context cancellation return execution errors. `exec.CommandContext`
honors parent cancellation, but no general CLI step timeout is applied.

The runtime supplies no CLI filesystem allowlist/read-only sandbox, no network
deny or URL-scheme/host restriction, no closed stdin requirement, no stdout or
stderr cap, no process-tree resource sandbox, and no profile-owned executable,
argument, environment, or side-effect policy. Runbook governance command
allow/deny checks do not provide those controls.

Therefore an external bounded launcher is required. It must enforce an
approved absolute curl executable identity, a closed argument grammar, only
approved `file://` inputs under a read-only root, network denial including
redirects, a minimal fixed environment, fixed cwd, closed or bounded stdin,
separate and aggregate stdout/stderr ceilings, a deadline with process-tree
termination, accepted exit-code policy, and a no-write/no-side-effect filesystem
boundary before Yawr may invoke it.

## Independent Tester review

Tester reviewed the complete candidate diff from baseline
`7ffddf8b77f84bf3cf357ce9415d433b36997943` and found no compatibility
aliases, generic reason construction, wire spelling changes, caller-specific
availability behavior, unrelated production changes, or generated expected
values. All five status/reason pairs, strict mutation-safe JSON handling,
production delivery paths, fail-closed mappings, fixed vectors, budgets, and
the R2 **NOT SUPPORTED** conclusion matched the frozen brief.

Independent uncached exits were all zero:

- focused unavailable contract: 3 top-level tests, 5 exact pair subtests, and
  14 malformed-input subtests;
- digest vectors: 2 top-level tests and 11 implementation-parity subtests;
- production boundaries: 2 tests covering direct, open-stdin inline/chunk,
  active, persisted/reopened, and resume/no-replay controls;
- relevant packages: 4 packages passed;
- supplemental mappings: 4 named tests, including 4 execution-state and
  2 corrupt-store subtests;
- `npm run runtime:test`: complete runtime suite passed uncached; `cmd/yawr`
  completed in 70.794 seconds.

A separate Node.js standard-library check decoded all fixed base64 fields and
recomputed all 11 SHA-256 values directly from the literal
`canonical_utf8` bytes without calling Yawr canonicalization. Object reorder
equivalence, absent versus present-null, nested null, and parent/child origin
distinctions passed. All listed pre-review artifact hashes matched. Generated
JSONL test state was removed, the final changed-file set is authorized, and the
handoff drop remained unaccessed and unchanged.
