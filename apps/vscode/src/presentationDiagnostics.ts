import * as fs from 'fs';
import { createHash } from 'crypto';
import type { CodeHelperTimings } from './presentationClient';
import type { ResolveRequest } from './presentationProtocol';

export function helperIdentity(binary: string) {
  try {
    const info = fs.statSync(binary);
    return { path: binary, realPath: fs.realpathSync(binary), bytes: info.size,
      modifiedMs: info.mtimeMs, changedMs: info.ctimeMs };
  } catch { return { path: binary }; }
}

export function presentationDiagnostic(
  helper: ReturnType<typeof helperIdentity>, request: ResolveRequest, knownEntrypoint: boolean,
  code: { status?: string; reason?: string; timings: CodeHelperTimings }, expressions: { status?: string; durationMs: number },
) {
  // Whitelist metadata; never retain the request, source, bindings or stderr.
  return {
    diagnosticVersion: 1, capturedAt: new Date().toISOString(), helper,
    context: { projectRoot: request.context.project_root, entrypoint: request.context.entrypoint_path ?? null,
      packageMap: request.context.package_map_path ?? null, knownEntrypoint, generation: request.context.generation },
    document: { uri: request.document.uri, version: request.document.version, utf16Count: request.document.text.length },
    overlays: { count: request.overlays.length, utf8Bytes: request.overlays.reduce((n, b) => n + Buffer.byteLength(b.text, 'utf8'), 0) },
    code: { status: code.status ?? 'unavailable', reason: code.reason ?? null, timings: code.timings },
    expressions: { status: expressions.status ?? 'unavailable', durationMs: expressions.durationMs },
  };
}
export type PresentationDiagnostic = ReturnType<typeof presentationDiagnostic>;

export async function diagnosticHelperBuild(helper: ReturnType<typeof helperIdentity>) {
  const before = helperIdentity(helper.path);
  if (JSON.stringify(before) !== JSON.stringify(helper) || !before.bytes) return { sha256: null, reason: 'helper-changed-or-unavailable' };
  try {
    const hash = createHash('sha256');
    for await (const chunk of fs.createReadStream(helper.path)) hash.update(chunk);
    if (JSON.stringify(helperIdentity(helper.path)) !== JSON.stringify(before)) return { sha256: null, reason: 'helper-changed' };
    return { sha256: hash.digest('hex').toUpperCase(), reason: 'ok' };
  } catch { return { sha256: null, reason: 'helper-unavailable' }; }
}
