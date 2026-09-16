# Yawr Examples

This directory contains runbook examples that demonstrate the yawr runbook syntax and core features.

## Fundamentals

Basic concepts and minimal runbooks.

| Example | Description | Features |
|---------|-------------|----------|
| [simple-health-check](./simple-health-check/) | Basic service reachability check — ping then HTTP | Tool steps, capture, collector, end outcomes |
| [edge-cases](./edge-cases/) | Minimal runbooks for testing edge cases | Single-step runbooks, noop with delay, minimal structure |

## Branching & Decisions

Conditional execution and user-driven routing.

| Example | Description | Features |
|---------|-------------|----------|
| [service-health-branching](./service-health-branching/) | DNS + HTTP health check with nested branching | Multi-level branches, template conditions, required evidence |

## Orchestration

Runbook composition, iteration, and multi-file organization.

| Example | Description | Features |
|---------|-------------|----------|
| [collect-health](./collect-health/) | Sequential health checks with result accumulation | Iterate + include, result capture, accumulation pattern |
| [collect-health-parallel](./collect-health-parallel/) | Parallel health checks with concurrent execution | Concurrency, collect field, join function |
| [nested-chain](./nested-chain/) | Five-level deep include chain | Deep nesting, include chain, execution tracing |
| [execution-graph](./execution-graph/) | Safe VS Code execution and history verification | Eager/lazy includes, nested dynamic routing, repeated invocations, returns, failure, operator choice |
| [multi-region-rollout](./multi-region-rollout/) | Rolling health check across regions | Iterate, governance, approval gates, assert steps, evidence checklists |

## Advanced Patterns

Complex workflows with multiple levels of composition.

| Example | Description | Features |
|---------|-------------|----------|
| [incident-triage](./incident-triage/) | Multi-level incident classification and routing | Choice steps, include with gate, 3-level nesting, fan-out branching, inline vs included paths, approval workflows |

## Running Examples

All examples can be run with:

```bash
yawr run <example-folder>/<runbook-file>.runbook.yaml
```

For example:

```bash
# Simple health check
yawr run simple-health-check/simple-health-check.runbook.yaml

# Multi-region rollout with governance
yawr run multi-region-rollout/multi-region-rollout.runbook.yaml

# Complex incident triage
yawr run incident-triage/incident-triage.runbook.yaml
```

## Feature Coverage

The examples collectively demonstrate:

- **Step types**: cli, tool, include, choice, collector, approve, assert, branch, iterate, end, noop
- **Flow control**: Conditional branching, iteration, parallel execution
- **Composition**: Multi-file runbooks, nested includes, imports
- **User interaction**: Choice steps, collector fields, evidence collection
- **Governance**: Approval gates, command allowlists, redaction rules
- **Data flow**: Variable capture, template expressions, result accumulation
- **Outcomes**: Terminal states with category and code
- **Gate semantics**: stop_if conditions on includes

## Example Structure

Each example folder contains:

- One or more `.runbook.yaml` files
- `README.md` with:
  - Description and purpose
  - File listing with roles
  - How to run
  - Key concepts demonstrated

## Contributing

When adding new examples:

1. Include a folder-level `README.md`
2. Document the features used
3. Keep examples focused on specific concepts
4. Test that the runbook is valid and executable
