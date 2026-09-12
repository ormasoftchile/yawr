import assert from 'node:assert/strict';
import test from 'node:test';
import { releaseReadiness } from './src/fixture.js';
import { edgeIsEmphasized, emphasizedNodeIDs, kindGroup } from './src/model.js';

test('semantic groups are derived from current neutral step kinds', () => {
  assert.equal(kindGroup('decision'), 'control');
  assert.equal(kindGroup('tool'), 'automation');
  assert.equal(kindGroup('end'), 'outcome');
});

test('decision lens emphasizes only decision nodes', () => {
  assert.deepEqual([...emphasizedNodeIDs(releaseReadiness, 'decisions')], ['review']);
});

test('exception lens includes both ends of exceptional routes', () => {
  assert.deepEqual([...emphasizedNodeIDs(releaseReadiness, 'exceptions')].sort(), ['review', 'revise']);
  assert.equal(edgeIsEmphasized(releaseReadiness.edges[3], 'exceptions'), true);
  assert.equal(edgeIsEmphasized(releaseReadiness.edges[2], 'exceptions'), false);
});
