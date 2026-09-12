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
