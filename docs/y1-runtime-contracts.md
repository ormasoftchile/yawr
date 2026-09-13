# Y1 runtime contracts capability evidence

**Result: PASS**

**Execution:** Automation, 2026-09-12
**Accepted baseline:** `992759fa918c05c6e452c116fbd5c3acd23cc757`
**Branch:** `milestone/y1-runtime-contracts`
**Current HEAD:** `992759fa918c05c6e452c116fbd5c3acd23cc757`

The accepted baseline is an ancestor of HEAD and is also the current HEAD. The
tested candidate was uncommitted worktree content, not a claimed commit. An
isolated temporary Git index produced candidate tree
`32c82f5938196e0bc7a58007280eb317ad99d920`; it did not stage or mutate the
user's index. The binary/full-index implementation diff has identity
`0a457cfdf387ea5c5dde4d049b9303e6c4e367a9`.

## Tested candidate files

At execution time the exact candidate change set was:

- `runtime\cmd\yawr\run.go`
- `runtime\docs\typed-results.md`
- `runtime\internal\serve\rpc.go`
- `runtime\cmd\yawr\y1_runtime_contracts_integration_test.go`
- `runtime\cmd\yawr\testdata\y1-runtime-contracts\child-blocked.runbook.yaml`
- `runtime\cmd\yawr\testdata\y1-runtime-contracts\child-success.runbook.yaml`
- `runtime\cmd\yawr\testdata\y1-runtime-contracts\native-values-served.runbook.yaml`
- `runtime\cmd\yawr\testdata\y1-runtime-contracts\native-values.runbook.yaml`
- `runtime\cmd\yawr\testdata\y1-runtime-contracts\parent-blocked.runbook.yaml`
- `runtime\cmd\yawr\testdata\y1-runtime-contracts\parent-success.runbook.yaml`

The final worktree adds only this report and its JSON manifest to those Runtime
Y1 files.

## Environment

| Item | Observed value |
|---|---|
| Windows | Microsoft Windows NT 10.0.26200.0 |
| Architecture | AMD64 |
| Go | go1.25.7 windows/amd64 |
| Node.js | v24.21.0 |
| npm | 11.19.0 |
| Git | 2.55.0.windows.5 |

Go was not initially on `PATH`. Execution used the official Go 1.25.7
disposable tree already provisioned in session state. Node/npm were invoked
only for the repository's `runtime:test` wrapper. Required local facilities
were loopback TCP, Go test temporary directories, and executable current test
binaries.

## Executed gates

| Command | Exit | Duration | Important result |
|---|---:|---:|---|
| `go test -C runtime ./cmd/yawr -run '^TestY1RuntimeContracts(DirectPersistedResume\|OpenStdinAndServedExecution\|ChildForwardingAndBlockedNoData\|UnsupportedCapabilityHasNoSideEffects)$' -count=1 -v` | 0 | 3.904 s | All four named Y1 integration tests passed. |
| `go test -C runtime ./internal/engine ./internal/runstore ./internal/serve ./internal/resultsdelivery ./pkg/engine -count=1` | 0 | 9.408 s | All directly relevant engine, store, serve, delivery, and public contract packages passed. |
| `go test -C runtime ./pkg/engine -run '^TestRunResults(CanonicalMatchesHistoricalSerialization\|RejectInvalidUTF8AndRecursiveData\|CanonicalNativeRecord)$' -count=1 -v` | 0 | — | Three canonical Results tests passed, including tamper and invalid-value rejection. |
| `go test -C runtime ./internal/runstore -run '^Test(CheckpointBudgetPendingTraceBoundary\|DirRunStore_RejectsTooManyValuesBeforeWritingBlobs)$' -count=1 -v` | 0 | — | Both checkpoint frame variants and pre-blob rejection passed. |
| `go test -C runtime ./internal/resultsdelivery ./internal/serve -run '^Test(PrepareInvalidProtectedAndNoPublication\|PublicResultsHTTPUnavailableProtection\|PublicResultsUnpublishedProtectionParity)$' -count=1 -v` | 0 | — | Availability/protection categories passed across active, reopened, failed, missing, corrupt, and nil-plan paths. |
| `npm run runtime:test` (`go test -C runtime ./... -count=1`) | 0 | 82.611 s | Every runtime package passed or had no tests; `cmd/yawr` passed in 75.839 s. |

All executable gates were uncached through `-count=1`. No required gate failed.

## C1-C8 evidence

### C1 — admission before side effects: PASS

`TestY1RuntimeContractsUnsupportedCapabilityHasNoSideEffects` supplied
unsupported capability `yawr.unsupported/v9` and a nonexistent runbook. It
required an `unsupported-capability` failure and proved the run directory and
trace file were still absent. Therefore admission occurred before runbook
read, run, store, trace, or dispatch.

### C2 — native values: PASS

`TestY1RuntimeContractsDirectPersistedResume`,
`TestY1RuntimeContractsOpenStdinAndServedExecution`, and
`TestRunResultsCanonicalNativeRecord` preserved and asserted:

- `false`;
- integer zero through a `json.Number` decoder;
- empty string;
- empty array;
- empty object;
- a present nested null;
- Unicode (`Español`, `Niño`, and emoji).

The assertions cross parsing, engine execution, Results publication, direct
JSON, stdio, HTTP, resume, and persistence boundaries.

### C3 — named typed Results and atomic identity: PASS

`TestRunResultsCanonicalNativeRecord` builds a named typed record with schema
version, publication ID, plan-snapshot digest, checkpoint sequence, origin,
and outputs; `Seal` and `Validate` pass. A post-seal mutation is rejected by
digest validation without mutating the original. The historical serialization
and invalid UTF-8/recursive-data controls also pass.

The integration tests then prove the complete canonical record remains equal
through resume and persisted `run.get`, rather than reconstructing separate
fields at delivery time.

### C4 — exact child forwarding: PASS

`TestY1RuntimeContractsChildForwardingAndBlockedNoData` runs a real composable
child and parent fixture. The parent's publication must deeply equal the
child's false, zero, empty string/list/map, nested null, and Unicode object.

### C5 — blocked intentional no-result safety: PASS

The same test runs an intentional `blocked` child with code
`intentional-no-result`. The parent reports `results: null` with
`results_unavailable.reason=no-publication`, and the downstream counter file
does not exist. No result or later dispatch is fabricated.

### C6 — outcome versus result availability: PASS

`TestPrepareInvalidProtectedAndNoPublication`,
`TestPublicResultsHTTPUnavailableProtection`,
`TestPublicResultsUnpublishedProtectionParity`, and the blocked integration
case distinguish:

- `no-publication`;
- `invalid-publication`;
- `protected-content`;
- `protection-unavailable`;
- running, completed, failed, and blocked execution status.

Presentation withholding does not rewrite execution failure, and execution
status does not invent result availability.

### C7 — CLI/stdio/served/persisted/resume equivalence: PASS

`TestY1RuntimeContractsDirectPersistedResume` compares direct CLI JSON,
resume, and persisted `run.get`; all retain the same run ID and canonical
Results. Its producer counter remains exactly one after resume and persisted
read.

`TestY1RuntimeContractsOpenStdinAndServedExecution` keeps stdin open, consumes
the actual newline-delimited stream through `run.finished`, validates its
Results, starts a real loopback server, advances through `run.next`, reads
active `run.get`, restarts the server, and compares reopened persisted
`run.get`. `TestPublicResultsHTTPActiveReopenedNoRepeat` independently covers
active/reopened HTTP equality and a one-execution counter.

### C8 — atomic budget rejection: PASS

`TestCheckpointBudgetPendingTraceBoundary/frame-false` and `/frame-true` first
save and reload a valid 4096-entry checkpoint. A 4097th value is rejected
before publication. The prior checkpoint remains deeply equal, blob count is
unchanged, and checkpoint sequence 2 does not exist.

`TestDirRunStore_RejectsTooManyValuesBeforeWritingBlobs` additionally proves a
first rejected save with a large value creates no blob directory.

## Negative controls and non-vacuity

- **No mock/caller substitution:** integration subprocesses enter production
  `runRun`/`runServe`, parse real YAML, execute the production engine, and use
  real filesystem persistence. Runrail and caller projects were not accessed.
- **No direct `RunResults`/`sendFinished` shortcut:** fixtures contain actual
  `results` steps. Evidence is collected from direct JSON, the actual
  `run.finished` frame, and `run.get`.
- **No cached helper binary:** the helper is `os.Executable()` for the
  just-built current-package Go test binary. The pre-existing root `yawr.exe`
  was not invoked or modified.
- **Transport is exercised:** stdio scans actual frames while stdin is open;
  served coverage binds loopback TCP and sends HTTP JSON-RPC, including server
  restart/reopen.
- **Budget test is non-vacuous:** a valid boundary checkpoint is proven first,
  followed by a deliberately invalid save with exact no-mutation assertions.
- **No caller inference:** only Yawr behavior is claimed. No caller acceptance
  is asserted.

## Artifact hashes

| File | SHA-256 |
|---|---|
| `runtime\cmd\yawr\run.go` | `416a7dee9240a74dfe9ca971cc008e146955a8c98833b4622d3ad42bd8751bbe` |
| `runtime\internal\serve\rpc.go` | `c859e55afb4cfbb63b7f2ac3dfbe460127e490fb59a49a9794cc775c57c00de5` |
| `runtime\docs\typed-results.md` | `b6353058ce0e17855b61b3256e880b8bfdd3ba819e507a68b349b6876d7d901d` |
| `runtime\cmd\yawr\y1_runtime_contracts_integration_test.go` | `50a322d797fc64ea2d8e27816559a55acc71ebd40e154c17c8657e2460c3ff9d` |
| `child-blocked.runbook.yaml` | `c24a5e81b810c3ff0e4641975308df0392f730d2c81704c304ae614d1c10edea` |
| `child-success.runbook.yaml` | `344deabcd3dfbc5ddee2310e0b56f1cfb28652ef8a76383894930e340f290518` |
| `native-values-served.runbook.yaml` | `457ac2d5922e96807b8e4d12a2eeccff5d536bdcb4a755123c76c3d8d3776c9a` |
| `native-values.runbook.yaml` | `0c46b732766b598024714c8b43c7c094df3ecb4cbf75b8fe9618c1c6919fece9` |
| `parent-blocked.runbook.yaml` | `0c7608b3bed56571b773077e59c7394eae2eddae19a29115803c24dfc70e65b6` |
| `parent-success.runbook.yaml` | `a21117d941b31206e1b14dc0b2c5957c3033d133021d145b9a8bc2de53e6f994` |
| `runtime\internal\runstore\budget_test.go` | `8fbbbdc95e3a280b67ed98d5217d049717c0369a82307aa3cf3990dc6a08d61b` |
| `runtime\internal\runstore\dir_store_test.go` | `959efb2c54f0ba7c241b3aa539db31f61c8022a7b7bb6f789de9412605d98444` |
| `runtime\internal\serve\results_test.go` | `89437e6fba9f431e4cc7ce35927c2d94f65087c784d13a89d5c83821a99b70e2` |
| `runtime\internal\resultsdelivery\results_test.go` | `0d9b3ce428274fd387ae7a531981b02a2719407c3de811ec1f43e0c1e699efec` |
| `runtime\pkg\engine\results.go` | `b9504851a0b3768e8c69a5c5d93cc50810b4b7115b4d3dd0b9d80017c56baf3b` |
| `runtime\pkg\engine\results_test.go` | `c77e14bd800fb57d25cff5dd8f1680cdd20a1d3329f96e9e08ad1be9963666f9` |

## Cleanup and residuals

Go test cleanup hooks removed all test-owned stores, counters, listeners, and
subprocesses. The isolated candidate index was removed. No new repository
binary, store, counter, trace, or tool installation remains. A root
`yawr.exe` dated before this execution was pre-existing and was neither used,
changed, nor removed.

Residual scope:

- Windows AMD64 only;
- no caller, Runrail, release, publication, signing, or distribution evidence;
- the tested candidate is an uncommitted Git tree, not a commit.

## Native authoring

**EXTERNALLY GATED — NOT RUN — NOT PASSED**

Checkout evidence is not accepted, and no caller acceptance is claimed.

## Independent review

**Reviewer:** Tester, Conformance Tester
**Review timestamp:** `2026-09-12T08:30:11.613-07:00`
**Verdict:** **PASS**

I independently reviewed every changed source, fixture, test, and evidence file
against the frozen Y1 scope. The implementation is limited to direct canonical
Results delivery and preserving the served run context after the `run.start`
HTTP response. The integration tests enter production `runRun` and `runServe`,
parse real YAML, use real durable stores and loopback HTTP, and reproduce the
served request-context cancellation boundary. I found no compatibility shim,
caller-specific branch, duplicate Results format, fake implementation, direct
Results shortcut, unrelated source change, or caller-acceptance inference.

Independent uncached reruns:

| Command | Exit | Duration | Result |
|---|---:|---:|---|
| `go test -C runtime ./cmd/yawr -run '^TestY1RuntimeContracts(DirectPersistedResume\|OpenStdinAndServedExecution\|ChildForwardingAndBlockedNoData\|UnsupportedCapabilityHasNoSideEffects)$' -count=1 -v` | 0 | 4.591 s | Four production-boundary Y1 integration tests passed. |
| `go test -C runtime ./internal/engine ./internal/runstore ./internal/serve ./internal/resultsdelivery ./pkg/engine -count=1` | 0 | 10.625 s | All directly relevant packages passed. |
| `go test -C runtime ./internal/parser ./cmd/yawr -run '^Test(Parser_(MissingAPIVersion\|WrongAPIVersion)\|StdioResults(UnavailableAndTransportFailure\|MissingAndInvalid)\|Y1RuntimeContractsUnsupportedCapabilityHasNoSideEffects)$' -count=1 -v` | 0 | 3.742 s | Strict runbook admission plus absent, malformed, failed, cancelled, pending, and protected/unavailable Results controls passed. |
| `npm run runtime:test` | 0 | 82.329 s | Full uncached runtime suite passed; `cmd/yawr` reported 74.999 s. |

C1 rejects missing/wrong `yawr.runbook/v1` and unsupported capabilities before
runbook access or run/trace creation. C2-C5 preserve native values, canonical
typed identity, exact child forwarding, and intentional blocked no-publication
without capture or downstream dispatch. C6 keeps malformed/unavailable Results
separate from failed, denied, indeterminate, cancelled, blocked, pending, and
successful domain `no-data` outcomes; the directly relevant and full suites
execute those existing outcome controls. C7 exercises direct CLI, open-stdin
stdio, real served `run.start`/`run.next`, persisted `run.get` after restart,
and resume while producer counters remain one. C8 proves rejection before blob
or snapshot publication and byte-for-byte preservation of the prior checkpoint.

All 16 recorded SHA-256 values matched. I independently reproduced candidate
tree `32c82f5938196e0bc7a58007280eb317ad99d920` and implementation diff identity
`0a457cfdf387ea5c5dde4d049b9303e6c4e367a9`. Review scratch state was removed;
the final diff contains only the authorized Y1 runtime files and these two
evidence files. No sensitive or personal data remains in the evidence.

Native authoring remains exactly **EXTERNALLY GATED — NOT RUN — NOT PASSED**.
Checkout evidence was not accepted as a substitute.
