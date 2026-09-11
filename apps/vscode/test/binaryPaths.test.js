'use strict';

const assert = require('node:assert/strict');
const path = require('node:path');
const test = require('node:test');

const { binarySearchRoots, relativeBinaryCandidates } = require('../out/binaryPaths');

test('binary search roots prefer the active project and remove duplicates', () => {
  const projectA = path.join('C:', 'workspace', 'project-a');
  const projectB = path.join('C:', 'workspace', 'project-b');

  assert.deepEqual(binarySearchRoots(projectB, [projectA, projectB]), [projectB, projectA]);
});

test('relative binary candidates are rooted in active-project order', () => {
  const configured = path.join('bin', 'yawr.exe');
  const projectA = path.join('C:', 'workspace', 'project-a');
  const projectB = path.join('C:', 'workspace', 'project-b');

  assert.deepEqual(
    relativeBinaryCandidates(configured, projectB, [projectA, projectB]),
    [path.join(projectB, configured), path.join(projectA, configured)],
  );
  assert.deepEqual(relativeBinaryCandidates('yawr', projectB, [projectA]), []);
  assert.deepEqual(relativeBinaryCandidates('yawr', projectB, [projectA]), []);
});
