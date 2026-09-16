import { createHash } from 'crypto';
import { parseGraphDocument, type GraphDocument } from './directGraphPreview';

const MAX_GRAPH_BYTES = 32 * 1024 * 1024;
export interface ExecutionGraphUpdate { document: GraphDocument; nodeIDs: string[] }

export class ExecutionGraphAssembly {
  private current?: { revision: number; digest: string; total: number; offset: number; chunks: Buffer[] };
  private revision = 0;

  accept(frame: Record<string, unknown>): ExecutionGraphUpdate | undefined {
    const { revision, digest, offset, totalBytes, data } = frame;
    if (!Number.isSafeInteger(revision) || (revision as number) <= this.revision ||
        typeof digest !== 'string' || !/^sha256:[a-f0-9]{64}$/.test(digest) ||
        !Number.isSafeInteger(totalBytes) || (totalBytes as number) < 1 || (totalBytes as number) > MAX_GRAPH_BYTES ||
        !Number.isSafeInteger(offset) || (offset as number) < 0 || typeof data !== 'string' ||
        data.length === 0 || data.length > 256 * 1024 || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(data)) {
      throw new Error('invalid execution graph chunk');
    }
    const chunk = Buffer.from(data, 'base64');
    if (!this.current) {
      if (offset !== 0) throw new Error('execution graph must start at byte zero');
      this.current = { revision: revision as number, digest, total: totalBytes as number, offset: 0, chunks: [] };
    }
    const current = this.current;
    if (revision !== current.revision || digest !== current.digest || totalBytes !== current.total ||
        offset !== current.offset || current.offset + chunk.length > current.total) {
      throw new Error('execution graph chunks are not contiguous');
    }
    current.chunks.push(chunk);
    current.offset += chunk.length;
    if (current.offset !== current.total) return undefined;
    const bytes = Buffer.concat(current.chunks);
    if (`sha256:${createHash('sha256').update(bytes).digest('hex')}` !== current.digest) {
      throw new Error('execution graph digest mismatch');
    }
    const wire = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes)) as {
      document?: unknown; nodeIDs?: unknown;
    };
    const document = parseGraphDocument(JSON.stringify(wire.document));
    const ids = new Set(document.nodes.map(node => node.id));
    if (!Array.isArray(wire.nodeIDs) || wire.nodeIDs.some(id => typeof id !== 'string' || !ids.has(id)) ||
        new Set(wire.nodeIDs).size !== wire.nodeIDs.length) throw new Error('invalid execution graph node bindings');
    this.revision = current.revision;
    this.current = undefined;
    return { document, nodeIDs: wire.nodeIDs };
  }

  complete(): void {
    if (this.current) throw new Error('execution graph ended before all chunks arrived');
  }
}
