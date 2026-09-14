// Extension-host smoke tests — run via `npm run test:e2e` (vscode-test).
//
// These tests execute INSIDE the VS Code extension host. They cannot be run
// with plain `node --test` because they import `vscode`, which is only
// available as a built-in module inside the host process.
//
// Smoke coverage:
//   1. The extension activates without throwing.
//   2. All declared commands are registered after activation.
//
// Phase 2 tool-discovery, invocation, result-normalisation, auth-unavailable,
// timeout, and cancellation tests belong alongside these, once the LM tools
// API integration (vscode.lm.tools / invokeTool) is implemented.

import * as assert from 'assert';
import { execFile } from 'child_process';
import { EventEmitter } from 'events';
import * as path from 'path';
import { PassThrough } from 'stream';
import { promisify } from 'util';
import * as vscode from 'vscode';
const environmentValue = (environment: NodeJS.ProcessEnv, suffix: string): string | undefined =>
  environment[`YAWR_${suffix}`];

const EXTENSION_ID = 'ormasoftchile.yawr-preview';
const testStatePath = ['.vscode-test', 'runs', environmentValue(process.env, 'TEST_RUN_ID') ?? 'test', 'workspace'];
const execFileAsync = promisify(execFile);
const EXPECTED_COMMANDS = [
  'yawr.preview',
  'yawr.previewGraph',
  'yawr.validateInputs',
  'yawr.showServerLog',
];

function isGraphPreviewTab(tab: vscode.Tab): boolean {
  return tab.label === 'Yawr: enum.runbook.yaml';
}

function isTextTabFor(tab: vscode.Tab, uri: vscode.Uri): boolean {
  const input = tab.input as { uri?: unknown };
  return input !== null
    && typeof input === 'object'
    && String(input.uri) === uri.toString();
}

suite('Yawr extension smoke tests', () => {
  let sourceYawrBinaryPath = '';
  let ownsSourceYawrBinary = false;

  suiteSetup(async function () {
    this.timeout(120_000);
    const configuredBinary = environmentValue(process.env, 'E2E_BINARY')?.trim();
    if (configuredBinary) {
      sourceYawrBinaryPath = path.resolve(configuredBinary);
      try {
        await vscode.workspace.fs.stat(vscode.Uri.file(sourceYawrBinaryPath));
      } catch {
        assert.fail(`YAWR_E2E_BINARY does not exist: ${sourceYawrBinaryPath}`);
      }
      return;
    }
    const configuredCoreRoot = environmentValue(process.env, 'CORE_ROOT')?.trim();
    assert.ok(configuredCoreRoot, 'Set YAWR_E2E_BINARY or YAWR_CORE_ROOT before running source Extension Host tests.');
    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const binaryDir = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'bin');
    const binaryUri = vscode.Uri.joinPath(binaryDir, process.platform === 'win32' ? 'yawr-e2e.exe' : 'yawr-e2e');
    await vscode.workspace.fs.createDirectory(binaryDir);
    await execFileAsync('go', ['build', '-o', binaryUri.fsPath, './cmd/yawr'], {
      cwd: path.resolve(configuredCoreRoot),
      windowsHide: true,
    });
    sourceYawrBinaryPath = binaryUri.fsPath;
    ownsSourceYawrBinary = true;
  });

  suiteTeardown(async () => {
    if (!sourceYawrBinaryPath || !ownsSourceYawrBinary) return;
    try {
      await vscode.workspace.fs.delete(vscode.Uri.file(sourceYawrBinaryPath), { useTrash: false });
    } catch {
      // The test host may already have removed its generated workspace data.
    }
  });

  test('extension is present in the host and activates without error', async () => {
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(
      ext,
      `extension ${EXTENSION_ID} must be discoverable — check the extensionDevelopmentPath in .vscode-test.mjs`,
    );
    await ext.activate();
    assert.strictEqual(ext.isActive, true, 'extension must report isActive === true after activate()');
  });

  test('all package.json commands are registered after activation', async () => {
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();
    const registered = await vscode.commands.getCommands(true /* filterInternal */);
    for (const cmd of EXPECTED_COMMANDS) {
      assert.ok(
        registered.includes(cmd),
        `command "${cmd}" must be registered — if it is missing, the contribute.commands entry in package.json is wrong or activate() did not complete`,
      );
    }
  });

  test('bundled direct graph webview renders and refreshes graphjson without a server', async () => {
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-save.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    fixture.frames = [{ id: 'frame:root', runbook_id: 'enum-fixture', runbook_path: runbookUri.fsPath, depth: 0 }];
    fixture.groups = [{
      id: 'group:branch',
      kind: 'branch-arm',
      parent_node_id: 'branch',
      frame_id: 'frame:root',
      label: 'Selected route',
    }, {
      id: 'group:empty',
      kind: 'branch-arm',
      parent_node_id: 'branch',
      frame_id: 'frame:root',
      label: 'Empty route',
      index: 1,
    }];
    fixture.nodes[0].data.group_id = 'group:branch';
    fixture.nodes[0].data.frame_id = 'frame:root';
    fixture.nodes[0].parentNode = 'group:branch';
    fixture.nodes[0].extent = 'parent';
    fixture.nodes.unshift({
      id: 'branch',
      type: 'decision',
      data: {
        id: 'branch',
        kind: 'branch',
        title: 'Choose route',
        group_id: '',
        frame_id: 'frame:root',
        details: {
          kind: 'branch',
          arms: [{ label: 'Selected route', condition: 'route == "selected"', steps: 1 }],
        },
      },
      position: { x: 0, y: 0 },
    });
    fixture.edges = [{ id: 'e1', source: 'branch', target: 'end', type: 'branch-arm', label: 'selected' }];
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-save\nsteps: []\n'));

    let loadCount = 0;
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => {
          loadCount += 1;
          return fixture;
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type Rendered = {
      nodeCount: number;
      frameCount: number;
      edgeCount: number;
      style: string;
      nodeBorderRadius: string;
      edgeClassName: string;
    };
    const waitForRendered = (predicate: (value: Rendered) => boolean, failure: string) =>
      new Promise<Rendered>((resolve, reject) => {
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(failure));
        }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          if (typeof message !== 'object' || message === null) return;
          const candidate = message as {
            type?: unknown;
            nodeCount?: unknown;
            frameCount?: unknown;
            edgeCount?: unknown;
            style?: unknown;
            nodeBorderRadius?: unknown;
            edgeClassName?: unknown;
          };
          if (candidate.type !== 'rendered') return;
          const rendered = {
            nodeCount: Number(candidate.nodeCount),
            frameCount: Number(candidate.frameCount),
            edgeCount: Number(candidate.edgeCount),
            style: String(candidate.style),
            nodeBorderRadius: String(candidate.nodeBorderRadius),
            edgeClassName: String(candidate.edgeClassName),
          };
          if (!predicate(rendered)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve(rendered);
        });
      });

    const config = vscode.workspace.getConfiguration('yawr', runbookUri);
    const previousStyle = config.inspect<string>('preview.nodeStyle')?.workspaceValue;
    try {
      const rendered = await waitForRendered(
        (value) => value.nodeCount === 2 && value.frameCount === 2,
        'direct graph webview did not report rendered nodes',
      );
      assert.strictEqual(loadCount, 1, 'opening the panel must load the graph once');
      assert.strictEqual(rendered.edgeCount, 1, 'the grouped fixture has one edge');

      const inspectorState = new Promise<{ tabs: string[]; sections: string[] }>((resolve, reject) => {
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error('tailored branch inspector did not render'));
        }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const candidate = message as { type?: unknown; nodeID?: unknown; tabs?: unknown; sections?: unknown };
          if (candidate?.type !== 'inspector.state' || candidate.nodeID !== 'branch' ||
              !Array.isArray(candidate.tabs) || !Array.isArray(candidate.sections)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve({ tabs: candidate.tabs.map(String), sections: candidate.sections.map(String) });
        });
      });
      await panel.webview.postMessage({ type: 'test.action', action: 'select-node', name: 'branch' });
      const inspector = await inspectorState;
      assert.deepStrictEqual(inspector.tabs, ['Definition', 'Run', 'Debug']);
      assert.ok(inspector.sections.includes('Ordered arms'));

      const styleRenders = new Map<string, Rendered>([[rendered.style, rendered]]);
      for (const style of ['smooth-curves', 'minimalist', 'header-badges']) {
        if (styleRenders.has(style)) continue;
        const restyled = waitForRendered(
          (value) => value.style === style,
          `direct graph webview did not apply style ${style}`,
        );
        await config.update('preview.nodeStyle', style, vscode.ConfigurationTarget.Workspace);
        styleRenders.set(style, await restyled);
      }
      const visualTelemetry = Object.fromEntries(styleRenders);
      assert.strictEqual(
        new Set([...styleRenders.values()].map((value) => value.nodeBorderRadius)).size,
        3,
        `each configured style must produce a distinct computed node shape: ${JSON.stringify(visualTelemetry)}`,
      );
      assert.deepStrictEqual(
        new Set([...styleRenders.values()].map((value) => value.edgeClassName)),
        new Set([
          'react-flow__edge react-flow__edge-smoothstep nopan',
          'react-flow__edge react-flow__edge-straight nopan',
          'react-flow__edge react-flow__edge-step nopan',
        ]),
        'each configured style must select a distinct React Flow edge renderer',
      );
      const currentStyle = 'header-badges';
      const activeStyle = [...styleRenders.keys()].at(-1);
      if (activeStyle !== currentStyle) {
        const selected = waitForRendered(
          (value) => value.style === currentStyle,
          `direct graph webview did not restore style ${currentStyle}`,
        );
        await config.update('preview.nodeStyle', currentStyle, vscode.ConfigurationTarget.Workspace);
        await selected;
      }

      const reloaded = waitForRendered(
        (value) => value.style === currentStyle && value.nodeCount === 2 && value.frameCount === 2,
        'direct graph webview did not reload after the runbook was saved',
      );
      const document = await vscode.workspace.openTextDocument(runbookUri);
      const edit = new vscode.WorkspaceEdit();
      edit.insert(runbookUri, document.lineAt(document.lineCount - 1).range.end, '\n# saved\n');
      assert.strictEqual(await vscode.workspace.applyEdit(edit), true);
      assert.strictEqual(await document.save(), true);
      await reloaded;
      assert.strictEqual(loadCount, 2, 'saving the runbook must invoke the graph loader again');
      const runtimeState = await vscode.commands.executeCommand<{ mcpBridgeStarted: boolean }>(
        'yawr.test.getRuntimeState',
      );
      assert.deepStrictEqual(runtimeState, { mcpBridgeStarted: false });
    } finally {
      panel.dispose();
      await config.update('preview.nodeStyle', previousStyle, vscode.ConfigurationTarget.Workspace);
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('newer graph reload wins when an older route-test load resolves last', async function () {
    this.timeout(20_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-reload-race.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-reload-race\nsteps: []\n'));

    const graphDocument = (label: string) => {
      const document = JSON.parse(JSON.stringify(fixture));
      const previousNodeID = document.nodes[0].id;
      const nodeID = `${label}-node`;
      document.hash = `sha256:${label}`;
      document.runbook = { ...document.runbook, id: label, name: label };
      document.nodes[0] = {
        ...document.nodes[0],
        id: nodeID,
        data: { ...document.nodes[0].data, id: nodeID, step_id: nodeID, title: label },
      };
      document.edges = document.edges.map((edge: Record<string, unknown>) => ({
        ...edge,
        source: edge.source === previousNodeID ? nodeID : edge.source,
        target: edge.target === previousNodeID ? nodeID : edge.target,
      }));
      return document;
    };
    const documents = [graphDocument('initial'), graphDocument('older'), graphDocument('newer')];
    let documentLoadCount = 0;

    const routeTestArtifactsModule = require('../../routeTestArtifacts') as {
      loadRouteTestArtifacts: (projectRoot: string, runbook: string) => Promise<{ artifacts: unknown[]; warnings: string[] }>;
    };
    const originalLoadRouteTestArtifacts = routeTestArtifactsModule.loadRouteTestArtifacts;
    let routeTestLoadCount = 0;
    let olderRouteLoadStarted!: () => void;
    const olderLoading = new Promise<void>((resolve) => { olderRouteLoadStarted = resolve; });
    let releaseOlderRouteLoad: () => void = () => {};
    const olderRelease = new Promise<void>((resolve) => { releaseOlderRouteLoad = resolve; });
    routeTestArtifactsModule.loadRouteTestArtifacts = async () => {
      routeTestLoadCount += 1;
      if (routeTestLoadCount === 2) {
        olderRouteLoadStarted();
        await olderRelease;
      }
      return { artifacts: [], warnings: [] };
    };

    let panel: vscode.WebviewPanel | undefined;
    const publishedNodeIDs: string[][] = [];
    let messageSubscription: vscode.Disposable | undefined;
    try {
      panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
        'yawr.test.openDirectGraphPanel',
        runbookUri.fsPath,
        {
          documentLoader: async () => documents[Math.min(documentLoadCount++, documents.length - 1)],
        },
      );
      assert.ok(panel, 'test command must return the direct graph WebviewPanel');

      const waitForGraph = (nodeID: string, failure: string) => new Promise<void>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel!.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown; graphNodeIDs?: unknown };
          if (state?.type !== 'ui.state' || !Array.isArray(state.graphNodeIDs) || !state.graphNodeIDs.includes(nodeID)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve();
        });
      });
      messageSubscription = panel.webview.onDidReceiveMessage((message: unknown) => {
        const state = message as { type?: unknown; graphNodeIDs?: unknown };
        if (state?.type === 'ui.state' && Array.isArray(state.graphNodeIDs)) {
          publishedNodeIDs.push(state.graphNodeIDs.map(String));
        }
      });

      await waitForGraph('initial-node', 'initial graph did not render');
      const document = await vscode.workspace.openTextDocument(runbookUri);
      const saveMarker = async (marker: string) => {
        const edit = new vscode.WorkspaceEdit();
        edit.insert(runbookUri, document.lineAt(document.lineCount - 1).range.end, `\n# ${marker}\n`);
        assert.strictEqual(await vscode.workspace.applyEdit(edit), true);
        assert.strictEqual(await document.save(), true);
      };

      await saveMarker('older reload');
      await olderLoading;
      const newerRendered = waitForGraph('newer-node', 'newer graph did not publish while the older route-test load was pending');
      await saveMarker('newer reload');
      await newerRendered;

      releaseOlderRouteLoad();
      await new Promise<void>((resolve) => setTimeout(resolve, 750));

      assert.strictEqual(
        publishedNodeIDs.some((nodeIDs) => nodeIDs.includes('older-node')),
        false,
        `superseded graph published after the newer revision: ${JSON.stringify(publishedNodeIDs)}`,
      );
    } finally {
      releaseOlderRouteLoad();
      messageSubscription?.dispose();
      panel?.dispose();
      routeTestArtifactsModule.loadRouteTestArtifacts = originalLoadRouteTestArtifacts;
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('save-triggered reload disables Run and Debug and host-rejects a racing run.start', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-run-reload-race.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-run-reload-race\nsteps: []\n'));

    let loadCount = 0;
    let reloadStarted!: () => void;
    const reloading = new Promise<void>((resolve) => { reloadStarted = resolve; });
    let releaseReload: () => void = () => {};
    const reloadRelease = new Promise<void>((resolve) => { releaseReload = resolve; });
    let beforeSpawnCount = 0;
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => {
          loadCount += 1;
          if (loadCount === 2) {
            reloadStarted();
            await reloadRelease;
          }
          return fixture;
        },
        beforeSpawn: async () => {
          beforeSpawnCount += 1;
          throw new Error('run reached beforeSpawn while graph reload was active');
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = {
      runStatus: string;
      runError?: string;
      inputValues: Record<string, string>;
      runButtonCount: number;
      debugRunButtonCount: number;
      breakpointCount: number;
    };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        let lastState: UIState | undefined;
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`${failure}; last state: ${JSON.stringify(lastState)}`));
        }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type === 'ui.state') lastState = state;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });
    const waitForDOM = (predicate: (state: Record<string, unknown>) => boolean, failure: string) =>
      new Promise<Record<string, unknown>>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as Record<string, unknown>;
          if (state?.type !== 'test.dom.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });

    try {
      await waitForUI(
        (state) => state.runButtonCount === 1 && state.debugRunButtonCount === 1,
        'direct graph did not render run controls',
      );
      const inputReady = waitForUI(
        (state) => state.inputValues.env_name === 'prod',
        'direct graph did not accept the required input',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'set-input', name: 'env_name', value: 'prod' });
      await inputReady;
      const breakpointReady = waitForUI(
        (state) => state.breakpointCount === 1,
        'direct graph did not enable a debug breakpoint',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'toggle-breakpoint', name: 'end', value: 'before' });
      await breakpointReady;

      const document = await vscode.workspace.openTextDocument(runbookUri);
      const edit = new vscode.WorkspaceEdit();
      edit.insert(runbookUri, document.lineAt(document.lineCount - 1).range.end, '\n# trigger paused reload\n');
      assert.strictEqual(await vscode.workspace.applyEdit(edit), true);
      assert.strictEqual(await document.save(), true);
      await reloading;

      const controls = waitForDOM((state) => state.clicked === '__inspect__', 'run controls were not inspected');
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: '__inspect__' });
      const controlsState = await controls;

      const rejected = waitForUI((state) => typeof state.runError === 'string', 'racing run.start was not rejected');
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      const rejectedState = await rejected;

      const disabledButtons = Array.isArray(controlsState.disabledButtons)
        ? controlsState.disabledButtons.map(String)
        : [];
      assert.deepStrictEqual({
        runDisabled: disabledButtons.includes('Run'),
        debugDisabled: disabledButtons.includes('Debug Run'),
        beforeSpawnCount,
        runError: rejectedState.runError,
      }, {
        runDisabled: true,
        debugDisabled: true,
        beforeSpawnCount: 0,
        runError: 'The runbook graph is reloading. Wait for it to finish before starting a run.',
      });
    } finally {
      releaseReload();
      panel.dispose();
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('multi-select choice enforces max at the exact boundary', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-choice-max.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    fixture.inputs = [];
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-choice-max\nsteps: []\n'));

    const fakeChild = new EventEmitter() as EventEmitter & {
      stdin: PassThrough;
      stdout: PassThrough;
      stderr: PassThrough;
      killed: boolean;
      exitCode: number | null;
      kill(): boolean;
    };
    fakeChild.stdin = new PassThrough();
    fakeChild.stdout = new PassThrough();
    fakeChild.stderr = new PassThrough();
    fakeChild.killed = false;
    fakeChild.exitCode = null;
    fakeChild.kill = () => { fakeChild.killed = true; return true; };
    const writeFrame = (frame: Record<string, unknown>) => {
      fakeChild.stdout.write(`${JSON.stringify({ ...frame, version: 'yawr.stdio/v1' })}\n`);
    };
    let answerReceived!: (answer: Record<string, unknown>) => void;
    const submittedAnswer = new Promise<Record<string, unknown>>((resolve) => { answerReceived = resolve; });
    let commandBuffer = '';
    fakeChild.stdin.setEncoding('utf8');
    fakeChild.stdin.on('data', (chunk: string) => {
      commandBuffer += chunk;
      let newline = commandBuffer.indexOf('\n');
      while (newline >= 0) {
        const command = JSON.parse(commandBuffer.slice(0, newline)) as Record<string, unknown>;
        commandBuffer = commandBuffer.slice(newline + 1);
        if (command.type === 'interaction.answer') answerReceived(command.answer as Record<string, unknown>);
        newline = commandBuffer.indexOf('\n');
      }
    });

    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => fixture,
        spawnRun: () => {
          setImmediate(() => {
            writeFrame({ type: 'run.started', runID: 'run-choice-max', status: 'running' });
            writeFrame({
              type: 'interaction.pending', runID: 'run-choice-max', turnID: 'turn-choice-max',
              interaction: {
                type: 'pending', runID: 'run-choice-max', turnID: 'turn-choice-max', stepID: 'choose',
                kind: 'choice', title: 'Choose two', prompt: 'Select at most two options.',
                multiple: true, min: 1, max: 2,
                options: [
                  { value: 'one', label: 'One' },
                  { value: 'two', label: 'Two' },
                  { value: 'three', label: 'Three' },
                ],
              },
            });
          });
          return fakeChild;
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = { runStatus: string; pendingKind?: string; runButtonCount: number };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });
    const toggleChoice = (name: string) => new Promise<Record<string, unknown>>(async (resolve, reject) => {
      const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(`choice ${name} was not toggled`)); }, 10_000);
      const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
        const state = message as Record<string, unknown>;
        if (state?.type !== 'test.dom.state' || state.choice !== name) return;
        clearTimeout(timeout); subscription.dispose(); resolve(state);
      });
      await panel.webview.postMessage({ type: 'test.action', action: 'toggle-choice', name });
    });

    try {
      await waitForUI((state) => state.runButtonCount === 1, 'direct choice graph did not render');
      const pending = waitForUI(
        (state) => state.runStatus === 'waiting' && state.pendingKind === 'choice',
        'multi-select interaction did not become pending',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      await pending;

      await toggleChoice('One');
      const boundary = await toggleChoice('Two');
      const afterThird = await toggleChoice('Three');
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Continue' });
      const answer = await Promise.race([
        submittedAnswer,
        new Promise<never>((_, reject) => setTimeout(() => reject(new Error('choice answer was not submitted')), 10_000)),
      ]);

      assert.deepStrictEqual({
        boundarySelected: boundary.selectedChoices,
        boundaryDisabled: boundary.disabledChoices,
        boundaryContinueDisabled: boundary.continueDisabled,
        boundaryLimit: boundary.choiceLimitText,
        afterThirdSelected: afterThird.selectedChoices,
        submitted: answer.selected,
      }, {
        boundarySelected: ['One', 'Two'],
        boundaryDisabled: ['Three'],
        boundaryContinueDisabled: false,
        boundaryLimit: '2 of 2 selected. Deselect an option to choose another.',
        afterThirdSelected: ['One', 'Two'],
        submitted: ['one', 'two'],
      });
    } finally {
      panel.dispose();
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('direct run controls drive stdio input, choice, status, and completion', async () => {
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-run.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    fixture.inputs.push({ name: 'access_token', type: 'secret', required: true });
    const nodeTemplate = fixture.nodes[0];
    const executionNode = (id: string, stepID: string, title: string, kind: string, order: number) => ({
      ...nodeTemplate,
      id,
      type: kind === 'end' ? 'terminal' : 'step',
      data: { ...nodeTemplate.data, id, step_id: stepID, title, kind, order },
    });
    fixture.nodes = [
      executionNode('fallback-before-canonical', 'canonical-started', 'Wrong fallback match', 'display', 0),
      executionNode('canonical-started', 'started', 'Canonical start', 'display', 1),
      executionNode('choice-node', 'choice', 'Choose next action', 'choice', 2),
      executionNode('parent-container', 'parent', 'Parent workflow', 'include', 3),
      executionNode('resumed-node', 'resumed', 'Resume workflow', 'display', 4),
      executionNode('host-node', 'host', 'Open supporting tool', 'host_action', 5),
      executionNode('end', 'end', 'Finish workflow', 'end', 6),
    ];
    fixture.edges = fixture.nodes.slice(1).map((node: { id: string }, index: number) => ({
      id: `execution-edge-${index}`,
      source: fixture.nodes[index].id,
      target: node.id,
    }));
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-run\nsteps: []\n'));

    let loadCount = 0;
    let resolveDeferredReload!: () => void;
    const deferredReload = new Promise<void>((resolve) => { resolveDeferredReload = resolve; });
    let spawnArgs: string[] = [];
    const commands: unknown[] = [];
    const fakeChild = new EventEmitter() as EventEmitter & {
      stdin: PassThrough;
      stdout: PassThrough;
      stderr: PassThrough;
      killed: boolean;
      exitCode: number | null;
      kill(): boolean;
    };
    fakeChild.stdin = new PassThrough();
    fakeChild.stdout = new PassThrough();
    fakeChild.stderr = new PassThrough();
    fakeChild.killed = false;
    fakeChild.exitCode = null;
    fakeChild.kill = () => {
      fakeChild.killed = true;
      return true;
    };
    const writeFrame = (frame: Record<string, unknown>) => {
      fakeChild.stdout.write(`${JSON.stringify({ ...frame, version: 'yawr.stdio/v1' })}\n`);
    };
    let commandBuffer = '';
    fakeChild.stdin.setEncoding('utf8');
    fakeChild.stdin.on('data', (chunk: string) => {
      commandBuffer += chunk;
      let newline = commandBuffer.indexOf('\n');
      while (newline >= 0) {
        const line = commandBuffer.slice(0, newline);
        commandBuffer = commandBuffer.slice(newline + 1);
        const command = JSON.parse(line) as Record<string, unknown>;
        commands.push(command);
        if (command.type === 'interaction.answer') {
          const answer = command.answer as { kind?: string } | undefined;
          if (answer?.kind === 'choice') {
            writeFrame({ type: 'interaction.resolved', runID: 'run-ui', turnID: 'turn-choice' });
            writeFrame({
              type: 'run.event',
              runID: 'run-ui',
              event: {
                kind: 'step/completed',
                run_id: 'run-ui',
                sequence: 2,
                payload: { node_id: 'parent-container', step_id: 'parent' },
              },
            });
            writeFrame({
              type: 'run.event',
              runID: 'run-ui',
              event: {
                kind: 'step/resumed',
                run_id: 'run-ui',
                sequence: 3,
                payload: { node_id: 'resumed-node', step_id: 'resumed' },
              },
            });
          } else if (answer?.kind === 'host_action') {
            writeFrame({ type: 'interaction.resolved', runID: 'run-ui', turnID: 'turn-host' });
            writeFrame({
              type: 'run.event',
              runID: 'run-ui',
              event: {
                kind: 'step/completed',
                run_id: 'run-ui',
                sequence: 4,
                payload: { node_id: 'parent-container', step_id: 'parent', duration_ms: 12 },
              },
            });
            writeFrame({ type: 'run.finished', runID: 'run-ui', status: 'completed' });
            setImmediate(() => {
              fakeChild.exitCode = 0;
              fakeChild.emit('exit', 0, null);
              fakeChild.emit('close', 0, null);
            });
          }
        }
        newline = commandBuffer.indexOf('\n');
      }
    });

    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => {
          loadCount += 1;
          if (loadCount === 2) resolveDeferredReload();
          return fixture;
        },
        spawnRun: (_binary: string, args: string[]) => {
          spawnArgs = args;
          setImmediate(() => {
            writeFrame({ type: 'run.started', runID: 'run-ui', status: 'running' });
            writeFrame({
              type: 'run.event',
              runID: 'run-ui',
              event: {
                kind: 'step/started',
                run_id: 'run-ui',
                sequence: 1,
                payload: { node_id: 'canonical-started', step_id: 'started' },
              },
            });
          });
          return fakeChild;
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = {
      runID?: string;
      runStatus: string;
      pendingKind?: string;
      inputCount: number;
      inputValues: Record<string, string>;
      runButtonCount: number;
      resetButtonCount: number;
      cancelButtonCount: number;
      nodeStatuses: Record<string, string>;
      executionNodeID?: string;
      executionPositionLabel?: string;
      executionPositionTitle?: string;
      currentExecutionMarkerCount: number;
      lastExecutionMarkerCount: number;
      visibleButtons: string[];
    };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(failure));
        }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          if (typeof message !== 'object' || message === null) return;
          const candidate = message as { type?: unknown } & UIState;
          if (candidate.type !== 'ui.state' || !predicate(candidate)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve(candidate);
        });
      });

    try {
      const initial = await waitForUI(
        (state) => state.inputCount === 2 && state.runButtonCount === 1,
        'direct webview did not render its input and Run controls',
      );
      assert.strictEqual(initial.cancelButtonCount, 0);

      const inputReady = waitForUI(
        (state) => state.inputValues.env_name === 'prod',
        'direct webview did not accept the run input',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'set-input', name: 'env_name', value: 'prod' });
      await inputReady;
      const secretReady = waitForUI(
        (state) => state.inputValues.access_token === '<redacted>',
        'direct webview did not mask the secret input',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'set-input', name: 'access_token', value: 'SENTINEL-private' });
      await secretReady;

      const started = waitForUI(
        (state) => state.runStatus === 'running' && state.executionNodeID === 'canonical-started',
        'direct webview did not identify the canonical started node',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      const startedState = await started;
      assert.strictEqual(startedState.executionPositionTitle, 'Canonical start');
      assert.strictEqual(startedState.currentExecutionMarkerCount, 1);

      const waiting = waitForUI(
        (state) => state.runStatus === 'waiting' && state.pendingKind === 'choice',
        'direct webview did not reach the choice interaction',
      );
      writeFrame({
        type: 'interaction.pending',
        runID: 'run-ui',
        turnID: 'turn-choice',
        interaction: {
          type: 'pending',
          runID: 'run-ui',
          turnID: 'turn-choice',
          stepID: 'choice',
          nodeID: 'choice-node',
          kind: 'choice',
          prompt: 'Continue?',
          options: [{ value: 'o:0', label: 'Continue' }],
        },
      });
      const waitingState = await waiting;
      assert.strictEqual(waitingState.cancelButtonCount, 1);
      assert.strictEqual(waitingState.nodeStatuses['canonical-started'], 'running');
      assert.strictEqual(waitingState.executionNodeID, 'choice-node');
      assert.strictEqual(waitingState.executionPositionTitle, 'Choose next action');
      assert.strictEqual(waitingState.executionPositionLabel, 'Current step');
      assert.strictEqual(waitingState.currentExecutionMarkerCount, 1);
      assert.strictEqual(waitingState.lastExecutionMarkerCount, 0);
      assert.ok(waitingState.visibleButtons.includes('Locate'));
      const activeDocument = await vscode.workspace.openTextDocument(runbookUri);
      const activeEdit = new vscode.WorkspaceEdit();
      activeEdit.insert(runbookUri, activeDocument.lineAt(activeDocument.lineCount - 1).range.end, '\n# saved during run\n');
      assert.strictEqual(await vscode.workspace.applyEdit(activeEdit), true);
      assert.strictEqual(await activeDocument.save(), true);
      assert.strictEqual(loadCount, 1, 'saving during an active run must defer graph reload');
      assert.ok(spawnArgs.includes('--stdio'));
      assert.ok(spawnArgs.includes('--configure'));
      assert.ok(spawnArgs.includes('env_name=prod'));
      assert.ok(!spawnArgs.some((arg) => arg.includes('SENTINEL-private')));

      const resumed = waitForUI(
        (state) => state.runStatus === 'running' && state.executionNodeID === 'resumed-node',
        'direct webview did not identify the resumed node after parent completion',
      );
      await panel.webview.postMessage({
        type: 'test.action',
        action: 'answer',
        answer: { kind: 'choice', selected: ['o:0'] },
      });
      const resumedState = await resumed;
      assert.strictEqual(resumedState.nodeStatuses['parent-container'], 'completed');
      assert.strictEqual(resumedState.executionPositionTitle, 'Resume workflow');

      const completed = waitForUI(
        (state) => state.runStatus === 'completed' && state.nodeStatuses['parent-container'] === 'completed' &&
          state.currentExecutionMarkerCount === 0,
        'direct webview did not complete after the host interaction',
      );
      writeFrame({
        type: 'interaction.pending',
        runID: 'run-ui',
        turnID: 'turn-host',
        interaction: {
          type: 'pending',
          runID: 'run-ui',
          turnID: 'turn-host',
          stepID: 'host',
          nodeID: 'host-node',
          kind: 'host_action',
          correlationID: 'corr-host',
          host_action: { capability: 'test.echo', request: { echo: 'hello' } },
        },
      });
      const completedState = await completed;
      await Promise.race([
        deferredReload,
        new Promise<never>((_, reject) => setTimeout(() => reject(new Error('deferred graph reload did not run after terminal completion')), 10_000)),
      ]);
      assert.strictEqual(loadCount, 2, 'terminal completion must apply one deferred graph reload');
      assert.strictEqual(completedState.runButtonCount, 1);
      assert.strictEqual(completedState.resetButtonCount, 1);
      assert.strictEqual(completedState.cancelButtonCount, 0);
      assert.strictEqual(completedState.executionNodeID, 'host-node');
      assert.strictEqual(completedState.executionPositionTitle, 'Open supporting tool');
      assert.strictEqual(completedState.executionPositionLabel, 'Last reached');
      assert.strictEqual(completedState.lastExecutionMarkerCount, 1);
      assert.strictEqual(completedState.currentExecutionMarkerCount, 0);
      assert.strictEqual(commands.length, 3);
      assert.deepStrictEqual(commands[0], {
        type: 'run.configure',
        inputs: { access_token: 'SENTINEL-private' },
      });
      assert.deepStrictEqual(commands[1], {
        type: 'interaction.answer',
        runID: 'run-ui',
        turnID: 'turn-choice',
        answer: { kind: 'choice', selected: ['o:0'] },
      });
      const hostCommand = commands[2] as {
        type: string;
        runID: string;
        turnID: string;
        answer: Record<string, unknown>;
      };
      assert.strictEqual(hostCommand.type, 'interaction.answer');
      assert.strictEqual(hostCommand.runID, 'run-ui');
      assert.strictEqual(hostCommand.turnID, 'turn-host');
      assert.strictEqual(hostCommand.answer.kind, 'host_action');
      assert.strictEqual(hostCommand.answer.status, 'completed');
      assert.deepStrictEqual(hostCommand.answer.result, { echo: 'hello' });

      const startingAgain = waitForUI(
        (state) => state.runStatus === 'starting' && state.executionNodeID === undefined,
        'a new run did not clear the retained execution position',
      );
      await panel.webview.postMessage({ type: 'run.starting' });
      const startingAgainState = await startingAgain;
      assert.strictEqual(startingAgainState.executionPositionLabel, undefined);
      assert.strictEqual(startingAgainState.currentExecutionMarkerCount, 0);
      assert.strictEqual(startingAgainState.lastExecutionMarkerCount, 0);

      const cancelledAgain = waitForUI(
        (state) => state.runStatus === 'cancelled',
        'the synthetic second run did not reach a terminal state',
      );
      await panel.webview.postMessage({
        type: 'run.frame',
        frame: { type: 'run.finished', version: 'yawr.stdio/v1', status: 'cancelled' },
      });
      await cancelledAgain;

      const reset = waitForUI(
        (state) => state.runStatus === 'idle' && state.runID === undefined && Object.keys(state.nodeStatuses).length === 0,
        'direct webview did not reset completed execution state',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'reset' });
      const resetState = await reset;
      assert.strictEqual(resetState.runButtonCount, 1);
      assert.strictEqual(resetState.resetButtonCount, 0);
      assert.strictEqual(resetState.cancelButtonCount, 0);
      assert.strictEqual(resetState.executionNodeID, undefined);
      assert.strictEqual(resetState.executionPositionLabel, undefined);
      assert.strictEqual(resetState.currentExecutionMarkerCount, 0);
      assert.strictEqual(resetState.lastExecutionMarkerCount, 0);
      assert.strictEqual(resetState.inputValues.env_name, 'prod');
      assert.strictEqual(resetState.inputValues.access_token, '<redacted>');
    } finally {
      panel.dispose();
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('execution transitions keep one current marker, fixed geometry and bounded offscreen pans', async function () {
    this.timeout(60_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext);
    await ext.activate();
    const workspace = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspace);
    const uri = vscode.Uri.joinPath(workspace.uri, ...testStatePath, 'execution-transitions.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspace.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    const template = fixture.nodes[0];
    fixture.inputs = []; fixture.groups = []; fixture.frames = [];
    fixture.nodes = Array.from({ length: 10 }, (_, index) => ({
      ...template, id: `step-${index}`, type: 'step',
      data: { ...template.data, id: `step-${index}`, step_id: `step-${index}`,
        kind: index === 2 ? 'results' : 'assign', title: `Transition ${index}`, group_id: '', frame_id: '', order: index },
    }));
    fixture.edges = fixture.nodes.slice(1).map((node: { id: string }, index: number) => ({
      id: `edge-${index}`, source: `step-${index}`, target: node.id, type: 'sequence',
    }));
    await vscode.workspace.fs.writeFile(uri, Buffer.from('apiVersion: yawr.runbook/v1\nid: execution-transitions\nsteps: []\n'));
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>('yawr.test.openDirectGraphPanel',
      uri.fsPath, { documentLoader: async () => fixture });
    assert.ok(panel);
    type Viewport = { x: number; y: number; zoom: number };
    type Visibility = {
      currentNodeID?: string; currentMarkerCount: number; topExecutionLogCount: number; selectedID?: string;
      viewport: Viewport; canvas: { width: number; height: number };
      positions: Array<{ id: string; position: { x: number; y: number }; width: number; height: number }>;
      nodes: Array<{ id: string; intersects: boolean }>;
    };
    type Sample = {
      currentIDs: string[]; selectedIDs: string[]; progressing: string[]; topLogCount: number; reducedMotion: boolean;
      viewport: Viewport; canvas: object; positions: Visibility['positions']; at: number;
      runStatus?: string; resultsState?: string;
    };
    const message = <T,>(type: string): Promise<T> => new Promise((resolve, reject) => {
      const timer = setTimeout(() => { subscription.dispose(); reject(new Error(`Missing ${type}`)); }, 10_000);
      const subscription = panel.webview.onDidReceiveMessage(value => {
        if (value.type !== type) return;
        clearTimeout(timer); subscription.dispose(); resolve(value as T);
      });
    });
    const inspect = async () => {
      const result = message<Visibility>('graph.visibility');
      await panel.webview.postMessage({ type: 'test.action', action: 'inspect-graph-visibility' });
      return result;
    };
    const settle = async (predicate: (value: Visibility) => boolean) => {
      for (let attempt = 0; attempt < 60; attempt++) {
        const value = await inspect();
        if (predicate(value)) return value;
        await new Promise(resolve => setTimeout(resolve, 30));
      }
      throw new Error('Execution view did not settle');
    };
    let sequence = 0;
    const step = (id: string, kind: string) => panel.webview.postMessage({ type: 'run.frame', frame: {
      type: 'run.event', event: { run_id: 'transition-run', kind: `step/${kind}`, sequence: ++sequence,
        payload: { qualified_node_id: id, invocation: 1 } },
    } });
    const trace = async (transition: () => Promise<unknown>, pacing = false) => {
      const ready = message('execution.transition-sampling');
      const result = message<{ samples: Sample[] }>('execution.transition-samples');
      await panel.webview.postMessage({ type: 'test.action', action: 'sample-execution-transition', value: pacing ? 'pacing' : undefined });
      await ready;
      await transition();
      return (await result).samples;
    };
    const { publicationDigest }: { publicationDigest(value: unknown): string } =
      require(path.join(ext.extensionPath, 'out', 'typedResults'));
    const resultBody = {
      schema_version: 'yawr.run-results/v1', publication_id: 'transition-run/root/results/1',
      plan_snapshot_digest: `sha256:${'a'.repeat(64)}`, checkpoint_sequence: 42,
      origin: { node_id: 'step-2', invocation: 1 },
      outputs: { retained: { type: 'boolean', value: true } },
    };
    const resultsAvailability = { state: 'available', publication: { ...resultBody, digest: publicationDigest(resultBody) } };
    try {
      await message('ui.state');
      await panel.webview.postMessage({ type: 'run.starting' });
      await panel.webview.postMessage({ type: 'run.frame', frame: { type: 'run.started', runID: 'transition-run' } });
      await step('step-0', 'started');
      const started = await settle(value => value.currentNodeID === 'step-0' && value.currentMarkerCount === 1 &&
        !!value.positions?.find(node => node.id === 'step-0' && node.width > 0));
      const first = started.positions.find(node => node.id === 'step-0')!;
      const viewport = { x: started.canvas.width / 2 - first.position.x - first.width / 2,
        y: 60 - first.position.y, zoom: 1 };
      await panel.webview.postMessage({ type: 'test.action', action: 'set-graph-viewport', value: JSON.stringify(viewport) });
      await panel.webview.postMessage({ type: 'test.action', action: 'select-node', name: 'step-0' });
      await settle(value => value.selectedID === 'step-0' && Math.abs(value.viewport.y - viewport.y) < 0.1);
      const stable = await trace(async () => {});
      assert.ok(stable.every(value => value.currentIDs.join() === 'step-0' && value.progressing.join() === 'step-0'));
      const baseline = stable.at(-1)!;
      console.log(`Execution transition sampling: reducedMotion=${baseline.reducedMotion}, 30 frames per transition`);
      const routine = await trace(async () => {
        await step('step-0', 'completed');
        await new Promise(resolve => setTimeout(resolve, 100));
        await step('step-1', 'started');
      });
      assert.ok(routine.every(value => value.currentIDs.length === 1), 'no frame may transiently lose its current marker');
      assert.ok(routine.some(value => value.currentIDs[0] === 'step-1'));
      for (const value of [...stable, ...routine]) {
        assert.equal(value.topLogCount, 0);
        assert.deepStrictEqual(value.selectedIDs, ['step-0']);
        assert.deepStrictEqual(value.canvas, baseline.canvas);
        assert.deepStrictEqual(value.positions, baseline.positions);
        assert.deepStrictEqual(value.viewport, baseline.viewport, 'visible advances must not pan or zoom');
      }
      const distant = await trace(async () => {
        await step('step-1', 'completed');
        await step('step-9', 'started');
      });
      assert.ok(distant.every(value => value.currentIDs.length === 1 && value.viewport.zoom === 1));
      assert.ok(new Set(distant.map(value => Math.round(value.viewport.y))).size > (baseline.reducedMotion ? 1 : 2),
        `offscreen pan must ${baseline.reducedMotion ? 'reveal the target' : 'have intermediate frames'}: ${
          JSON.stringify(distant.map(value => ({ ids: value.currentIDs, y: value.viewport.y })))}`);
      const end = await settle(value => value.currentNodeID === 'step-9' && value.nodes.some(node => node.id === 'step-9' && node.intersects));
      assert.equal(end.selectedID, 'step-0');
      assert.equal(end.topExecutionLogCount, 0);
      await step('step-9', 'completed');
      await panel.webview.postMessage({ type: 'run.frame', frame: { type: 'run.finished', status: 'completed' } });
      await settle(value => value.currentMarkerCount === 0);
      for (const interval of [undefined, 500, 0]) {
        await panel.webview.postMessage({ type: 'run.starting', minimumStepDisplayMs: interval });
        await panel.webview.postMessage({ type: 'run.frame', frame: { type: 'run.started', runID: 'transition-run' } });
        await panel.webview.postMessage({ type: 'test.action', action: 'set-graph-viewport',
          value: JSON.stringify({ x: 100, y: 30, zoom: 0.2 }) });
        const samples = await trace(async () => {
          await step('step-0', 'started'); await step('step-0', 'completed');
          await step('step-1', 'started'); await step('step-1', 'completed');
          await step('step-2', 'started');
        }, true);
        const active = samples.slice(samples.findIndex(value => value.currentIDs.length));
        assert.ok(active.every(value => value.currentIDs.length === 1 && value.topLogCount === 0));
        const changes = active.filter((value, index) => index === 0 || value.currentIDs[0] !== active[index - 1].currentIDs[0]);
        if (interval === 0) {
          assert.equal(changes.at(-1)?.currentIDs[0], 'step-2');
          assert.ok(changes.at(-1)!.at - samples[0].at < 150, 'disabled pacing must converge without a timer backlog');
        } else {
          assert.deepStrictEqual(changes.map(value => value.currentIDs[0]), ['step-0', 'step-1', 'step-2']);
          for (let index = 1; index < changes.length; index++) {
            const duration = changes[index].at - changes[index - 1].at;
            assert.ok(duration >= (interval ?? 200) - 35, `pacing ${interval ?? 200}ms: observed ${duration}ms (35ms frame/IPC tolerance)`);
          }
        }
        console.log(`Visual pacing ${interval ?? 200}ms: ${changes.map(value => `${value.currentIDs[0]}@${value.at.toFixed(1)}`).join(', ')}`);
        await panel.webview.postMessage({ type: 'run.frame', frame: { type: 'run.finished', status: 'completed' } });
        await settle(value => value.currentMarkerCount === 0);
      }
      for (const interval of [200, 500]) {
        await panel.webview.postMessage({ type: 'test.action', action: 'select-node', name: 'step-0' });
        await panel.webview.postMessage({ type: 'test.action', action: 'set-graph-viewport',
          value: JSON.stringify({ x: 100, y: 30, zoom: 0.2 }) });
        const samples = await trace(async () => {
          await panel.webview.postMessage({ type: 'run.starting', minimumStepDisplayMs: interval });
          await panel.webview.postMessage({ type: 'run.frame', frame: { type: 'run.started', runID: 'transition-run' } });
          await step('runtime-only-wrapper', 'started');
          for (const nodeID of ['step-0', 'step-1', 'step-2', 'step-3']) {
            await new Promise(resolve => setTimeout(resolve, 45));
            await step(nodeID, 'started');
            await step(nodeID, 'completed');
          }
          await step('runtime-only-wrapper', 'completed');
          await panel.webview.postMessage({ type: 'run.frame', frame: { type: 'run.event',
            event: { kind: 'run/completed', run_id: 'transition-run', sequence: ++sequence } } });
          await panel.webview.postMessage({ type: 'run.frame', frame: {
            type: 'run.finished', status: 'completed', resultsAvailability,
          } });
          await panel.webview.postMessage({ type: 'run.exit', code: 0, signal: null });
          await panel.webview.postMessage({ type: 'loading' });
          await panel.webview.postMessage({ type: 'graph', document: fixture, style: 'smooth-curves', testMode: true });
        }, true);
        const first = samples.findIndex(value => value.currentIDs.length > 0);
        assert.ok(first >= 0, 'completion must not erase the initial current step before paint');
        const playback = samples.slice(first);
        const finished = playback.findIndex(value => value.currentIDs.length === 0);
        assert.ok(finished > 0, 'visual playback must finish after its final dwell');
        const active = playback.slice(0, finished);
        assert.ok(active.every(value => value.currentIDs.length === 1), 'no no-current frames inside completed-run playback');
        const transitions = playback.slice(0, finished + 1).filter((value, index) =>
          index === 0 || value.currentIDs[0] !== playback[index - 1].currentIDs[0]);
        assert.deepStrictEqual(transitions.map(value => value.currentIDs[0]),
          ['step-0', 'step-1', 'step-2', 'step-3', undefined]);
        for (let index = 1; index < transitions.length; index++) {
          const elapsed = transitions[index].at - transitions[index - 1].at;
          assert.ok(elapsed >= interval - 35,
            `${transitions[index - 1].currentIDs[0]} held ${elapsed}ms after runtime completion, minimum ${interval - 35}ms`);
        }
        const internalCompletion = active.find(value => value.runStatus === 'completed' && value.resultsState === 'available');
        assert.equal(internalCompletion?.currentIDs[0], 'step-0', 'runtime completion and Results must be available while later visuals remain queued');
        for (const value of active) {
          assert.deepStrictEqual(value.selectedIDs, ['step-0'], 'automatic Results selection must wait for playback');
          assert.deepStrictEqual(value.progressing, value.currentIDs, 'the paced current step keeps its progress treatment');
          assert.deepStrictEqual(value.positions, active[0].positions);
          assert.deepStrictEqual(value.canvas, active[0].canvas);
          assert.equal(value.viewport.zoom, 0.2);
          assert.deepStrictEqual(value.viewport, active[0].viewport, 'on-screen playback must not pan the viewport');
          assert.equal(value.topLogCount, 0);
        }
        assert.ok(playback.slice(finished).every(value => value.currentIDs.length === 0), 'duplicate completion and exit must not replay');
        await settle(value => value.currentMarkerCount === 0 && value.selectedID === 'step-2');
        console.log(`Completed-run pacing ${interval}ms: ${transitions.map(value => `${value.currentIDs[0] ?? 'drained'}@${value.at.toFixed(1)}`).join(', ')}`);
      }
      await panel.webview.postMessage({ type: 'run.starting', minimumStepDisplayMs: 5000 });
      await panel.webview.postMessage({ type: 'run.frame', frame: { type: 'run.started', runID: 'transition-run' } });
      await step('step-0', 'started'); await step('step-1', 'started');
      const failureAt = Date.now();
      await step('step-9', 'failed');
      await settle(value => value.currentNodeID === 'step-9');
      assert.ok(Date.now() - failureAt < 500, 'failure must bypass the five-second visual queue');
      const promptAt = Date.now();
      await panel.webview.postMessage({ type: 'run.frame', frame: { type: 'interaction.pending', interaction: {
        runID: 'transition-run', turnID: 'pacing-input', nodeID: 'step-8', stepID: 'step-8', kind: 'choice',
        prompt: 'Immediate pacing control', options: [{ value: 'continue', label: 'Continue' }],
      } } });
      await settle(value => value.currentNodeID === 'step-8');
      assert.ok(Date.now() - promptAt < 500, 'interaction must bypass the visual queue');
      const completedAt = Date.now();
      await panel.webview.postMessage({ type: 'run.frame', frame: { type: 'run.finished', status: 'cancelled' } });
      await settle(value => value.currentMarkerCount === 0);
      assert.ok(Date.now() - completedAt < 500, 'runtime completion must not drain the visual queue');
    } finally {
      panel.dispose();
      await vscode.workspace.fs.delete(uri, { useTrash: false });
    }
  });

  test('direct XTS handoff waits for Open XTS and collector review submits once', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-xts-review.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    fixture.inputs = [];
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-xts-review\nsteps: []\n'));

    const answers: Array<Record<string, unknown>> = [];
    const fakeChild = new EventEmitter() as EventEmitter & {
      stdin: PassThrough;
      stdout: PassThrough;
      stderr: PassThrough;
      killed: boolean;
      exitCode: number | null;
      kill(): boolean;
    };
    fakeChild.stdin = new PassThrough();
    fakeChild.stdout = new PassThrough();
    fakeChild.stderr = new PassThrough();
    fakeChild.killed = false;
    fakeChild.exitCode = null;
    fakeChild.kill = () => { fakeChild.killed = true; return true; };
    const writeFrame = (frame: Record<string, unknown>) => {
      fakeChild.stdout.write(`${JSON.stringify({ ...frame, version: 'yawr.stdio/v1' })}\n`);
    };
    let commandBuffer = '';
    fakeChild.stdin.setEncoding('utf8');
    fakeChild.stdin.on('data', (chunk: string) => {
      commandBuffer += chunk;
      let newline = commandBuffer.indexOf('\n');
      while (newline >= 0) {
        const command = JSON.parse(commandBuffer.slice(0, newline)) as Record<string, unknown>;
        commandBuffer = commandBuffer.slice(newline + 1);
        if (command.type === 'interaction.answer') {
          const answer = command.answer as Record<string, unknown>;
          answers.push(answer);
          if (answer.kind === 'host_action') {
            writeFrame({ type: 'interaction.resolved', runID: 'run-xts', turnID: 'turn-xts' });
            writeFrame({
              type: 'interaction.pending', runID: 'run-xts', turnID: 'turn-findings',
              interaction: {
                type: 'pending', runID: 'run-xts', turnID: 'turn-findings', stepID: 'record_findings', kind: 'collector',
                title: 'Record findings', prompt: 'Review XTS, then return here.',
                fields: [{
                  name: 'primary_health', type: 'select', label: 'Primary health', required: true,
                  options: [{ value: 'unavailable', label: 'Unavailable' }, { value: 'healthy', label: 'Healthy' }],
                }],
              },
            });
          } else if (answer.kind === 'collector') {
            writeFrame({ type: 'interaction.resolved', runID: 'run-xts', turnID: 'turn-findings' });
            writeFrame({ type: 'run.finished', runID: 'run-xts', status: 'completed' });
            setImmediate(() => {
              fakeChild.exitCode = 0;
              fakeChild.emit('exit', 0, null);
              fakeChild.emit('close', 0, null);
            });
          }
        }
        newline = commandBuffer.indexOf('\n');
      }
    });

    let xtsDispatches = 0;
    let xtsAcks = 0;
    let xtsReminders = 0;
    const xtsCommand = vscode.commands.registerCommand('xts.openViewWithParameters', () => {
      xtsDispatches += 1;
      return { status: 'opened' };
    });
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => fixture,
        onHostActionAck: () => { xtsAcks += 1; },
        showXtsReminder: () => { xtsReminders += 1; },
        spawnRun: () => {
          setImmediate(() => {
            writeFrame({ type: 'run.started', runID: 'run-xts', status: 'running' });
            writeFrame({
              type: 'interaction.pending', runID: 'run-xts', turnID: 'turn-xts',
              interaction: {
                type: 'pending', runID: 'run-xts', turnID: 'turn-xts', stepID: 'open_xts_view', kind: 'host_action',
                correlationID: 'corr-xts', title: 'Open replication view',
                host_action: {
                  capability: 'xts.open-view',
                  request: { view_path: 'replicas.xts', environment: 'Production', parameters: { server: 'db01' }, focus: true },
                },
              },
            });
          });
          return fakeChild;
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = { runStatus: string; pendingKind?: string; runButtonCount: number; visibleButtons?: string[] };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });
    const waitForDOM = (predicate: (state: Record<string, unknown>) => boolean, failure: string) =>
      new Promise<Record<string, unknown>>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as Record<string, unknown>;
          if (state?.type !== 'test.dom.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });

    try {
      await waitForUI((state) => state.runButtonCount === 1, 'direct XTS graph did not render');
      const hostPending = waitForUI(
        (state) => state.pendingKind === 'host_action' && state.visibleButtons?.includes('Open XTS') === true,
        'XTS interaction did not wait behind Open XTS',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      await hostPending;
      assert.strictEqual(xtsDispatches, 0, 'XTS dispatched before explicit panel confirmation');

      const collectorPending = waitForUI((state) => state.pendingKind === 'collector', 'collector did not follow the opened XTS result');
      const openClicked = waitForDOM((state) => state.clicked === 'Open XTS', 'Open XTS button was not clicked');
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Open XTS' });
      assert.strictEqual((await openClicked).found, true);
      await collectorPending;
      assert.strictEqual(xtsDispatches, 1);
      assert.strictEqual(xtsAcks, 1, 'successful XTS dispatch must traverse the production host-action transport');
      assert.strictEqual(xtsReminders, 1, 'successful focused XTS dispatch must traverse the production reminder path');

      const fieldSet = waitForDOM((state) => state.field === 'primary_health', 'collector field was not edited');
      await panel.webview.postMessage({ type: 'test.action', action: 'set-collector-field', name: 'primary_health', value: 'unavailable' });
      assert.strictEqual((await fieldSet).value, 'unavailable');
      const reviewed = waitForDOM((state) => state.clicked === 'Review answers', 'collector review screen did not open');
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Review answers' });
      assert.strictEqual((await reviewed).collectorReviewVisible, true);
      assert.strictEqual(answers.filter((answer) => answer.kind === 'collector').length, 0, 'review resumed Yawr before final submission');

      const completed = waitForUI((state) => state.runStatus === 'completed', 'run did not complete after saving answers');
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Save answers and continue' });
      await completed;
      assert.strictEqual(answers.filter((answer) => answer.kind === 'host_action').length, 1);
      const collectorAnswers = answers.filter((answer) => answer.kind === 'collector');
      assert.strictEqual(collectorAnswers.length, 1);
      assert.deepStrictEqual(collectorAnswers[0].values, { primary_health: 'unavailable' });
    } finally {
      panel.dispose();
      xtsCommand.dispose();
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('unexpected run exit invalidates a stale Open XTS action', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-xts-unexpected-exit.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    fixture.inputs = [];
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-xts-unexpected-exit\nsteps: []\n'));

    const answers: Array<Record<string, unknown>> = [];
    const fakeChild = new EventEmitter() as EventEmitter & {
      stdin: PassThrough;
      stdout: PassThrough;
      stderr: PassThrough;
      killed: boolean;
      exitCode: number | null;
      kill(): boolean;
    };
    fakeChild.stdin = new PassThrough();
    fakeChild.stdout = new PassThrough();
    fakeChild.stderr = new PassThrough();
    fakeChild.killed = false;
    fakeChild.exitCode = null;
    fakeChild.kill = () => { fakeChild.killed = true; return true; };
    const writeFrame = (frame: Record<string, unknown>) => {
      fakeChild.stdout.write(`${JSON.stringify({ ...frame, version: 'yawr.stdio/v1' })}\n`);
    };
    let commandBuffer = '';
    fakeChild.stdin.setEncoding('utf8');
    fakeChild.stdin.on('data', (chunk: string) => {
      commandBuffer += chunk;
      let newline = commandBuffer.indexOf('\n');
      while (newline >= 0) {
        const command = JSON.parse(commandBuffer.slice(0, newline)) as Record<string, unknown>;
        commandBuffer = commandBuffer.slice(newline + 1);
        if (command.type === 'interaction.answer') answers.push(command.answer as Record<string, unknown>);
        newline = commandBuffer.indexOf('\n');
      }
    });

    let xtsDispatches = 0;
    let xtsAcks = 0;
    let xtsReminders = 0;
    const xtsCommand = vscode.commands.registerCommand('xts.openViewWithParameters', () => {
      xtsDispatches += 1;
      return { status: 'opened' };
    });
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => fixture,
        onHostActionAck: () => { xtsAcks += 1; },
        showXtsReminder: () => { xtsReminders += 1; },
        spawnRun: () => {
          setImmediate(() => {
            writeFrame({ type: 'run.started', runID: 'run-unexpected-exit', status: 'running' });
            writeFrame({
              type: 'interaction.pending', runID: 'run-unexpected-exit', turnID: 'turn-unexpected-exit',
              interaction: {
                type: 'pending', runID: 'run-unexpected-exit', turnID: 'turn-unexpected-exit',
                stepID: 'open_xts_view', kind: 'host_action', correlationID: 'corr-unexpected-exit',
                title: 'Open generic XTS view',
                host_action: {
                  capability: 'xts.open-view',
                  request: { view_path: 'custom/path.xts', environment: 'Canary', parameters: {}, focus: true },
                },
              },
            });
          });
          return fakeChild;
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = { runID?: string; runStatus: string; pendingKind?: string; runButtonCount: number; visibleButtons?: string[] };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });
    const waitForDOM = (predicate: (state: Record<string, unknown>) => boolean, failure: string) =>
      new Promise<Record<string, unknown>>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as Record<string, unknown>;
          if (state?.type !== 'test.dom.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });

    try {
      await waitForUI((state) => state.runButtonCount === 1, 'direct XTS graph did not render');
      const hostPending = waitForUI(
        (state) => state.pendingKind === 'host_action' && state.visibleButtons?.includes('Open XTS') === true,
        'XTS interaction did not become pending',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      await hostPending;

      const failed = waitForUI((state) => state.runStatus === 'failed', 'unexpected process exit did not fail the run');
      fakeChild.exitCode = 17;
      fakeChild.stdout.end();
      fakeChild.stderr.end();
      fakeChild.emit('close', 17, null);
      const failedState = await failed;

      const staleClick = waitForDOM((state) => state.clicked === 'Open XTS', 'stale XTS click was not observed');
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Open XTS' });
      const staleClickState = await staleClick;
      await new Promise<void>((resolve) => setTimeout(resolve, 50));

      assert.deepStrictEqual({
        runID: failedState.runID,
        pendingKind: failedState.pendingKind,
        openXtsVisible: failedState.visibleButtons?.includes('Open XTS') === true,
        staleButtonFound: staleClickState.found,
        xtsDispatches,
        xtsAcks,
        answers: answers.length,
        xtsReminders,
      }, {
        runID: undefined,
        pendingKind: undefined,
        openXtsVisible: false,
        staleButtonFound: false,
        xtsDispatches: 0,
        xtsAcks: 0,
        answers: 0,
        xtsReminders: 0,
      });
    } finally {
      panel.dispose();
      xtsCommand.dispose();
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('Reset cancels an in-flight XTS action and rejects a stale Open XTS click', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-xts-reset.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    fixture.inputs = [];
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-xts-reset\nsteps: []\n'));

    const answers: Array<Record<string, unknown>> = [];
    const fakeChild = new EventEmitter() as EventEmitter & {
      stdin: PassThrough;
      stdout: PassThrough;
      stderr: PassThrough;
      killed: boolean;
      exitCode: number | null;
      kill(): boolean;
    };
    fakeChild.stdin = new PassThrough();
    fakeChild.stdout = new PassThrough();
    fakeChild.stderr = new PassThrough();
    fakeChild.killed = false;
    fakeChild.exitCode = null;
    fakeChild.kill = () => { fakeChild.killed = true; return true; };
    const writeFrame = (frame: Record<string, unknown>) => {
      fakeChild.stdout.write(`${JSON.stringify({ ...frame, version: 'yawr.stdio/v1' })}\n`);
    };
    let commandBuffer = '';
    fakeChild.stdin.setEncoding('utf8');
    fakeChild.stdin.on('data', (chunk: string) => {
      commandBuffer += chunk;
      let newline = commandBuffer.indexOf('\n');
      while (newline >= 0) {
        const command = JSON.parse(commandBuffer.slice(0, newline)) as Record<string, unknown>;
        commandBuffer = commandBuffer.slice(newline + 1);
        if (command.type === 'interaction.answer') answers.push(command.answer as Record<string, unknown>);
        newline = commandBuffer.indexOf('\n');
      }
    });

    let dispatchStarted!: () => void;
    const dispatching = new Promise<void>((resolve) => { dispatchStarted = resolve; });
    let resolveDispatch!: (value: unknown) => void;
    const delayedDispatch = new Promise<unknown>((resolve) => { resolveDispatch = resolve; });
    let xtsDispatches = 0;
    let xtsAcks = 0;
    let xtsReminders = 0;
    const xtsCommand = vscode.commands.registerCommand('xts.openViewWithParameters', () => {
      xtsDispatches += 1;
      dispatchStarted();
      return delayedDispatch;
    });
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => fixture,
        onHostActionAck: () => { xtsAcks += 1; },
        showXtsReminder: () => { xtsReminders += 1; },
        spawnRun: () => {
          setImmediate(() => {
            writeFrame({ type: 'run.started', runID: 'run-reset', status: 'running' });
            writeFrame({
              type: 'interaction.pending', runID: 'run-reset', turnID: 'turn-reset',
              interaction: {
                type: 'pending', runID: 'run-reset', turnID: 'turn-reset', stepID: 'open_xts_view',
                kind: 'host_action', correlationID: 'corr-reset', title: 'Open generic XTS view',
                host_action: {
                  capability: 'xts.open-view',
                  request: { view_path: 'custom/path.xts', environment: 'Canary', parameters: {}, focus: true },
                },
              },
            });
          });
          return fakeChild;
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = { runStatus: string; pendingKind?: string; runButtonCount: number; resetButtonCount: number; visibleButtons?: string[] };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });
    const waitForDOM = (predicate: (state: Record<string, unknown>) => boolean, failure: string) =>
      new Promise<Record<string, unknown>>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as Record<string, unknown>;
          if (state?.type !== 'test.dom.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });

    try {
      await waitForUI((state) => state.runButtonCount === 1, 'direct XTS graph did not render');
      const hostPending = waitForUI(
        (state) => state.pendingKind === 'host_action' && state.visibleButtons?.includes('Open XTS') === true,
        'XTS interaction did not become pending',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      await hostPending;

      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Open XTS' });
      await dispatching;
      assert.strictEqual(xtsDispatches, 1, 'the regression requires an in-flight XTS dispatch');

      const failed = waitForUI(
        (state) => state.runStatus === 'failed' && state.resetButtonCount === 1,
        'synthetic terminal error did not expose Reset',
      );
      await panel.webview.postMessage({ type: 'run.error', message: 'synthetic terminal error' });
      await failed;

      const reset = waitForUI((state) => state.runStatus === 'idle', 'Reset did not clear terminal execution state');
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Reset' });
      await reset;

      const staleClick = waitForDOM((state) => state.clicked === 'Open XTS', 'post-Reset stale click was not observed');
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Open XTS' });
      const staleClickState = await staleClick;
      resolveDispatch({ status: 'opened' });
      await delayedDispatch;
      await new Promise<void>((resolve) => setTimeout(resolve, 50));

      assert.deepStrictEqual({
        staleButtonFound: staleClickState.found,
        dispatchesAfterStaleClick: xtsDispatches,
        xtsAcks,
        answers: answers.length,
        xtsReminders,
      }, {
        staleButtonFound: false,
        dispatchesAfterStaleClick: 1,
        xtsAcks: 0,
        answers: 0,
        xtsReminders: 0,
      });
    } finally {
      resolveDispatch({ status: 'opened' });
      panel.dispose();
      xtsCommand.dispose();
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('a superseded child close cannot mutate the replacement run', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-stale-close.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    fixture.inputs = [];
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-stale-close\nsteps: []\n'));

    type FakeChild = EventEmitter & {
      stdin: PassThrough;
      stdout: PassThrough;
      stderr: PassThrough;
      killed: boolean;
      exitCode: number | null;
      kill(): boolean;
    };
    const createFakeChild = (): FakeChild => {
      const child = new EventEmitter() as FakeChild;
      child.stdin = new PassThrough();
      child.stdout = new PassThrough();
      child.stderr = new PassThrough();
      child.killed = false;
      child.exitCode = null;
      child.kill = () => { child.killed = true; return true; };
      return child;
    };
    const oldChild = createFakeChild();
    const newChild = createFakeChild();
    const writeFrame = (child: FakeChild, frame: Record<string, unknown>) => {
      child.stdout.write(`${JSON.stringify({ ...frame, version: 'yawr.stdio/v1' })}\n`);
    };
    let resolveNewAnswer!: (answer: Record<string, unknown>) => void;
    const newAnswer = new Promise<Record<string, unknown>>((resolve) => { resolveNewAnswer = resolve; });
    let newCommandBuffer = '';
    newChild.stdin.setEncoding('utf8');
    newChild.stdin.on('data', (chunk: string) => {
      newCommandBuffer += chunk;
      let newline = newCommandBuffer.indexOf('\n');
      while (newline >= 0) {
        const command = JSON.parse(newCommandBuffer.slice(0, newline)) as Record<string, unknown>;
        newCommandBuffer = newCommandBuffer.slice(newline + 1);
        if (command.type === 'interaction.answer') {
          const answer = command.answer as Record<string, unknown>;
          resolveNewAnswer(answer);
          if (answer.status === 'completed') {
            writeFrame(newChild, { type: 'interaction.resolved', runID: 'run-new', turnID: 'turn-new' });
          }
        }
        newline = newCommandBuffer.indexOf('\n');
      }
    });

    let dispatchStarted!: () => void;
    const dispatching = new Promise<void>((resolve) => { dispatchStarted = resolve; });
    let resolveDispatch!: (value: unknown) => void;
    const delayedDispatch = new Promise<unknown>((resolve) => { resolveDispatch = resolve; });
    const xtsCommand = vscode.commands.registerCommand('xts.openViewWithParameters', () => {
      dispatchStarted();
      return delayedDispatch;
    });
    let loadCount = 0;
    let resolveDeferredReload!: () => void;
    const deferredReload = new Promise<void>((resolve) => { resolveDeferredReload = resolve; });
    let spawnCount = 0;
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => {
          loadCount += 1;
          if (loadCount === 2) resolveDeferredReload();
          return fixture;
        },
        spawnRun: () => {
          spawnCount += 1;
          if (spawnCount === 1) {
            setImmediate(() => {
              writeFrame(oldChild, { type: 'run.started', runID: 'run-old', status: 'running' });
              writeFrame(oldChild, { type: 'run.finished', runID: 'run-old', status: 'completed' });
            });
            return oldChild;
          }
          setImmediate(() => {
            writeFrame(newChild, { type: 'run.started', runID: 'run-new', status: 'running' });
            writeFrame(newChild, {
              type: 'interaction.pending', runID: 'run-new', turnID: 'turn-new',
              interaction: {
                type: 'pending', runID: 'run-new', turnID: 'turn-new', stepID: 'open_xts_view',
                kind: 'host_action', correlationID: 'corr-new', title: 'Open generic XTS view',
                host_action: {
                  capability: 'xts.open-view',
                  request: { view_path: 'custom/path.xts', environment: 'Canary', parameters: {}, focus: true },
                },
              },
            });
          });
          return newChild;
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = { runID?: string; runStatus: string; pendingKind?: string; runButtonCount: number; visibleButtons?: string[] };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        let lastState: UIState | undefined;
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`${failure}; last state: ${JSON.stringify(lastState)}`));
        }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type === 'ui.state') lastState = state;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve(state);
        });
      });

    try {
      await waitForUI((state) => state.runButtonCount === 1, 'stale-close graph did not render');
      const oldCompleted = waitForUI((state) => state.runStatus === 'completed', 'old run did not reach Reset');
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      await oldCompleted;

      const reset = waitForUI((state) => state.runID === undefined && state.runStatus === 'idle', 'Reset did not clear the old run');
      await panel.webview.postMessage({ type: 'test.action', action: 'reset' });
      await reset;

      const newWaiting = waitForUI(
        (state) => state.runID === 'run-new' && state.runStatus === 'waiting' && state.pendingKind === 'host_action',
        'replacement run did not reach its host action',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      await newWaiting;
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Open XTS' });
      await dispatching;

      const liveDocument = await vscode.workspace.openTextDocument(runbookUri);
      const edit = new vscode.WorkspaceEdit();
      edit.insert(runbookUri, liveDocument.lineAt(liveDocument.lineCount - 1).range.end, '\n# defer reload for replacement run\n');
      assert.strictEqual(await vscode.workspace.applyEdit(edit), true);
      assert.strictEqual(await liveDocument.save(), true);
      assert.strictEqual(loadCount, 1, 'saving during the replacement run must defer reload');

      oldChild.exitCode = 0;
      oldChild.emit('close', 0, null);
      assert.strictEqual(loadCount, 1, 'the superseded child close must not apply the replacement run\'s deferred reload');

      const runningAfterAnswer = waitForUI(
        (state) => state.runID === 'run-new' && state.runStatus === 'running' && state.pendingKind === undefined,
        'replacement run was failed or reset by the superseded child close',
      );
      resolveDispatch({ status: 'opened' });
      const answer = await Promise.race([
        newAnswer,
        new Promise<never>((_, reject) => setTimeout(() => reject(new Error('replacement host action did not complete')), 10_000)),
      ]);
      assert.strictEqual(answer.status, 'completed', 'the superseded child close must not cancel the replacement host action');
      await runningAfterAnswer;
      assert.strictEqual(loadCount, 1, 'deferred reload must remain pending until the replacement run finishes');

      writeFrame(newChild, { type: 'run.finished', runID: 'run-new', status: 'completed' });
      await Promise.race([
        deferredReload,
        new Promise<never>((_, reject) => setTimeout(() => reject(new Error('replacement run did not apply its deferred reload')), 10_000)),
      ]);
      assert.strictEqual(loadCount, 2);
      newChild.exitCode = 0;
      newChild.emit('close', 0, null);
    } finally {
      resolveDispatch({ status: 'opened' });
      panel.dispose();
      xtsCommand.dispose();
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('cancelling a run cancels a delayed XTS handoff before it can ack or show a reminder', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-xts-cancel.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    fixture.inputs = [];
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-xts-cancel\nsteps: []\n'));

    const answers: Array<Record<string, unknown>> = [];
    const fakeChild = new EventEmitter() as EventEmitter & {
      stdin: PassThrough;
      stdout: PassThrough;
      stderr: PassThrough;
      killed: boolean;
      exitCode: number | null;
      kill(): boolean;
    };
    fakeChild.stdin = new PassThrough();
    fakeChild.stdout = new PassThrough();
    fakeChild.stderr = new PassThrough();
    fakeChild.killed = false;
    fakeChild.exitCode = null;
    fakeChild.kill = () => { fakeChild.killed = true; return true; };
    const writeFrame = (frame: Record<string, unknown>) => {
      fakeChild.stdout.write(`${JSON.stringify({ ...frame, version: 'yawr.stdio/v1' })}\n`);
    };
    let runCancelReceived!: () => void;
    const runCancelled = new Promise<void>((resolve) => { runCancelReceived = resolve; });
    let commandBuffer = '';
    fakeChild.stdin.setEncoding('utf8');
    fakeChild.stdin.on('data', (chunk: string) => {
      commandBuffer += chunk;
      let newline = commandBuffer.indexOf('\n');
      while (newline >= 0) {
        const command = JSON.parse(commandBuffer.slice(0, newline)) as Record<string, unknown>;
        commandBuffer = commandBuffer.slice(newline + 1);
        if (command.type === 'interaction.answer') answers.push(command.answer as Record<string, unknown>);
        if (command.type === 'run.cancel') {
          runCancelReceived();
          writeFrame({ type: 'run.finished', runID: 'run-cancel-xts', status: 'cancelled' });
        }
        newline = commandBuffer.indexOf('\n');
      }
    });

    let dispatchStarted!: () => void;
    const dispatching = new Promise<void>((resolve) => { dispatchStarted = resolve; });
    let resolveDispatch!: (value: unknown) => void;
    const delayedDispatch = new Promise<unknown>((resolve) => { resolveDispatch = resolve; });
    let xtsDispatches = 0;
    let xtsAcks = 0;
    let xtsReminders = 0;
    const xtsCommand = vscode.commands.registerCommand('xts.openViewWithParameters', () => {
      xtsDispatches += 1;
      dispatchStarted();
      return delayedDispatch;
    });
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => fixture,
        onHostActionAck: () => { xtsAcks += 1; },
        showXtsReminder: () => { xtsReminders += 1; },
        spawnRun: () => {
          setImmediate(() => {
            writeFrame({ type: 'run.started', runID: 'run-cancel-xts', status: 'running' });
            writeFrame({
              type: 'interaction.pending', runID: 'run-cancel-xts', turnID: 'turn-cancel-xts',
              interaction: {
                type: 'pending', runID: 'run-cancel-xts', turnID: 'turn-cancel-xts', stepID: 'open_xts_view', kind: 'host_action',
                correlationID: 'corr-cancel-xts', title: 'Open generic XTS view',
                host_action: {
                  capability: 'xts.open-view',
                  request: { view_path: 'custom/path.xts', environment: 'Canary', parameters: { arbitrary_name: 'arbitrary-value' }, focus: true },
                },
              },
            });
          });
          return fakeChild;
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = { runStatus: string; pendingKind?: string; runButtonCount: number; visibleButtons?: string[] };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(failure)); }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout); subscription.dispose(); resolve(state);
        });
      });

    try {
      await waitForUI((state) => state.runButtonCount === 1, 'direct XTS cancellation graph did not render');
      const hostPending = waitForUI(
        (state) => state.pendingKind === 'host_action' && state.visibleButtons?.includes('Open XTS') === true,
        'XTS cancellation interaction did not wait behind Open XTS',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      await hostPending;

      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Open XTS' });
      await dispatching;
      assert.strictEqual(xtsDispatches, 1, 'the regression requires an in-flight XTS dispatch');

      const cancelledUI = waitForUI((state) => state.runStatus === 'cancelled', 'run did not settle as cancelled');
      await panel.webview.postMessage({ type: 'test.action', action: 'cancel' });
      await runCancelled;
      await cancelledUI;

      resolveDispatch({ status: 'opened' });
      await delayedDispatch;
      await new Promise<void>((resolve) => setImmediate(resolve));

      assert.strictEqual(answers.length, 0, 'cancelled XTS dispatch must not answer the run interaction');
      assert.strictEqual(xtsAcks, 0, 'cancelled XTS dispatch must not emit a host-action acknowledgment');
      assert.strictEqual(xtsReminders, 0, 'cancelled XTS dispatch must not show the return-to-Yawr reminder');
    } finally {
      resolveDispatch({ status: 'opened' });
      panel.dispose();
      xtsCommand.dispose();
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('disposing the direct panel during startup prevents child spawn', async () => {
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'direct-start-dispose.runbook.yaml');
    const fixtureUri = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum-preview-graphjson.json');
    const fixture = JSON.parse(Buffer.from(await vscode.workspace.fs.readFile(fixtureUri)).toString('utf8'));
    fixture.inputs = [];
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: direct-start-dispose\nsteps: []\n'));

    let releaseStartup!: () => void;
    const startupReleased = new Promise<void>((resolve) => { releaseStartup = resolve; });
    let reachedPreSpawn!: () => void;
    const preSpawnReached = new Promise<void>((resolve) => { reachedPreSpawn = resolve; });
    let settleStartup!: () => void;
    const startupSettled = new Promise<void>((resolve) => { settleStartup = resolve; });
    let spawnCount = 0;
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      {
        documentLoader: async () => fixture,
        beforeSpawn: async () => {
          reachedPreSpawn();
          await startupReleased;
        },
        onStartSettled: settleStartup,
        spawnRun: () => {
          spawnCount += 1;
          throw new Error('spawnRun must not be called after panel disposal');
        },
      },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    try {
      const ready = new Promise<void>((resolve, reject) => {
        const timeout = setTimeout(() => reject(new Error('direct panel did not become ready')), 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown; runButtonCount?: unknown };
          if (state?.type !== 'ui.state' || state.runButtonCount !== 1) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve();
        });
      });
      await ready;
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      await Promise.race([
        preSpawnReached,
        new Promise<never>((_, reject) => setTimeout(() => reject(new Error('startup never reached the pre-spawn guard')), 2_000)),
      ]);
      panel.dispose();
      releaseStartup();
      await startupSettled;
      assert.strictEqual(spawnCount, 0);
    } finally {
      panel.dispose();
      releaseStartup();
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('direct webview completes a real yawr stdio choice run', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'real-stdio-choice.runbook.yaml');
    const runbook = `apiVersion: yawr.runbook/v1
id: real-stdio-choice
name: real-stdio-choice
kind: reference
governance:
  require_approval: true
inputs:
  environment:
    type: string
    required: true
    enum: [prod, staging]
flow:
  - step:
      id: choose-route
      type: choice
      prompt: Choose a route for \${environment}
      variable: selected_route
      options:
        - value: primary
          label: Primary
        - value: fallback
          label: Fallback
`;
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from(runbook));
    const config = vscode.workspace.getConfiguration('yawr', runbookUri);
    const previousBinary = config.inspect<string>('binaryPath')?.workspaceValue;
    await config.update('binaryPath', sourceYawrBinaryPath, vscode.ConfigurationTarget.Workspace);

    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = {
      runStatus: string;
      pendingKind?: string;
      inputCount: number;
      inputValues: Record<string, string>;
      runButtonCount: number;
      cancelButtonCount: number;
      nodeStatuses: Record<string, string>;
    };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        let lastState: UIState | undefined;
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`${failure}; last state: ${JSON.stringify(lastState)}`));
        }, 15_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          if (typeof message !== 'object' || message === null) return;
          const state = message as { type?: unknown } & UIState;
          if (state.type === 'ui.state') lastState = state;
          if (state.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve(state);
        });
      });

    try {
      await waitForUI(
        (state) => state.inputCount === 1 && state.runButtonCount === 1,
        'real direct view did not render Run and the declared input',
      );
      const inputReady = waitForUI(
        (state) => state.inputValues.environment === 'prod',
        'real direct view did not accept the declared input',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'set-input', name: 'environment', value: 'prod' });
      await inputReady;

      const approval = waitForUI(
        (state) => state.runStatus === 'waiting' && state.pendingKind === 'approval',
        'real yawr process did not publish its governance approval',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'run' });
      const approvalState = await approval;
      assert.strictEqual(approvalState.cancelButtonCount, 1);

      const waiting = waitForUI(
        (state) => state.runStatus === 'waiting' && state.pendingKind === 'choice',
        'real yawr process did not continue to its choice prompt after approval',
      );
      await panel.webview.postMessage({
        type: 'test.action',
        action: 'answer',
        answer: { kind: 'approval', approved: true, approver: 'operator-id' },
      });
      const waitingState = await waiting;
      assert.strictEqual(waitingState.cancelButtonCount, 1);
      assert.strictEqual(waitingState.nodeStatuses['choose-route'], 'running');

      const completed = waitForUI(
        (state) => state.runStatus === 'completed' && state.nodeStatuses['choose-route'] === 'completed',
        'real yawr process did not complete after the choice answer',
      );
      await panel.webview.postMessage({
        type: 'test.action',
        action: 'answer',
        answer: { kind: 'choice', selected: ['o:1'] },
      });
      const completedState = await completed;
      assert.strictEqual(completedState.runButtonCount, 1);
      assert.strictEqual(completedState.cancelButtonCount, 0);
      const runtimeState = await vscode.commands.executeCommand<{ mcpBridgeStarted: boolean }>(
        'yawr.test.getRuntimeState',
      );
      assert.deepStrictEqual(runtimeState, { mcpBridgeStarted: false });
    } finally {
      panel.dispose();
      await config.update('binaryPath', previousBinary, vscode.ConfigurationTarget.Workspace);
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
      const traces = await vscode.workspace.findFiles(
        new vscode.RelativePattern(vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath), 'real-stdio-choice.runbook-*.jsonl'),
      );
      await Promise.all(traces.map((uri) => vscode.workspace.fs.delete(uri, { useTrash: false })));
    }
  });

  test('route-test pane persists an untouched required boolean as false', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'route-test-required-boolean.runbook.yaml');
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: route-test-required-boolean\nsteps: []\n'));
    const fixture = {
      schema_version: '1' as const,
      hash: 'sha256:route-test-required-boolean',
      runbook: { id: 'route-test-required-boolean', name: 'route-test-required-boolean', path: runbookUri.fsPath },
      inputs: [],
      frames: [{ id: 'frame:root', runbook_id: 'route-test-required-boolean', runbook_path: runbookUri.fsPath, depth: 0 }],
      groups: [],
      nodes: [{
        id: 'record_findings',
        type: 'collector',
        position: { x: 0, y: 0 },
        data: {
          id: 'record_findings', step_id: 'record_findings', kind: 'collector', title: 'Record findings', frame_id: 'frame:root',
          details: {
            kind: 'collector',
            fields: [
              { name: 'confirmed', label: 'Confirmed', type: 'boolean', required: true },
              { name: 'use_cache', label: 'Use cache', type: 'boolean', default: true },
            ],
          },
        },
      }, {
        id: 'target',
        type: 'cli',
        position: { x: 240, y: 0 },
        data: { id: 'target', step_id: 'target', kind: 'cli', title: 'Target command', frame_id: 'frame:root' },
      }],
      edges: [{ id: 'findings-target', source: 'record_findings', target: 'target' }],
    };
    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
      { documentLoader: async () => fixture },
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = {
      graphNodeIDs: string[];
      runButtonCount: number;
      routeTestPlanHash?: string;
      routeTestError?: string;
      savedRouteTestIDs?: string[];
    };
    type DOMState = { clicked?: string; routeTestCheckbox?: string; found?: boolean; checked?: boolean };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        let lastState: UIState | undefined;
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`${failure}; last state: ${JSON.stringify(lastState)}`));
        }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type === 'ui.state') lastState = state;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve(state);
        });
      });
    const waitForInspector = (nodeID: string) => new Promise<void>((resolve, reject) => {
      const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(`inspector did not select ${nodeID}`)); }, 10_000);
      const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
        const state = message as { type?: unknown; nodeID?: unknown };
        if (state?.type !== 'inspector.state' || state.nodeID !== nodeID) return;
        clearTimeout(timeout);
        subscription.dispose();
        resolve();
      });
    });
    const invokeDOMAction = (action: string, name: string, key: 'clicked' | 'routeTestCheckbox') => new Promise<DOMState>((resolve, reject) => {
      const timeout = setTimeout(() => { subscription.dispose(); reject(new Error(`${action} ${name} did not complete`)); }, 10_000);
      const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
        const state = message as { type?: unknown } & DOMState;
        if (state?.type !== 'test.dom.state' || state[key] !== name) return;
        clearTimeout(timeout);
        subscription.dispose();
        resolve(state);
      });
      void panel.webview.postMessage({ type: 'test.action', action, name });
    });
    let savedArtifactUri: vscode.Uri | undefined;

    try {
      const initial = await waitForUI(
        (state) => state.runButtonCount === 1 && state.routeTestPlanHash !== undefined,
        'required-boolean route-test graph did not render',
      );
      assert.deepStrictEqual(initial.graphNodeIDs.sort(), ['record_findings', 'target']);
      const selected = waitForInspector('target');
      await panel.webview.postMessage({ type: 'test.action', action: 'select-node', name: 'target' });
      await selected;
      assert.strictEqual((await invokeDOMAction('click-button', 'Show routes through this step', 'clicked')).found, true);
      assert.strictEqual((await invokeDOMAction('click-button', 'Test reaching this step', 'clicked')).found, true);
      const condition = await invokeDOMAction('click-route-test-checkbox', 'Record findings', 'routeTestCheckbox');
      assert.deepStrictEqual({ found: condition.found, checked: condition.checked }, { found: true, checked: true });
      const sensitivity = await invokeDOMAction('click-route-test-checkbox', 'I reviewed the saved values', 'routeTestCheckbox');
      assert.deepStrictEqual({ found: sensitivity.found, checked: sensitivity.checked }, { found: true, checked: true });

      const saved = waitForUI(
        (state) => state.savedRouteTestIDs?.length === 1 || state.routeTestError !== undefined,
        'route-test pane did not save the untouched required boolean',
      );
      assert.strictEqual((await invokeDOMAction('click-button', 'Save draft', 'clicked')).found, true);
      const savedState = await saved;
      assert.strictEqual(savedState.routeTestError, undefined);
      assert.strictEqual(savedState.savedRouteTestIDs?.length, 1);
      savedArtifactUri = vscode.Uri.joinPath(
        workspaceFolder.uri,
        ...testStatePath,
        '.yawr',
        'route-tests',
        `${savedState.savedRouteTestIDs![0]}.route-test.yaml`,
      );
      const persisted = Buffer.from(await vscode.workspace.fs.readFile(savedArtifactUri)).toString('utf8');
      assert.match(persisted, /confirmed: false/);
      assert.match(persisted, /use_cache: true/);
    } finally {
      panel.dispose();
      if (savedArtifactUri) await vscode.workspace.fs.delete(savedArtifactUri, { useTrash: false });
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
    }
  });

  test('direct webview runs a saved route test and stops before the target', async function () {
    this.timeout(45_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'real-route-test.runbook.yaml');
    const routeTestUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, '.yawr', 'route-tests', 'route-test-e2e.route-test.yaml');
    const missingHashUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, '.yawr', 'route-tests', 'route-test-missing-hash.route-test.yaml');
    const nonStringHashUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, '.yawr', 'route-tests', 'route-test-non-string-hash.route-test.yaml');
    const runbook = `apiVersion: yawr.runbook/v1
id: real-route-test
name: real-route-test
kind: mitigation
flow:
  - step:
      id: load_context
      type: host_action
      host_action:
        capability: test.saved-context
        request: { key: route }
      capture: { route_value: outputs.result.value }
  - step:
      id: choose_route
      type: branch
      branches:
        - condition: route_value == "go"
          steps:
            - step:
                id: dangerous_command
                type: cli
                run: must-not-run
        - else: true
          steps:
            - step:
                id: wrong_route
                type: end
                outcome: { category: blocked, code: wrong-route }
`;
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from(runbook));
    const config = vscode.workspace.getConfiguration('yawr', runbookUri);
    const previousBinary = config.inspect<string>('binaryPath')?.workspaceValue;
    await config.update('binaryPath', sourceYawrBinaryPath, vscode.ConfigurationTarget.Workspace);

    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = {
      inputCount: number;
      runButtonCount: number;
      debugRunButtonCount: number;
      runStatus: string;
      graphNodeIDs: string[];
      visibleButtons?: string[];
      routeTestPlanHash?: string;
      routeTestPassed?: boolean;
      routeTestTargetReached?: boolean;
      routeTestExternalDispatches?: number;
      routeTestError?: string;
      savedRouteTestIDs?: string[];
      savedRouteTestResults?: Record<string, string | undefined>;
    };
    type DOMState = {
      clicked?: string;
      found?: boolean;
      visibleButtons?: string[];
      disabledButtons?: string[];
      routeTestSafetyText?: string;
    };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        let lastState: UIState | undefined;
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`${failure}; last state: ${JSON.stringify(lastState)}`));
        }, 20_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type === 'ui.state') lastState = state;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve(state);
        });
      });
    const clickButton = (name: string) => new Promise<DOMState>((resolve, reject) => {
      const timeout = setTimeout(() => {
        subscription.dispose();
        reject(new Error(`button ${JSON.stringify(name)} did not report its resulting DOM state`));
      }, 10_000);
      const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
        const state = message as { type?: unknown } & DOMState;
        if (state?.type !== 'test.dom.state' || state.clicked !== name) return;
        clearTimeout(timeout);
        subscription.dispose();
        resolve(state);
      });
      void panel.webview.postMessage({ type: 'test.action', action: 'click-button', name });
    });
    const waitForInspector = (nodeID: string) => new Promise<void>((resolve, reject) => {
      const timeout = setTimeout(() => {
        subscription.dispose();
        reject(new Error(`inspector did not select ${JSON.stringify(nodeID)}`));
      }, 10_000);
      const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
        const state = message as { type?: unknown; nodeID?: unknown };
        if (state?.type !== 'inspector.state' || state.nodeID !== nodeID) return;
        clearTimeout(timeout);
        subscription.dispose();
        resolve();
      });
    });

    try {
      const initial = await waitForUI((state) => state.inputCount === 0 && state.runButtonCount === 1, 'route-test graph did not render');
      assert.ok(initial.routeTestPlanHash, 'route-test graph must expose its reviewed plan hash');
      const targetNodeID = initial.graphNodeIDs.find((nodeID) => nodeID.includes('dangerous_command'));
      assert.ok(targetNodeID, `route-test target is missing from graph nodes: ${JSON.stringify(initial.graphNodeIDs)}`);
      const targetSelected = waitForInspector(targetNodeID);
      await panel.webview.postMessage({ type: 'test.action', action: 'select-node', name: targetNodeID });
      await targetSelected;
      const routeView = await clickButton('Show routes through this step');
      assert.ok(routeView.visibleButtons?.includes('Test reaching this step'), 'route-test launcher must be visible for the selected target');
      const routeEditor = await clickButton('Test reaching this step');
      assert.ok(routeEditor.disabledButtons?.includes('Run'), 'normal Run must be disabled while route-test review is open');
      assert.ok(routeEditor.disabledButtons?.includes('Debug Run'), 'normal Debug Run must be disabled while route-test review is open');
      assert.match(routeEditor.routeTestSafetyText ?? '', /protected execution starts only when you run this route test/i);
      const planReloaded = waitForUI(
        (state) => state.routeTestPlanHash !== undefined &&
          state.routeTestPlanHash !== initial.routeTestPlanHash &&
          state.visibleButtons?.includes('Test reaching this step') === true,
        'route-test plan reload did not close the stale editor',
      );
      const liveDocument = await vscode.workspace.openTextDocument(runbookUri);
      const edit = new vscode.WorkspaceEdit();
      edit.replace(
        runbookUri,
        new vscode.Range(liveDocument.positionAt(0), liveDocument.positionAt(liveDocument.getText().length)),
        liveDocument.getText().replace('run: must-not-run', 'run: still-must-not-run'),
      );
      assert.strictEqual(await vscode.workspace.applyEdit(edit), true);
      assert.strictEqual(await liveDocument.save(), true);
      const reloadedState = await planReloaded;
      assert.ok(reloadedState.visibleButtons?.includes('Test reaching this step'), 'plan reload must close the stale route-test editor');
      assert.ok(!reloadedState.visibleButtons?.includes('Check this route'), 'old route-test conditions must not survive a plan reload');
      assert.ok(reloadedState.routeTestPlanHash, 'reloaded route-test graph must expose its current plan hash');
      const artifact = {
        apiVersion: 'yawr.route-test/v1' as const,
        id: 'route-test-e2e',
        name: 'Reach dangerous command',
        runbook: 'real-route-test.runbook.yaml',
        sensitivity_reviewed: true,
        target: { call_path: ['choose_route'], step: 'dangerous_command', phase: 'before' as const, invocation: 1, attempt: 1 },
        host_action_responses: [{
          at: { step: 'load_context', phase: 'execute' as const, invocation: 1, attempt: 1 },
          capability: 'test.saved-context',
          response: { status: 'completed', result: { value: 'go' } },
          source: { kind: 'manual' as const },
          review: { state: 'reviewed' as const, reviewed_by: 'operator', reviewed_at: '2026-08-28T12:05:00Z', sensitivity_reviewed: true },
        }],
      };
      const missingHashHandled = waitForUI(
        (state) => state.routeTestError !== undefined ||
          state.savedRouteTestIDs?.includes('route-test-missing-hash') === true,
        'extension host did not handle the route-test artifact with no plan hash',
      );
      await panel.webview.postMessage({
        type: 'test.action', action: 'save-route-test',
        artifact: { ...artifact, id: 'route-test-missing-hash' },
      });
      const missingHashState = await missingHashHandled;
      assert.match(missingHashState.routeTestError ?? '', /plan[_ ]hash/i);

      const staleHandled = waitForUI(
        (state) => state.routeTestError !== undefined ||
          state.savedRouteTestIDs?.includes(artifact.id) === true,
        'extension host did not handle the stale route-test artifact',
      );
      await panel.webview.postMessage({
        type: 'test.action', action: 'save-route-test',
        artifact: { ...artifact, plan_hash: initial.routeTestPlanHash },
      });
      const staleState = await staleHandled;
      assert.match(staleState.routeTestError ?? '', /runbook changed|plan[_ ]hash/i);

      await clickButton('Test reaching this step');
      const completed = waitForUI(
        (state) => state.runStatus === 'paused_at_boundary' && state.routeTestPassed === true,
        'route test did not report its protected target as reached',
      );
      await panel.webview.postMessage({
        type: 'test.action',
        action: 'run-route-test',
        artifact: {
          ...artifact,
          plan_hash: reloadedState.routeTestPlanHash,
        },
      });
      const state = await completed;
      assert.strictEqual(state.routeTestTargetReached, true);
      assert.strictEqual(state.routeTestExternalDispatches, 0);
      const resultSaved = waitForUI(
        (candidate) => candidate.savedRouteTestResults?.['route-test-e2e'] === 'reached',
        'completed route-test evidence was not saved',
      );
      await panel.webview.postMessage({
        type: 'test.action', action: 'save-route-test-result',
        artifact: { ...artifact, plan_hash: reloadedState.routeTestPlanHash },
      });
      await resultSaved;
      const resetState = await clickButton('Reset');
      assert.ok(resetState.visibleButtons?.includes('Test reaching this step'), 'Reset must close the route-test editor');
      assert.ok(!resetState.visibleButtons?.includes('Check this route'), 'Reset must discard the mounted route-test stage');
      const saved = Buffer.from(await vscode.workspace.fs.readFile(routeTestUri)).toString('utf8');
      assert.match(saved, /plan_hash: sha256:[a-f0-9]{64}/);
      assert.match(saved, /last_result:/);
      assert.match(saved, /conditions_digest: sha256:[a-f0-9]{64}/);
      const nonStringHashHandled = waitForUI(
        (candidate) => candidate.routeTestError !== undefined ||
          candidate.savedRouteTestIDs?.includes('route-test-non-string-hash') === true,
        'extension host did not handle the route-test artifact with a non-string plan hash',
      );
      await panel.webview.postMessage({
        type: 'test.action', action: 'save-route-test',
        artifact: { ...artifact, id: 'route-test-non-string-hash', plan_hash: 42 } as unknown as typeof artifact,
      });
      const nonStringHashState = await nonStringHashHandled;
      assert.match(nonStringHashState.routeTestError ?? '', /plan[_ ]hash/i);
      const runtimeState = await vscode.commands.executeCommand<{ mcpBridgeStarted: boolean }>('yawr.test.getRuntimeState');
      assert.deepStrictEqual(runtimeState, { mcpBridgeStarted: false });
    } finally {
      panel.dispose();
      await config.update('binaryPath', previousBinary, vscode.ConfigurationTarget.Workspace);
      for (const uri of [runbookUri, routeTestUri, missingHashUri, nonStringHashUri]) {
        try { await vscode.workspace.fs.delete(uri, { useTrash: false }); } catch { /* already absent */ }
      }
      const traces = await vscode.workspace.findFiles(
        new vscode.RelativePattern(vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath), 'real-route-test.runbook-*.jsonl'),
      );
      await Promise.all(traces.map((uri) => vscode.workspace.fs.delete(uri, { useTrash: false })));
    }
  });

  test('direct webview debug override changes downstream branch execution', async function () {
    this.timeout(30_000);
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath, 'real-stdio-debug.runbook.yaml');
    const runbook = `apiVersion: yawr.runbook/v1
id: real-stdio-debug
name: real-stdio-debug
kind: reference
flow:
  - step:
      id: get-incident
      type: cli
      command: ${JSON.stringify(sourceYawrBinaryPath)}
      args: [version]
      capture:
        incident_status: stdout
  - step:
      id: route-incident
      type: branch
      branches:
        - condition: 'incident_status == "Active"'
          label: Active
          steps:
            - step:
                id: active-path
                type: noop
        - condition: 'incident_status != "Active"'
          label: Other
          steps:
            - step:
                id: other-path
                type: noop
`;
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from(runbook));
    const config = vscode.workspace.getConfiguration('yawr', runbookUri);
    const previousBinary = config.inspect<string>('binaryPath')?.workspaceValue;
    await config.update('binaryPath', sourceYawrBinaryPath, vscode.ConfigurationTarget.Workspace);

    const panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
      'yawr.test.openDirectGraphPanel',
      runbookUri.fsPath,
    );
    assert.ok(panel, 'test command must return the direct graph WebviewPanel');

    type UIState = {
      runStatus: string;
      runError?: string;
      pendingKind?: string;
      pendingDebugPhase?: string;
      runButtonCount: number;
      debugRunButtonCount: number;
      cancelButtonCount: number;
      breakpointCount: number;
      debugOverrideCount: number;
      nodeStatuses: Record<string, string>;
    };
    const waitForUI = (predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        let lastState: UIState | undefined;
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`${failure}; last state: ${JSON.stringify(lastState)}`));
        }, 15_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          if (typeof message !== 'object' || message === null) return;
          const state = message as { type?: unknown } & UIState;
          if (state.type === 'ui.state') lastState = state;
          if (state.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve(state);
        });
      });

    try {
      await waitForUI(
        (state) => state.runButtonCount === 1 && state.debugRunButtonCount === 1 && state.breakpointCount === 0,
        'direct view did not render normal and debug run controls',
      );
      const breakpointReady = waitForUI(
        (state) => state.breakpointCount === 1,
        'direct view did not retain the after breakpoint',
      );
      await panel.webview.postMessage({
        type: 'test.action',
        action: 'toggle-breakpoint',
        name: 'get-incident',
        value: 'after',
      });
      await breakpointReady;

      const paused = waitForUI(
        (state) => state.runStatus === 'waiting' && state.pendingKind === 'debug_break' && state.pendingDebugPhase === 'after',
        'real yawr process did not publish its after-execution debug pause',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'debug' });
      const pausedState = await paused;
      assert.strictEqual(pausedState.nodeStatuses['get-incident'], 'running');
      assert.strictEqual(pausedState.cancelButtonCount, 1);

      const completed = waitForUI(
        (state) => state.runStatus === 'completed' &&
          state.debugOverrideCount === 1 &&
          state.nodeStatuses['route-incident/active-path'] === 'completed' &&
          state.nodeStatuses['route-incident/other-path'] !== 'completed',
        'debug override did not drive execution through the Active branch',
      );
      await panel.webview.postMessage({
        type: 'test.action',
        action: 'answer',
        answer: {
          kind: 'debug_break',
          action: 'continue',
          set: {
            status: 'completed',
            output_patch: { stdout: 'Active' },
          },
        },
      });
      const completedState = await completed;
      assert.strictEqual(completedState.nodeStatuses['get-incident'], 'completed');
      assert.strictEqual(completedState.runButtonCount, 1);
      assert.strictEqual(completedState.cancelButtonCount, 0);
    } finally {
      panel.dispose();
      await config.update('binaryPath', previousBinary, vscode.ConfigurationTarget.Workspace);
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false });
      const traces = await vscode.workspace.findFiles(
        new vscode.RelativePattern(vscode.Uri.joinPath(workspaceFolder.uri, ...testStatePath), 'real-stdio-debug.runbook-*.jsonl'),
      );
      await Promise.all(traces.map((uri) => vscode.workspace.fs.delete(uri, { useTrash: false })));
    }
  });

  test('preview placement: the default opens the graph in the runbook editor group', async () => {
    const ext = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(ext, `extension ${EXTENSION_ID} must be present`);
    await ext.activate();

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'the Extension Host test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(
      workspaceFolder.uri,
      'test',
      'fixtures',
      'enum.runbook.yaml',
    );
    const config = vscode.workspace.getConfiguration('yawr', runbookUri);
    const previousOpenLocation = config.inspect<string>('preview.openLocation')?.workspaceValue;

    try {
      await config.update('preview.openLocation', undefined, vscode.ConfigurationTarget.Workspace);

      const document = await vscode.workspace.openTextDocument(runbookUri);
      const editor = await vscode.window.showTextDocument(document, {
        viewColumn: vscode.ViewColumn.Two,
        preview: false,
      });
      assert.ok(editor.viewColumn, 'runbook editor must have a view column');

      let tabSubscription: vscode.Disposable;
      const previewTabOpened = new Promise<vscode.Tab>((resolve, reject) => {
        const timeout = setTimeout(() => {
          tabSubscription.dispose();
          const openTabs = vscode.window.tabGroups.all
            .flatMap((group) => group.tabs)
            .map((tab) => `${tab.label} (${tab.input?.constructor.name ?? 'unknown'})`)
            .join(', ');
          reject(new Error(`yawrPreviewGraph tab did not open; current tabs: ${openTabs}`));
        }, 5_000);
        tabSubscription = vscode.window.tabGroups.onDidChangeTabs((event) => {
          const opened = event.opened.find(isGraphPreviewTab);
          if (opened) {
            clearTimeout(timeout);
            tabSubscription.dispose();
            resolve(opened);
          }
        });
      });
      await vscode.commands.executeCommand('yawr.previewGraph');
      const previewTab = await previewTabOpened;
      assert.strictEqual(
        previewTab.group.viewColumn,
        editor.viewColumn,
        'the default placement must put the graph tab in the captured runbook editor group',
      );

      await vscode.window.tabGroups.close(previewTab, true);
      const otherDocument = await vscode.workspace.openTextDocument(
        vscode.Uri.joinPath(workspaceFolder.uri, 'README.md'),
      );
      await vscode.window.showTextDocument(otherDocument, {
        viewColumn: vscode.ViewColumn.One,
        preview: false,
      });

      let reopenedTabSubscription: vscode.Disposable;
      const previewTabReopened = new Promise<vscode.Tab>((resolve, reject) => {
        const timeout = setTimeout(() => {
          reopenedTabSubscription.dispose();
          reject(new Error('yawrPreviewGraph tab did not reopen from durable runbook state'));
        }, 5_000);
        reopenedTabSubscription = vscode.window.tabGroups.onDidChangeTabs((event) => {
          const opened = event.opened.find(isGraphPreviewTab);
          if (opened) {
            clearTimeout(timeout);
            reopenedTabSubscription.dispose();
            resolve(opened);
          }
        });
      });
      await vscode.commands.executeCommand('yawr.previewGraph');
      const reopenedPreviewTab = await previewTabReopened;
      assert.strictEqual(
        reopenedPreviewTab.group.viewColumn,
        editor.viewColumn,
        'an inactive runbook must retain ownership of the default preview group',
      );

      await vscode.window.tabGroups.close(reopenedPreviewTab, true);
      await config.update('preview.openLocation', 'beside', vscode.ConfigurationTarget.Workspace);
      await vscode.window.showTextDocument(document, {
        viewColumn: editor.viewColumn,
        preview: false,
      });

      let besideTabSubscription: vscode.Disposable;
      const besideTabOpened = new Promise<vscode.Tab>((resolve, reject) => {
        const timeout = setTimeout(() => {
          besideTabSubscription.dispose();
          reject(new Error('yawrPreviewGraph tab did not open beside the runbook'));
        }, 5_000);
        besideTabSubscription = vscode.window.tabGroups.onDidChangeTabs((event) => {
          const opened = event.opened.find(isGraphPreviewTab);
          if (opened) {
            clearTimeout(timeout);
            besideTabSubscription.dispose();
            resolve(opened);
          }
        });
      });
      await vscode.commands.executeCommand('yawr.previewGraph');
      const besidePreviewTab = await besideTabOpened;
      assert.notStrictEqual(
        besidePreviewTab.group.viewColumn,
        editor.viewColumn,
        'explicit beside placement must open outside the runbook editor group',
      );
    } finally {
      const openedTabs = vscode.window.tabGroups.all
        .flatMap((group) => group.tabs)
        .filter((tab) => isGraphPreviewTab(tab) || isTextTabFor(tab, runbookUri));
      if (openedTabs.length > 0) {
        await vscode.window.tabGroups.close(openedTabs, true);
      }
      await config.update('preview.openLocation', previousOpenLocation, vscode.ConfigurationTarget.Workspace);
    }
  });
});
