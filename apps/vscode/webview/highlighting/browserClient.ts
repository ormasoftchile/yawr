import { MAX_CODE_UNITS, supportedDescriptor, type Descriptor } from '../../src/presentationProtocol';
import type { ColoredSpan } from '../../src/presentationScalar';
import type { Palette } from './tokenizer';

let workerURL = '';
let worker: Worker | undefined;
let ready: Promise<void> | undefined;
let sequence = 0;
let idleTimer: ReturnType<typeof setTimeout> | undefined;
const pending = new Map<number, { resolve(spans: ColoredSpan[]): void; reject(error: Error): void; timer: ReturnType<typeof setTimeout> }>();
export function configureHighlighting(url: string): void { workerURL = url; }
export function palette(): Palette {
  if (document.body.classList.contains('vscode-high-contrast-light')) return 'hc-light';
  if (document.body.classList.contains('vscode-high-contrast') || matchMedia('(forced-colors: active)').matches) return 'hc';
  return document.body.classList.contains('vscode-light') ? 'light' : 'dark';
}
function stop(reason: string): void {
  clearTimeout(idleTimer);
  worker?.terminate(); worker = undefined; ready = undefined;
  for (const item of pending.values()) { clearTimeout(item.timer); item.reject(new Error(reason)); }
  pending.clear();
}
function ensureWorker(): Promise<void> {
  if (!workerURL) return Promise.reject(new Error('worker-unavailable'));
  if (ready) return ready;
  ready = new Promise<void>((resolve, reject) => {
    void (async () => {
    try {
      const response = await fetch(workerURL, { signal: AbortSignal.timeout(5000) });
      if (!response.ok) throw new Error('worker-unavailable');
      const blobURL = URL.createObjectURL(new Blob([await response.text()], { type: 'text/javascript' }));
      worker = new Worker(blobURL, { type: 'module' });
      URL.revokeObjectURL(blobURL);
      const timer = setTimeout(() => { stop('warmup-deadline'); reject(new Error('warmup-deadline')); }, 5000);
      worker.onerror = () => { clearTimeout(timer); stop('worker-unavailable'); reject(new Error('worker-unavailable')); };
      worker.onmessage = (event: MessageEvent<unknown>) => {
        const data = event.data;
        if (!data || typeof data !== 'object') return;
        if ('ready' in data && data.ready === true) { clearTimeout(timer); resolve(); return; }
        if ('failed' in data) { clearTimeout(timer); stop('worker-unavailable'); reject(new Error('worker-unavailable')); return; }
        if (!('id' in data) || typeof data.id !== 'number') return;
        const item = pending.get(data.id);
        if (!item) return;
        clearTimeout(item.timer); pending.delete(data.id);
        if (!pending.size) idleTimer = setTimeout(() => stop('idle'), 3000);
        if (!('spans' in data) || !Array.isArray(data.spans)) { item.reject(new Error('tokenization-unavailable')); return; }
        const spans: ColoredSpan[] = [];
        let previousEnd = 0;
        for (const entry of data.spans) {
          if (!entry || typeof entry !== 'object' || typeof entry.start !== 'number' || typeof entry.end !== 'number' ||
            !Number.isSafeInteger(entry.start) || !Number.isSafeInteger(entry.end) || entry.start < previousEnd || entry.end <= entry.start ||
            typeof entry.color !== 'string' || !/^#[0-9a-f]{6}$/i.test(entry.color)) { item.reject(new Error('invalid-tokens')); return; }
          previousEnd = entry.end;
          spans.push({ start: entry.start, end: entry.end, color: entry.color, macro: entry.macro === true });
        }
        item.resolve(spans);
      };
    } catch { ready = undefined; reject(new Error('worker-unavailable')); }
    })();
  });
  return ready;
}
export async function browserTokens(text: string, descriptor: Descriptor, signal: AbortSignal): Promise<ColoredSpan[]> {
  if (!supportedDescriptor(descriptor)) throw new Error('unsupported-language');
  if (text.length > MAX_CODE_UNITS) throw new Error('limit-exceeded');
  await ensureWorker();
  clearTimeout(idleTimer);
  if (signal.aborted) throw new Error('stale-request');
  if (pending.size >= 32) throw new Error('limit-exceeded');
  const id = ++sequence;
  return new Promise((resolve, reject) => {
    const aborted = () => {
      const item = pending.get(id);
      if (item) { clearTimeout(item.timer); pending.delete(id); reject(new Error('stale-request')); }
    };
    const cleanup = () => signal.removeEventListener('abort', aborted);
    pending.set(id, { resolve: spans => {
      cleanup();
      if (spans.some(s => s.end > text.length)) reject(new Error('invalid-tokens')); else resolve(spans);
    }, reject: error => { cleanup(); reject(error); }, timer: setTimeout(() => stop('tokenizer-deadline'), 250) });
    signal.addEventListener('abort', aborted, { once: true });
    worker?.postMessage({ id, text, descriptor, palette: palette() });
  });
}
