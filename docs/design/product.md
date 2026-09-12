# Product

Yawr is a workflow and runbook runtime. It turns reviewed YAML procedures into
validated execution plans, runs those plans through explicit tool and operator
boundaries, and retains enough durable state to explain or resume a run.

## Product contract

Yawr treats an operational procedure as three related contracts:

1. **Authoring contract** — the runbook declares inputs, flow, tool references,
   captures, outputs, governance, and interaction points.
2. **Execution contract** — the runtime validates and plans before dispatching
   effects, then advances the plan with explicit status and failure semantics.
3. **Evidence contract** — execution produces ordered events, checkpoints, and
   bounded projections without treating a UI rendering as the source of truth.

The runtime is authoritative for parsing, validation, planning, governance,
execution, and persisted state. The CLI, server transports, and VS Code
extension are clients of those capabilities; they do not independently decide
whether a runbook or result is valid.

## Design priorities

- **Fail closed at trust boundaries.** Invalid schemas, unresolved bindings,
  unsupported capabilities, ambiguous resume ownership, and malformed
  interactions are errors rather than guesses.
- **Make effects explicit.** Tool calls, command execution, host actions,
  approvals, and operator input are represented as named steps with observable
  outcomes.
- **Separate contracts from transports.** A tool or runbook contract remains
  meaningful whether invoked from the CLI, stdio, the server, or an editor.
- **Preserve domain truth.** Empty, false, zero, null, blocked, denied, failed,
  and unavailable are distinct states. Completion alone does not prove a
  successful domain result.
- **Bound every projection.** Interactive views, protocol frames, expression
  evaluation, authoring requests, and persisted state have explicit limits.
- **Prefer executable evidence.** Current schemas, protocol specifications,
  tests, and source behavior take precedence over descriptive claims.

## Product boundaries

Yawr is not a general web platform, tenant portal, identity provider, scheduler,
or infrastructure control plane. It does not infer authorization from the
presence of a UI, turn provider output into verified evidence without a declared
contract, or silently adapt old schemas and protocols.

External systems remain responsible for their own authentication, authorization,
availability, and domain validation. Yawr governs how a runbook requests those
systems and how their declared results enter workflow state.
