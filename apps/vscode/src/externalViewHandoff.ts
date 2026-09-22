import type { CancellationToken, HostActionResult } from './hostActionBridge';
import type { ExternalViewVerification } from './externalViewVerification';

export function externalViewParameterArguments(environment: string, parameters: Record<string, unknown>): string {
  const entries = [['environment', environment], ...Object.entries(parameters)];
  if (Object.prototype.hasOwnProperty.call(parameters, 'environment')) {
    throw new Error('parameters must not override the requested external view environment.');
  }
  return entries.map(([name, value]) => {
    if (typeof name !== 'string' || !/^\w+$/.test(name) ||
        !['string', 'number', 'boolean'].includes(typeof value) ||
        (typeof value === 'number' && !Number.isFinite(value)) ||
        String(value).length === 0 || String(value) !== String(value).trim() ||
        /(?:^|\s)-p\s/i.test(String(value))) {
      throw new Error('External view parameters require word-character names and non-empty scalar values without outer whitespace or an embedded -p argument.');
    }
    return `-p ${name}:${value}`;
  }).join(' ');
}

export interface ExternalViewHandoffDependencies {
  dispatch(): PromiseLike<unknown>;
  verifyView(): PromiseLike<ExternalViewVerification>;
  showDispatchError(message: string): void;
  showReminder(): void;
}

function cancelledResult(): HostActionResult {
  return {
    status: 'execution-not-started',
    error: { code: 'USER_CANCELLED', message: 'External view handoff was cancelled.' },
  };
}

export async function launchExternalViewWithHandoff(
  focus: boolean,
  cancellationToken: CancellationToken,
  dependencies: ExternalViewHandoffDependencies,
): Promise<HostActionResult> {
  if (cancellationToken.isCancellationRequested) return cancelledResult();

  try {
    await dependencies.dispatch();
  } catch {
    if (cancellationToken.isCancellationRequested) return cancelledResult();
    const message = 'External view command failed. The view has not been confirmed ready.';
    dependencies.showDispatchError(message);
    return { status: 'failed', error: { code: 'HANDLER_ERROR', message } };
  }

  if (cancellationToken.isCancellationRequested) return cancelledResult();

  let verification: ExternalViewVerification;
  try {
    verification = await dependencies.verifyView();
  } catch {
    if (cancellationToken.isCancellationRequested) return cancelledResult();
    const message = 'External view readiness could not be confirmed.';
    dependencies.showDispatchError(message);
    return { status: 'failed', error: { code: 'HANDLER_ERROR', message } };
  }
  if (cancellationToken.isCancellationRequested || verification === 'cancelled') return cancelledResult();
  if (verification !== 'opened') {
    const message = 'The operator reported that the requested external view is not ready. The handoff failed.';
    dependencies.showDispatchError(message);
    return { status: 'failed', error: { code: 'HANDLER_ERROR', message } };
  }

  if (focus) dependencies.showReminder();
  return { status: 'completed', result: { status: 'opened' } };
}