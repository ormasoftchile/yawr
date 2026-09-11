import React, { useState } from 'react';
import type { GraphDocument, GraphNode } from '../src/directGraphPreview';
import type {
  RouteTestArtifact,
  RouteTestReview,
  RouteTestSelector,
  RouteTestSource,
} from '../src/routeTestTypes';
import {
  collectorInputType,
  formatMultipleCollectorText,
  normalizeCollectorValues,
  parseMultipleCollectorText,
  type CollectorFieldDefinition,
  type CollectorFieldOption,
} from '../src/collectorFieldValues';

export interface RouteTestOutcome {
  passed: boolean;
  targetReached: boolean;
  externalDispatches: number;
  status?: 'reached' | 'route-changed' | 'runtime-failed' | 'safety-failed' | 'stopped';
  message?: string;
}

export interface SavedRouteTestView {
  artifact: RouteTestArtifact;
  needsReview: boolean;
}

interface RouteTestContext {
  runbook: string;
  planHash: string;
}

interface ConditionState {
  node: GraphNode;
  enabled: boolean;
  status: string;
  nestedStatus: string;
  objectValue: string;
  selected: string[];
  label: string;
  values: Record<string, unknown>;
  source?: RouteTestSource;
  testApproval: boolean;
  approvalGranted: boolean;
}

export function routeTestMatchesTarget(artifact: RouteTestArtifact, node: GraphNode): boolean {
  return selectorKey(artifact.target) === selectorKey(selectorForNode(node, 'before'));
}

export function RouteTestPane({
  document,
  target,
  candidates,
  blockers,
  context,
  initial,
  needsReview,
  inputValues,
  runStatus,
  runStarting,
  outcome,
  error,
  onSave,
  onRun,
  onStop,
  onClose,
}: {
  document: GraphDocument;
  target: GraphNode;
  candidates: GraphNode[];
  blockers: string[];
  context: RouteTestContext;
  initial?: RouteTestArtifact;
  needsReview?: boolean;
  inputValues: Record<string, string>;
  runStatus: string;
  runStarting: boolean;
  outcome?: RouteTestOutcome;
  error?: string;
  onSave(artifact: RouteTestArtifact): void;
  onRun(artifact: RouteTestArtifact): void;
  onStop(): void;
  onClose(): void;
}) {
  const targetName = nodeTitle(target);
  const [id] = useState(initial?.id ?? `${slug(selectorForNode(target).step)}-route-${Date.now().toString(36)}`);
  const [stage, setStage] = useState<'conditions' | 'review' | 'running'>('conditions');
  const [name, setName] = useState(initial?.name ?? `Reach ${targetName}`);
  const [reviewer, setReviewer] = useState(firstReviewer(initial) ?? '');
  const [acknowledged, setAcknowledged] = useState(false);
  const [sensitivityReviewed, setSensitivityReviewed] = useState(firstSensitivityReview(initial));
  const [localError, setLocalError] = useState<string>();
  const [executedArtifact, setExecutedArtifact] = useState<RouteTestArtifact>();
  const [inputs, setInputs] = useState<Record<string, string>>(() => initial?.inputs ?? routeTestInputs(document, inputValues));
  const [conditions, setConditions] = useState<ConditionState[]>(() => (
    candidates.map((node) => conditionForNode(node, initial))
  ));

  const updateCondition = (nodeID: string, patch: Partial<ConditionState>) => {
    const materialEdit = ['status', 'nestedStatus', 'objectValue', 'selected', 'label', 'values', 'testApproval', 'approvalGranted']
      .some((key) => Object.prototype.hasOwnProperty.call(patch, key));
    setConditions((current) => current.map((condition) => (
      condition.node.id === nodeID
        ? { ...condition, ...patch, ...(materialEdit ? { source: { kind: 'manual' as const } } : {}) }
        : condition
    )));
  };
  const build = (reviewed: boolean): RouteTestArtifact => {
    if (!name.trim()) throw new Error('Route test name is required.');
    if (!sensitivityReviewed) throw new Error('Confirm that the saved values contain no sensitive data.');
    if (reviewed && !reviewer.trim()) throw new Error('Reviewer name is required.');
    if (reviewed && !sensitivityReviewed) throw new Error('Confirm that the saved values contain no sensitive data.');
    const review: RouteTestReview = reviewed
      ? { state: 'reviewed', reviewed_by: reviewer.trim(), reviewed_at: new Date().toISOString(), sensitivity_reviewed: true }
      : { state: 'draft', sensitivity_reviewed: true };
    const artifact: RouteTestArtifact = {
      apiVersion: 'yawr.route-test/v1',
      id,
      name: name.trim(),
      runbook: context.runbook,
      plan_hash: context.planHash,
      sensitivity_reviewed: true,
      target: selectorForNode(target, 'before'),
      ...(Object.keys(inputs).length > 0 ? { inputs } : {}),
    };
    for (const condition of conditions.filter((item) => item.enabled)) {
      const kind = nodeKind(condition.node);
      const source = condition.source ?? { kind: 'manual' as const };
      if (kind === 'cli' || kind === 'tool') {
        const output = parseObject(condition.objectValue, `${nodeTitle(condition.node)} saved result`);
        (artifact.step_responses ??= []).push({
          at: selectorForNode(condition.node), kind, status: condition.status,
          outcome: condition.status === 'completed' ? 'success' : condition.status === 'skipped' ? 'skipped' : 'failed', output, source, review,
        });
      } else if (kind === 'host_action') {
        const capability = hostCapability(condition.node);
        const result = condition.status === 'completed'
          ? capability === 'xts.open-view'
            ? { status: condition.nestedStatus }
            : parseObject(condition.objectValue, `${nodeTitle(condition.node)} result`)
          : undefined;
        (artifact.host_action_responses ??= []).push({
          at: selectorForNode(condition.node), capability,
          response: { status: condition.status, ...(result ? { result } : {}) }, source, review,
        });
      } else if (kind === 'collector') {
        const fields = collectorFields(condition.node);
        for (const field of fields) {
          if (field.ephemeral) throw new Error(`${field.label ?? field.name} is ephemeral and cannot be saved in a route test.`);
        }
        const values = normalizeCollectorValues(fields, condition.values);
        (artifact.interaction_answers ??= []).push({
          at: selectorForNode(condition.node), kind, values, source, review,
        });
      } else if (kind === 'choice') {
        if (condition.selected.length === 0) throw new Error(`${nodeTitle(condition.node)} needs a saved choice.`);
        (artifact.interaction_answers ??= []).push({
          at: selectorForNode(condition.node), kind, selected: condition.selected, source, review,
        });
      } else if (kind === 'decision') {
        if (!condition.label) throw new Error(`${nodeTitle(condition.node)} needs a saved decision.`);
        (artifact.interaction_answers ??= []).push({
          at: selectorForNode(condition.node), kind, label: condition.label, source, review,
        });
      }
      if (condition.testApproval || kind === 'approve') {
        (artifact.test_approvals ??= []).push({
          at: selectorForNode(condition.node),
          approved: condition.approvalGranted,
          approver: reviewer.trim() || 'route-test-draft',
          source,
          review,
        });
      }
    }
    return artifact;
  };
  const check = () => {
    try {
      if (blockers.length > 0) throw new Error(`This route cannot be tested yet: ${blockers.join('; ')}`);
      build(false);
      setLocalError(undefined);
      setStage('review');
    } catch (buildError) {
      setLocalError(buildError instanceof Error ? buildError.message : String(buildError));
    }
  };
  const save = () => {
    try {
      onSave(build(false));
      setLocalError(undefined);
    } catch (buildError) {
      setLocalError(buildError instanceof Error ? buildError.message : String(buildError));
    }
  };
  const run = () => {
    try {
      if (!acknowledged) throw new Error('Acknowledge the route-test boundary before running.');
      const artifact = build(true);
      setExecutedArtifact(artifact);
      setLocalError(undefined);
      setStage('running');
      onRun(artifact);
    } catch (buildError) {
      setLocalError(buildError instanceof Error ? buildError.message : String(buildError));
    }
  };

  if (stage === 'running' && outcome?.passed) {
    return (
      <section className="route-test-pane route-test-result" aria-label="Route test passed" aria-live="assertive">
        <h2>Step reached - command not run</h2>
        <p>Yawr reached <strong>{targetName}</strong> and stopped before running it.</p>
        <dl>
          <dt>External actions</dt><dd>{outcome.externalDispatches}</dd>
          <dt>Saved answers used</dt><dd>{conditions.filter((condition) => condition.enabled).length}</dd>
        </dl>
        {(localError || error) ? <div className="interaction-error" role="alert">{localError ?? error}</div> : null}
        <button type="button" className="primary" onClick={() => {
          try {
            if (!executedArtifact) throw new Error('The executed route-test conditions are unavailable.');
            const artifact: RouteTestArtifact = { ...executedArtifact };
            artifact.last_result = {
              status: outcome.status ?? 'reached', target_reached: outcome.targetReached,
              external_dispatches: outcome.externalDispatches, ran_at: new Date().toISOString(), conditions_digest: 'pending-extension-stamp',
            };
            onSave(artifact);
          } catch (buildError) {
            setLocalError(buildError instanceof Error ? buildError.message : String(buildError));
          }
        }}>Save route test</button>
      </section>
    );
  }
  if (stage === 'running' && !runStarting && (runStatus === 'cancelled' || outcome?.status === 'stopped')) {
    return (
      <section className="route-test-pane route-test-result" aria-label="Route test stopped" aria-live="assertive">
        <h2>Route test stopped</h2>
        <p>The operator stopped this route test before it reached the selected step.</p>
        <button type="button" className="primary" onClick={() => setStage('conditions')}>Review test conditions</button>
      </section>
    );
  }
  if (stage === 'running' && (error || outcome || (!runStarting && runStatus === 'failed'))) {
    const status = outcome?.status ?? (error?.toLowerCase().includes('safety failure') ? 'safety-failed' : 'runtime-failed');
    const routeChanged = status === 'route-changed';
    const safetyFailed = status === 'safety-failed';
    return (
      <section className="route-test-pane route-test-result" aria-label={routeChanged ? 'Route test changed' : safetyFailed ? 'Route test safety failure' : 'Route test failed'} aria-live="assertive">
        <h2>{routeChanged ? 'The route went somewhere else' : safetyFailed ? 'External action blocked' : 'Route test failed'}</h2>
        <p>{outcome?.message || error || 'Yawr did not reach the selected step under these saved conditions.'}</p>
        <button type="button" className="primary" onClick={() => setStage('conditions')}>Fix test conditions</button>
      </section>
    );
  }
  if (stage === 'running') {
    return (
      <section className="route-test-pane" aria-label="Route test running">
        <h2>{targetName}</h2>
        <p>{runStarting ? 'Starting route test...' : 'Testing the saved route toward this step.'}</p>
        <button type="button" className="danger" onClick={onStop}>Stop test</button>
      </section>
    );
  }

  if (stage === 'review') {
    return (
      <section className="route-test-pane" aria-label="Check route">
        <strong className="route-test-review-safety">No external actions will run.</strong>
        <div className="route-test-stepper"><strong>1 Set conditions</strong><strong>2 Check route</strong></div>
        <h2>{targetName}</h2>
        <p>Yawr will stop before this step. XTS, commands, tools, transfers, and connectors are blocked.</p>
        <ol className="route-test-review-list">
          {conditions.filter((condition) => condition.enabled).map((condition) => (
            <li key={condition.node.id}>
              <strong>{nodeTitle(condition.node)}</strong>
              <span>{conditionSummary(condition)} · {sourceSummary(condition.source)}</span>
            </li>
          ))}
          <li><strong>{targetName}</strong><span>Stop before this step</span></li>
        </ol>
        <label className="route-test-field">
          <span>Reviewed by</span>
          <input value={reviewer} onChange={(event) => setReviewer(event.target.value)} />
        </label>
        <label className="route-test-ack">
          <input type="checkbox" checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} />
          <span>This tests the runbook route, not Azure or the external service.</span>
        </label>
        <label className="route-test-ack">
          <input type="checkbox" checked={sensitivityReviewed} onChange={(event) => setSensitivityReviewed(event.target.checked)} />
          <span>I reviewed the saved values. They contain no credentials, tokens, customer data, or raw XTS output.</span>
        </label>
        {(localError || error) ? <div className="interaction-error" role="alert">{localError ?? error}</div> : null}
        <div className="route-test-actions">
          <button type="button" className="primary" disabled={!acknowledged || !sensitivityReviewed || runStarting} onClick={run}>Run route test</button>
          <button type="button" onClick={() => setStage('conditions')}>Edit conditions</button>
          <button type="button" onClick={save}>Save draft</button>
        </div>
      </section>
    );
  }

  return (
    <section className="route-test-pane" aria-label="Set route-test conditions">
      <div className="route-test-stepper"><strong>1 Set conditions</strong><span>2 Check route</span></div>
      <h2>Test reaching {targetName}</h2>
      {needsReview ? <p className="route-test-warning">The runbook changed. Review these conditions before running again.</p> : null}
      {blockers.length > 0 ? <p className="route-test-warning">This route cannot run yet: {blockers.join('; ')}.</p> : null}
      <label className="route-test-field">
        <span>Route test name</span>
        <input value={name} onChange={(event) => setName(event.target.value)} />
      </label>
      {graphInputs(document).length > 0 ? (
        <fieldset className="route-test-group">
          <legend>Runbook inputs</legend>
          {graphInputs(document).filter((input) => input.type !== 'secret').map((input) => (
            <label className="route-test-field" key={input.name}>
              <span>{input.name}</span>
              <input value={inputs[input.name] ?? ''} onChange={(event) => setInputs((current) => ({ ...current, [input.name]: event.target.value }))} />
            </label>
          ))}
          {graphInputs(document).some((input) => input.type === 'secret') ? <p>Secret inputs are not saved in route tests.</p> : null}
        </fieldset>
      ) : null}
      <div className="route-test-conditions">
        {conditions.length === 0 ? <p>This route reaches the selected step using only Yawr logic.</p> : null}
        {conditions.map((condition) => (
          <ConditionEditor key={condition.node.id} condition={condition} onChange={(patch) => updateCondition(condition.node.id, patch)} />
        ))}
      </div>
      <label className="route-test-ack">
        <input type="checkbox" checked={sensitivityReviewed} onChange={(event) => setSensitivityReviewed(event.target.checked)} />
        <span>I reviewed the saved values. They contain no credentials, tokens, customer data, or raw external output.</span>
      </label>
      {(localError || error) ? <div className="interaction-error" role="alert">{localError ?? error}</div> : null}
      <div className="route-test-actions">
        <button type="button" className="primary" disabled={!sensitivityReviewed || blockers.length > 0} onClick={check}>Check this route</button>
        <button type="button" disabled={!sensitivityReviewed} onClick={save}>Save draft</button>
        <button type="button" onClick={onClose}>Close</button>
      </div>
    </section>
  );
}

function ConditionEditor({ condition, onChange }: { condition: ConditionState; onChange(patch: Partial<ConditionState>): void }) {
  const kind = nodeKind(condition.node);
  const capability = hostCapability(condition.node);
  const details = condition.node.data.details as Record<string, unknown> | undefined;
  return (
    <article className={`route-test-condition${condition.enabled ? ' enabled' : ''}`}>
      <label className="route-test-enable">
        <input type="checkbox" checked={condition.enabled} onChange={(event) => onChange({ enabled: event.target.checked })} />
        <span><strong>{nodeTitle(condition.node)}</strong><small>{kindLabels[kind] ?? kind}</small></span>
      </label>
      {condition.enabled && kind === 'host_action' ? (
        <div className="route-test-condition-fields">
          <p>Source: {sourceSummary(condition.source)}</p>
          {capability === 'xts.open-view' ? <p>XTS will not open in this route test.</p> : null}
          <label className="route-test-field"><span>Assume host result</span>
            <select value={condition.status} onChange={(event) => onChange({ status: event.target.value })}>
              {['completed', 'failed', 'timed-out', 'execution-not-started', 'unsupported'].map((status) => <option key={status}>{status}</option>)}
            </select>
          </label>
          {condition.status === 'completed' && capability === 'xts.open-view' ? (
            <label className="route-test-field"><span>Assume XTS launch result</span>
              <select value={condition.nestedStatus} onChange={(event) => onChange({ nestedStatus: event.target.value })}>
                {['opened', 'view-not-found', 'environment-not-found', 'invalid-parameters', 'execution-not-started'].map((status) => <option key={status}>{status}</option>)}
              </select>
            </label>
          ) : condition.status === 'completed' ? <JSONField label="Saved result" value={condition.objectValue} onChange={(objectValue) => onChange({ objectValue })} /> : null}
        </div>
      ) : null}
      {condition.enabled && (kind === 'cli' || kind === 'tool') ? (
        <div className="route-test-condition-fields">
          <p>Source: {sourceSummary(condition.source)}</p>
          <label className="route-test-field"><span>Saved result status</span>
            <select value={condition.status} onChange={(event) => onChange({ status: event.target.value })}>
              <option value="completed">completed</option><option value="failed">failed</option><option value="skipped">skipped</option>
            </select>
          </label>
          <JSONField label="Saved result" value={condition.objectValue} onChange={(objectValue) => onChange({ objectValue })} />
        </div>
      ) : null}
      {condition.enabled && kind === 'collector' ? (
        <fieldset className="route-test-condition-fields"><legend>Saved findings</legend>
          <p>Source: {sourceSummary(condition.source)}</p>
          {collectorFields(condition.node).map((field) => (
            <label className="route-test-field" key={field.name}><span>{field.label ?? field.name}{field.required ? ' *' : ''}</span>
              {field.ephemeral ? <small>Ephemeral field cannot be saved in a route test.</small> : null}
              {field.type === 'boolean' ? (
                <input
                  type="checkbox"
                  checked={condition.values[field.name] === true}
                  onChange={(event) => onChange({ values: { ...condition.values, [field.name]: event.target.checked } })}
                />
              ) : field.options.length > 0 ? (
                <select
                  multiple={field.multiple}
                  value={field.multiple
                    ? (Array.isArray(condition.values[field.name]) ? condition.values[field.name] as string[] : [])
                    : String(condition.values[field.name] ?? '')}
                  onChange={(event) => onChange({ values: {
                    ...condition.values,
                    [field.name]: field.multiple
                      ? Array.from(event.target.selectedOptions).map((option) => option.value)
                      : event.target.value,
                  } })}
                >
                  {!field.multiple ? <option value="">Select...</option> : null}
                  {field.options.map((option) => <option value={option.value} key={option.value}>{option.label}</option>)}
                </select>
              ) : field.multiple ? (
                <textarea
                  rows={3}
                  value={formatMultipleCollectorText(condition.values[field.name])}
                  placeholder="One value per line"
                  onChange={(event) => onChange({ values: {
                    ...condition.values,
                    [field.name]: parseMultipleCollectorText(event.target.value),
                  } })}
                />
              ) : (
                <input
                  type={collectorInputType(field.type)}
                  step={field.type === 'integer' ? 1 : field.type === 'number' ? 'any' : undefined}
                  value={String(condition.values[field.name] ?? '')}
                  onChange={(event) => onChange({ values: { ...condition.values, [field.name]: event.target.value } })}
                />
              )}
            </label>
          ))}
        </fieldset>
      ) : null}
      {condition.enabled && kind === 'choice' ? (
        <label className="route-test-field"><span>Saved choice</span>
          <select
            multiple={details?.multiple === true}
            value={details?.multiple === true ? condition.selected : condition.selected[0] ?? ''}
            onChange={(event) => onChange({ selected: Array.from(event.target.selectedOptions).map((option) => option.value) })}
          >
            <option value="">Select...</option>
            {detailOptions(condition.node).map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
          </select>
        </label>
      ) : null}
      {condition.enabled && kind === 'decision' ? (
        <label className="route-test-field"><span>Saved decision</span>
          <select value={condition.label} onChange={(event) => onChange({ label: event.target.value })}>
            <option value="">Select...</option>
            {detailRoutes(condition.node).map((route) => <option key={route.label} value={route.label}>{route.label}</option>)}
          </select>
        </label>
      ) : null}
      {condition.enabled && kind === 'approve' ? (
        <label className="route-test-field"><span>Test approval</span>
          <select value={condition.approvalGranted ? 'approved' : 'denied'} onChange={(event) => onChange({ approvalGranted: event.target.value === 'approved' })}>
            <option value="approved">Approve for this route test</option>
            <option value="denied">Deny for this route test</option>
          </select>
        </label>
      ) : null}
      {condition.enabled && ['cli', 'tool', 'host_action'].includes(kind) ? (
        <label className="route-test-ack">
          <input type="checkbox" checked={condition.testApproval} onChange={(event) => onChange({ testApproval: event.target.checked })} />
          <span>Use a non-production test approval for this step.</span>
        </label>
      ) : null}
    </article>
  );
}

function JSONField({ label, value, onChange }: { label: string; value: string; onChange(value: string): void }) {
  return <label className="route-test-field"><span>{label}</span><textarea rows={4} spellCheck={false} value={value} onChange={(event) => onChange(event.target.value)} /></label>;
}

function conditionForNode(node: GraphNode, artifact?: RouteTestArtifact): ConditionState {
  const selector = selectorForNode(node);
  const key = selectorKey(selector);
  const step = artifact?.step_responses?.find((binding) => selectorKey(binding.at) === key);
  const host = artifact?.host_action_responses?.find((binding) => selectorKey(binding.at) === key);
  const interaction = artifact?.interaction_answers?.find((binding) => selectorKey(binding.at) === key);
  const approval = artifact?.test_approvals?.find((binding) => selectorKey(binding.at) === key);
  const values = interaction?.values ?? Object.fromEntries(collectorFields(node).flatMap((field) => {
    if (field.default !== undefined) return [[field.name, field.default]];
    return field.type === 'boolean' && field.required ? [[field.name, false]] : [];
  }));
  return {
    node,
    enabled: !!(step || host || interaction || approval),
    status: approval ? (approval.approved ? 'approved' : 'denied') : step?.status ?? host?.response.status ?? 'completed',
    nestedStatus: String(host?.response.result?.status ?? 'opened'),
    objectValue: JSON.stringify(step?.output ?? host?.response.result ?? {}, null, 2),
    selected: interaction?.selected ?? [],
    label: interaction?.label ?? '',
    values,
    source: step?.source ?? host?.source ?? interaction?.source ?? approval?.source,
    testApproval: approval !== undefined,
    approvalGranted: approval?.approved ?? true,
  };
}

function selectorForNode(node: GraphNode, phase: 'before' | 'execute' = 'execute'): RouteTestSelector {
  const callPath = Array.isArray(node.data.call_path)
    ? node.data.call_path.filter((part): part is string => typeof part === 'string' && part.length > 0)
    : [];
  const step = typeof node.data.step_id === 'string' && node.data.step_id ? node.data.step_id : node.id;
  return { ...(callPath.length > 0 ? { call_path: callPath } : {}), step, phase, invocation: 1, attempt: 1 };
}

function selectorKey(selector: RouteTestSelector): string {
  return JSON.stringify([selector.call_path ?? [], selector.step, selector.phase, selector.invocation, selector.attempt]);
}

function nodeKind(node: GraphNode): string {
  return typeof node.data.kind === 'string' ? node.data.kind : '';
}

function nodeTitle(node: GraphNode): string {
  return String(node.data.title || node.data.step_id || node.data.id || node.id);
}

function hostCapability(node: GraphNode): string {
  const details = node.data.details as Record<string, unknown> | undefined;
  return typeof details?.capability === 'string' ? details.capability : '';
}

function detailOptions(node: GraphNode): Array<{ label: string; value: string }> {
  const details = node.data.details as Record<string, unknown> | undefined;
  return Array.isArray(details?.options) ? details.options.flatMap((raw) => {
    const option = raw as Record<string, unknown>;
    return typeof option.label === 'string' && typeof option.value === 'string' ? [{ label: option.label, value: option.value }] : [];
  }) : [];
}

function detailRoutes(node: GraphNode): Array<{ label: string }> {
  const details = node.data.details as Record<string, unknown> | undefined;
  return Array.isArray(details?.routes) ? details.routes.flatMap((raw) => {
    const route = raw as Record<string, unknown>;
    return typeof route.label === 'string' ? [{ label: route.label }] : [];
  }) : [];
}

type RouteCollectorField = CollectorFieldDefinition & {
  required: boolean;
  multiple: boolean;
  ephemeral: boolean;
  default?: unknown;
  options: Array<CollectorFieldOption & { label: string }>;
};

function collectorFields(node: GraphNode): RouteCollectorField[] {
  const details = node.data.details as Record<string, unknown> | undefined;
  return Array.isArray(details?.fields) ? details.fields.flatMap((raw) => {
    const field = raw as Record<string, unknown>;
    if (typeof field.name !== 'string') return [];
    const options = Array.isArray(field.options) ? field.options.flatMap((item) => {
      const option = item as Record<string, unknown>;
      return typeof option.label === 'string' && typeof option.value === 'string' ? [{ label: option.label, value: option.value }] : [];
    }) : [];
    const validation = typeof field.validation === 'object' && field.validation !== null && !Array.isArray(field.validation)
      ? field.validation as CollectorFieldDefinition['validation']
      : undefined;
    return [{
      name: field.name,
      type: typeof field.type === 'string' ? field.type : 'text',
      label: typeof field.label === 'string' ? field.label : undefined,
      display_name: typeof field.display_name === 'string' ? field.display_name : undefined,
      required: field.required === true,
      multiple: field.multiple === true,
      ephemeral: field.ephemeral === true,
      default: field.default,
      options,
      validation,
    }];
  }) : [];
}

function graphInputs(document: GraphDocument): Array<{ name: string; type?: string }> {
  return Array.isArray(document.inputs) ? document.inputs.flatMap((raw) => {
    const input = raw as Record<string, unknown>;
    return typeof input?.name === 'string' ? [{ name: input.name, type: typeof input.type === 'string' ? input.type : undefined }] : [];
  }) : [];
}

function routeTestInputs(document: GraphDocument, values: Record<string, string>): Record<string, string> {
  return Object.fromEntries(graphInputs(document).filter((input) => input.type !== 'secret' && values[input.name] !== '').map((input) => [input.name, values[input.name] ?? '']));
}

function parseObject(value: string, label: string): Record<string, unknown> {
  let parsed: unknown;
  try { parsed = JSON.parse(value || '{}'); } catch { throw new Error(`${label} must be valid JSON.`); }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) throw new Error(`${label} must be a JSON object.`);
  return parsed as Record<string, unknown>;
}

function conditionSummary(condition: ConditionState): string {
  const kind = nodeKind(condition.node);
  if (kind === 'host_action' && hostCapability(condition.node) === 'xts.open-view') return `Assume XTS launch result: ${condition.nestedStatus}`;
  if (kind === 'collector') return `Saved findings: ${Object.values(condition.values).join(', ')}`;
  if (kind === 'choice') return `Saved choice: ${condition.selected.join(', ')}`;
  if (kind === 'decision') return `Saved decision: ${condition.label}`;
  if (kind === 'approve') return `Test approval: ${condition.approvalGranted ? 'approved' : 'denied'}`;
  const summary = `Use saved ${kind} result: ${condition.status}`;
  return condition.testApproval ? `${summary}; test approval: ${condition.approvalGranted ? 'approved' : 'denied'}` : summary;
}

function firstReviewer(artifact?: RouteTestArtifact): string | undefined {
  const binding = artifact && [
    ...(artifact.step_responses ?? []),
    ...(artifact.host_action_responses ?? []),
    ...(artifact.interaction_answers ?? []),
    ...(artifact.test_approvals ?? []),
  ].find((item) => item.review.reviewed_by);
  return binding?.review.reviewed_by;
}

function firstSensitivityReview(artifact?: RouteTestArtifact): boolean {
  return !!artifact && [
    ...(artifact.step_responses ?? []),
    ...(artifact.host_action_responses ?? []),
    ...(artifact.interaction_answers ?? []),
    ...(artifact.test_approvals ?? []),
  ].some((item) => item.review.sensitivity_reviewed);
}

function sourceSummary(source?: RouteTestSource): string {
  if (!source || source.kind === 'manual') return 'Entered for this route test';
  return `Copied from run ${source.run_id}, interaction ${source.interaction_id}`;
}

function slug(value: string): string {
  return value.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 80) || 'route-test';
}

const kindLabels: Record<string, string> = {
  cli: 'Automated command', tool: 'Automated tool', host_action: 'Host action',
  collector: 'Human check', choice: 'Choice', decision: 'Decision',
  approve: 'Approval',
};