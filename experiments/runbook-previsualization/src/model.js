/** @typedef {'assert' | 'decision' | 'end' | 'parallel' | 'tool'} StepKind */
/** @typedef {'all' | 'decisions' | 'exceptions'} Lens */
/** @typedef {{ id: string, title: string, kind: StepKind, x: number, y: number }} PreviewNode */
/** @typedef {{ id: string, source: string, target: string, label: string }} PreviewEdge */
/** @typedef {{ name: string, nodes: PreviewNode[], edges: PreviewEdge[] }} PreviewDocument */

const exceptionPattern = /fail|reject|rework|rollback|timeout/i;

/** @param {StepKind} kind */
export function kindGroup(kind) {
  if (kind === 'decision' || kind === 'parallel' || kind === 'assert') return 'control';
  if (kind === 'end') return 'outcome';
  return 'automation';
}

/** @param {PreviewDocument} document @param {Lens} lens */
export function emphasizedNodeIDs(document, lens) {
  if (lens === 'all') return new Set(document.nodes.map((node) => node.id));
  if (lens === 'decisions') {
    return new Set(document.nodes.filter((node) => node.kind === 'decision').map((node) => node.id));
  }
  const ids = new Set();
  for (const edge of document.edges) {
    if (exceptionPattern.test(edge.label)) {
      ids.add(edge.source);
      ids.add(edge.target);
    }
  }
  return ids;
}

/** @param {PreviewEdge} edge @param {Lens} lens */
export function edgeIsEmphasized(edge, lens) {
  return lens === 'all' || (lens === 'decisions' && edge.label.length > 0) ||
    (lens === 'exceptions' && exceptionPattern.test(edge.label));
}
