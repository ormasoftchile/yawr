'use strict';

// Locally inspected provider recordings, with all identities, authored names,
// bodies and captures replaced. Keep the wire envelope and preview defects.
const error = 'MCP-008: mcp-http: transport error reading SSE stream: bufio.Scanner: token too long';
const envelope = {
  action: 'read', arguments: [{ name: 'input', reason: 'missing-descriptor', status: 'unavailable', value_type: 'string' }],
  origin: 'frozen',
  outputs: [
    { name: 'field00', reason: 'missing-descriptor', status: 'unavailable', value_type: 'string' },
    { name: 'field01', reason: 'missing-descriptor', status: 'unavailable', value_type: 'string' },
    { name: 'field02', reason: 'missing-descriptor', status: 'unavailable', value_type: 'integer' },
    { name: 'field03', reason: 'missing-descriptor', status: 'unavailable', value_type: 'string' },
    { name: 'field04', reason: 'missing-descriptor', status: 'unavailable', value_type: 'string' },
    { name: 'field05', reason: 'missing-descriptor', status: 'unavailable', value_type: 'string' },
    { name: 'field06', reason: 'missing-descriptor', status: 'unavailable', value_type: 'array' },
    { name: 'field07', reason: 'missing-descriptor', status: 'unavailable', value_type: 'string' },
    { name: 'field08', reason: 'missing-descriptor', status: 'unavailable', value_type: 'string' },
    { name: 'field09', reason: 'missing-descriptor', status: 'unavailable', value_type: 'integer' },
    { _preview_omitted: 32, _preview_truncated: true },
  ],
  outputs_preview_truncated: true,
  plan_snapshot_digest: 'sha256:' + 'a'.repeat(64),
  status: 'resolved', tool_digest: 'sha256:' + 'b'.repeat(64), tool_id: 'synthetic-reader', version: 1,
};
function frames(variant) {
  const success = variant === 'success', old = variant === 'old-failure';
  const runID = 'synthetic-run', stepID = 'read', qualified = 'wrapper/read';
  const payload = {
    call_path: [{ runbook_path: 'synthetic.runbook.yaml', step_id: 'wrapper' }],
    captures: Object.fromEntries(Array.from({ length: success ? 5 : 9 }, (_, i) => [`capture${i}`, 'synthetic'])),
    ...(!old ? { code_presentation: structuredClone(envelope), code_presentation_preview_truncated: true } : {}),
    duration_ms: success ? 3729 : old ? 6340 : 4715,
    ...(success ? { evidence: [{ captured_at: '2026-01-01T00:00:00Z', kind: 'text', name: 'synthetic', value: 'synthetic' }] } : { error }),
    ...(!old ? { frame_id: 'synthetic-frame', frame_step_index: success ? 0 : 3 } : {}),
    invocation: 1, kind: 'tool', occurrence_sequence: 1, outcome: success ? 'success' : 'failed',
    output: success ? { exit_code: 0, preview_fields_omitted: 34, preview_truncated: true,
      stderr_excerpt: '', stderr_truncated: false, stdout_excerpt: '', stdout_truncated: false } : {},
    ...(!success ? { output_preview_truncated: true } : {}),
    ...(!old ? { output_value_status: {} } : {}),
    phase: 'execute', qualified_node_id: qualified, retry_attempt: 1,
    status: success ? 'completed' : 'failed', step_id: stepID,
    structural_path: [{ invocation: 1, kind: 'include', qualified_node_id: 'wrapper' }],
  };
  const terminal = { event: { event_id: 'synthetic-event', kind: success ? 'step/completed' : 'step/failed',
    payload, run_id: runID, runbook_id: 'synthetic-book', sequence: success ? 133 : old ? 37 : 38,
    timestamp: '2026-01-01T00:00:00Z' }, runID, type: 'run.event', version: 'yawr.stdio/v1' };
  return [
    { runID, status: 'running', type: 'run.started', version: 'yawr.stdio/v1' },
    { ...structuredClone(terminal), event: { ...terminal.event, kind: 'step/started',
      payload: { qualified_node_id: qualified, step_id: stepID, kind: 'tool' } } },
    terminal,
    // Synthetic lifecycle close: the successful recording was intentionally
    // stopped after ICM completion and has no run.finished.
    { runID, status: payload.status, type: 'run.finished', version: 'yawr.stdio/v1',
      ...(!success ? { error } : {}), steps: [{ node_id: qualified, status: payload.status,
        ...(!success ? { error } : {}), output: { field00: 'UNCLASSIFIED_SUMMARY' } }] },
  ];
}
module.exports = { frames, envelope, error };
