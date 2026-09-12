import { sanitizePresentationPayload, terminalPresentation, type RuntimePresentation } from './presentationProjection';
import { compareOccurrences, directOccurrenceID, validProgressIdentity, producerStepKind, normalizeRuntimeStatuses, isExecutionEnded } from './executionProgress';
import { sanitizeDisplayPayload, hasDisplayPayload } from './displayPresentation';
import { retainDisplayObservation, reconcileRuntimeDisplayState, type DisplayObservations } from './displayObservations';
import { validateSessionPresentationBinding } from './directGraphPreview';
import type {
  GraphDocument,
  GraphEdge,
  GraphFrame,
  GraphGroup,
  GraphNode,
} from './directGraphPreview';
import type { SessionFrameGroup, SessionProtocolFrame } from './sessionStdioClient';

export interface SessionRecord {
  session_id: string;
  status: string;
  root_segment_id: string;
  active_segment_id?: string;
  active_run_id?: string;
  sequence: number;
}

export interface SessionSegment {
  segment_id: string;
  ordinal: number;
  runbook_id: string;
  runbook_name: string;
  status: string;
  entry_selector?: { step?: string };
  attempt_run_ids: string[];
  graph_revision?: number;
  graph_hash?: string;
  executable_revision?: number;
  plan_hash?: string;
  executable_snapshot_hash?: string;
  catalog_digest?: string;
  package_lock_digest?: string;
  profile_digest?: string;
}

export interface SessionAttempt {
  run_id: string;
  segment_id: string;
  ordinal: number;
  mode: string;
  status: string;
}

export interface SessionTransition {
  transition_id: string;
  status: string;
  source_segment_id: string;
  source_occurrence: {
    run_id: string;
    qualified_node_id: string;
    step: string;
  };
  target_segment_id: string;
  target_run_id: string;
  target_runbook_id: string;
  reason_code?: string;
  reason_summary?: string;
}

export interface SessionPreparedTransitionTarget {
  segment: SessionSegment;
  attempt: SessionAttempt;
}

export interface SessionManifest {
  schema_version: 'investigation-session-manifest/v1';
  session: SessionRecord;
  segments: Record<string, SessionSegment>;
  attempts: Record<string, SessionAttempt>;
  transitions: Record<string, SessionTransition>;
  occurrences: Record<string, unknown>;
  accepted_commands: Record<string, unknown>;
}

export interface ComposeSessionGraphRequest {
  sessionID: string;
  manifest: SessionManifest;
  segmentGraphs: ReadonlyMap<string, GraphDocument>;
  segmentGraphRevisions?: ReadonlyMap<string, readonly number[]>;
}

export interface SessionRuntimeValueState extends RuntimePresentation {
  directDisplaySnapshot?: string;
  displayObservations?: DisplayObservations;
  status: string;
  error?: string;
  durationMs?: number;
  attempt?: number;
  startedAt?: string;
  finishedAt?: string;
  lastActivityAt?: string;
  stepKind?: string;
  output?: Record<string, unknown>;
  captures?: Record<string, unknown>;
  evidence?: unknown;
  logs?: Array<{ stream: string; line: string }>;
}

export interface SessionRuntimeOccurrence extends SessionRuntimeValueState {
  occurrenceID: string;
  runID: string;
  segmentID: string;
  qualifiedNodeID: string;
  phase?: string;
  invocation?: number;
  retryAttempt?: number;
  progressIdentity?: string;
  occurrenceSequence?: number;
  executionSource: 'saved' | 'live';
  startedEventSequence?: number;
  finishedEventSequence?: number;
  predecessorNodeID?: string;
  executionLane?: string;
  graphRevision?: number;
  frameID?: string;
  frameStepIndex?: number;
  dispatchOccurrenceID?: string;
}

export interface SessionRuntimeNodeState extends SessionRuntimeValueState {
  occurrenceID?: string;
  occurrences?: SessionRuntimeOccurrence[];
}

export interface SessionPendingInteraction extends Record<string, unknown> {
  type: 'pending';
  turnID: string;
  runID: string;
  stepID: string;
  nodeID: string;
  kind: string;
}

export interface SessionGraphViewState {
  sessionID: string;
  sequence: number;
  sessionStatus: string;
  runStatus: string;
  activeSegmentID?: string;
  activeRunID?: string;
  document?: GraphDocument;
  runtimeNodes: Readonly<Record<string, SessionRuntimeNodeState>>;
  executionNodeID?: string;
  pending?: SessionPendingInteraction;
  manifest?: SessionManifest;
  segmentGraphRevisions: Readonly<Record<string, readonly number[]>>;
  preparedTransitionTargets: Readonly<Record<string, SessionPreparedTransitionTarget>>;
  unloadedSegmentIDs: readonly string[];
  segmentGraphAvailability: Readonly<Record<string, Readonly<Record<string, SessionSegment>>>>;
}

function scopedID(
  sessionID: string,
  segmentID: string,
  kind: 'node' | 'edge' | 'frame' | 'group' | 'transition' | 'entry' | 'entry-edge',
  localID: string,
): string {
  return [
    `session:${encodeURIComponent(sessionID)}`,
    `segment:${encodeURIComponent(segmentID)}`,
    `${kind}:${encodeURIComponent(localID)}`,
  ].join('/');
}

export function sessionGraphNodeID(sessionID: string, segmentID: string, nodeID: string): string {
  return scopedID(sessionID, segmentID, 'node', nodeID);
}

export function sessionGraphEntryNodeID(sessionID: string, segmentID: string): string {
  return scopedID(sessionID, segmentID, 'entry', '$entry');
}

function sessionSegmentGroupID(sessionID: string, segmentID: string): string {
  return `session:${encodeURIComponent(sessionID)}/segment:${encodeURIComponent(segmentID)}/segment-group`;
}

function segmentAttempt(manifest: SessionManifest, segment: SessionSegment): SessionAttempt | undefined {
  const attempts = segment.attempt_run_ids
    .map((runID) => manifest.attempts[runID])
    .filter((attempt): attempt is SessionAttempt => attempt !== undefined)
    .sort((left, right) => right.ordinal - left.ordinal || right.run_id.localeCompare(left.run_id));
  if (manifest.session.active_run_id) {
    const active = attempts.find((attempt) => attempt.run_id === manifest.session.active_run_id);
    if (active) return active;
  }
  return attempts[0];
}

function namespaceFrame(sessionID: string, segmentID: string, frame: GraphFrame): GraphFrame {
  return {
    ...frame,
    id: scopedID(sessionID, segmentID, 'frame', frame.id),
    ...(frame.parent_include_node_id
      ? { parent_include_node_id: sessionGraphNodeID(sessionID, segmentID, frame.parent_include_node_id) }
      : {}),
  };
}

function namespaceGroup(sessionID: string, segmentID: string, group: GraphGroup): GraphGroup {
  return {
    ...group,
    id: scopedID(sessionID, segmentID, 'group', group.id),
    parent_node_id: group.parent_node_id
      ? sessionGraphNodeID(sessionID, segmentID, group.parent_node_id)
      : '',
    frame_id: scopedID(sessionID, segmentID, 'frame', group.frame_id),
  };
}

function namespaceNode(
  sessionID: string,
  segment: SessionSegment,
  attempt: SessionAttempt | undefined,
  node: GraphNode,
  graphRevisions: readonly number[],
  incomingTransitions: readonly Record<string, unknown>[],
  outgoingTransitions: readonly Record<string, unknown>[],
  graphRevision = segment.graph_revision,
  graphHash = segment.graph_hash,
  displaySnapshotDigest?: string,
): GraphNode {
  const id = sessionGraphNodeID(sessionID, segment.segment_id, node.id);
  const sourceGroupID = typeof node.data.group_id === 'string' ? node.data.group_id : '';
  const groupID = sourceGroupID
    ? scopedID(sessionID, segment.segment_id, 'group', sourceGroupID)
    : sessionSegmentGroupID(sessionID, segment.segment_id);
  const frameID = typeof node.data.frame_id === 'string' && node.data.frame_id
    ? scopedID(sessionID, segment.segment_id, 'frame', node.data.frame_id)
    : '';
  return {
    ...node,
    id,
    data: {
      ...node.data,
      id,
      original_node_id: node.id,
      session_id: sessionID,
      segment_id: segment.segment_id,
      segment_ordinal: segment.ordinal,
      segment_status: segment.status,
      run_id: attempt?.run_id,
      attempt_status: attempt?.status,
      execution_source: attempt?.mode === 'replay' || attempt?.mode === 'route-test' ? 'saved' : 'live',
      runbook_id: segment.runbook_id,
      runbook_name: segment.runbook_name,
      graph_revision: graphRevision,
      graph_hash: graphHash,
      executable_revision: segment.executable_revision,
      plan_hash: segment.plan_hash,
      executable_snapshot_hash: segment.executable_snapshot_hash,
      ...(displaySnapshotDigest ? { display_plan_snapshot_digest: displaySnapshotDigest } : {}),
      catalog_digest: segment.catalog_digest,
      package_lock_digest: segment.package_lock_digest,
      profile_digest: segment.profile_digest,
      available_graph_revisions: [...graphRevisions],
      incoming_transitions: incomingTransitions,
      outgoing_transitions: outgoingTransitions,
      graph_loaded: true,
      group_id: groupID,
      frame_id: frameID,
    },
    parentNode: node.parentNode
      ? scopedID(sessionID, segment.segment_id, 'group', node.parentNode)
      : sessionSegmentGroupID(sessionID, segment.segment_id),
    extent: 'parent',
  };
}

function namespaceEdge(sessionID: string, segmentID: string, edge: GraphEdge): GraphEdge {
  return {
    ...edge,
    id: scopedID(sessionID, segmentID, 'edge', edge.id),
    source: sessionGraphNodeID(sessionID, segmentID, edge.source),
    target: sessionGraphNodeID(sessionID, segmentID, edge.target),
    ...(edge.runtimeNodeID
      ? { runtimeNodeID: sessionGraphNodeID(sessionID, segmentID, edge.runtimeNodeID) }
      : {}),
    ...(edge.runtimeFallbackNodeID
      ? { runtimeFallbackNodeID: sessionGraphNodeID(sessionID, segmentID, edge.runtimeFallbackNodeID) }
      : {}),
  };
}

function entryNodeIDs(segment: SessionSegment, graph: GraphDocument): string[] {
  const selector = segment.entry_selector?.step;
  if (selector && selector !== '$entry') {
    const exact = graph.nodes.filter((node) => node.id === selector);
    const selected = exact.length > 0
      ? exact
      : graph.nodes.filter((node) => node.data.step_id === selector);
    if (selected.length !== 1) {
      throw new Error(`segment ${JSON.stringify(segment.segment_id)} entry selector is unresolved or ambiguous`);
    }
    return [selected[0].id];
  }
  const incoming = new Set(graph.edges.map((edge) => edge.target));
  const roots = graph.nodes
    .filter((node) => !incoming.has(node.id) && node.data.synthetic !== true)
    .map((node) => node.id)
    .sort();
  if (roots.length === 0 && graph.nodes.some((node) => node.data.synthetic !== true)) {
    throw new Error(`segment ${JSON.stringify(segment.segment_id)} has no structural entry node`);
  }
  return roots;
}

function sourceNodeID(transition: SessionTransition, graph: GraphDocument): string {
  const qualifiedID = transition.source_occurrence.qualified_node_id;
  const exact = graph.nodes.filter((node) => node.id === qualifiedID);
  if (exact.length === 1) return exact[0].id;
  throw new Error(`committed transition source ${JSON.stringify(qualifiedID)} is unresolved`);
}

export function composeSessionGraph(request: ComposeSessionGraphRequest): GraphDocument {
  const { sessionID, manifest, segmentGraphs, segmentGraphRevisions } = request;
  if (!sessionID || manifest.session.session_id !== sessionID) {
    throw new Error('session graph manifest does not match the requested session');
  }

  const nodes: GraphNode[] = [];
  const edges: GraphEdge[] = [];
  const frames: GraphFrame[] = [];
  const groups: GraphGroup[] = [];
  const segments = Object.values(manifest.segments)
    .sort((left, right) => left.ordinal - right.ordinal || left.segment_id.localeCompare(right.segment_id));

  for (const segment of segments) {
    if (segment.segment_id === '') continue;
    const graph = segmentGraphs.get(segment.segment_id);
    const attempt = segmentAttempt(manifest, segment);
    const graphRevisions = segmentGraphRevisions?.get(segment.segment_id) ??
      (segment.graph_revision ? [segment.graph_revision] : []);
    const incomingTransitions = transitionProvenance(manifest, segment.segment_id, 'incoming');
    const outgoingTransitions = transitionProvenance(manifest, segment.segment_id, 'outgoing');
    const rootFrame = graph?.frames
      .filter((frame) => frame.depth === 0)
      .sort((left, right) => left.id.localeCompare(right.id))[0] ?? graph?.frames[0];
    groups.push({
      id: sessionSegmentGroupID(sessionID, segment.segment_id),
      kind: 'session-segment',
      parent_node_id: '',
      frame_id: rootFrame ? scopedID(sessionID, segment.segment_id, 'frame', rootFrame.id) : '',
      label: `${segment.ordinal} | ${segment.runbook_name || segment.runbook_id}`,
      index: segment.ordinal,
      segment_id: segment.segment_id,
      segment_status: segment.status,
      graph_loaded: graph !== undefined,
      ...(attempt ? { run_id: attempt.run_id } : {}),
    });
    const entryID = sessionGraphEntryNodeID(sessionID, segment.segment_id);
    nodes.push({
      id: entryID,
      type: 'sessionEntry',
      data: {
        id: entryID,
        kind: 'session-entry',
        title: `${segment.runbook_name || segment.runbook_id} entry`,
        synthetic: true,
        session_id: sessionID,
        segment_id: segment.segment_id,
        segment_ordinal: segment.ordinal,
        segment_status: segment.status,
        run_id: attempt?.run_id,
        runbook_id: segment.runbook_id,
        runbook_name: segment.runbook_name,
        graph_revision: segment.graph_revision,
        graph_hash: segment.graph_hash,
        available_graph_revisions: [...graphRevisions],
        incoming_transitions: incomingTransitions,
        outgoing_transitions: outgoingTransitions,
        graph_loaded: graph !== undefined,
        group_id: sessionSegmentGroupID(sessionID, segment.segment_id),
        frame_id: rootFrame ? scopedID(sessionID, segment.segment_id, 'frame', rootFrame.id) : '',
      },
      parentNode: sessionSegmentGroupID(sessionID, segment.segment_id),
      extent: 'parent',
      position: { x: 0, y: 0 },
    });
    for (const rootID of graph ? entryNodeIDs(segment, graph) : []) {
      edges.push({
        id: scopedID(sessionID, segment.segment_id, 'entry-edge', rootID),
        source: entryID,
        target: sessionGraphNodeID(sessionID, segment.segment_id, rootID),
        type: 'session-entry',
      });
    }
    if (graph) {
      nodes.push(...graph.nodes.map((node) => namespaceNode(
        sessionID,
        segment,
        attempt,
        node,
        graphRevisions,
        incomingTransitions,
        outgoingTransitions,
        segment.graph_revision,
        segment.graph_hash,
        graph.display_plan_snapshot_digest,
      )));
      edges.push(...graph.edges.map((edge) => namespaceEdge(sessionID, segment.segment_id, edge)));
      frames.push(...graph.frames.map((frame) => namespaceFrame(sessionID, segment.segment_id, frame)));
      groups.push(...graph.groups.map((group) => namespaceGroup(sessionID, segment.segment_id, group)));
    }
  }

  const transitions = Object.values(manifest.transitions ?? {})
    .filter((transition) => transition.status === 'committed')
    .sort((left, right) => left.transition_id.localeCompare(right.transition_id));
  for (const transition of transitions) {
    const sourceGraph = segmentGraphs.get(transition.source_segment_id);
    const targetSegment = manifest.segments[transition.target_segment_id];
    if (!targetSegment) continue;
    const source = sourceGraph
      ? sourceNodeID(transition, sourceGraph)
      : transition.source_occurrence.qualified_node_id;
    const sourceCompositeID = sessionGraphNodeID(sessionID, transition.source_segment_id, source);
    if (!nodes.some((node) => node.id === sourceCompositeID)) {
      const sourceSegment = manifest.segments[transition.source_segment_id];
      if (!sourceSegment) throw new Error('committed transition source segment is unavailable');
      nodes.push({
        id: sourceCompositeID,
        type: 'yawrStep',
        data: {
          id: sourceCompositeID,
          original_node_id: source,
          step_id: transition.source_occurrence.step,
          kind: 'session-history-placeholder',
          title: transition.source_occurrence.step,
          session_id: sessionID,
          segment_id: sourceSegment.segment_id,
          segment_ordinal: sourceSegment.ordinal,
          segment_status: sourceSegment.status,
          graph_loaded: false,
          group_id: sessionSegmentGroupID(sessionID, sourceSegment.segment_id),
          incoming_transitions: transitionProvenance(manifest, sourceSegment.segment_id, 'incoming'),
          outgoing_transitions: transitionProvenance(manifest, sourceSegment.segment_id, 'outgoing'),
        },
        parentNode: sessionSegmentGroupID(sessionID, sourceSegment.segment_id),
        extent: 'parent',
        position: { x: 0, y: 0 },
      });
    }
    const target = sessionGraphEntryNodeID(sessionID, targetSegment.segment_id);
    edges.push({
      id: scopedID(sessionID, transition.target_segment_id, 'transition', transition.transition_id),
      source: sourceCompositeID,
      target,
      type: 'session-transition',
      label: transition.reason_summary || transition.reason_code || 'Handoff',
    });
  }

  return {
    schema_version: '1',
    hash: `session:${sessionID}:${manifest.session.sequence}`,
    runbook: {
      id: sessionID,
      name: `Investigation session`,
    },
    nodes,
    edges,
    frames,
    groups,
  };
}

export function sessionGraphTopologyKey(document: GraphDocument): string {
  return JSON.stringify({
    nodes: document.nodes.map((node) => ({
      id: node.id,
      kind: node.data.kind ?? '',
      groupID: node.data.group_id ?? '',
      frameID: node.data.frame_id ?? '',
      parentNode: node.parentNode ?? '',
    })),
    edges: document.edges.map((edge) => ({
      id: edge.id,
      source: edge.source,
      target: edge.target,
      type: edge.type ?? '',
      routeKind: edge.routeKind ?? '',
      runtimeNodeID: edge.runtimeNodeID ?? '',
      runtimeArmIndex: edge.runtimeArmIndex ?? -1,
    })),
    groups: document.groups.map((group) => ({
      id: group.id,
      kind: group.kind,
      parentNodeID: group.parent_node_id,
      frameID: group.frame_id,
    })),
    frames: document.frames.map((frame) => ({
      id: frame.id,
      parentIncludeNodeID: frame.parent_include_node_id ?? '',
      depth: frame.depth,
    })),
  });
}

export class SessionGraphModel {
  private manifest: SessionManifest | undefined;
  private readonly segmentGraphs = new Map<string, GraphDocument>();
  private readonly graphRevisions = new Map<string, number>();
  private readonly graphHistory = new Map<string, Map<number, {
    wholeBlobHash: string;
    document: GraphDocument;
    segmentSnapshot: SessionSegment;
  }>>();
  private readonly graphAvailability = new Map<string, Map<number, SessionSegment>>();
  private composedDocument: GraphDocument | undefined;
  private documentDirty = true;
  private runtimeNodes: Record<string, SessionRuntimeNodeState> = {};
  private currentSequence = 0;
  private currentExecutionNodeID: string | undefined;
  private currentPending: SessionPendingInteraction | undefined;
  private readonly lastStartedNodeByLane = new Map<string, { nodeID: string; eventSequence: number }>();
  private readonly preparedTransitionTargets = new Map<string, SessionPreparedTransitionTarget>();

  constructor(private readonly sessionID: string) {
    if (!sessionID) throw new Error('session graph model requires a session ID');
  }

  restore(
    sequence: number,
    manifest: SessionManifest,
    segmentGraphs: Readonly<Record<string, {
      revision: number;
      wholeBlobHash: string;
      document: GraphDocument;
      segmentSnapshot: SessionSegment;
    }>>,
    runtimeNodes: Readonly<Record<string, SessionRuntimeNodeState>> = {},
    executionNodeID?: string,
    pending?: SessionPendingInteraction,
    graphHistory?: Readonly<Record<string, Readonly<Record<string, {
      revision: number;
      wholeBlobHash: string;
      document: GraphDocument;
      segmentSnapshot: SessionSegment;
    }>>>>,
    preparedTransitionTargets: Readonly<Record<string, SessionPreparedTransitionTarget>> = {},
    graphAvailability: Readonly<Record<string, Readonly<Record<string, SessionSegment>>>> = {},
  ): SessionGraphViewState {
    if (!Number.isSafeInteger(sequence) || sequence < 0 || manifest.session.session_id !== this.sessionID ||
        sequence > manifest.session.sequence) {
      throw new Error('session graph cache cursor is invalid');
    }
    this.manifest = cloneManifest(manifest);
    this.currentSequence = sequence;
    this.segmentGraphs.clear();
    this.graphRevisions.clear();
    this.graphHistory.clear();
    this.graphAvailability.clear();
    this.preparedTransitionTargets.clear();
    this.composedDocument = undefined;
    this.documentDirty = true;
    for (const [segmentID, cached] of Object.entries(segmentGraphs)) {
      const segmentSnapshot = parseSessionSegment(cached.segmentSnapshot);
      if (!this.manifest.segments[segmentID] || !Number.isSafeInteger(cached.revision) || cached.revision < 1 ||
          segmentSnapshot.segment_id !== segmentID || segmentSnapshot.graph_revision !== cached.revision ||
          segmentSnapshot.graph_hash !== cached.wholeBlobHash) {
        throw new Error('session graph cache contains an invalid segment revision');
      }
      validateSessionSegmentGraphBinding(segmentSnapshot, cached.revision, cached.wholeBlobHash);
      validateSessionPresentationBinding(cached.document, segmentSnapshot.executable_snapshot_hash);
      this.segmentGraphs.set(segmentID, cached.document);
      this.graphRevisions.set(segmentID, cached.revision);
      this.graphHistory.set(segmentID, new Map([[
        cached.revision,
        {
          wholeBlobHash: cached.wholeBlobHash,
          document: cached.document,
          segmentSnapshot: { ...segmentSnapshot, attempt_run_ids: [...segmentSnapshot.attempt_run_ids] },
        },
      ]]));
      this.graphAvailability.set(segmentID, new Map([[cached.revision, segmentSnapshot]]));
    }
    for (const [segmentID, revisions] of Object.entries(graphHistory ?? {})) {
      if (!this.manifest.segments[segmentID]) throw new Error('session graph history contains an unknown segment');
      const restored = new Map<number, {
        wholeBlobHash: string;
        document: GraphDocument;
        segmentSnapshot: SessionSegment;
      }>();
      for (const [revisionKey, cached] of Object.entries(revisions)) {
        const segmentSnapshot = parseSessionSegment(cached.segmentSnapshot);
        if (revisionKey !== String(cached.revision) ||
            cached.revision > (this.manifest.segments[segmentID].graph_revision ?? 0) ||
            segmentSnapshot.segment_id !== segmentID || segmentSnapshot.graph_revision !== cached.revision ||
            segmentSnapshot.graph_hash !== cached.wholeBlobHash) {
          throw new Error('session graph history contains an invalid revision');
        }
        validateSessionSegmentGraphBinding(segmentSnapshot, cached.revision, cached.wholeBlobHash);
        validateSessionPresentationBinding(cached.document, segmentSnapshot.executable_snapshot_hash);
        restored.set(cached.revision, {
          wholeBlobHash: cached.wholeBlobHash,
          document: cached.document,
          segmentSnapshot: { ...segmentSnapshot, attempt_run_ids: [...segmentSnapshot.attempt_run_ids] },
        });
      }
      const latestRevision = this.graphRevisions.get(segmentID);
      if (latestRevision !== undefined) {
        const latest = restored.get(latestRevision);
        const expected = segmentGraphs[segmentID];
        if (!latest || !expected || latest.wholeBlobHash !== expected.wholeBlobHash) {
          throw new Error('session graph history does not contain the latest loaded revision');
        }
      }
      this.graphHistory.set(segmentID, restored);
    }
    for (const [segmentID, revisions] of Object.entries(graphAvailability)) {
      if (!this.manifest.segments[segmentID]) throw new Error('graph availability contains an unknown segment');
      const available = this.graphAvailability.get(segmentID) ?? new Map<number, SessionSegment>();
      for (const [revisionKey, raw] of Object.entries(revisions)) {
        const segment = parseSessionSegment(raw);
        if (segment.segment_id !== segmentID || revisionKey !== String(segment.graph_revision) ||
            !segment.graph_revision || !segment.graph_hash) {
          throw new Error('graph availability metadata is invalid');
        }
        available.set(segment.graph_revision, segment);
      }
      this.graphAvailability.set(segmentID, available);
    }
    this.validateRestoredGraphAvailability();
    for (const [transitionID, target] of Object.entries(preparedTransitionTargets)) {
      const transition = this.manifest.transitions[transitionID];
      if (!transition) throw new Error('prepared transition cache has no manifest transition');
      const segment = parseSessionSegment(target.segment);
      const attempt = parseSessionAttempt(target.attempt);
      validatePreparedTransitionTarget(transition, segment, attempt);
      this.preparedTransitionTargets.set(transitionID, { segment, attempt });
    }
    this.runtimeNodes = normalizeRuntimeStatuses(JSON.parse(JSON.stringify(runtimeNodes)) as Record<string, SessionRuntimeNodeState>);
    this.lastStartedNodeByLane.clear();
    for (const [nodeID, state] of Object.entries(this.runtimeNodes)) {
      for (const occurrence of state.occurrences ?? []) {
        if (occurrence.startedEventSequence === undefined) continue;
        const lane = occurrence.executionLane ?? this.executionLane(occurrence.segmentID, occurrence.qualifiedNodeID);
        const laneKey = `${occurrence.runID}\u0000${lane}`;
        const previous = this.lastStartedNodeByLane.get(laneKey);
        if (!previous || occurrence.startedEventSequence > previous.eventSequence) {
          this.lastStartedNodeByLane.set(laneKey, {
            nodeID,
            eventSequence: occurrence.startedEventSequence,
          });
        }
      }
    }
    this.currentExecutionNodeID = executionNodeID;
    this.currentPending = pending ? { ...pending } : undefined;
    return this.snapshot();
  }

  applyGroup(group: SessionFrameGroup): SessionGraphViewState {
    if (group.sequence < this.currentSequence || (!group.handshake && group.sequence !== this.currentSequence + 1)) {
      throw new Error('session graph group is not contiguous');
    }
    const rollback = this.rollbackPoint();
    try {
      const previousTopology = this.manifest ? compositeManifestFingerprint(this.manifest) : '';
      for (const frame of group.frames) this.applyFrame(frame);
      this.validateGraphManifestTransaction(group, rollback.manifest);
      const nextTopology = this.manifest ? compositeManifestFingerprint(this.manifest) : '';
      if (previousTopology !== nextTopology) this.documentDirty = true;
      for (const update of group.graphs) this.applyGraphUpdate(update);
      if (!group.handshake) this.currentSequence = group.sequence;
      if (this.manifest && this.manifest.session.sequence < group.sequence) {
        this.manifest.session.sequence = group.sequence;
      }
      return this.snapshot();
    } catch (error) {
      this.restoreRollbackPoint(rollback);
      throw error;
    }
  }

  graphRevision(segmentID: string, revision: number): GraphDocument | undefined {
    return this.graphHistory.get(segmentID)?.get(revision)?.document;
  }

  loadGraphRevision(
    segmentSnapshot: SessionSegment,
    revision: number,
    wholeBlobHash: string,
    document: GraphDocument,
  ): SessionGraphViewState {
    const manifest = this.requireManifest();
    const current = manifest.segments[segmentSnapshot.segment_id];
    if (!current || segmentSnapshot.graph_revision !== revision || segmentSnapshot.graph_hash !== wholeBlobHash ||
        revision > (current.graph_revision ?? 0)) {
      throw new Error('lazy graph revision does not match the session manifest');
    }
    validateSessionSegmentGraphBinding(segmentSnapshot, revision, wholeBlobHash);
    validateSessionPresentationBinding(document, segmentSnapshot.executable_snapshot_hash);
    validateSegmentRecord(segmentSnapshot, current);
    const advertised = this.graphAvailability.get(segmentSnapshot.segment_id)?.get(revision);
    if (advertised && advertised.graph_hash !== wholeBlobHash) {
      throw new Error('advertised graph revision digest conflict');
    }
    if (advertised && !sameGraphBinding(advertised, segmentSnapshot)) {
      throw new Error('advertised graph revision metadata conflict');
    }
    const history = this.graphHistory.get(segmentSnapshot.segment_id) ?? new Map();
    const existing = history.get(revision);
    if (existing && existing.wholeBlobHash !== wholeBlobHash) {
      throw new Error('lazy graph revision digest conflict');
    }
    history.set(revision, {
      wholeBlobHash,
      document,
      segmentSnapshot: { ...segmentSnapshot, attempt_run_ids: [...segmentSnapshot.attempt_run_ids] },
    });
    this.graphHistory.set(segmentSnapshot.segment_id, history);
    this.recomputeSegmentExecutionLanes(segmentSnapshot.segment_id);
    const available = this.graphAvailability.get(segmentSnapshot.segment_id) ?? new Map<number, SessionSegment>();
    available.set(revision, { ...segmentSnapshot, attempt_run_ids: [...segmentSnapshot.attempt_run_ids] });
    this.graphAvailability.set(segmentSnapshot.segment_id, available);
    if (revision === current.graph_revision) {
      this.segmentGraphs.set(segmentSnapshot.segment_id, document);
      this.graphRevisions.set(segmentSnapshot.segment_id, revision);
      this.documentDirty = true;
    }
    return this.snapshot();
  }

  graphRevisionNode(segmentID: string, revision: number, originalNodeID: string): GraphNode | undefined {
    const manifest = this.requireManifest();
    const segment = manifest.segments[segmentID];
    const archived = this.graphHistory.get(segmentID)?.get(revision);
    const source = archived?.document.nodes.find((node) => node.id === originalNodeID);
    if (!segment || !archived || !source) return undefined;
    return namespaceNode(
      this.sessionID,
      archived.segmentSnapshot,
      segmentAttempt(manifest, segment),
      source,
      [...(this.graphHistory.get(segmentID)?.keys() ?? [])].sort((left, right) => left - right),
      transitionProvenance(manifest, segmentID, 'incoming'),
      transitionProvenance(manifest, segmentID, 'outgoing'),
      revision,
      archived.wholeBlobHash,
      archived.document.display_plan_snapshot_digest,
    );
  }

  snapshot(): SessionGraphViewState {
    const manifest = this.manifest;
    const activeRunID = manifest?.session.active_run_id;
    const activeAttempt = activeRunID ? manifest?.attempts[activeRunID] : undefined;
    const pending = this.currentPending && (!activeRunID || this.currentPending.runID === activeRunID) &&
      !isExecutionEnded(activeAttempt?.status ?? '') && !isExecutionEnded(manifest?.session.status ?? '')
      ? this.currentPending : undefined;
    const segmentGraphRevisions = Object.fromEntries(Object.values(manifest?.segments ?? {}).map((segment) => {
      const revisions = new Set(this.graphAvailability.get(segment.segment_id)?.keys() ?? []);
      for (const revision of this.graphHistory.get(segment.segment_id)?.keys() ?? []) revisions.add(revision);
      if (segment.graph_revision) revisions.add(segment.graph_revision);
      return [segment.segment_id, [...revisions].sort((left, right) => left - right)];
    }));
    if (manifest && this.segmentGraphs.size > 0 && (this.documentDirty || !this.composedDocument)) {
      this.composedDocument = composeSessionGraph({
        sessionID: this.sessionID,
        manifest,
        segmentGraphs: this.segmentGraphs,
        segmentGraphRevisions: new Map(Object.entries(segmentGraphRevisions)),
      });
      this.documentDirty = false;
    }
    return {
      sessionID: this.sessionID,
      sequence: this.currentSequence,
      sessionStatus: manifest?.session.status ?? 'loading',
      runStatus: pending ? 'waiting' : activeAttempt?.status ?? manifest?.session.status ?? 'loading',
      ...(manifest?.session.active_segment_id ? { activeSegmentID: manifest.session.active_segment_id } : {}),
      ...(activeRunID ? { activeRunID } : {}),
      ...(this.composedDocument ? { document: this.composedDocument } : {}),
      runtimeNodes: { ...this.runtimeNodes },
      segmentGraphRevisions,
      preparedTransitionTargets: Object.fromEntries([...this.preparedTransitionTargets].map(([transitionID, target]) => [
        transitionID,
        clonePreparedTransitionTarget(target),
      ])),
      unloadedSegmentIDs: manifest
        ? Object.keys(manifest.segments).filter((segmentID) => !this.segmentGraphs.has(segmentID))
        : [],
      segmentGraphAvailability: Object.fromEntries([...this.graphAvailability].map(([segmentID, revisions]) => [
        segmentID,
        Object.fromEntries([...revisions].map(([revision, segment]) => [
          String(revision),
          { ...segment, attempt_run_ids: [...segment.attempt_run_ids] },
        ])),
      ])),
      ...(this.currentExecutionNodeID ? { executionNodeID: this.currentExecutionNodeID } : {}),
      ...(pending ? { pending: { ...pending } } : {}),
      ...(manifest ? { manifest: cloneManifest(manifest) } : {}),
    };
  }

  private applyFrame(frame: SessionProtocolFrame): void {
    if (frame.sessionID !== this.sessionID) throw new Error('session graph frame belongs to a different session');
    switch (frame.type) {
      case 'session.snapshot':
        this.manifest = mergeSessionManifest(
          this.manifest,
          parseSessionManifest(frame.payload, this.sessionID),
        );
        return;
      case 'segment.added':
      case 'segment.finished':
        this.applySegment(frame.payload);
        return;
      case 'segment.graph.available':
        this.applyGraphAvailable(frame);
        return;
      case 'attempt.started':
      case 'attempt.paused':
      case 'attempt.finished':
        this.applyAttempt(frame);
        return;
      case 'transition.prepared':
        this.applyPreparedTransition(frame.payload);
        return;
      case 'transition.committed':
        this.applyCommittedTransition(frame.payload);
        return;
      case 'session.finished':
        this.applyFinishedSession(frame.payload);
        return;
      case 'run.event':
        this.applyRunEvent(frame);
        return;
      case 'interaction.pending':
        this.currentPending = this.parsePendingInteraction(frame);
        this.currentExecutionNodeID = this.currentPending.nodeID;
        return;
      case 'interaction.resolved': {
        const resolved = objectValue(frame.payload, 'resolved interaction');
        const turnID = stringValue(resolved.turn_id ?? resolved.turnID, 'resolved interaction turn');
        if (this.currentPending?.turnID === turnID) this.currentPending = undefined;
        return;
      }
      default:
        return;
    }
  }

  private applyGraphUpdate(update: SessionFrameGroup['graphs'][number]): void {
    const segment = this.requireManifest().segments[update.segmentID];
    if (!segment) throw new Error('session graph update belongs to an unknown segment');
    validateSessionSegmentGraphBinding(segment, update.revision, update.wholeBlobHash);
    validateSessionPresentationBinding(update.document, segment.executable_snapshot_hash);
    const previousRevision = this.graphRevisions.get(update.segmentID) ?? 0;
    const knownRevision = this.highestKnownGraphRevision(update.segmentID);
    const history = this.graphHistory.get(update.segmentID) ?? new Map();
    const existing = history.get(update.revision);
    if (existing && existing.wholeBlobHash !== update.wholeBlobHash) {
      throw new Error('session graph revision digest conflict');
    }
    if (update.revision < previousRevision) {
      if (!existing) throw new Error('session graph revision moved backwards');
      return;
    }
    if (!existing && update.revision > knownRevision + 1) {
      throw new Error('session graph revision is not contiguous');
    }
    if (!existing) {
      history.set(update.revision, {
        wholeBlobHash: update.wholeBlobHash,
        document: update.document,
        segmentSnapshot: { ...segment, attempt_run_ids: [...segment.attempt_run_ids] },
      });
      this.graphHistory.set(update.segmentID, history);
    }
    this.recomputeSegmentExecutionLanes(update.segmentID);
    const available = this.graphAvailability.get(update.segmentID) ?? new Map<number, SessionSegment>();
    available.set(update.revision, { ...segment, attempt_run_ids: [...segment.attempt_run_ids] });
    this.graphAvailability.set(update.segmentID, available);
    if (update.revision === previousRevision) return;
    this.segmentGraphs.set(update.segmentID, update.document);
    this.graphRevisions.set(update.segmentID, update.revision);
    this.documentDirty = true;
    this.manifest!.segments[update.segmentID] = {
      ...segment,
      graph_revision: update.revision,
      graph_hash: update.wholeBlobHash,
    };
  }

  private applyGraphAvailable(frame: SessionProtocolFrame): void {
    if (!frame.segmentID) throw new Error('deferred graph frame has no segment ID');
    const payload = objectValue(frame.payload, 'deferred graph revision');
    const revision = integerValue(payload.graphRevision, 'deferred graph revision');
    const wholeBlobHash = stringValue(payload.wholeBlobHash, 'deferred graph digest');
    if (!/^sha256:[0-9a-f]{64}$/.test(wholeBlobHash)) throw new Error('deferred graph digest is invalid');
    const segment = this.requireManifest().segments[frame.segmentID];
    if (!segment) {
      throw new Error('deferred graph revision does not match its manifest');
    }
    validateSessionSegmentGraphBinding(segment, revision, wholeBlobHash);
    const available = this.graphAvailability.get(frame.segmentID) ?? new Map<number, SessionSegment>();
    const existing = available.get(revision);
    if (existing && existing.graph_hash !== wholeBlobHash) {
      throw new Error('deferred graph revision digest conflict');
    }
    if (!existing && revision > this.highestKnownGraphRevision(frame.segmentID) + 1) {
      throw new Error('deferred graph revision is not contiguous');
    }
    available.set(revision, { ...segment, attempt_run_ids: [...segment.attempt_run_ids] });
    this.graphAvailability.set(frame.segmentID, available);
    this.documentDirty = true;
    const loadedRevision = this.graphRevisions.get(frame.segmentID);
    if (loadedRevision !== undefined && loadedRevision !== revision) {
      this.segmentGraphs.delete(frame.segmentID);
      this.graphRevisions.delete(frame.segmentID);
      this.documentDirty = true;
    }
  }

  private validateGraphManifestTransaction(
    group: SessionFrameGroup,
    previousManifest: SessionManifest | undefined,
  ): void {
    if (!previousManifest || !this.manifest) return;
    const updates = new Map(group.graphs.map((update) => [update.segmentID, update]));
    const available = new Map(group.frames.flatMap((frame) => {
      if (frame.type !== 'segment.graph.available' || !frame.segmentID) return [];
      const payload = objectValue(frame.payload, 'deferred graph revision');
      return [[frame.segmentID, {
        revision: integerValue(payload.graphRevision, 'deferred graph revision'),
        wholeBlobHash: stringValue(payload.wholeBlobHash, 'deferred graph digest'),
      }] as const];
    }));
    const snapshots = group.frames.flatMap((frame) => (
      frame.type === 'session.snapshot' ? [parseSessionManifest(frame.payload, this.sessionID)] : []
    ));
    for (const [segmentID, segment] of Object.entries(this.manifest.segments)) {
      const previous = previousManifest.segments[segmentID];
      if (!previous || previous.graph_revision === segment.graph_revision && previous.graph_hash === segment.graph_hash) {
        continue;
      }
      const update = updates.get(segmentID);
      const deferred = available.get(segmentID);
      const snapshotBound = snapshots.some((snapshot) => {
        const bound = snapshot.segments[segmentID];
        if (!bound || bound.graph_revision !== segment.graph_revision || bound.graph_hash !== segment.graph_hash ||
            bound.executable_revision !== segment.executable_revision || bound.plan_hash !== segment.plan_hash ||
            bound.executable_snapshot_hash !== segment.executable_snapshot_hash) return false;
        try {
          validateSessionSegmentGraphBinding(bound, segment.graph_revision!, segment.graph_hash!);
          return true;
        } catch {
          return false;
        }
      });
      const transmittedGraphBound = update !== undefined && update.revision === segment.graph_revision &&
        update.wholeBlobHash === segment.graph_hash;
      const deferredGraphBound = deferred !== undefined && deferred.revision === segment.graph_revision &&
        deferred.wholeBlobHash === segment.graph_hash;
      const graphBound = transmittedGraphBound || deferredGraphBound;
      if (!graphBound || !snapshotBound) {
        throw new Error('session graph revision change lacks exact graph and executable binding');
      }
    }
  }

  private rollbackPoint() {
    return {
      manifest: this.manifest ? cloneManifest(this.manifest) : undefined,
      segmentGraphs: new Map(this.segmentGraphs),
      graphRevisions: new Map(this.graphRevisions),
      graphHistory: new Map([...this.graphHistory].map(([segmentID, revisions]) => (
        [segmentID, new Map(revisions)]
      ))),
      graphAvailability: new Map([...this.graphAvailability].map(([segmentID, revisions]) => (
        [segmentID, new Map(revisions)]
      ))),
      composedDocument: this.composedDocument,
      documentDirty: this.documentDirty,
      runtimeNodes: this.runtimeNodes,
      currentSequence: this.currentSequence,
      currentExecutionNodeID: this.currentExecutionNodeID,
      currentPending: this.currentPending,
      lastStartedNodeByLane: new Map(this.lastStartedNodeByLane),
      preparedTransitionTargets: new Map([...this.preparedTransitionTargets].map(([transitionID, target]) => [
        transitionID,
        clonePreparedTransitionTarget(target),
      ])),
    };
  }

  private restoreRollbackPoint(rollback: ReturnType<SessionGraphModel['rollbackPoint']>): void {
    this.manifest = rollback.manifest;
    replaceMap(this.segmentGraphs, rollback.segmentGraphs);
    replaceMap(this.graphRevisions, rollback.graphRevisions);
    replaceMap(this.graphHistory, rollback.graphHistory);
    replaceMap(this.graphAvailability, rollback.graphAvailability);
    this.composedDocument = rollback.composedDocument;
    this.documentDirty = rollback.documentDirty;
    this.runtimeNodes = rollback.runtimeNodes;
    this.currentSequence = rollback.currentSequence;
    this.currentExecutionNodeID = rollback.currentExecutionNodeID;
    this.currentPending = rollback.currentPending;
    replaceMap(this.lastStartedNodeByLane, rollback.lastStartedNodeByLane);
    replaceMap(this.preparedTransitionTargets, rollback.preparedTransitionTargets);
  }

  private requireManifest(): SessionManifest {
    if (!this.manifest) throw new Error('session topology frame arrived before a manifest snapshot');
    return this.manifest;
  }

  private applySegment(value: unknown): void {
    const manifest = this.requireManifest();
    const segment = parseSessionSegment(value);
    const existing = manifest.segments[segment.segment_id];
    manifest.segments[segment.segment_id] = existing
      ? mergeSegmentRecord(existing, segment)
      : segment;
  }

  private applyAttempt(frame: SessionProtocolFrame): void {
    const manifest = this.requireManifest();
    const payload = objectValue(frame.payload, 'session attempt');
    const runID = typeof payload.run_id === 'string' && payload.run_id ? payload.run_id : frame.runID;
    if (!runID) throw new Error('session attempt frame has no run ID');
    const existing = manifest.attempts[runID];
    if (typeof payload.segment_id === 'string') {
      const attempt = parseSessionAttempt(payload);
      manifest.attempts[runID] = existing ? mergeAttemptRecord(existing, attempt) : attempt;
    } else if (existing) {
      const status = typeof payload.status === 'string'
        ? payload.status
        : frame.type === 'attempt.paused'
          ? existing.status === 'handoff_pending' ? 'handoff_pending' : 'paused_at_boundary'
          : frame.type === 'attempt.started' ? 'running' : existing.status;
      manifest.attempts[runID] = { ...existing, status };
    }
    if (frame.type === 'attempt.started' && frame.segmentID) {
      manifest.session.active_segment_id = frame.segmentID;
      manifest.session.active_run_id = runID;
      manifest.session.status = 'active';
    }
  }

  private applyPreparedTransition(value: unknown): void {
    const manifest = this.requireManifest();
    const payload = objectValue(value, 'prepared transition');
    const transition = parseTransition(payload.transition);
    const targetSegment = parseSessionSegment(payload.target_segment);
    const targetAttempt = parseSessionAttempt(payload.target_attempt);
    validatePreparedTransitionTarget(transition, targetSegment, targetAttempt);
    const existing = manifest.transitions[transition.transition_id];
    if (existing) {
      validateTransitionRecord(existing, transition);
      if (existing.status !== transition.status) {
        throw new Error('session frame rewrites immutable transition status');
      }
    }
    const existingTarget = this.preparedTransitionTargets.get(transition.transition_id);
    if (existingTarget && (!jsonEqual(existingTarget.segment, targetSegment) ||
        !jsonEqual(existingTarget.attempt, targetAttempt))) {
      throw new Error('session frame rewrites immutable prepared transition target');
    }
    manifest.transitions[transition.transition_id] = transition;
    this.preparedTransitionTargets.set(transition.transition_id, {
      segment: targetSegment,
      attempt: targetAttempt,
    });
  }

  private applyCommittedTransition(value: unknown): void {
    const manifest = this.requireManifest();
    const payload = objectValue(value, 'committed transition');
    const transitionID = stringValue(payload.transition_id, 'committed transition id');
    const previous = manifest.transitions[transitionID];
    if (!previous) throw new Error('committed transition has no prepared history');
    const targetSegment = parseSessionSegment(payload.target_segment);
    const targetAttempt = parseSessionAttempt(payload.target_attempt);
    const preparedTarget = this.preparedTransitionTargets.get(transitionID);
    if (!preparedTarget || !jsonEqual(preparedTarget.segment, targetSegment) ||
        !jsonEqual(preparedTarget.attempt, targetAttempt)) {
      throw new Error('committed transition target does not match its prepared transition target');
    }
    if (previous.status !== 'prepared' && previous.status !== 'committed') {
      throw new Error('committed transition does not follow prepared history');
    }
    manifest.transitions[transitionID] = { ...previous, status: 'committed' };
    const sourceSegment = manifest.segments[previous.source_segment_id];
    if (!sourceSegment) throw new Error('committed transition source segment is unavailable');
    manifest.segments[sourceSegment.segment_id] = { ...sourceSegment, status: 'handed_off' };
    const sourceAttempt = manifest.attempts[previous.source_occurrence.run_id];
    if (!sourceAttempt) throw new Error('committed transition source attempt is unavailable');
    manifest.attempts[sourceAttempt.run_id] = { ...sourceAttempt, status: 'completed' };
    manifest.segments[targetSegment.segment_id] = manifest.segments[targetSegment.segment_id]
      ? mergeSegmentRecord(manifest.segments[targetSegment.segment_id], targetSegment)
      : targetSegment;
    manifest.attempts[targetAttempt.run_id] = manifest.attempts[targetAttempt.run_id]
      ? mergeAttemptRecord(manifest.attempts[targetAttempt.run_id], targetAttempt)
      : targetAttempt;
    manifest.session.active_segment_id = targetSegment.segment_id;
    manifest.session.active_run_id = targetAttempt.run_id;
    manifest.session.status = 'active';
  }

  private applyFinishedSession(value: unknown): void {
    const manifest = this.requireManifest();
    const payload = objectValue(value, 'finished session');
    const activeRunID = manifest.session.active_run_id;
    const activeAttempt = activeRunID ? manifest.attempts[activeRunID] : undefined;
    if (activeAttempt && ['waiting', 'paused_at_boundary', 'handoff_pending'].includes(activeAttempt.status)) {
      manifest.attempts[activeAttempt.run_id] = { ...activeAttempt, status: 'cancelled' };
      const segment = manifest.segments[activeAttempt.segment_id];
      if (segment) manifest.segments[segment.segment_id] = { ...segment, status: 'cancelled' };
    }
    manifest.session.status = stringValue(payload.status, 'finished session status');
    delete manifest.session.active_segment_id;
    delete manifest.session.active_run_id;
    this.currentPending = undefined;
  }

  private applyRunEvent(frame: SessionProtocolFrame): void {
    const event = objectValue(frame.payload, 'session run event');
    const runID = stringValue(event.run_id, 'run event run id');
    if (frame.runID && frame.runID !== runID) throw new Error('run event does not match its frame run ID');
    const payload = objectValue(event.payload ?? {}, 'run event payload');
    const localNodeID = runtimeEventNodeID(payload);
    if (!localNodeID) return;
    const segmentID = frame.segmentID || this.requireManifest().attempts[runID]?.segment_id;
    if (!segmentID) throw new Error('run event has no owning segment');
    const nodeID = sessionGraphNodeID(this.sessionID, segmentID, localNodeID);
    let previous = this.runtimeNodes[nodeID] ?? { status: 'pending' };
    const kind = stringValue(event.kind, 'run event kind');
    const phase = typeof payload.phase === 'string' && payload.phase ? payload.phase : undefined;
    const invocation = positiveInteger(payload.invocation) ? payload.invocation : undefined;
    const retryAttempt = positiveInteger(payload.retry_attempt)
      ? payload.retry_attempt
      : payload.retry_attempt === undefined && positiveInteger(payload.attempt) ? payload.attempt : undefined;
    const suppliedOccurrenceSequence = positiveInteger(payload.occurrence_sequence)
      ? payload.occurrence_sequence
      : undefined;
    const frameID = typeof payload.frame_id === 'string' ? payload.frame_id : undefined;
    const frameStepIndex = typeof payload.frame_step_index === 'number' && Number.isSafeInteger(payload.frame_step_index) && payload.frame_step_index >= 0
      ? payload.frame_step_index : undefined;
    const dispatchOccurrenceID = typeof payload.dispatch_occurrence_id === 'string' ? payload.dispatch_occurrence_id : undefined;
    const graphRevision = this.requireManifest().segments[segmentID]?.graph_revision;
    const executionLane = typeof payload.execution_lane === 'string' ? payload.execution_lane
      : this.executionLane(segmentID, localNodeID, graphRevision);
    const occurrences = [...(previous.occurrences ?? [])];
    const progressIdentity = directOccurrenceID(runID, localNodeID, payload);
    const occurrenceIndex = occurrences.findIndex((occurrence) => (
      validProgressIdentity(payload) && occurrence.progressIdentity === progressIdentity
    ));
    const existingOccurrence = occurrenceIndex >= 0 ? occurrences[occurrenceIndex] : undefined;
    const displayRevision = existingOccurrence?.graphRevision ?? graphRevision;
    const displayGraph = displayRevision === undefined ? undefined : this.graphRevision(segmentID, displayRevision);
    const expectedDisplaySnapshot = displayGraph?.display_plan_snapshot_digest ?? 'missing-binding';
    if ((kind === 'step/completed' || kind === 'step/failed') &&
        (hasDisplayPayload(payload) || previous.displayObservations)) {
      previous = { ...previous, displayObservations: retainDisplayObservation(previous.displayObservations ?? {},
        localNodeID, runID, { ...payload, execution_lane: executionLane }, expectedDisplaySnapshot) };
    }
    if (kind === 'step/output' && !existingOccurrence) {
      if (typeof payload.line !== 'string' || !payload.line) return;
      this.runtimeNodes = { ...this.runtimeNodes, [nodeID]: { ...previous,
        ...(typeof event.timestamp === 'string' ? { lastActivityAt: event.timestamp } : {}),
        logs: [...(previous.logs ?? []), {
          stream: typeof payload.stream === 'string' ? payload.stream : 'stdout', line: payload.line,
        }].slice(-50),
      } };
      return;
    }
    if (existingOccurrence && TERMINAL_OCCURRENCE_STATUSES.has(existingOccurrence.status) && kind !== 'step/output') {
      if (['step/started', 'step/resumed', 'step/delaying'].includes(kind)) return;
      if ((kind === 'step/completed' || kind === 'step/failed') &&
          (existingOccurrence.displayPresentation || existingOccurrence.displayPresentationDiagnostic ||
            'display_presentation' in payload || 'display_presentation_diagnostic' in payload)) {
        const display = terminalPresentation(payload, existingOccurrence, expectedDisplaySnapshot);
        const update = { output: plainRecord(payload.output),
          displayPresentation: display.displayPresentation,
          displayPresentationDiagnostic: display.displayPresentationDiagnostic };
        occurrences[occurrenceIndex] = { ...existingOccurrence, ...update };
        this.runtimeNodes = { ...this.runtimeNodes, [nodeID]: reconcileRuntimeDisplayState({
          ...previous, ...(previous.occurrenceID === existingOccurrence.occurrenceID ? update : {}), occurrences,
        }) as SessionRuntimeNodeState };
        return;
      }
      throw new Error('session run event rewrites a terminal occurrence');
    }
    const eventSequence = positiveInteger(event.sequence) ? event.sequence : 1;
    const laneKey = `${runID}\u0000${executionLane}`;
    const occurrenceSequence = suppliedOccurrenceSequence ?? existingOccurrence?.occurrenceSequence;
    const attempt = this.requireManifest().attempts[runID];
    const executionSource = attempt?.mode === 'replay' || attempt?.mode === 'route-test' ? 'saved' : 'live';
    const occurrenceID = existingOccurrence?.occurrenceID ??
      `${progressIdentity}${validProgressIdentity(payload) ? '' : `:uncertain:${eventSequence}`}`;
    let occurrence: SessionRuntimeOccurrence = existingOccurrence ?? {
      occurrenceID,
      progressIdentity,
      runID,
      segmentID,
      qualifiedNodeID: localNodeID,
      phase,
      invocation,
      retryAttempt,
      occurrenceSequence,
      executionSource,
      executionLane,
      graphRevision,
      ...(frameID === undefined ? {} : { frameID }),
      ...(frameStepIndex === undefined ? {} : { frameStepIndex }),
      ...(dispatchOccurrenceID === undefined ? {} : { dispatchOccurrenceID }),
      status: 'pending',
    };
    if (hasDisplayPayload(payload)) occurrence = { ...occurrence, directDisplaySnapshot: expectedDisplaySnapshot };
    if (kind === 'step/output') {
      if (typeof payload.line !== 'string' || !payload.line) return;
      occurrence = {
        ...occurrence,
        ...(typeof event.timestamp === 'string' ? { lastActivityAt: event.timestamp } : {}),
        logs: [...(occurrence.logs ?? []), {
          stream: typeof payload.stream === 'string' ? payload.stream : 'stdout',
          line: payload.line,
        }].slice(-50),
      };
    } else {
      const statuses: Record<string, string> = {
        'step/started': 'running',
        'step/resumed': 'running',
        'step/delaying': 'delaying',
        'step/completed': 'completed',
        'step/failed': 'failed',
        'step/indeterminate': 'indeterminate',
        'step/skipped': 'skipped',
        'step/cancelled': 'cancelled',
        'step/denied': 'denied',
        'step/blocked': 'blocked',
      };
      const status = statuses[kind];
      if (!status) return;
      const timestamp = typeof event.timestamp === 'string' ? event.timestamp : undefined;
      if (kind === 'step/completed' || kind === 'step/failed') {
        sanitizeDisplayPayload(payload, expectedDisplaySnapshot);
        sanitizePresentationPayload(payload);
      }
      const presentation = kind === 'step/completed' || kind === 'step/failed'
        ? terminalPresentation(payload, existingOccurrence, expectedDisplaySnapshot) : undefined;
      const output = plainRecord(payload.output);
      const captures = plainRecord(payload.captures);
      occurrence = {
        ...occurrence,
        status,
        ...(timestamp ? { lastActivityAt: timestamp } : {}),
        ...(producerStepKind(payload) ? { stepKind: producerStepKind(payload) } : {}),
        ...((kind === 'step/completed' || kind === 'step/failed') ? {
          codePresentation: undefined, outputValueStatus: undefined, presentationDiagnostic: undefined,
          displayPresentation: undefined, displayPresentationDiagnostic: undefined,
          output, ...presentation,
        } : {}),
        ...(typeof payload.error === 'string' ? { error: payload.error } : {}),
        ...(typeof payload.duration_ms === 'number' ? { durationMs: payload.duration_ms } : {}),
        ...(typeof payload.attempt === 'number' ? { attempt: payload.attempt } : {}),
        ...(output ? { output } : {}),
        ...(captures ? { captures } : {}),
        ...(payload.evidence !== undefined ? { evidence: payload.evidence } : {}),
        ...((kind === 'step/started' || kind === 'step/resumed') && timestamp ? { startedAt: timestamp } : {}),
        ...((kind === 'step/started' || kind === 'step/resumed') ? {
          startedEventSequence: occurrence.startedEventSequence ?? eventSequence,
          executionLane,
          graphRevision,
          ...(occurrence.predecessorNodeID || !this.lastStartedNodeByLane.get(laneKey)
            ? {}
            : { predecessorNodeID: this.lastStartedNodeByLane.get(laneKey)!.nodeID }),
        } : {}),
        ...(['step/completed', 'step/failed', 'step/indeterminate', 'step/skipped'].includes(kind) && timestamp
          ? { finishedAt: timestamp, finishedEventSequence: eventSequence }
          : {}),
      };
    }
    if (occurrenceIndex >= 0) occurrences[occurrenceIndex] = occurrence;
    else occurrences.push(occurrence);
    occurrences.sort((left, right) => {
      const leftAttempt = this.requireManifest().attempts[left.runID]?.ordinal ?? 0;
      const rightAttempt = this.requireManifest().attempts[right.runID]?.ordinal ?? 0;
      return leftAttempt - rightAttempt || compareOccurrences(left, right);
    });
    const activeRunID = this.requireManifest().session.active_run_id;
    const selected = [...occurrences].reverse().find((candidate) => candidate.runID === activeRunID)
      ?? occurrences[occurrences.length - 1];
    this.runtimeNodes = {
      ...this.runtimeNodes,
      [nodeID]: reconcileRuntimeDisplayState({ ...selected, occurrences,
        displayObservations: previous.displayObservations }) as SessionRuntimeNodeState,
    };
    if ((kind === 'step/started' || kind === 'step/resumed') && selected.occurrenceID === occurrence.occurrenceID &&
        (!activeRunID || activeRunID === runID)) {
      this.currentExecutionNodeID = nodeID;
      this.lastStartedNodeByLane.set(laneKey, { nodeID, eventSequence });
    }
  }

  private executionLane(segmentID: string, localNodeID: string, revision?: number): string {
    const graph = revision === undefined
      ? this.segmentGraphs.get(segmentID)
      : this.graphHistory.get(segmentID)?.get(revision)?.document ?? this.segmentGraphs.get(segmentID);
    return executionLaneForGraph(graph, localNodeID);
  }

  private highestKnownGraphRevision(segmentID: string): number {
    return Math.max(
      0,
      this.graphRevisions.get(segmentID) ?? 0,
      ...this.graphHistory.get(segmentID)?.keys() ?? [],
      ...this.graphAvailability.get(segmentID)?.keys() ?? [],
    );
  }

  private validateRestoredGraphAvailability(): void {
    const manifest = this.requireManifest();
    for (const [segmentID, segment] of Object.entries(manifest.segments)) {
      const currentRevision = segment.graph_revision;
      const available = this.graphAvailability.get(segmentID);
      const loaded = this.graphHistory.get(segmentID);
      if (currentRevision === undefined) {
        if ((available?.size ?? 0) > 0 || (loaded?.size ?? 0) > 0) {
          throw new Error('graph availability requires a manifest revision head');
        }
        continue;
      }
      for (const revision of available?.keys() ?? []) {
        if (revision < 1 || revision > currentRevision) {
          throw new Error('graph availability revision is outside the manifest head');
        }
      }
      for (let revision = 1; revision <= currentRevision; revision += 1) {
        if (!available?.has(revision)) {
          throw new Error('graph availability revisions are not contiguous');
        }
      }
      for (const [revision, loadedRevision] of loaded ?? []) {
        const advertised = available?.get(revision);
        if (!advertised || loadedRevision.wholeBlobHash !== advertised.graph_hash ||
            !sameGraphBinding(loadedRevision.segmentSnapshot, advertised)) {
          throw new Error('graph availability conflicts with loaded history');
        }
      }
    }
  }

  private recomputeSegmentExecutionLanes(
    segmentID: string,
  ): void {
    const occurrences = Object.entries(this.runtimeNodes).flatMap(([nodeID, state]) => (
      (state.occurrences ?? []).flatMap((occurrence) => (
        occurrence.segmentID === segmentID
          ? [{ nodeID, occurrence }]
          : []
      ))
    )).sort((left, right) => (
      (left.occurrence.startedEventSequence ?? Number.MAX_SAFE_INTEGER) -
        (right.occurrence.startedEventSequence ?? Number.MAX_SAFE_INTEGER)
    ));
    const runIDs = new Set(occurrences.map(({ occurrence }) => occurrence.runID));
    for (const key of [...this.lastStartedNodeByLane.keys()]) {
      if (runIDs.has(key.slice(0, key.indexOf('\u0000')))) this.lastStartedNodeByLane.delete(key);
    }
    const previousByLane = new Map<string, { nodeID: string; eventSequence: number }>();
    const updated = new Map<string, SessionRuntimeOccurrence>();
    for (const { nodeID, occurrence } of occurrences) {
      const graph = occurrence.graphRevision === undefined
        ? undefined
        : this.graphHistory.get(segmentID)?.get(occurrence.graphRevision)?.document;
      const lane = graph
        ? executionLaneForGraph(graph, occurrence.qualifiedNodeID)
        : occurrence.executionLane ?? 'main';
      const laneKey = `${occurrence.runID}\u0000${lane}`;
      const previous = previousByLane.get(laneKey);
      const { predecessorNodeID: _, ...withoutPredecessor } = occurrence;
      const next: SessionRuntimeOccurrence = {
        ...withoutPredecessor,
        executionLane: lane,
        graphRevision: occurrence.graphRevision,
        ...(previous && occurrence.startedEventSequence !== undefined
          ? { predecessorNodeID: previous.nodeID }
          : {}),
      };
      updated.set(occurrence.occurrenceID, next);
      if (occurrence.startedEventSequence !== undefined) {
        const cursor = { nodeID, eventSequence: occurrence.startedEventSequence };
        previousByLane.set(laneKey, cursor);
        this.lastStartedNodeByLane.set(laneKey, cursor);
      }
    }
    if (updated.size === 0) return;
    this.runtimeNodes = Object.fromEntries(Object.entries(this.runtimeNodes).map(([nodeID, state]) => {
      const nextOccurrences = state.occurrences?.map((occurrence) => updated.get(occurrence.occurrenceID) ?? occurrence);
      if (!nextOccurrences || nextOccurrences === state.occurrences) return [nodeID, state];
      const selected = selectRuntimeOccurrence(nextOccurrences, this.requireManifest().session.active_run_id);
      return [nodeID, { ...selected, occurrences: nextOccurrences }];
    }));
  }

  private parsePendingInteraction(frame: SessionProtocolFrame): SessionPendingInteraction {
    const state = objectValue(frame.payload, 'pending interaction');
    const request = objectValue(state.request, 'pending interaction request');
    const runID = frame.runID ?? stringValue(request.runID, 'pending interaction run ID');
    const stepID = stringValue(state.step_id, 'pending interaction step');
    const localNodeID = typeof state.node_id === 'string' && state.node_id ? state.node_id : stepID;
    const segmentID = frame.segmentID || this.requireManifest().attempts[runID]?.segment_id;
    if (!segmentID) throw new Error('pending interaction has no owning segment');
    return {
      ...request,
      type: 'pending',
      turnID: stringValue(state.turn_id, 'pending interaction turn'),
      runID,
      stepID,
      nodeID: sessionGraphNodeID(this.sessionID, segmentID, localNodeID),
      kind: stringValue(state.kind, 'pending interaction kind'),
    };
  }
}

function executionLaneForGraph(graph: GraphDocument | undefined, localNodeID: string): string {
  const nodeByID = new Map(graph?.nodes.map((node) => [node.id, node]) ?? []);
  const groupByID = new Map(graph?.groups.map((group) => [group.id, group]) ?? []);
  let groupID = typeof nodeByID.get(localNodeID)?.data.group_id === 'string'
    ? String(nodeByID.get(localNodeID)?.data.group_id)
    : '';
  const visited = new Set<string>();
  while (groupID && !visited.has(groupID)) {
    visited.add(groupID);
    const group = groupByID.get(groupID);
    if (!group) break;
    if (group.kind === 'parallel-branch') return `parallel:${group.id}`;
    const parent = nodeByID.get(group.parent_node_id);
    groupID = typeof parent?.data.group_id === 'string' ? parent.data.group_id : '';
  }
  return 'main';
}

function selectRuntimeOccurrence(
  occurrences: readonly SessionRuntimeOccurrence[],
  activeRunID?: string,
): SessionRuntimeOccurrence {
  return [...occurrences].reverse().find((candidate) => candidate.runID === activeRunID)
    ?? occurrences[occurrences.length - 1];
}

export function parseSessionManifest(value: unknown, sessionID: string): SessionManifest {
  const root = objectValue(value, 'session manifest');
  if (root.schema_version !== 'investigation-session-manifest/v1') {
    throw new Error('session manifest schema is unsupported');
  }
  const session = objectValue(root.session, 'session record');
  if (session.session_id !== sessionID || !positiveInteger(session.sequence)) {
    throw new Error('session manifest identity is invalid');
  }
  return {
    schema_version: 'investigation-session-manifest/v1',
    session: {
      ...(session as unknown as SessionRecord),
      session_id: sessionID,
      status: stringValue(session.status, 'session status'),
      root_segment_id: stringValue(session.root_segment_id, 'root segment id'),
      sequence: session.sequence,
    },
    segments: parseRecord(root.segments, 'session segments', parseSessionSegment, 'segment_id'),
    attempts: parseRecord(root.attempts, 'session attempts', parseSessionAttempt, 'run_id'),
    transitions: parseRecord(root.transitions ?? {}, 'session transitions', parseTransition, 'transition_id'),
    occurrences: plainRecord(root.occurrences) ?? {},
    accepted_commands: plainRecord(root.accepted_commands) ?? {},
  };
}

function compositeManifestFingerprint(manifest: SessionManifest): string {
  return JSON.stringify({
    activeSegmentID: manifest.session.active_segment_id ?? '',
    activeRunID: manifest.session.active_run_id ?? '',
    segments: Object.values(manifest.segments)
      .sort((left, right) => left.segment_id.localeCompare(right.segment_id))
      .map((segment) => ({
        id: segment.segment_id,
        ordinal: segment.ordinal,
        runbookID: segment.runbook_id,
        runbookName: segment.runbook_name,
        status: segment.status,
        entryStep: segment.entry_selector?.step ?? '',
        attemptRunIDs: segment.attempt_run_ids,
      })),
    attempts: Object.values(manifest.attempts)
      .sort((left, right) => left.run_id.localeCompare(right.run_id))
      .map((attempt) => ({
        id: attempt.run_id,
        segmentID: attempt.segment_id,
        ordinal: attempt.ordinal,
        mode: attempt.mode,
        status: attempt.status,
      })),
    transitions: Object.values(manifest.transitions)
      .sort((left, right) => left.transition_id.localeCompare(right.transition_id))
      .map((transition) => ({
        id: transition.transition_id,
        status: transition.status,
        sourceSegmentID: transition.source_segment_id,
        sourceRunID: transition.source_occurrence.run_id,
        sourceNodeID: transition.source_occurrence.qualified_node_id,
        sourceStepID: transition.source_occurrence.step,
        targetSegmentID: transition.target_segment_id,
        targetRunID: transition.target_run_id,
        reasonCode: transition.reason_code ?? '',
        reasonSummary: transition.reason_summary ?? '',
      })),
  });
}

function mergeSessionManifest(
  current: SessionManifest | undefined,
  update: SessionManifest,
): SessionManifest {
  if (!current) return update;
  if (update.session.sequence < current.session.sequence ||
      !jsonEqual(
        withoutKeys(current.session, SESSION_MUTABLE_FIELDS),
        withoutKeys(update.session, SESSION_MUTABLE_FIELDS),
      ) || !monotonicField(current.session, update.session, 'manifest_revision') ||
      !monotonicField(current.session, update.session, 'writer_epoch')) {
    throw new Error('session snapshot rewrites immutable history');
  }
  for (const [segmentID, segment] of Object.entries(update.segments)) {
    const existing = current.segments[segmentID];
    if (existing) validateSegmentRecord(existing, segment);
  }
  for (const [runID, attempt] of Object.entries(update.attempts)) {
    const existing = current.attempts[runID];
    if (existing) validateAttemptRecord(existing, attempt);
  }
  for (const [transitionID, transition] of Object.entries(update.transitions)) {
    const existing = current.transitions[transitionID];
    if (existing) validateTransitionRecord(existing, transition);
  }
  for (const [occurrenceID, occurrence] of Object.entries(update.occurrences)) {
    const existing = current.occurrences[occurrenceID];
    if (existing !== undefined && !jsonEqual(existing, occurrence)) {
      throw new Error('session snapshot rewrites immutable occurrence metadata');
    }
  }
  return {
    ...update,
    segments: mergeRecordMap(current.segments, update.segments, mergeSegmentRecord),
    attempts: mergeRecordMap(current.attempts, update.attempts, mergeAttemptRecord),
    transitions: mergeRecordMap(current.transitions, update.transitions, (existing, next) => ({
      ...existing,
      ...next,
    })),
    occurrences: { ...current.occurrences, ...update.occurrences },
  };
}

const SESSION_MUTABLE_FIELDS = new Set([
  'status', 'active_segment_id', 'active_run_id', 'sequence',
  'manifest_revision', 'writer_epoch', 'updated_at',
]);
const SEGMENT_MUTABLE_FIELDS = new Set([
  'status', 'attempt_run_ids', 'plan_hash', 'graph_hash', 'executable_snapshot_hash',
  'executable_revision', 'graph_revision',
]);
const ATTEMPT_MUTABLE_FIELDS = new Set([
  'status', 'updated_at', 'completed_at', 'run_projection_hash', 'execution_mutation_hash',
  'checkpoint_sequence', 'committed_trace_sequence', 'journaled_trace_sequence',
]);
const TRANSITION_MUTABLE_FIELDS = new Set(['status', 'committed_at', 'aborted_at']);
const TERMINAL_OCCURRENCE_STATUSES = new Set(['completed', 'failed', 'skipped', 'indeterminate', 'cancelled', 'denied', 'blocked']);

function validateSegmentRecord(existing: SessionSegment, update: SessionSegment): void {
  if (!jsonEqual(
    withoutKeys(existing, SEGMENT_MUTABLE_FIELDS),
    withoutKeys(update, SEGMENT_MUTABLE_FIELDS),
  ) || !arrayPrefix(existing.attempt_run_ids, update.attempt_run_ids) ||
    !monotonicField(existing, update, 'graph_revision') ||
    !monotonicField(existing, update, 'executable_revision') ||
    sameRevisionChanged(existing, update, 'graph_revision', ['graph_hash']) ||
    sameRevisionChanged(existing, update, 'executable_revision', ['plan_hash', 'executable_snapshot_hash'])) {
    throw new Error('session frame rewrites immutable segment metadata');
  }
}

function mergeSegmentRecord(existing: SessionSegment, update: SessionSegment): SessionSegment {
  validateSegmentRecord(existing, update);
  return { ...existing, ...update };
}

function validateAttemptRecord(existing: SessionAttempt, update: SessionAttempt): void {
  if (!jsonEqual(
    withoutKeys(existing, ATTEMPT_MUTABLE_FIELDS),
    withoutKeys(update, ATTEMPT_MUTABLE_FIELDS),
  )) {
    throw new Error('session frame rewrites immutable attempt metadata');
  }
}

function mergeAttemptRecord(existing: SessionAttempt, update: SessionAttempt): SessionAttempt {
  validateAttemptRecord(existing, update);
  return { ...existing, ...update };
}

function validateTransitionRecord(existing: SessionTransition, update: SessionTransition): void {
  if (!jsonEqual(
    withoutKeys(existing, TRANSITION_MUTABLE_FIELDS),
    withoutKeys(update, TRANSITION_MUTABLE_FIELDS),
  )) {
    throw new Error('session frame rewrites immutable transition metadata');
  }
}

function withoutKeys(value: object, omitted: ReadonlySet<string>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(value).filter(([key]) => !omitted.has(key)));
}

function arrayPrefix(existing: readonly string[], update: readonly string[]): boolean {
  return existing.length <= update.length && existing.every((value, index) => update[index] === value);
}

function monotonicField(existing: object, update: object, key: string): boolean {
  const previous = (existing as Record<string, unknown>)[key];
  const next = (update as Record<string, unknown>)[key];
  return previous === undefined || next === undefined ||
    typeof previous !== 'number' || typeof next !== 'number' || next >= previous;
}

function sameRevisionChanged(
  existing: object,
  update: object,
  revisionKey: string,
  valueKeys: readonly string[],
): boolean {
  const previous = existing as Record<string, unknown>;
  const next = update as Record<string, unknown>;
  return previous[revisionKey] !== undefined && previous[revisionKey] === next[revisionKey] &&
    valueKeys.some((key) => previous[key] !== next[key]);
}

export function parseSessionSegment(value: unknown): SessionSegment {
  const segment = objectValue(value, 'session segment');
  if (segment.graph_revision !== undefined && !positiveInteger(segment.graph_revision) ||
      segment.executable_revision !== undefined && !positiveInteger(segment.executable_revision)) {
    throw new Error('session segment revisions must be positive integers');
  }
  return {
    ...(segment as unknown as SessionSegment),
    segment_id: stringValue(segment.segment_id, 'segment id'),
    ordinal: integerValue(segment.ordinal, 'segment ordinal'),
    runbook_id: stringValue(segment.runbook_id, 'segment runbook id'),
    runbook_name: stringValue(segment.runbook_name, 'segment runbook name'),
    status: stringValue(segment.status, 'segment status'),
    attempt_run_ids: stringArray(segment.attempt_run_ids, 'segment attempt run ids'),
  };
}

export function parseSessionAttempt(value: unknown): SessionAttempt {
  const attempt = objectValue(value, 'session attempt');
  return {
    ...(attempt as unknown as SessionAttempt),
    run_id: stringValue(attempt.run_id, 'attempt run id'),
    segment_id: stringValue(attempt.segment_id, 'attempt segment id'),
    ordinal: integerValue(attempt.ordinal, 'attempt ordinal'),
    mode: stringValue(attempt.mode, 'attempt mode'),
    status: stringValue(attempt.status, 'attempt status'),
  };
}

function parseTransition(value: unknown): SessionTransition {
  const transition = objectValue(value, 'session transition');
  const source = objectValue(transition.source_occurrence, 'transition source occurrence');
  return {
    ...(transition as unknown as SessionTransition),
    transition_id: stringValue(transition.transition_id, 'transition id'),
    status: stringValue(transition.status, 'transition status'),
    source_segment_id: stringValue(transition.source_segment_id, 'transition source segment'),
    source_occurrence: {
      ...(source as unknown as SessionTransition['source_occurrence']),
      run_id: stringValue(source.run_id, 'transition source run'),
      qualified_node_id: stringValue(source.qualified_node_id, 'transition source node'),
      step: stringValue(source.step, 'transition source step'),
    },
    target_segment_id: stringValue(transition.target_segment_id, 'transition target segment'),
    target_run_id: stringValue(transition.target_run_id, 'transition target run'),
    target_runbook_id: stringValue(transition.target_runbook_id, 'transition target runbook'),
  };
}

function parseRecord<T>(
  value: unknown,
  label: string,
  parse: (item: unknown) => T,
  identityKey: keyof T,
): Record<string, T> {
  const source = objectValue(value, label);
  const result: Record<string, T> = {};
  for (const [key, item] of Object.entries(source)) {
    const parsed = parse(item);
    if (parsed[identityKey] !== key) throw new Error(`${label} key does not match its identity`);
    result[key] = parsed;
  }
  return result;
}

function runtimeEventNodeID(payload: Record<string, unknown>): string | undefined {
  if (typeof payload.qualified_node_id === 'string' && payload.qualified_node_id) return payload.qualified_node_id;
  if (typeof payload.node_id === 'string' && payload.node_id) return payload.node_id;
  if (typeof payload.step_id !== 'string' || !payload.step_id) return undefined;
  const callPath = Array.isArray(payload.call_path)
    ? payload.call_path.flatMap((value) => {
        const frame = plainRecord(value);
        return typeof frame?.step_id === 'string' && frame.step_id ? [frame.step_id] : [];
      })
    : [];
  const escape = (part: string) => part.replaceAll('~', '~0').replaceAll('/', '~1');
  return [...callPath.map(escape), escape(payload.step_id)].join('/');
}

function cloneManifest(manifest: SessionManifest): SessionManifest {
  return JSON.parse(JSON.stringify(manifest)) as SessionManifest;
}

function replaceMap<K, V>(target: Map<K, V>, source: ReadonlyMap<K, V>): void {
  target.clear();
  for (const [key, value] of source) target.set(key, value);
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  const result = plainRecord(value);
  if (!result) throw new Error(`${label} must be an object`);
  return result;
}

function plainRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined;
}

function stringValue(value: unknown, label: string): string {
  if (typeof value !== 'string' || !value) throw new Error(`${label} must be a non-empty string`);
  return value;
}

function integerValue(value: unknown, label: string): number {
  if (!positiveInteger(value)) throw new Error(`${label} must be a positive integer`);
  return value;
}

function positiveInteger(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) > 0;
}

function stringArray(value: unknown, label: string): string[] {
  if (!Array.isArray(value) || value.some((item) => typeof item !== 'string' || !item)) {
    throw new Error(`${label} must be an array of non-empty strings`);
  }
  return [...value];
}

function transitionProvenance(
  manifest: SessionManifest,
  segmentID: string,
  direction: 'incoming' | 'outgoing',
): Record<string, unknown>[] {
  return Object.values(manifest.transitions)
    .filter((transition) => direction === 'incoming'
      ? transition.target_segment_id === segmentID
      : transition.source_segment_id === segmentID)
    .sort((left, right) => left.transition_id.localeCompare(right.transition_id))
    .map((transition) => ({
      transition_id: transition.transition_id,
      status: transition.status,
      source_segment_id: transition.source_segment_id,
      source_run_id: transition.source_occurrence.run_id,
      source_node_id: transition.source_occurrence.qualified_node_id,
      target_segment_id: transition.target_segment_id,
      target_run_id: transition.target_run_id,
      target_runbook_id: transition.target_runbook_id,
      reason_code: transition.reason_code,
      reason_summary: transition.reason_summary,
    }));
}

function jsonEqual(left: unknown, right: unknown): boolean {
  if (Object.is(left, right)) return true;
  if (Array.isArray(left) || Array.isArray(right)) {
    return Array.isArray(left) && Array.isArray(right) && left.length === right.length &&
      left.every((value, index) => jsonEqual(value, right[index]));
  }
  const leftRecord = plainRecord(left);
  const rightRecord = plainRecord(right);
  if (!leftRecord || !rightRecord) return false;
  const leftKeys = Object.keys(leftRecord).sort();
  const rightKeys = Object.keys(rightRecord).sort();
  return leftKeys.length === rightKeys.length && leftKeys.every((key, index) => (
    key === rightKeys[index] && jsonEqual(leftRecord[key], rightRecord[key])
  ));
}

function validatePreparedTransitionTarget(
  transition: SessionTransition,
  targetSegment: SessionSegment,
  targetAttempt: SessionAttempt,
): void {
  const raw = transition as SessionTransition & Record<string, unknown>;
  const checks: Array<[unknown, unknown]> = [
    [transition.target_segment_id, targetSegment.segment_id],
    [transition.target_run_id, targetAttempt.run_id],
    [transition.target_runbook_id, targetSegment.runbook_id],
    [targetAttempt.segment_id, targetSegment.segment_id],
    [raw.target_runbook_name, targetSegment.runbook_name],
    [raw.target_plan_hash, targetSegment.plan_hash],
    [raw.target_graph_hash, targetSegment.graph_hash],
    [raw.target_executable_snapshot_hash, targetSegment.executable_snapshot_hash],
  ];
  if (checks.some(([expected, actual]) => expected !== undefined && expected !== '' && expected !== actual) ||
      !targetSegment.attempt_run_ids.includes(targetAttempt.run_id)) {
    throw new Error('committed transition does not match its prepared transition target');
  }
}

function mergeRecordMap<T>(
  current: Readonly<Record<string, T>>,
  update: Readonly<Record<string, T>>,
  merge: (existing: T, next: T) => T,
): Record<string, T> {
  const result = { ...current };
  for (const [key, value] of Object.entries(update)) {
    result[key] = current[key] === undefined ? value : merge(current[key], value);
  }
  return result;
}

function clonePreparedTransitionTarget(target: SessionPreparedTransitionTarget): SessionPreparedTransitionTarget {
  return {
    segment: { ...target.segment, attempt_run_ids: [...target.segment.attempt_run_ids] },
    attempt: { ...target.attempt },
  };
}

export function validateSessionSegmentGraphBinding(
  segment: SessionSegment,
  revision: number,
  graphHash: string,
): void {
  const sha256 = /^sha256:[0-9a-f]{64}$/;
  const planHash = /^(?:sha256:)?[0-9a-f]{64}$/;
  if (!Number.isSafeInteger(revision) || revision < 1 || segment.graph_revision !== revision ||
      segment.executable_revision !== revision || segment.graph_hash !== graphHash ||
      !sha256.test(graphHash) || typeof segment.plan_hash !== 'string' || !planHash.test(segment.plan_hash) ||
      typeof segment.executable_snapshot_hash !== 'string' || !sha256.test(segment.executable_snapshot_hash)) {
    throw new Error('session segment graph and executable binding is invalid');
  }
}

function sameGraphBinding(left: SessionSegment, right: SessionSegment): boolean {
  return left.segment_id === right.segment_id &&
    left.graph_revision === right.graph_revision && left.graph_hash === right.graph_hash &&
    left.executable_revision === right.executable_revision && left.plan_hash === right.plan_hash &&
    left.executable_snapshot_hash === right.executable_snapshot_hash;
}