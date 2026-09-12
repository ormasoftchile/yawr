# Architecture

Yawr keeps workflow semantics in the runtime and exposes them through bounded
clients and transports.

## Runtime layers

### Admission

The parser loads the current runbook shape and rejects malformed or structurally
invalid documents. Semantic validation checks relationships that a JSON Schema
cannot prove alone. Package and tool catalogs resolve declared references; the
planner then produces the execution structure used by the engine.

Planning is a trust boundary. Runtime values cannot turn an include into an
arbitrary filesystem path. Dynamic includes resolve identities from the approved
catalog, while static and deferred includes retain source identity needed for
durable execution.

### Execution

The engine owns control flow, step status, retries, captures, interactions,
governance checks, checkpointing, and terminal outcomes. Executors implement
specific effects such as commands, tools, includes, or operator interactions.
They return results to the engine rather than mutating presentation state.

Run and session stores retain durable state. Trace and evidence components record
execution facts. A single active writer owns a durable run; lease and epoch
checks prevent a second process from bypassing that ownership.

### Integration

The CLI is the direct user and automation surface. `yawr run --stdio` uses a
JSON-lines protocol while preserving the same parser, planner, catalog, bindings,
and engine as direct execution. Serve mode exposes JSON-RPC and interaction
endpoints for longer-lived clients.

Tool definitions and package bindings describe external execution. Runtime
profiles parameterize the already selected tools; they do not replace package
resolution or rewrite a tool's transport.

Host actions cross a narrower boundary. The runtime requests a logical
capability, and a trusted host maps that capability to an allowlisted operation.
A runbook cannot select an arbitrary editor command.

### Presentation

Preview and authoring models are projections of runtime-owned contracts. The VS
Code extension invokes the matching helper, renders graph and run state, and
submits explicit operator actions. It does not carry a competing runbook schema
or validation engine.

## Contract rules

- Boundary messages have explicit versions and closed shapes where the current
  protocol requires them.
- Clients negotiate required capabilities and refuse unsupported versions
  before execution.
- Identifiers, content hashes, plan digests, invocation paths, and turn IDs are
  preserved across boundaries instead of rewritten for display convenience.
- Unknown or malformed data is handled according to the specific current
  contract; there is no repository-wide promise of backward compatibility.
- Schemas and detailed protocol specifications remain closer to the executable
  authority than this overview.

## Dependency direction

Presentation depends on runtime projections. Transports depend on public runtime
contracts. Executors depend on narrow engine and integration interfaces. Core
workflow decisions do not depend on a particular editor, web framework, cloud,
or customer topology.
