# UX principles

## Runtime-authoritative presentation

The interface explains runtime state; it does not invent it. Graph nodes, status
badges, diagnostics, results, and interaction controls are projections of
versioned runtime messages. When presentation data is unavailable or bounded,
the UI says so instead of filling gaps from stale source or client assumptions.

## Make the next action explicit

Operator actions are deliberate and scoped to the active run and turn:

- running and debugging are separate choices;
- approval and denial are explicit;
- host actions require an in-panel confirmation;
- required arguments are inserted only by an explicit command;
- cancellation, retry, and resume are not presented as interchangeable.

The UI disables or removes actions that the current capability set cannot
support. It does not downgrade to an older protocol or alternate execution path
after a capability failure.

## Show definition, execution, and debug truth separately

The inspector separates:

- **Definition** — authored behavior and declared contracts;
- **Run** — bounded live status, timings, output, captures, and evidence;
- **Debug** — breakpoints, watches, and actual versus effective values.

This avoids presenting authored intent as observed fact. Debug overrides remain
visibly marked, and the immutable actual result remains available beside the
effective value that drove later flow.

## Preserve semantic distinctions

Color, labels, icons, and layout reinforce status but do not replace text.
Failure, denial, indeterminate completion, cancellation, blocked domain outcomes,
and unavailable data remain distinguishable. Empty, zero, false, and null are
rendered as values rather than absence.

Theme and node-style choices are visual. They cannot change workflow semantics,
hide mandatory safety states, execute tenant-provided code, or weaken redaction.

## Protect sensitive data

Secret inputs use password controls and private configuration frames. Sensitive
arguments, credential-like values, process output, debug variables, and host
action data are redacted or omitted according to runtime metadata and bounded
client safeguards.

The browser or webview is not an authorization boundary. The runtime and trusted
host validate run, turn, capability, and payload identity before dispatch.

## Work at editor scale

The graph is the primary structural view. The inspector is a side rail when
space permits and becomes a scrollable drawer in narrow editor groups. Selection
keeps the relevant definition and run state together without requiring a second
generic metadata surface.

Diagnostics identify the affected source or operation and offer repair only when
the runtime can prove a safe edit. Broken or ambiguous source remains unavailable
instead of receiving a speculative fix.
