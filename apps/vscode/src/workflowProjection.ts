import type { GraphDocument, GraphEdge } from './directGraphPreview';

export type WorkflowMode = 'workflow' | 'all';
export interface WorkflowViewOptions {
  mode: WorkflowMode;
  expandedNodeIDs: ReadonlySet<string>;
  pinnedNodeIDs: ReadonlySet<string>;
  collapsedGroupIDs: ReadonlySet<string>;
}
export interface TechnicalSegment {
  id: string;
  memberNodeIDs: readonly string[];
  internalEdgeIDs: readonly string[];
  groupID: string;
  frameID: string;
  segmentID?: string;
}
export interface ProjectedEdgeProvenance {
  originalEdgeID: string;
  originalSource: string;
  originalTarget: string;
}
export interface WorkflowProjection {
  document: GraphDocument;
  segments: ReadonlyMap<string, TechnicalSegment>;
  representativeByNodeID: ReadonlyMap<string, string>;
  edgeProvenance: ReadonlyMap<string, ProjectedEdgeProvenance>;
  forcedNodeIDs: ReadonlySet<string>;
  expandedGroupIDs: ReadonlySet<string>;
}
interface Observation {
  status: string;
  output?: Record<string, unknown>;
  occurrenceID?: string;
  graphRevision?: number;
  segmentID?: string;
  qualifiedNodeID?: string;
  runID?: string;
  executionLane?: string;
  predecessorNodeID?: string;
}
export interface WorkflowRuntime extends Observation {
  occurrences?: readonly Observation[];
}
export interface WorkflowIssue {
  nodeID: string;
  occurrenceID?: string;
  graphRevision?: number;
  segmentID?: string;
  qualifiedNodeID?: string;
  runID?: string;
  status: string;
  blockedOutcome: boolean;
}
export function isWorkflowIssue(value: Observation): boolean {
  return ['failed', 'denied', 'indeterminate', 'cancelled', 'blocked'].includes(value.status) ||
    value.output?.outcome_category === 'blocked';
}
export function workflowIssueIndex(runtime: Readonly<Record<string, WorkflowRuntime>>): WorkflowIssue[] {
  const result: WorkflowIssue[] = [];
  for (const [nodeID, node] of Object.entries(runtime)) {
    const occurrences = node.occurrences ?? [];
    for (const value of occurrences) {
      if (isWorkflowIssue(value)) result.push({
        nodeID, occurrenceID: value.occurrenceID, graphRevision: value.graphRevision,
        segmentID: value.segmentID, qualifiedNodeID: value.qualifiedNodeID, runID: value.runID,
        status: value.status, blockedOutcome: value.output?.outcome_category === 'blocked',
      });
    }
    if (isWorkflowIssue(node) && !occurrences.some(value =>
      isWorkflowIssue(value) && value.status === node.status && value.output?.outcome_category === node.output?.outcome_category)) {
      result.push({ nodeID, status: node.status, blockedOutcome: node.output?.outcome_category === 'blocked' });
    }
  }
  return result;
}

// Iterative Kosaraju: no recursive DFS or enumeration of paths, even for large loops.
function cycleMembers(ids: readonly string[], outgoing: Map<string, GraphEdge[]>, incoming: Map<string, GraphEdge[]>): Set<string> {
  const seen = new Set<string>(), order: string[] = [];
  for (const root of ids) {
    if (seen.has(root)) continue;
    seen.add(root);
    const stack: Array<{ id: string; next: number }> = [{ id: root, next: 0 }];
    while (stack.length) {
      const top = stack[stack.length - 1], edges = outgoing.get(top.id) ?? [];
      if (top.next === edges.length) { order.push(top.id); stack.pop(); continue; }
      const id = edges[top.next++].target;
      if (!seen.has(id)) { seen.add(id); stack.push({ id, next: 0 }); }
    }
  }
  seen.clear();
  const cyclic = new Set<string>();
  for (let i = order.length - 1; i >= 0; i--) {
    const root = order[i];
    if (seen.has(root)) continue;
    const members: string[] = [], stack = [root];
    seen.add(root);
    while (stack.length) {
      const id = stack.pop()!;
      members.push(id);
      for (const edge of incoming.get(id) ?? []) if (!seen.has(edge.source)) {
        seen.add(edge.source); stack.push(edge.source);
      }
    }
    if (members.length > 1 || (outgoing.get(root) ?? []).some(edge => edge.target === root)) {
      for (const id of members) cyclic.add(id);
    }
  }
  return cyclic;
}

export function projectWorkflow(
  structuralDocument: GraphDocument,
  runtime: Readonly<Record<string, WorkflowRuntime>>,
  options: WorkflowViewOptions,
  routeDocument = structuralDocument,
): WorkflowProjection {
  const nodes = new Map(structuralDocument.nodes.map(node => [node.id, node]));
  const groups = new Map(structuralDocument.groups.map(group => [group.id, group]));
  const frames = new Map(structuralDocument.frames.map(frame => [frame.id, frame]));
  const incoming = new Map<string, GraphEdge[]>(), outgoing = new Map<string, GraphEdge[]>();
  for (const edge of structuralDocument.edges) {
    if (!nodes.has(edge.source) || !nodes.has(edge.target)) continue;
    if (!incoming.has(edge.target)) incoming.set(edge.target, []);
    if (!outgoing.has(edge.source)) outgoing.set(edge.source, []);
    incoming.get(edge.target)!.push(edge); outgoing.get(edge.source)!.push(edge);
  }
  const forced = new Set<string>(), expandedGroups = new Set<string>();
  const issueRoots = workflowIssueIndex(runtime).map(issue => issue.nodeID);
  const prerequisites = new Set<string>(), reverse = [...issueRoots];
  for (let i = 0; i < reverse.length; i++) {
    const id = reverse[i];
    if (prerequisites.has(id)) continue;
    prerequisites.add(id);
    for (const edge of incoming.get(id) ?? []) reverse.push(edge.source);
  }
  const queue = [...options.pinnedNodeIDs, ...prerequisites];
  const visitedGroups = new Set<string>();
  for (let i = 0; i < queue.length; i++) {
    const id = queue[i];
    if (forced.has(id)) continue;
    forced.add(id);
    const node = nodes.get(id);
    if (!node) continue;
    const groupID = String(node.data.group_id ?? '');
    if (groupID && !visitedGroups.has(groupID)) {
      visitedGroups.add(groupID); expandedGroups.add(groupID);
      const parent = groups.get(groupID)?.parent_node_id;
      if (parent) queue.push(parent);
    }
    if (node.parentNode) {
      const parentGroup = groups.get(node.parentNode);
      if (parentGroup) {
        expandedGroups.add(parentGroup.id);
        if (parentGroup.parent_node_id) queue.push(parentGroup.parent_node_id);
      } else queue.push(node.parentNode);
    }
    const include = frames.get(String(node.data.frame_id ?? ''))?.parent_include_node_id;
    if (include) queue.push(include);
  }
  // Issue context must use an honest full structural view, never a fabricated cross-route path.
  const routeIDs = new Set(routeDocument.nodes.map(node => node.id));
  const source = [...forced].some(id => nodes.has(id) && !routeIDs.has(id)) ? structuralDocument : routeDocument;
  const owners = new Set(structuralDocument.groups.map(group => group.parent_node_id));
  for (const frame of structuralDocument.frames) if (frame.parent_include_node_id) owners.add(frame.parent_include_node_id);
  const eligible = new Set(source.nodes.filter(node =>
    options.mode === 'workflow' && (node.data.kind === 'noop' || node.data.kind === 'assert' || node.data.kind === 'assign') &&
    node.data.synthetic !== true && !owners.has(node.id) && !forced.has(node.id) &&
    !options.expandedNodeIDs.has(node.id)).map(node => node.id));
  const ids = [...nodes.keys()].sort();
  const cyclic = eligible.size ? cycleMembers(ids, outgoing, incoming) : new Set<string>();
  const sameScope = (a: string, b: string) => ['frame_id', 'group_id', 'segment_id'].every(
    field => (nodes.get(a)?.data[field] ?? '') === (nodes.get(b)?.data[field] ?? ''));
  const routeEdges = new Set(source.edges.map(edge => edge.id));
  const next = new Map<string, GraphEdge>(), previous = new Set<string>();
  for (const id of eligible) {
    const edges = outgoing.get(id) ?? [];
    if (cyclic.has(id) || edges.length !== 1 || incoming.get(id)?.length !== 1) continue;
    const edge = edges[0], target = edge.target;
    if (!eligible.has(target) || cyclic.has(target) || !sameScope(id, target) ||
      outgoing.get(target)?.length !== 1 || incoming.get(target)?.length !== 1 ||
      edge.type !== 'sequence' || !!edge.label || !routeEdges.has(edge.id) ||
      Object.keys(edge).some(key => (key === 'routeKind' || key.startsWith('runtime')) && edge[key as keyof GraphEdge] !== undefined)) continue;
    next.set(id, edge); previous.add(target);
  }
  const reserved = new Set([
    ...structuralDocument.nodes.map(node => node.id), ...structuralDocument.groups.map(group => group.id),
    ...structuralDocument.edges.map(edge => edge.id), ...source.nodes.map(node => node.id),
    ...source.groups.map(group => group.id), ...source.edges.map(edge => edge.id),
  ]);
  const segments = new Map<string, TechnicalSegment>(), representatives = new Map<string, string>();
  const internal = new Set<string>();
  for (const node of source.nodes) representatives.set(node.id, node.id);
  for (const first of [...eligible].sort()) {
    if (previous.has(first)) continue;
    const members = [first], edges: string[] = [];
    let link = next.get(first);
    while (link) {
      members.push(link.target); edges.push(link.id); internal.add(link.id);
      link = next.get(link.target);
    }
    const base = '__yawr_workflow_v1__:' + encodeURIComponent(JSON.stringify(members));
    let id = base, suffix = 0;
    while (reserved.has(id)) id = `${base}:${++suffix}`;
    reserved.add(id);
    const data = nodes.get(first)!.data;
    segments.set(id, { id, memberNodeIDs: members, internalEdgeIDs: edges,
      groupID: String(data.group_id ?? ''), frameID: String(data.frame_id ?? ''),
      ...(typeof data.segment_id === 'string' ? { segmentID: data.segment_id } : {}) });
    for (const member of members) representatives.set(member, id);
  }
  const provenance = new Map<string, ProjectedEdgeProvenance>();
  const projectedEdges = source.edges.filter(edge => !internal.has(edge.id)).map(edge => {
    provenance.set(edge.id, { originalEdgeID: edge.id, originalSource: edge.source, originalTarget: edge.target });
    return { ...edge, source: representatives.get(edge.source) ?? edge.source,
      target: representatives.get(edge.target) ?? edge.target, runtimeNodeID: edge.runtimeNodeID ?? edge.target };
  });
  return {
    document: { ...source, nodes: [
      ...source.nodes.filter(node => !eligible.has(node.id)),
      ...[...segments.values()].map(segment => ({
        id: segment.id, type: 'technicalSegment', position: { x: 0, y: 0 },
        data: { id: segment.id, kind: 'technical-segment', synthetic: true,
          title: `${segment.memberNodeIDs.length} technical ${segment.memberNodeIDs.length === 1 ? 'step' : 'steps'}`,
          group_id: segment.groupID, frame_id: segment.frameID, segment_id: segment.segmentID },
      })),
    ], edges: projectedEdges },
    segments, representativeByNodeID: representatives, edgeProvenance: provenance,
    forcedNodeIDs: forced, expandedGroupIDs: expandedGroups,
  };
}
