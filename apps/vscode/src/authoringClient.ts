import { stat } from 'fs/promises';
import { finiteHelper } from './presentationClient';
import { AUTHORING_MAX_BYTES, decodeAuthoringCapabilities, decodeAuthoringReply, parseAuthoringJSON, validBoundary,
  validUnicode, type AuthoringReply, type AuthoringRequest } from './authoringProtocol';

export async function authoringBinaryIdentity(binary: string, signal?: AbortSignal): Promise<string> {
  if (signal?.aborted) throw new Error('stale-request');
  let abort: (() => void) | undefined;
  try {
    const info = await Promise.race([stat(binary), new Promise<never>((_, reject) => {
      abort = () => reject(new Error('stale-request'));
      signal?.addEventListener('abort', abort, { once: true });
    })]);
    return `${info.dev}:${info.ino}:${info.size}:${info.mtimeMs}:${info.ctimeMs}`;
  } catch { throw new Error(signal?.aborted ? 'stale-request' : 'helper-unavailable'); }
  finally { if (abort) signal?.removeEventListener('abort', abort); }
}
export class AuthoringClient {
  private readonly capabilities = new Map<string, { identity?: string }>();
  private readonly pending = new Map<string, AbortController>();
  private disposed = false;
  cancel(): void { for (const controller of this.pending.values()) controller.abort(); this.pending.clear(); }
  dispose(): void { this.disposed = true; this.cancel(); this.capabilities.clear(); }
  async resolve(binary: string, request: AuthoringRequest, signal?: AbortSignal): Promise<AuthoringReply> {
    if (this.disposed || signal?.aborted) throw new Error('stale-request');
    const key = `${request.document.uri}\0${request.operation}`;
    this.pending.get(key)?.abort(); this.pending.delete(key);
    if (this.pending.size >= 16) {
      const oldest = this.pending.keys().next().value!;
      this.pending.get(oldest)!.abort(); this.pending.delete(oldest);
    }
    const controller = new AbortController(), abort = () => controller.abort();
    this.pending.set(key, controller);
    signal?.addEventListener('abort', abort, { once: true });
    const timer = setTimeout(abort, 5000);
    const capabilityEntry = { ...this.capabilities.get(binary) };
    if (!this.capabilities.has(binary) && this.capabilities.size >= 32) this.capabilities.delete(this.capabilities.keys().next().value!);
    this.capabilities.set(binary, capabilityEntry);
    const current = () => {
      if (controller.signal.aborted || this.pending.get(key) !== controller || this.disposed) throw new Error('stale-request');
    };
    try {
      let input = JSON.stringify(request);
      if (request.overlays.length > 128 || Buffer.byteLength(input, 'utf8') > AUTHORING_MAX_BYTES) throw new Error('limit-exceeded');
      if (!validBoundary(request.document.text, request.position) ||
        [request.document, ...request.overlays].some(b => !validUnicode(b.text))) throw new Error('invalid-request');
      const identity = await authoringBinaryIdentity(binary, controller.signal); current();
      if (capabilityEntry.identity !== identity) {
        capabilityEntry.identity = undefined;
        decodeAuthoringCapabilities(await finiteHelper(binary, ['authoring', 'capabilities', '--v3'], '', controller.signal, undefined, parseAuthoringJSON));
        current();
        if (await authoringBinaryIdentity(binary, controller.signal) !== identity) throw new Error('stale-request');
        current();
        if (this.capabilities.get(binary) === capabilityEntry) capabilityEntry.identity = identity;
      }
      input = JSON.stringify(request);
      current();
      const reply = decodeAuthoringReply(await finiteHelper(binary, ['authoring', request.operation, '--stdio'],
        input, controller.signal, undefined, parseAuthoringJSON), request);
      if (await authoringBinaryIdentity(binary, controller.signal) !== identity) {
        if (this.capabilities.get(binary) === capabilityEntry) capabilityEntry.identity = undefined;
        throw new Error('stale-request');
      }
      current(); return reply;
    } finally {
      clearTimeout(timer); signal?.removeEventListener('abort', abort);
      if (this.pending.get(key) === controller) this.pending.delete(key);
    }
  }
}
