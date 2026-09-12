import type { GraphDocument, GraphEdge, GraphGroup, GraphNode } from './directGraphPreview';

export interface BranchRuntimeState {
  status: string;
  output?: Record<string, unknown>;
}

interface BranchExit {
  sourceID: string;
  label?: string;
  routeKind?: GraphEdge['routeKind'];
  runtimeNodeID: string;
  runtimeExpectedStatus?: string;
  runtimeArmIndex?: number;
  runtimeArmLabel?: string;
  runtimeFallbackNodeID?: string;
}

function nodeKind(node: GraphNode | undefined): string {
  return typeof node?.data.kind === 'string' ? node.data.kind : '';
}

function nodeOrder(node: GraphNode | undefined): number {
  return typeof node?.data.order === 'number' ? node.data.order : 0;
}

function nodeGroupID(node: GraphNode | undefined): string {
  return typeof node?.data.group_id === 'string' ? node.data.group_id : '';
}

function nodeFrameID(node: GraphNode | undefined): string {
  return typeof node?.data.frame_id === 'string' ? node.data.frame_id : '';
}

function scopeKey(node: GraphNode): string {
  return `${nodeFrameID(node)}\u0000${nodeGroupID(node)}`;
}

function routeLabel(group: GraphGroup): string {
  if (group.fallback) return group.label ? `Otherwise - ${group.label}` : 'Otherwise';
  return group.label || 'Empty matched arm';
}

function routeIndex(group: GraphGroup): number {
  return typeof group.index === 'number' ? group.index : 0;
}

function uniqueRouteLabel(groups: GraphGroup[], group: GraphGroup): string {
  if (!group.label) return '';
  return groups.filter((candidate) => candidate.label === group.label).length === 1 ? group.label : '';
}

function allocateID(used: Set<string>, prefix: string): string {
  let id = prefix;
  let suffix = 1;
  while (used.has(id)) id = `${prefix}:${suffix++}`;
  used.add(id);
  return id;
}

export function withBranchMerges(document: GraphDocument): GraphDocument {
  if (document.nodes.some((node) => node.data.synthetic === true && typeof node.data.merge_for === 'string')) {
    return document;
  }
  const originalNodes = document.nodes.map((node) => ({ ...node, data: { ...node.data } }));
  const originalEdges = document.edges.map((edge) => ({ ...edge }));
  const nodeByID = new Map(originalNodes.map((node) => [node.id, node]));
  const armsByBranch = new Map<string, GraphGroup[]>();
  const childGroupsByParent = new Map<string, GraphGroup[]>();
  const nodesByGroup = new Map<string, GraphNode[]>();
  const nodesByScope = new Map<string, GraphNode[]>();

  for (const group of document.groups) {
    if (!group.parent_node_id) continue;
    const children = childGroupsByParent.get(group.parent_node_id) ?? [];
    children.push(group);
    childGroupsByParent.set(group.parent_node_id, children);
    if (group.kind === 'branch-arm') {
      const arms = armsByBranch.get(group.parent_node_id) ?? [];
      arms.push(group);
      armsByBranch.set(group.parent_node_id, arms);
    }
  }
  if (armsByBranch.size === 0) return document;
  for (const arms of armsByBranch.values()) {
    arms.sort((left, right) => routeIndex(left) - routeIndex(right) || left.id.localeCompare(right.id));
  }
  for (const node of originalNodes) {
    const groupID = nodeGroupID(node);
    if (groupID) {
      const members = nodesByGroup.get(groupID) ?? [];
      members.push(node);
      nodesByGroup.set(groupID, members);
    }
    const scoped = nodesByScope.get(scopeKey(node)) ?? [];
    scoped.push(node);
    nodesByScope.set(scopeKey(node), scoped);
  }
  for (const members of [...nodesByGroup.values(), ...nodesByScope.values()]) {
    members.sort((left, right) => nodeOrder(left) - nodeOrder(right));
  }

  const directNodes = (group: GraphGroup): GraphNode[] => nodesByGroup.get(group.id) ?? [];
  const pruned = new Set<string>();
  const markSubtree = (nodeID: string): void => {
    if (!nodeID || pruned.has(nodeID)) return;
    pruned.add(nodeID);
    for (const group of childGroupsByParent.get(nodeID) ?? []) {
      for (const child of directNodes(group)) markSubtree(child.id);
    }
  };
  const markFollowingSiblings = (branchID: string): void => {
    const branch = nodeByID.get(branchID);
    if (!branch) return;
    const members = nodesByScope.get(scopeKey(branch)) ?? [];
    const index = members.findIndex((node) => node.id === branchID);
    for (const sibling of members.slice(index + 1)) markSubtree(sibling.id);
  };

  const branchMemo = new Map<string, BranchExit[]>();
  const analyzeNode = (node: GraphNode | undefined, visiting: ReadonlySet<string>): BranchExit[] => {
    if (!node || nodeKind(node) === 'end') return [];
    if (nodeKind(node) === 'branch') return analyzeBranch(node.id, visiting);
    return [{ sourceID: node.id, runtimeNodeID: node.id }];
  };
  const analyzeSequence = (members: GraphNode[], visiting: ReadonlySet<string>): BranchExit[] => {
    if (members.length === 0) return [];
    for (let index = 0; index < members.length; index += 1) {
      const exits = analyzeNode(members[index], visiting);
      if (index === members.length - 1) return exits;
      if (exits.length === 0) {
        for (const unreachable of members.slice(index + 1)) markSubtree(unreachable.id);
        return [];
      }
    }
    return [];
  };
  const analyzeBranch = (branchID: string, visiting: ReadonlySet<string> = new Set()): BranchExit[] => {
    const cached = branchMemo.get(branchID);
    if (cached) return cached;
    if (visiting.has(branchID)) return [{ sourceID: branchID, runtimeNodeID: branchID }];
    const nextVisiting = new Set(visiting);
    nextVisiting.add(branchID);
    const arms = armsByBranch.get(branchID) ?? [];
    const exits: BranchExit[] = [];
    for (const arm of arms) {
      const members = directNodes(arm);
      const route: Omit<BranchExit, 'sourceID'> = {
        routeKind: members.length === 0 ? 'empty-arm' : 'arm-exit',
        runtimeNodeID: branchID,
        runtimeExpectedStatus: 'completed',
        runtimeArmIndex: routeIndex(arm),
        runtimeArmLabel: uniqueRouteLabel(arms, arm),
      };
      if (members.length === 0) {
        exits.push({ sourceID: branchID, label: routeLabel(arm), ...route });
      } else {
        exits.push(...analyzeSequence(members, nextVisiting).map((exit) => ({
          ...exit,
          ...route,
          runtimeFallbackNodeID: exit.sourceID,
        })));
      }
    }
    if (!arms.some((arm) => arm.fallback)) {
      exits.push({
        sourceID: branchID,
        label: 'No matching arm',
        routeKind: 'no-match',
        runtimeNodeID: branchID,
        runtimeExpectedStatus: 'skipped',
      });
    }
    branchMemo.set(branchID, exits);
    return exits;
  };

  for (const [branchID, arms] of armsByBranch) {
    for (const arm of arms) {
      const members = directNodes(arm);
      if (members.length === 0) continue;
      const entry = originalEdges.find((edge) => edge.source === branchID && edge.target === members[0].id && edge.type === 'branch-arm');
      if (!entry) continue;
      entry.routeKind = 'arm-entry';
      entry.runtimeNodeID = branchID;
      entry.runtimeExpectedStatus = 'completed';
      entry.runtimeArmIndex = routeIndex(arm);
      entry.runtimeArmLabel = uniqueRouteLabel(arms, arm);
      entry.runtimeFallbackNodeID = members[0].id;
    }
    analyzeBranch(branchID);
  }
  for (const branchID of armsByBranch.keys()) {
    const hasContinuation = originalEdges.some((edge) => edge.source === branchID && edge.type === 'sequence');
    if (hasContinuation && (branchMemo.get(branchID) ?? []).length === 0) markFollowingSiblings(branchID);
  }

  const nodes = originalNodes.filter((node) => !pruned.has(node.id));
  const edges = originalEdges.filter((edge) => !pruned.has(edge.source) && !pruned.has(edge.target));
  const usedNodeIDs = new Set(nodes.map((node) => node.id));
  const usedEdgeIDs = new Set(edges.map((edge) => edge.id));
  for (const branchID of armsByBranch.keys()) {
    const continuationIndex = edges.findIndex((edge) => edge.source === branchID && edge.type === 'sequence');
    if (continuationIndex < 0) continue;
    const [continuation] = edges.splice(continuationIndex, 1);
    const uniqueExits: BranchExit[] = [];
    const seenExits = new Set<string>();
    for (const exit of branchMemo.get(branchID) ?? []) {
      if (pruned.has(exit.sourceID)) continue;
      const key = `${exit.sourceID}\u0000${exit.label ?? ''}\u0000${exit.routeKind ?? ''}\u0000${exit.runtimeArmIndex ?? ''}`;
      if (seenExits.has(key)) continue;
      seenExits.add(key);
      uniqueExits.push(exit);
    }
    if (uniqueExits.length === 0) continue;

    const branch = nodeByID.get(branchID);
    const continuationNode = nodeByID.get(continuation.target);
    const mergeID = allocateID(usedNodeIDs, `__yawr_preview_merge__:${branchID}`);
    nodes.push({
      id: mergeID,
      type: 'branchMerge',
      data: {
        id: mergeID,
        kind: 'merge',
        title: 'Branch merge',
        group_id: nodeGroupID(branch),
        frame_id: nodeFrameID(branch),
        order: nodeOrder(continuationNode) - 0.5,
        synthetic: true,
        merge_for: branchID,
        ...(typeof branch?.data.segment_id === 'string' ? { segment_id: branch.data.segment_id } : {}),
      },
      position: { x: 0, y: 0 },
    });
    for (const exit of uniqueExits) {
      edges.push({
        id: allocateID(usedEdgeIDs, `__yawr_preview_edge__:${exit.sourceID}->${mergeID}:${exit.routeKind ?? 'route'}:${exit.runtimeArmIndex ?? ''}`),
        source: exit.sourceID,
        target: mergeID,
        type: 'branch-merge',
        label: exit.label ?? '',
        routeKind: exit.routeKind,
        runtimeNodeID: exit.runtimeNodeID,
        runtimeExpectedStatus: exit.runtimeExpectedStatus,
        runtimeArmIndex: exit.runtimeArmIndex,
        runtimeArmLabel: exit.runtimeArmLabel,
        runtimeFallbackNodeID: exit.runtimeFallbackNodeID,
      });
    }
    edges.push({ ...continuation, source: mergeID, runtimeNodeID: continuation.target });
  }

  return { ...document, nodes, edges };
}

export function branchArmRouteMatches(edge: GraphEdge, state: BranchRuntimeState | undefined): boolean | undefined {
  if (edge.runtimeArmIndex === undefined) return true;
  const output = state?.output ?? {};
  if (output.matched_arm_index !== undefined && output.matched_arm_index !== null) {
    return Number(output.matched_arm_index) === edge.runtimeArmIndex;
  }
  if (edge.runtimeArmLabel && output.matched_arm !== undefined) {
    return output.matched_arm === edge.runtimeArmLabel;
  }
  return undefined;
}

export function edgeRuntimeState(
  edge: GraphEdge,
  runtimeNodes: Readonly<Record<string, BranchRuntimeState>>,
): BranchRuntimeState | undefined {
  const state = runtimeNodes[edge.runtimeNodeID ?? edge.target];
  if (!state) return undefined;
  if (edge.runtimeExpectedStatus && state.status !== edge.runtimeExpectedStatus) {
    if (edge.routeKind !== 'arm-entry' || !['running', 'failed', 'indeterminate'].includes(state.status)) return undefined;
  }
  const armMatch = branchArmRouteMatches(edge, state);
  if (armMatch === false) return undefined;
  if (armMatch === undefined) {
    const fallbackState = edge.runtimeFallbackNodeID ? runtimeNodes[edge.runtimeFallbackNodeID] : undefined;
    if (!fallbackState || fallbackState.status === 'pending' || fallbackState.status === 'skipped') return undefined;
    return fallbackState;
  }
  if (edge.routeKind === 'no-match' && state.status === 'skipped') return { ...state, status: 'completed' };
  return state;
}

export function activeGraphNodeIDs(
  document: GraphDocument,
  runtimeNodes: Readonly<Record<string, BranchRuntimeState>>,
): Set<string> {
  const graph = withBranchMerges(document);
  const nodeByID = new Map(graph.nodes.map((node) => [node.id, node]));
  const outgoing = new Map<string, GraphEdge[]>();
  const incoming = new Map<string, number>();
  for (const node of graph.nodes) {
    outgoing.set(node.id, []);
    incoming.set(node.id, 0);
  }
  for (const edge of graph.edges) {
    if (!outgoing.has(edge.source) || !incoming.has(edge.target)) continue;
    outgoing.get(edge.source)?.push(edge);
    incoming.set(edge.target, (incoming.get(edge.target) ?? 0) + 1);
  }
  const executed = (nodeID: string): boolean => {
    const status = runtimeNodes[nodeID]?.status;
    return !!status && status !== 'pending' && status !== 'skipped';
  };
  const liveMemo = new Map<string, boolean>();
  const visiting = new Set<string>();
  const routeActive = (edge: GraphEdge): boolean => edgeRuntimeState(edge, runtimeNodes) !== undefined;
  const routeResolved = (state: BranchRuntimeState | undefined): boolean => (
    !!state && !['pending', 'running', 'delaying', 'waiting'].includes(state.status)
  );
  const live = (nodeID: string): boolean => {
    const cached = liveMemo.get(nodeID);
    if (cached !== undefined) return cached;
    if (visiting.has(nodeID)) return false;
    visiting.add(nodeID);
    let result = executed(nodeID) || (outgoing.get(nodeID) ?? []).some((edge) => edge.routeKind && routeActive(edge));
    if (!result) {
      result = (outgoing.get(nodeID) ?? []).some((edge) => edge.type !== 'branch-merge' && live(edge.target));
    }
    visiting.delete(nodeID);
    liveMemo.set(nodeID, result);
    return result;
  };
  const edgeActive = (edge: GraphEdge): boolean => {
    if (edge.type === 'branch-merge') {
      if (!edge.routeKind) return executed(edge.runtimeNodeID ?? edge.source);
      if (!routeResolved(runtimeNodes[edge.runtimeNodeID ?? edge.source])) return true;
      return routeActive(edge);
    }
    if (nodeKind(nodeByID.get(edge.source)) === 'branch' && routeResolved(runtimeNodes[edge.source])) return live(edge.target);
    return true;
  };

  const visible = new Set<string>();
  const queue: string[] = [];
  for (const node of graph.nodes) {
    if ((incoming.get(node.id) ?? 0) === 0) {
      visible.add(node.id);
      queue.push(node.id);
    }
  }
  for (let index = 0; index < queue.length; index += 1) {
    for (const edge of outgoing.get(queue[index]) ?? []) {
      if (!edgeActive(edge) || visible.has(edge.target)) continue;
      visible.add(edge.target);
      queue.push(edge.target);
    }
  }
  return new Set(graph.nodes.filter((node) => node.data.synthetic !== true && visible.has(node.id)).map((node) => node.id));
}