# Runbook Interaction Wire Protocol v1

Status: accepted (May 2026)
Replaces: nothing (first version)
Scope: HTTP/SSE between the yawr server and a runtime UI (web, VS Code,
mobile bridge) that drives interactive runbook execution.

## Goals

1. Let a network client start a run and answer its interactive prompts
  (`choice`, `decision`, `collector`, `host_action`, `debug_break`) without using the in-process
   `input.PromptProvider` interface.
2. Be observable: the client can replay missed prompts after a brief
   reconnect window without losing state.
3. Stay close to the existing `RunEvent` and `runstate` SSE shape — no
   new authentication/authorization concepts in v1; reuse server config.

## Non-goals

- v1 does not standardize a way to upload binary attachments inside an
  answer. (Files are still posted via the existing
  `/api/yawr/v1/runs/{runID}/attachments/{sha256}` route.)
- v1 does not stream typing-ahead or partial-answer state.

## Lifecycle

```
client                            server                       engine
  │                                  │                            │
  │ POST /runs (runbookPath, vars)   │                            │
  │ ────────────────────────────────►│                            │
  │                                  │ engine.Start(plan, ...)    │
  │                                  │ ───────────────────────────►
  │ ◄────── 201 {runID} ─────────────│                            │
  │                                  │                            │
  │ GET /runs/{id}/interactions      │                            │
  │   (SSE; Last-Event-ID for replay)│                            │
  │ ◄═══════════════ subscribe ═════►│                            │
  │                                  │ executor calls Prompt*     │
  │                                  │ ◄──────────────────────────│
  │ ◄── data: PendingInteraction ────│                            │
  │                                  │                            │
  │ POST /runs/{id}/interactions/{turnID}                         │
  │  body: AnswerEnvelope            │                            │
  │ ────────────────────────────────►│                            │
  │ ◄────── 204 ─────────────────────│ Prompt* returns ──────────►│
  │                                  │ ... step continues         │
  │                                  │                            │
  │ ◄── data: {"type":"resolved",    │                            │
  │            "turnID":"..."}       │                            │
```

The same `runID` is used for the existing `/runs/{id}/document` and
`/runs/{id}/state` SSE channels.

## Endpoints

### `POST /runs`

Creates and starts a run. Body:

```json
{
  "runbookPath": "/abs/path/to/x.runbook.yaml",
  "inputs":      { "foo": "bar" },
  "mode":        "real",
  "actor":       "web-ui"
}
```

`mode` ∈ `{real, dry-run, replay}`, defaults to `real`. `actor` defaults
to `"http"`.

Response `201 Created`:

```json
{ "runID": "9f3...e2" }
```

Errors map onto the existing JSON-RPC code → HTTP code table:
`invalid params → 400`, `runbook not found → 404`, `internal → 500`.

### `GET /runs/{id}/interactions` (SSE)

Server-Sent Events stream of prompts requested by the run. The stream
emits exactly one of two frame shapes:

- `pending` — a new prompt the client must answer
- `resolved` — a previously-pending prompt whose answer has been
  accepted (so a reconnecting client knows not to ask again)

Frames carry a monotonically increasing `id` per run. Reconnecting
clients SHOULD send `Last-Event-ID` to receive missed frames.

Response headers:

```
Content-Type: text/event-stream
Cache-Control: no-cache
```

#### `pending` frame

```json
{
  "type":   "pending",
  "turnID": "01HW...XX",
  "runID":  "9f3...e2",
  "stepID": "collect_findings",
  "kind":   "collector",
  "title":  "Record findings",
  "prompt": "Describe the impact",
  "fields": [
    {
      "name":     "severity",
      "type":     "choice",
      "label":    "Severity",
      "required": true,
      "options":  [
        { "label": "Low",    "value": "low"    },
        { "label": "Medium", "value": "medium" },
        { "label": "High",   "value": "high"   }
      ]
    },
    { "name": "notes", "type": "text", "label": "Notes" }
  ]
}
```

`kind` ∈ `{choice, decision, collector, host_action, debug_break}`. The `fields` array is present
only for `collector` and `choice`-with-multiple. `options` is present
for `choice` and `decision` (mapped from `routes`).

`turnID` is generated server-side and is the only id the client uses to
post the answer.

### Typed host action

> Go API migration: see [Generic host-action migration](../docs/host-action-generic-migration.md).

`host_action` is a deliberately narrow interaction rather than a generic
command bridge. Runbooks supply a logical capability name and structured data,
never a host command ID. The connected host owns a static capability registry
and validates each capability's request object before dispatch. Its pending
frame includes a server-generated `correlationID` and this generic request:

```json
{
  "type": "pending",
  "kind": "host_action",
  "runID": "run-...",
  "turnID": "opaque-server-turn",
  "correlationID": "opaque-server-turn",
  "stepID": "open_resource",
  "host_action": {
    "capability": "product.open-resource",
    "request": {
      "resource": "incident-42",
      "options": { "focus": true }
    }
  }
}
```

The acknowledgement carries one generic bridge status (`completed`, `failed`,
`timed-out`, `unsupported`, or `execution-not-started`) and an optional
capability-owned `result` object. A completed acknowledgement requires a result;
non-completed acknowledgements carry no result. Host handlers must return only
bounded, non-sensitive result data.
Each turn is scoped to one run and may be acknowledged once; stale or duplicate
acknowledgements receive `409 Conflict`.

The served preview forwards a pending host action to an embedding parent with
`yawr.host-action.request` and requires a matching
`yawr.host-action.ack` containing the same `runID`, `turnID`,
`correlationID`, preview session ID, and request ID. A preview reload creates a
new session ID and sends `yawr.host-action.cancel` for its outstanding
requests, so an acknowledgement for an earlier page instance is ignored.
The exact closed request, acknowledgement, and cancellation frames are fixed
by `internal/serve/testdata/host_action_wire_v1.json`: capability appears only
inside a request frame's `request` object and only at the acknowledgement's
top level. When no host provider exists, the framework-local completed result
has `outputs.status: "unsupported"`, `outputs.result.status: "unsupported"`,
and `outputs.outcome: "unsupported"`. A standalone browser preview reports
`execution-not-started` without attempting host work.

### Debug break

`debug_break` is emitted only for a `POST /runs` request that explicitly enables
debugging. It pauses before execution or after executor return but before result
commit. The bounded `debug` object carries phase, exact nested call path,
invocation/attempt identity, variables, optional actual result, and read-only GXL
watch results. See [Runbook Debugging v1](runbook-debugging-v1.md) for the complete
shape and safety rules.

#### `resolved` frame

```json
{ "type": "resolved", "turnID": "01HW...XX" }
```

### `POST /runs/{id}/interactions/{turnID}`

Submit an answer. Body (`AnswerEnvelope`):

```json
{
  "kind":   "collector",
  "values": { "severity": "high", "notes": "..." }
}
```

For `choice`, the body is `{"kind":"choice","selected":["a","b"]}`.
For `decision`, the body is `{"kind":"decision","label":"approved"}`.
For `debug_break`, the body carries `action` (`continue`, `stop`, `step_into`,
`step_over`, or `step_out`) and an optional validated `set` object containing
runtime-variable patches and effective output/status.

`kind` MUST match the pending frame's `kind`. Mismatches return `400
{"error":"interaction kind mismatch"}`.

Response `204 No Content` on success. After this, the engine resumes
the step. The client SHOULD NOT post a second answer for the same
`turnID`; subsequent posts return `409 Conflict`.

If the run is unknown: `404`. If the `turnID` is unknown or already
resolved: `409`.

## Replay & reconnect

- The server keeps the last 64 interaction frames per run (configurable
  via `ServerConfig.InteractionBufferSize`, default 64).
- A reconnecting client sends `Last-Event-ID: <id>`; the server replays
  every frame with `id > Last-Event-ID` then resumes live streaming.
- A still-pending interaction with no answer yet is re-emitted as a
  fresh `pending` frame after a reconnect even if it was already in the
  buffer, so a client that never received the original frame still sees
  it.

## Cancellation

If the run is cancelled while a prompt is open, the server emits a
`resolved` frame with `"cancelled": true` so the client can drop its
form state. The engine's `Prompt*` call returns `context.Canceled` and
the step fails with the standard cancellation status.

## Security

v1 inherits the bearer-token model from `ServerConfig.AuthToken`. Same
token gates `/rpc`, `/events`, `/runs/...` and the new routes.
