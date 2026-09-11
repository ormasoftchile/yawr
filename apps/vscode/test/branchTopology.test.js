'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const { activeGraphNodeIDs, withBranchMerges } = require('../out/branchTopology');

function node(id, kind, groupID, order) {
  return { id, data: { id, kind, group_id: groupID || '', frame_id: 'frame:root', order }, position: { x: 0, y: 0 } };
}

function edge(id, source, target, type, label = '') {
  return { id, source, target, type, label };
}

function document(nodes, groups, edges) {
  return { schema_version: '1', runbook: { id: 'test', name: 'Test' }, frames: [], nodes, groups, edges };
}

function mergeID(graph, branchID) {
  return graph.nodes.find((candidate) => candidate.data.synthetic === true && candidate.data.merge_for === branchID)?.id;
}

test('branch topology merges selected routes and excludes fallback continuations', () => {
  const source = document(
    [node('branch', 'branch', '', 0), node('success', 'display', 'g-success', 1), node('blocked', 'end', 'g-blocked', 2), node('done', 'end', '', 3)],
    [
      { id: 'g-success', kind: 'branch-arm', parent_node_id: 'branch', frame_id: 'frame:root', index: 0 },
      { id: 'g-blocked', kind: 'branch-arm', parent_node_id: 'branch', frame_id: 'frame:root', index: 1, fallback: true },
    ],
    [edge('e0', 'branch', 'success', 'branch-arm'), edge('e1', 'branch', 'blocked', 'branch-arm'), edge('e2', 'branch', 'done', 'sequence')],
  );

  const graph = withBranchMerges(source);
  const merge = mergeID(graph, 'branch');
  assert.ok(merge);
  assert.ok(graph.edges.some((candidate) => candidate.source === 'success' && candidate.target === merge));
  assert.ok(graph.edges.some((candidate) => candidate.source === merge && candidate.target === 'done'));
  assert.ok(!graph.edges.some((candidate) => candidate.source === 'blocked' && candidate.target === merge));
  assert.ok(!graph.edges.some((candidate) => candidate.source === 'branch' && candidate.target === 'done'));

  assert.deepEqual(
    [...activeGraphNodeIDs(source, {})].sort(),
    ['blocked', 'branch', 'done', 'success'],
    'all potential routes must remain active until the branch outcome is known',
  );

  const selected = activeGraphNodeIDs(source, {
    branch: { status: 'completed', output: { matched_arm_index: 0 } },
    success: { status: 'completed' },
    done: { status: 'completed' },
  });
  assert.deepEqual([...selected].sort(), ['branch', 'done', 'success']);

  const fallback = activeGraphNodeIDs(source, {
    branch: { status: 'completed', output: { matched_arm_index: 1 } },
    blocked: { status: 'completed' },
  });
  assert.deepEqual([...fallback].sort(), ['blocked', 'branch']);
});

test('branch topology models empty and no-match routes', () => {
  const emptySource = document(
    [node('branch', 'branch', '', 0), node('blocked', 'end', 'g-blocked', 1), node('next', 'display', '', 2)],
    [
      { id: 'g-empty', kind: 'branch-arm', parent_node_id: 'branch', frame_id: 'frame:root', label: 'Empty route', index: 0 },
      { id: 'g-blocked', kind: 'branch-arm', parent_node_id: 'branch', frame_id: 'frame:root', index: 1, fallback: true },
    ],
    [edge('e0', 'branch', 'blocked', 'branch-arm'), edge('e1', 'branch', 'next', 'sequence')],
  );
  const emptyGraph = withBranchMerges(emptySource);
  const emptyMerge = mergeID(emptyGraph, 'branch');
  assert.ok(emptyGraph.edges.some((candidate) => candidate.source === 'branch' && candidate.target === emptyMerge && candidate.routeKind === 'empty-arm'));
  assert.deepEqual([...activeGraphNodeIDs(emptySource, {
    branch: { status: 'completed', output: { matched_arm_index: 0 } },
    next: { status: 'completed' },
  })].sort(), ['branch', 'next']);

  const noMatchSource = document(
    [node('branch', 'branch', '', 0), node('arm-end', 'end', 'g-arm', 1), node('next', 'display', '', 2)],
    [{ id: 'g-arm', kind: 'branch-arm', parent_node_id: 'branch', frame_id: 'frame:root', index: 0 }],
    [edge('e0', 'branch', 'arm-end', 'branch-arm'), edge('e1', 'branch', 'next', 'sequence')],
  );
  assert.deepEqual([...activeGraphNodeIDs(noMatchSource, {
    branch: { status: 'skipped' },
    next: { status: 'completed' },
  })].sort(), ['branch', 'next']);
});

test('branch topology prunes terminal paths and merges nested arm exits', () => {
  const terminal = withBranchMerges(document(
    [node('branch', 'branch', '', 0), node('end-a', 'end', 'g-a', 1), node('end-b', 'end', 'g-b', 2), node('unreachable', 'display', '', 3)],
    [
      { id: 'g-a', kind: 'branch-arm', parent_node_id: 'branch', frame_id: 'frame:root', index: 0 },
      { id: 'g-b', kind: 'branch-arm', parent_node_id: 'branch', frame_id: 'frame:root', index: 1, fallback: true },
    ],
    [edge('e0', 'branch', 'end-a', 'branch-arm'), edge('e1', 'branch', 'end-b', 'branch-arm'), edge('e2', 'branch', 'unreachable', 'sequence')],
  ));
  assert.equal(mergeID(terminal, 'branch'), undefined);
  assert.ok(!terminal.nodes.some((candidate) => candidate.id === 'unreachable'));

  const nested = withBranchMerges(document(
    [
      node('outer', 'branch', '', 0), node('inner', 'branch', 'g-outer', 1),
      node('inner-a', 'display', 'g-inner-a', 2), node('inner-b', 'display', 'g-inner-b', 3),
      node('outer-end', 'end', 'g-outer-fallback', 4), node('next', 'display', '', 5),
    ],
    [
      { id: 'g-outer', kind: 'branch-arm', parent_node_id: 'outer', frame_id: 'frame:root', index: 0 },
      { id: 'g-outer-fallback', kind: 'branch-arm', parent_node_id: 'outer', frame_id: 'frame:root', index: 1, fallback: true },
      { id: 'g-inner-a', kind: 'branch-arm', parent_node_id: 'inner', frame_id: 'frame:root', index: 0 },
      { id: 'g-inner-b', kind: 'branch-arm', parent_node_id: 'inner', frame_id: 'frame:root', index: 1, fallback: true },
    ],
    [
      edge('e0', 'outer', 'inner', 'branch-arm'), edge('e1', 'inner', 'inner-a', 'branch-arm'),
      edge('e2', 'inner', 'inner-b', 'branch-arm'), edge('e3', 'outer', 'outer-end', 'branch-arm'),
      edge('e4', 'outer', 'next', 'sequence'),
    ],
  ));
  const outerMerge = mergeID(nested, 'outer');
  assert.ok(nested.edges.some((candidate) => candidate.source === 'inner-a' && candidate.target === outerMerge));
  assert.ok(nested.edges.some((candidate) => candidate.source === 'inner-b' && candidate.target === outerMerge));
  assert.ok(!nested.edges.some((candidate) => candidate.source === 'inner' && candidate.target === outerMerge));
});