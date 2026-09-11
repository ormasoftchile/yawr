import * as fs from 'fs';
import * as path from 'path';
import * as vscode from 'vscode';
import { binarySearchRoots, relativeBinaryCandidates } from './binaryPaths';
import { bundledPresentationHelper, verifyPresentationHelper } from './presentationClient';

let bundledRoot: string | undefined;
export function configureBundledRuntime(extensionPath: string): void { bundledRoot = extensionPath; }

// resolveBinary tries deterministic local locations rather than relying on the
// Extension Host's stripped PATH. The active project is always searched first.
export async function resolveBinary(
  configured: string,
  output: vscode.OutputChannel,
  activeProjectRoot: string,
  workspaceFolders: readonly string[],
): Promise<string> {
  const candidates: string[] = [];
  const useDefaultDiscovery = !configured || configured === 'yawr';
  if (useDefaultDiscovery && bundledRoot) {
    const helper = bundledPresentationHelper(bundledRoot);
    let present = false;
    try {
      await fs.promises.access(helper, fs.constants.X_OK);
      present = true;
    } catch {
      // An unpackaged development build may still use local discovery.
    }
    if (present) {
      await verifyPresentationHelper(helper);
      output.appendLine('[Yawr] using matching packaged runtime');
      return helper;
    }
  }

  if (path.isAbsolute(configured)) {
    candidates.push(configured);
  } else if (!useDefaultDiscovery) {
    candidates.push(...relativeBinaryCandidates(configured, activeProjectRoot, workspaceFolders));
  }

  if (useDefaultDiscovery) {
    const seen = new Set<string>();
    for (const root of binarySearchRoots(activeProjectRoot, workspaceFolders)) {
      let dir = root;
      for (let depth = 0; depth < 6; depth++) {
        if (seen.has(dir)) break;
        seen.add(dir);
        candidates.push(path.join(dir, 'yawr'));
        candidates.push(path.join(dir, 'bin', 'yawr'));
        const parent = path.dirname(dir);
        if (parent === dir) break;
        dir = parent;
      }
    }

    const home = process.env.HOME || process.env.USERPROFILE;
    if (home) {
      candidates.push(path.join(home, 'go', 'bin', 'yawr'));
    }
    if (process.env.GOPATH) {
      candidates.push(path.join(process.env.GOPATH, 'bin', 'yawr'));
    }
    if (process.env.PATH) {
      for (const dir of process.env.PATH.split(path.delimiter)) {
        if (dir) {
          candidates.push(path.join(dir, 'yawr'));
        }
      }
    }
  }

  const executableCandidates = process.platform === 'win32'
    ? candidates.flatMap((candidate) => path.extname(candidate) ? [candidate] : [candidate, `${candidate}.exe`])
    : candidates;

  for (const candidate of executableCandidates) {
    try {
      const stat = await fs.promises.stat(candidate);
      if (!stat.isFile()) continue;
      await fs.promises.access(candidate, fs.constants.X_OK);
      output.appendLine(`[Yawr] resolved binary: ${candidate}`);
      return candidate;
    } catch {
      // Try the next candidate.
    }
  }

  throw new Error(
    `cannot find Yawr helper on disk. Tried:\n  ${executableCandidates.join('\n  ')}\n` +
    'Set "yawr.binaryPath" in settings to an absolute path.',
  );
}
