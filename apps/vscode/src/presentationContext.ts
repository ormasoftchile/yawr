import * as fs from 'fs';
import * as path from 'path';
import { pickProjectRoot } from './projectRoot';

function contains(root: string, file: string): boolean {
  const relative = path.relative(root, file);
  return relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative);
}
function fileExists(file: string): boolean {
  try { return fs.statSync(file).isFile(); } catch { return false; }
}
export function presentationProjectRoot(document: string, folders: readonly string[], fallback: string, configuredMap: string): string {
  const nearest = pickProjectRoot(document, folders, fallback);
  if (!configuredMap) return nearest;
  const mapAt = (root: string) => path.resolve(root, configuredMap);
  if (contains(nearest, mapAt(nearest)) && fileExists(mapAt(nearest))) return nearest;
  // A package's own tools/runbooks directories are not the project that owns
  // an explicitly configured workspace map. Require both document containment
  // and the actual map file; never infer language or change caller settings.
  const candidates = folders.filter(folder => contains(folder, document))
    .map(folder => pickProjectRoot(path.join(folder, '_'), [folder], fallback))
    .filter(root => contains(root, document) && contains(root, mapAt(root)) && fileExists(mapAt(root)))
    .sort((a, b) => b.length - a.length);
  return candidates[0] ?? nearest;
}
