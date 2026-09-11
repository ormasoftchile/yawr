'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const {
  composeSessionGraph,
  SessionGraphModel,
  sessionGraphEntryNodeID,
  sessionGraphNodeID,
  sessionGraphTopologyKey,
} = require('../out/sessionCompositeGraph');
const {
  computeRouteProjection,
  computeSessionRouteProjection,
  sessionRouteDocument,
} = require('../out/routeProjection');

function node(id, kind = 'noop') {
  return {
    id,
    type: 'yawrStep',
    data: { id, kind, title: id, frame_id: 'root', group_id: '' },
    position: { x: 0, y: 0 },
  };
}

function graph(runbookID, nodes, edges) {
  return {
    schema_version: '1',
    runbook: { id: runbookID, name: runbookID, path: `${runbookID}.runbook.yaml` },
    frames: [{ id: 'root', runbook_id: runbookID, runbook_path: `${runbookID}.runbook.yaml`, depth: 0 }],
    groups: [],
    nodes,
    edges,
  };
}

test('composite session graph namespaces repeated IDs and routes across committed handoffs', () => {
  const sessionID = '11111111-1111-4111-8111-111111111111';
  const sourceSegmentID = '22222222-2222-4222-8222-222222222222';
  const targetSegmentID = '33333333-3333-4333-8333-333333333333';
  const sourceRunID = '44444444-4444-4444-8444-444444444444';
  const targetRunID = '55555555-5555-4555-8555-555555555555';
  const manifest = {
    schema_version: 'investigation-session-manifest/v1',
    session: {
      session_id: sessionID,
      status: 'active',
      root_segment_id: sourceSegmentID,
      active_segment_id: targetSegmentID,
      active_run_id: targetRunID,
      sequence: 7,
    },
    segments: {
      [sourceSegmentID]: {
        segment_id: sourceSegmentID,
        ordinal: 1,
        runbook_id: 'source',
        runbook_name: 'Source runbook',
        status: 'handed_off',
        entry_selector: { step: '$entry' },
        attempt_run_ids: [sourceRunID],
      },
      [targetSegmentID]: {
        segment_id: targetSegmentID,
        ordinal: 2,
        runbook_id: 'target',
        runbook_name: 'Target runbook',
        status: 'active',
        entry_selector: { step: '$entry' },
        attempt_run_ids: [targetRunID],
      },
    },
    attempts: {
      [sourceRunID]: { run_id: sourceRunID, segment_id: sourceSegmentID, ordinal: 1, mode: 'real', status: 'completed' },
      [targetRunID]: { run_id: targetRunID, segment_id: targetSegmentID, ordinal: 1, mode: 'real', status: 'running' },
    },
    transitions: {
      route: {
        transition_id: 'route',
        status: 'committed',
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
      },
    },
    occurrences: {},
    accepted_commands: {},
  };
  const source = graph('source', [node('shared'), node('unvisited'), node('handoff', 'handoff')], [
    { id: 'source-flow', source: 'shared', target: 'handoff' },
    { id: 'unvisited-flow', source: 'unvisited', target: 'handoff' },
  ]);
  const target = graph('target', [node('shared'), node('target')], [
    { id: 'target-flow', source: 'shared', target: 'target' },
  ]);

  const document = composeSessionGraph({
    sessionID,
    manifest,
    segmentGraphs: new Map([
      [sourceSegmentID, source],
      [targetSegmentID, target],
    ]),
  });

  const sourceShared = sessionGraphNodeID(sessionID, sourceSegmentID, 'shared');
  const targetShared = sessionGraphNodeID(sessionID, targetSegmentID, 'shared');
  const targetNode = sessionGraphNodeID(sessionID, targetSegmentID, 'target');
  assert.notEqual(sourceShared, targetShared);
  assert.ok(document.nodes.some((candidate) => candidate.id === sourceShared));
  assert.ok(document.nodes.some((candidate) => candidate.id === targetShared));
  assert.equal(document.nodes.find((candidate) => candidate.id === sourceShared).data.segment_status, 'handed_off');
  assert.equal(document.nodes.find((candidate) => candidate.id === targetShared).data.run_id, targetRunID);

  const segmentGroups = document.groups.filter((group) => group.kind === 'session-segment');
  assert.equal(segmentGroups.length, 2);
  const sourceGroup = segmentGroups.find((group) => group.segment_id === sourceSegmentID);
  const targetGroup = segmentGroups.find((group) => group.segment_id === targetSegmentID);
  assert.ok(sourceGroup);
  assert.ok(targetGroup);
  assert.match(sourceGroup.label, /1.*Source runbook/i);
  assert.match(targetGroup.label, /2.*Target runbook/i);
  assert.equal(sourceGroup.segment_status, 'handed_off');
  assert.equal(targetGroup.segment_status, 'active');
  assert.equal(document.nodes.find((candidate) => candidate.id === sourceShared).parentNode, sourceGroup.id);
  assert.equal(document.nodes.find((candidate) => candidate.id === targetShared).parentNode, targetGroup.id);

  const sourceEntry = sessionGraphEntryNodeID(sessionID, sourceSegmentID);
  const targetEntry = sessionGraphEntryNodeID(sessionID, targetSegmentID);
  assert.equal(document.nodes.find((candidate) => candidate.id === sourceEntry).data.kind, 'session-entry');
  assert.equal(document.nodes.find((candidate) => candidate.id === targetEntry).parentNode, targetGroup.id);
  assert.deepEqual(document.edges
    .filter((edge) => edge.source === sourceEntry)
    .map((edge) => edge.target)
    .sort(), [
      sourceShared,
      sessionGraphNodeID(sessionID, sourceSegmentID, 'unvisited'),
    ].sort());
  assert.deepEqual(document.edges.filter((edge) => edge.source === targetEntry).map((edge) => edge.target), [targetShared]);

  const transition = document.edges.find((edge) => edge.type === 'session-transition');
  assert.ok(transition);
  assert.equal(transition.source, sessionGraphNodeID(sessionID, sourceSegmentID, 'handoff'));
  assert.equal(transition.target, targetEntry);
  assert.equal(transition.label, 'Continue investigation');

  const actualDocument = sessionRouteDocument(document, {
    [sourceShared]: {
      status: 'completed',
      occurrences: [{
        occurrenceID: 'source', runID: sourceRunID, segmentID: sourceSegmentID,
        qualifiedNodeID: 'shared', phase: 'execute', invocation: 1, retryAttempt: 1,
        occurrenceSequence: 1, executionSource: 'live', status: 'completed', startedEventSequence: 1,
      }],
    },
    [transition.source]: {
      status: 'completed',
      occurrences: [{
        occurrenceID: 'handoff', runID: sourceRunID, segmentID: sourceSegmentID,
        qualifiedNodeID: 'handoff', phase: 'execute', invocation: 1, retryAttempt: 1,
        occurrenceSequence: 2, executionSource: 'live', status: 'completed', startedEventSequence: 2,
        predecessorNodeID: sourceShared,
      }],
    },
    [sessionGraphNodeID(sessionID, sourceSegmentID, 'unvisited')]: {
      status: 'completed',
      occurrences: [{
        occurrenceID: 'unvisited', runID: sourceRunID, segmentID: sourceSegmentID,
        qualifiedNodeID: 'unvisited', phase: 'execute', invocation: 1, retryAttempt: 1,
        occurrenceSequence: 3, executionSource: 'live', status: 'completed', startedEventSequence: 3,
      }],
    },
  });
  const projection = computeRouteProjection(actualDocument, targetNode, 'to');
  assert.ok(projection);
  assert.deepEqual([...projection.nodeIDs].sort(), [sourceEntry, sourceShared, transition.source, targetEntry, targetShared, targetNode].sort());
  assert.ok(projection.edgeIDs.has(transition.id));
  assert.ok(!projection.nodeIDs.has(sessionGraphNodeID(sessionID, sourceSegmentID, 'unvisited')));
  assert.ok(!actualDocument.edges.some((edge) => edge.id.includes('unvisited-flow')));

  const fromProjection = computeSessionRouteProjection(actualDocument, targetNode, 'from');
  assert.ok(fromProjection);
  assert.ok(fromProjection.nodeIDs.has(sourceShared));
  assert.ok(fromProjection.nodeIDs.has(transition.source));
  assert.ok(fromProjection.edgeIDs.has(transition.id));
  assert.ok(!fromProjection.nodeIDs.has(sessionGraphNodeID(sessionID, sourceSegmentID, 'unvisited')));

  const historicalTarget = sessionGraphNodeID(sessionID, sourceSegmentID, 'unvisited');
  const historicalDocument = sessionRouteDocument(document, {
    [sourceShared]: { status: 'completed' },
    [transition.source]: { status: 'completed' },
  }, historicalTarget);
  const historicalProjection = computeSessionRouteProjection(historicalDocument, historicalTarget, 'through');
  assert.ok(historicalProjection);
  assert.ok(historicalProjection.nodeIDs.has(historicalTarget));
  assert.ok(!historicalDocument.nodes.some((candidate) => candidate.data.segment_id === targetSegmentID));

  const ambiguousManifest = structuredClone(manifest);
  ambiguousManifest.transitions.route.source_occurrence.qualified_node_id = 'missing-qualified-node';
  ambiguousManifest.transitions.route.source_occurrence.step = 'duplicate-authored-step';
  const duplicateSource = graph('source', [
    { ...node('first'), data: { ...node('first').data, step_id: 'duplicate-authored-step' } },
    { ...node('second'), data: { ...node('second').data, step_id: 'duplicate-authored-step' } },
  ], []);
  assert.throws(() => composeSessionGraph({
    sessionID,
    manifest: ambiguousManifest,
    segmentGraphs: new Map([
      [sourceSegmentID, duplicateSource],
      [targetSegmentID, target],
    ]),
  }), /transition source.*unresolved/i);
});

test('session graph model projects runtime and pending interaction onto immutable graph nodes', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const manifest = {
    schema_version: 'investigation-session-manifest/v1',
    session: {
      session_id: sessionID,
      status: 'active',
      root_segment_id: segmentID,
      active_segment_id: segmentID,
      active_run_id: runID,
      sequence: 1,
    },
    segments: {
      [segmentID]: {
        segment_id: segmentID,
        ordinal: 1,
        runbook_id: 'entry',
        runbook_name: 'Entry',
        status: 'active',
        entry_selector: { step: '$entry' },
        attempt_run_ids: [runID],
        graph_revision: 1,
        graph_hash: `sha256:${'a'.repeat(64)}`,
        executable_revision: 1,
        plan_hash: `sha256:${'b'.repeat(64)}`,
        executable_snapshot_hash: `sha256:${'c'.repeat(64)}`,
      },
    },
    attempts: {
      [runID]: { run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'running' },
    },
    transitions: {}, occurrences: {}, accepted_commands: {},
  };
  const definition = { common: { summary: 'Immutable snapshot definition' } };
  const segmentGraph = graph('entry', [{
    ...node('choose', 'choice'),
    data: { ...node('choose', 'choice').data, details: definition },
  }], []);
  const model = new SessionGraphModel(sessionID);
  const definitionState = model.applyGroup({
    sequence: 1,
    writerEpoch: 2,
    handshake: false,
    graphs: [{ segmentID, runID, revision: 1, wholeBlobHash: `sha256:${'a'.repeat(64)}`, document: segmentGraph }],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:frame',
      sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 2,
      segmentID, runID, payload: manifest,
    }],
  });
  const runtimeState = model.applyGroup({
    sequence: 2,
    writerEpoch: 2,
    handshake: false,
    graphs: [],
    frames: [
      {
        version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:runtime-snapshot',
        sessionID, sessionSequence: 2, sequenceIndex: 0, sequenceCount: 3, writerEpoch: 2,
        segmentID, runID,
        payload: {
          ...manifest,
          session: { ...manifest.session, sequence: 2 },
          accepted_commands: { runtime: { sequence: 2 } },
        },
      },
      {
        version: 'yawr.session-stdio/v1', type: 'run.event', frameID: 'sha256:event',
        sessionID, sessionSequence: 2, sequenceIndex: 1, sequenceCount: 3, writerEpoch: 2,
        segmentID, runID,
        payload: {
          event_id: 'event', run_id: runID, runbook_id: 'entry', sequence: 1,
          timestamp: '2026-09-02T00:00:00Z', kind: 'step/started',
          payload: { node_id: 'choose', step_id: 'choose' },
        },
      },
      {
        version: 'yawr.session-stdio/v1', type: 'interaction.pending', frameID: 'sha256:pending',
        sessionID, sessionSequence: 2, sequenceIndex: 2, sequenceCount: 3, writerEpoch: 2,
        segmentID, runID,
        payload: {
          schema_version: 'interaction-state/v1', turn_id: 'turn-1', owner_step_id: 'choose',
          node_id: 'choose', step_id: 'choose', kind: 'choice', ordinal: 1, status: 'pending',
          request_digest: 'sha256:request',
          request: {
            type: 'pending', kind: 'choice', stepID: 'choose', prompt: 'Choose one',
            options: [{ label: 'One', value: 'o:0' }],
          },
        },
      },
    ],
  });

  const state = model.snapshot();
  const compositeNodeID = sessionGraphNodeID(sessionID, segmentID, 'choose');
  assert.equal(state.sequence, 2);
  assert.equal(state.sessionStatus, 'active');
  assert.equal(state.runStatus, 'waiting');
  assert.equal(state.executionNodeID, compositeNodeID);
  assert.equal(state.runtimeNodes[compositeNodeID].status, 'running');
  assert.equal(state.pending.turnID, 'turn-1');
  assert.equal(state.pending.nodeID, compositeNodeID);
  assert.equal(state.pending.prompt, 'Choose one');
  assert.deepEqual(state.document.nodes.find((candidate) => candidate.data.original_node_id === 'choose').data.details, definition);
  assert.strictEqual(runtimeState.document, definitionState.document);
});

test('session graph model loads historical segment definitions on demand without moving the cursor', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const sourceSegmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const targetSegmentID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const sourceRunID = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd';
  const targetRunID = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee';
  const source = graph('source', [node('handoff', 'handoff')], []);
  const target = graph('target', [node('target')], []);
  const sourceEncoded = JSON.stringify(source);
  const targetEncoded = JSON.stringify(target);
  const hash = (value) => `sha256:${require('node:crypto').createHash('sha256').update(value).digest('hex')}`;
  const sourceHash = hash(sourceEncoded);
  const targetHash = hash(targetEncoded);
  const sourceSegment = {
    segment_id: sourceSegmentID, ordinal: 1, runbook_id: 'source', runbook_name: 'Source',
    status: 'handed_off', attempt_run_ids: [sourceRunID], graph_revision: 1, graph_hash: sourceHash,
    executable_revision: 1, plan_hash: `sha256:${'a'.repeat(64)}`,
    executable_snapshot_hash: `sha256:${'b'.repeat(64)}`,
  };
  const targetSegment = {
    segment_id: targetSegmentID, ordinal: 2, runbook_id: 'target', runbook_name: 'Target',
    status: 'active', attempt_run_ids: [targetRunID], graph_revision: 1, graph_hash: targetHash,
    executable_revision: 1, plan_hash: `sha256:${'c'.repeat(64)}`,
    executable_snapshot_hash: `sha256:${'d'.repeat(64)}`,
  };
  const model = new SessionGraphModel(sessionID);
  const state = model.applyGroup({
    sequence: 1, writerEpoch: 1, handshake: false,
    graphs: [{ segmentID: targetSegmentID, runID: targetRunID, revision: 1, wholeBlobHash: targetHash, document: target }],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:snapshot',
      sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      payload: {
        schema_version: 'investigation-session-manifest/v1',
        session: {
          session_id: sessionID, status: 'active', root_segment_id: sourceSegmentID,
          active_segment_id: targetSegmentID, active_run_id: targetRunID, sequence: 1,
        },
        segments: { [sourceSegmentID]: sourceSegment, [targetSegmentID]: targetSegment },
        attempts: {
          [sourceRunID]: { run_id: sourceRunID, segment_id: sourceSegmentID, ordinal: 1, mode: 'real', status: 'completed' },
          [targetRunID]: { run_id: targetRunID, segment_id: targetSegmentID, ordinal: 1, mode: 'real', status: 'running' },
        },
        transitions: { route: {
          transition_id: 'route', status: 'committed', source_segment_id: sourceSegmentID,
          source_occurrence: { run_id: sourceRunID, qualified_node_id: 'handoff', step: 'handoff' },
          target_segment_id: targetSegmentID, target_run_id: targetRunID, target_runbook_id: 'target',
        } },
        occurrences: {}, accepted_commands: {},
      },
    }],
  });
  const placeholderID = sessionGraphNodeID(sessionID, sourceSegmentID, 'handoff');
  assert.deepEqual(state.unloadedSegmentIDs, [sourceSegmentID]);
  assert.equal(state.document.nodes.find((candidate) => candidate.id === placeholderID).data.graph_loaded, false);
  assert.equal(state.document.groups.filter((group) => group.kind === 'session-segment').length, 2);
  assert.equal(state.document.edges.find((edge) => edge.type === 'session-transition').source, placeholderID);

  const loaded = model.loadGraphRevision(sourceSegment, 1, sourceHash, source);
  assert.equal(loaded.sequence, 1);
  assert.deepEqual(loaded.unloadedSegmentIDs, []);
  assert.equal(loaded.document.nodes.find((candidate) => candidate.id === placeholderID).data.graph_loaded, true);
});

test('session graph model merges event-scoped snapshots without erasing history', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const sourceSegmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const targetSegmentID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const sourceRunID = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd';
  const targetRunID = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee';
  const model = new SessionGraphModel(sessionID);
  const fullManifest = {
    schema_version: 'investigation-session-manifest/v1',
    session: {
      session_id: sessionID, status: 'active', root_segment_id: sourceSegmentID,
      active_segment_id: targetSegmentID, active_run_id: targetRunID, sequence: 1,
    },
    segments: {
      [sourceSegmentID]: {
        segment_id: sourceSegmentID, ordinal: 1, runbook_id: 'source', runbook_name: 'Source',
        status: 'handed_off', attempt_run_ids: [sourceRunID],
      },
      [targetSegmentID]: {
        segment_id: targetSegmentID, ordinal: 2, runbook_id: 'target', runbook_name: 'Target',
        status: 'active', attempt_run_ids: [targetRunID], graph_revision: 1,
        graph_hash: `sha256:${'a'.repeat(64)}`,
        executable_revision: 1, plan_hash: `sha256:${'b'.repeat(64)}`,
        executable_snapshot_hash: `sha256:${'c'.repeat(64)}`,
      },
    },
    attempts: {
      [sourceRunID]: { run_id: sourceRunID, segment_id: sourceSegmentID, ordinal: 1, mode: 'real', status: 'completed' },
      [targetRunID]: { run_id: targetRunID, segment_id: targetSegmentID, ordinal: 1, mode: 'real', status: 'running' },
    },
    transitions: {
      route: {
        transition_id: 'route', status: 'committed', source_segment_id: sourceSegmentID,
        source_occurrence: { run_id: sourceRunID, qualified_node_id: 'handoff', step: 'handoff' },
        target_segment_id: targetSegmentID, target_run_id: targetRunID, target_runbook_id: 'target',
      },
    },
    occurrences: { historical: { status: 'completed' } },
    accepted_commands: { old: { sequence: 1 } },
  };
  model.applyGroup({
    sequence: 1, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:full',
      sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID: targetSegmentID, runID: targetRunID, payload: fullManifest,
    }],
  });

  model.applyGroup({
    sequence: 2, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:compact',
      sessionID, sessionSequence: 2, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID: targetSegmentID, runID: targetRunID,
      payload: {
        schema_version: 'investigation-session-manifest/v1',
        session: { ...fullManifest.session, sequence: 2 },
        segments: { [targetSegmentID]: { ...fullManifest.segments[targetSegmentID], status: 'paused' } },
        attempts: { [targetRunID]: { ...fullManifest.attempts[targetRunID], status: 'paused_at_boundary' } },
        transitions: {}, occurrences: {}, accepted_commands: { current: { sequence: 2 } },
      },
    }],
  });

  const manifest = model.snapshot().manifest;
  const topologyDocument = graph('topology', [node('one')], []);
  topologyDocument.groups = [{
    id: 'segment', kind: 'session-segment', parent_node_id: '', frame_id: '',
    segment_id: 'segment-1', segment_status: 'active',
  }];
  assert.equal(
    sessionGraphTopologyKey({
      ...topologyDocument,
      groups: topologyDocument.groups.map((group) => ({ ...group, segment_status: 'paused' })),
    }),
    sessionGraphTopologyKey(topologyDocument),
  );
  assert.equal(manifest.session.sequence, 2);
  assert.equal(manifest.segments[sourceSegmentID].status, 'handed_off');
  assert.equal(manifest.segments[targetSegmentID].status, 'paused');
  assert.equal(manifest.attempts[sourceRunID].status, 'completed');
  assert.equal(manifest.attempts[targetRunID].status, 'paused_at_boundary');
  assert.equal(manifest.transitions.route.status, 'committed');
  assert.deepEqual(manifest.occurrences.historical, { status: 'completed' });

  model.applyGroup({
    sequence: 3, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:sparse',
      sessionID, sessionSequence: 3, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      payload: {
        schema_version: 'investigation-session-manifest/v1',
        session: { ...fullManifest.session, sequence: 3 },
        segments: { [targetSegmentID]: { ...fullManifest.segments[targetSegmentID], status: 'active' } },
        attempts: {}, transitions: {}, occurrences: {}, accepted_commands: {},
      },
    }],
  });
  assert.equal(model.snapshot().manifest.segments[targetSegmentID].graph_revision, 1);
  assert.equal(model.snapshot().manifest.segments[targetSegmentID].graph_hash, `sha256:${'a'.repeat(64)}`);

  assert.throws(() => model.applyGroup({
    sequence: 4, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:rewrite',
      sessionID, sessionSequence: 4, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      payload: {
        schema_version: 'investigation-session-manifest/v1',
        session: { ...fullManifest.session, sequence: 4 },
        segments: {}, attempts: {}, transitions: {
          route: { ...fullManifest.transitions.route, reason_summary: 'Rewritten history' },
        },
        occurrences: { historical: { status: 'failed' } }, accepted_commands: {},
      },
    }],
  }), /immutable (transition|occurrence)/i);
  assert.equal(model.snapshot().sequence, 3);
  assert.equal(model.snapshot().manifest.transitions.route.reason_summary, undefined);
  assert.deepEqual(model.snapshot().manifest.occurrences.historical, { status: 'completed' });
});

test('session graph model rejects conflicting graph revisions atomically and retains prior revisions', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const firstGraph = graph('entry', [node('first')], []);
  const secondGraph = graph('entry', [node('first'), node('second')], [
    { id: 'next', source: 'first', target: 'second' },
  ]);
  const manifest = {
    schema_version: 'investigation-session-manifest/v1',
    session: {
      session_id: sessionID, status: 'active', root_segment_id: segmentID,
      active_segment_id: segmentID, active_run_id: runID, sequence: 1,
    },
    segments: {
      [segmentID]: {
        segment_id: segmentID, ordinal: 1, runbook_id: 'entry', runbook_name: 'Entry',
        status: 'active', attempt_run_ids: [runID], graph_revision: 1,
        graph_hash: `sha256:${'a'.repeat(64)}`,
        executable_revision: 1, plan_hash: `sha256:${'d'.repeat(64)}`,
        executable_snapshot_hash: `sha256:${'e'.repeat(64)}`,
      },
    },
    attempts: {
      [runID]: { run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'running' },
    },
    transitions: {}, occurrences: {}, accepted_commands: {},
  };
  const model = new SessionGraphModel(sessionID);
  const mismatched = new SessionGraphModel(sessionID);
  assert.throws(() => mismatched.applyGroup({
    sequence: 1, writerEpoch: 1, handshake: false,
    graphs: [{
      segmentID, runID, revision: 1, wholeBlobHash: `sha256:${'b'.repeat(64)}`, document: firstGraph,
    }],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:mismatch',
      sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID, runID, payload: manifest,
    }],
  }), /(graph.*manifest|graph and executable binding)/i);
  assert.equal(mismatched.snapshot().sequence, 0);

  model.applyGroup({
    sequence: 1, writerEpoch: 1, handshake: false,
    graphs: [{
      segmentID, runID, revision: 1, wholeBlobHash: `sha256:${'a'.repeat(64)}`, document: firstGraph,
    }],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:first',
      sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID, runID, payload: manifest,
    }],
  });

  assert.throws(() => model.applyGroup({
    sequence: 2, writerEpoch: 1, handshake: false,
    graphs: [{
      segmentID, runID, revision: 1, wholeBlobHash: `sha256:${'b'.repeat(64)}`, document: secondGraph,
    }],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'run.event', frameID: 'sha256:event',
      sessionID, sessionSequence: 2, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID, runID,
      payload: {
        event_id: 'event', run_id: runID, runbook_id: 'entry', sequence: 1,
        timestamp: '2026-09-02T00:00:00Z', kind: 'step/completed',
        payload: { node_id: 'first', step_id: 'first' },
      },
    }],
  }), /(graph.*(manifest|conflict)|graph and executable binding)/i);
  assert.equal(model.snapshot().sequence, 1);
  assert.equal(model.snapshot().runtimeNodes[sessionGraphNodeID(sessionID, segmentID, 'first')], undefined);
  assert.equal(model.snapshot().document.nodes.filter((candidate) => candidate.data.synthetic !== true).length, 1);

  assert.throws(() => model.applyGroup({
    sequence: 2, writerEpoch: 1, handshake: false,
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:missing-executable',
      sessionID, sessionSequence: 2, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID, runID,
      payload: {
        ...manifest,
        session: { ...manifest.session, sequence: 2 },
        segments: { [segmentID]: {
          ...manifest.segments[segmentID], graph_revision: 2, graph_hash: `sha256:${'c'.repeat(64)}`,
          executable_revision: undefined, plan_hash: undefined, executable_snapshot_hash: undefined,
        } },
      },
    }],
    graphs: [{
      segmentID, runID, revision: 2, wholeBlobHash: `sha256:${'c'.repeat(64)}`, document: secondGraph,
    }],
  }), /executable binding/i);
  assert.equal(model.snapshot().sequence, 1);

  model.applyGroup({
    sequence: 2, writerEpoch: 1, handshake: false,
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:revision-two',
      sessionID, sessionSequence: 2, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID, runID,
      payload: {
        ...manifest,
        session: { ...manifest.session, sequence: 2 },
        segments: {
          [segmentID]: {
            ...manifest.segments[segmentID], graph_revision: 2,
            graph_hash: `sha256:${'c'.repeat(64)}`,
            executable_revision: 2, plan_hash: `sha256:${'f'.repeat(64)}`,
            executable_snapshot_hash: `sha256:${'1'.repeat(64)}`,
          },
        },
      },
    }],
    graphs: [{
      segmentID, runID, revision: 2, wholeBlobHash: `sha256:${'c'.repeat(64)}`, document: secondGraph,
    }],
  });
  assert.equal(model.graphRevision(segmentID, 1).nodes.length, 1);
  assert.equal(model.graphRevision(segmentID, 2).nodes.length, 2);
  const historicalNode = model.graphRevisionNode(segmentID, 1, 'first');
  assert.equal(historicalNode.id, sessionGraphNodeID(sessionID, segmentID, 'first'));
  assert.equal(historicalNode.data.graph_revision, 1);
  assert.equal(historicalNode.data.graph_hash, `sha256:${'a'.repeat(64)}`);
  assert.equal(historicalNode.data.plan_hash, `sha256:${'d'.repeat(64)}`);
  assert.equal(historicalNode.data.executable_snapshot_hash, `sha256:${'e'.repeat(64)}`);
  assert.deepEqual(historicalNode.data.available_graph_revisions, [1, 2]);
  const latestNode = model.graphRevisionNode(segmentID, 2, 'first');
  assert.equal(latestNode.data.plan_hash, `sha256:${'f'.repeat(64)}`);
  assert.equal(model.snapshot().document.nodes.filter((candidate) => candidate.data.synthetic !== true).length, 2);

  const thirdGraph = graph('entry', [node('first'), node('second'), node('third')], [
    { id: 'next', source: 'first', target: 'second' },
    { id: 'last', source: 'second', target: 'third' },
  ]);
  const thirdSegment = {
    ...manifest.segments[segmentID], graph_revision: 3, graph_hash: `sha256:${'2'.repeat(64)}`,
    executable_revision: 3, plan_hash: `sha256:${'3'.repeat(64)}`,
    executable_snapshot_hash: `sha256:${'4'.repeat(64)}`,
  };
  const deferred = model.applyGroup({
    sequence: 3, writerEpoch: 1, handshake: false, graphs: [],
    frames: [
      {
        version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:revision-three',
        sessionID, sessionSequence: 3, sequenceIndex: 0, sequenceCount: 2, writerEpoch: 1,
        segmentID, runID,
        payload: {
          ...manifest,
          session: { ...manifest.session, sequence: 3 },
          segments: { [segmentID]: thirdSegment },
        },
      },
      {
        version: 'yawr.session-stdio/v1', type: 'segment.graph.available', frameID: 'sha256:available-three',
        sessionID, sessionSequence: 3, sequenceIndex: 1, sequenceCount: 2, writerEpoch: 1,
        segmentID, runID,
        payload: { graphRevision: 3, wholeBlobHash: thirdSegment.graph_hash },
      },
    ],
  });
  assert.deepEqual(deferred.segmentGraphRevisions[segmentID], [1, 2, 3]);
  assert.deepEqual(deferred.unloadedSegmentIDs, [segmentID]);
  assert.equal(model.graphRevision(segmentID, 2).nodes.length, 2);
  const loaded = model.loadGraphRevision(thirdSegment, 3, thirdSegment.graph_hash, thirdGraph);
  assert.deepEqual(loaded.unloadedSegmentIDs, []);
  assert.equal(model.graphRevisionNode(segmentID, 3, 'third').data.plan_hash, thirdSegment.plan_hash);
});

test('cacheless replay accepts the current graph after contiguous deferred revisions', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const model = new SessionGraphModel(sessionID);
  const revision = (value) => ({
    segment_id: segmentID, ordinal: 1, runbook_id: 'entry', runbook_name: 'Entry',
    status: 'active', attempt_run_ids: [runID], graph_revision: value,
    graph_hash: `sha256:${String(value).repeat(64)}`,
    executable_revision: value, plan_hash: `sha256:${String(value + 3).repeat(64)}`,
    executable_snapshot_hash: `sha256:${String(value + 6).repeat(64)}`,
  });
  const snapshot = (sequence, segment) => ({
    version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: `sha256:snapshot-${sequence}`,
    sessionID, sessionSequence: sequence, sequenceIndex: 0, sequenceCount: 2, writerEpoch: 1,
    segmentID, runID,
    payload: {
      schema_version: 'investigation-session-manifest/v1',
      session: {
        session_id: sessionID, status: 'active', root_segment_id: segmentID,
        active_segment_id: segmentID, active_run_id: runID, sequence,
      },
      segments: { [segmentID]: segment },
      attempts: { [runID]: { run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'running' } },
      transitions: {}, occurrences: {}, accepted_commands: {},
    },
  });
  for (const sequence of [1, 2]) {
    const segment = revision(sequence);
    model.applyGroup({
      sequence, writerEpoch: 1, handshake: false, graphs: [],
      frames: [snapshot(sequence, segment), {
        version: 'yawr.session-stdio/v1', type: 'segment.graph.available', frameID: `sha256:available-${sequence}`,
        sessionID, sessionSequence: sequence, sequenceIndex: 1, sequenceCount: 2, writerEpoch: 1,
        segmentID, runID,
        payload: { graphRevision: sequence, wholeBlobHash: segment.graph_hash },
      }],
    });
  }
  const current = revision(3);
  const currentGraph = graph('entry', [node('current')], []);
  const state = model.applyGroup({
    sequence: 3, writerEpoch: 1, handshake: false,
    frames: [{ ...snapshot(3, current), sequenceCount: 1 }],
    graphs: [{ segmentID, runID, revision: 3, wholeBlobHash: current.graph_hash, document: currentGraph }],
  });
  assert.equal(state.sequence, 3);
  assert.deepEqual(state.segmentGraphRevisions[segmentID], [1, 2, 3]);
  assert.deepEqual(state.unloadedSegmentIDs, []);
  assert.equal(state.document.nodes.some((candidate) => candidate.data.original_node_id === 'current'), true);

  const conflictingRevisionOne = {
    ...revision(1),
    graph_hash: `sha256:${'f'.repeat(64)}`,
  };
  assert.throws(() => model.loadGraphRevision(
    conflictingRevisionOne,
    1,
    conflictingRevisionOne.graph_hash,
    graph('entry', [node('forged')], []),
  ), /advertised graph revision digest conflict/i);
  assert.equal(model.snapshot().segmentGraphAvailability[segmentID]['1'].graph_hash, revision(1).graph_hash);
  assert.equal(model.graphRevision(segmentID, 1), undefined);
});

test('restore rejects gapped or conflicting graph availability metadata', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const segment = (revision, graphHash) => ({
    segment_id: segmentID, ordinal: 1, runbook_id: 'entry', runbook_name: 'Entry',
    status: 'active', attempt_run_ids: [runID], graph_revision: revision, graph_hash: graphHash,
    executable_revision: revision, plan_hash: `sha256:${String(revision + 3).repeat(64)}`,
    executable_snapshot_hash: `sha256:${String(revision + 6).repeat(64)}`,
  });
  const revisionTwo = segment(2, `sha256:${'2'.repeat(64)}`);
  const manifest = {
    schema_version: 'investigation-session-manifest/v1',
    session: {
      session_id: sessionID, status: 'active', root_segment_id: segmentID,
      active_segment_id: segmentID, active_run_id: runID, sequence: 3,
    },
    segments: { [segmentID]: revisionTwo },
    attempts: { [runID]: { run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'running' } },
    transitions: {}, occurrences: {}, accepted_commands: {},
  };
  assert.throws(() => new SessionGraphModel(sessionID).restore(
    3,
    manifest,
    {},
    {},
    undefined,
    undefined,
    {},
    {},
    { [segmentID]: { 2: revisionTwo } },
  ), /availability.*contiguous/i);

  const revisionOne = segment(1, `sha256:${'1'.repeat(64)}`);
  const revisionNinetyNine = segment(99, `sha256:${'9'.repeat(64)}`);
  assert.throws(() => new SessionGraphModel(sessionID).restore(
    3,
    manifest,
    {},
    {},
    undefined,
    undefined,
    {},
    {},
    { [segmentID]: { 1: revisionOne, 2: revisionTwo, 99: revisionNinetyNine } },
  ), /availability.*manifest head/i);

  const fractionalRevision = segment(1.5, `sha256:${'8'.repeat(64)}`);
  assert.throws(() => new SessionGraphModel(sessionID).restore(
    3,
    manifest,
    {},
    {},
    undefined,
    undefined,
    {},
    {},
    { [segmentID]: { 1: revisionOne, 1.5: fractionalRevision, 2: revisionTwo } },
  ), /revisions must be positive integers/i);

  const noHeadManifest = {
    ...manifest,
    segments: { [segmentID]: {
      ...revisionTwo,
      graph_revision: undefined,
      graph_hash: undefined,
      executable_revision: undefined,
      plan_hash: undefined,
      executable_snapshot_hash: undefined,
    } },
  };
  assert.throws(() => new SessionGraphModel(sessionID).restore(
    3,
    noHeadManifest,
    {},
    {},
    undefined,
    undefined,
    {},
    {},
    { [segmentID]: { 99: revisionNinetyNine } },
  ), /availability.*revision head/i);

  const revisionOneGraph = graph('entry', [node('one')], []);
  assert.throws(() => new SessionGraphModel(sessionID).restore(
    3,
    manifest,
    {},
    {},
    undefined,
    undefined,
    { [segmentID]: { 1: {
      revision: 1,
      wholeBlobHash: revisionOne.graph_hash,
      document: revisionOneGraph,
      segmentSnapshot: revisionOne,
    } } },
    {},
    { [segmentID]: { 1: { ...revisionOne, graph_hash: `sha256:${'f'.repeat(64)}` }, 2: revisionTwo } },
  ), /availability.*loaded history/i);
});

test('committed transition cannot substitute a target after preparation', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const sourceSegmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const targetSegmentID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const sourceRunID = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd';
  const targetRunID = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee';
  const model = new SessionGraphModel(sessionID);
  model.applyGroup({
    sequence: 1, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:snapshot',
      sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      payload: {
        schema_version: 'investigation-session-manifest/v1',
        session: {
          session_id: sessionID, status: 'active', root_segment_id: sourceSegmentID,
          active_segment_id: sourceSegmentID, active_run_id: sourceRunID, sequence: 1,
        },
        segments: { [sourceSegmentID]: {
          segment_id: sourceSegmentID, ordinal: 1, runbook_id: 'source', runbook_name: 'Source',
          status: 'active', attempt_run_ids: [sourceRunID],
        } },
        attempts: { [sourceRunID]: {
          run_id: sourceRunID, segment_id: sourceSegmentID, ordinal: 1, mode: 'real', status: 'handoff_pending',
        } },
        transitions: {}, occurrences: {}, accepted_commands: {},
      },
    }],
  });
  const transition = {
    transition_id: 'route', status: 'prepared', source_segment_id: sourceSegmentID,
    source_occurrence: { run_id: sourceRunID, qualified_node_id: 'handoff', step: 'handoff' },
    target_segment_id: targetSegmentID, target_run_id: targetRunID, target_runbook_id: 'target',
    target_graph_hash: `sha256:${'a'.repeat(64)}`,
  };
  const preparedSegment = {
    segment_id: targetSegmentID, ordinal: 2,
    runbook_id: 'target', runbook_name: 'Target', status: 'prepared',
    attempt_run_ids: [targetRunID], graph_revision: 1, graph_hash: `sha256:${'a'.repeat(64)}`,
  };
  const preparedAttempt = {
    run_id: targetRunID, segment_id: targetSegmentID, ordinal: 1, mode: 'real', status: 'starting',
  };
  model.applyGroup({
    sequence: 2, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'transition.prepared', frameID: 'sha256:prepared',
      sessionID, sessionSequence: 2, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      payload: { transition, target_segment: preparedSegment, target_attempt: preparedAttempt },
    }],
  });
  assert.throws(() => model.applyGroup({
    sequence: 3, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'transition.committed', frameID: 'sha256:committed',
      sessionID, sessionSequence: 3, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      payload: {
        transition_id: 'route',
        target_segment: preparedSegment,
        target_attempt: { ...preparedAttempt, mode: 'replay' },
      },
    }],
  }), /prepared transition target/i);
  assert.equal(model.snapshot().sequence, 2);
  assert.equal(model.snapshot().manifest.session.active_segment_id, sourceSegmentID);
});

test('session graph model preserves ordered saved and live occurrences for one definition node', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const savedRunID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const liveRunID = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd';
  const manifest = {
    schema_version: 'investigation-session-manifest/v1',
    session: {
      session_id: sessionID, status: 'active', root_segment_id: segmentID,
      active_segment_id: segmentID, active_run_id: liveRunID, sequence: 1,
    },
    segments: {
      [segmentID]: {
        segment_id: segmentID, ordinal: 1, runbook_id: 'entry', runbook_name: 'Entry',
        status: 'active', attempt_run_ids: [savedRunID, liveRunID],
      },
    },
    attempts: {
      [savedRunID]: { run_id: savedRunID, segment_id: segmentID, ordinal: 1, mode: 'replay', status: 'completed' },
      [liveRunID]: { run_id: liveRunID, segment_id: segmentID, ordinal: 2, mode: 'real', status: 'running' },
    },
    transitions: {}, occurrences: {}, accepted_commands: {},
  };
  const model = new SessionGraphModel(sessionID);
  model.applyGroup({
    sequence: 1, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:snapshot',
      sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID, runID: liveRunID, payload: manifest,
    }],
  });
  const eventFrame = (sequence, runID, invocation, status, output) => ({
    version: 'yawr.session-stdio/v1', type: 'run.event', frameID: `sha256:event-${sequence}`,
    sessionID, sessionSequence: sequence, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
    segmentID, runID,
    payload: {
      event_id: `event-${sequence}`, run_id: runID, runbook_id: 'entry', sequence,
      timestamp: `2026-09-02T00:00:0${sequence}Z`, kind: `step/${status}`,
      payload: {
        node_id: 'inspect', step_id: 'inspect', phase: 'execute', invocation,
        retry_attempt: 1, occurrence_sequence: invocation, output,
      },
    },
  });
  model.applyGroup({
    sequence: 2, writerEpoch: 1, handshake: false, graphs: [],
    frames: [eventFrame(2, savedRunID, 1, 'completed', { source: 'saved' })],
  });
  model.applyGroup({
    sequence: 3, writerEpoch: 1, handshake: false, graphs: [],
    frames: [eventFrame(3, liveRunID, 2, 'failed', { source: 'live' })],
  });

  const runtime = model.snapshot().runtimeNodes[sessionGraphNodeID(sessionID, segmentID, 'inspect')];
  assert.equal(runtime.status, 'failed');
  assert.equal(runtime.output.source, 'live');
  assert.deepEqual(runtime.occurrences.map((occurrence) => ({
    runID: occurrence.runID,
    invocation: occurrence.invocation,
    source: occurrence.executionSource,
    status: occurrence.status,
    output: occurrence.output.source,
  })), [
    { runID: savedRunID, invocation: 1, source: 'saved', status: 'completed', output: 'saved' },
    { runID: liveRunID, invocation: 2, source: 'live', status: 'failed', output: 'live' },
  ]);

  assert.throws(() => model.applyGroup({
    sequence: 4, writerEpoch: 1, handshake: false, graphs: [],
    frames: [eventFrame(4, liveRunID, 2, 'completed', { source: 'rewritten' })],
  }), /terminal occurrence/i);
  assert.equal(model.snapshot().sequence, 3);
  assert.equal(
    model.snapshot().runtimeNodes[sessionGraphNodeID(sessionID, segmentID, 'inspect')].output.source,
    'live',
  );

  assert.throws(() => model.applyGroup({
    sequence: 4, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'segment.finished', frameID: 'sha256:segment-rewrite',
      sessionID, sessionSequence: 4, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID, runID: liveRunID,
      payload: { ...manifest.segments[segmentID], runbook_name: 'Rewritten entry', status: 'completed' },
    }],
  }), /immutable segment/i);
  assert.equal(model.snapshot().manifest.segments[segmentID].runbook_name, 'Entry');
});

test('session.finished terminalizes paused topology and clears pending interaction state', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const manifest = {
    schema_version: 'investigation-session-manifest/v1',
    session: {
      session_id: sessionID, status: 'paused', root_segment_id: segmentID,
      active_segment_id: segmentID, active_run_id: runID, sequence: 1,
    },
    segments: {
      [segmentID]: {
        segment_id: segmentID, ordinal: 1, runbook_id: 'entry', runbook_name: 'Entry',
        status: 'paused', attempt_run_ids: [runID],
      },
    },
    attempts: {
      [runID]: { run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'paused_at_boundary' },
    },
    transitions: {}, occurrences: {}, accepted_commands: {},
  };
  const model = new SessionGraphModel(sessionID);
  model.applyGroup({
    sequence: 1, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:snapshot',
      sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID, runID, payload: manifest,
    }],
  });
  model.applyGroup({
    sequence: 2, writerEpoch: 1, handshake: false, graphs: [],
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.finished', frameID: 'sha256:finished',
      sessionID, sessionSequence: 2, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      payload: { status: 'escalated' },
    }],
  });

  const state = model.snapshot();
  assert.equal(state.sessionStatus, 'escalated');
  assert.equal(state.runStatus, 'escalated');
  assert.equal(state.activeRunID, undefined);
  assert.equal(state.manifest.attempts[runID].status, 'cancelled');
  assert.equal(state.manifest.segments[segmentID].status, 'cancelled');
  assert.equal(state.pending, undefined);
});

test('interleaved parallel events retain independent historical arm edges', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const definition = graph('parallel', [
    { ...node('parallel', 'parallel'), data: { ...node('parallel', 'parallel').data, group_id: '' } },
    { ...node('a1'), data: { ...node('a1').data, group_id: 'arm-a' } },
    { ...node('a2'), data: { ...node('a2').data, group_id: 'arm-a' } },
    { ...node('b1'), data: { ...node('b1').data, group_id: 'arm-b' } },
    { ...node('b2'), data: { ...node('b2').data, group_id: 'arm-b' } },
  ], [
    { id: 'p-a', source: 'parallel', target: 'a1' },
    { id: 'a-next', source: 'a1', target: 'a2' },
    { id: 'p-b', source: 'parallel', target: 'b1' },
    { id: 'b-next', source: 'b1', target: 'b2' },
  ]);
  definition.groups = [
    { id: 'arm-a', kind: 'parallel-branch', parent_node_id: 'parallel', frame_id: 'root' },
    { id: 'arm-b', kind: 'parallel-branch', parent_node_id: 'parallel', frame_id: 'root' },
  ];
  const encoded = JSON.stringify(definition);
  const graphHash = `sha256:${require('node:crypto').createHash('sha256').update(encoded).digest('hex')}`;
  const segmentSnapshot = {
    segment_id: segmentID, ordinal: 1, runbook_id: 'parallel', runbook_name: 'Parallel',
    status: 'completed', attempt_run_ids: [runID], graph_revision: 1, graph_hash: graphHash,
    executable_revision: 1, plan_hash: `sha256:${'a'.repeat(64)}`,
    executable_snapshot_hash: `sha256:${'b'.repeat(64)}`,
  };
  const model = new SessionGraphModel(sessionID);
  model.applyGroup({
    sequence: 1, writerEpoch: 1, handshake: false, graphs: [],
    frames: [
      {
        version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:snapshot',
        sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 2, writerEpoch: 1,
        segmentID, runID,
        payload: {
          schema_version: 'investigation-session-manifest/v1',
          session: { session_id: sessionID, status: 'paused', root_segment_id: segmentID, sequence: 1 },
          segments: { [segmentID]: segmentSnapshot },
          attempts: { [runID]: {
            run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'completed',
          } },
          transitions: {}, occurrences: {}, accepted_commands: {},
        },
      },
      {
        version: 'yawr.session-stdio/v1', type: 'segment.graph.available', frameID: 'sha256:available',
        sessionID, sessionSequence: 1, sequenceIndex: 1, sequenceCount: 2, writerEpoch: 1,
        segmentID, runID, payload: { graphRevision: 1, wholeBlobHash: graphHash },
      },
    ],
  });
  const started = (sequence, nodeID) => ({
    version: 'yawr.session-stdio/v1', type: 'run.event', frameID: `sha256:${nodeID}`,
    sessionID, sessionSequence: sequence, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
    segmentID, runID,
    payload: {
      event_id: `event-${nodeID}`, run_id: runID, runbook_id: 'parallel', sequence,
      timestamp: `2026-09-02T00:00:0${sequence}Z`, kind: 'step/started',
      payload: { node_id: nodeID, step_id: nodeID },
    },
  });
  for (const [sequence, nodeID] of [[2, 'parallel'], [3, 'a1'], [4, 'b1'], [5, 'a2'], [6, 'b2']]) {
    model.applyGroup({ sequence, writerEpoch: 1, handshake: false, graphs: [], frames: [started(sequence, nodeID)] });
  }
  const state = model.loadGraphRevision(segmentSnapshot, 1, graphHash, definition);
  const a1 = sessionGraphNodeID(sessionID, segmentID, 'a1');
  const a2 = sessionGraphNodeID(sessionID, segmentID, 'a2');
  const b1 = sessionGraphNodeID(sessionID, segmentID, 'b1');
  const b2 = sessionGraphNodeID(sessionID, segmentID, 'b2');
  assert.equal(state.runtimeNodes[a2].occurrences[0].predecessorNodeID, a1);
  assert.equal(state.runtimeNodes[b2].occurrences[0].predecessorNodeID, b1);
  const filtered = sessionRouteDocument(state.document, state.runtimeNodes);
  assert.ok(filtered.edges.some((edge) => edge.source === a1 && edge.target === a2));
  assert.ok(filtered.edges.some((edge) => edge.source === b1 && edge.target === b2));
});

test('hydrating an old revision preserves the latest live lane cursor', () => {
  const sessionID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  const segmentID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const runID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
  const makeGraph = (runbookID, groupID, nodeIDs) => {
    const document = graph(runbookID, nodeIDs.map((id) => ({
      ...node(id), data: { ...node(id).data, group_id: groupID },
    })), nodeIDs.slice(1).map((id, index) => ({ id: `${nodeIDs[index]}-${id}`, source: nodeIDs[index], target: id })));
    document.groups = [{ id: groupID, kind: 'parallel-branch', parent_node_id: 'parallel', frame_id: 'root' }];
    return document;
  };
  const revisionOneGraph = makeGraph('entry', 'arm-a', ['old1', 'old2']);
  const revisionTwoGraph = makeGraph('entry', 'arm-a', ['a1', 'a2', 'a3']);
  const segment = (revision, graphHash) => ({
    segment_id: segmentID, ordinal: 1, runbook_id: 'entry', runbook_name: 'Entry',
    status: 'active', attempt_run_ids: [runID], graph_revision: revision, graph_hash: graphHash,
    executable_revision: revision, plan_hash: `sha256:${String(revision + 2).repeat(64)}`,
    executable_snapshot_hash: `sha256:${String(revision + 5).repeat(64)}`,
  });
  const revisionOne = segment(1, `sha256:${'1'.repeat(64)}`);
  const revisionTwo = segment(2, `sha256:${'2'.repeat(64)}`);
  const model = new SessionGraphModel(sessionID);
  const manifest = (sequence, segmentSnapshot) => ({
    schema_version: 'investigation-session-manifest/v1',
    session: {
      session_id: sessionID, status: 'active', root_segment_id: segmentID,
      active_segment_id: segmentID, active_run_id: runID, sequence,
    },
    segments: { [segmentID]: segmentSnapshot },
    attempts: { [runID]: { run_id: runID, segment_id: segmentID, ordinal: 1, mode: 'real', status: 'running' } },
    transitions: {}, occurrences: {}, accepted_commands: {},
  });
  model.applyGroup({
    sequence: 1, writerEpoch: 1, handshake: false, graphs: [],
    frames: [
      {
        version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:snapshot-one',
        sessionID, sessionSequence: 1, sequenceIndex: 0, sequenceCount: 2, writerEpoch: 1,
        segmentID, runID, payload: manifest(1, revisionOne),
      },
      {
        version: 'yawr.session-stdio/v1', type: 'segment.graph.available', frameID: 'sha256:available-one',
        sessionID, sessionSequence: 1, sequenceIndex: 1, sequenceCount: 2, writerEpoch: 1,
        segmentID, runID, payload: { graphRevision: 1, wholeBlobHash: revisionOne.graph_hash },
      },
    ],
  });
  const started = (sequence, nodeID) => ({
    version: 'yawr.session-stdio/v1', type: 'run.event', frameID: `sha256:${nodeID}-${sequence}`,
    sessionID, sessionSequence: sequence, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
    segmentID, runID,
    payload: {
      event_id: `event-${nodeID}-${sequence}`, run_id: runID, runbook_id: 'entry', sequence,
      timestamp: `2026-09-02T00:00:${String(sequence).padStart(2, '0')}Z`, kind: 'step/started',
      payload: { node_id: nodeID, step_id: nodeID },
    },
  });
  model.applyGroup({ sequence: 2, writerEpoch: 1, handshake: false, graphs: [], frames: [started(2, 'old1')] });
  model.applyGroup({ sequence: 3, writerEpoch: 1, handshake: false, graphs: [], frames: [started(3, 'old2')] });
  model.applyGroup({
    sequence: 4, writerEpoch: 1, handshake: false,
    frames: [{
      version: 'yawr.session-stdio/v1', type: 'session.snapshot', frameID: 'sha256:snapshot-two',
      sessionID, sessionSequence: 4, sequenceIndex: 0, sequenceCount: 1, writerEpoch: 1,
      segmentID, runID, payload: manifest(4, revisionTwo),
    }],
    graphs: [{ segmentID, runID, revision: 2, wholeBlobHash: revisionTwo.graph_hash, document: revisionTwoGraph }],
  });
  model.applyGroup({ sequence: 5, writerEpoch: 1, handshake: false, graphs: [], frames: [started(5, 'a1')] });
  model.applyGroup({ sequence: 6, writerEpoch: 1, handshake: false, graphs: [], frames: [started(6, 'a2')] });
  model.loadGraphRevision(revisionOne, 1, revisionOne.graph_hash, revisionOneGraph);
  const state = model.applyGroup({
    sequence: 7, writerEpoch: 1, handshake: false, graphs: [], frames: [started(7, 'a3')],
  });
  const a2 = sessionGraphNodeID(sessionID, segmentID, 'a2');
  const a3 = sessionGraphNodeID(sessionID, segmentID, 'a3');
  assert.equal(state.runtimeNodes[a3].occurrences[0].predecessorNodeID, a2);
});