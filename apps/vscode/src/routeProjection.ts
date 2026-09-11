import type { GraphDocument, GraphGroup, GraphNode } from './directGraphPreview';
import { edgeRuntimeState, type BranchRuntimeState } from './branchTopology';

export type RouteProjectionScope = 'through' | 'to' | 'from';

export interface RouteBoundaryEdge {
  edgeID: string;
  visibleNodeID: string;
  hiddenNodeID: string;
  direction: 'incoming' | 'outgoing';
}

export interface RouteProjection {
  targetID: string;
  scope: RouteProjectionScope;
  nodeIDs: ReadonlySet<string>;
  edgeIDs: ReadonlySet<string>;
  predecessorCount: number;
  successorCount: number;
  hiddenNodeCount: number;
  boundaryEdges: RouteBoundaryEdge[];
}

export interface RouteProjectionIndex {
  knownNodeIDs: ReadonlySet<string>;
  outgoing: ReadonlyMap<string, readonly string[]>;
  incoming: ReadonlyMap<string, readonly string[]>;
  nodeByID: ReadonlyMap<string, GraphNode>;
  groupByID: ReadonlyMap<string, GraphGroup>;
}

export function buildRouteProjectionIndex(document: GraphDocument): RouteProjectionIndex {
  const knownNodeIDs = new Set(document.nodes.map((node) => node.id));
  const outgoing = new Map<string, string[]>();
  const incoming = new Map<string, string[]>();
  for (const edge of document.edges) {
    if (!knownNodeIDs.has(edge.source) || !knownNodeIDs.has(edge.target)) continue;
    outgoing.set(edge.source, [...(outgoing.get(edge.source) ?? []), edge.target]);
    incoming.set(edge.target, [...(incoming.get(edge.target) ?? []), edge.source]);
  }
  return {
    knownNodeIDs,
    outgoing,
    incoming,
    nodeByID: new Map(document.nodes.map((node) => [node.id, node])),
    groupByID: new Map(document.groups.map((group) => [group.id, group])),
  };
}

function reachable(startID: string, adjacency: ReadonlyMap<string, readonly string[]>): Set<string> {
  const visited = new Set<string>();
  const pending = [...(adjacency.get(startID) ?? [])];
  while (pending.length > 0) {
    const nodeID = pending.pop()!;
    if (nodeID === startID || visited.has(nodeID)) continue;
    visited.add(nodeID);
    pending.push(...(adjacency.get(nodeID) ?? []));
  }
  return visited;
}

export function computeRouteProjection(
  document: GraphDocument,
  targetID: string,
  scope: RouteProjectionScope,
  index: RouteProjectionIndex = buildRouteProjectionIndex(document),
): RouteProjection | undefined {
  const { knownNodeIDs, outgoing, incoming, nodeByID, groupByID } = index;
  if (!knownNodeIDs.has(targetID)) return undefined;

  const predecessors = reachable(targetID, incoming);
  const successors = reachable(targetID, outgoing);
  const nodeIDs = new Set<string>([targetID]);
  if (scope !== 'from') predecessors.forEach((nodeID) => nodeIDs.add(nodeID));
  if (scope !== 'to') successors.forEach((nodeID) => nodeIDs.add(nodeID));

  const parallelBranchForNode = (nodeID: string): { ownerID: string; groupID: string } | undefined => {
    let groupID = typeof nodeByID.get(nodeID)?.data.group_id === 'string' ? String(nodeByID.get(nodeID)?.data.group_id) : '';
    const visited = new Set<string>();
    while (groupID && !visited.has(groupID)) {
      visited.add(groupID);
      const group = groupByID.get(groupID);
      if (!group) return undefined;
      if (group.kind === 'parallel-branch') return { ownerID: group.parent_node_id, groupID: group.id };
      const parent = nodeByID.get(group.parent_node_id);
      groupID = typeof parent?.data.group_id === 'string' ? parent.data.group_id : '';
    }
    return undefined;
  };
  const expandedParallelOwners = new Set<string>();
  while (true) {
    const parallelOwners = new Set<string>();
    for (const nodeID of nodeIDs) {
      if (nodeByID.get(nodeID)?.data.kind === 'parallel') parallelOwners.add(nodeID);
      const branch = parallelBranchForNode(nodeID);
      if (branch) parallelOwners.add(branch.ownerID);
    }
    const ownerID = [...parallelOwners].find((candidate) => !expandedParallelOwners.has(candidate));
    if (!ownerID) break;
    expandedParallelOwners.add(ownerID);
    nodeIDs.add(ownerID);
    const branchGroups = new Set(document.groups.filter((group) => group.kind === 'parallel-branch' && group.parent_node_id === ownerID).map((group) => group.id));
    for (const node of document.nodes) {
      const branch = parallelBranchForNode(node.id);
      if (branch?.ownerID === ownerID) nodeIDs.add(node.id);
    }
    for (const edge of document.edges) {
      if (edge.source !== ownerID) continue;
      const targetBranch = parallelBranchForNode(edge.target);
      if (targetBranch?.ownerID !== ownerID) nodeIDs.add(edge.target);
    }
    const joinSources = new Map<string, Set<string>>();
    for (const edge of document.edges) {
      const sourceBranch = parallelBranchForNode(edge.source);
      const targetBranch = parallelBranchForNode(edge.target);
      if (sourceBranch?.ownerID !== ownerID || targetBranch?.ownerID === ownerID) continue;
      const sources = joinSources.get(edge.target) ?? new Set<string>();
      sources.add(sourceBranch.groupID);
      joinSources.set(edge.target, sources);
    }
    for (const [joinID, sourceGroups] of joinSources) {
      if (sourceGroups.size === branchGroups.size && branchGroups.size > 0) nodeIDs.add(joinID);
    }
  }

  const edgeIDs = new Set<string>();
  const boundaryEdges: RouteBoundaryEdge[] = [];
  for (const edge of document.edges) {
    if (!knownNodeIDs.has(edge.source) || !knownNodeIDs.has(edge.target)) continue;
    const sourceVisible = nodeIDs.has(edge.source);
    const targetVisible = nodeIDs.has(edge.target);
    if (sourceVisible && targetVisible) {
      edgeIDs.add(edge.id);
    } else if (sourceVisible && !targetVisible) {
      boundaryEdges.push({
        edgeID: edge.id,
        visibleNodeID: edge.source,
        hiddenNodeID: edge.target,
        direction: 'outgoing',
      });
    } else if (!sourceVisible && targetVisible) {
      boundaryEdges.push({
        edgeID: edge.id,
        visibleNodeID: edge.target,
        hiddenNodeID: edge.source,
        direction: 'incoming',
      });
    }
  }

  return {
    targetID,
    scope,
    nodeIDs,
    edgeIDs,
    predecessorCount: [...predecessors].filter((nodeID) => nodeByID.get(nodeID)?.data.synthetic !== true).length,
    successorCount: [...successors].filter((nodeID) => nodeByID.get(nodeID)?.data.synthetic !== true).length,
    hiddenNodeCount: document.nodes.filter((node) => node.data.synthetic !== true && !nodeIDs.has(node.id)).length,
    boundaryEdges,
  };
}

export function projectRouteDocument(
  document: GraphDocument,
  projection: RouteProjection,
): GraphDocument {
  const nodes = document.nodes.filter((node) => projection.nodeIDs.has(node.id));
  const nodeByID = new Map(document.nodes.map((node) => [node.id, node]));
  const groupByID = new Map(document.groups.map((group) => [group.id, group]));
  const groupIDs = new Set<string>();
  const retainGroup = (groupID: string): void => {
    if (!groupID || groupIDs.has(groupID)) return;
    const group = groupByID.get(groupID);
    if (!group) return;
    groupIDs.add(groupID);
    const parentNode = nodeByID.get(group.parent_node_id);
    retainGroup(typeof parentNode?.data.group_id === 'string' ? parentNode.data.group_id : '');
  };
  for (const node of nodes) {
    retainGroup(typeof node.data.group_id === 'string' ? node.data.group_id : '');
  }

  const groups = document.groups.filter((group) => groupIDs.has(group.id));
  const frameIDs = new Set<string>();
  for (const node of nodes) {
    if (typeof node.data.frame_id === 'string' && node.data.frame_id) frameIDs.add(node.data.frame_id);
  }
  for (const group of groups) {
    if (group.frame_id) frameIDs.add(group.frame_id);
  }

  return {
    ...document,
    nodes,
    edges: document.edges.filter((edge) => projection.edgeIDs.has(edge.id)),
    groups,
    frames: document.frames.filter((frame) => frameIDs.has(frame.id)),
  };
}

export function computeSessionRouteProjection(
  document: GraphDocument,
  targetID: string,
  scope: RouteProjectionScope,
  index: RouteProjectionIndex = buildRouteProjectionIndex(document),
): RouteProjection | undefined {
  const projection = computeRouteProjection(document, targetID, scope, index);
  if (!projection) return undefined;
  const targetSegmentID = stringProperty(index.nodeByID.get(targetID)?.data.segment_id);
  if (!targetSegmentID) return projection;

  const nodeIDs = new Set(projection.nodeIDs);
  if (scope === 'from') {
    const prerequisites = computeRouteProjection(document, targetID, 'to', index);
    for (const nodeID of prerequisites?.nodeIDs ?? []) nodeIDs.add(nodeID);
  }

  const edgeIDs = new Set<string>();
  const boundaryEdges: RouteBoundaryEdge[] = [];
  for (const edge of document.edges) {
    const sourceVisible = nodeIDs.has(edge.source);
    const targetVisible = nodeIDs.has(edge.target);
    if (sourceVisible && targetVisible) {
      edgeIDs.add(edge.id);
    } else if (sourceVisible !== targetVisible) {
      boundaryEdges.push({
        edgeID: edge.id,
        visibleNodeID: sourceVisible ? edge.source : edge.target,
        hiddenNodeID: sourceVisible ? edge.target : edge.source,
        direction: sourceVisible ? 'outgoing' : 'incoming',
      });
    }
  }
  return {
    ...projection,
    nodeIDs,
    edgeIDs,
    hiddenNodeCount: document.nodes.filter((node) => node.data.synthetic !== true && !nodeIDs.has(node.id)).length,
    boundaryEdges,
  };
}

export function sessionRouteDocument(
  document: GraphDocument,
  runtimeNodes: Readonly<Record<string, SessionRouteRuntimeState>>,
  targetID?: string,
): GraphDocument {
  const segmentGroups = document.groups.filter((group) => group.kind === 'session-segment' && group.segment_id);
  const historicalSegments = new Set(segmentGroups.flatMap((group) => (
    group.kind === 'session-segment' &&
    ['handed_off', 'completed', 'failed', 'cancelled', 'indeterminate'].includes(group.segment_status ?? '') &&
    group.segment_id
      ? [group.segment_id]
      : []
  )));
  if (historicalSegments.size === 0) return document;

  const targetSegmentID = stringProperty(document.nodes.find((node) => node.id === targetID)?.data.segment_id);
  const targetOrdinal = segmentGroups.find((group) => group.segment_id === targetSegmentID)?.index;
  const segmentOrdinal = new Map(segmentGroups.flatMap((group) => (
    group.segment_id && typeof group.index === 'number' ? [[group.segment_id, group.index] as const] : []
  )));

  const transitionEndpoints = new Set<string>();
  for (const edge of document.edges) {
    if (edge.type !== 'session-transition') continue;
    const sourceSegment = stringProperty(document.nodes.find((node) => node.id === edge.source)?.data.segment_id);
    const destinationSegment = stringProperty(document.nodes.find((node) => node.id === edge.target)?.data.segment_id);
    if (targetOrdinal !== undefined &&
        ((segmentOrdinal.get(sourceSegment) ?? Infinity) > targetOrdinal ||
         (segmentOrdinal.get(destinationSegment) ?? Infinity) > targetOrdinal)) continue;
    transitionEndpoints.add(edge.source);
    transitionEndpoints.add(edge.target);
  }
  const nodeIDs = new Set(document.nodes.flatMap((node) => {
    const segmentID = stringProperty(node.data.segment_id);
    if (targetOrdinal !== undefined && (segmentOrdinal.get(segmentID) ?? Infinity) > targetOrdinal) return [];
    if (node.data.synthetic === true || node.data.kind === 'session-entry' || segmentID === targetSegmentID ||
      !historicalSegments.has(segmentID) || transitionEndpoints.has(node.id)) {
      return [node.id];
    }
    const status = runtimeNodes[node.id]?.status;
    if (status === 'skipped' && document.edges.some((edge) => (
      edge.routeKind === 'no-match' && edge.runtimeNodeID === node.id && routeEdgeExecuted(edge, runtimeNodes)
    ))) return [node.id];
    return status && status !== 'pending' && status !== 'skipped' ? [node.id] : [];
  }));
  const executedPairs = new Set<string>();
  for (const [nodeID, state] of Object.entries(runtimeNodes)) {
    for (const occurrence of state.occurrences ?? []) {
      if (occurrence.predecessorNodeID) {
        executedPairs.add(`${occurrence.predecessorNodeID}\u0000${nodeID}`);
      }
    }
  }
  const nodeByID = new Map(document.nodes.map((node) => [node.id, node]));
  const edgeIDs = new Set<string>();
  const historicalEdge = (sourceID: string, targetIDValue: string): boolean => {
    const sourceSegment = stringProperty(nodeByID.get(sourceID)?.data.segment_id);
    const targetSegment = stringProperty(nodeByID.get(targetIDValue)?.data.segment_id);
    return historicalSegments.has(sourceSegment) && sourceSegment === targetSegment &&
      sourceSegment !== targetSegmentID;
  };
  for (const edge of document.edges) {
    if (!nodeIDs.has(edge.source) || !nodeIDs.has(edge.target)) continue;
    if (edge.type === 'session-transition' || !historicalEdge(edge.source, edge.target)) {
      edgeIDs.add(edge.id);
      continue;
    }
    if (edge.type === 'session-entry') {
      if (runtimeExecuted(runtimeNodes[edge.target])) edgeIDs.add(edge.id);
      continue;
    }
    if (nodeByID.get(edge.source)?.data.kind === 'parallel' && runtimeExecuted(runtimeNodes[edge.target])) {
      edgeIDs.add(edge.id);
      continue;
    }
    if (executedPairs.has(`${edge.source}\u0000${edge.target}`) ||
        routeEdgeExecuted(edge, runtimeNodes)) {
      edgeIDs.add(edge.id);
    }
  }
  let changed = true;
  while (changed) {
    changed = false;
    for (const edge of document.edges) {
      if (edgeIDs.has(edge.id) || !nodeIDs.has(edge.source) || !nodeIDs.has(edge.target) ||
          !historicalEdge(edge.source, edge.target)) continue;
      const source = nodeByID.get(edge.source);
      if (source?.data.synthetic !== true || !runtimeExecuted(runtimeNodes[edge.target])) continue;
      if (document.edges.some((incoming) => incoming.target === edge.source && edgeIDs.has(incoming.id))) {
        edgeIDs.add(edge.id);
        changed = true;
      }
    }
  }
  return projectRouteDocument(document, {
    targetID: '',
    scope: 'through',
    nodeIDs,
    edgeIDs,
    predecessorCount: 0,
    successorCount: 0,
    hiddenNodeCount: document.nodes.length - nodeIDs.size,
    boundaryEdges: [],
  });
}

function stringProperty(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

interface SessionRouteOccurrence extends BranchRuntimeState {
  predecessorNodeID?: string;
}

interface SessionRouteRuntimeState extends BranchRuntimeState {
  occurrences?: readonly SessionRouteOccurrence[];
}

function runtimeExecuted(state: SessionRouteRuntimeState | undefined): boolean {
  return !!state && state.status !== 'pending' && state.status !== 'skipped';
}

function routeEdgeExecuted(
  edge: GraphDocument['edges'][number],
  runtimeNodes: Readonly<Record<string, SessionRouteRuntimeState>>,
): boolean {
  if (!edge.routeKind && !edge.runtimeNodeID) return false;
  const runtimeNodeID = edge.runtimeNodeID ?? edge.target;
  const occurrences = runtimeNodes[runtimeNodeID]?.occurrences ?? [];
  if (occurrences.length === 0) return edgeRuntimeState(edge, runtimeNodes) !== undefined;
  return occurrences.some((occurrence) => edgeRuntimeState(edge, {
    ...runtimeNodes,
    [runtimeNodeID]: occurrence,
  }) !== undefined);
}