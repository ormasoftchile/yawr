const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createHash } = require('node:crypto');
const { spawnSync } = require('node:child_process');
const { parseGraphDocument } = require('../out/directGraphPreview');
const { withBranchMerges } = require('../out/branchTopology');
const { projectWorkflow, workflowIssueIndex } = require('../out/workflowProjection');
const { value } = require('../scripts/environment.cjs');
const artifactRoot = value('WORKFLOW_ARTIFACTS');
const candidate = value('WORKFLOW_PREVIEW_BINARY');
assert.ok(artifactRoot && candidate, 'Explicit task artifacts and approved preview executable required');
const inspection = JSON.parse(fs.readFileSync(path.join(artifactRoot, 'inspection.json'), 'utf8'));
const root = inspection.fixtureRoot;
for (const entry of inspection.inspectedFixtureHashes) {
  assert.equal(createHash('sha256').update(fs.readFileSync(path.join(root, entry.file))).digest('hex'), entry.sha256,
    `Binding changed: ${entry.file}. Stop and reinspect before executing anything.`);
}
const outputRoot = path.join(artifactRoot, 'fixture-projection');
fs.mkdirSync(outputRoot, { recursive: true });
const evidence = [];
for (const name of ['prefilled-singleton', 'prefilled-invalid-window']) {
  const result = spawnSync(candidate, ['preview', '--format', 'graphjson', '--recurse',
    '--package-map', path.join(root, 'tests', 'icm-db-info-probe', 'synthetic.package-map.yaml'),
    path.join(root, 'tests', 'icm-db-info-probe', name + '.runbook.yaml')], {
    cwd: root, encoding: 'utf8', timeout: 60000, maxBuffer: 16 * 1024 * 1024,
    env: { ...process.env, YAWR_TELEMETRY_DISABLED: '1', HTTP_PROXY: 'http://127.0.0.1:9', HTTPS_PROXY: 'http://127.0.0.1:9' },
  });
  assert.equal(result.status, 0, result.stderr);
  const canonical = parseGraphDocument(result.stdout), before = JSON.stringify(canonical);
  const structural = withBranchMerges(canonical);
  const options = { mode: 'workflow', expandedNodeIDs: new Set(), pinnedNodeIDs: new Set(), collapsedGroupIDs: new Set() };
  const workflow = projectWorkflow(structural, {}, options);
  assert.ok(workflow.segments.size > 0);
  const all = projectWorkflow(structural, {}, { ...options, mode: 'all' });
  assert.deepEqual(all.document.nodes.map(node => node.id), structural.nodes.map(node => node.id));
  assert.deepEqual(all.document.edges.map(edge => edge.id), structural.edges.map(edge => edge.id));
  const covered = new Set(workflow.document.edges.map(edge => edge.id));
  for (const segment of workflow.segments.values()) for (const edge of segment.internalEdgeIDs) covered.add(edge);
  assert.deepEqual([...covered].sort(), structural.edges.map(edge => edge.id).sort());
  const failedID = 'launch_scenario/validate_query_scope';
  assert.ok(canonical.nodes.some(node => node.id === failedID), 'actual supplied fixture qualified failure node');
  const runtime = { [failedID]: { status: 'failed', occurrences: [{ status: 'failed', occurrenceID: 'design-only-failure', qualifiedNodeID: failedID }] } };
  const failure = projectWorkflow(structural, runtime, options, { ...structural, nodes: [], edges: [] });
  assert.ok(failure.document.nodes.some(node => node.id === failedID));
  assert.ok(failure.forcedNodeIDs.has('launch_scenario'));
  assert.equal(workflowIssueIndex(runtime)[0].nodeID, failedID);
  assert.equal(JSON.stringify(canonical), before);
  fs.writeFileSync(path.join(outputRoot, name + '.graph.json'), result.stdout);
  evidence.push({ fixture: name, canonical: canonical.nodes.length, structural: structural.nodes.length,
    segments: workflow.segments.size, visible: workflow.document.nodes.length, edges: workflow.document.edges.length,
    expandedFailureID: failedID, execution: false });
}
fs.writeFileSync(path.join(outputRoot, 'fixture-projection-results.json'), JSON.stringify({
  note: 'Real supplied fixture graph projection; preview only. Injected failure is a design vector, not runtime evidence.',
  candidateSHA256: createHash('sha256').update(fs.readFileSync(candidate)).digest('hex'), evidence,
}, null, 2));
console.log(JSON.stringify(evidence));
