'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const { normalizeCollectorValues } = require('../out/collectorFieldValues');
const { parseRouteTestArtifact, validateRouteTestAgainstDocument } = require('../out/routeTestArtifacts');

const fields = [
  { name: 'confirmed', label: 'Confirmed', type: 'boolean', required: true },
  { name: 'ratio', label: 'Ratio', type: 'number', required: true },
  { name: 'retries', label: 'Retries', type: 'integer', required: true },
  { name: 'notes', label: 'Notes', type: 'text', required: true, multiple: true },
];

test('collector field normalization preserves declared boolean, number, integer, and multiple text types', () => {
  assert.deepEqual(normalizeCollectorValues(fields, {
    confirmed: true,
    ratio: '2.5',
    retries: '3',
    notes: ['primary is healthy', 'replica is catching up'],
  }), {
    confirmed: true,
    ratio: 2.5,
    retries: 3,
    notes: ['primary is healthy', 'replica is catching up'],
  });
});

test('collector field normalization rejects an omitted required boolean', () => {
  assert.throws(() => normalizeCollectorValues(fields, {
    ratio: '2.5',
    retries: '3',
    notes: ['primary is healthy'],
  }), /Confirmed is required/);
});

test('normalized boolean and string-array collector values produce a route-test artifact accepted by its graph document', () => {
  const values = normalizeCollectorValues(fields, {
    confirmed: false,
    ratio: '1.25',
    retries: '2',
    notes: ['first finding', 'second finding'],
  });
  const artifact = parseRouteTestArtifact({
    apiVersion: 'yawr.route-test/v1',
    id: 'typed-collector',
    name: 'Typed collector values',
    runbook: 'runbooks/typed.runbook.yaml',
    plan_hash: 'sha256:plan',
    sensitivity_reviewed: true,
    target: { step: 'target', phase: 'before', invocation: 1, attempt: 1 },
    interaction_answers: [{
      at: { step: 'findings', phase: 'execute', invocation: 1, attempt: 1 },
      kind: 'collector',
      values,
      source: { kind: 'manual' },
      review: { state: 'draft', sensitivity_reviewed: true },
    }],
  });
  const document = {
    schema_version: '1',
    hash: 'sha256:plan',
    runbook: {},
    inputs: [],
    edges: [],
    frames: [],
    groups: [],
    nodes: [{
      id: 'findings',
      position: { x: 0, y: 0 },
      data: { id: 'findings', step_id: 'findings', kind: 'collector', details: { kind: 'collector', fields } },
    }, {
      id: 'target',
      position: { x: 0, y: 0 },
      data: { id: 'target', step_id: 'target', kind: 'cli' },
    }],
  };

  assert.equal(typeof values.confirmed, 'boolean');
  assert.deepEqual(values.notes, ['first finding', 'second finding']);
  assert.doesNotThrow(() => validateRouteTestAgainstDocument(artifact, document));
});