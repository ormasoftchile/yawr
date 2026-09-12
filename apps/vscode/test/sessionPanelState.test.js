'use strict';

const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const {
  SESSION_WORKSPACE_STATE_KEY,
  CoalescedAsyncWriter,
  SessionGraphCacheStore,
  parseSessionGraphRevisionResponse,
  parseStoredSessionDescriptor,
  recoverySequence,
} = require('../out/sessionPanelState');
const { SessionGraphModel } = require('../out/sessionCompositeGraph');

const sessionID = '11111111-1111-4111-8111-111111111111';

test('frozen graph presentation keeps full-byte hashes and exact committed snapshot equality', () => {
  const vector = require('./fixtures/presentation-contract.json');
  const snapshot = `sha256:${'b'.repeat(64)}`;
  const details = structuredClone(vector.graph_details_example);
  Object.assign(details.code_presentation, { origin: 'frozen', plan_snapshot_digest: snapshot });
  const document = {
    schema_version: '1', execution_plan_hash: snapshot,
    runbook: { id: 'entry', name: 'Entry', path: 'entry.runbook.yaml' },
    frames: [], groups: [], edges: [],
    nodes: [{ id: 'query', position: { x: 0, y: 0 }, data: { id: 'query', kind: 'tool', details } }],
  };
  const response = graph => {
    const data = Buffer.from(JSON.stringify(graph));
    const hash = `sha256:${createHash('sha256').update(data).digest('hex')}`;
    return { schema_version: 'yawr.session-graph-revision/v1', session_id: sessionID, graph_revision: 1,
      graph_hash: hash, data: data.toString('base64'), segment: {
        segment_id: 'segment-1', ordinal: 1, runbook_id: 'entry', runbook_name: 'Entry',
        status: 'paused', attempt_run_ids: ['run-1'], graph_revision: 1, graph_hash: hash,
        executable_revision: 1, plan_hash: `sha256:${'a'.repeat(64)}`, executable_snapshot_hash: snapshot,
      } };
  };
  const parse = value => parseSessionGraphRevisionResponse(JSON.stringify(value), sessionID, 'segment-1', 1);
  const original = response(document);
  assert.equal(parse(original).document.nodes[0].data.details.code_presentation.plan_snapshot_digest, snapshot);
  for (const mutate of [
    graph => { graph.nodes[0].data.details.code_presentation.arguments[0].presentation.language = 'kql'; },
    graph => { graph.nodes[0].data.details.code_presentation.plan_snapshot_digest = `sha256:${'c'.repeat(64)}`; },
  ]) {
    const changed = structuredClone(document); mutate(changed);
    assert.throws(() => parse({ ...original, data: response(changed).data }), /digest mismatch/);
  }
  for (const mutate of [
    graph => { graph.nodes[0].data.details.code_presentation.plan_snapshot_digest = `sha256:${'c'.repeat(64)}`; },
    graph => { graph.execution_plan_hash = `sha256:${'c'.repeat(64)}`; },
    graph => { delete graph.execution_plan_hash; },
    graph => { graph.nodes[0].data.details.code_presentation.origin = 'current'; },
  ]) {
    const changed = structuredClone(document); mutate(changed);
    assert.throws(() => parse(response(changed)), /frozen execution-plan binding/);
  }
  const legacy = structuredClone(document);
  delete legacy.execution_plan_hash; delete legacy.nodes[0].data.details.code_presentation;
  assert.deepEqual(parse(response(legacy)).document, legacy);
});

test('session descriptor parsing preserves only reconnect identity and cursor', () => {
  assert.equal(SESSION_WORKSPACE_STATE_KEY, 'yawr.investigationSession.v1');
  const descriptor = parseStoredSessionDescriptor({
    schemaVersion: 'yawr-vscode-session/v1',
    sessionID,
    creationCommandID: '22222222-2222-4222-8222-222222222222',
    runbookPath: 'C:\\work\\entry.runbook.yaml',
    projectRoot: 'C:\\work',
    acceptedSequence: 12,
    ignoredSecret: 'must-not-survive',
  });
  assert.deepEqual(descriptor, {
    schemaVersion: 'yawr-vscode-session/v1',
    sessionID,
    creationCommandID: '22222222-2222-4222-8222-222222222222',
    runbookPath: 'C:\\work\\entry.runbook.yaml',
    projectRoot: 'C:\\work',
    acceptedSequence: 12,
  });
  assert.throws(() => parseStoredSessionDescriptor({ ...descriptor, acceptedSequence: -1 }), /sequence/i);
});

test('graph cache round-trips immutable definitions and controls reconnect cursor', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'yawr-session-cache-'));
  const store = new SessionGraphCacheStore(root);
  const document = {
    schema_version: '1',
    runbook: { id: 'entry', name: 'Entry', path: 'entry.runbook.yaml' },
    frames: [], groups: [], edges: [],
    nodes: [{
      id: 'inspect', type: 'yawrStep', position: { x: 0, y: 0 },
      data: { id: 'inspect', kind: 'noop', details: { kind: 'noop', common: { summary: 'Frozen definition' } } },
    }],
  };
  const encodedDocument = JSON.stringify(document);
  const graphHash = `sha256:${createHash('sha256').update(encodedDocument).digest('hex')}`;
  const segmentSnapshot = {
    segment_id: 'segment-1', ordinal: 1, runbook_id: 'entry', runbook_name: 'Entry',
    status: 'paused', entry_selector: { step: '$entry' }, attempt_run_ids: ['run-1'],
    graph_revision: 1, graph_hash: graphHash,
    executable_revision: 1, plan_hash: `sha256:${'a'.repeat(64)}`,
    executable_snapshot_hash: `sha256:${'b'.repeat(64)}`,
  };
  const cache = {
    schemaVersion: 'yawr-vscode-session-graph-cache/v2',
    sessionID,
    sequence: 7,
    manifest: {
      schema_version: 'investigation-session-manifest/v1',
      session: {
        session_id: sessionID,
        status: 'paused',
        root_segment_id: 'segment-1',
        active_segment_id: 'segment-1',
        active_run_id: 'run-1',
        sequence: 7,
      },
      segments: {
        'segment-1': segmentSnapshot,
      },
      attempts: {
        'run-1': { run_id: 'run-1', segment_id: 'segment-1', ordinal: 1, mode: 'real', status: 'paused_at_boundary' },
      },
      transitions: {
        route: {
          transition_id: 'route', status: 'prepared', source_segment_id: 'segment-1',
          source_occurrence: { run_id: 'run-1', qualified_node_id: 'inspect', step: 'inspect' },
          target_segment_id: 'segment-2', target_run_id: 'run-2', target_runbook_id: 'next',
        },
      }, occurrences: {}, accepted_commands: {},
    },
    segmentGraphs: {
      'segment-1': {
        revision: 1,
        wholeBlobHash: graphHash,
        encodedDocument,
        document,
        segmentSnapshot,
      },
    },
    segmentGraphHistory: {
      'segment-1': {
        1: {
          revision: 1,
          wholeBlobHash: graphHash,
          encodedDocument,
          document,
          segmentSnapshot,
        },
      },
    },
    segmentGraphAvailability: {
      'segment-1': { 1: segmentSnapshot },
    },
    preparedTransitionTargets: {
      route: {
        segment: {
          segment_id: 'segment-2', ordinal: 2, runbook_id: 'next', runbook_name: 'Next',
          status: 'prepared', attempt_run_ids: ['run-2'], graph_revision: 1,
          graph_hash: `sha256:${'b'.repeat(64)}`,
          executable_revision: 1, plan_hash: `sha256:${'c'.repeat(64)}`,
          executable_snapshot_hash: `sha256:${'d'.repeat(64)}`,
        },
        attempt: {
          run_id: 'run-2', segment_id: 'segment-2', ordinal: 1, mode: 'real', status: 'starting',
        },
      },
    },
    runtimeNodes: {
      'session:node': { status: 'completed', output: { result: 'redacted output' } },
    },
    executionNodeID: 'session:node',
  };
  try {
    await store.save(cache);
    const loaded = await store.load(sessionID);
    assert.deepEqual(loaded, cache);
    assert.equal(recoverySequence({ acceptedSequence: 9 }, loaded), 7);
    assert.equal(recoverySequence({ acceptedSequence: 6 }, loaded), 7);
    assert.equal(recoverySequence({ acceptedSequence: 9 }, undefined), 0);
    assert.equal(loaded.segmentGraphs['segment-1'].document.nodes[0].data.details.common.summary, 'Frozen definition');
    const restoredModel = new SessionGraphModel(sessionID);
    const restored = restoredModel.restore(
      loaded.sequence,
      loaded.manifest,
      loaded.segmentGraphs,
      loaded.runtimeNodes,
      loaded.executionNodeID,
      loaded.pending,
      loaded.segmentGraphHistory,
      loaded.preparedTransitionTargets,
    );
    assert.equal(restored.sequence, 7);
    assert.equal(
      restored.document.nodes.find((candidate) => candidate.data.original_node_id === 'inspect').data.details.common.summary,
      'Frozen definition',
    );
    assert.equal(restored.runtimeNodes['session:node'].output.result, 'redacted output');
    assert.equal(restored.executionNodeID, 'session:node');
    assert.equal(restoredModel.graphRevision('segment-1', 1).nodes.length, 1);
    assert.equal(restored.preparedTransitionTargets.route.attempt.run_id, 'run-2');

    const deferredSegment = {
      ...segmentSnapshot,
      graph_revision: 2,
      graph_hash: `sha256:${'b'.repeat(64)}`,
      executable_revision: 2,
      plan_hash: `sha256:${'c'.repeat(64)}`,
      executable_snapshot_hash: `sha256:${'d'.repeat(64)}`,
    };
    const deferredCache = {
      ...cache,
      sequence: 8,
      manifest: {
        ...cache.manifest,
        session: { ...cache.manifest.session, sequence: 8 },
        segments: { 'segment-1': deferredSegment },
      },
      segmentGraphs: {},
      segmentGraphAvailability: {
        'segment-1': { 1: segmentSnapshot, 2: deferredSegment },
      },
    };
    await store.save(deferredCache);
    const deferredLoaded = await store.load(sessionID);
    assert.ok(deferredLoaded);
    const deferredState = new SessionGraphModel(sessionID).restore(
      deferredLoaded.sequence,
      deferredLoaded.manifest,
      deferredLoaded.segmentGraphs,
      deferredLoaded.runtimeNodes,
      deferredLoaded.executionNodeID,
      deferredLoaded.pending,
      deferredLoaded.segmentGraphHistory,
      deferredLoaded.preparedTransitionTargets,
      deferredLoaded.segmentGraphAvailability,
    );
    assert.deepEqual(deferredState.unloadedSegmentIDs, ['segment-1']);
    assert.deepEqual(deferredState.segmentGraphRevisions['segment-1'], [1, 2]);

    await store.save(cache);
    const persisted = JSON.parse(await fs.readFile(store.pathFor(sessionID), 'utf8'));
    assert.match(persisted.integrityDigest, /^sha256:[0-9a-f]{64}$/);
    persisted.segmentGraphs['segment-1'].encodedDocument = JSON.stringify({ ...document, edges: [{ id: 'tampered', source: 'inspect', target: 'inspect' }] });
    delete persisted.integrityDigest;
    persisted.integrityDigest = `sha256:${createHash('sha256').update(JSON.stringify(persisted)).digest('hex')}`;
    await fs.writeFile(store.pathFor(sessionID), JSON.stringify(persisted), 'utf8');
    assert.equal(await store.load(sessionID), undefined);

    await store.save(cache);
    const persistedRuntime = JSON.parse(await fs.readFile(store.pathFor(sessionID), 'utf8'));
    persistedRuntime.runtimeNodes['session:node'].status = 'failed';
    await fs.writeFile(store.pathFor(sessionID), JSON.stringify(persistedRuntime), 'utf8');
    assert.equal(await store.load(sessionID), undefined);

    await fs.writeFile(store.pathFor(sessionID), '{broken', 'utf8');
    assert.equal(await store.load(sessionID), undefined);
  } finally {
    await fs.rm(root, { recursive: true, force: true });
  }
});

test('checkpoint writer keeps one in-flight write and only the latest pending value', async () => {
  const written = [];
  let releaseFirst;
  const firstBlocked = new Promise((resolve) => { releaseFirst = resolve; });
  const writer = new CoalescedAsyncWriter(async (value) => {
    written.push(value);
    if (value === 1) await firstBlocked;
  });

  writer.enqueue(1);
  writer.enqueue(2);
  writer.enqueue(3);
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(written, [1]);
  releaseFirst();
  await writer.flush();
  assert.deepEqual(written, [1, 3]);
});

test('on-demand graph response is identity and hash verified', () => {
  const segmentID = '33333333-3333-4333-8333-333333333333';
  const document = {
    schema_version: '1', runbook: { id: 'history', name: 'History', path: 'history.runbook.yaml' },
    frames: [], groups: [], nodes: [], edges: [],
  };
  const encodedDocument = JSON.stringify(document);
  const graphHash = `sha256:${createHash('sha256').update(encodedDocument).digest('hex')}`;
  const response = JSON.stringify({
    schema_version: 'yawr.session-graph-revision/v1', session_id: sessionID,
    segment: {
      segment_id: segmentID, ordinal: 2, runbook_id: 'history', runbook_name: 'History',
      status: 'completed', attempt_run_ids: ['run-2'], graph_revision: 3, graph_hash: graphHash,
      executable_revision: 3, plan_hash: `sha256:${'a'.repeat(64)}`,
      executable_snapshot_hash: `sha256:${'b'.repeat(64)}`,
    },
    graph_revision: 3, graph_hash: graphHash,
    data: Buffer.from(encodedDocument).toString('base64'),
  });
  const parsed = parseSessionGraphRevisionResponse(response, sessionID, segmentID, 3);
  assert.equal(parsed.wholeBlobHash, graphHash);
  assert.equal(parsed.document.runbook.id, 'history');
  assert.throws(() => parseSessionGraphRevisionResponse(
    response.replace(graphHash, `sha256:${'f'.repeat(64)}`), sessionID, segmentID, 3,
  ), /metadata|digest/i);
});