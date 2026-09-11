# Tool defaults and internal helper bindings

## Defaults at the action boundary

Declare optional defaults on the **action**, and normally repeat them on the
backing runbook for a consistent standalone contract:

```yaml
# tools/observations.tool.yaml
apiVersion: yawr.tool/v1
meta: {name: observations, version: "1.0.0"}
transport: {mode: native, command: must-not-dispatch}
actions:
  - name: run
    args:
      query_hash: {type: string, required: false, default: ""}
      environment_verified: {type: boolean, required: false, default: false}
    outputs:
      result: {type: object}
    execute: {kind: runbook, path: observations.runbook.yaml}
```

```yaml
# tools/observations.runbook.yaml
apiVersion: yawr.runbook/v1
id: observations
name: Observation defaults
inputs:
  query_hash: {type: string, required: false, default: ""}
  environment_verified: {type: boolean, required: false, default: false}
outputs:
  result: {type: object, value_expr: vars}
flow:
  - step:
      id: verify_defaults
      type: assert
      assert:
        - type: eq
          subject: '${query_hash == "" and environment_verified == false}'
          expected: "true"
```

```yaml
# caller.runbook.yaml
apiVersion: yawr.runbook/v1
id: caller
name: Omitted defaults
toolRefs: [{name: observations}]
flow:
  - step:
      id: call
      type: tool
      tool: {name: observations, action: run, args: {}}
      capture: {result: outputs.result}
```

From the directory containing `caller.runbook.yaml` and `tools`, run:

```powershell
yawr run .\caller.runbook.yaml
```

Effective argument precedence:

1. A supplied argument wins, including `""`, `false`, and zero.
2. An omitted argument receives the action's non-null declared default.
3. Without an action default, the argument stays absent. A backing input
   default is **not** an additional fallback during substitution.

The action contract is the public boundary: differing backing defaults do
not override it or implicitly widen it. Defaults are literal native values;
they are not templates evaluated in caller scope. An explicit null does not
request defaulting: it is validated as a supplied value. Substitution rejects
missing required action arguments or backing inputs before running the body.
It validates declared types and enums; boolean strings such as `"false"` are
not native `false` and are not silently converted. Native process-backed
actions also receive their declared defaults before argument enum validation
and dispatch; their existing transport adaptation rules otherwise remain unchanged.

Substitutions receive only effective arguments, not the caller's ambient
variables. This holds for nested substitutions and frozen/saved plans.
HTTP root `inputs` string bindings are a different interface; this behavior
does not change root input conversion.

## Missing values: GIS is not GXL

GXL conditions and `value_expr` lookups are strict. Do **not** use
`vars?.query_hash == null` as a missing-field guard. Flat `query_hash` and
`vars.query_hash` both work when the binding exists; neither spelling makes
an absent binding valid.

GIS interpolation supports optional GDP reads. For optional *text* with no
default, a valid normalization is:

```yaml
- step:
    id: normalize_optional_text
    type: noop
    capture:
      hash_text: '${vars?.query_hash}'
# Later conditions can compare the present hash_text to "".
```

This is not general input validation or a safe way to coerce arbitrary text
into a trusted boolean. Prefer declared typed defaults or explicit typed args.

## Project-scanned, unexported helper tools

A package may export only its public tool while project scanning makes an
internal helper available by its unique bare name:

```yaml
# package-map.yaml (paths relative to workspace root)
apiVersion: yawr.config/v1
requires:
  - {package: example.observations, version: "^1.0.0", path: packages/observations}
tool-paths:
  - packages/observations
```

```yaml
# packages/observations/yawr-package.yaml
apiVersion: yawr.tool-package/v1
meta: {name: example.observations, version: "1.0.0"}
exports:
  tools:
    - {id: observations, path: observations.tool.yaml}
# helpers.tool.yaml is project-scanned, not exported.
```

Each runbook that invokes the helper must independently declare:

```yaml
toolRefs:
  - {name: observations-internal}
```

The helper file uses an ordinary `yawr.tool/v1` definition with
`meta.name: observations-internal`, declared actions/outputs, and a valid
implementation. Capture its typed result only at its call site, e.g.
`capture: {helper_result: outputs.result}`.

Run or serve with the same root-owned bindings:

```powershell
yawr run .\caller.runbook.yaml --package-map .\package-map.yaml
yawr serve --addr 127.0.0.1:7778 --package-map .\package-map.yaml
```

Do not add a public export to cure a repeated-run failure. A package-qualified
reference to an unexported helper is invalid; bare-name resolution still
requires a unique eligible catalog entry, and ambiguity/shadowing checks
remain enforced. Lexical `toolRefs` are not inherited. "Unexported" is an API
packaging boundary, not an authorization boundary.

Every plan owns independent tool definitions and its complete transitive
helper-action closure, including already-frozen substitutions. Repeated and
concurrent served requests must not mutate the startup registry or depend on
which request ran first. Unrelated registry tools are not added as a workaround.
