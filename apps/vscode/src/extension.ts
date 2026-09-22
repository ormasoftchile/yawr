// VS Code extension entry point.
//
// Registers three commands:
//
//   yawr.preview        — runs `yawr preview --format prose <activeFile>`
//                         and opens the result in a Markdown preview pane.
//                         Works fully offline; no server needed.
//
//   yawr.previewGraph   — runs `yawr preview --format graphjson` and renders
//                         the static structure in a bundled React Flow webview.
//
//   yawr.validateInputs — collects a value for each declared runbook input
//                         (a closed selector for enum-constrained inputs, a
//                         mandatory free-text fallback otherwise — see
//                         src/enumInputs.ts) and runs `yawr dry-run` against
//                         the real CLI so ENUM-0xx errors and ENUM-W001
//                         warnings are the engine's own, verbatim, never a
//                         client-side reimplementation.

import * as vscode from 'vscode';
import { execFile, spawn } from 'child_process';
import { promisify } from 'util';
import * as path from 'path';
import { randomBytes, randomUUID } from 'crypto';
import { mergeExecutionGraph } from './executionGraph';
import { configureBundledRuntime, resolveBinary } from './binaryResolver';
import { registerPresentationEditor, setPresentationEntrypoint } from './presentationEditor';
import { registerAuthoringEditor } from './authoringEditor';
import { presentationProjectRoot } from './presentationContext';
import { bundledPresentationHelper, requireCompatibleExecution, resolvePresentation, verifyPresentationHelper, usesTypedResults } from './presentationClient';
import { McpBridge } from './mcpBridge';
import { buildRegistryForRun, buildRegistryFromDir } from './toolDefinitionRegistry';
import { pickProjectRoot } from './projectRoot';
import { setToolToken, getToolToken, clearToolToken } from './toolTokenStore';
import { isArmCommand } from './chatParticipantGate';
import { executeRunHandoff } from './runHandoff';
import { resolveRunPackageMapPath } from './runHandoff';
import { DirectRunSession, RunChildProcess, buildStdioRunArgs } from './directRunSession';
import {
  SessionStdioClient,
  buildSessionAttachArgs,
  buildSessionGraphArgs,
  buildSessionStartArgs,
  withSessionPackageMap,
  type SessionCommandRequest,
} from './sessionStdioClient';
import { SessionGraphModel, sessionVisualSteps, type SessionGraphViewState } from './sessionCompositeGraph';
import {
  SESSION_GRAPH_CACHE_SCHEMA,
  SESSION_WORKSPACE_STATE_KEY,
  STORED_SESSION_SCHEMA,
  CoalescedAsyncWriter,
  SessionGraphCacheStore,
  parseSessionGraphRevisionResponse,
  parseStoredSessionDescriptor,
  recoverySequence,
  type CachedSegmentGraph,
  type StoredSessionDescriptor,
  type StoredSessionGraphCache,
} from './sessionPanelState';
import { parseDirectDebugConfig, validateDirectDebugTargets } from './directDebug';
import {
  HostActionHandler,
  HostActionHandlerArgs,
  HostActionResult,
  HostActionAckEnvelope,
  HostActionRegistration,
  createHostActionBridge,
  webviewPanelTransport,
  testEchoHandler,
} from './hostActionBridge';
import {
  createDirectGraphWebviewHtml,
  graphMayRequireMcpBridge,
  GraphDocument,
  loadGraphDocument,
  sessionMayRequireMcpBridge,
} from './directGraphPreview';
import { graphSourceChanged } from './graphSourceChanged';
import { WORKSPACE_RUNBOOK_KEY, resolveRunbookPath, isRunbookPath } from './panelRecovery';
import { affectsSetting, runtimeEnvironment, getSetting } from './identity';
import { minimumStepDisplayMs } from './visualStepPacer';
import { resolvePreviewPanelTarget } from './previewPlacement';
import {
  loadRouteTestArtifacts,
  parseRouteTestArtifact,
  saveRouteTestArtifact,
  stampRouteTestResultDigest,
  validateRouteTestAgainstDocument,
} from './routeTestArtifacts';
import type { RouteTestArtifact } from './routeTestTypes';
import { launchExternalViewWithHandoff, externalViewParameterArguments } from './externalViewHandoff';
import { waitForExternalViewVerification } from './externalViewVerification';
import {
  CANCELLED,
  UNSET,
  InputDecl,
  chooseAffordance,
  extractInputDecls,
  nfcEquals,
  stderrOf,
  deriveFailureMessage,
  firstLine,
  warningLines,
} from './enumInputs';
import {
  discoverSavedRuns,
  loadSavedRunState,
  resolveRunIdentityFromPath,
  type LoadedSavedRun,
  type SavedRunSummary,
} from './savedRunLoader';

const pexec = promisify(execFile);

async function requireRunbookCompatibility(
  binary: string, document: GraphDocument, runbookPath: string, projectRoot: string, packageMapPath?: string,
): Promise<void> {
  if (/^\.env(?:\.|$)/i.test(path.basename(runbookPath))) throw new Error('A runbook path is required for compatibility verification.');
  const uri = vscode.Uri.file(runbookPath);
  const stat = await vscode.workspace.fs.stat(uri);
  if (stat.size > 8 * 1024 * 1024) throw new Error('Runbook metadata exceeds the local compatibility-check limit.');
  const text = Buffer.from(await vscode.workspace.fs.readFile(uri)).toString('utf8');
  const helper = bundledPresentationHelper(extensionContext!.extensionPath);
  const metadata = await resolvePresentation(helper, {
    schema_version: 'yawr.presentation-resolve/v1', request_id: `execution-preflight:${randomUUID()}`,
    context: { project_root: projectRoot, generation: 0, ...(packageMapPath ? { package_map_path: packageMapPath } : {}) },
    document: { uri: uri.toString(), path: runbookPath, version: 0, text }, overlays: [],
  });
  await requireCompatibleExecution(binary, document, metadata);
}

let output: vscode.OutputChannel | null = null;
// ExtensionContext is stored at module scope so workspace state can be
// accessed from previewGraph without threading
// context through every call site.
let extensionContext: vscode.ExtensionContext | null = null;
let directGraphPanel: vscode.WebviewPanel | undefined;
let activeLoadSavedRun: ((runID?: string, runDir?: string) => Promise<void>) | undefined;
let mcpBridge: McpBridge | null = null;
let mcpBridgeStarting: Promise<McpBridge> | null = null;

interface DirectGraphTestHooks {
  documentLoader?: () => Promise<GraphDocument>;
  beforeSpawn?: () => Promise<void>;
  onStartSettled?: () => void;
  onHostActionAck?: (ack: HostActionAckEnvelope) => void;
  showExternalViewReminder?: () => void;
  spawnRun?: (
    binary: string,
    args: string[],
    options: Parameters<typeof spawn>[2],
  ) => ReturnType<typeof spawn>;
  spawnSession?: (
    binary: string,
    args: string[],
    options: Parameters<typeof spawn>[2],
  ) => ReturnType<typeof spawn>;
  loadSavedRun?: (runDir?: string, runID?: string) => Promise<LoadedSavedRun | undefined>;
}

interface ProductionRunResult {
  extensionPath: string;
  binary: string;
  args: string[];
  cwd: string;
  frames: Array<Record<string, unknown>>;
  stderr: string;
  finished: Record<string, unknown>;
}

interface ProductionRunRequest {
  inputs: Record<string, string>;
  resolve(result: ProductionRunResult): void;
  reject(error: Error): void;
}

interface DirectRouteTestOutcome {
  passed: boolean;
  targetReached: boolean;
  externalDispatches: number;
  status: 'reached' | 'route-changed' | 'runtime-failed' | 'safety-failed' | 'stopped';
  message?: string;
}

function parseDirectRouteTestOutcome(value: unknown): DirectRouteTestOutcome | undefined {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined;
  const outcome = value as Record<string, unknown>;
  const statuses = new Set(['reached', 'route-changed', 'runtime-failed', 'safety-failed', 'stopped']);
  if (typeof outcome.passed !== 'boolean' || typeof outcome.targetReached !== 'boolean' ||
      !Number.isInteger(outcome.externalDispatches) || (outcome.externalDispatches as number) < 0 ||
      typeof outcome.status !== 'string' || !statuses.has(outcome.status)) return undefined;
  return {
    passed: outcome.passed,
    targetReached: outcome.targetReached,
    externalDispatches: outcome.externalDispatches as number,
    status: outcome.status as DirectRouteTestOutcome['status'],
    ...(typeof outcome.message === 'string' && outcome.message ? { message: outcome.message } : {}),
  };
}

function ensureMcpBridge(): Promise<McpBridge> {
  if (mcpBridge) return Promise.resolve(mcpBridge);
  if (mcpBridgeStarting) return mcpBridgeStarting;
  if (!output || !extensionContext) {
    return Promise.reject(new Error('Yawr extension is not active'));
  }

  mcpBridgeStarting = createMcpBridge({}, undefined).then((bridge) => {
    mcpBridge = bridge;
    return bridge;
  }).finally(() => {
    mcpBridgeStarting = null;
  });
  return mcpBridgeStarting;
}

function createMcpBridge(
  registry: Record<string, import('./mcpBridge').ToolActionSpec>,
  resource: vscode.Uri | undefined,
): Promise<McpBridge> {
  if (!output || !extensionContext) {
    return Promise.reject(new Error('Yawr extension is not active'));
  }
  const outputChannel = output;
  return McpBridge.create({
    get tools() { return vscode.lm.tools as unknown as readonly import('./mcpBridge').LmToolInfo[]; },
    getToolInvocationToken() { return getToolToken(); },
    onTokenRejected() { clearToolToken(); },
    invokeTool(name, options, token) {
      return vscode.lm.invokeTool(
        name,
        { input: options.input, toolInvocationToken: options.toolInvocationToken as never },
        token as vscode.CancellationToken,
      ) as Promise<import('./mcpBridge').LmToolResult>;
    },
  }, 0, outputChannel, {
    registry,
    overrides: getSetting('mcpBridge.toolNameOverrides', resource, {} as Record<string, string>),
  }).then((bridge) => {
    if (!extensionContext) {
      bridge.dispose();
      throw new Error('Yawr extension was deactivated during MCP bridge startup');
    }
    outputChannel.appendLine(`[Yawr] MCP bridge listening at ${bridge.bridgeUrl}`);
    return bridge;
  }).catch((error: unknown) => {
    const message = error instanceof Error ? error.message : String(error);
    outputChannel.appendLine(`[Yawr] WARNING: MCP bridge failed to start — ${message}`);
    throw error;
  });
}

export async function activate(context: vscode.ExtensionContext) {
  extensionContext = context;
  configureBundledRuntime(context.extensionPath);
  output = vscode.window.createOutputChannel('Yawr');
  context.subscriptions.push(registerPresentationEditor(context, output));
  context.subscriptions.push(registerAuthoringEditor(context));

  // Chat participant — drives runbooks or captures token for MCP discovery.
  // /run   — runs a runbook in-handler, keeping the handler open until terminal.
  //          Accesses request.toolInvocationToken (causes VS Code to discover
  //          MCP servers, ~60s on first use) and uses it live inside the handler.
  // /arm-mcp — diagnostic only: captures the token to trigger MCP server
  //          discovery, but the token does NOT authorize deferred runs.
  const chatHandler: vscode.ChatRequestHandler = async (request, _ctx, response, _token) => {
      if (isArmCommand(request.command)) {
        setToolToken(request.toolInvocationToken);
        try {
          await ensureMcpBridge();
        } catch (error) {
          response.markdown(`❌ **MCP bridge failed to start:** ${firstLine(deriveFailureMessage(error))}`);
          return {};
        }
        // Diagnostic: list tools visible in vscode.lm.tools right now.
        // Accessing toolInvocationToken triggers MCP server discovery, so this
        // snapshot reflects the state immediately after the discovery signal.
        // Zero invoke budget — no invokeTool calls.
        const visibleTools = (vscode.lm.tools as unknown as readonly { name: string }[])
          .map((t) => t.name)
          .sort();
        const toolsSummary = visibleTools.length === 0
          ? '_No tools visible yet — VS Code may still be starting MCP servers (~60s on ' +
            'first use). Run `/arm-mcp` again after ~60s to see them._'
          : visibleTools.map((n) => `- \`${n}\``).join('\n');
        response.markdown(
          'ℹ️ **Token armed — MCP dialog suppression enabled.**\n\n' +
          'Accessing `request.toolInvocationToken` causes VS Code to auto-discover ' +
          'and start MCP servers (~60s on first use).\n\n' +
          'The cached token is passed as `toolInvocationToken` on MCP tool calls. ' +
          'If VS Code rejects or cancels that token path, the bridge fails closed ' +
          'instead of retrying without a token. This token is NEVER an authorization credential.\n\n' +
          '**Re-arm when needed:** The token is cleared on rejection. ' +
          'Run `/arm-mcp` again if calls fail with `invocation_token_unavailable`.\n\n' +
          '**VS Code MCP tools visible right now:**\n\n' +
          toolsSummary,
        );
        return {};
      }

      if (request.command === 'run') {
        // Access toolInvocationToken first — triggers VS Code to start MCP servers
        // (~60s on first use) and caches the token for bridge invocation consent.
        // Store unconditionally; the bridge fails closed if VS Code rejects it.
        setToolToken(request.toolInvocationToken);

        const prompt = request.prompt.trim();
        if (!prompt) {
          response.markdown(
            '**Yawr /run**: runbook path required.\n\n' +
            'Usage: `@yawr /run <path/to/runbook.yaml> [key=val ...]`',
          );
          return {};
        }

        // First token is the runbook path; remainder are key=val pairs for --var.
        const [runbookArg, ...varPairArgs] = prompt.split(/\s+/);
        const runbookPath = path.isAbsolute(runbookArg)
          ? runbookArg
          : path.resolve(
              vscode.window.activeTextEditor?.document.uri.fsPath
                ? path.dirname(vscode.window.activeTextEditor.document.uri.fsPath)
                : process.cwd(),
              runbookArg,
            );

        const resource = vscode.Uri.file(runbookPath);
        const packageMapSetting = getSetting('packageMap', resource, '');
        const folders = (vscode.workspace.workspaceFolders ?? []).map(folder => folder.uri.fsPath);
        const projectRoot = pickProjectRoot(runbookPath, folders, path.dirname(runbookPath));
        let bin: string;
        try {
          bin = await resolveBinary(getSetting('binaryPath', resource, 'yawr'), output!, projectRoot, folders);
          const presentationBinary = bundledPresentationHelper(context.extensionPath);
          await verifyPresentationHelper(presentationBinary);
          const graph = await loadGraphDocument(presentationBinary, runbookPath, (command, args) => pexec(command, args, { cwd: projectRoot, maxBuffer: 16 * 1024 * 1024 }),
            resolveRunPackageMapPath(projectRoot, packageMapSetting).path);
          await requireRunbookCompatibility(bin, graph, runbookPath, projectRoot, resolveRunPackageMapPath(projectRoot, packageMapSetting).path);
          await ensureMcpBridge();
        } catch (error) {
          reportEngineFailure('Runtime compatibility or MCP bridge startup', error);
          response.markdown('❌ **Yawr run could not start: check runtime compatibility and bridge availability in the run log.**');
          return {};
        }

        // Refresh registry so the bridge dispatches against this runbook's tools.
        refreshBridgeRegistry(runbookPath);

        const bridgeVars: Record<string, string> = mcpBridge
          ? {
              ...runtimeEnvironment('VSCODE_BRIDGE_URL', mcpBridge.bridgeUrl),
              ...runtimeEnvironment('VSCODE_BRIDGE_TOKEN', mcpBridge.bridgeToken),
            }
          : {};

        try {
          const result = await executeRunHandoff({
            bin,
            runbookPath,
            varPairArgs,
            projectRoot,
            packageMapSetting,
            bridgeVars,
            baseEnv: process.env,
            execFile,
          });
          if (result.packageMap.warning) output?.appendLine(`[yawr run] WARNING: ${result.packageMap.warning}`);
          if (result.stderr.trim()) output?.appendLine(`[yawr run] ${result.stderr.trim()}`);
          response.markdown(result.stdout.trim() || '✅ Runbook completed.');
        } catch (err: unknown) {
          const withOutput = err as { stderr?: string; packageMap?: { warning?: string } };
          if (withOutput.packageMap?.warning) output?.appendLine(`[yawr run] WARNING: ${withOutput.packageMap.warning}`);
          if (withOutput.stderr?.trim()) output?.appendLine(`[yawr run] ${withOutput.stderr.trim()}`);
          reportEngineFailure('yawr run', err);
          response.markdown(
            '❌ **Yawr run failed** — see the `Yawr` output channel for details.',
          );
        }
        return {};
      }

      response.markdown(
        '**Yawr**: Unknown command.\n\n' +
        'Commands:\n' +
        '- `/arm-mcp` — optional: capture a token for MCP dialog suppression\n' +
        '- `/run <runbook.yaml> [key=val ...]` — run a runbook live\n',
      );
      return {};
    };
  const participant = vscode.chat.createChatParticipant('yawr.chat', chatHandler);
  participant.iconPath = new vscode.ThemeIcon('run');

  context.subscriptions.push(
    output,
    { dispose: () => { mcpBridge?.dispose(); mcpBridge = null; } },
    participant,
    vscode.commands.registerCommand('yawr.preview', () => previewProse()),
    vscode.commands.registerCommand('yawr.previewGraph', () => previewGraph()),
    vscode.commands.registerCommand('yawr.loadRun', (runID?: string, runDir?: string) => {
      if (activeLoadSavedRun) {
        return activeLoadSavedRun(typeof runID === 'string' ? runID : undefined, typeof runDir === 'string' ? runDir : undefined);
      }
      return previewGraph().then(() => {
        if (activeLoadSavedRun) {
          return activeLoadSavedRun(typeof runID === 'string' ? runID : undefined, typeof runDir === 'string' ? runDir : undefined);
        }
      });
    }),
    vscode.commands.registerCommand('yawr.runCurrentRunbook', () => runCurrentRunbook()),
    ...(context.extensionMode === vscode.ExtensionMode.Test
      ? [
          vscode.commands.registerCommand(
            'yawr.test.openDirectGraphPanel',
            (runbookPath: string, hooks?: DirectGraphTestHooks) => openDirectGraphPanelForRunbook(runbookPath, hooks),
          ),
          vscode.commands.registerCommand(
            'yawr.test.clearInvestigationSession',
            async () => {
              const stored = extensionContext?.workspaceState.get<unknown>(SESSION_WORKSPACE_STATE_KEY);
              await extensionContext?.workspaceState.update(SESSION_WORKSPACE_STATE_KEY, undefined);
              try {
                const descriptor = parseStoredSessionDescriptor(stored);
                const cache = new SessionGraphCacheStore(
                  vscode.Uri.joinPath(extensionContext!.globalStorageUri, 'investigation-sessions').fsPath,
                );
                await cache.delete(descriptor.sessionID);
              } catch {
                // Missing or invalid test state requires no cache cleanup.
              }
            },
          ),
          vscode.commands.registerCommand('yawr.test.getRuntimeState', () => ({
            mcpBridgeStarted: mcpBridge !== null || mcpBridgeStarting !== null,
          })),
        ]
      : []),
    vscode.commands.registerCommand('yawr.validateInputs', () => validateInputs()),
    vscode.commands.registerCommand('yawr.showServerLog', () => output?.show(true)),
    ...(context.extensionMode === vscode.ExtensionMode.Test
      ? [vscode.commands.registerCommand(
          'yawr.test.openHostActionPanel',
          async (): Promise<vscode.WebviewPanel> => {
        let disposed = false;
        const panel = vscode.window.createWebviewPanel(
          'yawrTestHostAction',
          'yawr: host-action test harness',
          vscode.ViewColumn.One,
          { enableScripts: true, retainContextWhenHidden: true },
        );
        panel.webview.html = HOST_ACTION_TEST_HTML;

        // Production capability registry — identical to the one in previewGraph().
        const testRegistry = new Map<string, HostActionRegistration>([
          ['test.echo', { handler: testEchoHandler }],
          ['external-view.open', { handler: makeExternalViewOpenHandler(panel), timeoutMs: EXTERNAL_VIEW_HOST_ACTION_TIMEOUT_MS }],
        ]);
        const testBridge = createHostActionBridge(
          testRegistry,
          webviewPanelTransport(panel, () => !disposed),
        );
        panel.onDidDispose(() => { disposed = true; testBridge.dispose(); });

        // Production onDidReceiveMessage wiring — same pattern as previewGraph().
        panel.webview.onDidReceiveMessage((message) => { void testBridge.receive(message); });

        // Wait for the webview harness to signal it is ready before returning
        // the panel to the test. This prevents a race where postMessage is called
        // before the webview script is loaded.
        await new Promise<void>((resolve, reject) => {
          const t = setTimeout(
            () => reject(new Error('yawr.test.openHostActionPanel: webview not ready within 10 s')),
            10_000,
          );
          const sub = panel.webview.onDidReceiveMessage((msg) => {
            if (msg && (msg as Record<string, unknown>).type === 'yawr.test.ready') {
              clearTimeout(t);
              sub.dispose();
              resolve();
            }
          });
          panel.onDidDispose(() => {
            clearTimeout(t);
            sub.dispose();
            reject(new Error('panel disposed before yawr.test.ready'));
          });
        });

            return panel;
          },
        )]
      : []),
  );
}

// HOST_ACTION_TEST_HTML is the webview content for the test-only
// yawr.test.openHostActionPanel command. It provides two services:
//
//  1. Signals yawr.test.ready when the script loads (so the command can
//     await the webview before returning the panel to the test).
//
//  2. Forwards yawr.test.inject payloads as vscode.postMessage() calls,
//     which triggers the production onDidReceiveMessage handler.
//     This is the key boundary: the webview calls vscode.postMessage(),
//     NOT the test calling bridge.receive() directly.
//
//  3. Echoes any yawr.host-action.ack/cancel received from the bridge
//     back to the extension as yawr.test.ackCaptured so the test can
//     assert on it and then POST it to the Go broker.
const HOST_ACTION_TEST_HTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="UTF-8">
  <meta http-equiv="Content-Security-Policy"
        content="default-src 'none'; script-src 'unsafe-inline';">
</head>
<body>
<script>
  (function () {
    var vscode = acquireVsCodeApi();
    vscode.postMessage({ type: 'yawr.test.ready' });
    window.addEventListener('message', function (event) {
      var msg = event.data;
      if (!msg || typeof msg !== 'object') { return; }
      if (msg.type === 'yawr.test.inject') {
        vscode.postMessage(msg.payload);
      } else if (msg.type === 'yawr.host-action.ack' || msg.type === 'yawr.host-action.cancel') {
        vscode.postMessage({ type: 'yawr.test.ackCaptured', ack: msg });
      }
    });
  }());
</script>
</body>
</html>`;

export function deactivate() {
  clearToolToken();
  mcpBridge?.dispose();
  mcpBridge = null;
  extensionContext = null;
}

// refreshBridgeRegistry rebuilds the MCP bridge registry from the active
// runbook's resolved project so the bridge always dispatches against the
// correct set of tool definitions, regardless of workspace folder order.
// Multi-root: a runbook in the second folder correctly selects that folder's
// project rather than workspaceFolders[0].
function refreshBridgeRegistry(runbookPath: string): void {
  if (!mcpBridge) return;
  const folders = (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);
  const projectRoot = pickProjectRoot(runbookPath, folders, path.dirname(runbookPath));
  const registry = buildRegistryFromDir(projectRoot);
  mcpBridge.updateRegistry(registry);
  output?.appendLine(`[yawr] MCP bridge registry refreshed from ${projectRoot}`);
}

// previewProse runs the yawr CLI with --format prose against the active
// runbook file and opens the rendered Markdown in a side-by-side preview.
async function previewProse() {
  const editor = vscode.window.activeTextEditor;
  if (!editor || !isRunbookPath(editor.document.fileName)) {
    void vscode.window.showWarningMessage('Open a *.runbook.yaml or *.yawr file first.');
    return;
  }
  const runbookPath = editor.document.fileName;
  refreshBridgeRegistry(runbookPath);
  // binaryPath is folder-scoped: different projects may use different local
  // yawr builds. runbookPath is in scope here so there is no obstacle to
  // reading from the correct folder.
  const bin = getSetting('binaryPath', vscode.Uri.file(runbookPath), 'yawr');
  try {
    const { stdout } = await pexec(bin, ['preview', '--format', 'prose', runbookPath]);
    const doc = await vscode.workspace.openTextDocument({ content: stdout, language: 'markdown' });
    await vscode.window.showTextDocument(doc, { viewColumn: vscode.ViewColumn.Beside, preview: true });
    await vscode.commands.executeCommand('markdown.showPreview', doc.uri);
  } catch (err) {
    reportEngineFailure('yawr preview', err);
  }
}

// validateInputs runs `yawr dry-run` against the active runbook so
// declared inputs (including `enum`-constrained ones) are checked through
// the real CLI/engine path — never a client-side re-implementation.
// Declared-input metadata is read from the current preview document's
// `inputs[]` array: a closed selector is offered for each
// enum-constrained input, in declared order, with no auto-select and no
// client-side normalisation. Redacted enum metadata uses free text and the
// engine adjudicates the value.
async function validateInputs() {
  const editor = vscode.window.activeTextEditor;
  if (!editor || !isRunbookPath(editor.document.fileName)) {
    void vscode.window.showWarningMessage('Open a *.runbook.yaml or *.yawr file first.');
    return;
  }
  const file = editor.document.fileName;
  // binaryPath is folder-scoped: same reasoning as previewProse.
  const bin = getSetting('binaryPath', vscode.Uri.file(file), 'yawr');
  refreshBridgeRegistry(file);

  let doc: unknown;
  try {
    const { stdout } = await pexec(bin, ['preview', '--format', 'graphjson', file]);
    doc = JSON.parse(stdout);
  } catch (err) {
    reportEngineFailure('yawr preview', err);
    return;
  }

  let vars: Record<string, string> | undefined;
  try {
    vars = await collectInputs(doc);
  } catch (err) {
    reportEngineFailure('yawr preview', err);
    return;
  }
  if (vars === undefined) return; // operator cancelled a prompt

  // NOTE: must be `--var`, not `-var` — `yawr`'s own arg splitter
  // (`cmd/yawr/run.go:splitRunArgs`) only recognises the double-dash form
  // when attaching the following token as the flag's value; `-var` silently
  // becomes a bare flag and the CLI rejects it with "flag needs an
  // argument: -var" before ever reaching validation. Verified against the
  // real `yawr` binary while building this command.
  const varArgs = Object.entries(vars).flatMap(([k, v]) => ['--var', `${k}=${v}`]);
  try {
    const { stdout, stderr } = await pexec(bin, ['dry-run', ...varArgs, file]);
    surfaceWarnings(stderr);
    if (stdout.trim()) output?.appendLine(stdout.trim());
    void vscode.window.showInformationMessage('Yawr: runbook inputs are valid.');
  } catch (err) {
    surfaceWarnings(stderrOf(err));
    reportEngineFailure('yawr dry-run', err);
  }
}

// collectInputs prompts for a value per declared input (selector for
// non-redacted enum, free text otherwise). Returns undefined if the operator
// cancelled.
async function collectInputs(doc: unknown): Promise<Record<string, string> | undefined> {
  const decls = extractInputDecls(doc);

  const vars: Record<string, string> = {};
  for (const decl of decls) {
    const result = await promptForInput(decl);
    if (result === CANCELLED) return undefined;
    if (result !== UNSET) vars[decl.name] = result;
  }
  return vars;
}

// promptForInput renders exactly one of: a redacted free-text box, a
// closed QuickPick selector over declared enum members (in declared
// order), or a plain free-text box. The selected/typed value is returned
// verbatim — no trim, case-fold, or NFC-normalisation (AR-CE-5 §1/§2).
async function promptForInput(
  decl: InputDecl,
): Promise<string | typeof UNSET | typeof CANCELLED> {
  const affordance = chooseAffordance(decl);
  const requiredLabel = decl.required ? ' (required)' : ' (optional)';

  if (affordance.kind === 'redacted-freetext') {
    const value = await vscode.window.showInputBox({
      prompt: `${decl.name}${requiredLabel} — ${affordance.hint}`,
      ignoreFocusOut: true,
    });
    return value === undefined ? CANCELLED : value;
  }

  if (affordance.kind === 'selector') {
    type Item = vscode.QuickPickItem & { value: string | undefined };
    const items: Item[] = affordance.members.map((m) => ({
      label: m,
      description:
        affordance.preselect !== undefined && nfcEquals(m, affordance.preselect) ? '(default)' : undefined,
      value: m,
    }));
    if (affordance.allowUnset) {
      items.push({ label: '$(circle-slash) Leave unset', value: undefined });
    }

    const qp = vscode.window.createQuickPick<Item>();
    qp.title = `${decl.name}${requiredLabel}`;
    qp.placeholder = decl.description ?? 'Select a declared value';
    qp.ignoreFocusOut = true;
    qp.items = items;
    // Preselecting only highlights the default item (cursor position);
    // it does not choose it. A required input with no default opens with
    // nothing highlighted, and in all cases the operator must still press
    // Enter — nothing here auto-submits (AR-CE-3 §5).
    if (affordance.preselect !== undefined) {
      const active = items.find((it) => it.value !== undefined && nfcEquals(it.value, affordance.preselect!));
      if (active) qp.activeItems = [active];
    }

    const picked = await new Promise<Item | undefined>((resolve) => {
      qp.onDidAccept(() => {
        resolve(qp.selectedItems[0]);
        qp.hide();
      });
      qp.onDidHide(() => {
        resolve(undefined);
        qp.dispose();
      });
      qp.show();
    });
    if (!picked) return CANCELLED;
    return picked.value === undefined ? UNSET : picked.value;
  }

  const value = await vscode.window.showInputBox({
    prompt: `${decl.name}${requiredLabel}${decl.description ? ' — ' + decl.description : ''}`,
    value: affordance.defaultValue,
    ignoreFocusOut: true,
  });
  return value === undefined ? CANCELLED : value;
}

// surfaceWarnings relays every `yawr: warning: ...` stderr line (e.g.
// ENUM-W001, AR-CE-4 §6) to the user and the output channel, verbatim and
// with its code intact. Warnings are always non-fatal; this never blocks
// or fails the calling command.
function surfaceWarnings(stderrText: string) {
  for (const line of warningLines(stderrText)) {
    output?.appendLine(line);
    void vscode.window.showWarningMessage(line.replace(/^yawr: warning:\s*/, ''));
  }
}

// reportEngineFailure surfaces the engine's own error text verbatim,
// including its ENUM-0xx (or other) code, instead of paraphrasing it into
// a generic failure string. Preferring the raw stderr capture over
// child_process's combined `err.message` keeps a coded error from being
// buried under a "Command failed: <argv>" prefix (AR-CE-4 §4/§5; D-3
// regression: an engine-coded error must never present as a bare "Parse
// error" to the operator).
function reportEngineFailure(step: string, err: unknown) {
  const verbatim = deriveFailureMessage(err);
  output?.appendLine(`[yawr] ${step} failed:\n${verbatim}`);
  void vscode.window.showErrorMessage(`${step}: ${firstLine(verbatim)}`, 'Show log').then((sel) => {
    if (sel === 'Show log') output?.show(true);
  });
}

// External view capability handler factory. The host validates and forwards an arbitrary
// view request; product-owned view paths and parameters stay in runbooks.

const EXTERNAL_VIEW_HOST_ACTION_TIMEOUT_MS = 310_000;

function makeExternalViewOpenHandler(
  panel: vscode.WebviewPanel,
  consumePanelConfirmation: (requestId: string) => boolean = () => false,
  showReminder: () => void = () => {
    void vscode.window.showInformationMessage(
      'External view is open. Review the view, then return to the Yawr preview to record your finding.',
    );
  },
): HostActionHandler {
  return async (args: HostActionHandlerArgs): Promise<HostActionResult> => {
    const req = args.request;
    if (typeof req !== 'object' || req === null || Array.isArray(req)) {
      return { status: 'failed', error: { code: 'INVALID_REQUEST', message: 'request must be an object' } };
    }
    const r = req as Record<string, unknown>;
    const viewPath = typeof r.view_path === 'string' && r.view_path.length > 0 ? r.view_path : undefined;
    const environment = typeof r.environment === 'string' && r.environment.length > 0 ? r.environment : undefined;
    const focus = typeof r.focus === 'boolean' ? r.focus : undefined;
    const params = typeof r.parameters === 'object' && r.parameters !== null && !Array.isArray(r.parameters)
      ? r.parameters as Record<string, unknown> : undefined;
    if (viewPath === undefined || environment === undefined || focus === undefined || params === undefined) {
      return { status: 'failed', error: { code: 'INVALID_REQUEST', message: 'Missing required view_path, environment, focus, or parameters fields.' } };
    }
    let parameters: string;
    try {
      parameters = externalViewParameterArguments(environment, params);
    } catch (error) {
      return { status: 'failed', error: { code: 'INVALID_REQUEST', message: deriveFailureMessage(error) } };
    }
    const panelConfirmed = consumePanelConfirmation(args.requestId);
    if (!panelConfirmed) {
      return {
        status: 'execution-not-started',
        error: { code: 'CONFIRMATION_REQUIRED', message: 'Confirm the external view launch in the Yawr preview.' },
      };
    }
    return launchExternalViewWithHandoff(
      focus,
      args.cancellationToken,
      {
        dispatch: async () => {
          // window-scoped: external view dispatch is configured for the active window
          const config = vscode.workspace.getConfiguration('yawr');
          const command = config.get<string>('externalView.openCommand', 'externalView.openByPath');
          const extensionId = config.get<string>('externalView.extensionId', '');
          if (extensionId) {
            const ext = vscode.extensions.getExtension(extensionId);
            if (ext && !ext.isActive) await ext.activate();
          }
          if (args.cancellationToken.isCancellationRequested) return;
          await vscode.commands.executeCommand(command, viewPath, parameters);
        },
        verifyView: () => waitForExternalViewVerification(args, panel.webview),
        showDispatchError: (message) => {
          void vscode.window.showErrorMessage(`Yawr could not open the external view: ${message}`);
        },
        showReminder,
      },
    );
  };
}

// previewGraph renders graphjson directly in a bundled webview. It does not
// start a background service or load a remote document.
async function previewGraph() {
  const activeEditor = vscode.window.activeTextEditor;
  const runbookPath = resolveRunbookPath(
    activeEditor?.document.fileName,
    extensionContext?.workspaceState.get<string>(WORKSPACE_RUNBOOK_KEY),
  );
  if (!runbookPath) {
    void vscode.window.showWarningMessage('Open a *.runbook.yaml or *.yawr file first.');
    return;
  }
  return openDirectGraphPanelForRunbook(runbookPath);
}

async function runCurrentRunbook(): Promise<ProductionRunResult | undefined> {
  const activeEditor = vscode.window.activeTextEditor;
  const runbookPath = resolveRunbookPath(
    activeEditor?.document.fileName,
    extensionContext?.workspaceState.get<string>(WORKSPACE_RUNBOOK_KEY),
  );
  if (!runbookPath) {
    void vscode.window.showWarningMessage('Open a *.runbook.yaml or *.yawr file first.');
    return;
  }
  return new Promise<ProductionRunResult>((resolve, reject) => {
    void openDirectGraphPanelForRunbook(runbookPath, undefined, {
      inputs: {},
      resolve,
      reject,
    }).catch((error: unknown) => reject(error instanceof Error ? error : new Error(String(error))));
  });
}

function findRunbookViewColumn(runbookPath: string): vscode.ViewColumn | undefined {
  const runbookUri = vscode.Uri.file(runbookPath).toString();
  const activeGroup = vscode.window.tabGroups.activeTabGroup;
  const groups = [
    activeGroup,
    ...vscode.window.tabGroups.all.filter((group) => group !== activeGroup),
  ];
  return groups.find((group) => group.tabs.some((tab) => (
    tab.input instanceof vscode.TabInputText
    && tab.input.uri.toString() === runbookUri
  )))?.viewColumn;
}

function directRunInputs(value: unknown): Record<string, string> {
  if (value === undefined || value === null) return {};
  if (typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('Run inputs must be an object.');
  }
  const entries = Object.entries(value as Record<string, unknown>);
  if (entries.length > 64) throw new Error('Run inputs exceed the 64-field limit.');
  const inputs: Record<string, string> = {};
  for (const [name, raw] of entries) {
    if (!name || Buffer.byteLength(name, 'utf8') > 256) {
      throw new Error('Run input names must be between 1 and 256 UTF-8 bytes.');
    }
    if (typeof raw !== 'string') {
      throw new Error(`Run input ${name} must be a string.`);
    }
    if (Buffer.byteLength(raw, 'utf8') > 64 * 1024) {
      throw new Error(`Run input ${name} exceeds 64 KiB.`);
    }
    inputs[name] = raw;
  }
  return inputs;
}

function directSecretInputNames(document: GraphDocument): Set<string> {
  if (!Array.isArray(document.inputs)) return new Set();
  return new Set(document.inputs.flatMap((value) => {
    if (typeof value !== 'object' || value === null || Array.isArray(value)) return [];
    const input = value as { name?: unknown; type?: unknown };
    return typeof input.name === 'string' && input.type === 'secret' ? [input.name] : [];
  }));
}

function parseRouteTestPlanHash(stdout: string): string {
  let value: unknown;
  try { value = JSON.parse(stdout); } catch { throw new Error('yawr plan did not return valid JSON'); }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error('yawr plan result must be an object');
  const hash = (value as Record<string, unknown>).route_test_hash;
  if (typeof hash !== 'string' || !/^sha256:[a-f0-9]{64}$/.test(hash)) throw new Error('yawr plan did not return a valid route_test_hash');
  return hash;
}

async function openDirectGraphPanelForRunbook(
  runbookPath: string,
  testHooks?: DirectGraphTestHooks,
  productionRun?: ProductionRunRequest,
): Promise<vscode.WebviewPanel> {
  const resource = vscode.Uri.file(runbookPath);
  const runbookViewColumn = findRunbookViewColumn(runbookPath);
  const panelTarget = resolvePreviewPanelTarget(
    getSetting('preview.openLocation', resource, 'sameGroup'),
    runbookViewColumn,
  );
  const panelColumn = panelTarget === 'beside'
    ? vscode.ViewColumn.Beside
    : panelTarget === 'active'
      ? vscode.ViewColumn.Active
      : panelTarget;
  const mediaRoot = vscode.Uri.joinPath(extensionContext!.extensionUri, 'media');

  directGraphPanel?.dispose();
  const panel = vscode.window.createWebviewPanel(
    'yawrPreviewGraph',
    `Yawr: ${path.basename(runbookPath)}`,
    panelColumn,
    {
      enableScripts: true,
      retainContextWhenHidden: true,
      localResourceRoots: [mediaRoot],
    },
  );
  const scriptUri = panel.webview.asWebviewUri(vscode.Uri.joinPath(mediaRoot, 'graph.js')).toString();
  const styleUri = panel.webview.asWebviewUri(vscode.Uri.joinPath(mediaRoot, 'graph.css')).toString();
  panel.webview.html = createDirectGraphWebviewHtml(
    scriptUri,
    styleUri,
    panel.webview.cspSource,
    randomBytes(16).toString('hex'),
    panel.webview.asWebviewUri(vscode.Uri.joinPath(mediaRoot, 'highlighting-worker.js')).toString(),
    getSetting('highlighting.enabled', resource, true),
  );

  let disposed = false;
  let ready = false;
  let loadRevision = 0;
  let loadController: AbortController | undefined;
  let currentStyle = getSetting('preview.nodeStyle', resource, 'smooth-curves');
  const pacingInterval = () => {
    const configured = getSetting<unknown>('preview.minimumStepDisplayMs', resource, 200);
    const interval = minimumStepDisplayMs(configured);
    if (configured !== interval) output?.appendLine(`[yawr preview] Invalid minimumStepDisplayMs; using ${interval}.`);
    return interval;
  };
  let currentDocument: GraphDocument | undefined;
  let sourceDocument: GraphDocument | undefined;
  let retainExecutionGraph = false;
  let currentProjectRoot: string | undefined;
  let currentRunbookRelative: string | undefined;
  let currentPlanHash: string | undefined;
  let runSession: DirectRunSession | undefined;
  let investigationClient: SessionStdioClient | undefined;
  let investigationModel: SessionGraphModel | undefined;
  let investigationDescriptor: StoredSessionDescriptor | undefined;
  let investigationState: SessionGraphViewState | undefined;
  let investigationGraphs: Record<string, CachedSegmentGraph> = {};
  let investigationGraphHistory: Record<string, Record<string, CachedSegmentGraph>> = {};
  let investigationRevision = 0;
  let investigationStarting = false;
  const investigationGraphLoads = new Map<string, Promise<SessionGraphViewState | undefined>>();
  let runBridge: McpBridge | undefined;
  let runStarting = false;
  let runStartRevision = 0;
  let activeHostActionRunID: string | undefined;
  let reloadPending = false;
  let activeRouteTest: { revision: number; artifact: RouteTestArtifact; outcome?: DirectRouteTestOutcome } | undefined;
  let productionRunStarted = false;
  let productionRunSettled = false;
  let productionLaunch: { binary: string; args: string[]; cwd: string } | undefined;
  const productionFrames: Array<Record<string, unknown>> = [];
  let productionStderr = '';
  const rejectProductionRun = (error: unknown) => {
    if (!productionRun || productionRunSettled) return;
    productionRunSettled = true;
    productionRun.reject(error instanceof Error ? error : new Error(String(error)));
  };
  let latestMessage: { type: string; [key: string]: unknown } = { type: 'loading' };
  const sessionCache = new SessionGraphCacheStore(
    vscode.Uri.joinPath(extensionContext!.globalStorageUri, 'investigation-sessions').fsPath,
  );
  const publish = (message: { type: string; [key: string]: unknown }) => {
    latestMessage = message;
    if (ready && !disposed) {
      void panel.webview.postMessage(message);
    }
  };
  const publishReloadState = (active: boolean) => {
    if (ready && !disposed) {
      void panel.webview.postMessage({ type: 'graph.reload-state', active });
    }
  };
  const reportInvestigationError = (message: string) => {
    output?.appendLine(`[yawr session] ${message}`);
    if (ready && !disposed) void panel.webview.postMessage({ type: 'session.error', message });
  };
  const closedInvestigationStatus = (status: string | undefined) => (
    status === 'resolved' || status === 'escalated' || status === 'cancelled' || status === 'abandoned'
  );
  const checkpointWriter = new CoalescedAsyncWriter<{
    cache: StoredSessionGraphCache;
    descriptor: StoredSessionDescriptor;
  }>(async ({ cache, descriptor }) => {
    await sessionCache.save(cache);
    if (disposed || investigationDescriptor?.sessionID !== descriptor.sessionID) return;
    await extensionContext!.workspaceState.update(SESSION_WORKSPACE_STATE_KEY, descriptor);
    if (investigationDescriptor?.sessionID === descriptor.sessionID) investigationDescriptor = descriptor;
  }, (error) => reportInvestigationError(`Could not persist session checkpoint: ${deriveFailureMessage(error)}`));
  const persistInvestigationCheckpoint = (state: SessionGraphViewState, acceptedSequence: number) => {
    if (!state.manifest || !investigationDescriptor || state.sequence !== acceptedSequence) {
      reportInvestigationError('Accepted session cursor does not match the projected checkpoint.');
      return;
    }
    const descriptor = { ...investigationDescriptor, acceptedSequence };
    const cache: StoredSessionGraphCache = {
      schemaVersion: SESSION_GRAPH_CACHE_SCHEMA,
      sessionID: descriptor.sessionID,
      sequence: acceptedSequence,
      manifest: state.manifest,
      segmentGraphs: { ...investigationGraphs },
      segmentGraphHistory: Object.fromEntries(Object.entries(investigationGraphHistory).map(
        ([segmentID, revisions]) => [segmentID, { ...revisions }],
      )),
      segmentGraphAvailability: state.segmentGraphAvailability as Record<string, Record<string, import('./sessionCompositeGraph').SessionSegment>>,
      preparedTransitionTargets: state.preparedTransitionTargets,
      runtimeNodes: state.runtimeNodes,
      ...(state.executionNodeID ? { executionNodeID: state.executionNodeID } : {}),
      ...(state.pending ? { pending: state.pending } : {}),
    };
    checkpointWriter.enqueue({ cache, descriptor });
  };
  const publishInvestigationState = (state: SessionGraphViewState, live = false, liveSteps: string[] = []) => {
    const previousDocument = investigationState?.document;
    investigationState = state;
    if (state.document) currentDocument = state.document;
    activeHostActionRunID = state.activeRunID;
    if (ready && !disposed) {
      const publishedState = state.document && state.document === previousDocument
        ? { ...state, document: undefined }
        : state;
      void panel.webview.postMessage({ type: 'session.update', state: publishedState, style: currentStyle, live, liveSteps });
    }
  };
  const ensureInvestigationGraph = (
    segmentID: string,
    revision: number,
  ): Promise<SessionGraphViewState | undefined> => {
    const key = `${segmentID}\u0000${revision}`;
    const existing = investigationGraphLoads.get(key);
    if (existing) return existing;
    const load = (async () => {
      if (!investigationDescriptor || !investigationModel) return undefined;
      if (investigationModel.graphRevision(segmentID, revision)) return investigationModel.snapshot();
      const descriptor = investigationDescriptor;
      const requestRevision = investigationRevision;
      const workspaceFolders = (vscode.workspace.workspaceFolders ?? []).map((folder) => folder.uri.fsPath);
      const configuredBinary = getSetting('binaryPath', resource, 'yawr');
      const binary = await resolveBinary(configuredBinary, output!, descriptor.projectRoot, workspaceFolders);
      if (disposed || requestRevision !== investigationRevision || investigationDescriptor?.sessionID !== descriptor.sessionID) {
        return undefined;
      }
      const { stdout } = await pexec(binary, buildSessionGraphArgs(descriptor.sessionID, segmentID, revision), {
        cwd: descriptor.projectRoot,
        maxBuffer: 192 * 1024 * 1024,
        windowsHide: true,
      });
      const graph = parseSessionGraphRevisionResponse(stdout, descriptor.sessionID, segmentID, revision);
      if (disposed || requestRevision !== investigationRevision || investigationDescriptor?.sessionID !== descriptor.sessionID) {
        return undefined;
      }
      const state = investigationModel.loadGraphRevision(
        graph.segmentSnapshot,
        graph.revision,
        graph.wholeBlobHash,
        graph.document,
      );
      investigationGraphHistory[segmentID] = {
        ...(investigationGraphHistory[segmentID] ?? {}),
        [String(revision)]: graph,
      };
      if (state.manifest?.segments[segmentID]?.graph_revision === revision) {
        investigationGraphs[segmentID] = graph;
      }
      publishInvestigationState(state);
      persistInvestigationCheckpoint(state, state.sequence);
      return state;
    })().catch((error: unknown) => {
      reportInvestigationError(`Could not load historical graph: ${deriveFailureMessage(error)}`);
      return undefined;
    }).finally(() => {
      investigationGraphLoads.delete(key);
    });
    investigationGraphLoads.set(key, load);
    return load;
  };
  const configureInvestigationClient = (
    child: ReturnType<typeof spawn>,
    descriptor: StoredSessionDescriptor,
    afterSequence: number,
    restored: Awaited<ReturnType<SessionGraphCacheStore['load']>>,
    bridge: McpBridge | undefined,
    revision: number,
    startupConfiguration?: { commandID: string; inputs: Readonly<Record<string, string>> },
  ) => {
    const model = new SessionGraphModel(descriptor.sessionID);
    investigationGraphs = {};
    investigationGraphHistory = {};
    if (restored && restored.sequence === afterSequence) {
      investigationGraphs = { ...restored.segmentGraphs };
      investigationGraphHistory = Object.fromEntries(Object.entries(restored.segmentGraphHistory).map(
        ([segmentID, revisions]) => [segmentID, { ...revisions }],
      ));
      publishInvestigationState(model.restore(
        afterSequence,
        restored.manifest,
        restored.segmentGraphs,
        restored.runtimeNodes,
        restored.executionNodeID,
        restored.pending,
        restored.segmentGraphHistory,
        restored.preparedTransitionTargets,
        restored.segmentGraphAvailability,
      ));
    }
    investigationModel = model;
    let client: SessionStdioClient;
    let startupConfigurationSent = false;
    let liveAttachment = false;
    client = new SessionStdioClient(child as RunChildProcess, {
      sessionID: descriptor.sessionID,
      afterSequence,
      onGroup: (group) => {
        if (disposed || revision !== investigationRevision) return;
        const state = model.applyGroup(group);
        for (const segmentID of state.unloadedSegmentIDs) delete investigationGraphs[segmentID];
        for (const graph of group.graphs) {
          const segmentSnapshot = state.manifest?.segments[graph.segmentID];
          if (!segmentSnapshot) throw new Error('Accepted graph revision has no segment snapshot.');
          investigationGraphs[graph.segmentID] = {
            revision: graph.revision,
            wholeBlobHash: graph.wholeBlobHash,
            encodedDocument: graph.encodedDocument,
            document: graph.document,
            segmentSnapshot: {
              ...segmentSnapshot,
              attempt_run_ids: [...segmentSnapshot.attempt_run_ids],
            },
          };
          investigationGraphHistory[graph.segmentID] = {
            ...(investigationGraphHistory[graph.segmentID] ?? {}),
            [String(graph.revision)]: investigationGraphs[graph.segmentID],
          };
        }
        publishInvestigationState(state, liveAttachment && !group.handshake, sessionVisualSteps(group, state.runtimeNodes));
        if (group.handshake) liveAttachment = true;
    if (group.handshake && startupConfiguration && !startupConfigurationSent) {
      startupConfigurationSent = true;
      client.send({
      type: 'session.configure',
      commandID: startupConfiguration.commandID,
      payload: { inputs: startupConfiguration.inputs },
      });
    }
      },
      onAcceptedSequence: (acceptedSequence) => {
        if (disposed || revision !== investigationRevision || !investigationDescriptor) return;
        if (investigationState) persistInvestigationCheckpoint(investigationState, acceptedSequence);
      },
      onError: (message) => {
        if (revision === investigationRevision) reportInvestigationError(message);
      },
      onStderr: (text) => {
        if (revision !== investigationRevision) return;
        output?.append(text);
        if (ready && !disposed) void panel.webview.postMessage({ type: 'session.stderr', text });
      },
      onExit: (code, signal) => {
        if (revision !== investigationRevision) return;
        if (investigationClient === client) investigationClient = undefined;
        if (bridge && runBridge === bridge) {
          runBridge.dispose();
          runBridge = undefined;
        }
        if (ready && !disposed) void panel.webview.postMessage({ type: 'session.exit', code, signal });
        applyDeferredReload();
      },
    });
    investigationClient = client;
  };
  const spawnInvestigation = async (
    descriptor: StoredSessionDescriptor,
    args: string[],
    afterSequence: number,
    restored?: Awaited<ReturnType<SessionGraphCacheStore['load']>>,
    startupConfiguration?: { commandID: string; inputs: Readonly<Record<string, string>> },
  ) => {
    const revision = ++investigationRevision;
    const isCurrent = () => !disposed && revision === investigationRevision;
    let bridge: McpBridge | undefined;
    let child: ReturnType<typeof spawn> | undefined;
    let ownershipTransferred = false;
    const workspaceFolders = (vscode.workspace.workspaceFolders ?? []).map((folder) => folder.uri.fsPath);
    const packageMap = resolveRunPackageMapPath(
      descriptor.projectRoot,
      getSetting('packageMap', resource, ''),
    );
    if (packageMap.warning) output?.appendLine(`[yawr session] WARNING: ${packageMap.warning}`);
    const vscodeMcpActions = currentDocument
      ? buildRegistryForRun(descriptor.projectRoot, packageMap.path)
      : {};
    try {
      const configuredBinary = getSetting('binaryPath', resource, 'yawr');
      const binary = testHooks?.spawnSession
        ? configuredBinary
        : await resolveBinary(configuredBinary, output!, descriptor.projectRoot, workspaceFolders);
      if (!testHooks?.spawnSession && currentDocument) await requireCompatibleExecution(binary, currentDocument);
      bridge = currentDocument && sessionMayRequireMcpBridge(currentDocument, vscodeMcpActions)
        ? await createMcpBridge(vscodeMcpActions, vscode.Uri.file(runbookPath))
        : undefined;
      if (!isCurrent()) return;
      if (!isCurrent()) return;
	  const launchArgs = withSessionPackageMap(args, packageMap.path);
      const spawnOptions: Parameters<typeof spawn>[2] = {
        cwd: descriptor.projectRoot,
        env: {
          ...process.env,
          ...(bridge ? {
            YAWR_VSCODE_BRIDGE_URL: bridge.bridgeUrl,
            YAWR_VSCODE_BRIDGE_TOKEN: bridge.bridgeToken,
          } : {}),
        },
        stdio: ['pipe', 'pipe', 'pipe'],
        windowsHide: true,
      };
      child = testHooks?.spawnSession
        ? testHooks.spawnSession(binary, launchArgs, spawnOptions)
        : spawn(binary, launchArgs, spawnOptions);
      if (!isCurrent()) return;
      if (!child.stdin || !child.stdout || !child.stderr) {
        throw new Error('yawr session process did not expose stdin, stdout, and stderr pipes');
      }
      configureInvestigationClient(child, descriptor, afterSequence, restored, bridge, revision, startupConfiguration);
      runBridge = bridge;
      ownershipTransferred = true;
    } finally {
      if (!ownershipTransferred) {
        if (child && child.exitCode === null && !child.killed) child.kill();
        bridge?.dispose();
        if (runBridge === bridge) runBridge = undefined;
      }
    }
  };
  const startInvestigation = async (rawInputs: unknown) => {
    if (runSession?.isFinished()) {
      runSession.dispose();
      runSession = undefined;
      runBridge?.dispose();
      runBridge = undefined;
    }
    if (investigationClient && (investigationClient.isFinished() || closedInvestigationStatus(investigationState?.sessionStatus))) {
      investigationClient.dispose();
      investigationClient = undefined;
      investigationDescriptor = undefined;
    }
    if (investigationDescriptor && !investigationClient) {
      investigationDescriptor = undefined;
    }
    if (disposed || runStarting || (runSession && !runSession.isFinished()) || investigationClient || investigationDescriptor || investigationStarting) {
      reportInvestigationError('Another run or session is already active.');
      return;
    }
    if (!currentDocument) {
      reportInvestigationError('The runbook graph is not loaded.');
      return;
    }
    const inputs = directRunInputs(rawInputs);
    const privateInputNames = directSecretInputNames(currentDocument);
    const privateInputs = Object.fromEntries(
      [...privateInputNames].filter((name) => inputs[name] !== undefined).map((name) => [name, inputs[name]]),
    );
    const workspaceFolders = (vscode.workspace.workspaceFolders ?? []).map((folder) => folder.uri.fsPath);
    const projectRoot = pickProjectRoot(runbookPath, workspaceFolders, path.dirname(runbookPath));
    const descriptor: StoredSessionDescriptor = {
      schemaVersion: STORED_SESSION_SCHEMA,
      sessionID: randomUUID(),
      creationCommandID: randomUUID(),
      configurationCommandID: randomUUID(),
      runbookPath,
      projectRoot,
      acceptedSequence: 0,
    };
    investigationStarting = true;
    const startRevision = ++investigationRevision;
    try {
      await checkpointWriter.flush();
      await extensionContext!.workspaceState.update(SESSION_WORKSPACE_STATE_KEY, descriptor);
    } catch (error) {
      investigationStarting = false;
      reportInvestigationError(deriveFailureMessage(error));
      return;
    }
    if (disposed || startRevision !== investigationRevision) {
      investigationStarting = false;
      return;
    }
    investigationDescriptor = descriptor;
    investigationState = undefined;
    investigationGraphs = {};
    investigationGraphHistory = {};
    if (ready && !disposed) void panel.webview.postMessage({ type: 'session.starting', sessionID: descriptor.sessionID, minimumStepDisplayMs: pacingInterval() });
    try {
      await spawnInvestigation(
        descriptor,
        buildSessionStartArgs(
          runbookPath,
          descriptor.sessionID,
          descriptor.creationCommandID,
          inputs,
          privateInputNames,
          projectRoot,
        ),
        0,
        undefined,
        { commandID: descriptor.configurationCommandID!, inputs: privateInputs },
      );
    } catch (error) {
      reportInvestigationError(deriveFailureMessage(error));
    } finally {
      investigationStarting = false;
    }
  };
  const reconnectInvestigation = async () => {
    if (disposed || investigationClient || runSession || !currentDocument) return;
    let descriptor: StoredSessionDescriptor;
    try {
      descriptor = parseStoredSessionDescriptor(
        extensionContext!.workspaceState.get<unknown>(SESSION_WORKSPACE_STATE_KEY),
      );
    } catch {
      return;
    }
    if (path.resolve(descriptor.runbookPath) !== path.resolve(runbookPath)) return;
    investigationDescriptor = descriptor;
    const cache = await sessionCache.load(descriptor.sessionID);
    const afterSequence = recoverySequence(descriptor, cache);
    if (cache && cache.sequence === afterSequence && closedInvestigationStatus(cache.manifest.session.status)) {
      investigationGraphs = { ...cache.segmentGraphs };
      investigationGraphHistory = Object.fromEntries(Object.entries(cache.segmentGraphHistory).map(
        ([segmentID, revisions]) => [segmentID, { ...revisions }],
      ));
      const model = new SessionGraphModel(descriptor.sessionID);
      investigationModel = model;
      publishInvestigationState(model.restore(
        afterSequence,
        cache.manifest,
        cache.segmentGraphs,
        cache.runtimeNodes,
        cache.executionNodeID,
        cache.pending,
        cache.segmentGraphHistory,
        cache.preparedTransitionTargets,
        cache.segmentGraphAvailability,
      ));
      return;
    }
    if (ready && !disposed) void panel.webview.postMessage({ type: 'session.reconnecting', sessionID: descriptor.sessionID, minimumStepDisplayMs: pacingInterval() });
    try {
      await spawnInvestigation(
        descriptor,
        buildSessionAttachArgs(descriptor.sessionID, afterSequence, descriptor.projectRoot),
        afterSequence,
        cache,
      );
    } catch (error) {
      reportInvestigationError(deriveFailureMessage(error));
    }
  };
  const loadSavedRouteTests = async (projectRoot: string, runbookRelative: string, planHash: string) => {
    const loaded = await loadRouteTestArtifacts(projectRoot, runbookRelative);
    return {
      routeTests: loaded.artifacts.map(({ artifact }) => ({
        artifact,
        needsReview: artifact.plan_hash !== planHash,
      })),
      warnings: loaded.warnings,
    };
  };
  const publishSavedRouteTests = async () => {
    if (disposed || !currentProjectRoot || !currentRunbookRelative || !currentPlanHash) return;
    const projectRoot = currentProjectRoot;
    const runbookRelative = currentRunbookRelative;
    const planHash = currentPlanHash;
    const loaded = await loadSavedRouteTests(projectRoot, runbookRelative, planHash);
    if (disposed || projectRoot !== currentProjectRoot || runbookRelative !== currentRunbookRelative || planHash !== currentPlanHash) return;
    for (const warning of loaded.warnings) output?.appendLine(`[yawr route test] WARNING: ${warning}`);
    void panel.webview.postMessage({ type: 'route-tests', routeTests: loaded.routeTests });
  };
  const reload = async () => {
    const revision = ++loadRevision;
    loadController?.abort();
    const controller = new AbortController();
    loadController = controller;
    publish({ type: 'loading' });
    publishReloadState(true);
    const style = getSetting('preview.nodeStyle', resource, 'smooth-curves');
    const workspaceFolders = (vscode.workspace.workspaceFolders ?? []).map((folder) => folder.uri.fsPath);
    const projectRoot = presentationProjectRoot(
      runbookPath,
      workspaceFolders,
      path.dirname(runbookPath),
      getSetting('packageMap', resource, ''),
    );
    const isCurrentRevision = () => !disposed && revision === loadRevision;
    const authoringRoot = projectRoot;
    try {
      let document: GraphDocument;
      if (testHooks?.documentLoader) {
        document = await testHooks.documentLoader();
        if (!isCurrentRevision()) return;
      } else {
        const configuredBinary = getSetting('binaryPath', resource, 'yawr');
        let binary = getSetting('highlighting.developmentHelperPath', resource, '') || bundledPresentationHelper(extensionContext!.extensionPath);
        if (!path.isAbsolute(binary)) throw new Error('Presentation helper path must be absolute.');
        try { await verifyPresentationHelper(binary, controller.signal); }
        catch { binary = await resolveBinary(configuredBinary, output!, projectRoot, workspaceFolders); }
        if (!isCurrentRevision()) return;
        document = await loadGraphDocument(binary, runbookPath, async (command, args) => {
          const { stdout } = await pexec(command, args, {
            cwd: authoringRoot,
            signal: controller.signal,
            maxBuffer: 16 * 1024 * 1024,
          });
          if (!isCurrentRevision()) throw new Error('Graph reload was superseded.');
          return { stdout };
        }, resolveRunPackageMapPath(authoringRoot, getSetting('packageMap', resource, '')).path);
        if (!isCurrentRevision()) return;
      }
      let planHash = document.hash;
      setPresentationEntrypoint(authoringRoot, runbookPath, document.frames.map(frame => frame.runbook_path));
      let planWarning: string | undefined;
      if (!testHooks?.documentLoader) {
        try {
          const configuredBinary = getSetting('binaryPath', resource, 'yawr');
          const binary = await resolveBinary(configuredBinary, output!, projectRoot, workspaceFolders);
          if (!isCurrentRevision()) return;
          const packageMap = resolveRunPackageMapPath(projectRoot, getSetting('packageMap', resource, ''));
          const planArgs = ['plan', '--output', 'json', '--expand', 'eager'];
          if (packageMap.path) planArgs.push('--package-map', packageMap.path);
          planArgs.push(runbookPath);
          const { stdout } = await pexec(binary, planArgs, { cwd: projectRoot, signal: controller.signal, maxBuffer: 16 * 1024 * 1024 });
          if (!isCurrentRevision()) return;
          planHash = parseRouteTestPlanHash(stdout);
        } catch (planError) {
          if (!isCurrentRevision()) return;
          planHash = undefined;
          planWarning = `[yawr route test] unavailable for ${runbookPath}: ${deriveFailureMessage(planError)}`;
        }
      }
      const runbookRelative = path.relative(projectRoot, runbookPath).replaceAll(path.sep, '/');
      const loadedRouteTests = planHash
        ? await loadSavedRouteTests(projectRoot, runbookRelative, planHash)
        : { routeTests: [], warnings: [] };
      if (!isCurrentRevision()) return;

      currentStyle = style;
      currentProjectRoot = projectRoot;
      currentRunbookRelative = runbookRelative;
      currentPlanHash = planHash;
      currentDocument = document;
      sourceDocument = document;
      if (planWarning) output?.appendLine(planWarning);
      for (const warning of loadedRouteTests.warnings) output?.appendLine(`[yawr route test] WARNING: ${warning}`);
      publish({
        type: 'graph',
        document,
        style,
        ...(planHash ? { routeTestContext: { runbook: runbookRelative, planHash } } : {}),
        routeTests: loadedRouteTests.routeTests,
        testMode: extensionContext?.extensionMode === vscode.ExtensionMode.Test,
      });
      output?.appendLine(`[yawr] direct graph loaded for ${runbookPath}`);
    } catch (error) {
      if (!isCurrentRevision()) return;
      const message = deriveFailureMessage(error);
      publish({ type: 'error', message });
      output?.appendLine(`[yawr] direct graph failed for ${runbookPath}:\n${message}`);
      rejectProductionRun(new Error(message));
    } finally {
      if (loadController === controller) {
        loadController = undefined;
        publishReloadState(false);
        tryStartProductionRun();
      }
    }
  };
  const tryStartProductionRun = () => {
    if (!productionRun || !ready || !panel.visible || !currentDocument || loadController ||
        disposed || productionRunStarted || productionRunSettled) return;
    productionRunStarted = true;
    void startRun(productionRun.inputs, undefined);
  };
  const requestReload = () => {
    if (runStarting || runSession || investigationClient || investigationDescriptor || retainExecutionGraph) {
      reloadPending = true;
      return;
    }
    void reload();
  };
  const applyDeferredReload = () => {
    if (!reloadPending || disposed || retainExecutionGraph) return;
    reloadPending = false;
    void reload();
  };

  const confirmedHostActionRequests = new Set<string>();
  const consumePanelConfirmation = (requestId: string): boolean => confirmedHostActionRequests.delete(requestId);
  const recordPanelConfirmation = (value: Record<string, unknown>): boolean => {
    const expectedFields = [
      'type', 'version', 'capability', 'runId', 'turnId', 'correlationId', 'previewSessionId', 'requestId',
    ];
    if (Object.keys(value).length !== expectedFields.length ||
        !expectedFields.every((field) => Object.prototype.hasOwnProperty.call(value, field)) ||
        value.type !== 'yawr.host-action.confirmed-request' ||
        value.version !== 'yawr.host-action/v1' ||
        value.capability !== 'external-view.open') return false;
    for (const field of expectedFields.slice(3)) {
      const item = value[field];
      if (typeof item !== 'string' || item.length === 0 || item.length > 1024) return false;
    }
    if (confirmedHostActionRequests.size >= 32) return false;
    confirmedHostActionRequests.add(value.requestId as string);
    return true;
  };

  const hostActionRegistry = new Map<string, HostActionRegistration>([
    ['test.echo', { handler: testEchoHandler }],
    ['external-view.open', { handler: makeExternalViewOpenHandler(panel, consumePanelConfirmation, testHooks?.showExternalViewReminder), timeoutMs: EXTERNAL_VIEW_HOST_ACTION_TIMEOUT_MS }],
  ]);
  const hostActionTransport = webviewPanelTransport(panel, () => !disposed && directGraphPanel === panel);
  const hostActions = createHostActionBridge(
    hostActionRegistry,
    {
      sendAck: (ack) => {
        testHooks?.onHostActionAck?.(ack);
        hostActionTransport.sendAck(ack);
      },
      sendCancel: (cancel) => hostActionTransport.sendCancel(cancel),
    },
  );
  const invalidateHostActionRun = () => {
    activeHostActionRunID = undefined;
    confirmedHostActionRequests.clear();
    hostActions.cancelAllPending('run-replaced');
  };

  const startRun = async (rawInputs: unknown, rawDebug: unknown, routeTestPath?: string, reservedRevision?: number) => {
    if (disposed) return;
    if (loadController !== undefined) {
      if (reservedRevision !== undefined && activeRouteTest?.revision === reservedRevision) {
        activeRouteTest = undefined;
        runStarting = false;
      }
      void panel.webview.postMessage({
        type: 'run.error',
        message: 'The runbook graph is reloading. Wait for it to finish before starting a run.',
      });
      return;
    }
    if (runSession?.isFinished()) {
      runSession.dispose();
      runSession = undefined;
      runBridge?.dispose();
      runBridge = undefined;
    }
    if (investigationClient && (investigationClient.isFinished() || closedInvestigationStatus(investigationState?.sessionStatus))) {
      investigationClient.dispose();
      investigationClient = undefined;
      investigationDescriptor = undefined;
    }
    if (investigationDescriptor && !investigationClient) {
      investigationDescriptor = undefined;
    }
    if ((runStarting || (runSession && !runSession.isFinished()) || investigationClient) && reservedRevision === undefined) {
      void panel.webview.postMessage({ type: 'run.error', message: 'A run is already active.' });
      return;
    }
    retainExecutionGraph = false;
    let inputs: Record<string, string>;
    let debug;
    try {
      if (sourceDocument) currentDocument = sourceDocument;
      if (!currentDocument) throw new Error('The runbook graph is not loaded.');
      inputs = routeTestPath ? {} : directRunInputs(rawInputs);
      debug = routeTestPath ? undefined : parseDirectDebugConfig(rawDebug);
      if (!routeTestPath) validateDirectDebugTargets(debug, currentDocument?.nodes ?? []);
    } catch (error) {
      void panel.webview.postMessage({ type: 'run.error', message: deriveFailureMessage(error) });
      return;
    }

    invalidateHostActionRun();
    if (reservedRevision === undefined) runStarting = true;
    const startRevision = reservedRevision ?? ++runStartRevision;
    const startupIsActive = () => !disposed && startRevision === runStartRevision;
    void panel.webview.postMessage({ type: 'run.starting', routeTest: routeTestPath !== undefined, minimumStepDisplayMs: pacingInterval() });
    try {
      await testHooks?.beforeSpawn?.();
      if (!startupIsActive()) return;
      const workspaceFolders = (vscode.workspace.workspaceFolders ?? []).map((folder) => folder.uri.fsPath);
      const projectRoot = currentProjectRoot ?? presentationProjectRoot(
        runbookPath,
        workspaceFolders,
        path.dirname(runbookPath),
        getSetting('packageMap', resource, ''),
      );
      const packageMap = resolveRunPackageMapPath(
        projectRoot,
        getSetting('packageMap', resource, ''),
      );
      if (packageMap.warning) output?.appendLine(`[yawr run] WARNING: ${packageMap.warning}`);
      const configuredBinary = getSetting('binaryPath', resource, 'yawr');
      const binary = testHooks?.spawnRun
        ? configuredBinary
        : await resolveBinary(configuredBinary, output!, projectRoot, workspaceFolders);
      if (!testHooks?.spawnRun) await requireRunbookCompatibility(binary, currentDocument!, runbookPath, projectRoot, packageMap.path);
      const vscodeMcpActions = routeTestPath ? {} : buildRegistryForRun(projectRoot, packageMap.path);
      const bridge = !routeTestPath && graphMayRequireMcpBridge(currentDocument!, vscodeMcpActions)
        ? await createMcpBridge(vscodeMcpActions, vscode.Uri.file(runbookPath))
        : undefined;
      runBridge = bridge;
      if (!startupIsActive()) {
        runBridge?.dispose();
        runBridge = undefined;
        return;
      }
      if (!startupIsActive()) return;
      const privateInputNames = routeTestPath ? new Set<string>() : directSecretInputNames(currentDocument!);
      const args = buildStdioRunArgs(runbookPath, inputs, packageMap.path, debug !== undefined, privateInputNames, routeTestPath,
        currentDocument !== undefined && usesTypedResults(currentDocument), true);
      const spawnOptions: Parameters<typeof spawn>[2] = {
        cwd: projectRoot,
        env: {
          ...process.env,
          ...(bridge ? {
            YAWR_VSCODE_BRIDGE_URL: bridge.bridgeUrl,
            YAWR_VSCODE_BRIDGE_TOKEN: bridge.bridgeToken,
          } : {}),
        },
        stdio: ['pipe', 'pipe', 'pipe'],
      };
      if (!startupIsActive()) return;
      if (productionRun) {
        productionLaunch = { binary, args: [...args], cwd: projectRoot };
      }
      const child = testHooks?.spawnRun
        ? testHooks.spawnRun(binary, args, spawnOptions)
        : spawn(binary, args, spawnOptions);
      if (!startupIsActive()) {
        child.kill();
        bridge?.dispose();
        return;
      }
      if (!child.stdin || !child.stdout || !child.stderr) {
        child.kill();
        throw new Error('yawr stdio process did not expose stdin, stdout, and stderr pipes');
      }
      let session: DirectRunSession;
      session = new DirectRunSession(child as RunChildProcess, {
        onFrame: (frame) => {
          if (!startupIsActive()) return;
          if (frame.type === 'run.graph' && currentDocument) {
            currentDocument = mergeExecutionGraph(currentDocument, frame.document as GraphDocument, frame.nodeIDs as string[]);
            setPresentationEntrypoint(projectRoot, runbookPath, currentDocument.frames.map(frame => frame.runbook_path));
            retainExecutionGraph = true;
          }
          if (productionRun) productionFrames.push({ ...frame });
          if (frame.type === 'run.started') {
            invalidateHostActionRun();
            activeHostActionRunID = frame.runID;
          }
          const eventKind = frame.type === 'run.event' && typeof frame.event === 'object' && frame.event !== null
            ? (frame.event as Record<string, unknown>).kind
            : undefined;
            if (frame.type === 'run.finished' || frame.type === 'protocol.error' ||
              eventKind === 'run/completed' || eventKind === 'run/failed' ||
              eventKind === 'run/cancelled' || eventKind === 'run/indeterminate') {
            invalidateHostActionRun();
          }
          if (routeTestPath && frame.type === 'run.finished') {
            if (activeRouteTest?.revision === startRevision) {
              activeRouteTest.outcome = parseDirectRouteTestOutcome(frame.routeTest);
            }
          }
          void panel.webview.postMessage({ type: 'run.frame', frame });
          if (bridge && frame.type === 'run.finished' && runBridge === bridge) {
            runBridge.dispose();
            runBridge = undefined;
          }
          if (frame.type === 'run.finished') {
            applyDeferredReload();
            if (productionRun && productionLaunch && !productionRunSettled) {
              productionRunSettled = true;
              productionRun.resolve({
                extensionPath: extensionContext!.extensionPath,
                ...productionLaunch,
                frames: productionFrames,
                stderr: productionStderr,
                finished: { ...frame },
              });
            }
          }
        },
        onError: (message) => {
          if (!startupIsActive()) return;
          invalidateHostActionRun();
          output?.appendLine(`[yawr] stdio protocol error: ${message}`);
          void panel.webview.postMessage({ type: 'run.error', message });
          rejectProductionRun(new Error(message));
        },
        onStderr: (text) => {
          if (!startupIsActive()) return;
          if (productionRun) productionStderr += text;
          output?.append(text);
          void panel.webview.postMessage({ type: 'run.stderr', text });
        },
        onExit: (code, signal) => {
          if (!startupIsActive()) return;
          invalidateHostActionRun();
          if (runSession === session) runSession = undefined;
          if (bridge && runBridge === bridge) {
            runBridge.dispose();
            runBridge = undefined;
          }
          void panel.webview.postMessage({ type: 'run.exit', code, signal });
          if (productionRun && !productionRunSettled) {
            rejectProductionRun(new Error(`yawr run exited before run.finished: code=${code} signal=${signal}`));
          }
          applyDeferredReload();
        },
      });
      runSession = session;
      if (!routeTestPath && (debug || privateInputNames.size > 0)) {
        const privateInputs = Object.fromEntries(
          [...privateInputNames].filter((name) => inputs[name] !== undefined).map((name) => [name, inputs[name]]),
        );
        session.send({
          type: 'run.configure',
          ...(Object.keys(privateInputs).length > 0 ? { inputs: privateInputs } : {}),
          ...(debug ? { debug } : {}),
        });
      }
    } catch (error) {
      invalidateHostActionRun();
      runBridge?.dispose();
      runBridge = undefined;
      if (reservedRevision !== undefined && activeRouteTest?.revision === reservedRevision && !runSession) {
        activeRouteTest = undefined;
      }
      const message = deriveFailureMessage(error);
      output?.appendLine(`[yawr] direct run failed to start:\n${message}`);
      if (startupIsActive()) void panel.webview.postMessage({ type: 'run.error', message });
      rejectProductionRun(new Error(message));
    } finally {
      if (startRevision === runStartRevision) runStarting = false;
      if (!runSession) applyDeferredReload();
      testHooks?.onStartSettled?.();
    }
  };

  const saveRouteTest = async (rawArtifact: unknown, run: boolean) => {
    try {
      if (!currentProjectRoot || !currentRunbookRelative || !currentPlanHash || !currentDocument) {
        throw new Error('The runbook graph is not loaded.');
      }
      if (typeof rawArtifact !== 'object' || rawArtifact === null || Array.isArray(rawArtifact)) {
        throw new Error('Route test must be an object.');
      }
      const requested = rawArtifact as Record<string, unknown>;
      if (typeof requested.plan_hash !== 'string') {
        throw new Error('The route test plan_hash must exist and be a string. Review it again before saving or running.');
      }
      if (requested.plan_hash !== currentPlanHash) {
        throw new Error('The route test plan_hash no longer matches this runbook. Review it again before saving or running.');
      }
      const savingResult = requested.last_result !== undefined;
      let candidate: Record<string, unknown>;
      if (savingResult) {
        if (!activeRouteTest?.outcome) throw new Error('No completed route test is available to save.');
        if (currentPlanHash !== activeRouteTest.artifact.plan_hash) throw new Error('The runbook changed while the route test was running. Review and run it again.');
        if (requested.id !== activeRouteTest.artifact.id) throw new Error('The route-test result does not match the executed artifact.');
        candidate = stampRouteTestResultDigest({
          ...activeRouteTest.artifact,
          last_result: {
            status: activeRouteTest.outcome.status,
            target_reached: activeRouteTest.outcome.targetReached,
            external_dispatches: activeRouteTest.outcome.externalDispatches,
            ran_at: new Date().toISOString(),
            conditions_digest: 'pending-extension-stamp',
          },
        } as unknown as Record<string, unknown>);
      } else {
        const { last_result: _discardedResult, ...draft } = requested;
        candidate = draft;
      }
      const artifact = parseRouteTestArtifact(candidate);
      if (artifact.runbook !== currentRunbookRelative) throw new Error('Route test runbook does not match this panel.');
      validateRouteTestAgainstDocument(artifact, currentDocument);
      const savedArtifact = artifact;
      const reviews = [
        ...(savedArtifact.step_responses ?? []),
        ...(savedArtifact.host_action_responses ?? []),
        ...(savedArtifact.interaction_answers ?? []),
        ...(savedArtifact.test_approvals ?? []),
      ].map((binding) => binding.review);
      if (!savedArtifact.sensitivity_reviewed || reviews.some((review) => !review.sensitivity_reviewed)) {
        throw new Error('Review saved values for sensitive data before persisting this route test.');
      }
      if (run) {
        if (reviews.some((review) => review.state !== 'reviewed')) {
          throw new Error('Review every saved result and answer before running the route test.');
        }
      }
      let reservedRevision: number | undefined;
      if (run) {
        if (runSession?.isFinished()) {
          runSession.dispose();
          runSession = undefined;
          runBridge?.dispose();
          runBridge = undefined;
        }
        if (investigationClient && (investigationClient.isFinished() || closedInvestigationStatus(investigationState?.sessionStatus))) {
          investigationClient.dispose();
          investigationClient = undefined;
          investigationDescriptor = undefined;
        }
        if (investigationDescriptor && !investigationClient) {
          investigationDescriptor = undefined;
        }
        if (runStarting || (runSession && !runSession.isFinished()) || investigationClient || investigationDescriptor) throw new Error('A run is already active.');
        runStarting = true;
        reservedRevision = ++runStartRevision;
        activeRouteTest = {
          revision: reservedRevision,
          artifact: JSON.parse(JSON.stringify(savedArtifact)) as RouteTestArtifact,
        };
      }
      let filePath: string;
      try {
        filePath = await saveRouteTestArtifact(currentProjectRoot, savedArtifact);
      } catch (error) {
        if (reservedRevision !== undefined && activeRouteTest?.revision === reservedRevision) activeRouteTest = undefined;
        if (reservedRevision !== undefined) runStarting = false;
        throw error;
      }
      await publishSavedRouteTests();
      void panel.webview.postMessage({ type: 'route-test.saved', artifact: savedArtifact, running: run });
      if (run) await startRun({}, undefined, filePath, reservedRevision);
    } catch (error) {
      const message = deriveFailureMessage(error);
      output?.appendLine(`[yawr route test] ${message}`);
      void panel.webview.postMessage({ type: 'route-test.error', message });
    }
  };

  const loadSavedRun = async (explicitRunID?: string, explicitRunDir?: string): Promise<void> => {
    try {
      if (runStarting || (runSession && !runSession.isFinished()) || investigationStarting || (investigationClient && !closedInvestigationStatus(investigationState?.sessionStatus))) {
        void vscode.window.showWarningMessage('A run or investigation session is currently active. Stop or reset it before loading a saved run.');
        return;
      }
      if (runSession?.isFinished()) {
        runSession.dispose();
        runSession = undefined;
        runBridge?.dispose();
        runBridge = undefined;
      }
      if (investigationClient && (investigationClient.isFinished() || closedInvestigationStatus(investigationState?.sessionStatus))) {
        investigationClient.dispose();
        investigationClient = undefined;
        investigationDescriptor = undefined;
      }
      if (investigationDescriptor && !investigationClient) {
        investigationDescriptor = undefined;
      }

      if (testHooks?.loadSavedRun) {
        const loaded = await testHooks.loadSavedRun(explicitRunDir, explicitRunID);
        if (!loaded || disposed) return;
        currentDocument = loaded.document;
        retainExecutionGraph = true;
        runStarting = false;
        invalidateHostActionRun();
        activeHostActionRunID = loaded.runID;
        publish({
          type: 'run.loaded',
          document: loaded.document,
          runID: loaded.runID,
          status: loaded.status,
          error: loaded.error,
          events: loaded.events,
          steps: loaded.steps,
          resultsAvailability: loaded.resultsAvailability,
        });
        return;
      }

      let runID = explicitRunID;
      let runDir = explicitRunDir;

      if (!runID || !runDir) {
        const workspaceFolders = (vscode.workspace.workspaceFolders ?? []).map((folder) => folder.uri.fsPath);
        const projectRoot = currentProjectRoot ?? presentationProjectRoot(
          runbookPath,
          workspaceFolders,
          path.dirname(runbookPath),
          getSetting('packageMap', resource, ''),
        );
        const candidateDirs: string[] = Array.from(new Set([
          path.join(projectRoot, '.runbook', 'runs'),
          path.join(path.dirname(runbookPath), '.runbook', 'runs'),
          ...workspaceFolders.map((f) => path.join(f, '.runbook', 'runs')),
        ]));

        const discovered = await discoverSavedRuns(candidateDirs);

        interface SavedRunQuickPickItem extends vscode.QuickPickItem {
          runID?: string;
          runDir?: string;
          isBrowse?: boolean;
        }

        const items: SavedRunQuickPickItem[] = [];
        const currentNorm = path.normalize(runbookPath);
        const currentBase = path.basename(runbookPath);
        const currentRuns: SavedRunSummary[] = [];
        const otherRuns: SavedRunSummary[] = [];

        for (const run of discovered) {
          const runNorm = run.runbookPath ? path.normalize(run.runbookPath) : '';
          if (runNorm === currentNorm || runNorm.endsWith(currentBase)) {
            currentRuns.push(run);
          } else {
            otherRuns.push(run);
          }
        }

        const formatItem = (run: SavedRunSummary, isCurrentRunbook: boolean): SavedRunQuickPickItem => {
          const statusIcon = run.status === 'completed' ? '$(check)'
            : run.status === 'failed' ? '$(error)'
            : run.status === 'cancelled' ? '$(circle-slash)'
            : '$(history)';
          const timeStr = run.startedAt ? new Date(run.startedAt).toLocaleString() : 'Unknown date';
          const stepsStr = run.stepCount !== undefined ? `${run.stepCount} steps` : '';
          const tag = isCurrentRunbook ? '(current runbook)' : (run.runbookPath ? path.basename(run.runbookPath) : '');
          return {
            label: `${statusIcon} ${run.runID}`,
            description: `${run.status ?? 'unknown'} • ${timeStr} ${tag ? `• ${tag}` : ''}`,
            detail: `${run.runDir}${stepsStr ? ` • ${stepsStr}` : ''}`,
            runID: run.runID,
            runDir: run.runDir,
          };
        };

        if (currentRuns.length > 0) {
          items.push({ label: 'Current Runbook', kind: vscode.QuickPickItemKind.Separator });
          items.push(...currentRuns.map((r) => formatItem(r, true)));
        }
        if (otherRuns.length > 0) {
          items.push({ label: 'Other Runs', kind: vscode.QuickPickItemKind.Separator });
          items.push(...otherRuns.map((r) => formatItem(r, false)));
        }

        items.push({ label: '', kind: vscode.QuickPickItemKind.Separator });
        items.push({
          label: '$(folder-opened) Browse for run directory or trace...',
          description: 'Select a run directory, trace.jsonl, or checkpoint file from disk',
          isBrowse: true,
        });

        const selected = await vscode.window.showQuickPick(items, {
          placeHolder: 'Select a saved run to load in the graph view',
          matchOnDescription: true,
          matchOnDetail: true,
        });

        if (!selected) return;

        if (selected.isBrowse) {
          const uris = await vscode.window.showOpenDialog({
            canSelectFiles: true,
            canSelectFolders: true,
            canSelectMany: false,
            openLabel: 'Load Run',
            title: 'Select run directory or trace/checkpoint file',
          });
          if (!uris || uris.length === 0) return;
          const identity = resolveRunIdentityFromPath(uris[0].fsPath);
          if (!identity) {
            void vscode.window.showErrorMessage(`Selected path does not appear to be a valid Yawr run: ${uris[0].fsPath}`);
            return;
          }
          runID = identity.runID;
          runDir = identity.runDir;
        } else if (selected.runID && selected.runDir) {
          runID = selected.runID;
          runDir = selected.runDir;
        } else {
          return;
        }
      }

      if (disposed) return;

      const workspaceFolders = (vscode.workspace.workspaceFolders ?? []).map((folder) => folder.uri.fsPath);
      const projectRoot = currentProjectRoot ?? presentationProjectRoot(
        runbookPath,
        workspaceFolders,
        path.dirname(runbookPath),
        getSetting('packageMap', resource, ''),
      );
      const configuredBinary = getSetting('binaryPath', resource, 'yawr');
      const binary = await resolveBinary(configuredBinary, output!, projectRoot, workspaceFolders);

      const loaded = await loadSavedRunState(
        binary,
        runDir,
        runID,
        (cmd, args) => pexec(cmd, args, { cwd: projectRoot, maxBuffer: 32 * 1024 * 1024 }),
      );

      if (disposed) return;

      currentDocument = loaded.document;
      retainExecutionGraph = true;
      runStarting = false;
      invalidateHostActionRun();
      activeHostActionRunID = loaded.runID;

      publish({
        type: 'run.loaded',
        document: loaded.document,
        runID: loaded.runID,
        status: loaded.status,
        error: loaded.error,
        events: loaded.events,
        steps: loaded.steps,
        resultsAvailability: loaded.resultsAvailability,
      });
    } catch (error) {
      const message = deriveFailureMessage(error);
      output?.appendLine(`[yawr load run] ${message}`);
      void vscode.window.showErrorMessage(`Failed to load saved run: ${message}`);
    }
  };

  const messageSub = panel.webview.onDidReceiveMessage((message: unknown) => {
    if (typeof message !== 'object' || message === null || Array.isArray(message)) return;
    const candidate = message as Record<string, unknown>;
    if (candidate.type === 'ready') {
      ready = true;
      void panel.webview.postMessage({ type: 'preview.visibility', visible: panel.visible });
      void panel.webview.postMessage(latestMessage);
      if (investigationState) {
        void panel.webview.postMessage({ type: 'session.update', state: investigationState, style: currentStyle, minimumStepDisplayMs: pacingInterval() });
      }
      void panel.webview.postMessage({ type: 'graph.reload-state', active: loadController !== undefined });
      tryStartProductionRun();
      return;
    }
    if (candidate.type === 'session.start') {
      void startInvestigation(candidate.inputs);
      return;
    }
    if (candidate.type === 'session.command') {
      if (!investigationClient || typeof candidate.command !== 'object' || candidate.command === null || Array.isArray(candidate.command)) {
        reportInvestigationError('No investigation session is attached.');
        return;
      }
      const command = candidate.command as Record<string, unknown>;
      const supported = new Set<SessionCommandRequest['type']>([
        'session.configure', 'interaction.answer', 'session.cancel', 'session.detach',
        'session.resume', 'session.continue_live', 'session.close',
      ]);
      if (typeof command.type !== 'string' || !supported.has(command.type as SessionCommandRequest['type'])) {
        reportInvestigationError('Unsupported investigation session command.');
        return;
      }
      const request: SessionCommandRequest = {
        type: command.type as SessionCommandRequest['type'],
        commandID: randomUUID(),
        ...(typeof command.segmentID === 'string' ? { segmentID: command.segmentID } : {}),
        ...(typeof command.runID === 'string' ? { runID: command.runID } : {}),
        ...(typeof command.turnID === 'string' ? { turnID: command.turnID } : {}),
        ...(command.payload !== undefined ? { payload: command.payload } : {}),
      };
      investigationClient.send(request);
      return;
    }
    if (candidate.type === 'session.graph-revision') {
      const requestID = typeof candidate.requestID === 'string' ? candidate.requestID : '';
      const segmentID = typeof candidate.segmentID === 'string' ? candidate.segmentID : '';
      const originalNodeID = typeof candidate.originalNodeID === 'string' ? candidate.originalNodeID : '';
      const revision = candidate.revision;
      if (!requestID || requestID.length > 1024 || !segmentID || segmentID.length > 1024 ||
          !originalNodeID || originalNodeID.length > 4096 || !Number.isSafeInteger(revision) ||
          (revision as number) < 1 || !investigationModel) {
        reportInvestigationError('Historical graph revision request is invalid.');
        return;
      }
      void ensureInvestigationGraph(segmentID, revision as number).then(() => {
        const node = investigationModel?.graphRevisionNode(segmentID, revision as number, originalNodeID);
        if (ready && !disposed) {
          void panel.webview.postMessage({
            type: 'session.graph-revision', requestID,
            ...(node ? { node } : { error: 'The selected node is unavailable in that graph revision.' }),
          });
        }
      });
      return;
    }
    if (candidate.type === 'session.load-segment') {
      const segmentID = typeof candidate.segmentID === 'string' ? candidate.segmentID : '';
      const revision = candidate.revision;
      if (!segmentID || segmentID.length > 1024 || !Number.isSafeInteger(revision) || (revision as number) < 1) {
        reportInvestigationError('Historical segment request is invalid.');
        return;
      }
      void ensureInvestigationGraph(segmentID, revision as number);
      return;
    }
    if (candidate.type === 'session.load-route') {
      const targetSegmentID = typeof candidate.targetSegmentID === 'string' ? candidate.targetSegmentID : '';
      const manifest = investigationState?.manifest;
      const targetOrdinal = manifest?.segments[targetSegmentID]?.ordinal;
      if (!manifest || targetOrdinal === undefined) {
        reportInvestigationError('Historical route request is invalid.');
        return;
      }
      const missing = investigationState?.unloadedSegmentIDs ?? [];
      void (async () => {
        for (const segmentID of missing
          .filter((id) => (manifest.segments[id]?.ordinal ?? Number.MAX_SAFE_INTEGER) <= targetOrdinal)
          .sort((left, right) => manifest.segments[left].ordinal - manifest.segments[right].ordinal)) {
          const revision = manifest.segments[segmentID].graph_revision;
          if (revision) await ensureInvestigationGraph(segmentID, revision);
        }
      })();
      return;
    }
    if (candidate.type === 'session.reset') {
      if (investigationClient && !closedInvestigationStatus(investigationState?.sessionStatus)) {
        reportInvestigationError('Close or detach the active investigation before resetting it.');
        return;
      }
      investigationClient?.dispose();
      investigationClient = undefined;
      const resetSessionID = investigationDescriptor?.sessionID;
      investigationRevision += 1;
      investigationStarting = false;
      investigationDescriptor = undefined;
      investigationModel = undefined;
      investigationState = undefined;
      investigationGraphs = {};
      investigationGraphHistory = {};
      runBridge?.dispose();
      runBridge = undefined;
      void checkpointWriter.flush()
        .then(async () => {
          await extensionContext!.workspaceState.update(SESSION_WORKSPACE_STATE_KEY, undefined);
          if (resetSessionID) await sessionCache.delete(resetSessionID);
        })
        .then(() => reload())
        .catch((error: unknown) => reportInvestigationError(`Could not reset investigation: ${deriveFailureMessage(error)}`));
      return;
    }
    if (candidate.type === 'run.start') {
      void startRun(candidate.inputs, candidate.debug);
      return;
    }
    if (candidate.type === 'run.load-request') {
      const explicitRunID = typeof candidate.runID === 'string' ? candidate.runID : undefined;
      const explicitRunDir = typeof candidate.runDir === 'string' ? candidate.runDir : undefined;
      void loadSavedRun(explicitRunID, explicitRunDir);
      return;
    }
    if (candidate.type === 'run.command') {
      if (typeof candidate.command === 'object' && candidate.command !== null && !Array.isArray(candidate.command)) {
        const command = candidate.command as Record<string, unknown>;
        if (command.runID === activeHostActionRunID) runSession?.send(command);
      }
      return;
    }
    if (candidate.type === 'run.reset') {
      invalidateHostActionRun();
      runStartRevision += 1;
      runStarting = false;
      runSession?.dispose();
      runSession = undefined;
      runBridge?.dispose();
      runBridge = undefined;
      activeRouteTest = undefined;
      retainExecutionGraph = false;
      if (sourceDocument) {
        currentDocument = sourceDocument;
        latestMessage = {
          type: 'graph',
          document: sourceDocument,
          style: currentStyle,
          routeTests: [],
          planHash: currentPlanHash,
          activeRunID: undefined,
          sessionID: undefined,
        };
      }
      applyDeferredReload();
      return;
    }
    if (candidate.type === 'route-test.save' || candidate.type === 'route-test.run') {
      void saveRouteTest(candidate.artifact, candidate.type === 'route-test.run');
      return;
    }
    if (candidate.type === 'yawr.host-action.confirmed-request') {
      if (candidate.runId === activeHostActionRunID) recordPanelConfirmation(candidate);
      return;
    }
    if (candidate.type === 'style.change' &&
        (candidate.style === 'smooth-curves' || candidate.style === 'minimalist' || candidate.style === 'header-badges')) {
      void vscode.workspace
        .getConfiguration('yawr', resource)
        .update('preview.nodeStyle', candidate.style, vscode.ConfigurationTarget.WorkspaceFolder);
      return;
    }
    if (candidate.type !== 'yawr.host-action.request' || candidate.runId === activeHostActionRunID) {
      void hostActions.receive(message);
    }
  });
  const saveSub = vscode.workspace.onDidSaveTextDocument((document) => {
    const folders = (vscode.workspace.workspaceFolders ?? []).map(folder => folder.uri.fsPath);
    const configuredMap = getSetting('packageMap', resource, '');
    const projectRoot = presentationProjectRoot(runbookPath, folders, path.dirname(runbookPath), configuredMap);
    const packageMap = resolveRunPackageMapPath(projectRoot, configuredMap);
    if (graphSourceChanged(document.fileName, runbookPath, projectRoot, currentDocument, packageMap.path)) requestReload();
  });
  const visibilitySub = panel.onDidChangeViewState(() => {
    if (ready && !disposed) void panel.webview.postMessage({ type: 'preview.visibility', visible: panel.visible });
    tryStartProductionRun();
  });
  const configSub = vscode.workspace.onDidChangeConfiguration((event) => {
    if (affectsSetting(event, 'packageMap', resource)) requestReload();
    if (affectsSetting(event, 'highlighting.enabled', resource)) {
      void panel.webview.postMessage({ type: 'highlighting', enabled: getSetting('highlighting.enabled', resource, true) });
    }
    if (!affectsSetting(event, 'preview.nodeStyle', resource)) return;
    const updatedStyle = getSetting('preview.nodeStyle', resource, 'smooth-curves');
    currentStyle = updatedStyle;
    if (latestMessage.type === 'graph') {
      latestMessage = { ...latestMessage, style: updatedStyle };
    }
    if (ready && !disposed) {
      void panel.webview.postMessage({ type: 'style', style: updatedStyle });
    }
  });

  void extensionContext?.workspaceState.update(WORKSPACE_RUNBOOK_KEY, runbookPath);
  directGraphPanel = panel;
  panel.onDidDispose(() => {
    disposed = true;
    investigationRevision += 1;
    investigationStarting = false;
    runStartRevision += 1;
    loadRevision += 1;
    loadController?.abort();
    runSession?.dispose();
    const attachedInvestigation = investigationClient;
    if (attachedInvestigation) {
      attachedInvestigation.send({ type: 'session.detach', commandID: randomUUID() });
      const forceStop = setTimeout(() => attachedInvestigation.dispose(), 30_000);
      forceStop.unref();
    }
    runBridge?.dispose();
    hostActions.dispose();
    confirmedHostActionRequests.clear();
    messageSub.dispose();
    saveSub.dispose();
    configSub.dispose();
    visibilitySub.dispose();
    if (directGraphPanel === panel) {
      directGraphPanel = undefined;
    }
    if (activeLoadSavedRun === loadSavedRun) {
      activeLoadSavedRun = undefined;
    }
    rejectProductionRun(new Error('Yawr run panel was disposed before terminal completion.'));
  });
  activeLoadSavedRun = loadSavedRun;
  void reload().then(() => reconnectInvestigation());
  return panel;
}
