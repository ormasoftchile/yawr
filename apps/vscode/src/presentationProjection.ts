import { decodeEnvelope, decodeOutputStatuses, record, safeOutputs, type PresentationEnvelope } from './presentationProtocol';
import { decodeDisplayPresentation, mergeDisplayPayload, sanitizeDisplayPayload, type DisplayPresentationV1 } from './displayPresentation';

export interface RuntimePresentation {
  displayPresentation?: DisplayPresentationV1;
  displayPresentationDiagnostic?: string;
  codePresentation?: PresentationEnvelope;
  outputValueStatus?: Record<string, unknown>;
  presentationDiagnostic?: string;
}
export const INVALID_PRESENTATION_DIAGNOSTIC = 'Code presentation unavailable: invalid metadata; output omitted.';
export const LIMITED_PRESENTATION_DIAGNOSTIC = 'Code presentation unavailable: preview limit exceeded; output omitted.';
export function terminalPresentation(payload: Record<string, unknown>,
  previous?: RuntimePresentation & { output?: Record<string, unknown> },
  expectedDisplaySnapshot = 'missing-binding'): RuntimePresentation {
  if (previous?.displayPresentation || previous?.displayPresentationDiagnostic) {
    const old = { output: previous.output,
      ...(previous.displayPresentation ? { display_presentation: previous.displayPresentation } : {}),
      ...(previous.displayPresentationDiagnostic ? { display_presentation_diagnostic: previous.displayPresentationDiagnostic } : {}) };
    const merged = mergeDisplayPayload(old, payload, expectedDisplaySnapshot);
    delete payload.display_presentation;
    delete payload.display_presentation_diagnostic;
    Object.assign(payload, merged);
  }
  sanitizeDisplayPayload(payload, expectedDisplaySnapshot);
  sanitizePresentationPayload(payload);
  const display = {
    displayPresentation: decodeDisplayPresentation(payload.display_presentation),
    ...(payload.display_presentation_diagnostic ? { displayPresentationDiagnostic: 'Unavailable — invalid frozen Markdown metadata' } : {}),
  };
  if (payload.presentation_diagnostic === 'invalid-metadata' || payload.presentation_diagnostic === 'limit-exceeded') {
    return { ...display, presentationDiagnostic: payload.presentation_diagnostic === 'invalid-metadata'
      ? INVALID_PRESENTATION_DIAGNOSTIC : LIMITED_PRESENTATION_DIAGNOSTIC };
  }
  if (payload.code_presentation === undefined) return display;
  const codePresentation = decodeEnvelope(payload.code_presentation);
  const outputValueStatus = decodeOutputStatuses(codePresentation, payload.output_value_status);
  return { ...display, codePresentation, outputValueStatus };
}
// Sanitize declared slots before event forwarding, caches or tokenization. The
// metadata itself is not permission; missing classifications omit code values.
export function sanitizePresentationPayload(payload: Record<string, unknown>): void {
  sanitizeDisplayPayload(payload);
  if (payload.presentation_diagnostic === 'invalid-metadata' || payload.presentation_diagnostic === 'limit-exceeded') {
    omitPresentation(payload, payload.presentation_diagnostic);
    return;
  }
  if (payload.code_presentation === undefined) return;
  let envelope: PresentationEnvelope;
  let statuses: ReturnType<typeof decodeOutputStatuses>;
  try {
    envelope = decodeEnvelope(payload.code_presentation);
    statuses = decodeOutputStatuses(envelope, payload.output_value_status);
  } catch {
    // Only optional decoration decoding is recoverable. Without a trustworthy
    // field list, none of the output can safely fall back to a plaintext view.
    omitPresentation(payload, 'invalid-metadata');
    return;
  }
  payload.code_presentation = envelope;
  payload.output_value_status = statuses;
  if (!payload.output || typeof payload.output !== 'object' || Array.isArray(payload.output)) return;
  const output = { ...record(payload.output) };
  for (const value of safeOutputs(envelope, output, payload.output_value_status)) {
    if (value.text === undefined) delete output[value.name];
  }
  payload.output = output;
}
function omitPresentation(payload: Record<string, unknown>, reason: 'invalid-metadata' | 'limit-exceeded'): void {
  delete payload.code_presentation;
  delete payload.output_value_status;
  delete payload.output;
  payload.presentation_diagnostic = reason;
}
export function sanitizeEventFrame(frame: Record<string, unknown>): void {
  if (Array.isArray(frame.steps)) for (const step of frame.steps) sanitizeDisplayPayload(record(step));
  if (frame.type !== 'run.event') return;
  const event = record(frame.event ?? frame.payload);
  if (event.kind !== 'step/completed' && event.kind !== 'step/failed') return;
  const payload = record(event.payload);
  sanitizePresentationPayload(payload);
}
