import * as path from 'node:path';
import type { GraphDocument } from './directGraphPreview';

export function graphSourceChanged(
  fileName: string, runbookPath: string, projectRoot: string,
  document?: GraphDocument, packageMapPath?: string,
): boolean {
  const same = (other: string) => path.relative(fileName, other) === '';
  if (same(runbookPath) || (packageMapPath && same(packageMapPath)) ||
      document?.frames.some(frame => same(frame.runbook_path))) return true;
  const relative = path.relative(projectRoot, fileName);
  return !path.isAbsolute(relative) && relative !== '..' && !relative.startsWith(`..${path.sep}`) &&
    /\.tool\.ya?ml$/i.test(fileName);
}
