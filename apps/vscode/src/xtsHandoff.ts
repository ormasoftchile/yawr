import type { CancellationToken, HostActionResult } from './hostActionBridge';

export interface XtsHandoffDependencies {
  dispatch(): PromiseLike<unknown>;
  parseAcknowledgment(value: unknown): { status: string } | undefined;
  showDispatchError(message: string): void;
  showReminder(): void;
}

function cancelledResult(): HostActionResult {
  return {
    status: 'execution-not-started',
    error: { code: 'USER_CANCELLED', message: 'XTS handoff was cancelled.' },
  };
}

export async function launchXtsWithHandoff(
  focus: boolean,
  cancellationToken: CancellationToken,
  dependencies: XtsHandoffDependencies,
): Promise<HostActionResult> {
  if (cancellationToken.isCancellationRequested) return cancelledResult();

  let rawResult: unknown;
  try {
    rawResult = await dependencies.dispatch();
  } catch {
    if (cancellationToken.isCancellationRequested) return cancelledResult();
    const message = 'XTS command failed before returning a launch status.';
    dependencies.showDispatchError(message);
    return { status: 'failed', error: { code: 'HANDLER_ERROR', message } };
  }

  if (cancellationToken.isCancellationRequested) return cancelledResult();

  if (rawResult === undefined || rawResult === null) {
    const message =
      'XTS view not opened: xts.openViewWithParameters returned no status. ' +
      'Ensure the XTS extension (microsoft.xts4vscode) is active in this VS Code window.';
    dependencies.showDispatchError(message);
    return { status: 'failed', error: { code: 'HANDLER_ERROR', message } };
  }

  const acknowledgment = dependencies.parseAcknowledgment(rawResult);
  if (!acknowledgment) {
    const message = 'XTS returned an invalid launch acknowledgment.';
    dependencies.showDispatchError(message);
    return { status: 'failed', error: { code: 'HANDLER_ERROR', message } };
  }

  if (acknowledgment.status === 'opened' && focus) dependencies.showReminder();
  return { status: 'completed', result: { status: acknowledgment.status } };
}