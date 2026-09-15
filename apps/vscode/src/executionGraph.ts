import type { GraphDocument, GraphNode } from './directGraphPreview';
import { graphExecutionNodeID, type ProgressNode } from './executionProgress';

// Dynamic occurrence identities are allocated by the runtime's frozen graph
// builder. Keep the preview's static identities, so an existing paced head
// remains valid when new runbooks appear.
export function mergeExecutionGraph(current: GraphDocument, incoming: GraphDocument, nodeIDs: readonly string[]): GraphDocument {
  if (current.runbook.id !== incoming.runbook.id || current.runbook.path !== incoming.runbook.path) {
    throw new Error('execution graph belongs to a different runbook');
  }
  const dynamicIDs = new Set(nodeIDs);
  const resolve = (id: string) => dynamicIDs.has(id) ? id : graphExecutionNodeID(current, id) ?? id;
  const nodes: GraphNode[] = incoming.nodes.filter(node => dynamicIDs.has(node.id)).map(node => ({
    ...node, data: { ...node.data, execution_occurrence: true },
  }));
  const frameIDs = new Set(nodes.map(node => String(node.data.frame_id)));
  const groupIDs = new Set(incoming.groups.filter(group => frameIDs.has(group.frame_id)).map(group => group.id));
  const merge = <T extends { id: string }>(prior: T[], next: T[]) =>
    [...new Map([...prior, ...next].map(item => [item.id, item])).values()];
  const document: GraphDocument = {
    ...current,
    schema_version: incoming.schema_version === '3' ? '3' : current.schema_version,
    hash: incoming.hash,
    nodes: merge(current.nodes, nodes),
    frames: merge(current.frames, incoming.frames.filter(frame => frameIDs.has(frame.id)).map(frame => ({
      ...frame, parent_include_node_id: frame.parent_include_node_id ? resolve(frame.parent_include_node_id) : undefined,
    }))),
    groups: merge(current.groups, incoming.groups.filter(group => groupIDs.has(group.id)).map(group => ({
      ...group, parent_node_id: resolve(group.parent_node_id),
    }))),
    edges: merge(current.edges, incoming.edges.filter(edge => dynamicIDs.has(edge.source) || dynamicIDs.has(edge.target)).map(edge => ({
      ...edge, id: `execution:${edge.source}:${edge.target}:${edge.type ?? ''}`,
      source: resolve(edge.source), target: resolve(edge.target),
    }))),
  };
  const ids = new Set(document.nodes.map(node => node.id));
  if (document.edges.some(edge => !ids.has(edge.source) || !ids.has(edge.target)) ||
      document.groups.some(group => !ids.has(group.parent_node_id))) {
    throw new Error('execution graph references an unavailable parent');
  }
  return document;
}

export function executionHistory(document: GraphDocument, runtime: Readonly<Record<string, ProgressNode>>) {
  return Object.entries(runtime).flatMap(([id, state]) => {
    const nodeID = graphExecutionNodeID(document, id);
    if (!nodeID) return [];
    return (state.occurrences ?? [state]).flatMap(occurrence =>
      occurrence.startedEventSequence === undefined ? [] : [{
        nodeID, sequence: occurrence.startedEventSequence,
        occurrenceID: occurrence.occurrenceID, path: occurrence.qualifiedNodeID ?? id,
        status: occurrence.status,
        startedAt: occurrence.startedAt, finishedAt: occurrence.finishedAt,
      }]);
  }).sort((left, right) => left.sequence - right.sequence);
}

export function executionReturnEdges(document: GraphDocument, runtime: Readonly<Record<string, ProgressNode>>) {
  const nodes = new Map(document.nodes.map(node => [node.id, node]));
  const frames = new Map(document.frames.map(frame => [frame.id, frame]));
  const history = executionHistory(document, runtime);
  const edges = new Map<string, { id: string; source: string; target: string; label: string }>();
  for (let index = 1; index < history.length; index++) {
    const source = history[index - 1], target = history[index];
    const finished = Date.parse(source.finishedAt ?? ''), started = Date.parse(target.startedAt ?? '');
    if (!Number.isFinite(finished) || !Number.isFinite(started) || finished > started) continue;
    let frameID = String(nodes.get(source.nodeID)?.data.frame_id ?? '');
    const targetFrame = String(nodes.get(target.nodeID)?.data.frame_id ?? '');
    if (!frameID || !targetFrame || frameID === targetFrame) continue;
    const seen = new Set<string>();
    while (frameID && !seen.has(frameID)) {
      seen.add(frameID);
      const parent = frames.get(frameID)?.parent_include_node_id;
      frameID = parent ? String(nodes.get(parent)?.data.frame_id ?? '') : '';
      if (frameID !== targetFrame) continue;
      const id = `execution-return:${source.nodeID}:${target.nodeID}`;
      edges.set(id, { id, source: source.nodeID, target: target.nodeID, label: 'Return' });
      break;
    }
  }
  return [...edges.values()];
}

interface PositionedNode {
  id: string; position: { x: number; y: number }; parentNode?: string;
}

export function anchorExecutionLayout<T extends PositionedNode>(previous: readonly T[], next: T[], currentID?: string): T[] {
  const absolute = (nodes: readonly T[], id: string | undefined) => {
    const byID = new Map(nodes.map(node => [node.id, node]));
    const seen = new Set<string>();
    let node = id ? byID.get(id) : undefined;
    if (!node) return undefined;
    let x = 0, y = 0;
    while (node && !seen.has(node.id)) {
      seen.add(node.id); x += node.position.x; y += node.position.y;
      node = node.parentNode ? byID.get(node.parentNode) : undefined;
    }
    return { x, y };
  };
  const before = absolute(previous, currentID), after = absolute(next, currentID);
  if (!before || !after) return next;
  return next.map(node => node.parentNode ? node : {
    ...node, position: { x: node.position.x + before.x - after.x, y: node.position.y + before.y - after.y },
  });
}
