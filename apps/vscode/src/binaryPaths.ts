import * as path from 'path';

export function binarySearchRoots(
  activeProjectRoot: string,
  workspaceFolders: readonly string[],
): string[] {
  const seen = new Set<string>();
  return [activeProjectRoot, ...workspaceFolders].filter((root) => {
    const normalized = path.normalize(root);
    const key = process.platform === 'win32' ? normalized.toLowerCase() : normalized;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

export function relativeBinaryCandidates(
  configured: string,
  activeProjectRoot: string,
  workspaceFolders: readonly string[],
): string[] {
  if (!configured || configured === 'yawr' || configured === 'yawr' || path.isAbsolute(configured)) return [];
  return binarySearchRoots(activeProjectRoot, workspaceFolders)
    .map((root) => path.join(root, configured));
}
