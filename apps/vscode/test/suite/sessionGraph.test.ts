import * as assert from 'assert';
import { createHash } from 'crypto';
import { EventEmitter } from 'events';
import { PassThrough } from 'stream';
import * as vscode from 'vscode';

const EXTENSION_ID = 'ormasoftchile.yawr-preview';
const testStatePath = ['.vscode-test', 'runs', process.env.YAWR_TEST_RUN_ID ?? 'test', 'workspace'];
const PROTOCOL = 'yawr.session-stdio/v1';

interface FakeChild extends EventEmitter {
  stdin: PassThrough;
  stdout: PassThrough;
  stderr: PassThrough;
  killed: boolean;
  exitCode: number | null;
  kill(): boolean;
}

function fakeChild(): FakeChild {
  const child = new EventEmitter() as FakeChild;
  child.stdin = new PassThrough();
  child.stdout = new PassThrough();
  child.stderr = new PassThrough();
  child.killed = false;
  child.exitCode = null;
  child.kill = () => {
    child.killed = true;
    return true;
  };
  return child;
}

function digest(data: Buffer | string): string {
  return `sha256:${createHash('sha256').update(data).digest('hex')}`;
}

function graph(runbookID: string, nodes: Array<Record<string, unknown>>, edges: Array<Record<string, unknown>>) {
  return {
    schema_version: '1',
    runbook: { id: runbookID, name: runbookID, path: `${runbookID}.runbook.yaml` },
    frames: [{ id: 'root', runbook_id: runbookID, runbook_path: `${runbookID}.runbook.yaml`, depth: 0 }],
    groups: [],
    nodes,
    edges,
  };
}

function graphNode(id: string, kind: string, x: number, y: number) {
  return {
    id,
    type: 'yawrStep',
    data: { id, kind, title: id, group_id: '', frame_id: 'root' },
    position: { x, y },
  };
}

function compositeNodeID(sessionID: string, segmentID: string, nodeID: string): string {
  return `session:${encodeURIComponent(sessionID)}/segment:${encodeURIComponent(segmentID)}/node:${encodeURIComponent(nodeID)}`;
}

suite('investigation session graph', () => {
  test('renders a cross-runbook session and sends fenced interaction answers', async function () {
    this.timeout(30_000);
    const extension = vscode.extensions.getExtension(EXTENSION_ID);
    assert.ok(extension, `extension ${EXTENSION_ID} must be present`);
    await extension.activate();
    await vscode.commands.executeCommand('yawr.test.clearInvestigationSession');

    const workspace = vscode.workspace.workspaceFolders?.[0];
    assert.ok(workspace, 'session graph test requires a workspace folder');
    const runbookUri = vscode.Uri.joinPath(workspace.uri, ...testStatePath, 'session-graph.runbook.yaml');
    await vscode.workspace.fs.writeFile(runbookUri, Buffer.from('apiVersion: yawr.runbook/v1\nid: session-graph\nname: Session graph\nflow: []\n'));

    const sourceGraph = graph('source', [
      { ...graphNode('source-start', 'include', 0, 0), data: { ...graphNode('source-start', 'include', 0, 0).data, dynamic: true } },
      graphNode('handoff', 'handoff', 0, 120),
    ], [{ id: 'source-flow', source: 'source-start', target: 'handoff' }]);
    const targetGraph = graph('target', [
      graphNode('target-entry', 'noop', 0, 0),
      graphNode('choice', 'choice', 0, 120),
      graphNode('unrelated', 'noop', 420, 420),
    ], [{ id: 'target-flow', source: 'target-entry', target: 'choice' }]);

    const child = fakeChild();
    let spawnArgs: string[] = [];
    let spawnEnvironment: NodeJS.ProcessEnv | undefined;
    let sessionID = '';
    let sourceSegmentID = '';
    let targetSegmentID = '';
    let sourceRunID = '';
    let targetRunID = '';
    let nextFrameID = 0;
    const commands: Array<Record<string, unknown>> = [];
    let commandBuffer = '';
    let resolveAnswer!: (command: Record<string, unknown>) => void;
    const answered = new Promise<Record<string, unknown>>((resolve) => { resolveAnswer = resolve; });

    const writeGroup = (sequence: number, items: Array<Record<string, unknown>>) => {
      items.forEach((item, index) => {
        nextFrameID += 1;
        child.stdout.write(`${JSON.stringify({
          version: PROTOCOL,
          frameID: digest(`frame-${nextFrameID}`),
          sessionID,
          sessionSequence: sequence,
          sequenceIndex: index,
          sequenceCount: items.length,
          writerEpoch: 1,
          ...item,
        })}\n`);
      });
    };
    const graphFrame = (segmentID: string, runID: string, document: unknown) => {
      const encoded = Buffer.from(JSON.stringify(document));
      return {
        type: 'segment.graph',
        segmentID,
        runID,
        payload: {
          graphRevision: 1,
          chunkIndex: 0,
          chunkCount: 1,
          wholeBlobHash: digest(encoded),
          data: encoded.toString('base64'),
        },
      };
    };
    const transition = () => ({
      transition_id: 'route',
      status: 'prepared',
      source_segment_id: sourceSegmentID,
      source_occurrence: {
        run_id: sourceRunID,
        qualified_node_id: 'handoff',
        step: 'handoff',
        phase: 'execute',
        invocation: 1,
        retry_attempt: 1,
        occurrence_sequence: 2,
      },
      target_segment_id: targetSegmentID,
      target_run_id: targetRunID,
      target_runbook_id: 'target',
      reason_code: 'continue',
      reason_summary: 'Continue investigation',
    });
    const segment = (id: string, ordinal: number, runbookID: string, status: string, runID: string) => ({
      segment_id: id,
      ordinal,
      runbook_id: runbookID,
      runbook_name: `${runbookID} runbook`,
      status,
      entry_selector: { step: '$entry' },
      attempt_run_ids: [runID],
      graph_revision: 1,
      graph_hash: digest(Buffer.from(JSON.stringify(runbookID === 'source' ? sourceGraph : targetGraph))),
      executable_revision: 1,
      plan_hash: digest(Buffer.from(`${runbookID}-plan`)),
      executable_snapshot_hash: digest(Buffer.from(`${runbookID}-snapshot`)),
    });
    const attempt = (runID: string, segmentID: string, status: string) => ({
      run_id: runID,
      segment_id: segmentID,
      ordinal: 1,
      mode: 'real',
      status,
    });
    const manifest = (sequence: number, targetStatus: string, includeTarget = true) => ({
      schema_version: 'investigation-session-manifest/v1',
      session: {
        session_id: sessionID,
        status: 'active',
        root_segment_id: sourceSegmentID,
        active_segment_id: includeTarget ? targetSegmentID : sourceSegmentID,
        active_run_id: includeTarget ? targetRunID : sourceRunID,
        sequence,
      },
      segments: includeTarget ? {
        [sourceSegmentID]: segment(sourceSegmentID, 1, 'source', 'handed_off', sourceRunID),
        [targetSegmentID]: segment(targetSegmentID, 2, 'target', 'active', targetRunID),
      } : {
        [sourceSegmentID]: segment(sourceSegmentID, 1, 'source', 'active', sourceRunID),
      },
      attempts: includeTarget ? {
        [sourceRunID]: attempt(sourceRunID, sourceSegmentID, 'completed'),
        [targetRunID]: attempt(targetRunID, targetSegmentID, targetStatus),
      } : {
        [sourceRunID]: attempt(sourceRunID, sourceSegmentID, 'running'),
      },
      transitions: includeTarget ? { route: { ...transition(), status: 'committed' } } : {},
      occurrences: {},
      accepted_commands: {},
    });

    child.stdin.setEncoding('utf8');
    child.stdin.on('data', (chunk: string) => {
      commandBuffer += chunk;
      let newline = commandBuffer.indexOf('\n');
      while (newline >= 0) {
        const command = JSON.parse(commandBuffer.slice(0, newline)) as Record<string, unknown>;
        commandBuffer = commandBuffer.slice(newline + 1);
        commands.push(command);
        if (command.type === 'interaction.answer') resolveAnswer(command);
        if (command.type === 'session.detach') {
          child.exitCode = 0;
          child.emit('close', 0, null);
        }
        newline = commandBuffer.indexOf('\n');
      }
    });

    const emitInitialHistory = () => {
      writeGroup(1, [
        { type: 'session.snapshot', segmentID: sourceSegmentID, runID: sourceRunID, payload: manifest(1, 'running', false) },
        graphFrame(sourceSegmentID, sourceRunID, sourceGraph),
        {
          type: 'run.event', segmentID: sourceSegmentID, runID: sourceRunID,
          payload: {
            event_id: 'event-source-started', run_id: sourceRunID, runbook_id: 'source', sequence: 1,
            timestamp: '2026-09-02T00:00:00Z', kind: 'step/started',
            payload: { node_id: 'source-start', step_id: 'source-start' },
          },
        },
        {
          type: 'run.event', segmentID: sourceSegmentID, runID: sourceRunID,
          payload: {
            event_id: 'event-source-completed', run_id: sourceRunID, runbook_id: 'source', sequence: 2,
            timestamp: '2026-09-02T00:00:01Z', kind: 'step/completed',
            payload: { node_id: 'source-start', step_id: 'source-start' },
          },
        },
        {
          type: 'run.event', segmentID: sourceSegmentID, runID: sourceRunID,
          payload: {
            event_id: 'event-handoff-started', run_id: sourceRunID, runbook_id: 'source', sequence: 3,
            timestamp: '2026-09-02T00:00:02Z', kind: 'step/started',
            payload: { node_id: 'handoff', step_id: 'handoff' },
          },
        },
        {
          type: 'run.event', segmentID: sourceSegmentID, runID: sourceRunID,
          payload: {
            event_id: 'event-handoff-completed', run_id: sourceRunID, runbook_id: 'source', sequence: 4,
            timestamp: '2026-09-02T00:00:03Z', kind: 'step/completed',
            payload: { node_id: 'handoff', step_id: 'handoff' },
          },
        },
      ]);
    writeGroup(1, [{
    type: 'session.snapshot',
    segmentID: sourceSegmentID,
    runID: sourceRunID,
    payload: manifest(1, 'running', false),
    }]);
      writeGroup(2, [{
        type: 'transition.prepared',
        segmentID: sourceSegmentID,
        runID: sourceRunID,
        payload: {
          transition: transition(),
          target_segment: segment(targetSegmentID, 2, 'target', 'prepared', targetRunID),
          target_attempt: attempt(targetRunID, targetSegmentID, 'starting'),
        },
      }]);
      writeGroup(3, [
        {
          type: 'transition.committed',
          segmentID: targetSegmentID,
          runID: targetRunID,
          payload: {
            transition_id: 'route',
            target_segment: segment(targetSegmentID, 2, 'target', 'prepared', targetRunID),
            target_attempt: attempt(targetRunID, targetSegmentID, 'starting'),
          },
        },
        graphFrame(targetSegmentID, targetRunID, targetGraph),
      ]);
    };
    const emitPendingInteraction = () => {
      writeGroup(4, [
        { type: 'session.snapshot', segmentID: targetSegmentID, runID: targetRunID, payload: manifest(4, 'waiting') },
        {
          type: 'run.event',
          segmentID: targetSegmentID,
          runID: targetRunID,
          payload: {
            event_id: 'event-choice',
            run_id: targetRunID,
            runbook_id: 'target',
            sequence: 1,
            timestamp: '2026-09-02T00:00:00Z',
            kind: 'step/started',
            payload: { node_id: 'choice', step_id: 'choice' },
          },
        },
        {
          type: 'interaction.pending',
          segmentID: targetSegmentID,
          runID: targetRunID,
          payload: {
            schema_version: 'interaction-state/v1',
            turn_id: 'turn-choice',
            owner_step_id: 'choice',
            node_id: 'choice',
            step_id: 'choice',
            kind: 'choice',
            ordinal: 1,
            status: 'pending',
            request_digest: digest('choice-request'),
            request: {
              type: 'pending',
              runID: targetRunID,
              stepID: 'choice',
              nodeID: 'choice',
              kind: 'choice',
              prompt: 'Choose one',
              options: [{ value: 'o:0', label: 'One' }],
            },
          },
        },
      ]);
    };

    type UIState = {
      sessionID?: string;
      sessionStatus?: string;
      sessionAttached?: boolean;
      runID?: string;
      runStatus: string;
      pendingKind?: string;
      segmentCount?: number;
      handoffEdgeCount?: number;
      graphNodeIDs?: string[];
      visibleButtons?: string[];
    };
    const waitForUI = (panel: vscode.WebviewPanel, predicate: (state: UIState) => boolean, failure: string) =>
      new Promise<UIState>((resolve, reject) => {
        let latest: UIState | undefined;
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`${failure}; latest=${JSON.stringify(latest)}`));
        }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const state = message as { type?: unknown } & UIState;
          if (state?.type === 'ui.state') latest = state;
          if (state?.type !== 'ui.state' || !predicate(state)) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve(state);
        });
      });
    const waitForRendered = (panel: vscode.WebviewPanel, nodeCount: number) =>
      new Promise<void>((resolve, reject) => {
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`webview did not render ${nodeCount} route nodes`));
        }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const rendered = message as { type?: unknown; nodeCount?: unknown };
          if (rendered?.type !== 'rendered' || rendered.nodeCount !== nodeCount) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve();
        });
      });
    const waitForSelectedNode = (panel: vscode.WebviewPanel, nodeID: string) =>
      new Promise<void>((resolve, reject) => {
        const timeout = setTimeout(() => {
          subscription.dispose();
          reject(new Error(`webview did not select ${nodeID}`));
        }, 10_000);
        const subscription = panel.webview.onDidReceiveMessage((message: unknown) => {
          const selected = message as { type?: unknown; nodeID?: unknown };
          if (selected?.type !== 'inspector.state' || selected.nodeID !== nodeID) return;
          clearTimeout(timeout);
          subscription.dispose();
          resolve();
        });
      });

    let panel: vscode.WebviewPanel | undefined;
    try {
      panel = await vscode.commands.executeCommand<vscode.WebviewPanel>(
        'yawr.test.openDirectGraphPanel',
        runbookUri.fsPath,
        {
          documentLoader: async () => sourceGraph,
          spawnSession: (_binary: string, args: string[], options: { env?: NodeJS.ProcessEnv }) => {
            spawnArgs = args;
            spawnEnvironment = options.env;
            sessionID = args[args.indexOf('--session-id') + 1];
            sourceSegmentID = '22222222-2222-4222-8222-222222222222';
            targetSegmentID = '33333333-3333-4333-8333-333333333333';
            sourceRunID = '44444444-4444-4444-8444-444444444444';
            targetRunID = '55555555-5555-4555-8555-555555555555';
            setImmediate(emitInitialHistory);
            return child;
          },
        },
      );
      assert.ok(panel, 'test command must return the graph panel');
      await waitForUI(
        panel,
        (state) => state.visibleButtons?.includes('Start session') === true,
        'session start control did not render',
      );

      const composed = waitForUI(
        panel,
        (state) => state.sessionAttached === true && state.segmentCount === 2 && state.handoffEdgeCount === 1,
        'composite session graph did not render',
      );
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Start session' });
      const composedState = await composed;
      assert.match(sessionID, /^[0-9a-f-]{36}$/i);
      assert.deepStrictEqual(spawnArgs.slice(0, 3), ['session', 'start', runbookUri.fsPath]);
      assert.ok(spawnArgs.includes('--stdio'));
      const configureCommand = commands.find((command) => command.type === 'session.configure');
      assert.deepStrictEqual(configureCommand, {
        version: PROTOCOL,
        type: 'session.configure',
        commandID: configureCommand?.commandID,
        sessionID,
        writerEpoch: 1,
        expectedSequence: 1,
        payload: { inputs: {} },
      });
      assert.match(spawnEnvironment?.YAWR_VSCODE_BRIDGE_URL ?? '', /^http:\/\/127\.0\.0\.1:\d+$/);
      assert.match(spawnEnvironment?.YAWR_VSCODE_BRIDGE_TOKEN ?? '', /^[0-9a-f]{64}$/);
      assert.strictEqual(composedState.runID, targetRunID);
      assert.ok(composedState.graphNodeIDs?.includes(compositeNodeID(sessionID, sourceSegmentID, 'handoff')));
      assert.ok(composedState.graphNodeIDs?.includes(compositeNodeID(sessionID, targetSegmentID, 'choice')));

      const routeRendered = waitForRendered(panel, 4);
      const targetNodeID = compositeNodeID(sessionID, targetSegmentID, 'choice');
      const selected = waitForSelectedNode(panel, targetNodeID);
      await panel.webview.postMessage({
        type: 'test.action',
        action: 'select-node',
        name: targetNodeID,
      });
      await selected;
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Show routes through this step' });
      await routeRendered;

      const waiting = waitForUI(
        panel,
        (state) => state.runStatus === 'waiting' && state.pendingKind === 'choice',
        'session choice did not become pending',
      );
      emitPendingInteraction();
      await waiting;

      await panel.webview.postMessage({ type: 'test.action', action: 'toggle-choice', name: 'One' });
      await panel.webview.postMessage({ type: 'test.action', action: 'click-button', name: 'Continue' });
      const answerCommand = await answered;
      assert.strictEqual(answerCommand.version, PROTOCOL);
      assert.strictEqual(answerCommand.type, 'interaction.answer');
      assert.strictEqual(answerCommand.sessionID, sessionID);
      assert.strictEqual(answerCommand.writerEpoch, 1);
      assert.strictEqual(answerCommand.expectedSequence, 4);
      assert.strictEqual(answerCommand.runID, targetRunID);
      assert.strictEqual(answerCommand.turnID, 'turn-choice');
      assert.deepStrictEqual(answerCommand.payload, { kind: 'choice', selected: ['o:0'] });
      assert.match(String(answerCommand.commandID), /^[0-9a-f-]{36}$/i);
    } finally {
      panel?.dispose();
      await vscode.commands.executeCommand('yawr.test.clearInvestigationSession');
      await vscode.workspace.fs.delete(runbookUri, { useTrash: false }).then(undefined, () => undefined);
      assert.ok(commands.some((command) => command.type === 'session.detach') || child.killed,
        'panel disposal must detach or stop the session process');
    }
  });
});

