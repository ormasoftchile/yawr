# Yawr stdio run protocol v1

`yawr run --stdio <runbook>` reserves stdin and stdout for newline-delimited JSON.
It uses the normal CLI parser, planner, package-map resolution, tool registry, and
engine. No HTTP listener or SSE connection is created.

## Framing

Each message is one UTF-8 JSON object followed by `\n`. Every Yawr-to-client frame
contains:

```json
{"version":"yawr.stdio/v1","type":"..."}
```

Clients must reject unknown protocol versions. Human diagnostics and warnings are
written to stderr; stdout contains protocol frames only.

## Yawr-to-client frames

- `run.started`: `{ runID, status }`
- `run.event`: `{ runID, event }`, where `event` is an engine event with a bounded,
  preview-safe payload
- `interaction.pending`: `{ runID, turnID, interaction }`
- `interaction.resolved`: `{ runID, turnID, interaction }`
- `run.finished`: `{ runID, status, steps }`; each step summary includes the
  authored `step_id` and exact graph/runtime `node_id`
- `protocol.error`: `{ error: { code, message } }`

`interaction` uses the bounded, opaque-token wire shape shared with the previous
preview transport. Clients display labels but return tokens unchanged.

All `run.event` frames are written before `run.finished`. The terminal frame is
the last accepted frame for that run. Its step summaries are preview-bounded;
`stepsTruncated`/`stepsOmitted` and output omission markers distinguish bounded
data from genuinely empty results. Declared secret values and governance
redaction patterns are scrubbed from payload content without changing protocol
types, IDs, statuses, event kinds, schema keys, or opaque interaction tokens.
Clients reconcile terminal state by exact `node_id`; they must not suffix-match
`step_id`, because repeated included runbooks may contain the same authored ID.

## Optional live execution graphs

Clients opt in using `--require-capabilities yawr.run-graph/v1` (comma-separated
with other required capabilities). Older runtimes reject this before execution;
clients must not silently fall back to an incomplete dynamic graph.
Without this capability the existing frame stream is unchanged.

On dynamic include resolution, the runtime projects cumulative graphs from the
frozen execution plan and pinned child closures, including earlier invocations.
It never reloads current source definitions to describe an executed child.

`run.graph.chunk` carries:

```json
{
  "version": "yawr.stdio/v1",
  "type": "run.graph.chunk",
  "runID": "run-id",
  "revision": 1,
  "digest": "sha256:<digest-of-complete-decoded-body>",
  "offset": 0,
  "totalBytes": 12345,
  "data": "<base64>"
}
```

The complete UTF-8 body is `{ "document": <GraphJSON>, "nodeIDs": [...] }`.
`nodeIDs` identifies the exact dynamic occurrence nodes to merge into the
existing static preview; IDs are opaque, not inferred from a prefix or filename.
The revision increases with committed dynamic resolutions and may jump when a
snapshot already includes multiple resolutions. Maximum decoded body: 32 MiB;
maximum decoded chunk: 192 KiB. Each wire frame remains below the existing 1 MiB
limit. Clients require contiguous offsets, one revision/digest/length per
assembly, a matching SHA-256, valid GraphJSON, and unique existing node bindings.
Partial graphs must never be displayed. Content is redacted before base64
encoding; transport IDs remain intact.

The graph is delivered before dependent child events or interactions.
Child event payloads retain their original `qualified_node_id` and occurrence
metadata and add `graph_node_id` for that exact retained invocation.
`interaction.pending.interaction.nodeID` uses the corresponding graph node;
run/turn IDs and opaque answer tokens are unchanged. The client must not
suffix-match a repeated child step or substitute an ancestor.

Graph production runs in the protocol consumer, not the execution scheduler.
Clients may pace visual CURRENT transitions, but must not delay runtime work,
Results persistence, cancellation or interaction answers. Graph changes do not
flush a successful visual backlog. History remains until the client explicitly
resets/disposes its run view.

## Client-to-Yawr commands

Answer a pending interaction:

```json
{
  "type": "interaction.answer",
  "runID": "run-id",
  "turnID": "turn-id",
  "answer": {
    "kind": "choice",
    "selected": ["o:0"]
  }
}
```

Cancel the active run:

```json
{"type":"run.cancel","runID":"run-id","reason":"operator cancelled"}
```

Supported interaction answer kinds are `choice`, `decision`, `collector`,
`approval`, `host_action`, and `debug_break`. Yawr validates the run ID, turn ID, kind, opaque
answer tokens, host-action tuple, and status before unblocking execution.

Governance approval is explicit and fail-closed:

```json
{
  "type": "interaction.answer",
  "runID": "run-id",
  "turnID": "turn-id",
  "answer": {
    "kind": "approval",
    "approved": true,
    "approver": "operator-id"
  }
}
```

Approving requires a non-empty bounded identity. Denial uses
`{"kind":"approval","approved":false}` and fails the governed step. Stdio
never installs the unattended no-op approval gate.

Closing stdin cancels the active run. Closing the client process therefore cannot
leave Yawr blocked indefinitely on an interaction.

## Debug startup

`yawr run --stdio --debug <runbook>` opts into debugger startup. Before the engine
starts, the client must send exactly one configuration command:

```json
{
  "type": "run.configure",
  "debug": {
    "enabled": true,
    "breakpoints": [
      {
        "step": "get_incident",
        "phase": "after",
        "callPath": [{ "step_id": "inspect_primary_icm" }]
      }
    ],
    "watches": ["incident_status"]
  }
}
```

The configuration is validated by `PromptBroker.ConfigureDebug` before
`Engine.Start`. A missing, disabled, or malformed configuration fails startup.
Normal `--stdio` runs do not wait for this command. Debugger resume remains
unsupported in v1; clients start a new debug run.

## Private startup inputs

`yawr run --stdio --configure <runbook>` reads one `run.configure` frame before
planning. This keeps declared secret inputs out of process arguments:

```json
{
  "type": "run.configure",
  "inputs": { "access_token": "private value" }
}
```

Only inputs declared with `type: secret` may use this channel. Names and values
are bounded, and a key duplicated by `--var` is rejected. `--debug` uses the same
single frame and may carry both `inputs` and `debug`; clients must not send two
startup frames.
