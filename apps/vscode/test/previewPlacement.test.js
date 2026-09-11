'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');

const { resolvePreviewPanelTarget } = require('../out/previewPlacement');

test('resolvePreviewPanelTarget: sameGroup is the default', () => {
  assert.equal(resolvePreviewPanelTarget(undefined, 2), 2);
  assert.equal(resolvePreviewPanelTarget('unknown', 2), 2);
});

test('resolvePreviewPanelTarget: beside remains available as an explicit choice', () => {
  assert.equal(resolvePreviewPanelTarget('beside', 2), 'beside');
});

test('resolvePreviewPanelTarget: sameGroup uses the captured runbook editor column', () => {
  assert.equal(resolvePreviewPanelTarget('sameGroup', 3), 3);
});

test('resolvePreviewPanelTarget: sameGroup falls back to the active editor group', () => {
  assert.equal(resolvePreviewPanelTarget('sameGroup', undefined), 'active');
});

test('compiled extension applies preview.openLocation when creating the graph panel', () => {
  const extensionSource = fs.readFileSync(
    path.join(__dirname, '..', 'out', 'extension.js'),
    'utf8',
  );

  assert.match(extensionSource, /preview\.openLocation/);
  assert.match(extensionSource, /resolvePreviewPanelTarget/);
  assert.match(extensionSource, /runbookViewColumn/);
});