import * as fs from 'fs';
import * as path from 'path';

function hasDirectory(dir: string, name: string): boolean {
  try {
    return fs.statSync(path.join(dir, name)).isDirectory();
  } catch {
    return false;
  }
}

function isYawrProjectRoot(dir: string): boolean {
  const hasRunbooks = hasDirectory(dir, 'runbooks');
  return hasRunbooks && (
    hasDirectory(dir, '.yawr') ||
    hasDirectory(dir, 'packages') ||
    hasDirectory(dir, 'tools')
  );
}

function findYawrProjectRoot(start: string): string | undefined {
  let dir = start;
  while (true) {
    if (isYawrProjectRoot(dir)) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) return undefined;
    dir = parent;
  }
}

// Choose the narrowest Yawr project root for the active runbook so discovery
// and package-map resolution cannot drift into unrelated workspace folders.
export function pickProjectRoot(
  runbookPath: string,
  workspaceFolders: readonly string[],
  fallback: string,
): string {
  const fromRunbook = findYawrProjectRoot(path.dirname(runbookPath));
  if (fromRunbook) return fromRunbook;

  for (const folder of workspaceFolders) {
    const fromWorkspace = findYawrProjectRoot(folder);
    if (fromWorkspace) return fromWorkspace;
  }

  return path.dirname(runbookPath) || workspaceFolders[0] || fallback;
}
