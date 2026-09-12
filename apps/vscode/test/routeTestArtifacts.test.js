'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const {
  loadRouteTestArtifacts,
  parseRouteTestArtifact,
  saveRouteTestArtifact,
  stampRouteTestResultDigest,
  validateRouteTestAgainstDocument,
} = require('../out/routeTestArtifacts');

function artifact(overrides = {}) {
  const value = {
    apiVersion: 'yawr.route-test/v1',
    id: 'primary-unavailable',
    name: 'Primary unavailable reaches failover',
    runbook: 'runbooks/failover.runbook.yaml',
    plan_hash: 'sha256:plan',
    sensitivity_reviewed: true,
    target: { call_path: ['route_finding'], step: 'execute_failover', phase: 'before', invocation: 1, attempt: 1 },
    inputs: { server: 'db01' },
    host_action_responses: [{
      at: { call_path: ['inspect_replication'], step: 'open_xts_view', phase: 'execute', invocation: 1, attempt: 1 },
      capability: 'xts.open-view',
      response: { status: 'completed', result: { status: 'opened' } },
      source: { kind: 'manual' },
      review: { state: 'reviewed', reviewed_by: 'operator', reviewed_at: '2026-08-28T12:05:00Z', sensitivity_reviewed: true },
    }],
    interaction_answers: [{
      at: { call_path: ['inspect_replication', 'handle_xts_launch'], step: 'record_findings', phase: 'execute', invocation: 1, attempt: 1 },
      kind: 'collector',
      values: { primary_health: 'unavailable' },
      source: { kind: 'manual' },
      review: { state: 'reviewed', reviewed_by: 'operator', reviewed_at: '2026-08-28T12:05:00Z', sensitivity_reviewed: true },
    }],
    last_result: { status: 'reached', target_reached: true, external_dispatches: 0, ran_at: '2026-08-28T12:10:00Z' },
    ...overrides,
  };
  return value.last_result ? stampRouteTestResultDigest(value) : value;
}

test('parseRouteTestArtifact accepts a bounded reviewed artifact and rejects unknown fields', () => {
  assert.equal(parseRouteTestArtifact(artifact()).target.step, 'execute_failover');
  assert.throws(
    () => parseRouteTestArtifact({ ...artifact(), surprise: true }),
    /unknown field surprise/,
  );
  assert.throws(
    () => parseRouteTestArtifact(artifact({ id: '..\\escape' })),
    /id/,
  );
  assert.throws(
    () => parseRouteTestArtifact(artifact({ runbook: 'C:\\outside.runbook.yaml' })),
    /project-relative/,
  );
  assert.throws(() => parseRouteTestArtifact(artifact({ plan_hash: undefined })), /plan_hash/);
  assert.throws(() => parseRouteTestArtifact(artifact({ last_result: undefined, sensitivity_reviewed: false })), /sensitivity_reviewed/);
  const unreviewedSensitivity = artifact();
  unreviewedSensitivity.host_action_responses[0].review.sensitivity_reviewed = false;
  assert.throws(() => parseRouteTestArtifact(unreviewedSensitivity), /sensitivity review/);
  const draftSensitivity = artifact({ last_result: undefined });
  draftSensitivity.host_action_responses[0].review = { state: 'draft', sensitivity_reviewed: false };
  assert.throws(() => parseRouteTestArtifact(draftSensitivity), /sensitivity review/);
  const incompletePriorRun = artifact();
  incompletePriorRun.interaction_answers[0].source = { kind: 'prior-run' };
  assert.throws(() => parseRouteTestArtifact(incompletePriorRun), /prior-run provenance/);
  const staleResult = artifact();
  staleResult.interaction_answers[0].values.primary_health = 'healthy';
  assert.throws(() => parseRouteTestArtifact(staleResult), /does not match the current route-test conditions/);
  assert.throws(() => parseRouteTestArtifact(artifact({
    last_result: undefined,
    host_action_responses: [{
      at: { step: 'open', phase: 'execute', invocation: 1, attempt: 1 }, capability: 'test',
      response: { status: 'completed', result: { private_key: 'TOP-SECRET' } },
      source: { kind: 'manual' }, review: { state: 'reviewed', reviewed_by: 'operator', reviewed_at: '2026-08-28T12:05:00Z', sensitivity_reviewed: true },
    }],
  })), /Sensitive result key private_key/);
  assert.doesNotThrow(() => parseRouteTestArtifact(artifact({ last_result: undefined, inputs: { monkey: 'safe', keynote: 'safe' } })));
  const explicitEmptyPath = artifact({ last_result: undefined });
  explicitEmptyPath.target = { call_path: [], step: 'execute_failover', phase: 'before', invocation: 1, attempt: 1 };
  assert.equal(parseRouteTestArtifact(explicitEmptyPath).target.call_path, undefined);
});

test('validateRouteTestAgainstDocument rejects secret inputs and ephemeral findings', () => {
  const document = {
    schema_version: '1', hash: 'sha256:plan', runbook: {}, edges: [], frames: [], groups: [],
    inputs: [{ name: 'token', type: 'secret' }],
    nodes: [{
      id: 'findings', position: { x: 0, y: 0 },
      data: { id: 'findings', step_id: 'findings', kind: 'collector', details: { kind: 'collector', fields: [{ name: 'note', type: 'text', ephemeral: true }] } },
    }, {
      id: 'target', position: { x: 0, y: 0 }, data: { id: 'target', step_id: 'target', kind: 'cli' },
    }],
  };
  assert.throws(
    () => validateRouteTestAgainstDocument(artifact({
      target: { step: 'target', phase: 'before', invocation: 1, attempt: 1 }, inputs: { token: 'secret' },
      host_action_responses: [], interaction_answers: [],
    }), document),
    /(Sensitive|Secret) input.*token/,
  );
  assert.throws(
    () => validateRouteTestAgainstDocument(artifact({
      target: { step: 'target', phase: 'before', invocation: 1, attempt: 1 }, inputs: {}, host_action_responses: [],
      interaction_answers: [{
        at: { step: 'findings', phase: 'execute', invocation: 1, attempt: 1 }, kind: 'collector', values: { note: 'sensitive' },
        source: { kind: 'manual' }, review: { state: 'reviewed', reviewed_by: 'operator', reviewed_at: '2026-08-28T12:05:00Z', sensitivity_reviewed: true },
      }],
    }), document),
    /ephemeral/,
  );
});

test('validateRouteTestAgainstDocument rejects an omitted required boolean finding', () => {
  const document = {
    schema_version: '1', hash: 'sha256:plan', runbook: {}, inputs: [], edges: [], frames: [], groups: [],
    nodes: [{
      id: 'findings', position: { x: 0, y: 0 },
      data: {
        id: 'findings', step_id: 'findings', kind: 'collector',
        details: { kind: 'collector', fields: [{ name: 'confirmed', label: 'Confirmed', type: 'boolean', required: true }] },
      },
    }, {
      id: 'target', position: { x: 0, y: 0 }, data: { id: 'target', step_id: 'target', kind: 'cli' },
    }],
  };
  const missingBoolean = artifact({
    last_result: undefined,
    target: { step: 'target', phase: 'before', invocation: 1, attempt: 1 },
    inputs: {},
    host_action_responses: [],
    interaction_answers: [{
      at: { step: 'findings', phase: 'execute', invocation: 1, attempt: 1 }, kind: 'collector', values: {},
      source: { kind: 'manual' }, review: { state: 'draft', sensitivity_reviewed: true },
    }],
  });

  assert.throws(() => validateRouteTestAgainstDocument(missingBoolean, document), /Confirmed is required/);
});

test('saveRouteTestArtifact writes atomically under the project and list reloads it', async (t) => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'yawr-route-tests-'));
  t.after(() => fs.rm(root, { recursive: true, force: true }));

  const saved = await saveRouteTestArtifact(root, artifact());
  assert.equal(saved, path.join(root, '.yawr', 'route-tests', 'primary-unavailable.route-test.yaml'));
  assert.match(await fs.readFile(saved, 'utf8'), /apiVersion: yawr\.route-test\/v1/);

  const loaded = await loadRouteTestArtifacts(root, 'runbooks/failover.runbook.yaml');
  assert.equal(loaded.artifacts.length, 1);
  assert.equal(loaded.artifacts[0].artifact.name, 'Primary unavailable reaches failover');
  assert.equal(loaded.artifacts[0].artifact.last_result.status, 'reached');
  assert.equal(loaded.warnings.length, 0);
});

test('loadRouteTestArtifacts isolates invalid or unrelated artifacts', async (t) => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'yawr-route-tests-'));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const directory = path.join(root, '.yawr', 'route-tests');
  await fs.mkdir(directory, { recursive: true });
  await saveRouteTestArtifact(root, artifact({ id: 'other', runbook: 'runbooks/other.runbook.yaml' }));
  await fs.writeFile(path.join(directory, 'broken.route-test.yaml'), 'apiVersion: yawr.route-test/v1\nunknown: true\n');

  const loaded = await loadRouteTestArtifacts(root, 'runbooks/failover.runbook.yaml');
  assert.equal(loaded.artifacts.length, 0);
  assert.equal(loaded.warnings.length, 1);
});