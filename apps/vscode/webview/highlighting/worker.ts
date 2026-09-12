import { tokenize, warmup, type Palette } from './tokenizer';
import { decodeDescriptor, record } from '../../src/presentationProtocol';

const scope = globalThis as unknown as {
  onmessage: ((event: { data: unknown }) => void) | null;
  postMessage(value: unknown): void;
};
void warmup().then(() => scope.postMessage({ ready: true }), () => scope.postMessage({ failed: true }));
scope.onmessage = async event => {
  let id: unknown;
  try {
    const value = record(event.data);
    id = value.id;
    if (typeof id !== 'number' || !Number.isSafeInteger(id) || typeof value.text !== 'string') throw new Error('invalid-request');
    const palette: Palette = value.palette === 'light' || value.palette === 'hc' || value.palette === 'hc-light' ? value.palette : 'dark';
    const spans = await tokenize(value.text, decodeDescriptor(value.descriptor), palette);
    scope.postMessage({ id, spans });
  } catch { scope.postMessage({ id, reason: 'tokenization-unavailable' }); }
};
