import * as path from 'path';
import * as vscode from 'vscode';
import { presentationProjectRoot } from './presentationContext';
import { resolveRunPackageMapPath } from './runHandoff';
import type { ResolveContext, SourceBuffer } from './presentationProtocol';
import { getSetting } from './identity';

const knownContexts = new Map<string, { project_root: string; entrypoint_path: string }>();
const changes = new vscode.EventEmitter<void>();
export const onDidChangeAuthoringContext = changes.event;
export function setPresentationEntrypoint(projectRoot: string, entrypointPath: string, includedPaths: readonly string[]): void {
  knownContexts.clear();
  for (const file of [entrypointPath, ...includedPaths]) {
    if (path.isAbsolute(file)) knownContexts.set(vscode.Uri.file(file).toString(), { project_root: projectRoot, entrypoint_path: entrypointPath });
  }
  changes.fire();
}
export function eligibleAuthoringDocument(document: vscode.TextDocument): boolean {
  return document.languageId === 'yaml' && !/^\.env(?:\.|$)/i.test(path.basename(document.fileName)) &&
    (document.isUntitled || document.uri.scheme === 'file' && /\.runbook\.ya?ml$/i.test(document.fileName));
}
export function captureAuthoringContext(document: vscode.TextDocument, extensionPath: string, generation: number):
  { context: ResolveContext; document: SourceBuffer; overlays: SourceBuffer[]; known: boolean } {
  const folders = vscode.workspace.workspaceFolders?.map(folder => folder.uri.fsPath) ?? [];
  const virtualPath = document.isUntitled
    ? path.isAbsolute(document.uri.fsPath) ? document.uri.fsPath
      : path.join(folders[0] ?? extensionPath, `.yawr-untitled-${encodeURIComponent(document.uri.toString())}.runbook.yaml`) : document.uri.fsPath;
  const known = knownContexts.get(document.uri.toString()) ??
    (document.isUntitled ? knownContexts.get(vscode.Uri.file(virtualPath).toString()) : undefined);
  const configuredMap = getSetting('packageMap', document.uri, '');
  const root = known?.project_root ?? presentationProjectRoot(virtualPath, folders, extensionPath, configuredMap);
  const buffer = (doc: vscode.TextDocument): SourceBuffer => ({
    uri: doc.uri.toString(), path: doc === document ? virtualPath : path.isAbsolute(doc.uri.fsPath) ? doc.uri.fsPath
      : path.join(root, `.yawr-untitled-${encodeURIComponent(doc.uri.toString())}.yaml`), version: doc.version, text: doc.getText(),
  });
  const overlays = vscode.workspace.textDocuments.filter(doc => doc !== document && (doc.uri.scheme === 'file' || doc.isUntitled) &&
    (doc.languageId === 'yaml' || /(?:yawr-package|package-map|config)\.ya?ml$/i.test(doc.fileName)) &&
    !/^\.env(?:\.|$)/i.test(path.basename(doc.fileName))).map(buffer);
  const packageMap = resolveRunPackageMapPath(root, configuredMap).path ||
    (configuredMap ? path.resolve(root, configuredMap) :
      overlays.some(b => b.path === path.join(root, 'package-map.yaml')) ? path.join(root, 'package-map.yaml') : undefined);
  return {
    context: { project_root: root, generation, ...(known ? { entrypoint_path: known.entrypoint_path } : {}),
      ...(packageMap ? { package_map_path: packageMap } : {}) },
    document: buffer(document), overlays, known: !!known,
  };
}
