interface LayoutNode {
  id: string;
  type?: string;
  width?: number | null;
  height?: number | null;
  style?: { width?: unknown; height?: unknown };
}

// React Flow's controlled node updates replace, rather than merge, measured dimensions.
export function preserveLayoutMeasurements<T extends LayoutNode>(layout: readonly T[], measured: readonly T[]): T[] {
  const previous = new Map(measured.map(node => [node.id, node]));
  return layout.map(node => {
    const current = previous.get(node.id);
    if (!current || current.type !== node.type ||
        current.style?.width !== node.style?.width || current.style?.height !== node.style?.height) return node;
    return { ...node, width: current.width, height: current.height };
  });
}
