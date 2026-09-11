import { spawn } from 'child_process';
import * as path from 'path';
import { parseAuthoringJSON } from './authoringProtocol';
import { MAX_SOURCE_BYTES, decodeCapabilities, decodeReply, type ResolveReply, type ResolveRequest } from './presentationProtocol';

export interface HelperTiming {
  queueMs: number; processMs: number | null; totalMs: number; reason: string;
}
type TimingObserver = (timing: HelperTiming) => void;
export interface CodeHelperTimings { capabilities?: HelperTiming; resolve?: HelperTiming }
let active = 0;
const waiters: Array<() => void> = [];
async function slot(signal?: AbortSignal): Promise<() => void> {
  if (signal?.aborted) throw new Error('stale-request');
  await new Promise<void>((resolve, reject) => {
    const grant = () => { signal?.removeEventListener('abort', abort); active++; resolve(); };
    const abort = () => {
      const index = waiters.indexOf(grant);
      if (index >= 0) waiters.splice(index, 1);
      reject(new Error('stale-request'));
    };
    if (active < 2) grant();
    else { waiters.push(grant); signal?.addEventListener('abort', abort, { once: true }); }
  });
  return () => { active--; waiters.shift()?.(); };
}
export function bundledPresentationHelper(extensionPath: string): string {
  return path.join(extensionPath, 'bin', `${process.platform}-${process.arch}`, process.platform === 'win32' ? 'yawr.exe' : 'yawr');
}
export async function finiteHelper(binary: string, args: string[], input = '', signal?: AbortSignal, observe?: TimingObserver,
  parse: (bytes: Buffer) => unknown = parseAuthoringJSON): Promise<unknown> {
  const started = performance.now();
  let spawned: number | undefined, release: (() => void) | undefined, reason = 'ok';
  try {
    if (Buffer.byteLength(input, 'utf8') > MAX_SOURCE_BYTES) throw new Error('limit-exceeded');
    release = await slot(signal);
    return await new Promise<unknown>((resolve, reject) => {
      let done = false, stdoutSize = 0, stderrSize = 0;
      const chunks: Buffer[] = [];
      spawned = performance.now();
      const child = spawn(binary, args, { windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'], shell: false });
      const finish = (error?: string, value?: unknown) => {
        if (done) return;
        done = true;
        clearTimeout(timer);
        signal?.removeEventListener('abort', aborted);
        if (error) { child.kill(); reject(new Error(error)); } else resolve(value);
      };
      const aborted = () => finish('stale-request');
      const timer = setTimeout(() => finish('helper-deadline'), 5000);
      signal?.addEventListener('abort', aborted, { once: true });
      if (signal?.aborted) { aborted(); return; }
      child.on('error', () => finish('helper-unavailable'));
      child.stdin.on('error', () => finish('helper-unavailable'));
      child.stdout.on('data', (chunk: Buffer) => {
        stdoutSize += chunk.length;
        if (stdoutSize > MAX_SOURCE_BYTES) finish('limit-exceeded');
        else chunks.push(chunk);
      });
      // Diagnostics are counted, never retained/logged: they may contain tool source.
      child.stderr.on('data', (chunk: Buffer) => {
        stderrSize += chunk.length;
        if (stderrSize > MAX_SOURCE_BYTES) finish('limit-exceeded');
      });
      child.on('close', (code, signal) => {
        if (code !== 0) {
          finish('helper-unavailable');
          return;
        }
        try { finish(undefined, parse(Buffer.concat(chunks))); }
        catch { finish('invalid-helper-response'); }
      });
      child.stdin.end(input, 'utf8');
    });
  } catch (error) {
    reason = error instanceof Error && /^(?:limit-exceeded|stale-request|helper-unavailable|helper-deadline|invalid-helper-response)$/.test(error.message)
      ? error.message : 'helper-unavailable';
    throw error;
  } finally {
    release?.();
    const ended = performance.now();
    // Observability must not change queue ownership, cancellation or the result.
    try { observe?.({ queueMs: Math.round((spawned ?? ended) - started),
      processMs: spawned === undefined ? null : Math.round(ended - spawned), totalMs: Math.round(ended - started), reason }); } catch { /* Diagnostic sink only. */ }
  }
}
export async function verifyPresentationHelper(binary: string, signal?: AbortSignal, observe?: TimingObserver): Promise<void> {
  decodeCapabilities(await finiteHelper(binary, ['presentation', 'capabilities', '--v3'], '', signal, observe), 3);
}
export async function resolvePresentation(binary: string, request: ResolveRequest, signal?: AbortSignal, observe?: TimingObserver): Promise<ResolveReply> {
  if (request.overlays.length > 128) throw new Error('limit-exceeded');
  return decodeReply(await finiteHelper(binary, ['presentation', 'resolve', '--stdio'], JSON.stringify(request), signal, observe), request);
}
export async function resolveCodePresentation(binary: string, request: ResolveRequest, signal?: AbortSignal): Promise<{ reply?: ResolveReply; reason?: string; timings: CodeHelperTimings }> {
  const timings: CodeHelperTimings = {};
  let phase: keyof CodeHelperTimings = 'capabilities';
  try {
    await verifyPresentationHelper(binary, signal, timing => { timings.capabilities = timing; });
    phase = 'resolve';
    return { reply: await resolvePresentation(binary, request, signal, timing => { timings.resolve = timing; }), timings };
  } catch (error) {
    const message = error instanceof Error ? error.message : '';
    const reason = /^(?:helper-(?:unavailable|deadline)|limit-exceeded|stale-request|invalid-[a-z-]+|unknown-presentation-field|missing-presentation-reason|incomplete-presentation-identity|duplicate-presentation-[a-z-]+|incompatible-[a-z-]+)$/.test(message)
      ? message : 'presentation-unavailable';
    if (timings[phase]) timings[phase]!.reason = reason;
    return { reason, timings };
  }
}
export function requireTypedPlanVersion(value: unknown): void {
  try { decodeCapabilities(value, 3); }
  catch {
    throw new Error('Unsupported typed Results capability: this runbook requires an execution-plan/v3 runtime. No execution was started.');
  }
}
export function usesTypedResults(document: { schema_version?: string; frames?: Array<{ invocation?: unknown }>; nodes: Array<{ data: { kind?: string } }> }): boolean {
  return document.schema_version === '3' || !!document.frames?.some(frame => frame.invocation !== undefined) ||
    document.nodes.some(node => node.data.kind === 'results' || node.data.kind === 'assign');
}
export async function requireCompatibleExecution(binary: string, document: { schema_version?: string; frames?: Array<{ invocation?: unknown }>; nodes: Array<{ data: { kind?: string; details?: { code_presentation?: { arguments: Array<{ presentation?: unknown }>; outputs: Array<{ presentation?: unknown }> } } } }> }, localMetadata?: ResolveReply): Promise<void> {
  if (usesTypedResults(document)) {
    let capabilities: unknown;
    try { capabilities = await finiteHelper(binary, ['presentation', 'capabilities', '--v3'], ''); }
    catch (error) { throw new Error(`Typed Results capability query failed (${error instanceof Error ? error.message : 'helper-unavailable'}). No execution was started.`); }
    requireTypedPlanVersion(capabilities);
  }
  const metadata = localMetadata?.bindings.some(binding => binding.actions.some(action =>
    [...action.arguments, ...action.outputs].some(field => field.presentation !== undefined))) || document.nodes.some(node => {
    const presentation = node.data.details?.code_presentation;
    return presentation && [...presentation.arguments, ...presentation.outputs].some(field => field.presentation !== undefined);
  });
  if (!metadata) return;
  try { await verifyPresentationHelper(binary); }
  catch { throw new Error('This runbook declares code presentation and requires an execution-plan/v2 runtime. Set yawr.binaryPath to the matching upgraded runtime. Authoring uses the bundled helper independently.'); }
}
