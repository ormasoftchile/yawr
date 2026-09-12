# Authoring

## Source of truth

Runbooks use the current `yawr.runbook/v1` YAML contract. Executable schemas live
with the runtime; consolidated GXL, GIS, and GCP grammars live under the runtime
specification tree. Runtime parsing, semantic validation, and conformance tests
are authoritative over prose examples.

Authors declare:

- runbook identity, inputs, bindings, outputs, and defaults;
- required packages and tool references;
- flow nodes, conditions, captures, retries, timeouts, and governance;
- operator interactions and terminal results.

Declarations preserve native distinctions such as null, empty string, false, and
zero. Required values are not inferred from nearby files or previous runs.

## Expressions, interpolation, and captures

Yawr separates three concerns:

- **GXL** evaluates bounded expressions used by conditions and typed values.
- **GIS** interpolates expressions into authored strings; a sole interpolation
  may preserve a native value where the surrounding contract permits it.
- **GCP** selects declared result data for capture.

An ordinary string is not automatically an expression. SQL, query text, shell
content, prose, and regular expressions are interpreted only at schema-selected
sites. Missing paths and invalid types fail according to the selected contract
rather than producing guessed text.

Pure expression operations do not consult the host timezone or ambient clock
unless the function explicitly provides that behavior. Ordering helpers preserve
ambiguity or inconsistency instead of silently choosing an item.

## Packages and tools

Tool candidates come from the explicit local catalog: built-ins, project
configuration, package maps, entrypoint requirements, declared paths, and the
project tools convention. Completion is not a machine-wide inventory.

A runbook references a tool by name and optional package, version, or path.
Planning binds that reference before execution. Per-invocation package maps can
replace selected bindings without editing the runbook.

Tool and runbook outputs cross boundaries only through declarations and captures.
Descriptions are untrusted documentation. Secret defaults never cross the
authoring protocol.

## Offline authoring helper

The finite helper provides completion, expression signature help, and an explicit
missing-required-arguments edit:

```text
yawr authoring capabilities --v3
yawr authoring complete --stdio
yawr authoring signature --stdio
yawr authoring required-arguments --stdio
```

The helper does not execute or plan a runbook, contact providers, initialize
authentication, or discover live remote inventories. Requests include the
document and dirty overlays, use UTF-16 positions, and are rejected when their
shape, identity, bounds, or source mapping cannot be proven.

Completion changes only the selected syntax. It does not populate credentials,
default values, transport settings, unrelated arguments, captures, or outputs.
Required-argument insertion is a separate explicit and undoable editor action.

## Authoring discipline

- Prefer declared contracts over convention or ambient state.
- Keep side effects in named effectful steps.
- Initialize iteration-local values so a skipped branch cannot reuse a previous
  iteration's observation.
- Treat `blocked`, `no-data`, `failed`, and unavailable as meaningful outcomes.
- Validate with the matching runtime helper and execute against synthetic or
  controlled tools before using a new runbook on real systems.
