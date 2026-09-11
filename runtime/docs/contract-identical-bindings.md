# Contract-Identical Bindings Across Transports

**Audience:** Consumer repository authors (e.g., SQL Live-Site Operations) who need to run
the same logical tool action via a native mock, an MCP stdio subprocess, or an MCP HTTP
endpoint — verified against the same governance contract throughout.

**Version:** 2026-08-17

---

## 1. What "Contract-Identical" Means

Two tool definitions are contract-identical when they:

1. Carry **the same logical action name** (the `name:` within `actions:`)
2. Declare **identical output structure** — same key names and types under `returns:` or
   `outputs:` on each action
3. Carry **the same `classification`** on each action (`read-only`, `mutating`,
   `destructive`, or absent/`nil` for unspecified)
4. Carry **the same governance semantics** — `requires-approval:`, `allowed-environments:`,
   and `allowed-modes:` values must agree across all bindings for the same logical tool

**What may differ:** transport wiring (`transport.mode`, `transport.url`, `transport.command`,
`transport.auth`), the physical endpoint or binary, auth provider, and allowed hosts.

**What must never differ:** action names, output keys and types, classification, and
governance policy.

> **Why this matters for Yawr:** Yawr's approval gate, classification-routing (retry,
> timeout, INDETERMINATE halt), and tracing operate on the resolved tool definition.
> If a mock binding omits `classification: read-only` that the production binding carries,
> a test run silently produces different approval and retry behavior — the mock
> does not prove what the production run would do.

---

## 2. The Three Transport Modes

### 2a. Native (in-process / deterministic mock)

A `native` tool invokes a local binary directly.  It is the correct transport for
deterministic mocks in test profiles.  No subprocess MCP protocol, no network, no auth.

```yaml
# icm-mock.tool.yaml  — native mock
apiVersion: yawr.tool/v1
name: icm
version: "1.0"
description: Deterministic mock for IcM incident management (test use only)

transport:
  mode: native
  command: icm-mock            # a test binary or script on PATH

governance:
  allowed-environments:
    - test                     # restricts this definition to test-context profiles

actions:
  - name: get_incident
    description: Return a canned incident record (deterministic)
    classification: read-only
    args:
      incident_id:
        type: string
        required: true
        description: IcM incident ID
    returns: json

  - name: update_incident
    description: Silently accept a mitigation note (no side effects)
    classification: mutating
    args:
      incident_id: {type: string, required: true, description: IcM incident ID}
      note:        {type: string, required: true, description: Mitigation note}
      status:      {type: string, required: false, description: Optional status}
    returns: json

  - name: resolve_incident
    description: Accept a resolution (no side effects)
    classification: destructive
    args:
      incident_id: {type: string, required: true, description: IcM incident ID}
      summary:     {type: string, required: true, description: Resolution summary}
    returns: json
```

#### Native yawr.query-result/v1 output contract

Native actions remain plain stdout/stderr by default. A query-oriented native
action may opt in to a narrow structured result parser by declaring `result:` on
that action:

```yaml
actions:
  - name: query
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
      metadata: metadata   # optional
```

For opted-in actions, Yawr parses successful stdout as exactly one JSON object
and exposes typed `outputs.success`, `outputs.row_count`, `outputs.columns`,
`outputs.rows`, and optional `outputs.metadata`. The process diagnostics
`stdout`, `stderr`, and `exit_code` remain available. Unknown formats/sources or
invalid mappings are rejected at tool parse time; malformed JSON or missing/wrong
runtime fields fail the tool step rather than inventing values.

### 2b. MCP stdio (`mode: mcp`)

A `mcp` tool spawns a local subprocess that speaks the MCP stdio protocol
(version 2024-11-05).  `command:` is required; `url:` and `auth:` are invalid on this mode.

```yaml
# icm-stdio.tool.yaml  — MCP stdio binding
apiVersion: yawr.tool/v1
name: icm
version: "1.0"
description: IcM incident management via local MCP stdio subprocess

transport:
  mode: mcp
  command: icm-mcp              # binary that speaks MCP stdio

governance:
  allowed-environments:
    - cli-operator
    - vscode-operator

actions:
  - name: get_incident
    description: Retrieve the full record for an IcM incident by ID.
    classification: read-only
    args:
      incident_id:
        type: string
        required: true
        description: IcM incident ID (e.g. "12345678")
    returns: json

  - name: update_incident
    description: >
      Append a mitigation note or status update to an open IcM incident.
      Does not close the incident; use resolve_incident for that.
    classification: mutating
    args:
      incident_id: {type: string, required: true,  description: IcM incident ID}
      note:        {type: string, required: true,  description: Free-text mitigation note}
      status:      {type: string, required: false, description: Optional status transition}
    returns: json

  - name: resolve_incident
    description: Mark an IcM incident as resolved with a resolution summary.
    classification: destructive
    args:
      incident_id: {type: string, required: true, description: IcM incident ID}
      summary:     {type: string, required: true, description: Resolution summary}
    returns: json
```

### 2c. MCP HTTP (`mode: mcp-http`)

An `mcp-http` tool connects to a remote MCP server over HTTPS (protocol version 2025-03-26).
`url:` is required and **must use `https://`** — plain HTTP is rejected at validation time
with no override.  `command:` and `args:` are invalid on this mode.

`auth:` is optional; when present, `allowed_hosts:` is required (MCP-010).

```yaml
# icm.tool.yaml  — MCP HTTP production binding (matches tools/icm.tool.yaml in this repo)
apiVersion: yawr.tool/v1
name: icm
version: "1.0"
description: >
  IcM (Incident Management) tool via Azure's MCP HTTP endpoint.
  Provides incident lookup, update, and on-call resolution actions.

transport:
  mode: mcp-http
  url: https://icm-mcp-prod.azure-api.net/v1/
  auth:
    provider: azure-cli
    scope: api://icmmcpapi-prod/mcp.tools
    allowed_hosts:
      - icm-mcp-prod.azure-api.net

governance:
  allowed-environments:
    - cli-operator
    - headless-server

actions:
  - name: get_incident
    description: Retrieve the full record for an IcM incident by ID.
    classification: read-only
    args:
      incident_id:
        type: string
        required: true
        description: IcM incident ID (e.g. "12345678")
    returns: json

  - name: update_incident
    description: >
      Append a mitigation note or status update to an open IcM incident.
      Does not close the incident; use resolve_incident for that.
    classification: mutating
    args:
      incident_id: {type: string, required: true,  description: IcM incident ID}
      note:        {type: string, required: true,  description: Free-text mitigation note}
      status:      {type: string, required: false, description: Optional status transition}
    returns: json

  - name: resolve_incident
    description: Mark an IcM incident as resolved with a resolution summary.
    classification: destructive
    args:
      incident_id: {type: string, required: true, description: IcM incident ID}
      summary:     {type: string, required: true, description: Resolution summary}
    returns: json
```

---

## 3. Selecting a Binding with `--package-map`

`--package-map` is the **only sanctioned mechanism** for switching which transport binding
Yawr uses for a given tool name.  It tells the planner which package directory to resolve
each `toolRef:` against, before any profile is consulted.

### How it works

1. Your runbook declares a `toolRef` — it names the logical tool (`icm`), not a transport.
2. The project config (`.yawr/config.yaml`) maps `icm` to a default package.
3. `--package-map mymap.yaml` overrides that mapping for any packages it names;
   all others fall through to the project config unchanged.
4. After resolution, the profile (via `--profile`) applies runtime parameters to the
   resolved definition.  The profile **never re-resolves** which definition was selected.

```yaml
# testdata/package-map.mock.yaml  — override icm to the native mock package
apiVersion: yawr.config/v1
requires:
  - name: icm-tools
    path: ./testdata/icm-mock-pkg   # contains icm-mock.tool.yaml
```

```yaml
# .yawr/config.yaml  — project default: production mcp-http binding
apiVersion: yawr.config/v1
requires:
  - name: icm-tools
    path: ./tools/icm-production-pkg  # contains icm.tool.yaml (mcp-http)
```

Running without a package-map uses the production binding:

```sh
yawr run incident-tsg.runbook.yaml --profile profiles/cli-prod.profile.yaml
```

Running with the mock package-map uses the native mock:

```sh
yawr run incident-tsg.runbook.yaml \
  --package-map testdata/package-map.mock.yaml \
  --profile profiles/test.profile.yaml
```

The **same runbook YAML is used in both cases** — this is intentional.  The runbook is
transport-agnostic; the package-map and profile supply the environmental wiring.

### Why `--package-map` and not a profile setting

A profile parameterizes execution *after* tool selection: it carries auth provider,
endpoint overrides, attendance, and approval scope.  Changing which tool definition is
selected is a different decision — it changes the tool's declared transport mode, its
governance block, and potentially its schema.  These decisions happen at plan time, not
at execution time.  Putting transport selection in a profile would make the resolved tool
set dependent on both the profile and the catalog simultaneously, with no clear precedence.

`--package-map` answers "which definition?"; the profile answers "how do you invoke it?".
They compose and never conflict.

---

## 4. What a Profile Is (and Is Not) Allowed to Change

A runtime profile **parameterizes** an already-resolved tool definition.  It never selects
or rewrites the definition.

### What a profile may carry

| Field | Where in profile | Effect |
|---|---|---|
| `context` | top-level | Preflight context check against `allowed-environments:` |
| `attendance` | top-level | Selects approval gate behavior |
| `approval.scope` | top-level | Which classifications are permitted without an explicit gate |
| `tools.<name>.provider` | per-tool | Overrides auth provider (see Rule A below) |
| `tools.<name>.endpoint` | per-tool | Overrides connection endpoint (PLAN-013 applies) |

### What a profile must NOT contain

A profile **must not set a transport mode** on any tool override.  Attempting to do so
produces error PROF-001 and is rejected at profile parse time:

```
error: runtime profile "staging": tool "icm" sets transport mode "mcp-http";
  profiles must not rewrite transport modes (PROF-001)
```

This means: if your production actions are `mode: mcp` (stdio), you cannot produce a
contract-identical `mcp-http` binding by adding a profile setting.  You must author a
separate tool definition with `mode: mcp-http` and select it via `--package-map`.

> **For SQL Live-Site Operations:** Your production actions are `mcp` (stdio).  To add
> an `mcp-http` binding, create a separate `icm-http.tool.yaml` (or a new package) with
> `mode: mcp-http` and the same action names, classifications, and output keys.  Use a
> `--package-map` that points the `icm` toolRef at that package when running in contexts
> that should use the HTTP binding.  The profile then supplies auth and endpoint
> parameters for that binding — it does not create the binding.

---

## 5. Rule A — The Auth Provider Override Rule

**Rule A (ratified):** A runtime profile MAY substitute the auth `provider`.  It MUST NOT
supply or override `scope` or `allowed_hosts`.

### In practice

A profile `tools.icm` block may look like this:

```yaml
tools:
  icm:
    provider: managed-identity    # override: staging uses managed identity instead of azure-cli
    endpoint: https://icm-mcp-staging.azure-api.net/v1/
```

It may **not** look like this:

```yaml
tools:
  icm:
    provider: managed-identity
    scope: api://icmmcpapi-staging/mcp.tools    # ILLEGAL — scope comes from the tool definition
    allowed_hosts:                               # ILLEGAL — allowed_hosts comes from the tool definition
      - icm-mcp-staging.azure-api.net
```

The `ProfileToolOverride` struct in `pkg/schema/profile.go` enforces this structurally:
it has `Provider` and `Endpoint` fields and no `Scope` or `AllowedHosts` fields.  There is
no runtime assertion needed — the illegal fields simply cannot be set.

### Why

`provider` answers *who acquires the token* — the human at a terminal, a managed identity,
a workload identity.  This is legitimately context-dependent: the same tool might be
invoked by a developer (`azure-cli`) in one profile and a CI system (`managed-identity`)
in another.

`scope` and `allowed_hosts` answer *what the token is scoped for* and *where it may be
sent*.  These are properties of the **tool's contract** with its backing service.  The
scope `api://icmmcpapi-prod/mcp.tools` is a fixed resource identifier that the IcM service
requires — no deployment context changes it.  Allowing a profile to substitute a different
scope would let an operator silently change the resource the token grants access to.

`allowed_hosts` is a security control (see Section 6).  A profile that could rewrite it
could expand which hosts receive the bearer token, defeating the replay protection
`allowed_hosts` provides.

**In one sentence:** a profile changes *who acquires* the token, never *what it is for*
or *where it may go*.

---

## 6. `allowed_hosts` and Endpoint Override Safety

### What `allowed_hosts` protects against

Audience-scoped tokens (e.g., `api://icmmcpapi-prod/mcp.tools`) prevent a malicious server
from consuming the token at its own service — wrong audience is rejected.  They do NOT
prevent a malicious server from forwarding the token to the real IcM endpoint, because
possession is authorization.

`allowed_hosts` is the second line of defense.  Before the HTTP transport attaches an
`Authorization` header, `TokenGate.AttachToken` checks `req.URL.Hostname()` against the
list.  If the hostname is not listed, the request fails with MCP-012 and the token is
never sent.

Matching is **exact, case-insensitive, on the parsed hostname with port stripped**.
Wildcards are not supported — `*.azure-api.net` does not match `icm-mcp-prod.azure-api.net`
and will never match any request.  List each permitted host explicitly.

### Profile endpoint overrides and PLAN-013

When a profile overrides a tool's endpoint, the override host **must be present in the
tool definition's `auth.allowed_hosts`**.  This check (PLAN-013) runs at **planning time**
— it is a Tier 0 static check that fires during `yawr plan` and at the start of
`yawr run`, before any execution step begins.

**Confirmed by implementation** (`internal/planner/preflight.go`,
`checkToolEnvironmentPreflight`): the check parses the override URL, extracts the
hostname, and compares it case-insensitively against each entry in
`def.Transport.Auth.AllowedHosts`.  If no match is found, planning fails with PLAN-013
and execution never starts.

An endpoint resolving to a host **not in `allowed_hosts` fails during PLANNING (Tier 0),
not mid-execution**.

This means the only way to override an endpoint to a new host is to:

1. Add that host to `allowed_hosts` in the tool definition (requires updating the
   definition, not just the profile).
2. Commit the change — there is no runtime bypass.

This is intentional: the security boundary is on the tool definition, which lives in
source control and goes through code review.

---

## 7. Classification and Its Consequences

### The four values

| Value | Meaning | Source |
|---|---|---|
| `read-only` | Action has no side effects; safe to retry | Explicit declaration in tool definition |
| `mutating` | Action changes state; cannot be retried automatically | Explicit declaration |
| `destructive` | Action destroys or irrevocably changes state; gate always fires | Explicit declaration |
| absent (nil) | `unspecified` — conservative treatment applies | No governance block or no `classification:` key |

`classification` is a **per-action** field on `ToolAction`.  A single tool legitimately has
`read-only` and `destructive` actions.  Governance does not bucket the whole tool.

### Approval-gate behavior by classification

| `requires-approval` | `classification` | Gate behavior |
|---|---|---|
| `true` (explicit) | any | Gate fires — explicit legacy opt-in wins |
| `false` (explicit) | `read-only` | No gate |
| `false` / absent | `mutating` | Gate fires in attended contexts; denied in unattended |
| `false` / absent | `destructive` | Gate **always** fires |
| `false` / absent | nil (unspecified) | Denied in unattended; warn+proceed in attended (interactive operator present) |

### Retry and late-result behavior by classification

| Classification | On timeout / lost result | Runbook behavior |
|---|---|---|
| `read-only` | Discard; log warning | Continue (or retry if `idempotent: true`) |
| `mutating` | Record as INDETERMINATE | **Halt.** State is unknown. Operator must verify before resuming. |
| `destructive` | Record as INDETERMINATE | **Halt.** State is unknown. Operator must verify before resuming. |
| unspecified | Conservative: INDETERMINATE + halt | Same as mutating |

### `requires-approval: false` is an approval-routing concept only

`requires-approval: false` tells the approval gate to auto-approve this action.  It is an
explicit legacy opt-out from approval prompting.

**It never implies, coerces, or substitutes for `classification: read-only`.**  These two
fields are completely orthogonal.  An action can be:

- `requires-approval: false` + `classification: mutating` — approved automatically, but
  still subject to INDETERMINATE halt semantics on timeout.
- `requires-approval: true` + `classification: read-only` — prompts for approval, but safe
  to retry.

Reading `requires-approval: false` as "this action is safe" is incorrect.  The approval
field governs the interactive prompt; the classification field governs retry, timeout, and
late-result behavior.  **A consumer that omits `classification:` from an action and sets
`requires-approval: false` has suppressed the approval prompt but has NOT declared the
action read-only.  The action is `unspecified` and receives conservative (halt-on-timeout)
treatment.**

This distinction was formally negotiated with SQL Live-Site Operations and is non-negotiable.

---

## 8. Test-Context Rules

In `context: test` profiles, transport rules are absolute:

| Transport | Allowed? | Override? |
|---|---|---|
| `native` | Always | — |
| `mcp` (stdio) | Blocked by default | `transport.allow_subprocess_in_test: true` in profile |
| `mcp-http` | Never | No override exists |

**Confirmed by implementation** (`internal/planner/preflight.go`, PLAN-012): `mcp-http`
is unconditionally blocked in test context.  An `mcp` binding requires an explicit
profile opt-in, which is an auditable author assertion — the subprocess is **not sandboxed**
and inherits the full parent environment.

The PLAN-012 error for `mcp-http` in test context:

```
error: tool "icm": transport mode "mcp-http" is never allowed in test context
  use transport: native with a runbook-backed mock for deterministic testing
  no override exists: mcp-http is unconditionally blocked in context: test
```

---

## 9. Authoring Checklist — Adding a Contract-Identical Binding

Use this checklist to add a new binding for an existing logical tool and verify that it
is contract-identical to the existing binding(s).

### Step 1 — Author the new definition

Create a new `.tool.yaml` in your package directory:

- [ ] `apiVersion: yawr.tool/v1`
- [ ] Same logical `name:` as the existing binding (e.g., `icm`)
- [ ] Same `version:` (contract version, not transport version)
- [ ] Set the appropriate `transport.mode:` (`native`, `mcp`, or `mcp-http`)
- [ ] For `mcp-http`: set `url:` (must be `https://`) and `auth.allowed_hosts:`
- [ ] For `mcp`: set `command:`
- [ ] Set `governance.allowed-environments:` to restrict to the contexts this binding is
      valid for (prevents accidental use in wrong contexts via PLAN-010)

### Step 2 — Verify contract identity

For each action in the new definition, confirm:

- [ ] Action name matches exactly (same string, no aliases)
- [ ] `returns:` type matches (`json`, `text`, etc.)
- [ ] All arg names and types match
- [ ] `classification:` matches on every action — `read-only`, `mutating`, `destructive`,
      or absent on all bindings identically
- [ ] `requires-approval:` is consistent (or absent on all bindings identically)
- [ ] `idempotent:` is consistent (or absent on all bindings identically)

### Step 3 — Create a package-map for the new binding

```yaml
# package-map.<context>.yaml
apiVersion: yawr.config/v1
requires:
  - name: icm-tools
    path: ./path/to/new-binding-package
```

### Step 4 — Create or select a profile for the new context

```yaml
# profiles/<context>.profile.yaml
apiVersion: yawr.runtime-profile/v1
id: <context>
context: cli-operator    # or: test, ci, headless-server, vscode-operator
attendance: unattended   # or: attended
approval:
  scope:
    allow_read: true
    allow_mutating: false
    allow_destructive: false
tools:
  icm:
    provider: managed-identity   # optional: override auth provider (Rule A)
    endpoint: https://icm-mcp-staging.azure-api.net/v1/   # optional: override endpoint
```

If you set `endpoint:`, the host in that URL must already be in `allowed_hosts` in the
tool definition (PLAN-013 enforced at plan time).

### Step 5 — Validate with `yawr plan`

```sh
yawr plan --profile profiles/<context>.profile.yaml \
          --package-map package-map.<context>.yaml \
          incident-tsg.runbook.yaml
```

A clean `yawr plan` output with exit code 0 means:

- Every toolRef resolves in the selected package
- The resolved tool's `allowed-environments:` includes the profile's context
- Auth config is structurally complete
- Any endpoint override host is in `allowed_hosts` (PLAN-013)

### Step 6 — Verify at runtime

```sh
yawr run --profile profiles/<context>.profile.yaml \
         --package-map package-map.<context>.yaml \
         incident-tsg.runbook.yaml
```

Inspect the trace file: look for `package/resolved` events that confirm the expected
binding was selected, and `mcp/authAttached` events (for `mcp-http` bindings) confirming
the correct `url_host` and `scope`.

---

## 10. Full Profile Example (CLI Operator, Production mcp-http)

This is the profile shape for an attended human operator running against the production
mcp-http IcM endpoint.  It is the counterpart to the `mcp-http` tool definition in
Section 2c.

```yaml
apiVersion: yawr.runtime-profile/v1
id: cli-prod
context: cli-operator
attendance: attended
approval:
  scope:
    allow_read: true
    allow_mutating: true
    allow_destructive: false    # destructive actions always prompt regardless
# No tools: override needed — the tool definition's default azure-cli provider
# and production endpoint are used as-is.
```

For a staging environment where managed identity is used instead of azure-cli:

```yaml
apiVersion: yawr.runtime-profile/v1
id: ci-staging
context: ci
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: false
    allow_destructive: false
tools:
  icm:
    provider: managed-identity
    endpoint: https://icm-mcp-staging.azure-api.net/v1/
    # NOTE: icm-mcp-staging.azure-api.net must be in icm.tool.yaml's
    # auth.allowed_hosts, or PLAN-013 will reject this profile at plan time.
```

---

## 11. Reference — Error Codes Mentioned in This Document

| Code | Check | Layer |
|---|---|---|
| PLAN-010 | `allowed-environments` mismatch | Tier 0 planning |
| PLAN-011 | Attendance/context conflict (`attended` + `ci`/`headless-server`) | Tier 0 planning |
| PLAN-012 | Test-context transport violation (mcp-http or unapproved mcp) | Tier 0 planning |
| PLAN-013 | Profile endpoint override host not in `allowed_hosts` | Tier 0 planning |
| PROF-001 | Profile attempts to set transport mode on a tool override | Profile parse |
| MCP-010 | `auth:` present but `allowed_hosts:` absent | Config load |
| MCP-011 | Transport URL host not in `allowed_hosts` | Config load |
| MCP-012 | Request host not in `allowed_hosts` at runtime | HTTP dispatch |

All PLAN-* checks run during planning, before any execution step.  PROF-001 runs at
profile parse time.  MCP-010 and MCP-011 run at tool configuration load time.  MCP-012
is the only check that fires during an actual HTTP request — it is a defense-in-depth
backstop for cases where the URL is constructed at runtime (e.g., from env var
interpolation that was not fully evaluated at scan time).

---

*Yawr Core Team — 2026-08-17*
