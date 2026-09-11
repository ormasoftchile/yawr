import { hasDisplayPayload, mergeDisplayPayload, sanitizeDisplayPayload, selectDisplayPresentation } from './displayPresentation';

type RecordValue = Record<string, any>;
export type DisplayObservations = Record<string, RecordValue[]>;
const identityFields = ['phase', 'invocation', 'retry_attempt', 'occurrence_sequence',
  'frame_id', 'frame_step_index', 'dispatch_occurrence_id', 'execution_lane', 'event_id'] as const;

export const AMBIGUOUS_WITHDRAWAL = 'withdrawal-unassociated';
function identified(identity: RecordValue): boolean {
  return ['before', 'execute', 'after'].includes(identity.phase) &&
    ['invocation', 'retry_attempt', 'occurrence_sequence'].every(key =>
      Number.isSafeInteger(identity[key]) && identity[key] > 0) &&
    ['frame_id', 'dispatch_occurrence_id', 'execution_lane', 'event_id'].every(key =>
      identity[key] === undefined || typeof identity[key] === 'string') &&
    (identity.frame_step_index === undefined || Number.isSafeInteger(identity.frame_step_index) && identity.frame_step_index >= 0) &&
    (!identity.frame_id || identity.frame_step_index !== undefined);
}
function sameScope(left: RecordValue, right: RecordValue): boolean {
  return left.runID === right.runID && left.snapshotDigest === right.snapshotDigest;
}

function compatible(left: RecordValue, right: RecordValue): boolean {
  return sameScope(left, right) &&
    identityFields.every(key => left.identity[key] === right.identity[key] ||
      (key === 'event_id' && identified(left.identity) && identified(right.identity)) ||
      // Frame offset zero has no identity meaning outside a frame.
      (key === 'frame_step_index' && !left.identity.frame_id && !right.identity.frame_id));
}

function selection(items: RecordValue[], candidate: RecordValue): RecordValue | undefined {
  return items.find(item => sameScope(item, candidate) &&
    item.display_presentation_diagnostic === AMBIGUOUS_WITHDRAWAL) ??
    items.find(item => compatible(item, candidate));
}

function presentationRecord(value: RecordValue): RecordValue {
  return { identity: value.identity, runID: value.runID, snapshotDigest: value.snapshotDigest,
    qualifiedNodeID: value.qualifiedNodeID, occurrenceID: value.occurrenceID, output: value.output,
    ...(value.status !== undefined ? { status: value.status } : {}),
    ...(value.error !== undefined ? { error: value.error } : {}),
    ...(value.display_presentation !== undefined ? { display_presentation: value.display_presentation } : {}),
    ...(value.display_presentation_diagnostic !== undefined ? { display_presentation_diagnostic: value.display_presentation_diagnostic } : {}) };
}

export function retainDisplayObservation(current: DisplayObservations, nodeID: string, runID: string,
  payload: RecordValue, snapshotDigest: string | undefined, status?: string, historical = false): DisplayObservations {
  snapshotDigest ??= 'missing-binding';
  const identity = Object.fromEntries(identityFields.filter(key => payload[key] !== undefined).map(key => [key, payload[key]]));
  const observation: RecordValue = { ...payload, identity, runID, snapshotDigest,
    qualifiedNodeID: nodeID, ...(status ? { status } : {}) };
  if (historical && observation.display_presentation === undefined &&
      observation.display_presentation_diagnostic === undefined) {
    delete observation.display_presentation;
    delete observation.display_presentation_diagnostic;
  }
  sanitizeDisplayPayload(observation, snapshotDigest);
  if (observation.display_presentation_diagnostic === AMBIGUOUS_WITHDRAWAL ||
      (!identified(identity) && hasDisplayPayload(observation) &&
      selectDisplayPresentation(observation.display_presentation, observation.output, snapshotDigest).text === undefined)) {
    observation.identity = {};
    for (const key of identityFields) delete observation[key];
    delete observation.status;
    delete observation.error;
    observation.display_presentation_diagnostic = AMBIGUOUS_WITHDRAWAL;
    delete observation.display_presentation;
  }
  observation.output = Object.fromEntries(['content', 'terminal', 'outcome_category', 'outcome_code']
    .filter(key => observation.output && Object.prototype.hasOwnProperty.call(observation.output, key))
    .map(key => [key, observation.output[key]]));
  const previous = current[nodeID] || [];
  const candidates = previous.map((item, index) => compatible(item, observation) ? index : -1).filter(index => index >= 0);
  const index = candidates.at(-1) ?? -1;
  if (index >= 0) {
    const target = observation;
    const aliases = candidates.filter(i => compatible(previous[i], target));
    const retained = aliases.reduce((value, i) => mergeDisplayPayload(value, previous[i], snapshotDigest), {});
    const merged = mergeDisplayPayload(retained, observation, snapshotDigest);
    merged.identity = target.identity;
    merged.occurrenceID = previous[index].occurrenceID;
    if (!status) merged.status = previous[index].status ?? payload.status;
    if (['completed', 'failed', 'cancelled', 'denied', 'blocked', 'skipped', 'indeterminate'].includes(previous[index].status)) {
      merged.status = previous[index].status;
      merged.error = previous[index].error;
    }
    if (merged.display_presentation_diagnostic === AMBIGUOUS_WITHDRAWAL) {
      delete merged.status;
      delete merged.error;
    }
    return { ...current, [nodeID]: previous.flatMap((item, i) =>
      i === index ? [presentationRecord(merged)] : aliases.includes(i) ? [] : [item]) };
  }
  observation.occurrenceID = JSON.stringify([runID, nodeID, snapshotDigest, observation.identity]);
  return { ...current, [nodeID]: historical ? [presentationRecord(observation), ...previous] : [...previous, presentationRecord(observation)] };
}

export function reconcileDisplayDocument(document: RecordValue | null, current: DisplayObservations): RecordValue | null {
  if (!document?.presentation_state) return document;
  const state = document.presentation_state;
  let resolved = current;
  const latest = new Map<string, RecordValue>();
  for (const item of state.occurrences) if (item.details?.kind === 'display') latest.set(item.identity.qualified_node_id, item);
  for (const [nodeID, item] of latest) {
    resolved = retainDisplayObservation(resolved, nodeID, state.run_id,
      { ...item, ...item.identity }, state.plan_snapshot_digest);
  }
  return { ...document, presentation_state: { ...state, occurrences: state.occurrences.map((item: RecordValue) => {
    if (item.details?.kind !== 'display') return item;
    const candidate = { runID: state.run_id, snapshotDigest: state.plan_snapshot_digest, identity: item.identity };
    const matched = selection(resolved[item.identity.qualified_node_id] || [], candidate);
    if (!matched || !hasDisplayPayload(matched)) return item;
    const merged = mergeDisplayPayload(item, matched, state.plan_snapshot_digest);
    return { ...item, output: merged.output, display_presentation: merged.display_presentation,
      ...(merged.display_presentation_diagnostic ? { display_presentation_diagnostic: merged.display_presentation_diagnostic } : {}) };
  }) } };
}

export function reconcileRuntimeDisplayState(state: RecordValue): RecordValue {
  if (state.directDisplaySnapshot !== undefined || state.displayObservations !== undefined) {
    let observations = state.displayObservations ?? {};
    if (state.runID && state.qualifiedNodeID && hasDisplayPayload(directDisplayPayload(state))) {
      observations = retainDisplayObservation(observations, state.qualifiedNodeID, state.runID,
        directDisplayPayload(state), state.directDisplaySnapshot);
    }
    return reconcileDirectDisplayState({ ...state, displayObservations: observations });
  }
  const occurrences = Array.isArray(state.occurrences) ? [...state.occurrences] : [];
  const index = typeof state.occurrenceID === 'string'
    ? occurrences.findIndex(item => item.occurrenceID === state.occurrenceID &&
      ['runID', 'segmentID', 'qualifiedNodeID', 'graphRevision', 'phase', 'invocation', 'retryAttempt',
        'occurrenceSequence', 'frameID', 'frameStepIndex', 'dispatchOccurrenceID', 'executionLane'].every(key =>
        item[key] === undefined || state[key] === undefined || item[key] === state[key])) : -1;
  if (index < 0) return state;
  const wire = (value: RecordValue) => ({
    output: value.output,
    ...(value.displayPresentation !== undefined ? { display_presentation: value.displayPresentation } : {}),
    ...(value.displayPresentationDiagnostic !== undefined ? { display_presentation_diagnostic: value.displayPresentationDiagnostic } : {}),
  });
  const merged = mergeDisplayPayload(wire(occurrences[index]), wire(state));
  if (!hasDisplayPayload(merged)) return state;
  const slot = { output: merged.output, displayPresentation: merged.display_presentation,
    displayPresentationDiagnostic: merged.display_presentation_diagnostic };
  occurrences[index] = { ...occurrences[index], ...slot };
  return { ...state, ...slot, occurrences };
}

export function reconcileDisplayOverlay(overlay: RecordValue | null, current: DisplayObservations,
  runID: string, snapshotDigest: string): RecordValue | null {
  if (!overlay?.nodes) return overlay;
  const nodes = { ...overlay.nodes };
  for (const [nodeID, node] of Object.entries(nodes) as [string, RecordValue][]) {
    const candidate = { runID, snapshotDigest, identity: node };
    const observation = selection(current[nodeID] || [], candidate);
    if (!observation || !hasDisplayPayload(observation)) continue;
    const slot = mergeDisplayPayload(observation, node, snapshotDigest);
    nodes[nodeID] = { ...node, output: slot.output, display_presentation: slot.display_presentation,
      display_presentation_diagnostic: slot.display_presentation_diagnostic };
  }
  return { ...overlay, nodes };
}

// Direct cached history and live occurrences are two views of the same frozen
// observation. Reconcile both directions before either view can be selected.
export function reconcileDirectDisplayState(state: RecordValue, previous?: RecordValue): RecordValue {
  let observations: DisplayObservations = {};
  const observe = (value?: RecordValue) => {
    if (!value) return;
    for (const [nodeID, items] of Object.entries(value.displayObservations ?? {}) as [string, RecordValue[]][]) {
      for (const item of items) observations = retainDisplayObservation(observations, nodeID, item.runID,
        { ...item, ...item.identity }, item.snapshotDigest, item.status);
    }
    const scope = value.retainedDisplayScope;
    const retained = value.retainedPresentations ?? [];
    // Resolve a summary against the authoritative current retained row before
    // ingesting older rows, which must never consume a pending current identity.
    if (scope) for (const item of [...retained].reverse()) {
      if (item.details?.kind !== 'display' || item.details?.format !== 'markdown') continue;
      observations = retainDisplayObservation(observations, item.identity.qualified_node_id, scope.runID,
        { ...item.identity, ...(item.display_presentation !== undefined ? { display_presentation: item.display_presentation } : {}),
          ...(item.display_presentation_diagnostic !== undefined ? { display_presentation_diagnostic: item.display_presentation_diagnostic } : {}),
          output: item.output },
        scope.snapshotDigest, undefined, item !== retained.at(-1));
    }
    for (const item of value.occurrences ?? []) {
      const payload = directDisplayPayload(item);
      if (!hasDisplayPayload(payload) && !observations[item.qualifiedNodeID]?.length) continue;
      observations = retainDisplayObservation(observations, item.qualifiedNodeID, item.runID,
        payload, item.directDisplaySnapshot ?? 'missing-binding', item.status);
    }
  };
  observe(previous);
  observe(state);
  const scope = state.retainedDisplayScope;
  const retained = scope ? reconcileDisplayDocument({ presentation_state: {
    run_id: scope.runID, plan_snapshot_digest: scope.snapshotDigest, occurrences: state.retainedPresentations ?? [],
  } }, observations)?.presentation_state.occurrences : state.retainedPresentations;
  const occurrences = state.occurrences?.map((item: RecordValue) => {
    const payload = directDisplayPayload(item);
    const isCurrent = item.occurrenceID === state.occurrenceID;
    const candidate = { runID: item.runID, snapshotDigest: item.directDisplaySnapshot ?? 'missing-binding',
      identity: payload };
    const matched = selection(observations[item.qualifiedNodeID] ?? [], candidate);
    if (!matched || !hasDisplayPayload(matched)) return item;
    const output = { ...item.output };
    if (typeof matched.output?.content === 'string') output.content = matched.output.content;
    else delete output.content;
    return { ...item, output, displayPresentation: matched.display_presentation,
      displayPresentationDiagnostic: matched.display_presentation_diagnostic };
  });
  const active = occurrences?.find((item: RecordValue) => item.occurrenceID === state.occurrenceID);
  const summary = !state.occurrenceID ? selection(observations[state.qualifiedNodeID] ?? [], {
    runID: state.runID, snapshotDigest: state.directDisplaySnapshot, identity: state.displaySummaryIdentity ?? {},
  }) : undefined;
  return { ...state, displayObservations: observations, ...(scope ? { retainedPresentations: retained } : {}),
    ...(occurrences ? { occurrences } : {}),
    ...(summary && hasDisplayPayload(summary) ? { output: summary.output,
      displayPresentation: summary.display_presentation,
      displayPresentationDiagnostic: summary.display_presentation_diagnostic } : {}),
    ...(active && (active.displayPresentation || active.displayPresentationDiagnostic) ? {
      output: active.output, displayPresentation: active.displayPresentation,
      displayPresentationDiagnostic: active.displayPresentationDiagnostic,
    } : {}) };
}

function directDisplayPayload(value: RecordValue): RecordValue {
  return {
    phase: value.phase, invocation: value.invocation, retry_attempt: value.retryAttempt,
    occurrence_sequence: value.occurrenceSequence, frame_id: value.frameID, frame_step_index: value.frameStepIndex,
    dispatch_occurrence_id: value.dispatchOccurrenceID, execution_lane: value.executionLane,
    output: value.output,
    ...(value.displayPresentation !== undefined ? { display_presentation: value.displayPresentation } : {}),
    ...(value.displayPresentationDiagnostic !== undefined ? { display_presentation_diagnostic: value.displayPresentationDiagnostic } : {}),
  };
}

export function applyDirectDisplaySummary(state: RecordValue, payload: RecordValue, nodeID: string,
  runID: string, snapshotDigest: string): RecordValue {
  const current = reconcileDirectDisplayState(state);
  if (!hasDisplayPayload(payload) && !(current.displayObservations[nodeID]?.length)) return current;
  const identity = Object.fromEntries(identityFields.filter(key => payload[key] !== undefined).map(key => [key, payload[key]]));
  const observations = retainDisplayObservation(current.displayObservations ?? {}, nodeID, runID,
    { ...Object.fromEntries(identityFields.filter(key => identity[key] !== undefined).map(key => [key, identity[key]])),
      ...payload }, snapshotDigest);
  return reconcileDirectDisplayState({ ...current, runID, qualifiedNodeID: nodeID,
    ...((state.runID !== runID || state.directDisplaySnapshot !== snapshotDigest) ? { occurrenceID: undefined } : {}),
    directDisplaySnapshot: snapshotDigest, displayObservations: observations, displaySummaryIdentity: identity });
}

// Only presentation withdrawal knowledge crosses a webview reload. Execution
// statuses, progress, errors and current activity are never restored from it.
export function retainDirectDisplayWithdrawals(history: DisplayObservations, nodes: RecordValue): DisplayObservations {
  let result = history;
  for (const state of Object.values(nodes) as RecordValue[]) {
    for (const [nodeID, items] of Object.entries(state.displayObservations ?? {}) as [string, RecordValue[]][]) {
      for (const item of items) {
        if (!hasDisplayPayload(item) ||
            selectDisplayPresentation(item.display_presentation, item.output, item.snapshotDigest).text !== undefined) continue;
        result = retainDisplayObservation(result, nodeID, item.runID, {
          ...item.identity, output: {},
          display_presentation_diagnostic: item.display_presentation_diagnostic ?? 'withdrawn',
        }, item.snapshotDigest);
      }
    }
  }
  return result;
}

export function restoreDirectDisplayWithdrawals(nodes: RecordValue, history: DisplayObservations,
  runID: string, snapshotDigest: string): RecordValue {
  const result = { ...nodes };
  for (const [nodeID, items] of Object.entries(history)) {
    if (!Array.isArray(items)) continue;
    const scoped = items.filter(item => item && typeof item === 'object' && item.identity &&
      typeof item.identity === 'object' && !Array.isArray(item.identity) &&
      item.runID === runID && item.snapshotDigest === snapshotDigest);
    if (!scoped.length) continue;
    const current = result[nodeID] ?? { status: 'retained', runID, qualifiedNodeID: nodeID, directDisplaySnapshot: snapshotDigest };
    result[nodeID] = reconcileDirectDisplayState({ ...current,
      displayObservations: { [nodeID]: scoped } }, current);
  }
  return result;
}

export function beginDirectDisplayOccurrence(state: RecordValue, payload: RecordValue, runID: string,
  snapshotDigest: string): RecordValue {
  const nodeID = payload.qualified_node_id ?? payload.node_id ?? payload.step_id;
  if (typeof nodeID !== 'string') return state;
  const observations = retainDisplayObservation(state.displayObservations ?? {}, nodeID, runID,
    payload, snapshotDigest, 'running');
  return { ...state, displayObservations: observations };
}

export function applyDirectRetainedDocument(current: RecordValue, document: RecordValue): RecordValue {
  const state = document.presentation_state;
  if (!state) return current;
  const scope = { runID: state.run_id, snapshotDigest: state.plan_snapshot_digest };
  const retained: RecordValue = Object.fromEntries(Object.entries(current)
    .filter(([, value]) => value.runID === scope.runID && value.directDisplaySnapshot === scope.snapshotDigest)
    .map(([id, value]) => [id, { ...value, retainedPresentations: value.retainedPresentations ?? [],
      retainedDisplayScope: scope }]));
  for (const item of state.occurrences) {
    const nodeID = item.identity.qualified_node_id;
    const sameScope = current[nodeID]?.runID === state.run_id &&
      current[nodeID]?.directDisplaySnapshot === state.plan_snapshot_digest;
    const node = retained[nodeID] ?? {
      ...(sameScope ? current[nodeID] : { status: 'retained' }), retainedPresentations: [],
      runID: state.run_id, directDisplaySnapshot: state.plan_snapshot_digest,
      retainedDisplayScope: scope,
    };
    const identity = JSON.stringify(item.identity);
    const index = node.retainedPresentations.findIndex((value: RecordValue) => JSON.stringify(value.identity) === identity);
    node.retainedPresentations = index < 0 ? [...node.retainedPresentations, item]
      : node.retainedPresentations.map((value: RecordValue, i: number) => i === index ? item : value);
    retained[nodeID] = node;
  }
  return Object.fromEntries(Object.entries(retained).map(([id, state]) =>
    [id, reconcileDirectDisplayState(state as RecordValue, current[id])]));
}
