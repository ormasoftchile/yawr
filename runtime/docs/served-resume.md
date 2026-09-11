# Resuming a served interactive run

This is for a plain run created by `POST /runs`, not an investigation session.
Keep its original run ID, working directory, run store, package bindings and
runbook files. Stop the previous writer before takeover; never delete writer
lock/epoch files to bypass a live writer.

## Start or restart the server

From the original working directory:

```powershell
yawr serve --addr 127.0.0.1:7778 --run-dir .\runs --package-map .\probe.package-map.yaml
```

Use the same store and map after restart. Paths in the run and bindings must
still resolve. `GET /health` should return HTTP 200. These examples use loopback
without authentication; supply the configured authorization header when needed.

A fresh run is created with:

```http
POST /runs
Content-Type: application/json

{"runbookPath":"public.runbook.yaml","mode":"real","actor":"operator-name"}
```

HTTP 201 returns `{"runID":"run-..."}`. Fresh `POST /runs` execution advances
automatically. Its pending collector is delivered on
`GET /runs/<runID>/interactions` (an open SSE stream).

## Attach the saved run after restart

**GET does not autoattach.** A newly restarted server has an empty in-memory
registry, so `GET /runs/<runID>/interactions` returns 404 until attachment.
Repeating `POST /runs` creates a different run.

```http
POST /rpc
Content-Type: application/json

{"jsonrpc":"2.0","id":1,"method":"run.resume","params":{"runID":"run-...","actor":"operator-name"}}
```

HTTP 200 contains either a JSON-RPC result or error; check the body:

```json
{"jsonrpc":"2.0","id":1,"result":{"runID":"run-...","resumedFromStep":"public_call"}}
```

`resumedFromStep` identifies the root resume step when available; it can be
empty. Attachment preserves the original run ID. It registers the interaction
broker before engine recovery and uses a run-lifetime context, not the completed
HTTP request's context. Failure releases its broker reservation. A duplicate
or in-flight attachment returns `-32013` (`Run already attached`); a writer in
another process returns `-32013` (`Run locked by another process`). Neither
replaces an active handle or queue. Existing checkpoint/plan validation and
writer fencing remain mandatory; an invalid checkpoint is not repaired by
reattachment.

## Drive and answer concurrently

**RPC resume is step-driven, not auto-advance.** Use this order:

1. Attach with `run.resume` and inspect the result.
2. Open `GET /runs/<runID>/interactions` as SSE. After process restart, omit
   `Last-Event-ID`: SSE sequence IDs are local to the new broker queue. The
   durable pending **turn ID**, unlike the SSE cursor, is preserved.
3. Issue `run.next` on another connection/task:

   ```http
   POST /rpc
   Content-Type: application/json

   {"jsonrpc":"2.0","id":2,"method":"run.next","params":{"runID":"run-..."}}
   ```

4. Continue reading SSE while that request is outstanding. `run.next` can
   **block inside an interactive step until the answer is submitted**. A client
   that waits synchronously for its response before listening/submitting will
   deadlock. Keep the request/connection alive and account for client/proxy
   timeouts; issue only one advancing `run.next` at a time.
5. Use the regenerated pending frame's `runID`, `turnID`, `kind`, and actual
   field `name` tokens. For a collector with returned tokens `f:0`, `f:1`, `f:2`:

   ```http
   POST /runs/run-.../interactions/<returned-turnID>
   Content-Type: application/json

   {"kind":"collector","values":{"f:0":"blocked","f:1":false,"f:2":0}}
   ```

   HTTP 204 accepts the answer. Replace these synthetic values with the real
   operator's response. Field display names are not tokens. Preserve JSON
   booleans/numbers; `"false"` and `"0"` are different values.
6. Observe the `resolved` SSE frame with the original turn ID and the outstanding
   `run.next` response. Then issue subsequent `run.next` calls one at a time for
   remaining caller steps, continuing to handle any new pending turns.
7. At end-of-plan, `run.next` returns JSON-RPC `-32012` (`Run completed`).
   `run.get` returns `result.state: "completed"`; the interaction stream closes.
   A later `run.next` may return `-32011` (`Run not active`). Do not treat all
   JSON-RPC errors as successful completion.

Unknown runs return 404 on submission; stale/already-answered turns return 409.
Wrong kind or field tokens return 400. A terminal run rejects duplicate answers
with 409. Rejection does not supply a replacement result or advance the caller.

## CLI/stdio alternative

Stop the HTTP writer first, then run from the original working directory:

```powershell
yawr run --resume <saved-run-id> --stdio --run-dir .\runs --package-map .\probe.package-map.yaml
```

Keep stdin open. Wait for `interaction.pending`, then write/flush one JSON line:

```json
{"type":"interaction.answer","runID":"<saved-run-id>","turnID":"<returned-turn-id>","answer":{"kind":"collector","values":{"f:0":"blocked","f:1":false,"f:2":0}}}
```

The CLI drives progression itself. Wait for `interaction.resolved` and ultimately
`run.finished` with `status: "completed"` before closing stdin (EOF cancels an
active run). This resumes in the CLI process; it does not attach an HTTP server.
Do not use `--configure` or `--debug` with resume. `yawr session attach` instead
requires a separate investigation-session record, not an arbitrary served run ID.
Preview/stdio output may be bounded; verify full typed results through caller
assertions and durable state rather than assuming previews contain every field.

## Regression coverage

`cmd\yawr\served_resume_integration_test.go` launches real `serve` subprocesses
and uses no live provider. A public tool calls an outer tool and inner collector;
the caller asserts `blocked`, boolean `false`, and numeric `0`. It covers fresh
execution, durable pending state, live-writer rejection, abrupt process exit,
same-store HTTP reattachment, original IDs, concurrent Next/answer, invalid and
duplicate answers, exactly-once caller completion and released writer leases.
