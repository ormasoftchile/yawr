const test = require('node:test');
const assert = require('node:assert/strict');
const { preserveLayoutMeasurements } = require('../out/graphLayoutMeasurements');
const { applyNodeChanges } = require('reactflow');
const node = (id, extra = {}) => ({ id, type: 'yawrStep', style: { width: 196, height: 74 },
  position: { x: 10, y: 20 }, data: { kind: 'include' }, ...extra });

test('runtime and metadata updates preserve controlled React Flow measurements without mutating canonical layout', () => {
  const layout = [node('a')], before = JSON.stringify(layout);
  let measured = applyNodeChanges([{ id: 'a', type: 'dimensions', dimensions: { width: 196, height: 74 } }], layout);
  for (const status of ['starting', 'running', 'completed', 'failed']) {
    const next = layout.map(n => ({ ...n, data: { ...n.data, status } }));
    measured = preserveLayoutMeasurements(next, measured);
    assert.equal(measured[0].width, 196);
    assert.equal(measured[0].height, 74);
    assert.equal(measured[0].data.status, status);
    assert.deepEqual(measured[0].position, layout[0].position);
  }
  assert.equal(JSON.stringify(layout), before);
});

test('layout moves, selection and group metadata refresh keep measurements but never stale data or positions', () => {
  const measured = [node('a', { width: 196, height: 74, selected: false })];
  const next = node('a', { selected: true, parentNode: 'new-group', position: { x: 90, y: 100 },
    data: { kind: 'include', title: 'New revision', group_id: 'new-group' } });
  assert.deepEqual(preserveLayoutMeasurements([next], measured), [{ ...next, width: 196, height: 74 }]);
});

test('projection toggles, new namespace and changed renderer sizes measure new nodes instead of copying obsolete geometry', () => {
  const measured = [node('a', { width: 196, height: 74 }), node('removed', { width: 196, height: 74 })];
  for (const next of [
    node('session:a'), node('a', { type: 'technicalSegment' }),
    node('a', { style: { width: 240, height: 90 } }),
  ]) assert.deepEqual(preserveLayoutMeasurements([next], measured), [next]);
  assert.deepEqual(preserveLayoutMeasurements([], measured), []);
  assert.equal(preserveLayoutMeasurements([node('a')], measured).length, 1);
});
