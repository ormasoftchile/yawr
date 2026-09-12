import { mkdir, readFile, rename, rm, stat, writeFile } from 'fs/promises';
import { parseDisplayJSON } from './displayPresentationJSON';
import * as path from 'path';
import { createHash, randomUUID } from 'crypto';
import { sanitizeDisplayPayload, decodeDisplayPresentation } from './displayPresentation';
import { reconcileRuntimeDisplayState } from './displayObservations';
import { parseGraphDocument, validateSessionPresentationBinding, type GraphDocument } from './directGraphPreview';
import {
  parseSessionManifest,
  parseSessionAttempt,
  parseSessionSegment,
  sessionGraphNodeID,
  validateSessionSegmentGraphBinding,
  type SessionManifest,
  type SessionPendingInteraction,
  type SessionPreparedTransitionTarget,
  type SessionRuntimeNodeState,
  type SessionSegment,
} from './sessionCompositeGraph';

export const SESSION_WORKSPACE_STATE_KEY = 'yawr.investigationSession.v1';
export const STORED_SESSION_SCHEMA = 'yawr-vscode-session/v1' as const;
export const SESSION_GRAPH_CACHE_SCHEMA = 'yawr-vscode-session-graph-cache/v2' as const;
const MAX_SESSION_GRAPH_CACHE_BYTES = 128 * 1024 * 1024;

export interface StoredSessionDescriptor {
  schemaVersion: typeof STORED_SESSION_SCHEMA;
  sessionID: string;
  creationCommandID: string;
  configurationCommandID?: string;
  runbookPath: string;
  projectRoot: string;
  acceptedSequence: number;
}

export interface CachedSegmentGraph {
  revision: number;
  wholeBlobHash: string;
  encodedDocument: string;
  document: GraphDocument;
  segmentSnapshot: SessionSegment;
}

export interface StoredSessionGraphCache {
  schemaVersion: typeof SESSION_GRAPH_CACHE_SCHEMA;
  sessionID: string;
  sequence: number;
  manifest: SessionManifest;
  segmentGraphs: Record<string, CachedSegmentGraph>;
  segmentGraphHistory: Record<string, Record<string, CachedSegmentGraph>>;
  segmentGraphAvailability: Record<string, Record<string, SessionSegment>>;
  preparedTransitionTargets: Record<string, SessionPreparedTransitionTarget>;
  runtimeNodes: Record<string, SessionRuntimeNodeState>;
  executionNodeID?: string;
  pending?: SessionPendingInteraction;
}

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const digestPattern = /^sha256:[0-9a-f]{64}$/;

export function parseStoredSessionDescriptor(value: unknown): StoredSessionDescriptor {
  const source = objectValue(value, 'stored session');
  if (source.schemaVersion !== STORED_SESSION_SCHEMA ||
      !uuidPattern.test(stringValue(source.sessionID, 'session ID')) ||
      !uuidPattern.test(stringValue(source.creationCommandID, 'creation command ID'))) {
    throw new Error('stored session identity is invalid');
  }
  const acceptedSequence = nonNegativeInteger(source.acceptedSequence, 'stored session sequence');
  const configurationCommandID = source.configurationCommandID === undefined
    ? undefined
    : stringValue(source.configurationCommandID, 'configuration command ID');
  if (configurationCommandID !== undefined && !uuidPattern.test(configurationCommandID)) {
    throw new Error('stored session configuration identity is invalid');
  }
  return {
    schemaVersion: STORED_SESSION_SCHEMA,
    sessionID: source.sessionID as string,
    creationCommandID: source.creationCommandID as string,
    ...(configurationCommandID ? { configurationCommandID } : {}),
    runbookPath: stringValue(source.runbookPath, 'stored runbook path'),
    projectRoot: stringValue(source.projectRoot, 'stored project root'),
    acceptedSequence,
  };
}

export function recoverySequence(
  _descriptor: Pick<StoredSessionDescriptor, 'acceptedSequence'>,
  cache: StoredSessionGraphCache | undefined,
): number {
  if (!cache) return 0;
  return Math.min(cache.sequence, cache.manifest.session.sequence);
}

export class CoalescedAsyncWriter<T> {
  private pending: T | undefined;
  private draining: Promise<void> | undefined;

  constructor(
    private readonly write: (value: T) => Promise<void>,
    private readonly onError: (error: unknown) => void = () => undefined,
  ) {}

  enqueue(value: T): void {
    this.pending = value;
    if (!this.draining) this.draining = this.drain();
  }

  async flush(): Promise<void> {
    while (this.draining) await this.draining;
  }

  private async drain(): Promise<void> {
    try {
      while (this.pending !== undefined) {
        const value = this.pending;
        this.pending = undefined;
        try {
          await this.write(value);
        } catch (error) {
          this.onError(error);
        }
      }
    } finally {
      this.draining = undefined;
      if (this.pending !== undefined) this.draining = this.drain();
    }
  }
}

export class SessionGraphCacheStore {
  constructor(private readonly directory: string) {}

  pathFor(sessionID: string): string {
    if (!uuidPattern.test(sessionID)) throw new Error('session cache ID must be a UUID');
    return path.join(this.directory, `${sessionID}.json`);
  }

  async load(sessionID: string): Promise<StoredSessionGraphCache | undefined> {
    let info;
    try {
      info = await stat(this.pathFor(sessionID));
    } catch {
      return undefined;
    }
    if (!info.isFile() || info.size < 2 || info.size > MAX_SESSION_GRAPH_CACHE_BYTES) return undefined;
    try {
      const encoded = await readFile(this.pathFor(sessionID), 'utf8');
      return parseSessionGraphCache(parseDisplayJSON(encoded), sessionID, true);
    } catch {
      return undefined;
    }
  }

  async save(cache: StoredSessionGraphCache): Promise<void> {
    const validated = parseSessionGraphCache(cache, cache.sessionID);
    const encoded = JSON.stringify({
      ...validated,
      integrityDigest: cacheIntegrityDigest(validated),
    });
    if (Buffer.byteLength(encoded, 'utf8') > MAX_SESSION_GRAPH_CACHE_BYTES) {
      throw new Error('session graph cache exceeds 128 MiB');
    }
    await mkdir(this.directory, { recursive: true });
    const destination = this.pathFor(cache.sessionID);
    const temporary = `${destination}.${process.pid}.${randomUUID()}.tmp`;
    try {
      await writeFile(temporary, encoded, { encoding: 'utf8', mode: 0o600, flag: 'wx' });
      await rm(destination, { force: true });
      await rename(temporary, destination);
    } finally {
      await rm(temporary, { force: true }).catch(() => undefined);
    }
  }

  async delete(sessionID: string): Promise<void> {
    await rm(this.pathFor(sessionID), { force: true });
  }

}

function parseSessionGraphCache(
  value: unknown,
  expectedSessionID: string,
  verifyIntegrity = false,
): StoredSessionGraphCache {
  const source = objectValue(value, 'session graph cache');
  if (source.schemaVersion !== SESSION_GRAPH_CACHE_SCHEMA ||
      source.sessionID !== expectedSessionID) {
    throw new Error('session graph cache identity is invalid');
  }
  const sequence = nonNegativeInteger(source.sequence, 'session graph cache sequence');
  const manifest = parseSessionManifest(source.manifest, expectedSessionID);
  if (manifest.session.sequence < sequence) throw new Error('session graph cache is ahead of its manifest');
  const graphs = objectValue(source.segmentGraphs, 'cached segment graphs');
  const segmentGraphs: Record<string, CachedSegmentGraph> = {};
  for (const [segmentID, raw] of Object.entries(graphs)) {
    if (!manifest.segments[segmentID]) throw new Error('cached graph does not belong to a session segment');
    segmentGraphs[segmentID] = parseCachedSegmentGraph(raw);
  }
  const historySource = objectValue(source.segmentGraphHistory, 'cached segment graph history');
  const segmentGraphHistory: Record<string, Record<string, CachedSegmentGraph>> = {};
  for (const [segmentID, rawRevisions] of Object.entries(historySource)) {
    if (!manifest.segments[segmentID]) throw new Error('cached graph history does not belong to a session segment');
    const revisions = objectValue(rawRevisions, 'cached segment graph revisions');
    const parsedRevisions: Record<string, CachedSegmentGraph> = {};
    for (const [revisionKey, raw] of Object.entries(revisions)) {
      const graph = parseCachedSegmentGraph(raw);
      if (revisionKey !== String(graph.revision)) throw new Error('cached graph revision key does not match its identity');
      parsedRevisions[revisionKey] = graph;
    }
    segmentGraphHistory[segmentID] = parsedRevisions;
  }
  for (const [segmentID, latest] of Object.entries(segmentGraphs)) {
    const archived = segmentGraphHistory[segmentID]?.[String(latest.revision)];
    const current = manifest.segments[segmentID];
    if (!archived || archived.wholeBlobHash !== latest.wholeBlobHash ||
        current.graph_revision !== latest.revision || current.graph_hash !== latest.wholeBlobHash) {
      throw new Error('cached latest graph is missing from revision history');
    }
  }
  const availabilitySource = objectValue(source.segmentGraphAvailability, 'cached graph availability');
  const segmentGraphAvailability: Record<string, Record<string, SessionSegment>> = {};
  for (const [segmentID, rawRevisions] of Object.entries(availabilitySource)) {
    const current = manifest.segments[segmentID];
    if (!current) throw new Error('cached graph availability has an unknown segment');
    const revisions = objectValue(rawRevisions, 'cached available graph revisions');
    const available: Record<string, SessionSegment> = {};
    for (const [revisionKey, raw] of Object.entries(revisions)) {
      const segment = parseSessionSegment(raw);
      if (segment.segment_id !== segmentID || revisionKey !== String(segment.graph_revision) ||
          !segment.graph_revision || segment.graph_revision > (current.graph_revision ?? 0) || !segment.graph_hash) {
        throw new Error('cached graph availability metadata is invalid');
      }
      available[revisionKey] = segment;
    }
    segmentGraphAvailability[segmentID] = available;
  }
  const preparedSource = objectValue(source.preparedTransitionTargets, 'cached prepared transition targets');
  const preparedTransitionTargets: Record<string, SessionPreparedTransitionTarget> = {};
  for (const [transitionID, raw] of Object.entries(preparedSource)) {
    if (!manifest.transitions[transitionID]) throw new Error('cached prepared target has no manifest transition');
    const target = objectValue(raw, 'cached prepared transition target');
    preparedTransitionTargets[transitionID] = {
      segment: parseSessionSegment(target.segment),
      attempt: parseSessionAttempt(target.attempt),
    };
  }
  const cache: StoredSessionGraphCache = {
    schemaVersion: SESSION_GRAPH_CACHE_SCHEMA,
    sessionID: expectedSessionID,
    sequence,
    manifest,
    segmentGraphs,
    segmentGraphHistory,
    segmentGraphAvailability,
    preparedTransitionTargets,
    runtimeNodes: parseRuntimeNodes(source.runtimeNodes ?? {}, (nodeID, occurrence) => {
      const segmentID = occurrence.segmentID, localID = occurrence.qualifiedNodeID, revision = occurrence.graphRevision;
      if (typeof segmentID !== 'string' || typeof localID !== 'string' || typeof revision !== 'number' ||
        nodeID !== sessionGraphNodeID(expectedSessionID, segmentID, localID)) return undefined;
      const graph = segmentGraphHistory[segmentID]?.[String(revision)] ??
        (segmentGraphs[segmentID]?.revision === revision ? segmentGraphs[segmentID] : undefined);
      return graph?.document.display_plan_snapshot_digest;
    }),
    ...(typeof source.executionNodeID === 'string' && source.executionNodeID
      ? { executionNodeID: source.executionNodeID }
      : {}),
    ...(source.pending === undefined ? {} : { pending: parsePendingInteraction(source.pending) }),
  };
  if (verifyIntegrity) {
    const integrityDigest = stringValue(source.integrityDigest, 'session graph cache integrity digest');
    if (!digestPattern.test(integrityDigest) || cacheIntegrityDigest(cache) !== integrityDigest) {
      throw new Error('session graph cache integrity digest does not match its content');
    }
  }
  return cache;
}

function cacheIntegrityDigest(cache: object): string {
  return `sha256:${createHash('sha256').update(JSON.stringify(cache), 'utf8').digest('hex')}`;
}

function parseRuntimeNodes(value: unknown, snapshotFor: (nodeID: string, value: Record<string, unknown>) => string | undefined): Record<string, SessionRuntimeNodeState> {
  const source = objectValue(value, 'cached runtime nodes');
  const result: Record<string, SessionRuntimeNodeState> = {};
  for (const [nodeID, raw] of Object.entries(source)) {
    const state = objectValue(raw, 'cached runtime node');
    const displayScope = state.displayPresentation !== undefined || state.displayPresentationDiagnostic !== undefined ||
      state.displayObservations !== undefined || (Array.isArray(state.occurrences) && state.occurrences.some(value =>
        value && typeof value === 'object' && ('displayPresentation' in value || 'displayPresentationDiagnostic' in value)));
    const sanitize = (value: Record<string, unknown>) => {
      const directDisplaySnapshot = snapshotFor(nodeID, value) ?? 'missing-binding';
      if (value.displayPresentation === undefined && value.displayPresentationDiagnostic === undefined)
        return { ...value, ...(displayScope ? { directDisplaySnapshot } : {}) };
      const payload = { display_presentation: value.displayPresentation, output: value.output,
        display_presentation_diagnostic: value.displayPresentationDiagnostic };
      sanitizeDisplayPayload(payload, snapshotFor(nodeID, value) ?? 'missing-binding');
      return { ...value, directDisplaySnapshot, output: payload.output, displayPresentation: decodeDisplayPresentation(payload.display_presentation),
        displayPresentationDiagnostic: payload.display_presentation_diagnostic as string | undefined };
    };
    result[nodeID] = reconcileRuntimeDisplayState({
      ...(sanitize(state) as unknown as SessionRuntimeNodeState),
      ...(Array.isArray(state.occurrences) ? {
        occurrences: state.occurrences.map(value => sanitize(objectValue(value, 'cached occurrence'))) as unknown as SessionRuntimeNodeState['occurrences'],
      } : {}),
      status: stringValue(state.status, 'cached runtime status'),
    }) as SessionRuntimeNodeState;
  }
  return result;
}

function parsePendingInteraction(value: unknown): SessionPendingInteraction {
  const pending = objectValue(value, 'cached pending interaction');
  return {
    ...(pending as unknown as SessionPendingInteraction),
    type: 'pending',
    turnID: stringValue(pending.turnID, 'cached pending turn'),
    runID: stringValue(pending.runID, 'cached pending run'),
    stepID: stringValue(pending.stepID, 'cached pending step'),
    nodeID: stringValue(pending.nodeID, 'cached pending node'),
    kind: stringValue(pending.kind, 'cached pending kind'),
  };
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`);
  }
  return value as Record<string, unknown>;
}

function stringValue(value: unknown, label: string): string {
  if (typeof value !== 'string' || !value) throw new Error(`${label} must be a non-empty string`);
  return value;
}

function nonNegativeInteger(value: unknown, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0) {
    throw new Error(`${label} must be a non-negative integer`);
  }
  return value as number;
}

function positiveInteger(value: unknown, label: string): number {
  const result = nonNegativeInteger(value, label);
  if (result < 1) throw new Error(`${label} must be positive`);
  return result;
}

function parseCachedSegmentGraph(value: unknown): CachedSegmentGraph {
  const graph = objectValue(value, 'cached segment graph');
  const revision = positiveInteger(graph.revision, 'cached graph revision');
  const wholeBlobHash = stringValue(graph.wholeBlobHash, 'cached graph digest');
  if (!digestPattern.test(wholeBlobHash)) throw new Error('cached graph digest is invalid');
  const encodedDocument = stringValue(graph.encodedDocument, 'cached graph document');
  const actualDigest = `sha256:${createHash('sha256').update(encodedDocument, 'utf8').digest('hex')}`;
  if (actualDigest !== wholeBlobHash) throw new Error('cached graph digest does not match its document');
  const document = parseGraphDocument(encodedDocument);
  if (graph.document !== undefined && JSON.stringify(graph.document) !== JSON.stringify(document)) {
    throw new Error('cached parsed graph does not match its verified document');
  }
  const segmentSnapshot = parseSessionSegment(graph.segmentSnapshot);
  validateSessionSegmentGraphBinding(segmentSnapshot, revision, wholeBlobHash);
  validateSessionPresentationBinding(document, segmentSnapshot.executable_snapshot_hash);
  return { revision, wholeBlobHash, encodedDocument, document, segmentSnapshot };
}

export function parseSessionGraphRevisionResponse(
  encoded: string,
  expectedSessionID: string,
  expectedSegmentID: string,
  expectedRevision: number,
): CachedSegmentGraph {
  if (Buffer.byteLength(encoded, 'utf8') > MAX_SESSION_GRAPH_CACHE_BYTES * 2) {
    throw new Error('session graph response exceeds limits');
  }
  const source = objectValue(parseDisplayJSON(encoded), 'session graph response');
  if (source.schema_version !== 'yawr.session-graph-revision/v1' || source.session_id !== expectedSessionID ||
      source.graph_revision !== expectedRevision || typeof source.data !== 'string' ||
      !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(source.data)) {
    throw new Error('session graph response identity is invalid');
  }
  const segmentSnapshot = parseSessionSegment(source.segment);
  const wholeBlobHash = stringValue(source.graph_hash, 'session graph response digest');
  if (segmentSnapshot.segment_id !== expectedSegmentID || segmentSnapshot.graph_revision !== expectedRevision ||
      segmentSnapshot.graph_hash !== wholeBlobHash || !digestPattern.test(wholeBlobHash)) {
    throw new Error('session graph response metadata does not match its request');
  }
  validateSessionSegmentGraphBinding(segmentSnapshot, expectedRevision, wholeBlobHash);
  const graphBytes = Buffer.from(source.data, 'base64');
  if (graphBytes.toString('base64') !== source.data || graphBytes.length > MAX_SESSION_GRAPH_CACHE_BYTES) {
    throw new Error('session graph response data is invalid');
  }
  const encodedDocument = graphBytes.toString('utf8');
  const actualDigest = `sha256:${createHash('sha256').update(graphBytes).digest('hex')}`;
  if (actualDigest !== wholeBlobHash) throw new Error('session graph response digest mismatch');
  const document = parseGraphDocument(encodedDocument);
  validateSessionPresentationBinding(document, segmentSnapshot.executable_snapshot_hash);
  return {
    revision: expectedRevision,
    wholeBlobHash,
    encodedDocument,
    document,
    segmentSnapshot,
  };
}