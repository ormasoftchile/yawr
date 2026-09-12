import * as assert from 'assert';
import * as vscode from 'vscode';

const EXTENSION_ID = 'ormasoftchile.yawr-preview';

suite('Installed VSIX production surface', () => {
  test('exposes only the direct runbook view and no test or SSE commands', async () => {
    const extension = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(extension, `Extension ${EXTENSION_ID} must be installed from the VSIX`);
    assert.strictEqual(extension.packageJSON.name, 'yawr-preview');
    assert.ok(extension.extensionPath.includes('.vscode-test') && extension.extensionPath.includes('runs') &&
      extension.extensionPath.includes('extensions'),
      `production surface must load from the installed extensions directory, got ${extension.extensionPath}`);
    await extension.activate();

    const commands = await vscode.commands.getCommands(true);
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
  });
});
