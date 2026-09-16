import type { HostActionHandlerArgs } from './hostActionBridge';

type Correlation = Pick<HostActionHandlerArgs,
  'capability' | 'runId' | 'turnId' | 'correlationId' | 'previewSessionId' | 'requestId'>;

export interface XtsViewCheck extends Correlation {
  type: 'yawr.xts.verify-view';
}

export type XtsViewVerification = 'opened' | 'failed' | 'cancelled';

export function matchesXtsViewCheck(value: unknown, expected: Correlation): value is XtsViewCheck {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const candidate = value as Record<string, unknown>;
  return candidate.type === 'yawr.xts.verify-view' && Object.keys(candidate).length === 7 &&
    (['capability', 'runId', 'turnId', 'correlationId', 'previewSessionId', 'requestId'] as const)
      .every(key => candidate[key] === expected[key]);
}

interface VerificationTransport {
  postMessage(message: XtsViewCheck): PromiseLike<boolean>;
  onDidReceiveMessage(listener: (message: unknown) => void): { dispose(): void };
}

export function waitForXtsViewVerification(
  args: HostActionHandlerArgs,
  transport: VerificationTransport,
): Promise<XtsViewVerification> {
  if (args.cancellationToken.isCancellationRequested) return Promise.resolve('cancelled');
  const request: XtsViewCheck = {
    type: 'yawr.xts.verify-view', capability: args.capability,
    runId: args.runId, turnId: args.turnId, correlationId: args.correlationId,
    previewSessionId: args.previewSessionId, requestId: args.requestId,
  };
  return new Promise((resolve, reject) => {
    let settled = false;
    let messages: { dispose(): void } | undefined;
    let cancellation: { dispose(): void } | undefined;
    const finish = (result: XtsViewVerification | Error) => {
      if (settled) return;
      settled = true;
      messages?.dispose();
      cancellation?.dispose();
      if (result instanceof Error) reject(result);
      else resolve(result);
    };
    messages = transport.onDidReceiveMessage(value => {
      if (typeof value !== 'object' || value === null || Array.isArray(value)) return;
      const { type, status, ...correlation } = value as Record<string, unknown>;
      if (type !== 'yawr.xts.view-verified' || (status !== 'opened' && status !== 'failed')) return;
      if (matchesXtsViewCheck({ ...correlation, type: 'yawr.xts.verify-view' }, request)) finish(status);
    });
    cancellation = args.cancellationToken.onCancellationRequested(() => finish('cancelled'));
    if (settled) { messages.dispose(); cancellation.dispose(); return; }
    Promise.resolve().then(() => settled ? true : transport.postMessage(request)).then(
      delivered => { if (!delivered) finish(new Error('XTS readiness request could not reach the run panel.')); },
      error => finish(error instanceof Error ? error : new Error('XTS readiness request failed.')),
    );
  });
}
