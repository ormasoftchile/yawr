import * as assert from 'assert';
import * as vscode from 'vscode';
import { createHash } from 'node:crypto';
import { once } from 'node:events';
import { createServer } from 'node:net';
import { copyFile, mkdir, readFile, readdir, realpath, writeFile } from 'node:fs/promises';
import { isAbsolute, join, relative } from 'node:path';

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
  test('loads the deployed extension and opens the graph preview fixture', async () => {
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
    await record({
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
      'yawr.typed-results/v1,yawr.run-results-chunks/v1',
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
