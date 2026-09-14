# Yawr — Yet Another Workflow Runtime

[![Go](https://img.shields.io/badge/Go-1.25.7-blue.svg)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

Yawr is a Go-based workflow and runbook runtime with governance enforcement,
evidence capture, and pluggable tool integrations.

## What it is

- **Executable runbooks** — define multi-step operational procedures in YAML; execute them with full step tracking and traceability
- **Governance** — command allowlists/denylists, env var blocking, output redaction, per-step approval gates
- **Traceability** — append-only JSONL trace files, per-step state snapshots, run resumption after interruption
- **Pluggable tools** — tool definitions (`.tool.yaml`) and input providers for external systems
- **Evidence capture** — text, checklists, attachments with SHA256 hashing
- **Serve mode** — JSON-RPC 2.0 server for editor and automation integrations
- **Runbook debugging** — graph breakpoints, stepping, watches, result/variable overrides, and reusable root-scoped debug profiles

## Build

```bash
go build ./...
```

Build the CLI binary:

```bash
go build -o yawr ./cmd/yawr
```

## Test

```bash
go test ./... -race -count=1
```

## File-only subprocesses

Windows AMD64 runtimes with a working AppContainer backend advertise
`yawr.file-only-subprocess/v1` and accept the distinct
`native-file-only` tool transport. It is digest-pinned, package-relative,
network-denied, single-process, environment-scrubbed, and fail-closed. See
[`docs/native-file-only-subprocess.md`](docs/native-file-only-subprocess.md).

## CLI

Terminal `end` steps can opt in to durable named Results with
`publish_results: true`, preserving the exact outcome category/code through
nested branches and includes. See [typed Results](docs/typed-results.md#terminal-outcomes-with-results)
for declaration scope, recovery, and the `yawr.terminal-results/v1` capability.

```
yawr run <runbook.yaml>         Execute a runbook
yawr run --stdio <runbook.yaml> Execute with JSON-lines events and interactions
yawr dry-run <runbook.yaml>     Dry-run without side effects
yawr ls                         List recent runs
yawr gc                         Garbage-collect old run records
yawr serve                      Start JSON-RPC server
yawr version                    Print version
```

### Tool packages

`yawr run`/`yawr dry-run` load the project's package bindings from
`.yawr/config.yaml` and resolve every
`requires:`/`toolRefs:` entry in the runbook against them before planning.
Pass `--package-map <file>` to override individual package bindings for a
single invocation — the file uses the same `yawr.config/v1` shape
(`requires:`/`tool-paths:`) as the project config; packages it mentions win
over the project config, and every other project binding is preserved
unchanged. This makes it possible to run the exact same, unedited runbook
against real project tools normally and mock/test tools via
`--package-map`, e.g.:

```
yawr run incident.runbook.yaml
yawr run incident.runbook.yaml --package-map testdata/package-map.mock.yaml
```

Resolution failures (missing package, version mismatch, path/collision
errors) are reported with stable `PKG-*` codes.

### Editor stdio transport

`yawr run --stdio` runs through the same parser, planner, package catalog, tool
bindings, and engine as a normal CLI run, but reserves stdin/stdout for the
`yawr.stdio/v1` JSON-lines protocol. It carries live engine events, interactive
prompts, host actions, answers, cancellation, and terminal status without an HTTP
server or SSE connection. See [`specs/runbook-stdio-v1.md`](specs/runbook-stdio-v1.md).

### Debugging runbooks

The served React Flow preview supports explicit **Debug Run** sessions. Select a
step, add a before or after breakpoint, then inspect variables and the immutable
actual result. An after breakpoint can supply an effective output or status before
captures and downstream conditions are evaluated. Applied overrides remain visible
as amber `DBG` markers with actual and effective values.

Step Into/Over/Out use exact nested invocation paths, so stepping into one include
call does not stop in another call to the same child runbook. Read-only GXL watches
are evaluated at each pause. Reusable profiles are stored under
`.yawr/debug-profiles/*.yaml`. Profiles are bound to one root runbook and plan
hash, and are never discovered or applied by a normal run.

## Documentation

| Topic | File |
|---|---|
| Contract-identical bindings (native / mcp-stdio / mcp-http) | [`docs/contract-identical-bindings.md`](docs/contract-identical-bindings.md) |
| Typed collections, nested exports, and explicit date ordering | [`docs/structured-values.md`](docs/structured-values.md) |
| Typed bindings, named durable Results, v3 negotiation and bounded public retrieval (candidate) | [`docs/typed-results.md`](docs/typed-results.md) |
| Runbook debugger and profile contract | [`specs/runbook-debugging-v1.md`](specs/runbook-debugging-v1.md) |
| Editor stdio run protocol | [`specs/runbook-stdio-v1.md`](specs/runbook-stdio-v1.md) |

## Runtime contracts

Executable schemas live in [`schemas`](schemas). Consolidated expression
grammars live under [`spec`](spec); executable runtime behavior and tests remain
authoritative.
