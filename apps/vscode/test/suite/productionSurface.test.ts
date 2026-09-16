import * as assert from 'assert';
import * as vscode from 'vscode';
import { createHash } from 'node:crypto';
import { once } from 'node:events';
import { createServer } from 'node:net';
import { copyFile, cp, mkdir, readFile, readdir, realpath, writeFile } from 'node:fs/promises';
import { isAbsolute, join, relative } from 'node:path';
import { connectGraphObserver } from './graphPlaybackObserver';

const EXTENSION_ID = 'ormasoftchile.yawr-preview';
const canonicalWebviewViewType = (viewType: string) => viewType.replace(/^mainThreadWebview-/, '');
const RENDER_TELEMETRY_SCHEMA = 'yawr.render-telemetry/v1';

interface RenderTelemetry {
  type: 'render.telemetry';
  schema: typeof RENDER_TELEMETRY_SCHEMA;
  reactFlowRootCount: number;
  stepNodeCount: number;
  nodeIDs: string[];
  frameCount: number;
  edgeCount: number;
}

const hashFile = async (path: string) => createHash('sha256').update(await readFile(path)).digest('hex');

function assertInside(root: string, candidate: string, label: string): void {
  const rel = relative(root, candidate);
  assert.ok(rel && !rel.startsWith('..') && !isAbsolute(rel), `${label} must be inside ${root}, got ${candidate}`);
}

async function directoryEntries(path: string): Promise<string[]> {
  try {
    return (await readdir(path)).sort();
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return [];
    throw error;
  }
}

suite('Installed VSIX production surface', () => {
  for (const scenario of [
    { label: 'dynamic included runbooks', directory: 'execution-graph', entry: ['runbooks', 'dynamic-router.runbook.yaml'],
      title: 'Dynamic router - nested calls and returns', steps: 9, nodes: 9, runbooks: 3, evidence: 'included-runbooks-500ms.json' },
    { label: 'lexically scoped included runbooks', directory: 'dependency-scopes', entry: ['dynamic.runbook.yaml'],
      title: 'Dynamically selected children own their package dependencies',
      steps: 15, nodes: 13, runbooks: 7, evidence: 'dependency-scopes-500ms.json' },
  ]) test(`${scenario.label} form one paced CURRENT stream and retain the complete return history`, async function () {
    this.timeout(60_000);
    assert.equal(vscode.version, '1.136.2');
    const extension = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(extension);
    await extension.activate();
    const { directOccurrenceID }: {
      directOccurrenceID(runID: string, nodeID: string, payload: Record<string, unknown>): string;
    } = require(join(extension.extensionPath, 'out', 'executionProgress.js'));
    const root = process.env.YAWR_TEST_STATE_ROOT;
    const core = join(__dirname, '..', '..', '..', '..', '..', 'runtime');
    const port = process.env.YAWR_TEST_CDP_PORT;
    assert.ok(root && core && port, 'the installed harness must supply its isolated environment');
    const destination = join(root, 'workspace', scenario.directory);
    await cp(join(core, 'examples', scenario.directory), destination, {
      recursive: true, filter: source => !source.includes(`${require('node:path').sep}.runbook`),
    });
    const uri = vscode.Uri.file(join(destination, ...scenario.entry));
    const config = vscode.workspace.getConfiguration('yawr', uri);
    const previous = config.inspect<number>('preview.minimumStepDisplayMs')?.workspaceValue;
    const observer = await connectGraphObserver(port);
    let panel: vscode.WebviewPanel | undefined;
    try {
      await config.update('preview.minimumStepDisplayMs', 500, vscode.ConfigurationTarget.Workspace);
      await vscode.window.showTextDocument(await vscode.workspace.openTextDocument(uri));
      panel = await vscode.commands.executeCommand<vscode.WebviewPanel>('yawr.previewGraph');
      await observer.waitForGraph(scenario.title);
      const observation = observer.observe(scenario.steps * 500 + 4000);
      const result = await vscode.commands.executeCommand<{
        frames: Array<{ type: string; runID?: string; event?: { kind: string; payload: Record<string, unknown> } }>;
        finished: { status: string }; stderr: string;
      }>('yawr.runCurrentRunbook');
      const returnedAt = Date.now();
      const samples = await observation;
      await writeFile(join(root, scenario.evidence), JSON.stringify({ samples, returnedAt, result }, null, 2));
      if (process.env.YAWR_TEST_EVIDENCE_DIR) {
        await mkdir(process.env.YAWR_TEST_EVIDENCE_DIR, { recursive: true });
        await copyFile(join(root, scenario.evidence), join(process.env.YAWR_TEST_EVIDENCE_DIR, scenario.evidence));
      }
      assert.equal(result?.finished.status, 'completed', result?.stderr);
      assert.ok(result.frames.some(frame => frame.type === 'run.graph'), 'real runtime graph updates must reach the extension');
      const expected = result.frames.filter(frame => frame.event?.kind === 'step/started').map(frame =>
        String(frame.event!.payload.graph_node_id ?? frame.event!.payload.qualified_node_id));
      const expectedOccurrences = result.frames.filter(frame => frame.event?.kind === 'step/started').map(frame => {
        assert.ok(frame.runID);
        const payload = frame.event!.payload;
        return directOccurrenceID(frame.runID, String(payload.graph_node_id ?? payload.qualified_node_id), payload);
      });
      assert.equal(expected.length, scenario.steps, 'every canonical fixture step must execute');
      assert.equal(new Set(expected).size, scenario.nodes, 'the exact graph sites, including repeated include visits, must execute');
      assert.equal(new Set(expectedOccurrences).size, scenario.steps, 'each canonical execution occurrence must occur exactly once');
      const first = samples.findIndex(sample => sample.ids.length > 0);
      assert.ok(first >= 0, 'the run must display current steps');
      const playback = samples.slice(first);
      const end = playback.findIndex(sample => sample.ids.length === 0);
      assert.ok(end > 0, 'playback must drain after the final Results dwell');
      assert.ok(playback.slice(0, end).every(sample => sample.ids.length === 1), 'graph growth must never remove CURRENT');
      assert.ok(playback.slice(end).every(sample => sample.ids.length === 0), 'there must be no replay');
      const changes = playback.slice(0, end + 1).filter((sample, index) =>
        index === 0 || sample.ids[0] !== playback[index - 1].ids[0]);
      assert.deepEqual(changes.map(sample => sample.ids[0]), [...expected, undefined]);
      for (let index = 1; index < changes.length; index++) {
        assert.ok(changes[index].at - changes[index - 1].at >= 465,
          `included step ${changes[index - 1].ids[0]} dwell was ${changes[index].at - changes[index - 1].at}ms`);
      }
      const final = playback.at(-1)!;
      assert.equal(final.runbooks?.length, scenario.runbooks, 'all runbook invocations must remain navigable after completion');
      assert.equal(final.history?.length, scenario.steps, 'execution history must retain child entry and parent return');
      assert.deepEqual(final.historyIDs, expectedOccurrences, 'retained occurrence identities must exactly match canonical runtime order');
      for (const id of expected) assert.ok(final.nodes?.includes(id), `completed graph lost ${id}`);
      assert.ok(returnedAt < changes.at(-2)!.at, 'runtime completion must not wait for visual playback');
      console.log(`Installed included-runbook CURRENT dwells: ${changes.slice(1).map((sample, index) => sample.at - changes[index].at).join(', ')}ms`);
    } finally {
      await observer.close();
      panel?.dispose();
      await config.update('preview.minimumStepDisplayMs', previous, vscode.ConfigurationTarget.Workspace);
    }
  });

  for (const configuredInterval of [undefined, 500]) test(`one canonical CURRENT stream covers the entire installed runtime run and completion backlog at ${configuredInterval ?? 'default 200'}ms`, async function () {
    this.timeout(60_000);
    assert.strictEqual(vscode.version, '1.136.2', 'compatibility requires the actual minimum supported host');
    const interval = configuredInterval ?? 200;
    const extension = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(extension);
    assert.ok(extension.extensionPath.includes(join('extensions', 'ormasoftchile.yawr-preview-')));
    await extension.activate();
    const root = process.env.YAWR_TEST_STATE_ROOT;
    const port = process.env.YAWR_TEST_CDP_PORT;
    assert.ok(root && port);
    const runbook = vscode.Uri.file(join(root, 'workspace', 'single-current.runbook.yaml'));
    await vscode.workspace.fs.writeFile(runbook, Buffer.from(`apiVersion: yawr.runbook/v1
id: single-current
name: Single current stream
bindings:
  - {name: phase, type: string, mutable: true, value: pending}
outputs:
  status: {type: string, value_expr: phase}
flow:
  - step:
      id: get_database_info
      type: assign
      assign:
        - {name: phase, value: observed}
  - step: {id: container_transition, type: noop}
  - step: {id: configurations, type: noop}
  - step:
      id: final_assertion
      type: assert
      assert:
        - {type: eq, subject: '\${phase}', expected: observed}
  - step: {id: results, type: results, title: Results}
`));
    const config = vscode.workspace.getConfiguration('yawr', runbook);
    const previous = config.inspect<number>('preview.minimumStepDisplayMs')?.workspaceValue;
    const observer = await connectGraphObserver(port);
    let panel: vscode.WebviewPanel | undefined;
    try {
      await config.update('preview.minimumStepDisplayMs', configuredInterval, vscode.ConfigurationTarget.Workspace);
      const effectiveConfig = vscode.workspace.getConfiguration('yawr', runbook);
      assert.equal(effectiveConfig.get<number>('preview.minimumStepDisplayMs'), interval);
      if (configuredInterval === undefined) {
        assert.equal(effectiveConfig.inspect<number>('preview.minimumStepDisplayMs')?.workspaceValue, undefined);
        assert.equal(effectiveConfig.inspect<number>('preview.minimumStepDisplayMs')?.defaultValue, 200);
      }
      await vscode.window.showTextDocument(await vscode.workspace.openTextDocument(runbook));
      panel = await vscode.commands.executeCommand<vscode.WebviewPanel>('yawr.previewGraph');
      assert.ok(panel);
      await observer.waitForGraph('Single current stream');
      const observation = observer.observe();
      const execution = (async () => {
        const result = await vscode.commands.executeCommand<{
          finished: { status: string; resultsAvailability: { state: string } }; stderr: string;
        }>('yawr.runCurrentRunbook');
        const returnedAt = Date.now();
        return { result, returnedAt };
      })();
      const [samples, { result, returnedAt }] = await Promise.all([observation, execution]);
      const evidenceFile = `single-current-${interval}ms-frames.json`;
      await writeFile(join(root, evidenceFile), JSON.stringify({
        vscodeVersion: vscode.version, extensionVersion: extension.packageJSON.version,
        configuredInterval: configuredInterval ?? null, effectiveInterval: interval, samples, returnedAt,
      }, null, 2));
      if (process.env.YAWR_TEST_EVIDENCE_DIR) {
        await mkdir(process.env.YAWR_TEST_EVIDENCE_DIR, { recursive: true });
        await copyFile(join(root, evidenceFile), join(process.env.YAWR_TEST_EVIDENCE_DIR, evidenceFile));
      }
      assert.equal(result?.finished.status, 'completed', result?.stderr);
      assert.equal(result?.finished.resultsAvailability.state, 'available');
      const first = samples.findIndex(sample => sample.ids.length);
      assert.ok(first >= 0, `the entire run must display ordinary current steps; observed ${JSON.stringify(
        [...new Set(samples.map(sample => `${sample.documentID}: ${sample.graphTitle}, ${sample.status}, ${sample.visibility}`))],
      )}`);
      const playback = samples.slice(first);
      const final = playback.findIndex(sample => sample.ids.length === 0);
      assert.ok(final > 0, 'the final dwell must finish');
      assert.ok(playback.slice(0, final).every(sample => sample.ids.length === 1), 'exactly one current throughout playback');
      assert.ok(playback.slice(final).every(sample => sample.ids.length === 0), 'no transient deselection followed by replay');
      const changes = playback.slice(0, final + 1).filter((sample, index) =>
        index === 0 || sample.ids[0] !== playback[index - 1].ids[0]);
      assert.deepStrictEqual(changes.map(sample => sample.ids[0]),
        ['get_database_info', 'container_transition', 'configurations', 'final_assertion', 'results', undefined],
        'each canonical identity, including Results, must appear once and only in order');
      for (let index = 1; index < changes.length; index++) {
        assert.ok(changes[index].at - changes[index - 1].at >= interval - 35,
          `${changes[index - 1].ids[0]} dwell: ${changes[index].at - changes[index - 1].at}ms`);
      }
      for (const sample of playback.slice(0, final)) assert.deepStrictEqual(sample.progress, sample.ids);
      assert.ok(returnedAt < changes[2].at,
        `runtime completion must return while ordinary visuals remain queued: returned=${returnedAt}, transitions=${JSON.stringify(changes.map(sample => ({ at: sample.at, ids: sample.ids })))}`);
      assert.ok(playback.some(sample => sample.ids[0] === 'get_database_info' && sample.status === 'completed' && sample.results === 'available'),
        'Results availability must not wait for its visual position');
      console.log(`Installed VS Code ${vscode.version} whole-run CURRENT ${interval}ms: ${changes.map(sample => `${sample.ids[0] ?? 'drained'}@${sample.at.toFixed(1)}`).join(', ')}`);
    } finally {
      try { await observer.close(); } finally {
        panel?.dispose();
        await config.update('preview.minimumStepDisplayMs', previous, vscode.ConfigurationTarget.Workspace);
      }
    }
  });

  test('loads the deployed extension and opens the graph preview fixture', async () => {
    assert.strictEqual(vscode.version, '1.136.2');
    const stateRoot = process.env.YAWR_TEST_STATE_ROOT;
    assert.ok(stateRoot, 'installed VSIX validation requires YAWR_TEST_STATE_ROOT');
    const stateFile = join(stateRoot, 'diagnostic-state.json');
    const record = async (update: Record<string, unknown>) => {
      let current: Record<string, unknown> = {};
      try {
        current = JSON.parse(await readFile(stateFile, 'utf8'));
      } catch {}
      await writeFile(stateFile, `${JSON.stringify({ ...current, ...update }, null, 2)}\n`);
    };

    const extension = vscode.extensions.getExtension(EXTENSION_ID);
    await record({
      extensionFound: Boolean(extension),
      extensionActiveBeforeActivation: extension?.isActive ?? false,
    });
    assert.ok(extension, `Extension ${EXTENSION_ID} must be installed from the VSIX`);
    assert.strictEqual(extension.packageJSON.name, 'yawr-preview');
    assert.strictEqual(extension.packageJSON.version, process.env.YAWR_EXPECTED_EXTENSION_VERSION);
    assert.ok(extension.extensionPath.includes('.vscode-test') && extension.extensionPath.includes('runs') &&
      extension.extensionPath.includes('extensions'),
      `production surface must load from the installed extensions directory, got ${extension.extensionPath}`);
    try {
      await extension.activate();
      await record({ extensionActiveAfterActivation: extension.isActive, activationError: null });
    } catch (error) {
      await record({
        extensionActiveAfterActivation: extension.isActive,
        activationErrorCategory: error instanceof Error ? error.name : 'Error',
      });
      throw error;
    }

    const commands = await vscode.commands.getCommands(true);
    const expectedCommands = ['yawr.insertRequiredArguments', 'yawr.showHighlightingDiagnostics', 'yawr.preview',
      'yawr.previewGraph', 'yawr.runCurrentRunbook', 'yawr.showServerLog', 'yawr.validateInputs'];
    for (const command of expectedCommands) assert.ok(commands.includes(command), `Missing registered command: ${command}`);
    assert.deepStrictEqual(extension.packageJSON.contributes.commands.map((entry: { command: string }) => entry.command).sort(),
      [...expectedCommands].sort());
    console.log(`Installed VS Code ${vscode.version}: activation succeeded; all ${expectedCommands.length} YAWR commands registered`);
    await record({
      vscodeVersion: vscode.version,
      previewGraphCommandPresent: commands.includes('yawr.previewGraph'),
      yawrCommands: commands.filter((command) => command.startsWith('yawr.')).sort(),
    });
    assert.ok(commands.includes('yawr.previewGraph'));
    assert.ok(!commands.includes('yawr.previewLive'));
    assert.ok(!commands.includes('yawr.restartServer'));
    assert.ok(!commands.includes('yawr.test.openDirectGraphPanel'));
    assert.ok(!commands.includes('yawr.test.getRuntimeState'));
    assert.ok(!commands.includes('yawr.test.openHostActionPanel'));
    assert.ok(!commands.includes('yawr.test.openServedPreviewPanel'));

    const properties = extension.packageJSON.contributes.configuration.properties;
    assert.ok(properties['yawr.binaryPath']);
    assert.strictEqual(properties['yawr.binaryPath']?.deprecationMessage, undefined);
    assert.strictEqual(properties['yawr.binaryPath']?.default, 'yawr');
    assert.strictEqual(properties['yawr.serverUrl'], undefined);
    assert.strictEqual(properties['yawr.autoStartServer'], undefined);
    assert.strictEqual(properties['yawr.xts.showHandoffConfirmation'], undefined);
    const helperUri = vscode.Uri.joinPath(extension.extensionUri, 'bin', 'win32-x64', 'yawr.exe');
    if (process.platform === 'win32' && process.arch === 'x64') {
      await vscode.workspace.fs.stat(helperUri);
    }

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspaceFolder, 'installed VSIX validation requires a workspace folder');
    const runID = process.env.YAWR_TEST_RUN_ID;
    assert.ok(runID, 'installed VSIX validation requires YAWR_TEST_RUN_ID');
    const fixtureSource = vscode.Uri.joinPath(workspaceFolder.uri, 'test', 'fixtures', 'enum.runbook.yaml');
    const fixture = vscode.Uri.joinPath(
      workspaceFolder.uri,
      '.vscode-test',
      'runs',
      runID,
      'workspace',
      'installed-preview.runbook.yaml',
    );
    await vscode.workspace.fs.writeFile(fixture, await vscode.workspace.fs.readFile(fixtureSource));
    const document = await vscode.workspace.openTextDocument(fixture);
    await vscode.window.showTextDocument(document);
    const binaryConfiguration = vscode.workspace.getConfiguration('yawr', fixture);
    const previousBinaryPath = binaryConfiguration.inspect<string>('binaryPath')?.workspaceValue;
    const absentBinaryPath = join(stateRoot, 'asserted-absent-yawr.exe');
    assert.ok(isAbsolute(absentBinaryPath), 'binary sentinel must be absolute');
    await assert.rejects(
      Promise.resolve(vscode.workspace.fs.stat(vscode.Uri.file(absentBinaryPath))),
      (error: unknown) => error instanceof vscode.FileSystemError && error.code === 'FileNotFound',
      `binary sentinel must be absent: ${absentBinaryPath}`,
    );
    let panel: vscode.WebviewPanel | undefined;
    try {
      await binaryConfiguration.update('binaryPath', absentBinaryPath, vscode.ConfigurationTarget.Workspace);
      panel = await vscode.commands.executeCommand<vscode.WebviewPanel>('yawr.previewGraph');
      assert.ok(panel, 'yawr.previewGraph must return its production WebviewPanel');
      await record({ previewGraphCommandExecuted: true, previewGraphCommandError: null });

      const telemetry = await new Promise<RenderTelemetry>((resolve, reject) => {
        let latestMessage: unknown;
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`installed React Flow telemetry timed out after 20 s; latest=${JSON.stringify(latestMessage)}`));
        }, 20_000);
        const subscription = panel!.webview.onDidReceiveMessage((message: unknown) => {
          latestMessage = message;
          if (typeof message !== 'object' || message === null || Array.isArray(message)) return;
          const candidate = message as Record<string, unknown>;
          if (candidate.type === 'render.telemetry.error') {
            clearTimeout(timeout);
            subscription.dispose();
            reject(new Error(`installed webview reported a render error; telemetry=${JSON.stringify(candidate)}`));
            return;
          }
          if (candidate.type !== 'render.telemetry' || candidate.schema !== RENDER_TELEMETRY_SCHEMA) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve(candidate as unknown as RenderTelemetry);
        });
      });
      await record({ renderTelemetry: telemetry, webviewError: null });
      assert.strictEqual(telemetry.reactFlowRootCount, 1);
      assert.strictEqual(telemetry.stepNodeCount, 1);
      assert.deepStrictEqual(telemetry.nodeIDs, ['end']);
      assert.strictEqual(telemetry.frameCount, 0);
      assert.strictEqual(telemetry.edgeCount, 0);

      const deadline = Date.now() + 20_000;
      let preview: vscode.Tab | undefined;
      while (Date.now() < deadline) {
        preview = vscode.window.tabGroups.all
          .flatMap((group) => [...group.tabs])
          .find((tab) => tab.label === 'Yawr: installed-preview.runbook.yaml');
        if (preview?.input instanceof vscode.TabInputWebview) break;
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      const tabs = vscode.window.tabGroups.all.flatMap((group) => [...group.tabs]).map((tab) => ({
        label: tab.label,
        inputType: tab.input?.constructor?.name ?? typeof tab.input,
        rawViewType: tab.input instanceof vscode.TabInputWebview ? tab.input.viewType : null,
        viewType: tab.input instanceof vscode.TabInputWebview
          ? canonicalWebviewViewType(tab.input.viewType)
          : null,
      }));
      await record({ tabs });
      assert.ok(preview, 'yawr.previewGraph must open the installed fixture preview');
      assert.ok(preview.input instanceof vscode.TabInputWebview, 'preview surface must be a webview tab');
      assert.strictEqual(canonicalWebviewViewType(preview.input.viewType), 'yawrPreviewGraph');
    } catch (error) {
      await record({
        previewGraphCommandExecuted: false,
        previewGraphCommandErrorCategory: error instanceof Error ? error.name : 'Error',
      });
      throw error;
    } finally {
      panel?.dispose();
      await binaryConfiguration.update('binaryPath', previousBinaryPath, vscode.ConfigurationTarget.Workspace);
    }
  });

  if (process.platform === 'win32' && process.arch === 'x64') test(
    'executes the packaged native-file-only fixture through production stdio arguments',
    async function() {
    this.timeout(90_000);

    const stateRoot = process.env.YAWR_TEST_STATE_ROOT;
    assert.ok(stateRoot, 'installed VSIX validation requires YAWR_TEST_STATE_ROOT');
    const stateFile = join(stateRoot, 'diagnostic-state.json');
    const record = async (update: Record<string, unknown>) => {
      let current: Record<string, unknown> = {};
      try {
        current = JSON.parse(await readFile(stateFile, 'utf8'));
      } catch {}
      await writeFile(stateFile, `${JSON.stringify({ ...current, ...update }, null, 2)}\n`);
    };

    const extension = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(extension, `Extension ${EXTENSION_ID} must be installed from the VSIX`);
    await extension.activate();
    const extensionRoot = await realpath(extension.extensionPath);
    assertInside(await realpath(join(stateRoot, 'extensions')), extensionRoot, 'installed extension');

    const helper = await realpath(join(extensionRoot, 'bin', 'win32-x64', 'yawr.exe'));
    const fixtureRoot = await realpath(join(extensionRoot, 'fixtures', 'file-only-subprocess', 'win32-x64'));
    const fixture = await realpath(join(fixtureRoot, 'fixture.exe'));
    const stagedInput = await realpath(join(fixtureRoot, 'input.txt'));
    for (const [label, executable] of [['packaged helper', helper], ['packaged fixture', fixture]] as const) {
      assertInside(extensionRoot, executable, label);
    }
    if (process.env.YAWR_E2E_BINARY) {
      assert.notStrictEqual(await realpath(process.env.YAWR_E2E_BINARY), helper,
        'installed qualification must not execute the checkout helper');
    }
    assertInside(extensionRoot, stagedInput, 'packaged staged input');
    const helperSHA256 = await hashFile(helper);
    const fixtureSHA256 = await hashFile(fixture);
    assert.match(process.env.YAWR_EXPECTED_HELPER_SHA256 ?? '', /^[0-9a-f]{64}$/);
    assert.match(process.env.YAWR_EXPECTED_FIXTURE_SHA256 ?? '', /^[0-9a-f]{64}$/);
    assert.strictEqual(process.env.YAWR_EXPECTED_STANDALONE_SHA256, process.env.YAWR_EXPECTED_HELPER_SHA256,
      'standalone runtime must byte-match the helper independently extracted from the final VSIX');
    assert.strictEqual(
      helperSHA256,
      process.env.YAWR_EXPECTED_HELPER_SHA256,
      'installed helper SHA-256 must equal the independently recorded exact package input',
    );
    assert.strictEqual(
      fixtureSHA256,
      process.env.YAWR_EXPECTED_FIXTURE_SHA256,
      'installed fixture SHA-256 must equal the independently recorded exact package input',
    );

    const hostSentinel = join(stateRoot, 'file-only-host-sentinel.txt');
    await writeFile(hostSentinel, 'unchanged');
    let connectionCount = 0;
    const server = createServer((socket) => {
      connectionCount++;
      socket.destroy();
    });
    server.listen(0, '127.0.0.1');
    await once(server, 'listening');
    const address = server.address();
    assert.ok(address && typeof address === 'object', 'loopback listener must expose a TCP address');

    const workspaceRoot = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
    assert.ok(workspaceRoot, 'map-aware qualification requires the isolated workspace folder');
    const nestedRoot = join(workspaceRoot, 'packages', 'nested');
    const nestedRunbooks = join(nestedRoot, 'runbooks');
    const workspaceMap = join(workspaceRoot, 'maps', 'workspace.package-map.yaml');
    await mkdir(join(workspaceRoot, 'runbooks'), { recursive: true });
    await mkdir(join(nestedRoot, 'tools'), { recursive: true });
    await mkdir(nestedRunbooks, { recursive: true });
    await mkdir(join(workspaceRoot, 'maps'), { recursive: true });
    await writeFile(workspaceMap, 'apiVersion: yawr.config/v1\nrequires: []\n');
    await copyFile(fixture, join(nestedRunbooks, 'fixture.exe'));
    await copyFile(stagedInput, join(nestedRunbooks, 'input.txt'));
    const toolPath = join(nestedRunbooks, 'installed-qualification.tool.yaml');
    const runbookPath = join(nestedRunbooks, 'installed-qualification.runbook.yaml');
    const json = (value: string) => JSON.stringify(value);
    await writeFile(toolPath, `apiVersion: yawr.tool/v1
meta: {name: installed-file-only, version: "1.0.0", description: Installed file-only qualification}
transport:
  mode: native-file-only
  command: fixture.exe
  sha256: ${fixtureSHA256}
  inputs: [input.txt]
actions:
  - name: read
    classification: read-only
    argv: [input.txt]
    result: {format: yawr.query-result/v1, source: stdout-json, row_count: count, columns: cols, rows: items}
  - name: network
    classification: read-only
    argv: [network, ${json(`${address.address}:${address.port}`)}]
    result: {format: yawr.query-result/v1, source: stdout-json, row_count: count, columns: cols, rows: items}
  - name: host
    classification: read-only
    argv: [host, ${json(hostSentinel)}]
    result: {format: yawr.query-result/v1, source: stdout-json, row_count: count, columns: cols, rows: items}
  - name: child
    classification: read-only
    argv: [child, ${json(join(process.env.SystemRoot!, 'System32', 'cmd.exe'))}]
    result: {format: yawr.query-result/v1, source: stdout-json, row_count: count, columns: cols, rows: items}
`);
    await writeFile(runbookPath, `apiVersion: yawr.runbook/v1
id: installed-file-only
name: Installed file-only qualification
toolRefs:
  - {name: installed-file-only, path: installed-qualification.tool.yaml}
outputs:
  row_count: {type: integer, value_expr: row_count}
  rows: {type: array, value_expr: rows}
flow:
  - step:
      id: read
      type: tool
      tool: {name: installed-file-only, action: read}
      capture: {row_count: outputs.row_count, rows: outputs.rows}
  - step: {id: network, type: tool, tool: {name: installed-file-only, action: network}}
  - step: {id: host, type: tool, tool: {name: installed-file-only, action: host}}
  - step: {id: child, type: tool, tool: {name: installed-file-only, action: child}}
  - step: {id: publish, type: results, title: Results}
`);

    const runbookDocument = await vscode.workspace.openTextDocument(runbookPath);
    await vscode.window.showTextDocument(runbookDocument);
    const binaryConfiguration = vscode.workspace.getConfiguration('yawr', vscode.Uri.file(runbookPath));
    const previousBinaryPath = binaryConfiguration.inspect<string>('binaryPath')?.workspaceValue;
    const previousPackageMap = binaryConfiguration.inspect<string>('packageMap')?.workspaceValue;
    await binaryConfiguration.update('binaryPath', 'yawr', vscode.ConfigurationTarget.Workspace);
    await binaryConfiguration.update('packageMap', join('maps', 'workspace.package-map.yaml'), vscode.ConfigurationTarget.Workspace);
    const sandboxBase = join(process.env.LOCALAPPDATA!, 'yawr', 'native-file-only');
    const sandboxesBefore = await directoryEntries(sandboxBase);
    const expectedArgs = [
      'run',
      '--stdio',
      '--require-capabilities',
      'yawr.lexical-tool-scopes/v1,yawr.typed-results/v1,yawr.run-results-chunks/v1,yawr.run-graph/v1',
      '--package-map',
      workspaceMap,
      runbookDocument.fileName,
    ];

    const previousParentSentinel = process.env.YAWR_FILE_ONLY_PARENT_SENTINEL;
    process.env.YAWR_FILE_ONLY_PARENT_SENTINEL = 'must-not-inherit';
    let result!: {
      extensionPath: string;
      binary: string;
      args: string[];
      cwd: string;
      frames: Array<Record<string, unknown>>;
      stderr: string;
      finished: Record<string, unknown>;
    };
    try {
      result = await vscode.commands.executeCommand<typeof result>('yawr.runCurrentRunbook');
      assert.strictEqual(await realpath(result.extensionPath), extensionRoot,
        'registered production command must execute from the exact installed extension');
      assert.strictEqual(await realpath(result.binary), helper,
        'default production resolution must select the packaged helper without checkout/PATH fallback');
      assert.strictEqual(await realpath(result.cwd), await realpath(workspaceRoot));
      assert.deepStrictEqual(Buffer.from(result.args.join('\0')), Buffer.from(expectedArgs.join('\0')),
        'registered production command must use the normal byte-for-byte stdio argument construction');
      assert.strictEqual(result.finished.status, 'completed', `installed file-only run failed: ${result.stderr}`);
      const availability = result.finished.resultsAvailability as {
        state?: string;
        publication?: { outputs?: Record<string, { value?: unknown }> };
      };
      assert.strictEqual(availability.state, 'available', 'typed Results must be available at run.finished');
      assert.deepStrictEqual(availability.publication?.outputs?.rows?.value, [['installed-staged-value']]);
    } finally {
      if (previousParentSentinel === undefined) delete process.env.YAWR_FILE_ONLY_PARENT_SENTINEL;
      else process.env.YAWR_FILE_ONLY_PARENT_SENTINEL = previousParentSentinel;
      await binaryConfiguration.update('binaryPath', previousBinaryPath, vscode.ConfigurationTarget.Workspace);
      await binaryConfiguration.update('packageMap', previousPackageMap, vscode.ConfigurationTarget.Workspace);
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }

    await new Promise((resolve) => setTimeout(resolve, 250));
    assert.strictEqual(connectionCount, 0, 'file-only fixture must not connect to the loopback listener');
    assert.strictEqual(await readFile(hostSentinel, 'utf8'), 'unchanged', 'file-only fixture must not mutate the host sentinel');
    assert.deepStrictEqual(await directoryEntries(sandboxBase), sandboxesBefore, 'file-only sandboxes must be cleaned up');
    assert.ok(result.frames.some((frame) => frame.type === 'run.started'));
    assert.ok(result.frames.some((frame) => frame.type === 'run.finished'));
    await record({
      fileOnlyQualificationExecuted: true,
      productionCommandExecuted: 'yawr.runCurrentRunbook',
      installedPackageSHA256Equality: true,
      fileOnlyCommand: [result.binary, ...result.args],
      mapAwareRunRoot: {
        workspaceRoot,
        nestedInferredRoot: nestedRoot,
        packageMap: workspaceMap,
        commandArgs: result.args,
        cwd: result.cwd,
      },
      expectedHelperSHA256: process.env.YAWR_EXPECTED_HELPER_SHA256,
      packagedHelperSHA256: helperSHA256,
      expectedFixtureSHA256: process.env.YAWR_EXPECTED_FIXTURE_SHA256,
      packagedFixtureSHA256: fixtureSHA256,
      standaloneRuntimeSHA256: process.env.YAWR_EXPECTED_STANDALONE_SHA256,
      fileOnlyEvidence: {
        stagedInputRead: true,
        privateScratchWrite: true,
        parentEnvironmentAbsent: true,
        networkDenied: connectionCount === 0,
        hostSentinelReadWriteDenied: (await readFile(hostSentinel, 'utf8')) === 'unchanged',
        childProcessDenied: true,
        sandboxCleaned: true,
        typedResults: true,
      },
      installedExecutionPath: 'registered yawr.runCurrentRunbook -> production graph load -> normal resolveBinary/buildStdioRunArgs/spawn/DirectRunSession flow',
    });
  });
});
