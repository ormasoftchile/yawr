import { decodeOutputStatuses, record, type ValueStatus } from './presentationProtocol';
import { sanitizePresentationPayload } from './presentationProjection';
import { parseStepDetails, type StepDetails } from './stepDetails';
import { decodeDisplayPresentation, sanitizeDisplayPayload, type DisplayPresentationV1 } from './displayPresentation';

export interface RetainedIdentity {
  qualified_node_id: string; frame_id?: string; frame_step_index?: number; invocation?: number;
  retry_attempt?: number; occurrence_sequence?: number; event_id?: string; dispatch_occurrence_id?: string;
}
export interface RetainedPresentation {
  identity: RetainedIdentity; details: StepDetails; output: Record<string, unknown>; output_value_status: Record<string, ValueStatus>;
  display_presentation?: DisplayPresentationV1;
  display_presentation_diagnostic?: string;
}
export interface PresentationState {
  run_id: string; plan_snapshot_digest: string; checkpoint_sequence: number; occurrences: RetainedPresentation[];
}
export function decodePresentationState(value: unknown): PresentationState {
  const state = record(value);
  // An empty Go slice is null in native frozen graphs.
  if (state.occurrences === null) state.occurrences = [];
  if (Object.keys(state).some(k => !['run_id', 'plan_snapshot_digest', 'checkpoint_sequence', 'occurrences'].includes(k)) ||
    typeof state.run_id !== 'string' || !state.run_id || typeof state.plan_snapshot_digest !== 'string' || !/^sha256:[a-f0-9]{64}$/.test(state.plan_snapshot_digest) ||
    typeof state.checkpoint_sequence !== 'number' || !Number.isSafeInteger(state.checkpoint_sequence) || state.checkpoint_sequence < 0 ||
    !Array.isArray(state.occurrences) || state.occurrences.length > 4096) throw new Error('invalid-presentation-state');
  const seen = new Set<string>();
  const snapshotDigest = state.plan_snapshot_digest;
  const occurrences = state.occurrences.map(item => {
    const occurrence = record(item), identity = record(occurrence.identity), details = record(occurrence.details);
    if (Object.keys(occurrence).some(k => !['identity', 'details', 'output', 'output_value_status', 'display_presentation', 'display_presentation_diagnostic'].includes(k))) throw new Error('invalid-retained-occurrence');
    const strings = ['qualified_node_id', 'frame_id', 'event_id', 'dispatch_occurrence_id'];
    const numbers = ['frame_step_index', 'invocation', 'retry_attempt', 'occurrence_sequence'];
    if (typeof identity.qualified_node_id !== 'string' || !identity.qualified_node_id) throw new Error('missing-occurrence-identity');
    for (const [key, val] of Object.entries(identity)) {
      if (strings.includes(key)) { if (typeof val !== 'string' || !val) throw new Error('invalid-occurrence-identity'); }
      else if (numbers.includes(key)) { if (typeof val !== 'number' || !Number.isSafeInteger(val) || val < 0) throw new Error('invalid-occurrence-identity'); }
      else throw new Error('invalid-occurrence-identity');
    }
    const key = JSON.stringify(Object.keys(identity).sort().map(name => [name, identity[name]]));
    if (seen.has(key)) throw new Error('ambiguous-occurrence-identity');
    seen.add(key);
    if (typeof details.kind !== 'string') throw new Error('invalid-occurrence-details');
    const parsed = parseStepDetails(details, details.kind, 'retained occurrence');
    if (details.kind === 'display') {
      const metadata = details.format === 'markdown' && Object.keys(record(occurrence.output_value_status)).length === 0
        ? decodeDisplayPresentation(occurrence.display_presentation, snapshotDigest) : undefined;
      const projection: Record<string, unknown> = {
        display_presentation: metadata, output: occurrence.output,
        ...(!metadata || occurrence.display_presentation_diagnostic !== undefined
          ? { display_presentation_diagnostic: 'invalid-metadata' } : {}),
      };
      sanitizeDisplayPayload(projection, snapshotDigest);
      const explicit = Object.prototype.hasOwnProperty.call(occurrence, 'display_presentation') ||
        Object.prototype.hasOwnProperty.call(occurrence, 'display_presentation_diagnostic');
      const retainedMetadata = decodeDisplayPresentation(projection.display_presentation, snapshotDigest);
      // Preserve an explicit metadata withdrawal through normalization without
      // authorizing an output value.
      const withdrawnMetadata: DisplayPresentationV1 | undefined = explicit && !retainedMetadata ? {
        version: 1, format: 'markdown', output_field: 'content', origin: 'frozen',
        plan_snapshot_digest: snapshotDigest, value_status: 'unavailable',
      } : undefined;
      return { identity: identity as unknown as RetainedIdentity, details: parsed,
        output: record(projection.output), output_value_status: {},
        ...(retainedMetadata || withdrawnMetadata ? { display_presentation: retainedMetadata ?? withdrawnMetadata } : {}),
        ...(explicit && projection.display_presentation_diagnostic ? { display_presentation_diagnostic: 'invalid-metadata' } : {}) };
    }
    const envelope = parsed.code_presentation;
    if (!envelope || envelope.origin !== 'frozen' || envelope.plan_snapshot_digest !== state.plan_snapshot_digest) throw new Error('invalid-frozen-presentation');
    const output = record(occurrence.output);
    const output_value_status = decodeOutputStatuses(envelope, occurrence.output_value_status);
    const projection = { code_presentation: envelope, output, output_value_status };
    sanitizePresentationPayload(projection);
    return { identity: identity as unknown as RetainedIdentity, details: parsed, output: projection.output, output_value_status };
  });
  return { run_id: state.run_id, plan_snapshot_digest: state.plan_snapshot_digest, checkpoint_sequence: state.checkpoint_sequence, occurrences };
}
