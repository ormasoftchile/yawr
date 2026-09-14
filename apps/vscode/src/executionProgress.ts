import type { GraphDocument } from './directGraphPreview';
import { isSettledStepStatus, isTerminalRunStatus } from './runStatus';

export interface ProgressObservation {
  status: string;
  occurrenceID?: string;
  runID?: string;
  qualifiedNodeID?: string;
  invocation?: number;
  retryAttempt?: number;
  occurrenceSequence?: number;
  startedEventSequence?: number;
  phase?: string;
  frameID?: string;
  frameStepIndex?: number;
  dispatchOccurrenceID?: string;
  executionLane?: string;
  segmentID?: string;
  graphRevision?: number;
  startedAt?: string;
  finishedAt?: string;
  lastActivityAt?: string;
  stepKind?: string;
  output?: Record<string, unknown>;
}
export interface ProgressNode extends ProgressObservation {
  occurrences?: readonly ProgressObservation[];
}
export const isActiveStepStatus = (status: string) => ['running', 'delaying', 'waiting'].includes(status);
export const isExecutionEnded = (status: string) => isTerminalRunStatus(status) ||
  ['resolved', 'escalated', 'abandoned'].includes(status);

function graphProgressAliases(document: GraphDocument): Map<string, string> {
  const groups = new Map(document.groups.map(group => [group.id, group]));
  const nodes = new Map(document.nodes.map(node => [node.id, node]));
  const aliases = new Map<string, string>();
  const visiting = new Set<string>();
  const resolve = (id: string): string => {
    if (aliases.has(id)) return aliases.get(id)!;
    if (visiting.has(id)) return id;
    visiting.add(id);
    const node = nodes.get(id);
    const group = groups.get(String(node?.data.group_id ?? node?.parentNode ?? ''));
    const alias = group?.kind === 'parallel-branch' && group.parent_node_id && node
      ? `${resolve(group.parent_node_id)}/${String(node.data.step_id || id)}`
      : id;
    visiting.delete(id);
    aliases.set(id, alias);
    return alias;
  };
  for (const node of document.nodes) resolve(node.id);
  return aliases;
}

export function graphExecutionNodeID(document: GraphDocument, nodeID: string | undefined): string | undefined {
  if (!nodeID) return undefined;
  if (document.nodes.some(node => node.id === nodeID)) return nodeID;
  for (const [id, alias] of graphProgressAliases(document)) if (alias === nodeID) return id;
  return undefined;
}

export function directOccurrenceID(runID: string, nodeID: string, payload: Record<string, unknown>): string {
  const evidence = (value: unknown) => value === undefined ? ['absent'] : ['supplied', value];
  return JSON.stringify([runID, nodeID, ...[
    payload.phase, payload.invocation,
    payload.retry_attempt === undefined ? payload.attempt : payload.retry_attempt,
    payload.occurrence_sequence, payload.frame_id, payload.frame_step_index,
    payload.dispatch_occurrence_id, payload.execution_lane,
  ].map(evidence)]);
}

export function validProgressIdentity(payload: Record<string, unknown>): boolean {
  return ['invocation', 'retry_attempt', 'attempt', 'occurrence_sequence', 'frame_step_index'].every(key =>
    payload[key] === undefined || typeof payload[key] === 'number' && Number.isSafeInteger(payload[key]) &&
      (payload[key] as number) >= (key === 'frame_step_index' ? 0 : 1)) &&
    ['phase', 'frame_id', 'dispatch_occurrence_id', 'execution_lane'].every(key =>
      payload[key] === undefined || typeof payload[key] === 'string' && (payload[key] as string).length > 0);
}

export function producerStepKind(payload: Record<string, unknown>): string | undefined {
  const kind = payload.step_kind ?? payload.kind;
  return typeof kind === 'string' && /^[a-z][a-z0-9_-]*$/.test(kind) ? kind : undefined;
}

export function compareOccurrences(left: ProgressObservation, right: ProgressObservation): number {
  // An event's arrival sequence is not an invocation clock on older runtimes.
  if (left.runID === right.runID && left.phase === right.phase &&
      left.frameID === right.frameID && left.frameStepIndex === right.frameStepIndex &&
      left.executionLane === right.executionLane) {
    if (left.invocation !== undefined && right.invocation !== undefined) {
      const invocation = left.invocation - right.invocation;
      if (invocation) return invocation;
      if (left.retryAttempt !== undefined && right.retryAttempt !== undefined) {
        const retry = left.retryAttempt - right.retryAttempt;
        if (retry) return retry;
      }
      if (left.retryAttempt === right.retryAttempt && left.dispatchOccurrenceID === right.dispatchOccurrenceID &&
          left.occurrenceSequence !== undefined && right.occurrenceSequence !== undefined) {
        const occurrence = left.occurrenceSequence - right.occurrenceSequence;
        if (occurrence) return occurrence;
      }
    }
  }
  // Local counters cannot order different frames/lanes. Unknown starts remain
  // incomparable; callers retain stable observation order, never UUID order.
  if (left.startedEventSequence !== undefined && right.startedEventSequence !== undefined) {
    return left.startedEventSequence - right.startedEventSequence;
  }
  return 0;
}

export function latestProgress(node: ProgressNode): ProgressObservation {
  const history = node.occurrences?.filter(value => !node.runID || value.runID === node.runID);
  if (!history?.length) return node;
  const latest = history.reduce((left, right) => compareOccurrences(left, right) > 0 ? left : right);
  if (!latest.runID) return node;
  if (isSettledStepStatus(latest.status)) return latest;
  // Summary-only terminal status is authoritative when its occurrence is the latest.
  if (isSettledStepStatus(node.status) && (!node.occurrenceID || node.occurrenceID === latest.occurrenceID)) return node;
  return latest;
}

export function normalizeRuntimeStatuses<T extends ProgressNode>(runtime: Readonly<Record<string, T>>): Record<string, T> {
  return Object.fromEntries(Object.entries(runtime).map(([id, value]) => {
    const latest = latestProgress(value);
    return [id, latest.status === value.status ? value : { ...value, status: latest.status }];
  }));
}

export function displayRuntimeStatuses<T extends ProgressNode>(runtime: Readonly<Record<string, T>>, runStatus: string, document?: GraphDocument): Record<string, T> {
  const values = { ...runtime };
  if (document) for (const [id, alias] of graphProgressAliases(document)) {
    if (!values[id] && runtime[alias]) values[id] = runtime[alias];
  }
  return Object.fromEntries(Object.entries(values).map(([id, value]) => {
    const status = latestProgress(value).status;
    return [id, { ...value, status: isExecutionEnded(runStatus) && isActiveStepStatus(status) ? 'no-final-status' : status }];
  }));
}

export function canonicalProgress(document: GraphDocument, runtime: Readonly<Record<string, ProgressNode>>, runStatus: string) {
  const result = { total: 0, completed: 0, issues: 0, skipped: 0, running: 0, remaining: 0, missingFinal: 0 };
  const aliases = graphProgressAliases(document);
  for (const node of document.nodes) {
    if (node.data.synthetic === true || node.data.kind === 'session-entry') continue;
    result.total++;
    const observation = runtime[node.id] ?? runtime[aliases.get(node.id) ?? node.id];
    const value = observation ? latestProgress(observation) : undefined;
    const status = value?.status ?? 'pending';
    if (['failed', 'denied', 'indeterminate', 'cancelled', 'blocked'].includes(status) ||
        value?.output?.outcome_category === 'blocked') result.issues++;
    else if (status === 'completed') result.completed++;
    else if (status === 'skipped') result.skipped++;
    else if (isActiveStepStatus(status) && !isExecutionEnded(runStatus)) result.running++;
    else {
      result.remaining++;
      if (isExecutionEnded(runStatus)) result.missingFinal++;
    }
  }
  return result;
}

export interface CurrentActivity extends ProgressObservation {
  nodeID: string;
  path: string;
  title: string;
  label: string;
  container: boolean;
  inGraph: boolean;
}
export function currentActivities(
  document: GraphDocument, runtime: Readonly<Record<string, ProgressNode>>, runStatus: string,
  runID?: string, pending?: { nodeID?: string; stepID?: string; turnID?: string; kind?: string; runID?: string },
): CurrentActivity[] {
  if (isExecutionEnded(runStatus) || ['idle', 'not-started'].includes(runStatus)) return [];
  const nodes = new Map(document.nodes.map(node => [node.id, node]));
  for (const [id, alias] of graphProgressAliases(document)) {
    if (!nodes.has(alias)) nodes.set(alias, nodes.get(id)!);
  }
  const values: Array<[string, ProgressObservation]> = [];
  for (const [nodeID, state] of Object.entries(runtime)) {
    const lanes = new Map<string, ProgressObservation>();
    for (const observation of state.occurrences ?? []) {
      if (runID && observation.runID && observation.runID !== runID) continue;
      const key = JSON.stringify([observation.runID, observation.phase, observation.frameID,
        observation.frameStepIndex, observation.dispatchOccurrenceID, observation.executionLane]);
      const prior = lanes.get(key);
      if (!prior || compareOccurrences(prior, observation) <= 0) lanes.set(key, observation);
    }
    const latest = latestProgress(state);
    if (!lanes.size) lanes.set('', latest);
    for (const observation of lanes.values()) {
      const effective = isSettledStepStatus(state.status) && state.occurrenceID === observation.occurrenceID
        ? state : observation;
      if ((!runID || !effective.runID || effective.runID === runID) && isActiveStepStatus(effective.status)) values.push([nodeID, effective]);
    }
  }
  const pendingID = pending?.turnID && (!runID || !pending.runID || pending.runID === runID)
    ? pending.nodeID || pending.stepID : undefined;
  if (pendingID && !values.some(([id]) => id === pendingID)) values.push([pendingID, { status: 'waiting' }]);
  const paused = ['paused', 'paused_at_boundary', 'handoff_pending'].includes(runStatus);
  return values.map(([nodeID, value]) => {
    const node = nodes.get(nodeID);
    const path = value.qualifiedNodeID || String(node?.data.original_node_id || nodeID);
    const kind = String(node?.data.kind || value.stepKind || '');
    const container = ['include', 'parallel', 'branch', 'iterate'].includes(kind);
    const hasChild = container && values.some(([otherID, other]) =>
      otherID !== nodeID && (other.qualifiedNodeID || String(nodes.get(otherID)?.data.original_node_id || otherID)).startsWith(path + '/'));
    const interaction = pendingID === nodeID;
    const label = paused || (interaction && pending?.kind === 'debug_break') ? 'Paused'
      : interaction ? (pending?.kind === 'host_action' ? 'Waiting for host action' : 'Waiting for input')
      : value.status === 'delaying' ? 'Waiting — retry delay'
      : hasChild ? 'Waiting for child steps'
      : kind === 'tool' ? 'Waiting for tool result'
      : value.status === 'waiting' ? 'Waiting — reason not reported'
      : container ? 'Active container' : 'Active';
    return { ...value, nodeID: node?.id ?? nodeID, path, title: String(node?.data.title || node?.data.step_id || path),
      label, container, inGraph: !!node };
  }).sort((left, right) => Number(left.container) - Number(right.container) ||
    (right.startedEventSequence ?? 0) - (left.startedEventSequence ?? 0) ||
    (right.occurrenceSequence ?? 0) - (left.occurrenceSequence ?? 0) || left.path.localeCompare(right.path));
}
