import { stat } from 'fs/promises';
import { finiteHelper } from './presentationClient';
import { decodeExpressionCapabilities, decodeExpressionReply, type ExpressionResolveReply, type ExpressionResolveRequest } from './expressionPresentationProtocol';
import type { ResolveRequest } from './presentationProtocol';

const unsupportedCacheMs = 30_000;
type CapabilityResult = { identity: string; available: boolean; expires: number };
const capabilities = new Map<string, { cached?: CapabilityResult }>();
export async function expressionHelperAvailable(binary: string, signal?: AbortSignal): Promise<boolean> {
  if (signal?.aborted) return false;
  // Claim ownership before stat: filesystem checks can also finish out of order.
  // The same bounded map tracks pending paths; eviction revokes ownership too.
  const entry = { cached: capabilities.get(binary)?.cached };
  if (!capabilities.has(binary) && capabilities.size >= 32) capabilities.delete(capabilities.keys().next().value!);
  capabilities.set(binary, entry);
  let identity: string;
  try {
    const info = await stat(binary);
    identity = `${info.dev}:${info.ino}:${info.size}:${info.mtimeMs}:${info.ctimeMs}`;
  } catch { return false; }
  const cached = entry.cached;
  if (signal?.aborted) return false;
  if (cached?.identity === identity && cached.expires > Date.now()) return cached.available;
  entry.cached = undefined;
  let available = false;
  try {
    decodeExpressionCapabilities(await finiteHelper(binary, ['presentation', 'expressions', 'capabilities'], '', signal));
    available = true;
  } catch (error) {
    // Transport failures and malformed replies prove nothing about support. Retry
    // only on a later caller request, through the same bounded helper queue.
    if (!(error instanceof Error) || error.message !== 'unsupported-expression-capabilities') return false;
  }
  if (signal?.aborted) return false;
  if (capabilities.get(binary) === entry) {
    entry.cached = { identity, available, expires: available ? Infinity : Date.now() + unsupportedCacheMs };
  }
  return available;
}
export async function resolveExpressionPresentation(binary: string, request: ResolveRequest, signal?: AbortSignal): Promise<ExpressionResolveReply | undefined> {
  try {
    if (request.overlays.length > 128 || !await expressionHelperAvailable(binary, signal)) return undefined;
    if (!capabilities.get(binary)?.cached) return undefined;
    const expressionRequest: ExpressionResolveRequest = { ...request, schema_version: 'yawr.expression-resolve/v1' };
    return decodeExpressionReply(await finiteHelper(binary, ['presentation', 'expressions', 'resolve', '--stdio'], JSON.stringify(expressionRequest), signal), expressionRequest);
  } catch { return undefined; }
}
