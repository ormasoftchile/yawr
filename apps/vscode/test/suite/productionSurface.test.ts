import * as assert from 'assert';
import * as vscode from 'vscode';
import { readFile, writeFile } from 'node:fs/promises';
import { isAbsolute, join } from 'node:path';

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
});
