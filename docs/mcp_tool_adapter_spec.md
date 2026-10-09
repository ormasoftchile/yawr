# YAWR Model Context Protocol (MCP) Tool Adapter Specification

## 1. Overview & Architectural Role

YAWR provides a first-class, standalone tool invocation engine (`tool.ToolInvoker`) that enforces governance policies, approval gating, persistent intent journaling, and evidence capture without requiring synthetic workflow runs.

External agent orchestration platforms—such as A4C (Agents for Coding), GitHub Copilot, and OpenCode—expose tools to LLMs via the [Model Context Protocol (MCP)](https://modelcontextprotocol.io/).

This document specifies the standard adapter architecture for exposing YAWR-registered tools as an MCP Server, translating JSON-RPC `tools/list` and `tools/call` requests into YAWR's hardened Go SDK invocation primitives.

```text
       +---------------------------------------------+
       |   AI Agent Runtime (Copilot / OpenCode)     |
       +---------------------------------------------+
                              |
                     JSON-RPC | (tools/list, tools/call)
                              v
       +---------------------------------------------+
       |         Host MCP Adapter (e.g. A4C)         |
       |  - Authenticates caller session             |
       |  - Constructs trusted InvocationAuthority   |
       |  - Assigns CorrelationID & IdempotencyKey   |
       +---------------------------------------------+
                              |
               Go API         | run.NewToolInvoker / tool.ToolInvoker
                              v
       +---------------------------------------------+
       |             YAWR Tool Invoker               |
       |  - Schema & Argument Validation             |
       |  - Authority & Scope Allowlist Checking     |
       |  - Governance & Approval Evaluation         |
       |  - Durable Dispatch Journaling (flock)      |
       |  - Execution over Transport (Native/Stdio)  |
       |  - Evidence & Audit Record Persistence      |
       +---------------------------------------------+
```

---

## 2. Trust Boundary & Host Authority Model

The primary security requirement of the MCP adapter is strict preservation of the host trust boundary:

| Parameter Category | Fields | Provenance | Trust Level |
|---|---|---|---|
| **Untrusted Invocation Content** | `Tool`, `Action`, `Arguments` | Provided by LLM via MCP `tools/call` payload | **Untrusted**; validated against schema, but confers no permissions. |
| **Trusted Execution Envelope** | `Authority` (`Actor`, `Context`, `Attendance`, `AllowRead`, `AllowMutating`, `AllowDestructive`, `AllowedTools`), `CorrelationID`, `IdempotencyKey` | Constructed by host application from its authenticated session context | **Trusted**; must NEVER be deserialized from client JSON-RPC input. |

### Security Invariants
1. **No Client Privilege Escalation**: An agent cannot grant itself mutating or destructive capabilities by passing metadata in `arguments`.
2. **Actor-Scoped Idempotency**: The host must derive or bind `IdempotencyKey` to the authenticated `Authority.Actor`. Two distinct actors supplying the same key cannot collide or observe each other's settled outcomes.
3. **Fail-Closed Governance**: If an action is unclassified, or if the host authority lacks the required allowance (`AllowRead`, `AllowMutating`, `AllowDestructive`), the invoker denies execution before spawning any process.

---

## 3. Protocol Mapping: `tools/list`

The adapter maps registered YAWR tool definitions (`tool.ToolDef`) and their actions (`tool.ToolAction`) into MCP `Tool` descriptors.

### Naming Convention
Because MCP exposes tools with a single flat name while YAWR organizes capabilities into `Tool` and `Action`, the adapter canonicalizes tool names as:
```text
<tool_name>__<action_name>
```
For example:
- YAWR Tool `git`, Action `status` $\rightarrow$ MCP Tool `git__status`
- YAWR Tool `fs`, Action `read_file` $\rightarrow$ MCP Tool `fs__read_file`

### Schema Mapping
Each declared argument in `action.Args` is transformed into JSON Schema properties:
- `arg.Type`: Mapped to JSON schema `type` (`"string"`, `"integer"`, `"boolean"`, `"array"`).
- `arg.Required`: Added to the JSON schema `required` array.
- `arg.Description`: Copied to property `description`.
- `arg.Enum`: Mapped to JSON schema `enum` values.
- `arg.Default`: Copied to property `default`.

#### Example MCP `tools/list` Response
```json
{
  "tools": [
    {
      "name": "system_info__version",
      "description": "Print system version and OS release",
      "inputSchema": {
        "type": "object",
        "properties": {
          "target": {
            "type": "string",
            "description": "Target architecture or platform"
          }
        },
        "required": ["target"]
      }
    }
  ]
}
```

---

## 4. Protocol Mapping: `tools/call`

When an MCP client issues `tools/call`:
```json
{
  "jsonrpc": "2.0",
  "id": 42,
  "method": "tools/call",
  "params": {
    "name": "system_info__version",
    "arguments": {
      "target": "darwin-arm64"
    }
  }
}
```

### Execution Flow in the Host Adapter
1. **Parse Tool & Action**:
   Split `name` on `__` into `toolName = "system_info"`, `actionName = "version"`.
2. **Construct Trusted Context**:
   The host extracts the active session context and constructs the trusted request:
   ```go
   req := tool.InvocationRequest{
       Tool:      toolName,
       Action:    actionName,
       Arguments: params.Arguments,
       Authority: tool.InvocationAuthority{
           Actor:            session.AuthenticatedUser, // e.g. "a4c:user-123"
           Context:          schema.ProfileContextProduction,
           Attendance:       schema.ProfileAttendanceAttended,
           AllowRead:        session.HasScope("tools:read"),
           AllowMutating:    session.HasScope("tools:mutate"),
           AllowDestructive: session.HasScope("tools:destructive"),
           AllowedTools:     session.PermittedToolPatterns,
           ApprovalGate:     hostApprovalBridge,
       },
       CorrelationID:  requestHeader.TraceID,
       IdempotencyKey: deriveIdempotencyKey(session.ID, params),
   }
   ```
3. **Dispatch to YAWR Invoker**:
   ```go
   res, err := invoker.Invoke(ctx, req)
   ```
4. **Translate Outcome**:
   Map `tool.InvocationResult` to MCP tool response:
   - `Status == InvocationStatusCompleted`:
     Return MCP content (`text` containing stdout/output).
   - `Status == InvocationStatusDenied`:
     Return MCP result with `isError: true` containing governance denial reason.
   - `Status == InvocationStatusApprovalRequired`:
     Return structured message informing agent that human approval was requested out of band.
   - `Status == InvocationStatusIndeterminate`:
     Return `isError: true` indicating unconfirmed prior crash requiring manual verification.

---

## 5. Durable Intent Journaling & Idempotency

YAWR's `FileDispatchStore` guarantees crash recovery across process restarts and concurrent contention:
- **Locking**: Exclusive per-key cross-process file locks (`flock`) held across the `prepare` $\rightarrow$ `execute` $\rightarrow$ `settle` span.
- **Deduplication**: Replaying an identical request returns `replayed: true` with the settled output without repeating provider side effects.
- **Conflict Rejection**: Reusing an idempotency key with different arguments fails closed with `ErrIdempotencyConflict`.
- **Crash Recovery**: If an execution terminates unexpectedly (e.g. host crash), subsequent calls observe the uncommitted intent and safely fail closed as `InvocationStatusIndeterminate`.

---

## 6. Containment Realities & Advisory Workspace Boundaries

YAWR's `InvocationAuthority.WorkspaceRoot` provides advisory containment metadata for tools that honor it:
- Generic CLI processes (`TransportNative`) run directly under host OS credentials and cannot be strictly sandboxed by path sanitation alone.
- Strong filesystem confinement requires platform-level containment mechanisms, such as Windows AppContainer (`TransportNativeFileOnly`) or Linux container namespaces/cgroups.
- The host adapter MUST NOT assume filesystem isolation solely from `WorkspaceRoot` unless backed by an isolated transport.
