import type { HostActionHandlerArgs } from './hostActionBridge';

type Correlation = Pick<HostActionHandlerArgs,
  'capability' | 'runId' | 'turnId' | 'correlationId' | 'previewSessionId' | 'requestId'>;

export interface ExternalViewCheck extends Correlation {
  type: 'yawr.external-view.verify-view';
}

export type ExternalViewVerification = 'opened' | 'failed' | 'cancelled';

export function matchesExternalViewCheck(value: unknown, expected: Correlation): value is ExternalViewCheck {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const candidate = value as Record<string, unknown>;
  return candidate.type === 'yawr.external-view.verify-view' && Object.keys(candidate).length === 7 &&
    (['capability', 'runId', 'turnId', 'correlationId', 'previewSessionId', 'requestId'] as const)
      .every(key => candidate[key] === expected[key]);
}

interface VerificationTransport {
  postMessage(message: ExternalViewCheck): PromiseLike<boolean>;
  onDidReceiveMessage(listener: (message: unknown) => void): { dispose(): void };
}

export function waitForExternalViewVerification(
  args: HostActionHandlerArgs,
  transport: VerificationTransport,
): Promise<ExternalViewVerification> {
  if (args.cancellationToken.isCancellationRequested) return Promise.resolve('cancelled');
  const request: ExternalViewCheck = {
    type: 'yawr.external-view.verify-view', capability: args.capability,
    runId: args.runId, turnId: args.turnId, correlationId: args.correlationId,
    previewSessionId: args.previewSessionId, requestId: args.requestId,
  };
  return new Promise((resolve, reject) => {
    let settled = false;
    let messages: { dispose(): void } | undefined;
    let cancellation: { dispose(): void } | undefined;
    const finish = (result: ExternalViewVerification | Error) => {
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
      if (type !== 'yawr.external-view.view-verified' || (status !== 'opened' && status !== 'failed')) return;
      if (matchesExternalViewCheck({ ...correlation, type: 'yawr.external-view.verify-view' }, request)) finish(status);
    });
    cancellation = args.cancellationToken.onCancellationRequested(() => finish('cancelled'));
    if (settled) { messages.dispose(); cancellation.dispose(); return; }
    Promise.resolve().then(() => settled ? true : transport.postMessage(request)).then(
      delivered => { if (!delivered) finish(new Error('External view readiness request could not reach the run panel.')); },
      error => finish(error instanceof Error ? error : new Error('External view readiness request failed.')),
    );
  });
}
