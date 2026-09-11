import { retainDirectDisplayWithdrawals, retainDisplayObservation, type DisplayObservations } from './displayObservations';

interface DisplayStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

function isIdentity(value: unknown): value is Record<string, string | number> {
  return !!value && typeof value === 'object' && !Array.isArray(value) &&
    Object.values(value).every(field => typeof field === 'string' ||
      (typeof field === 'number' && Number.isSafeInteger(field) && field >= 0));
}

export function decodeDisplayWithdrawals(value: unknown, runID?: string): DisplayObservations {
  let result: DisplayObservations = {};
  if (!value || typeof value !== 'object' || Array.isArray(value)) return result;
  for (const [nodeID, items] of Object.entries(value)) {
    if (!Array.isArray(items)) continue;
    for (const item of items) {
      if (!item || typeof item.runID !== 'string' || (runID !== undefined && item.runID !== runID) ||
          typeof item.snapshotDigest !== 'string' || !isIdentity(item.identity)) continue;
      result = retainDisplayObservation(result, nodeID, item.runID, { ...item.identity,
        output: {}, display_presentation_diagnostic: 'withdrawn',
      }, item.snapshotDigest);
    }
  }
  return result;
}

export function loadDisplayWithdrawals(storage: DisplayStorage, runID: string): DisplayObservations {
  try {
    return decodeDisplayWithdrawals(JSON.parse(storage.getItem(`yawr.displayWithdrawals.v1:${runID}`) ?? '{}'), runID);
  } catch { return {}; }
}

export function storeDisplayWithdrawals(storage: DisplayStorage, runID: string, observations: DisplayObservations): void {
  const retained = retainDirectDisplayWithdrawals(loadDisplayWithdrawals(storage, runID),
    { current: { displayObservations: observations } });
  storage.setItem(`yawr.displayWithdrawals.v1:${runID}`, JSON.stringify(retained));
}
