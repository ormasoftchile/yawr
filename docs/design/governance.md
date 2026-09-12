# Governance

Governance is evaluated by the runtime at execution boundaries. UI affordances
may collect a decision, but they do not replace runtime enforcement.

## Policy evaluation

For command-bearing steps, command denial takes precedence over command
allowance. Environment variables are filtered before dispatch. Redaction
patterns scrub captured process output before it enters ordinary presentation
or trace paths. Steps and tools can require approval through the configured
approval gate.

These controls have different meanings:

- **Allow and deny rules** decide whether a command may be dispatched.
- **Environment rules** remove prohibited variables from the child environment.
- **Redaction** limits disclosure; it does not make an unsafe command safe.
- **Approval** records an explicit operator decision; it does not repair an
  invalid plan or authorize an undeclared capability.

Governance evidence records the evaluated step, matched rules, blocked variable
names, and outcome. It must not reproduce protected values.

## Profiles

A runtime profile declares execution context, attended or unattended operation,
approval scope, test transport affordances, and permitted per-tool parameters.
Context and attendance are independent: an editor-hosted process is not
automatically attended.

Profiles apply after package and tool selection. They cannot replace the selected
package, change a tool transport, or widen a tool's allowed hosts. A profile
attempting transport-mode rewriting is rejected.

## Secrets and protected values

Secret inputs use private configuration channels rather than command-line
arguments or public run frames. Sensitive values are omitted or redacted from
authoring metadata, graph details, diagnostics, traces, and debug snapshots.
Defaults for sensitive tool arguments are reported only as declared or absent,
never as values.

Protection follows dependencies where the runtime can prove them. If a dynamic
debug boundary makes the dependency set unknowable, Yawr restricts editing
rather than exposing a possibly protected value.

## Host and tool authority

A runbook invokes declared tools and logical host capabilities. Tool contracts,
package bindings, runtime profiles, and host registries each contribute a
different part of authority. None is a substitute for the others.

The trusted host maps a logical capability to a statically allowlisted operation
and validates its payload. External providers remain responsible for validating
their own target resources and authorization.

## Claims and verification

Yawr provides enforceable controls and evidence primitives; it does not claim
that using them certifies an organization or workload against a regulatory or
security standard. Such claims require system-specific configuration, operation,
and independent assessment.
