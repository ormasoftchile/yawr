/** @type {import('./model.js').PreviewDocument} */
export const releaseReadiness = {
  name: 'Release readiness',
  nodes: [
    { id: 'validate', title: 'Validate package', kind: 'assert', x: 60, y: 60 },
    { id: 'checks', title: 'Run checks', kind: 'parallel', x: 60, y: 190 },
    { id: 'review', title: 'Review results', kind: 'decision', x: 60, y: 320 },
    { id: 'publish', title: 'Publish release', kind: 'tool', x: 360, y: 470 },
    { id: 'revise', title: 'Revise package', kind: 'tool', x: 60, y: 470 },
    { id: 'done', title: 'Release complete', kind: 'end', x: 360, y: 600 }
  ],
  edges: [
    { id: 'e1', source: 'validate', target: 'checks', label: '' },
    { id: 'e2', source: 'checks', target: 'review', label: '' },
    { id: 'e3', source: 'review', target: 'publish', label: 'approved' },
    { id: 'e4', source: 'review', target: 'revise', label: 'rework required' },
    { id: 'e5', source: 'publish', target: 'done', label: '' }
  ]
};
