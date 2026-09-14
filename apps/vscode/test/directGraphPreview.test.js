'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const {
  createDirectGraphWebviewHtml,
  graphMayRequireMcpBridge,
  sessionMayRequireMcpBridge,
  graphPreviewArgs,
  loadGraphDocument,
  parseGraphDocument,
} = require('../out/directGraphPreview');

const root = path.join(__dirname, '..');

test('direct graph webview uses local assets with no iframe or HTTP server', () => {
  const html = createDirectGraphWebviewHtml(
    'vscode-webview-resource:/extension/media/graph.js',
    'vscode-webview-resource:/extension/media/graph.css',
    'vscode-webview-resource:',
    'test-nonce',
  );

  assert.match(html, /id="root"/);
  assert.match(html, /media\/graph\.js/);
  assert.match(html, /media\/graph\.css/);
  assert.match(html, /script-src 'nonce-test-nonce'/);
  assert.match(html, /style-src vscode-webview-resource:/);
  assert.doesNotMatch(html, /style-src[^;]*graph\.css/);
  assert.match(html, /style-src-attr 'unsafe-inline'/);
  assert.doesNotMatch(html, /<iframe\b/i);
  assert.doesNotMatch(html, /https?:\/\//i);
  assert.doesNotMatch(html, /script-src[^;]*unsafe-inline/);
});

test('graph preview invokes the CLI graphjson contract directly', () => {
  assert.deepEqual(
    graphPreviewArgs('C:\\work\\incident.runbook.yaml'),
    ['preview', '--format', 'graphjson', '--recurse', 'C:\\work\\incident.runbook.yaml'],
  );
});

test('graph preview passes the resolved package map before the positional runbook', async () => {
  const map = 'C:\\work\\packages\\local-onebox.serve-package-map.yaml';
  const runbook = 'C:\\work\\incident.runbook.yaml';
  const fixture = fs.readFileSync(path.join(__dirname, 'fixtures', 'enum-preview-graphjson.json'), 'utf8');
  await loadGraphDocument('yawr', runbook, async (binary, args) => {
    assert.deepEqual(args, ['preview', '--format', 'graphjson', '--recurse', '--package-map', map, runbook]);
    return { stdout: fixture };
  }, map);
});

test('current graph invalidation includes authored tool saves and map, not unrelated files', () => {
  const { graphSourceChanged } = require('../out/graphSourceChanged');
  const project = path.resolve('project');
  const runbook = path.join(project, 'main.runbook.yaml');
  const map = path.join(project, 'packages', 'custom-map.yaml');
  const included = path.resolve('external', 'included.runbook.yaml');
  const doc = { frames: [{ runbook_path: included }] };
  const changed = file => graphSourceChanged(file, runbook, project, doc, map);
  for (const file of [runbook, map, included, path.join(project, 'packages', 'query.tool.yaml')]) assert.equal(changed(file), true, file);
  for (const file of [path.join(project, 'notes.yaml'), path.resolve('project-other', 'query.tool.yaml'),
    path.join(project, 'other.runbook.yaml')]) assert.equal(changed(file), false, file);
  const source = fs.readFileSync(path.join(root, 'src', 'extension.ts'), 'utf8');
  assert.match(source, /if \(graphSourceChanged\([\s\S]*?\)\) requestReload\(\)/);
  assert.match(source, /const requestReload = \(\) => \{\s+if \(runStarting \|\| runSession \|\| investigationClient \|\| investigationDescriptor\)/);
});

test('loadGraphDocument executes graphjson and validates stdout', async () => {
  const calls = [];
  const fixture = fs.readFileSync(
    path.join(__dirname, 'fixtures', 'enum-preview-graphjson.json'),
    'utf8',
  );

  const document = await loadGraphDocument(
    'C:\\bin\\yawr.exe',
    'C:\\work\\incident.runbook.yaml',
    async (binary, args) => {
      calls.push({ binary, args });
      return { stdout: fixture };
    },
  );

  assert.deepEqual(calls, [{
    binary: 'C:\\bin\\yawr.exe',
    args: ['preview', '--format', 'graphjson', '--recurse', 'C:\\work\\incident.runbook.yaml'],
  }]);
  assert.equal(document.runbook.name, 'enum-fixture');
});

test('parseGraphDocument accepts the real graphjson fixture', () => {
  const fixture = fs.readFileSync(
    path.join(__dirname, 'fixtures', 'enum-preview-graphjson.json'),
    'utf8',
  );

  const document = parseGraphDocument(fixture);

  assert.equal(document.schema_version, '1');
  assert.equal(document.runbook.name, 'enum-fixture');
  assert.equal(document.nodes.length, 1);
  assert.equal(document.nodes[0].id, 'end');
});

test('parseGraphDocument rejects malformed or incompatible output', () => {
  assert.throws(() => parseGraphDocument('not json'), /valid JSON/i);
  assert.throws(
    () => parseGraphDocument(JSON.stringify({ schema_version: '2', nodes: [], edges: [] })),
    /schema_version/i,
  );
  assert.throws(
    () => parseGraphDocument(JSON.stringify({ schema_version: '1', nodes: {}, edges: [] })),
    /nodes/i,
  );
});

    const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');
  
    assert.match(source, />Test reaching this step</);
test('parseGraphDocument rejects render-breaking node and edge values', () => {
  const base = {
    schema_version: '1',
    runbook: { id: 'rb', name: 'Runbook', path: 'runbook.yaml' },
    frames: [],
    groups: [],
    nodes: [{
      id: 'start',
      type: 'action',
      data: { id: 'start', kind: 'cli', title: 'Start', group_id: '', frame_id: 'frame:root' },
      position: { x: 0, y: 0 },
    }],
    edges: [],
  };

  const cases = [
    [{ ...base, runbook: { ...base.runbook, name: {} } }, /runbook\.name/i],
    [{ ...base, nodes: [{ ...base.nodes[0], id: '' }] }, /node id/i],
    [{ ...base, nodes: [base.nodes[0], { ...base.nodes[0] }] }, /duplicate node id/i],
    [{ ...base, nodes: [{ ...base.nodes[0], data: [] }] }, /node.*data/i],
    [{ ...base, nodes: [{ ...base.nodes[0], position: { x: '0', y: 0 } }] }, /position/i],
    [{ ...base, edges: [{ id: 'e1', source: 'start' }] }, /edge.*target/i],
    [{ ...base, edges: [{ id: 'e1', source: 'start', target: 'missing' }] }, /unknown target/i],
    [{
      ...base,
      nodes: [base.nodes[0], { ...base.nodes[0], id: 'end', data: { ...base.nodes[0].data, id: 'end' } }],
      edges: [
        { id: 'e1', source: 'start', target: 'end', label: 'next' },
        { id: 'e1', source: 'start', target: 'end', label: 'again' },
      ],
    }, /duplicate edge id/i],
  ];

  for (const [value, expected] of cases) {
    assert.throws(() => parseGraphDocument(JSON.stringify(value)), expected);
  }
});

test('parseGraphDocument validates group and frame ownership', () => {
  const grouped = {
    schema_version: '1',
    runbook: { id: 'rb', name: 'Runbook', path: 'runbook.yaml' },
    frames: [{ id: 'frame:root', runbook_id: 'rb', runbook_path: 'runbook.yaml', depth: 0 }],
    groups: [{ id: 'group:branch', kind: 'branch-arm', parent_node_id: 'branch', frame_id: 'frame:root' }],
    nodes: [
      {
        id: 'branch',
        type: 'decision',
        data: { id: 'branch', kind: 'branch', group_id: '', frame_id: 'frame:root' },
        position: { x: 0, y: 0 },
      },
      {
        id: 'inside',
        type: 'action',
        data: { id: 'inside', kind: 'cli', group_id: 'group:branch', frame_id: 'frame:root' },
        parentNode: 'group:branch',
        extent: 'parent',
        position: { x: 0, y: 0 },
      },
    ],
    edges: [{ id: 'e1', source: 'branch', target: 'inside', type: 'branch-arm' }],
  };

  assert.equal(parseGraphDocument(JSON.stringify(grouped)).groups.length, 1);

  assert.throws(
    () => parseGraphDocument(JSON.stringify({ ...grouped, groups: [{ ...grouped.groups[0], parent_node_id: 'missing' }] })),
    /group.*parent/i,
  );
  assert.throws(
    () => parseGraphDocument(JSON.stringify({ ...grouped, nodes: [grouped.nodes[0], {
      ...grouped.nodes[1],
      data: { ...grouped.nodes[1].data, group_id: 'group:missing' },
      parentNode: 'group:missing',
    }] })),
    /unknown group/i,
  );
  assert.throws(
    () => parseGraphDocument(JSON.stringify({ ...grouped, nodes: [grouped.nodes[0], {
      ...grouped.nodes[1],
      parentNode: 'group:other',
    }] })),
    /parentNode/i,
  );
  assert.throws(
    () => parseGraphDocument(JSON.stringify({ ...grouped, frames: [{ ...grouped.frames[0], depth: '0' }] })),
    /frame.*depth/i,
  );
  assert.throws(
    () => parseGraphDocument(JSON.stringify({
      ...grouped,
      groups: [{ ...grouped.groups[0], id: 'inside' }],
      nodes: [grouped.nodes[0], {
        ...grouped.nodes[1],
        data: { ...grouped.nodes[1].data, group_id: 'inside' },
        parentNode: 'inside',
      }],
    })),
    /collides with node id/i,
  );

  const cyclic = JSON.parse(JSON.stringify(grouped));
  cyclic.groups = [
    { id: 'group:a', kind: 'branch-arm', parent_node_id: 'inside-b', frame_id: 'frame:root' },
    { id: 'group:b', kind: 'branch-arm', parent_node_id: 'inside-a', frame_id: 'frame:root' },
  ];
  cyclic.nodes = [
    { ...grouped.nodes[0], id: 'inside-a', data: { ...grouped.nodes[0].data, id: 'inside-a', group_id: 'group:a' }, parentNode: 'group:a', extent: 'parent' },
    { ...grouped.nodes[0], id: 'inside-b', data: { ...grouped.nodes[0].data, id: 'inside-b', group_id: 'group:b' }, parentNode: 'group:b', extent: 'parent' },
  ];
  cyclic.edges = [];
  assert.throws(() => parseGraphDocument(JSON.stringify(cyclic)), /group ownership cycle/i);
});

test('webview build is local and has no server transport', () => {
  const manifest = require('../package.json');
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

  assert.match(manifest.scripts.compile, /compile:webview/);
  assert.match(manifest.scripts['compile:webview'], /esbuild/);
  assert.match(source, /acquireVsCodeApi/);
  assert.match(source, /type:\s*['"]ready['"]/);
  assert.match(source, /type:\s*['"]rendered['"]/);
  assert.match(source, /if \(!testMode\) return/);
  assert.match(source, /message\.testMode === true/);
  assert.doesNotMatch(source, /\bfetch\s*\(/);
  assert.doesNotMatch(source, /\bEventSource\b/);
  assert.doesNotMatch(source, /<iframe\b/i);
});

test('webview includes full run controls, declared inputs, and interaction forms', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

  assert.match(source, /function\s+InputsForm/);
  assert.match(source, /declaration\.type === ['"]secret['"] \? ['"]password['"] : ['"]text['"]/);
  assert.match(source, /function\s+InteractionPane/);
  assert.match(source, />Run</);
  assert.match(source, />Cancel</);
  assert.match(source, /type:\s*['"]run\.start['"]/);
  assert.match(source, /type:\s*['"]run\.command['"]/);
  assert.match(source, /interaction\.pending/);
  assert.match(source, /kind === ['"]choice['"]/);
  assert.match(source, /kind === ['"]decision['"]/);
  assert.match(source, /kind === ['"]collector['"]/);
  assert.match(source, /kind === ['"]approval['"]/);
  assert.match(source, /approved:\s*true/);
  assert.match(source, /approved:\s*false/);
  assert.match(source, /yawr\.host-action\.request/);
  assert.doesNotMatch(source, /\bfetch\s*\(/);
  assert.doesNotMatch(source, /\bEventSource\b/);
});

test('webview exposes literal route-through-step controls without focus-corridor jargon', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

  assert.match(source, /computeRouteProjection/);
  assert.match(source, /projectRouteDocument/);
  assert.match(source, />Show routes through this step</);
  assert.match(source, />Through this step</);
  assert.match(source, />To this step</);
  assert.match(source, />From this step</);
  assert.match(source, />Show full graph</);
  assert.match(source, /const routeActionsDisabled = runActive && !sessionID/);
  assert.match(source, /disabled=\{routeActionsDisabled\}/);
  assert.doesNotMatch(source, /focus corridor/i);
});

test('webview identifies focused and current steps with distinct side arrows', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');
  const styles = fs.readFileSync(path.join(root, 'webview', 'graph.css'), 'utf8');

  assert.match(source, /NodeToolbar\s+isVisible=\{selected\}\s+position=\{Position\.Left\}/);
  assert.match(source, /NodeToolbar\s+isVisible=\{isCurrent\}\s+position=\{Position\.Right\}/);
  assert.match(source, /const locatorText = title \|\| id/);
  assert.match(source, /Focused step:\s*\$\{locatorText\}/);
  assert.match(source, /Current step:\s*\$\{locatorText\}/);
  assert.match(source, /<code title=\{id\}>\{locatorText\}<\/code>/);
  assert.match(source, /executionPosition\.terminal \? 'Last reached' : 'Current'/);
  const activity = fs.readFileSync(path.join(root, 'webview', 'CurrentActivity.tsx'), 'utf8');
  assert.match(source, /<ActivityDetails activities=\{activities\}/);
  assert.match(activity, /<details className="current-activity"/);
  assert.match(activity, />Locate<\/span>/);
  assert.match(source, /executionNodeID=\{executionNodeID\}/);
  assert.match(source, /setExecutionNodeID\(reachedNodeID\)/);
  assert.match(source, /className="node-locator focused"/);
  assert.match(source, /className=\{`node-locator \$\{executionPosition\.terminal \? 'last-reached' : 'current'\}`\}/);
  assert.match(source, /<ArrowRight/);
  assert.match(source, /<ArrowLeft/);
  assert.match(source, /const focusedNodeID = routeTargetID \?\? selectedId/);
  assert.match(source, /selected:\s*node\.id === focusedNodeID/);
  assert.match(source, /nodeColor=\{\(node\) => node\.id === resolvedExecutionNodeID/);
  assert.match(source, /: node\.selected \? 'var\(--vscode-charts-yellow\)'/);
  assert.match(styles, /\.node-locator\s*\{[^}]*color:\s*var\(--vscode-foreground\)/s);
  assert.match(styles, /\.node-locator\s*\{[^}]*background:\s*color-mix\(in srgb, var\(--locator-color\) 18%, var\(--vscode-editor-background\)\)/s);
  assert.match(styles, /\.node-locator\s*\{[^}]*border:\s*2px solid var\(--locator-color\)/s);
  assert.match(styles, /\.node-locator\.focused[^}]*var\(--vscode-charts-yellow\)/s);
  assert.match(styles, /\.node-locator\.current[^}]*var\(--vscode-charts-green\)/s);
  assert.match(styles, /\.node-locator\.last-reached[^}]*var\(--vscode-charts-blue\)/s);
  assert.match(styles, /\.step-node\.execution-current[^}]*outline:/s);
  assert.match(styles, /\.step-node\.execution-last[^}]*outline:/s);
  assert.doesNotMatch(styles, /\.execution-position-strip/);
  assert.match(styles, /\.node-locator code\s*\{[^}]*background:\s*transparent/s);
  assert.match(styles, /\.node-locator code\s*\{[^}]*font-weight:\s*600/s);
  assert.doesNotMatch(source, /\.setCenter\s*\(/);
});

test('webview resizes the inspector with an accessible persisted ratio', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');
  const styles = fs.readFileSync(path.join(root, 'webview', 'graph.css'), 'utf8');

  assert.match(source, /getState\?\(\)/);
  assert.match(source, /setState\?\(/);
  assert.match(source, /inspectorRatio/);
  assert.match(source, /role="separator"/);
  assert.match(source, /aria-orientation="vertical"/);
  assert.match(source, /onPointerDown=/);
  assert.match(source, /onPointerMove=/);
  assert.match(source, /onKeyDown=/);
  assert.match(source, /--inspector-width/);
  assert.match(styles, /\.inspector-resizer/);
  assert.match(styles, /grid-template-columns:[^;]*var\(--inspector-width/s);
  assert.doesNotMatch(source, /localStorage/);
});

test('webview reviews collector answers and confirms XTS before dispatch', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

  assert.match(source, />Open XTS</);
  assert.match(source, /yawr\.host-action\.confirmed-request/);
  assert.match(source, />Review answers</);
  assert.match(source, />Save answers and continue</);
  assert.match(source, />Edit answers</);
  assert.match(source, /Collected in this actual run/);
  assert.match(source, /review-before-submit/);
  assert.match(source, />1 Open XTS</);
  assert.match(source, />2 Answer questions</);
  assert.match(source, />3 Review</);
});

test('webview exposes the literal saved route-test workflow and safety boundary', () => {
  const source = [
    fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8'),
    fs.readFileSync(path.join(root, 'webview', 'routeTestPane.tsx'), 'utf8'),
  ].join('\n');

  assert.match(source, />Test reaching this step</);
  assert.match(source, /XTS will not open in this route test/);
  assert.match(source, />Check this route</);
  assert.match(source, /No external actions will run/);
  assert.match(source, />Run route test</);
  assert.match(source, /Step reached - command not run/);
  assert.match(source, /The route went somewhere else/);
  assert.match(source, /Saved route tests/);
  assert.match(source, /route-test\.save/);
  assert.match(source, /route-test\.run/);
  assert.match(source, /const \[routeTestRunning, setRouteTestRunning\] = useState\(false\)/);
  assert.match(source, /routeTestRunning\s*\?\s*'Testing route - XTS and external actions are blocked'/);
  assert.doesNotMatch(source, /runActive\s*\?\s*'Testing route - XTS and external actions are blocked'/);
});

test('webview invalidates a route-test editor during render when its graph context changes', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

  assert.match(source, /routeTestEditor\?\.contextKey === routeTestContextKey/);
  assert.doesNotMatch(
    source,
    /useEffect\(\(\) => \{\s*setRouteTestEditor\(undefined\);\s*\}, \[document\.hash, routeTestContext\?\.planHash\]\);/,
  );
});

test('route-test editor preserves skipped saved step outcomes', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'routeTestPane.tsx'), 'utf8');

  assert.match(
    source,
    /outcome:\s*condition\.status === 'completed'\s*\?\s*'success'\s*:\s*condition\.status === 'skipped'\s*\?\s*'skipped'\s*:\s*'failed'/,
  );
});

   test('webview includes direct debugger breakpoints, pause actions, and overrides', () => {
     const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

     assert.match(source, />Debug Run</);
     assert.match(source, /toggleBreakpoint/);
    assert.match(source, /onToggle\(node\.id,\s*['"]before['"]\)/);
    assert.match(source, /onToggle\(node\.id,\s*['"]after['"]\)/);
     assert.match(source, /kind === ['"]debug_break['"]/);
     assert.match(source, /output_patch/);
     assert.match(source, /protectedVariables/);
     assert.match(source, /step_into/);
     assert.match(source, /step_over/);
     assert.match(source, /step_out/);
     assert.match(source, /debug\/override_applied/);
    assert.match(source, /effectiveStatus === ['"]failed['"] && effectiveError/);
    assert.match(source, /aria-live="polite"/);
    assert.match(source, /pauseHeadingRef\.current\?\.focus\(\)/);
    assert.match(source, /MAX_DEBUG_PATCH_BYTES\s*=\s*64\s*\*\s*1024/);
    assert.match(source, /MAX_DEBUG_PATCH_DEPTH\s*=\s*8/);
    assert.match(source, /MAX_DEBUG_PATCH_NODES\s*=\s*512/);
   });

  test('webview retains bounded live step details for the inspector', () => {
    const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

    assert.match(source, /output:\s*event\.kind === 'step\/completed' \|\| event\.kind === 'step\/failed'\s*\?\s*recordValue\(payload\.output\)\s*:\s*previous\.output/);
    assert.match(source, /payload\.qualified_node_id/);
    assert.match(source, /captures:\s*recordValue\(payload\.captures\)/);
    assert.match(source, /evidence:\s*payload\.evidence/);
    assert.match(source, /logs:\s*appendRuntimeLog/);
    assert.match(source, /startedAt:\s*event\.timestamp/);
    assert.match(source, /finishedAt:\s*event\.timestamp/);
  });

  test('webview renders a compact kind-aware inspector and run overview', () => {
    const source = [
      fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8'),
      fs.readFileSync(path.join(root, 'webview', 'inspector.tsx'), 'utf8'),
    ].join('\n');

    assert.match(source, /function\s+StepInspector/);
    assert.match(source, /function\s+RunOverview/);
    assert.match(source, /role="tablist"/);
    assert.match(source, />Definition</);
    assert.match(source, />Run</);
    assert.match(source, />Debug</);
    for (const kind of ['cli', 'tool', 'include', 'choice', 'decision', 'collector', 'host_action', 'branch', 'iterate', 'parallel', 'approve', 'assert', 'wait_for_event', 'display', 'end', 'compensate', 'noop']) {
      assert.match(source, new RegExp(`case ['"]${kind}['"]`), `missing inspector renderer for ${kind}`);
    }
    assert.match(source, /function\s+NamedValueRows/);
    assert.match(source, /function\s+RuntimePane/);
    assert.match(source, /function\s+CommonDefinition/);
    assert.match(source, /aria-label="Execution occurrence"/);
    assert.match(source, /selectedOccurrence\?\.executionSource/);
    assert.match(source, /aria-label="Graph revision"/);
    assert.match(source, /title="Snapshot"/);
    assert.match(source, /title="Handoffs"/);
  });

test('webview renders graph groups as compound frame nodes', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

  assert.match(source, /function\s+FrameNode/);
  assert.match(source, /frameBox/);
  assert.match(source, /parentNode/);
  assert.match(source, /frameCount/);
  assert.match(source, /document\.groups/);
  assert.match(source, /function\s+SessionEntryNode/);
  assert.match(source, /kind === 'session-entry'/);
});

test('webview reconciles terminal state by exact node identity', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

  assert.match(source, /node_id\?: string/);
  assert.match(source, /const nodeID = step\.node_id \|\| step\.step_id/);
  assert.doesNotMatch(source, /nodeID\.endsWith\(/);
});

test('webview treats indeterminate runs and steps as terminal', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');

  assert.match(source, /event\.kind === 'step\/indeterminate'/);
  assert.match(source, /frame\.event\.kind === 'run\/indeterminate'/);
  assert.match(source, /status === 'indeterminate'/);
});

test('narrow layout keeps the no-selection run overview visible', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');
  const css = fs.readFileSync(path.join(root, 'webview', 'graph.css'), 'utf8');

  assert.match(source, /has-panel-content/);
  assert.doesNotMatch(source, /pending \|\| selected \? ' has-panel-content'/);
  assert.doesNotMatch(css, /app:not\(\.has-panel-content\) \.inspector/);
  assert.match(css, /@media \(max-width: 720px\)[\s\S]*\.react-flow__minimap\s*\{\s*display:\s*none/);
});

test('inspector exposes accessible progress and keyboard tabs', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'inspector.tsx'), 'utf8');

  assert.match(source, /role="progressbar"/);
  assert.match(source, /aria-valuenow=\{progress\}/);
  assert.match(source, /role="tabpanel"/);
  assert.match(source, /aria-controls=/);
  assert.match(source, /tabIndex=\{tab ===/);
  assert.match(source, /ArrowRight/);
  assert.match(source, /ArrowLeft/);
  assert.match(source, /event\.key === 'Home'/);
  assert.match(source, /event\.key === 'End'/);
});

test('all configured node styles have distinct renderer rules', () => {
  const source = fs.readFileSync(path.join(root, 'webview', 'graph.tsx'), 'utf8');
  const css = fs.readFileSync(path.join(root, 'webview', 'graph.css'), 'utf8');

  assert.match(css, /\.style-smooth-curves\s+\.step-node/);
  assert.match(css, /\.style-minimalist\s+\.step-node/);
  assert.match(css, /\.style-header-badges\s+\.step-node/);
  assert.match(source, /style === 'minimalist'.*straight/s);
  assert.match(source, /style === 'header-badges'.*step/s);
});

test('MCP bridge starts only for vscode-mcp graph actions or unknown dynamic work', () => {
  const base = JSON.parse(fs.readFileSync(path.join(root, 'test', 'fixtures', 'enum-preview-graphjson.json'), 'utf8'));
  const tool = structuredClone(base);
  tool.nodes[0].data.kind = 'tool';
  tool.nodes[0].data.tool_name = 'icm';
  tool.nodes[0].data.tool_action = 'get-incident';
  assert.equal(graphMayRequireMcpBridge(tool, {}), false, 'native/non-VS Code tools must not start a bridge');
  assert.equal(graphMayRequireMcpBridge(tool, { 'icm/get-incident': {} }), true);

  const dynamic = structuredClone(base);
  dynamic.nodes[0].data.kind = 'include';
  dynamic.nodes[0].data.dynamic = true;
  assert.equal(graphMayRequireMcpBridge(dynamic, {}), true, 'dynamic descendants are not statically knowable');

  const legacy = structuredClone(tool);
  delete legacy.nodes[0].data.tool_name;
  delete legacy.nodes[0].data.tool_action;
  assert.equal(graphMayRequireMcpBridge(legacy, {}), true, 'older graphjson must remain conservative');
});

test('session MCP bridge is provisioned before a later handoff needs a configured action', () => {
  const entry = JSON.parse(fs.readFileSync(path.join(root, 'test', 'fixtures', 'enum-preview-graphjson.json'), 'utf8'));
  assert.equal(graphMayRequireMcpBridge(entry, { 'xts/open': {} }), false);
  assert.equal(sessionMayRequireMcpBridge(entry, { 'xts/open': {} }), true);
  assert.equal(sessionMayRequireMcpBridge(entry, {}), false);
});

test('parseGraphDocument validates discriminated step details', () => {
  const fixture = JSON.parse(fs.readFileSync(path.join(root, 'test', 'fixtures', 'enum-preview-graphjson.json'), 'utf8'));
  fixture.nodes[0].data.details = {
    kind: 'end',
    category: 'resolved',
    code: 'complete',
    common: { timeout: '30s', captures: [{ name: 'result', source: 'stdout' }] },
  };
  const parsed = parseGraphDocument(JSON.stringify(fixture));
  assert.equal(parsed.nodes[0].data.details.kind, 'end');

  fixture.nodes[0].data.details.kind = 'tool';
  assert.throws(() => parseGraphDocument(JSON.stringify(fixture)), /details kind must match/i);
  fixture.nodes[0].data.kind = 'branch';
  fixture.nodes[0].data.details = { kind: 'branch', arms: 'not-an-array' };
  assert.throws(() => parseGraphDocument(JSON.stringify(fixture)), /details\.arms must be an array/i);
});

test('parseGraphDocument accepts additive step detail fields', () => {
  const fixture = JSON.parse(fs.readFileSync(path.join(root, 'test', 'fixtures', 'enum-preview-graphjson.json'), 'utf8'));
  fixture.nodes[0].data.kind = 'tool';
  fixture.nodes[0].data.details = {
    kind: 'tool',
    tool: 'icm',
    action: 'get-incident',
    future_summary: { tone: 'compact' },
    common: {
      retry: { max: 2, future_strategy: 'adaptive' },
      future_policy: true,
    },
  };

  const parsed = parseGraphDocument(JSON.stringify(fixture));
  assert.deepEqual(parsed.nodes[0].data.details.future_summary, { tone: 'compact' });
});

test('parseGraphDocument deeply validates known step detail fields', () => {
  const base = JSON.parse(fs.readFileSync(path.join(root, 'test', 'fixtures', 'enum-preview-graphjson.json'), 'utf8'));
  const invalidDetails = [
    [{ kind: 'tool', arguments: [{ name: 42 }] }, /details\.arguments\[0\]\.name must be a string/i],
    [{ kind: 'choice', options: [{ label: 'One', value: 1 }] }, /details\.options\[0\]\.value must be a string/i],
    [{ kind: 'branch', arms: [{ steps: '2' }] }, /details\.arms\[0\]\.steps must be a finite number/i],
    [{ kind: 'collector', fields: [{ name: 'region', type: 'select', options_from: { provider: 7, field: 'items' } }] }, /details\.fields\[0\]\.options_from\.provider must be a string/i],
    [{ kind: 'noop', common: { retry: { max: '2' } } }, /details\.common\.retry\.max must be a finite number/i],
    [{ kind: 'noop', common: { captures: [{ name: 'result', source: 9 }] } }, /details\.common\.captures\[0\]\.source must be a string/i],
    [{ kind: 'noop', common: { contract: { effects: ['read', 4] } } }, /details\.common\.contract\.effects\[1\] must be a string/i],
  ];

  for (const [details, expected] of invalidDetails) {
    const fixture = structuredClone(base);
    fixture.nodes[0].data.kind = details.kind;
    fixture.nodes[0].data.details = details;
    assert.throws(() => parseGraphDocument(JSON.stringify(fixture)), expected);
  }
});