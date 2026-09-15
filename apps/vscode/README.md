# Yawr Runbook Preview (VS Code extension)

Open a `*.runbook.yaml` file and use the preview or graph button in the editor
title bar. The same actions are also available from the Command Palette.

Three commands:

| Command | Behaviour | Requires |
|---|---|---|
| **Yawr: Open Runbook Preview** (`yawr.preview`) | Runs the helper preview command against the active `.runbook.yaml` file and opens the rendered Markdown in a side-by-side preview. | Packaged `yawr` helper, or set `yawr.binaryPath`. |
| **Yawr: Open Runbook Graph (React Flow)** (`yawr.previewGraph`) | Opens the complete runbook UI using the `yawr.stdio/v1` runtime protocol. | Packaged `yawr` helper, or set `yawr.binaryPath`. |
| **Yawr: Run Current Runbook** (`yawr.runCurrentRunbook`) | Opens the production graph session and starts its normal `yawr run --stdio` flow. The command resolves only at terminal `run.finished` with bounded, non-secret launch/frame/result metadata for automation. | Packaged `yawr` helper by default, or explicit `yawr.binaryPath`. |
| **Yawr: Validate Runbook Inputs (Dry Run)** (`yawr.validateInputs`) | Prompts for declared inputs and delegates validation to the real runtime. | Packaged `yawr` helper, or set `yawr.binaryPath`. |

## Inspector

The right rail is an operator inspector rather than a generic metadata panel.
With no selection it shows run progress, current step, outcome counts, inputs,
breakpoints, source, and diagnostics. Selecting a step opens three compact tabs:

- **Definition** renders authored behavior for that kind: tool/action and arguments,
  CLI command/process policy, branch conditions, prompts/options/fields, include
  bindings, loop or join policy, approval quorum, assertions, event filters,
  display content, compensation trigger, or terminal outcome.
- **Run** shows bounded live execution data: status, attempt, timing, errors,
  output, captures, process output, and evidence.
- **Debug** contains breakpoints, watches, and actual/effective override audit data.

The inspector becomes a scrollable bottom drawer on narrow editor groups. Declared
runtime secret values are never added to graph details; sensitive authored keys
and credential-like flags, assignments, and authorization values are redacted.

## Execution navigation

`yawr.preview.minimumStepDisplayMs` defaults to **200** milliseconds and accepts
nonnegative integers; **0** disables pacing. It affects editor visualization
only, never runtime execution, event persistence, tools, or headless clients.
Each run/session snapshots the setting at start (including reattachment);
mid-run changes apply to the next run/session.

Ordinary live steps are displayed in event order for at least that interval.
Only the visual queue head owns the graph's Current marker and progress glow.
Runtime-only wrappers and dynamic identities absent from the graph remain in
runtime/inspector evidence, but do not consume graph dwell intervals or redirect
the marker to live activity. Known graph aliases are canonicalized before queueing.
Successful runtime completion does not discard queued visuals: playback continues
through every ordinary step, including Results, and the final step's full interval.
Runtime status and Results processing/persistence update immediately; automatic
Results selection waits for playback to finish so it does not move selection mid-step.
Failure, cancellation (including user cancellation), blocked outcomes and input/debug
prompts discard queued visual steps immediately. Reconnect applies historical snapshots
directly until the attachment handshake; only later live starts are paced.
Hiding cancels timers and showing converges to the latest cursor (or completed
state); disposing or replacing the graph invalidates pending callbacks. Refreshing
the same graph and normal process exit do not interrupt successful playback.

Reduced motion changes the glow/pan animation, **not** the visibility interval.
Fake-clock tests enforce exact deadlines. Frame-sampled webview tests allow
35 ms of observation tolerance for frame/IPC scheduling at 200 and 500 ms;
this is a measurement tolerance, not time subtracted from the configured delay.
Installed-VSIX validation also observes the actual graph with a 10 ms sampler
and direct DOM-change records, avoiding misleading dwell measurements when the
browser throttles timers. It covers entire real-runtime runs at the default
200 ms and configured 500 ms, using production commands and a read-only
CDP observer in an isolated editor profile, with no keyboard/mouse input, and
checks uniqueness, canonical order, every dwell and immediate Results availability.

One green **Current** marker tracks execution independently of the yellow user
selection. It remains on the last reached step between runtime events, without
changing that step's completed/failed status, and becomes **Last reached** when
successful playback finishes (or immediately on unsuccessful termination).
Concurrent lanes retain their individual statuses; a new lane does
not steal the current marker from an active leaf. Input requests take priority.
A restrained, non-resizing glow indicates work in progress and respects reduced
motion preferences.

Execution temporarily shows all technical steps, including after completion,
so status updates cannot repeatedly collapse/expand groups and move the graph.
Reset restores the saved workflow-view preference. Routine advances preserve
selection and zoom; an offscreen current node receives only the minimum smooth
pan needed to reveal it. Explicit Locate, route-view and Fit controls still work.

There is no execution-log strip above the graph. **Execution activity** in the
inspector retains runtime paths, concurrent lanes, timing and Locate controls.
Per-step Run tabs retain output, errors, evidence and occurrence history; run
diagnostics remain in the overview and **Yawr: Show Run Log**. A runtime child
absent from the loaded graph remains identified by its exact path in activity
details, never by a fabricated child node.

YAWR tests cover cursor boundaries, parallel lanes, aliases, waiting/paused and
terminal rendering, fixed technical topology, animation timing/cancellation, and
frame-sampled VS Code webview transitions. Installed-editor review still needs
to check perceived animation quality across themes/styles, manual pan/zoom or
editor-group resizing during an advance, and hidden-tab reactivation during
dynamic session graph loading. A missing runtime child graph cannot be visually
qualified as an exact child node; its available container and activity path are
the honest navigation surfaces.

## Host-action protocol

The bundled webview may request the `xts.open-view` host capability, which requires
`view_path`, `environment`, `parameters`, and `focus`. The extension statically maps
each capability; a runbook can never choose a VS Code command ID. The correlated
result is written back to the active Yawr child over `yawr.stdio/v1`.

```json
{
  "type": "yawr.host-action.request",
  "version": "yawr.host-action/v1",
  "runId": "run-123",
  "turnId": "turn-123",
  "correlationId": "turn-123",
  "previewSessionId": "preview-session-123",
  "requestId": "preview-session-123:turn-123",
  "capability": "xts.open-view",
  "request": {
    "view_path": "Database Replicas.xts",
    "environment": "ProdEus1a",
    "parameters": { "server": "server-name", "database": "database-name" },
    "focus": true
  }
}
```

The Yawr wire shape is closed: undeclared fields are rejected. At the trusted
extension boundary, the capability maps only to
`xts.openViewByPath(view_path, args)`, preserving the relative filename verbatim.
The argument string starts with `-p environment:ProdEus1a`, followed by each
parameter as `-p name:value` (for example,
`-p environment:ProdEus1a -p server:server-name -p database:database-name`).
XTS 0.4.32 parses these values literally up to the next `-p`; no shell quoting
is added. String values preserve internal spaces, quotes, and backslashes;
finite numbers and booleans are serialized to text. Parameter names must use
word characters. Empty values, outer whitespace, nested objects/arrays, embedded
`-p` arguments, and overriding `environment` inside `parameters` are rejected
rather than changing the requested values. Runbooks own the view path and
must use the target view's declared parameter names. XTS remains responsible for
validating the target view's parameter schema, configured view roots,
authentication, and the explicitly supplied environment.
YAWR activates the installed XTS extension before dispatch when needed.
The current XTS command controls tab activation and has no `focus` argument;
`focus` continues to control YAWR's reminder after readiness verification.

The current command returns `void` and may display startup failures without
throwing. YAWR therefore never interprets command completion as `opened`.
The run remains at the host-action interaction until the operator returns to
the YAWR panel and selects **XTS view is ready**, after verifying that the real
view has loaded with the displayed filename, environment, and parameters.
**XTS failed to open** reports a failed handoff, not a completed investigation.
This is an explicit operator attestation, not an automatic XTS readiness probe.
The subsequent runbook classification/collector still requires its own answer.

Only after this correlated readiness confirmation does the webview receive:

```json
{
  "type": "yawr.host-action.ack",
  "version": "yawr.host-action/v1",
  "runId": "run-123",
  "turnId": "turn-123",
  "correlationId": "turn-123",
  "previewSessionId": "preview-session-123",
  "requestId": "preview-session-123:turn-123",
  "capability": "xts.open-view",
  "status": "completed",
  "result": { "status": "opened" },
  "error": null
}
```

The bridge ack statuses are `completed`, `failed`, `timed-out`, `unsupported`, and
`execution-not-started`. An operator-verified XTS view carries `opened` inside
`result.status` when the bridge status is `completed`. The extension never
includes a requested path, XTS row, or parameters in an acknowledgment.
Cancelling a matching tuple, reloading the preview, replacing its panel, or
disposing it acknowledges that tuple as `execution-not-started`; late command
results are ignored. Cancellation frames use their canonical
`correlationId`/`previewSessionId`/`requestId` subset, which is resolved only
against a pending full tuple. `unsupported` remains the framework-local Yawr
result for headless execution with no host provider and is also the bridge
acknowledgment for an unregistered capability. Registered XTS capabilities do
not normally produce it from the VS Code host.

## Debug runs

Select a graph node to add a before- or after-execution breakpoint and optional
watch expressions. **Debug Run** starts `yawr run --stdio --debug` and sends the
bounded breakpoint configuration over stdin before the engine starts. At a pause,
the inspector shows invocation identity, watches, runtime variables, the immutable
actual result, and controls for Continue, Apply and continue, Step into, Step over,
Step out, and Stop. After-execution pauses can patch effective output/status;
before-execution pauses can patch runtime variables. Applied overrides retain the
actual and effective values in the node audit details.

Reusable debug-profile list/save remains owned by Yawr's validated profile store
and is not exposed by this server-free extension yet. Interactive breakpoints,
watches, stepping, and overrides do not require profile persistence.

When governance requires approval, the same inspector displays an Approve/Deny
prompt. Approval requires an operator identity and is recorded by Yawr; direct
stdio runs never silently use the no-op approval gate.

Inputs declared as `type: secret` render as password fields and are sent in the
private `run.configure` stdin frame. They are never placed in child-process
arguments or emitted in run frames.

XTS host actions pause on an explicit **Open XTS** control in the run panel.
That reviewed in-panel action is the launch confirmation; the extension
does not show a second modal. After dispatch, a separate in-panel readiness
check keeps startup failures from advancing the run. A reminder appears only
after the operator verifies a focused XTS view is ready. Cancellation, panel
disposal, and the existing handoff timeout discard pending verification listeners.

## Installed-editor file-only subprocess support

Installed-editor file-only test subprocess execution is supported only through
the distinct `native-file-only` transport when the packaged runtime advertises
`yawr.file-only-subprocess/v1`. The existing unsandboxed test permission remains
distinct and is not a fallback. Installed qualification uses the production
`yawr run --stdio` argument construction and the packaged helper and fixture;
the current Extension Host harness executes that exact command directly because
it cannot drive a click from the webview back into the extension.

Native authoring remains exactly **EXTERNALLY GATED — NOT RUN — NOT PASSED**.

## Settings

- `yawr.packageMap` — package-map selection for direct execution. Absolute, or relative
  to the active runbook's project root. If it names `*.serve-package-map.yaml`, the extension uses
  the sibling `*.package-map.yaml` when present. Leave empty to use `package-map.yaml`. Example:
  `"packages/incident-routing.vscode-mcp.serve-package-map.yaml"`.
- `yawr.binaryPath` — path to the Yawr helper (default `yawr`).
- `yawr.preview.openLocation` — where the React Flow graph opens: `sameGroup` (default) opens it
  as a tab in the runbook editor's group; `beside` creates or uses an adjacent editor group.
- `yawr.mcpBridge.toolNameOverrides` — a JSON object mapping logical `"tool/action"` keys to
  registered MCP tool names. Use this to correct a name mismatch in a live session without a code
  change or extension release. Example:
  ```json
  {
    "tsg-recommendation/recommend": "my-org-tsg-recommend",
    "icm/get-incident": "corp-icm-get-incident"
  }
  ```
  This setting takes precedence over the name declared in the workspace's `.tool.yaml` definitions.
  The YAML-derived names are still used for all entries not listed here.

## Prerequisites

- VS Code 1.136.2 or newer. Source-host and installed-VSIX tests run on
  VS Code 1.136.2, including activation, all seven commands, graph rendering,
  zero-input execution, the bundled helper, Results, and 200/500 ms pacing.
- Node.js 20 or newer, including `npm`.
- Go 1.25 or newer to build the monorepo Yawr CLI.

## Build and debug

Open this component in VS Code and press F5. The debug task installs npm dependencies when needed, compiles the extension, builds `..\..\runtime\yawr.exe`, and prepends `..\..\runtime` to the Extension Development Host PATH.

Manual build:

```sh
npm install
npm run compile
```

## Package (.vsix)

Reproducible local build from a clean checkout:

```sh
npm install
npm run package
```

`npm run package` includes the runtime `js-yaml` dependency. Do not pass
`--no-dependencies`; that produces an extension which cannot activate.

Or use the pre-wired npm scripts:

```sh
npm run package          # produces yawr-preview.vsix (uses version from package.json)
npm run package:clean    # validates packaging in disposable state and leaves no artifact
npm run package:validate # same self-cleaning packaging validation used by automation
```

**Local install:**

```sh
code --install-extension yawr-preview.vsix
```

Uninstall: `code --uninstall-extension ormasoftchile.yawr-preview`

Marketplace publication and coexistence validation are deferred; this repository
only produces the local `ormasoftchile.yawr-preview` package identity.

The CI workflow `.github/workflows/ci.yml` validates packaging in disposable
state and then installs and exercises the resulting VSIX in an isolated
Extension Host profile.

## See also

- [Yawr](https://github.com/ormasoftchile/yawr) — the runbook engine, server,
  CLI, and extension monorepo.
- This extension carries no `yawr.runbook/v1` JSON Schema and no member-validation
  logic of its own. Every file is parsed and validated by the real `yawr`
  runtime, and enum acceptance or rejection is always the engine's verdict.
