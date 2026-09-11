# MCP HTTP response budgets

Streamable HTTP MCP uses the same response parser for initialization,
`tools/list`, `tools/call`, token-refresh retries, and session-expiration
reinitialization/retries. The following fixed limits apply independently to
each HTTP response. There is **no CLI, environment, profile, or tool-binding
setting** to override them.

| Budget | Limit | Accounting |
| --- | ---: | --- |
| SSE event | 16,777,216 bytes (16 MiB) | All wire bytes in one nonempty event block, including field names, comments, ignored fields, and LF/CRLF line endings; excludes its blank separator line |
| SSE response | 67,108,864 bytes (64 MiB) | All body bytes consumed through the matching response, including skipped events, comments and separators |
| SSE event count | 4,096 | Nonempty blocks, including comment-only, malformed, notification and unrelated-ID blocks; the matching block also counts |
| Plain JSON response | 67,108,864 bytes (64 MiB) | Complete body bytes |

An individual SSE line is bounded by the remaining event budget. Joining
multiple `data:` lines never bypasses that budget. Network reads are handled
in 32 KiB buffered fragments and checked before accumulation. The body reader
is additionally limited to the response budget plus one overflow-detection
byte. Only the current event is retained, not the stream history. These are
wire-size limits, not an exact process-heap bound: line buffers, event buffers,
JSON decoding and the resulting tool output require additional bounded memory.

Exceeding any limit fails explicitly with `MCP-008`, the budget name and
numeric limit, and `response rejected (not truncated)`. No successful partial
result is returned, no tool fields are omitted, and caller projection is not
used to hide transport failures. HTTP bodies are closed, not drained after a
terminal response, failure, or ignored notification acknowledgement. Bytes
after a matched SSE response are not needed and are not consumed deliberately;
bounded buffer read-ahead may already have read a prefix of them.

The parser preserves multiline data (joined with LF), CRLF, comments, ignored
SSE fields, matching JSON-RPC IDs, notifications, malformed-event skipping,
and the existing final-event-at-EOF behavior. EOF without a matching response
is `MCP-006`; network/cancellation errors remain `MCP-008`. Existing HTTP
request contexts/timeouts continue to bound waiting, independently of bytes.

## Why these limits

The previous `bufio.Scanner` used Go's default 65,536-byte maximum token buffer
(usable line length is smaller when terminators require buffer space), with
no supported override and no aggregate event/response budget. The reported
Performance failures on two delivered runtimes identify that scanner error;
the same-tool GeoDR response succeeds. **The actual failing SSE payload size
is unmeasured.** Neither the handoff nor a captured CLI error establishes it.

The 16 MiB event allowance raises the previous line ceiling by roughly 256
times while keeping a finite per-call allocation policy. The 64 MiB response
budget permits several large events or interleaved progress; 4,096 event blocks
separately bound repeated JSON parsing of small events. These are conservative
engineering limits, not measurements or a promise that the private incident
fits. Synthetic tests preserve a complete large multi-field incident and
prove exact-boundary/oversize behavior. The original incident must be rerun
by its owner; no live provider, incident, query or DRI action is needed for
these regressions.
