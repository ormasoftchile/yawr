package serve

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func previewHTMLForLifecycleTest(t *testing.T) string {
	t.Helper()
	data, err := staticFS.ReadFile("static/preview.html")
	if err != nil {
		t.Fatalf("read embedded preview.html: %v", err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

func sourceSection(t *testing.T, source, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatalf("missing source marker %q", startMarker)
	}
	end := strings.Index(source[start:], endMarker)
	if end < 0 {
		t.Fatalf("missing source marker %q after %q", endMarker, startMarker)
	}
	return source[start : start+end]
}

func TestPreviewStartRunDetachesOldStreamsBeforeClearingTerminalState(t *testing.T) {
	source := previewHTMLForLifecycleTest(t)
	startRun := sourceSection(t, source, "const startRun = useCallback", "// /runs/{id}/state SSE")

	detach := strings.Index(startRun, "setRunID('')")
	clearOverlay := strings.Index(startRun, "setStateOverlay(null)")
	if detach < 0 || clearOverlay < 0 || detach > clearOverlay {
		t.Fatalf("startRun must detach the old run before clearing its terminal overlay")
	}
	for _, required := range []string{
		"activeRunIDRef.current = ''",
		"activeRunIDRef.current = j.runID",
		"setIsStarting(true)",
		"setIsStarting(false)",
	} {
		if !strings.Contains(startRun, required) {
			t.Fatalf("startRun missing lifecycle guard %q", required)
		}
	}

	stateStream := sourceSection(t, source, "// /runs/{id}/state SSE", "// Forward a pending typed host action")
	for _, required := range []string{
		"const onState = (ev) => {\n      if (activeRunIDRef.current !== streamRunID) return;",
		"es.onopen = () => {\n      if (activeRunIDRef.current !== streamRunID) return;",
		"es.onerror = () => {\n      if (activeRunIDRef.current !== streamRunID) return;",
	} {
		if !strings.Contains(stateStream, required) {
			t.Fatalf("state stream lifecycle guard missing %q", required)
		}
	}
}

func TestPreviewDocumentHasNoContentAfterClosingHTML(t *testing.T) {
	source := strings.TrimSpace(previewHTMLForLifecycleTest(t))
	if !strings.HasSuffix(source, "</html>") || strings.Count(source, "</html>") != 1 {
		t.Fatalf("preview must contain exactly one closing document tag")
	}
}

func TestPreviewDecoratesExecutedPathEdgesFromCumulativeNodeState(t *testing.T) {
	source := previewHTMLForLifecycleTest(t)

	for _, required := range []string{
		"function edgeRuntimeClass(nodeState)",
		"edge-completed",
		"edge-running",
		"edge-failed",
		"function edgeRuntimeState(edge, overlayNodes)",
		"const targetState = edgeRuntimeState(ed, overlayNodes)",
		"const className = edgeRuntimeClass(targetState)",
		"return { ...ed, className",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("preview executed-path rendering missing %q", required)
		}
	}
	if !strings.Contains(source, ".react-flow__edge.edge-completed .react-flow__edge-path") {
		t.Fatal("completed path edges need an explicit persistent visual treatment")
	}
}

func TestPreviewBuildsNestedDebugBreakpointsAndRendersPauseControls(t *testing.T) {
	source := previewHTMLForLifecycleTest(t)
	for _, required := range []string{
		"function debugCallPathForNode(doc, nodeID)",
		"function debugTargetSupported(doc, nodeID)",
		"function toggleDebugBreakpoint(breakpoints, doc, nodeID, phase)",
		"function buildDebugRunConfig(breakpoints, profile, allowStaleProfile, watches)",
		"function shouldStartDebugRun(breakpoints, selectedProfileID)",
		"onClick: () => startRun(shouldStartDebugRun(breakpoints, selectedDebugProfileID))",
		"function parseDebugWatches(text)",
		"function debugProfileID(name)",
		"const [breakpoints, setBreakpoints]",
		"const [debugProfiles, setDebugProfiles]",
		"/debug-profiles?runbookPath=",
		"allowStaleProfile",
		"'Debug Run'",
		"pending.kind === 'debug_break'",
		"function DebugBreakForm({ pending, submit, saveDebugProfile })",
		"Continue unchanged",
		"Apply & Continue",
		"Step Into",
		"Step Over",
		"Step Out",
		"Actual result",
		"Effective result",
		"Save as debug profile",
		"Watch expressions",
		"debug.watches",
		"data.debugOverride = ns.debug_override",
		"Debug override",
		"Actual (debug)",
		"Effective (debug)",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("preview debugger UX missing %q", required)
		}
	}

	nodeExe := requireNode(t)
	pathFn := extractJSFunction(t, source, "debugCallPathForNode")
	toggleFn := extractJSFunction(t, source, "toggleDebugBreakpoint")
	configFn := extractJSFunction(t, source, "buildDebugRunConfig")
	startModeFn := extractJSFunction(t, source, "shouldStartDebugRun")
	idFn := extractJSFunction(t, source, "debugProfileID")
	watchFn := extractJSFunction(t, source, "parseDebugWatches")
	supportedFn := extractJSFunction(t, source, "debugTargetSupported")
	script := pathFn + "\n" + supportedFn + "\n" + toggleFn + "\n" + configFn + "\n" + startModeFn + "\n" + idFn + "\n" + watchFn + `
function assert(condition, message) { if (!condition) throw new Error(message); }
const doc = {
  nodes: [
    { id: 'inspect_primary_icm', data: { frame_id: 'root', group_id: '' } },
    { id: 'branch_on_state', data: { frame_id: 'child', group_id: '' } },
    { id: 'get_incident', data: { frame_id: 'child', group_id: 'active-arm' } },
  ],
  frames: [
    { id: 'root' },
    { id: 'child', parent_include_node_id: 'inspect_primary_icm' },
  ],
  groups: [
    { id: 'active-arm', parent_node_id: 'branch_on_state' },
  ],
};
const path = debugCallPathForNode(doc, 'get_incident');
assert(JSON.stringify(path.map(f => f.step_id)) === JSON.stringify(['inspect_primary_icm', 'branch_on_state']), 'nested call path was not root ordered');
let breakpoints = toggleDebugBreakpoint([], doc, 'get_incident', 'after');
assert(breakpoints.length === 1, 'breakpoint was not added');
assert(breakpoints[0].step === 'get_incident' && breakpoints[0].phase === 'after', 'breakpoint target is wrong');
const config = buildDebugRunConfig(breakpoints);
assert(config.enabled === true && config.breakpoints.length === 1, 'debug config is wrong');
assert(config.breakpoints[0].callPath[0].step_id === 'inspect_primary_icm', 'debug config lost call path');
assert(shouldStartDebugRun(breakpoints, '') === true, 'ordinary Run ignored configured breakpoints');
assert(shouldStartDebugRun([], 'saved-profile') === true, 'ordinary Run ignored the selected debug profile');
assert(shouldStartDebugRun([], '') === false, 'ordinary Run enabled debugging without debug configuration');
const profile = { version: 'yawr.debug-profile/v1', name: 'Mitigated ICM as active', root: {}, overrides: [] };
const profiled = buildDebugRunConfig([], profile, true, parseDebugWatches('incident_status\n\nincident_id'));
assert(profiled.profile === profile && profiled.allowStaleProfile === true, 'selected profile was not included');
assert(JSON.stringify(profiled.watches) === JSON.stringify(['incident_status', 'incident_id']), 'watches were not normalized');
assert(debugProfileID(' Mitigated ICM as active! ') === 'mitigated-icm-as-active', 'profile id slug is unstable');
assert(debugTargetSupported(doc, 'get_incident') === true, 'ordinary nested target was rejected');
const parallelDoc = {
	nodes: [
		{ id: 'fanout', data: { kind: 'parallel', group_id: '' } },
		{ id: 'parallel_child', data: { kind: 'noop', group_id: 'parallel-group' } },
	],
	groups: [{ id: 'parallel-group', parent_node_id: 'fanout' }], frames: [],
};
assert(debugTargetSupported(parallelDoc, 'fanout') === false, 'parallel target was accepted');
assert(debugTargetSupported(parallelDoc, 'parallel_child') === false, 'parallel child target was accepted');
const concurrentDoc = {
	nodes: [
		{ id: 'loop', data: { kind: 'iterate', concurrent: true, group_id: '' } },
		{ id: 'loop/child', data: { kind: 'noop', group_id: 'iterate-group' } },
	],
	groups: [{ id: 'iterate-group', parent_node_id: 'loop' }], frames: [],
};
assert(debugTargetSupported(concurrentDoc, 'loop/child') === false, 'concurrent iterate child target was accepted');
breakpoints = toggleDebugBreakpoint(breakpoints, doc, 'get_incident', 'after');
assert(breakpoints.length === 0, 'second toggle did not remove breakpoint');
`
	cmd := exec.Command(nodeExe, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("preview debugger behavior failed: %v\n%s", err, out)
	}
}

func TestPreviewSynthesizesBranchMergeTopology(t *testing.T) {
	source := previewHTMLForLifecycleTest(t)
	for _, required := range []string{
		"function withBranchMerges(doc)",
		"function trimVisibleGraph(allNodes, allEdges, overlayNodes)",
		"function BranchMergeNode()",
		"branchMerge: BranchMergeNode",
		"runtimeNodeID",
		"if (grp.kind === 'branch-arm' && grp.fallback)",
		"if (!node || (node.data && node.data.synthetic)) return;",
		"structuralNodeIDs: nodeOrder",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("preview branch merge rendering missing %q", required)
		}
	}

	nodeExe := requireNode(t)
	mergeFn := extractJSFunction(t, source, "withBranchMerges")
	trimFn := extractJSFunction(t, source, "trimVisibleGraph")
	edgeIDFn := extractJSFunction(t, source, "graphEdgeID")
	armMatchFn := extractJSFunction(t, source, "branchArmRouteMatches")
	edgeStateFn := extractJSFunction(t, source, "edgeRuntimeState")
	script := mergeFn + "\n" + edgeIDFn + "\n" + armMatchFn + "\n" + trimFn + "\n" + edgeStateFn + `
function node(id, kind, groupID, order) {
  return { id, data: { id, kind, group_id: groupID || '', frame_id: 'frame:root', order } };
}
function edge(source, target, type, label) { return { source, target, type, label: label || '' }; }
function assert(condition, message) { if (!condition) throw new Error(message); }
function hasEdge(doc, source, target) { return doc.edges.some(e => e.source === source && e.target === target); }
function mergeID(doc, branchID) {
  const merge = doc.nodes.find(n => n.data && n.data.synthetic && n.data.merge_for === branchID);
  return merge && merge.id;
}

const mixed = withBranchMerges({
  nodes: [node('branch', 'branch', '', 0), node('success', 'choice', 'g-success', 1), node('blocked', 'end', 'g-blocked', 2), node('done', 'end', '', 3)],
  groups: [
    { id: 'g-success', kind: 'branch-arm', parent_node_id: 'branch', fallback: false },
    { id: 'g-blocked', kind: 'branch-arm', parent_node_id: 'branch', fallback: true },
  ],
  edges: [edge('branch', 'success', 'branch-arm'), edge('branch', 'blocked', 'branch-arm'), edge('branch', 'done', 'sequence')],
});
const mixedMerge = mergeID(mixed, 'branch');
assert(mixedMerge, 'mixed branch missing merge node');
assert(hasEdge(mixed, 'success', mixedMerge), 'nonterminal arm must enter merge');
assert(!hasEdge(mixed, 'blocked', mixedMerge), 'terminal arm must not enter merge');
assert(hasEdge(mixed, mixedMerge, 'done'), 'merge must continue to done');
assert(!hasEdge(mixed, 'branch', 'done'), 'composite-to-continuation edge must be replaced');

const terminal = withBranchMerges({
  nodes: [node('branch', 'branch', '', 0), node('end-a', 'end', 'g-a', 1), node('end-b', 'end', 'g-b', 2), node('unreachable', 'display', '', 3)],
  groups: [
    { id: 'g-a', kind: 'branch-arm', parent_node_id: 'branch', fallback: false },
    { id: 'g-b', kind: 'branch-arm', parent_node_id: 'branch', fallback: true },
  ],
  edges: [edge('branch', 'end-a', 'branch-arm'), edge('branch', 'end-b', 'branch-arm'), edge('branch', 'unreachable', 'sequence')],
});
assert(!mergeID(terminal, 'branch'), 'all-terminal exhaustive branch must not have a merge');
assert(!hasEdge(terminal, 'branch', 'unreachable'), 'all-terminal exhaustive branch must not show an unreachable continuation');
assert(!terminal.nodes.some(n => n.id === 'unreachable'), 'all-terminal exhaustive continuation must be pruned');

const noFallback = withBranchMerges({
  nodes: [node('branch', 'branch', '', 0), node('end-a', 'end', 'g-a', 1), node('next', 'display', '', 2)],
  groups: [{ id: 'g-a', kind: 'branch-arm', parent_node_id: 'branch', fallback: false }],
  edges: [edge('branch', 'end-a', 'branch-arm'), edge('branch', 'next', 'sequence')],
});
const noFallbackMerge = mergeID(noFallback, 'branch');
assert(hasEdge(noFallback, 'branch', noFallbackMerge), 'non-exhaustive branch needs a no-match path');
assert(hasEdge(noFallback, noFallbackMerge, 'next'), 'no-match path must reach continuation through merge');
const noMatchEdge = noFallback.edges.find(e => e.source === 'branch' && e.target === noFallbackMerge);
assert(noMatchEdge.runtimeNodeID === 'branch' && noMatchEdge.runtimeExpectedStatus === 'skipped', 'no-match path must use real skipped branch state');
assert(edgeRuntimeState(noMatchEdge, { branch: { status: 'skipped' } }).status === 'completed', 'executed no-match edge must render as a completed route');
assert(edgeRuntimeState(noMatchEdge, { branch: { status: 'completed' } }) === null, 'no-match edge must stay neutral when an arm matched');

const emptyArm = withBranchMerges({
	nodes: [node('branch', 'branch', '', 0), node('blocked', 'end', 'g-blocked', 1), node('next', 'display', '', 2)],
	groups: [
		{ id: 'g-empty', kind: 'branch-arm', parent_node_id: 'branch', label: 'Empty matched arm', fallback: false },
		{ id: 'g-blocked', kind: 'branch-arm', parent_node_id: 'branch', fallback: true },
	],
	edges: [edge('branch', 'blocked', 'branch-arm'), edge('branch', 'next', 'sequence')],
});
const emptyMerge = mergeID(emptyArm, 'branch');
const emptyEdge = emptyArm.edges.find(e => e.source === 'branch' && e.target === emptyMerge);
assert(emptyEdge && emptyEdge.label === 'Empty matched arm', 'empty matched arm must retain arm identity');
assert(emptyEdge.routeKind === 'empty-arm', 'empty arm must not be mislabeled as no-match');
assert(emptyEdge.runtimeArmIndex === 0, 'empty arm must carry stable declaration index');
assert(edgeRuntimeState(emptyEdge, { branch: { status: 'completed', output: { matched_arm_index: 0 } } }).status === 'completed', 'selected empty arm must highlight');
assert(edgeRuntimeState(emptyEdge, { branch: { status: 'completed', output: { matched_arm_index: 1 } } }) === null, 'unselected empty arm must remain neutral');

const skippedChild = withBranchMerges({
	nodes: [
		node('branch', 'branch', '', 0), node('guarded', 'noop', 'g-selected', 1),
		node('blocked', 'end', 'g-fallback', 2), node('done', 'display', '', 3),
	],
	groups: [
		{ id: 'g-selected', kind: 'branch-arm', parent_node_id: 'branch', label: 'Selected route', index: 0, fallback: false },
		{ id: 'g-fallback', kind: 'branch-arm', parent_node_id: 'branch', label: 'Fallback', index: 1, fallback: true },
	],
	edges: [
		edge('branch', 'guarded', 'branch-arm', 'Selected route'),
		edge('branch', 'blocked', 'branch-arm', 'Otherwise — Fallback'),
		edge('branch', 'done', 'sequence'),
	],
});
const skippedMerge = mergeID(skippedChild, 'branch');
const skippedEntry = skippedChild.edges.find(e => e.source === 'branch' && e.target === 'guarded');
const skippedExit = skippedChild.edges.find(e => e.source === 'guarded' && e.target === skippedMerge);
for (const routeEdge of [skippedEntry, skippedExit]) {
	assert(routeEdge.runtimeNodeID === 'branch' && routeEdge.runtimeArmIndex === 0, 'selected arm entry/exit must use parent branch identity');
	assert(edgeRuntimeState(routeEdge, { branch: { status: 'completed', output: { matched_arm_index: 0 } }, guarded: { status: 'skipped' } }).status === 'completed', 'selected skipped-child route must render completed');
}
const skippedVisible = trimVisibleGraph(skippedChild.nodes, skippedChild.edges, {
	branch: { status: 'completed', output: { matched_arm_index: 0 } }, guarded: { status: 'skipped' }, done: { status: 'completed' },
});
for (const id of ['branch', 'guarded', skippedMerge, 'done']) assert(skippedVisible.nodeIDs.has(id), 'selected skipped-child route missing node ' + id);
for (const routeEdge of [skippedEntry, skippedExit, skippedChild.edges.find(e => e.source === skippedMerge && e.target === 'done')]) {
	assert(skippedVisible.edgeIDs.has(graphEdgeID(routeEdge)), 'selected skipped-child route missing active edge');
}
const selectedEntry = skippedChild.edges.find(e => e.source === 'branch' && e.target === 'guarded');
assert(edgeRuntimeState(selectedEntry, { branch: { status: 'running', output: { matched_arm_index: 0 } }, guarded: { status: 'running' } }).status === 'running', 'selected running arm entry must render running');
assert(edgeRuntimeState(selectedEntry, { branch: { status: 'failed', output: { matched_arm_index: 0 } }, guarded: { status: 'failed' } }).status === 'failed', 'selected failed arm entry must render failed');
assert(edgeRuntimeState(skippedExit, { branch: { status: 'failed', output: { matched_arm_index: 0 } }, guarded: { status: 'failed' } }) === null, 'failed arm must not enter shared continuation merge');

const duplicateEmpty = withBranchMerges({
	nodes: [node('branch', 'branch', '', 0), node('blocked', 'end', 'g-blocked', 1), node('next', 'display', '', 2)],
	groups: [
		{ id: 'g-empty-a', kind: 'branch-arm', parent_node_id: 'branch', label: 'Duplicate label', index: 0, fallback: false },
		{ id: 'g-empty-b', kind: 'branch-arm', parent_node_id: 'branch', label: 'Duplicate label', index: 1, fallback: false },
		{ id: 'g-blocked', kind: 'branch-arm', parent_node_id: 'branch', label: 'Fallback', index: 2, fallback: true },
	],
	edges: [edge('branch', 'blocked', 'branch-arm'), edge('branch', 'next', 'sequence')],
});
const duplicateMerge = mergeID(duplicateEmpty, 'branch');
const duplicateEdges = duplicateEmpty.edges.filter(e => e.source === 'branch' && e.target === duplicateMerge && e.routeKind === 'empty-arm');
assert(duplicateEdges.length === 2, 'duplicate-label empty arms must remain distinct');
assert(new Set(duplicateEdges.map(graphEdgeID)).size === 2, 'duplicate-label empty arms need distinct edge IDs');
const duplicateVisible = trimVisibleGraph(duplicateEmpty.nodes, duplicateEmpty.edges, {
	branch: { status: 'completed', output: { matched_arm_index: 1 } }, next: { status: 'completed' },
});
assert(!duplicateVisible.edgeIDs.has(graphEdgeID(duplicateEdges[0])), 'unselected empty arm must be removed by Trim');
assert(duplicateVisible.edgeIDs.has(graphEdgeID(duplicateEdges[1])), 'selected empty arm must survive Trim');

const nestedTerminal = withBranchMerges({
	nodes: [
		node('outer', 'branch', '', 0), node('nested', 'branch', 'g-outer', 1),
		node('unreachable-inner', 'display', 'g-outer', 2), node('nested-end-a', 'end', 'g-nested-a', 3),
		node('nested-end-b', 'end', 'g-nested-b', 4), node('outer-end', 'end', 'g-outer-fallback', 5),
		node('unreachable-root', 'display', '', 6),
	],
	groups: [
		{ id: 'g-outer', kind: 'branch-arm', parent_node_id: 'outer', fallback: false },
		{ id: 'g-outer-fallback', kind: 'branch-arm', parent_node_id: 'outer', fallback: true },
		{ id: 'g-nested-a', kind: 'branch-arm', parent_node_id: 'nested', fallback: false },
		{ id: 'g-nested-b', kind: 'branch-arm', parent_node_id: 'nested', fallback: true },
	],
	edges: [
		edge('outer', 'nested', 'branch-arm'), edge('nested', 'unreachable-inner', 'sequence'),
		edge('nested', 'nested-end-a', 'branch-arm'), edge('nested', 'nested-end-b', 'branch-arm'),
		edge('outer', 'outer-end', 'branch-arm'), edge('outer', 'unreachable-root', 'sequence'),
	],
});
assert(!nestedTerminal.nodes.some(n => n.id === 'unreachable-inner'), 'nested terminal branch must prune unreachable arm sibling');
assert(!nestedTerminal.nodes.some(n => n.id === 'unreachable-root'), 'exhaustive outer terminal branch must prune root continuation');
assert(!mergeID(nestedTerminal, 'outer'), 'exhaustive nested terminal branch must not feed an outer merge');

const nestedNonterminal = withBranchMerges({
	nodes: [
		node('outer', 'branch', '', 0), node('nested', 'branch', 'g-outer', 1),
		node('nested-a', 'display', 'g-nested-a', 2), node('nested-b', 'display', 'g-nested-b', 3),
		node('outer-end', 'end', 'g-outer-fallback', 4), node('next', 'display', '', 5),
	],
	groups: [
		{ id: 'g-outer', kind: 'branch-arm', parent_node_id: 'outer', fallback: false },
		{ id: 'g-outer-fallback', kind: 'branch-arm', parent_node_id: 'outer', fallback: true },
		{ id: 'g-nested-a', kind: 'branch-arm', parent_node_id: 'nested', fallback: false },
		{ id: 'g-nested-b', kind: 'branch-arm', parent_node_id: 'nested', fallback: true },
	],
	edges: [
		edge('outer', 'nested', 'branch-arm'), edge('nested', 'nested-a', 'branch-arm'),
		edge('nested', 'nested-b', 'branch-arm'), edge('outer', 'outer-end', 'branch-arm'),
		edge('outer', 'next', 'sequence'),
	],
});
const outerMerge = mergeID(nestedNonterminal, 'outer');
assert(hasEdge(nestedNonterminal, 'nested-a', outerMerge), 'nested arm A exit must feed outer merge');
assert(hasEdge(nestedNonterminal, 'nested-b', outerMerge), 'nested arm B exit must feed outer merge');
assert(!hasEdge(nestedNonterminal, 'nested', outerMerge), 'nested branch parent must not bypass its arm exits');

const colliding = withBranchMerges({
	nodes: [node('branch', 'branch', '', 0), node('arm', 'display', 'g-arm', 1), node('merge:branch', 'display', '', 2), node('next', 'display', '', 3)],
	groups: [{ id: 'g-arm', kind: 'branch-arm', parent_node_id: 'branch', fallback: true }],
	edges: [edge('branch', 'arm', 'branch-arm'), edge('branch', 'next', 'sequence')],
});
assert(mergeID(colliding, 'branch') !== 'merge:branch', 'synthetic merge ID must not collide with a real step ID');

const mixedVisible = trimVisibleGraph(mixed.nodes, mixed.edges, {
	branch: { status: 'completed' }, success: { status: 'completed' }, done: { status: 'completed' },
});
assert(mixedVisible.nodeIDs.has('success') && mixedVisible.nodeIDs.has(mixedMerge) && mixedVisible.nodeIDs.has('done'), 'taken arm and shared continuation must stay visible');
assert(!mixedVisible.nodeIDs.has('blocked'), 'untaken terminal arm must be trimmed despite live shared continuation');
for (const activeEdge of [
	mixed.edges.find(e => e.source === 'branch' && e.target === 'success'),
	mixed.edges.find(e => e.source === 'success' && e.target === mixedMerge),
	mixed.edges.find(e => e.source === mixedMerge && e.target === 'done'),
]) assert(activeEdge && mixedVisible.edgeIDs.has(graphEdgeID(activeEdge)), 'complete active route must survive Trim');
const inactiveNoMatch = mixed.edges.find(e => e.routeKind === 'no-match');
assert(!inactiveNoMatch || !mixedVisible.edgeIDs.has(graphEdgeID(inactiveNoMatch)), 'inactive no-match edge must not survive endpoint filtering');
const noMatchVisible = trimVisibleGraph(noFallback.nodes, noFallback.edges, {
	branch: { status: 'skipped' }, next: { status: 'completed' },
});
assert(noMatchVisible.nodeIDs.has(noFallbackMerge) && noMatchVisible.nodeIDs.has('next'), 'executed no-match route must retain merge and continuation');
assert(noMatchVisible.edgeIDs.has(graphEdgeID(noMatchEdge)), 'executed no-match edge must be retained');
assert(!noMatchVisible.nodeIDs.has('end-a'), 'untaken explicit arm must be trimmed on no-match');

const nestedNoMatch = withBranchMerges({
	nodes: [
		node('outer', 'branch', '', 0), node('nested', 'branch', 'g-outer', 1),
		node('nested-a', 'display', 'g-nested-a', 2), node('after-nested', 'display', 'g-outer', 3),
		node('outer-end', 'end', 'g-outer-fallback', 4), node('done', 'display', '', 5),
	],
	groups: [
		{ id: 'g-outer', kind: 'branch-arm', parent_node_id: 'outer', fallback: false },
		{ id: 'g-outer-fallback', kind: 'branch-arm', parent_node_id: 'outer', fallback: true },
		{ id: 'g-nested-a', kind: 'branch-arm', parent_node_id: 'nested', fallback: false },
	],
	edges: [
		edge('outer', 'nested', 'branch-arm'), edge('nested', 'nested-a', 'branch-arm'),
		edge('nested', 'after-nested', 'sequence'), edge('outer', 'outer-end', 'branch-arm'),
		edge('outer', 'done', 'sequence'),
	],
});
const nestedNoMatchMerge = mergeID(nestedNoMatch, 'outer');
const nestedNoMatchVisible = trimVisibleGraph(nestedNoMatch.nodes, nestedNoMatch.edges, {
	outer: { status: 'completed', output: { matched_arm_index: 0 } }, nested: { status: 'skipped' }, 'after-nested': { status: 'completed' }, done: { status: 'completed' },
});
assert(nestedNoMatchVisible.nodeIDs.has('nested'), 'executed nested skipped branch must stay visible');
assert(nestedNoMatchVisible.nodeIDs.has('after-nested'), 'nested no-match continuation must stay visible');
assert(nestedNoMatchVisible.nodeIDs.has(nestedNoMatchMerge) && nestedNoMatchVisible.nodeIDs.has('done'), 'nested no-match must reach outer merge and shared continuation');
const nestedEntry = nestedNoMatch.edges.find(e => e.source === 'outer' && e.target === 'nested');
assert(edgeRuntimeState(nestedEntry, { outer: { status: 'completed', output: { matched_arm_index: 0 } }, nested: { status: 'skipped' } }).status === 'completed', 'entry into executed skipped branch must render completed');
console.log(JSON.stringify({passed: 8}));
`
	cmd := exec.Command(nodeExe, "-")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("branch merge truth table failed: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), out)
	}
}

func TestPreviewDoesNotAutoSelectUnexecutedNodes(t *testing.T) {
	source := previewHTMLForLifecycleTest(t)

	for _, required := range []string{
		"--inspect:      var(--vscode-charts-blue, #58a6ff)",
		"outline: 1px solid var(--inspect)",
		".node.selected { filter: drop-shadow(0 0 3px var(--inspect)); }",
		".node-v2.selected { box-shadow: 0 0 0 2px var(--inspect) !important; }",
		"function visibleSelectedNodeID(selectedNode, selectionIsExplicit, runID, overlay, pending)",
		"if (selectionIsExplicit || !runID || (pending && (pending.nodeID || pending.stepID) === selectedNode)) return selectedNode;",
		"if (!nodeState || nodeState.status === 'pending' || nodeState.status === 'skipped') return null;",
		"const [selectionIsExplicit, setSelectionIsExplicit] = useState(false)",
		"const visibleSelectedNode = visibleSelectedNodeID(",
		"selectedNode: visibleSelectedNode",
		"nodeID: visibleSelectedNode",
		"selected: n.id === visibleSelectedNode",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("preview selection semantics missing %q", required)
		}
	}
	if strings.Contains(source, ".prose li.selected { background: var(--vscode-list-activeSelectionBackground") {
		t.Fatal("inspection styling must not inherit the theme's execution-green-looking active selection color")
	}
	if got := strings.Count(source, "selectedNode: visibleSelectedNode"); got != 2 {
		t.Fatalf("graph and prose must both consume visibleSelectedNode: got %d wiring sites", got)
	}

	nodeExe := requireNode(t)
	selectionFn := extractJSFunction(t, source, "visibleSelectedNodeID")
	cases := []struct {
		Name                string         `json:"name"`
		SelectedNode        any            `json:"selectedNode"`
		SelectionIsExplicit bool           `json:"selectionIsExplicit"`
		RunID               string         `json:"runID"`
		Overlay             map[string]any `json:"overlay"`
		Pending             map[string]any `json:"pending"`
		Want                any            `json:"want"`
	}{
		{Name: "none", SelectedNode: nil, Want: nil},
		{Name: "design-time", SelectedNode: "future", Want: "future"},
		{Name: "explicit pending", SelectedNode: "future", SelectionIsExplicit: true, RunID: "run-1", Want: "future"},
		{Name: "active interaction", SelectedNode: "question", RunID: "run-1", Pending: map[string]any{"stepID": "question"}, Want: "question"},
		{Name: "automatic absent", SelectedNode: "untaken", RunID: "run-1", Overlay: map[string]any{"nodes": map[string]any{}}, Want: nil},
		{Name: "automatic pending", SelectedNode: "untaken", RunID: "run-1", Overlay: map[string]any{"nodes": map[string]any{"untaken": map[string]any{"status": "pending"}}}, Want: nil},
		{Name: "automatic skipped", SelectedNode: "untaken", RunID: "run-1", Overlay: map[string]any{"nodes": map[string]any{"untaken": map[string]any{"status": "skipped"}}}, Want: nil},
		{Name: "automatic completed", SelectedNode: "done", RunID: "run-1", Overlay: map[string]any{"nodes": map[string]any{"done": map[string]any{"status": "completed"}}}, Want: "done"},
	}
	encodedCases, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("marshal selection cases: %v", err)
	}
	script := selectionFn + `
const cases = ` + string(encodedCases) + `;
for (const c of cases) {
  const got = visibleSelectedNodeID(c.selectedNode, c.selectionIsExplicit, c.runID, c.overlay, c.pending);
  if (got !== c.want) throw new Error(c.name + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(c.want));
}
console.log(JSON.stringify({ passed: cases.length }));
`
	cmd := exec.Command(nodeExe, "-")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("selection truth table failed: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), out)
	}
}
