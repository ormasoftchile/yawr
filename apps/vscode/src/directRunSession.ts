import { sanitizeEventFrame } from './presentationProjection';
import { parseDisplayJSON } from './displayPresentationJSON';
import { ResultsAssembly } from './typedResults';
import { visitJSONWire } from './authoringProtocol';
export const STDIO_PROTOCOL_VERSION = 'yawr.stdio/v1' as const;
const MAX_PROTOCOL_LINE_BYTES = 1024 * 1024;

export interface StdioProtocolFrame {
  type: string;
  version: typeof STDIO_PROTOCOL_VERSION;
  runID?: string;
  [key: string]: unknown;
}

export interface DirectRunCallbacks {
  onFrame(frame: StdioProtocolFrame): void;
  onError(message: string): void;
  onExit(code: number | null, signal: NodeJS.Signals | null): void;
  onStderr?(text: string): void;
}

export interface RunChildProcess {
  stdin: {
    write(data: string): unknown;
    on?(event: 'error', listener: (error: Error) => void): unknown;
  };
  stdout: NodeJS.ReadableStream;
  stderr: NodeJS.ReadableStream;
  killed: boolean;
  exitCode: number | null;
  kill(signal?: NodeJS.Signals): boolean;
  on(event: 'error', listener: (error: Error) => void): unknown;
  on(event: 'exit', listener: (code: number | null, signal: NodeJS.Signals | null) => void): unknown;
  on(event: 'close', listener: (code: number | null, signal: NodeJS.Signals | null) => void): unknown;
}

export function buildStdioRunArgs(
  runbookPath: string,
  inputs: Readonly<Record<string, string>>,
  packageMapPath?: string,
  debug = false,
  privateInputNames: ReadonlySet<string> = new Set(),
  routeTestPath?: string,
  typedResults = false,
): string[] {
  const args = ['run', '--stdio'];
  if (typedResults) args.push('--require-capabilities', 'yawr.typed-results/v1,yawr.run-results-chunks/v1');
  if (routeTestPath) {
    // The reviewed artifact is authoritative; never mix live input or debug
    // configuration into a zero-dispatch route test.
  } else if (debug) {
    args.push('--debug');
  } else if (privateInputNames.size > 0) {
    args.push('--configure');
  }
  if (packageMapPath) {
    args.push('--package-map', packageMapPath);
  }
  if (routeTestPath) {
    args.push('--route-test', routeTestPath);
  } else {
    for (const name of Object.keys(inputs).sort()) {
      if (privateInputNames.has(name)) continue;
      args.push('--var', `${name}=${inputs[name]}`);
    }
  }
  args.push(runbookPath);
  return args;
}

export class DirectRunSession {
  private buffer = '';
  private runID: string | undefined;
  private terminal = false;
  private disposed = false;
  private finalized = false;
  private results?: ResultsAssembly;

  constructor(
    private readonly child: RunChildProcess,
    private readonly callbacks: DirectRunCallbacks,
  ) {
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (chunk: string | Buffer) => this.receive(String(chunk)));
    child.stdout.resume();
    child.stdout.on('end', () => {
      if (!this.terminal && this.buffer.trim()) this.parseLine(this.buffer);
      this.buffer = '';
    });
    child.stderr.setEncoding('utf8');
    child.stderr.on('data', (chunk: string | Buffer) => callbacks.onStderr?.(String(chunk)));
    child.stdin.on?.('error', (error) => {
      if (!this.disposed) this.protocolFailure(error.message);
    });
    child.on('error', (error) => {
      if (!this.disposed) callbacks.onError(error.message);
      this.finalize(null, null);
    });
    child.on('close', (code, signal) => this.finalize(code, signal));
  }

  setRunID(runID: string): void {
    this.runID = runID;
  }

  send(command: Readonly<Record<string, unknown>>): void {
    if (this.disposed || this.terminal) return;
    let encoded: string;
    try {
      encoded = `${JSON.stringify(command)}\n`;
    } catch {
      this.protocolFailure('stdio command must be JSON serializable');
      return;
    }
    if (Buffer.byteLength(encoded, 'utf8') > MAX_PROTOCOL_LINE_BYTES) {
      this.protocolFailure(`stdio command exceeds ${MAX_PROTOCOL_LINE_BYTES} bytes`);
      return;
    }
    try {
      this.child.stdin.write(encoded);
    } catch (error) {
      this.protocolFailure(error instanceof Error ? error.message : String(error));
    }
  }

  dispose(): void {
    if (this.disposed) return;
    const shouldCancel = !this.terminal && this.runID && this.child.exitCode === null && !this.child.killed;
    this.disposed = true;
    if (shouldCancel) {
      const cancellation = `${JSON.stringify({
        type: 'run.cancel',
        runID: this.runID,
        reason: 'panel disposed',
      })}\n`;
      try {
        this.child.stdin.write(cancellation);
      } catch {
        // The child may have closed stdin before emitting close.
      }
    }
    if (this.child.exitCode === null && !this.child.killed) {
      this.child.kill();
    }
  }

  private receive(chunk: string): void {
    if (this.disposed || this.terminal) return;
    this.buffer += chunk;
    let newline = this.buffer.indexOf('\n');
    while (newline >= 0) {
      const line = this.buffer.slice(0, newline).trimEnd();
      this.buffer = this.buffer.slice(newline + 1);
      if (Buffer.byteLength(line, 'utf8') + 1 > MAX_PROTOCOL_LINE_BYTES) {
        this.protocolFailure(`stdio protocol line exceeds ${MAX_PROTOCOL_LINE_BYTES} bytes`);
        return;
      }
      if (line) this.parseLine(line);
      if (this.disposed || this.terminal) return;
      newline = this.buffer.indexOf('\n');
    }
    if (Buffer.byteLength(this.buffer, 'utf8') > MAX_PROTOCOL_LINE_BYTES) {
      this.buffer = '';
      this.protocolFailure(`stdio protocol line exceeds ${MAX_PROTOCOL_LINE_BYTES} bytes`);
    }
  }

  private parseLine(line: string): void {
    let value: unknown;
    const decorations = new Set(['results', 'results_ref', 'results_unavailable']);
    let typedWire = false, invalidDecoration = false, invalidIdentity = false, wireError = false;
    try {
      visitJSONWire(line, {
        // The terminal envelope adds one level to a canonical Results record.
        maxDepth: 129,
        onRootValue: (key, value) => {
          if (decorations.has(key) || (key === 'type' && value === 'run.results.chunk')) typedWire = true;
        },
        onDuplicate: (key, rootKey) => {
          if (decorations.has(rootKey ?? key)) invalidDecoration = true;
          else if (rootKey === undefined) invalidIdentity = true;
        },
      });
    } catch { wireError = true; }
    try {
      value = parseDisplayJSON(line);
    } catch {
      this.protocolFailure('stdio protocol emitted invalid JSON');
      return;
    }
    if (typeof value !== 'object' || value === null || Array.isArray(value)) {
      this.protocolFailure('stdio protocol frame must be an object');
      return;
    }
    const frame = value as Record<string, unknown>;
    typedWire ||= frame.type === 'run.results.chunk' ||
      [...decorations].some(key => Object.hasOwn(frame, key));
    if (typedWire && (invalidIdentity || wireError)) {
      this.protocolFailure('invalid typed Results wire identity or JSON');
      return;
    }
    if (frame.version !== STDIO_PROTOCOL_VERSION) {
      this.protocolFailure(`unsupported protocol version ${JSON.stringify(frame.version)}`);
      return;
    }
    if (typeof frame.type !== 'string' || frame.type.length === 0) {
      this.protocolFailure('stdio protocol frame type must be a non-empty string');
      return;
    }
    if (frame.type !== 'protocol.error') {
      if (typeof frame.runID !== 'string' || frame.runID.length === 0) {
        this.protocolFailure(`${frame.type} frame requires runID`);
        return;
      }
      if (frame.type === 'run.started') {
        if (this.runID && frame.runID !== this.runID) {
          this.protocolFailure(`runID ${JSON.stringify(frame.runID)} does not match active run ${JSON.stringify(this.runID)}`);
          return;
        }
        this.runID = frame.runID;
        this.results ??= new ResultsAssembly(frame.runID);
      } else if (!this.runID || frame.runID !== this.runID) {
        this.protocolFailure(`runID ${JSON.stringify(frame.runID)} does not match active run ${JSON.stringify(this.runID)}`);
        return;
      }
    }
    if (frame.type === 'run.results.chunk') {
      if (invalidDecoration) this.results?.rejectWire();
      this.results?.acceptChunk(frame);
      return;
    }
    if (frame.type === 'run.finished') {
      if (invalidDecoration) this.results?.rejectWire();
      frame.resultsAvailability = this.results?.complete(frame) ?? { state: 'unavailable', reason: 'no-active-run' };
      // Only the validated canonical document crosses into the webview.
      delete frame.results;
      delete frame.results_ref;
      this.terminal = true;
      this.buffer = '';
    }
    try { sanitizeEventFrame(frame); }
    catch { this.protocolFailure('invalid run event payload'); return; }
    this.callbacks.onFrame(frame as StdioProtocolFrame);
  }

  private protocolFailure(message: string): void {
    if (this.disposed) return;
    this.callbacks.onError(message);
    this.dispose();
  }

  private finalize(code: number | null, signal: NodeJS.Signals | null): void {
    if (this.finalized) return;
    this.finalized = true;
    this.disposed = true;
    this.callbacks.onExit(code, signal);
  }
}