import {
  Activity,
  Bug,
  CheckCircle2,
  Clock3,
  FileInput,
  Flag,
  GitBranch,
  ListChecks,
  MessageSquareText,
  PackageOpen,
  Puzzle,
  Radio,
  Repeat2,
  ShieldCheck,
  Terminal,
  Undo2,
  Workflow,
  Wrench,
} from 'lucide-react';
import React, { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import type { GraphDocument, GraphNode } from '../src/directGraphPreview';
import type { CommonStepDetails, NamedDetailValue, StepDetails } from '../src/stepDetails';
import { isIssueStepStatus, isSettledStepStatus, isTerminalRunStatus } from '../src/runStatus';
import { canonicalProgress, isExecutionEnded } from '../src/executionProgress';
import { safeOutputs, type PresentationEnvelope } from '../src/presentationProtocol';
import { CodeValue } from './highlighting/CodeValue';
import { AuthoredValue, AuthoredValues } from './highlighting/AuthoredValue';
import type { RetainedPresentation } from '../src/presentationHistory';
import type { DisplayPresentationV1 } from '../src/displayPresentation';
import { isWorkflowIssue } from '../src/workflowProjection';

export interface RuntimeLogLine {
  stream: string;
  line: string;
}

export interface InspectorRuntimeValueState {
  directDisplaySnapshot?: string;
  retainedDisplayScope?: { runID: string; snapshotDigest: string };
  displayPresentation?: DisplayPresentationV1;
  displayPresentationDiagnostic?: string;
  retainedPresentations?: RetainedPresentation[];
  codePresentation?: PresentationEnvelope;
  outputValueStatus?: Record<string, unknown>;
  presentationDiagnostic?: string;
  status: string;
  error?: string;
  durationMs?: number;
  attempt?: number;
  startedAt?: string;
  finishedAt?: string;
  lastActivityAt?: string;
  stepKind?: string;
  delay?: string;
  skipReason?: string;
  output?: Record<string, unknown>;
  captures?: Record<string, unknown>;
  evidence?: unknown;
  logs?: RuntimeLogLine[];
  debugOverride?: Record<string, unknown>;
}

export interface InspectorRuntimeOccurrence extends InspectorRuntimeValueState {
  graphRevision?: number;
  retainedDetails?: StepDetails;
  occurrenceID: string;
  runID: string;
  segmentID: string;
  qualifiedNodeID: string;
  phase?: string;
  invocation?: number;
  retryAttempt?: number;
  occurrenceSequence?: number;
  startedEventSequence?: number;
  executionSource: 'saved' | 'live';
}

export interface InspectorRuntimeState extends InspectorRuntimeValueState {
  displayObservations?: import('../src/displayObservations').DisplayObservations;
  occurrenceID?: string;
  runID?: string;
  invocation?: number;
  retryAttempt?: number;
  executionSource?: 'saved' | 'live';
  occurrences?: InspectorRuntimeOccurrence[];
}

export interface InspectorInputValue {
  name: string;
  value: string;
  secret: boolean;
}

const KIND_LABELS: Record<string, string> = {
  approve: 'Approval',
  assert: 'Assertion',
  assign: 'Assign bindings',
  branch: 'Branch',
  choice: 'Choice',
  cli: 'Command',
  collector: 'Collector',
  compensate: 'Compensation',
  decision: 'Decision',
  display: 'Display',
  end: 'Outcome',
  extension: 'Extension',
  host_action: 'Host action',
  include: 'Runbook include',
  iterate: 'Iteration',
  noop: 'No-op',
  parallel: 'Parallel',
  results: 'Results',
  tool: 'Tool call',
  wait_for_event: 'Event wait',
};

function KindIcon({ kind }: { kind: string }) {
  switch (kind) {
    case 'cli': return <Terminal aria-hidden="true" />;
    case 'tool': return <Wrench aria-hidden="true" />;
    case 'include': return <PackageOpen aria-hidden="true" />;
    case 'branch': return <GitBranch aria-hidden="true" />;
    case 'choice': return <ListChecks aria-hidden="true" />;
    case 'decision': return <Workflow aria-hidden="true" />;
    case 'collector': return <FileInput aria-hidden="true" />;
    case 'iterate': return <Repeat2 aria-hidden="true" />;
    case 'parallel': return <Workflow aria-hidden="true" />;
    case 'approve': return <ShieldCheck aria-hidden="true" />;
    case 'assert': return <CheckCircle2 aria-hidden="true" />;
    case 'wait_for_event': return <Radio aria-hidden="true" />;
    case 'display': return <MessageSquareText aria-hidden="true" />;
    case 'end': return <Flag aria-hidden="true" />;
    case 'compensate': return <Undo2 aria-hidden="true" />;
    case 'extension': return <Puzzle aria-hidden="true" />;
    case 'noop': return <Clock3 aria-hidden="true" />;
    case 'assign': return <FileInput aria-hidden="true" />;
    case 'results': return <CheckCircle2 aria-hidden="true" />;
    default: return <Activity aria-hidden="true" />;
  }
}

function StatusBadge({ status }: { status: string }) {
  return <span className={`inspector-status status-${status}`}>{status === 'no-final-status' ? 'No final status' : status}</span>;
}

function formatDuration(milliseconds: number | undefined): string {
  if (milliseconds === undefined) return '—';
  if (milliseconds < 1000) return `${milliseconds} ms`;
  if (milliseconds < 60_000) return `${(milliseconds / 1000).toFixed(milliseconds < 10_000 ? 2 : 1)} s`;
  return `${Math.floor(milliseconds / 60_000)}m ${Math.round((milliseconds % 60_000) / 1000)}s`;
}

function formatTime(value: string | undefined): string {
  if (!value) return '—';
  const time = new Date(value);
  return Number.isNaN(time.getTime()) ? value : time.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

function scalarText(value: unknown): string {
  if (value === undefined) return '—';
  if (value === null) return 'null';
  if (typeof value === 'string') return value || '""';
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  try { return JSON.stringify(value); } catch { return String(value); }
}

function Section({ title, count, children, className = '' }: { title: string; count?: number; children: ReactNode; className?: string }) {
  return (
    <section className={`inspector-section ${className}`}>
      <div className="section-heading"><h3>{title}</h3>{count !== undefined ? <span>{count}</span> : null}</div>
      {children}
    </section>
  );
}

function KeyValueRows({ rows }: { rows: Array<{ label: string; value: unknown; code?: boolean; path?: string }> }) {
  const visible = rows.filter((row) => row.value !== undefined && row.value !== '' && row.value !== false);
  if (visible.length === 0) return <p className="inspector-muted">No additional configuration.</p>;
  return (
    <dl className="detail-grid">
      {visible.map((row) => (
        <React.Fragment key={row.label}>
          <dt>{row.label}</dt>
          <dd className={row.code ? 'code-value' : ''}>{row.path ? <AuthoredValue path={row.path} value={row.value} /> : scalarText(row.value)}</dd>
        </React.Fragment>
      ))}
    </dl>
  );
}

export function NamedValueRows({ values, empty = 'None', path, startIndex = 0 }: { values: NamedDetailValue[] | undefined; empty?: string; path?: string; startIndex?: number }) {
  if (!values || values.length === 0) return <p className="inspector-muted">{empty}</p>;
  return (
    <div className="named-value-list">
      {values.map((item, index) => (
        <div className="named-value-row" key={item.name}>
          <code>{item.name}</code>
          <span className={item.redacted ? 'redacted-value' : ''}>{item.redacted ? 'redacted' : path
            ? <AuthoredValue path={`${path}/${index + startIndex}/value`} value={item.value} /> : scalarText(item.value)}</span>
        </div>
      ))}
    </div>
  );
}

function DataObject({ value, empty }: { value: Record<string, unknown> | undefined; empty: string }) {
  if (!value || Object.keys(value).length === 0) return <p className="inspector-muted">{empty}</p>;
  const entries = Object.entries(value).sort(([left], [right]) => left.localeCompare(right));
  return (
    <div className="runtime-data">
      {entries.map(([key, item]) => (
        <div className="runtime-data-row" key={key}>
          <code>{key}</code>
          {typeof item === 'object' && item !== null
            ? <pre>{JSON.stringify(item, null, 2)}</pre>
            : <span>{scalarText(item)}</span>}
        </div>
      ))}
    </div>
  );
}

export function RuntimePane({ runtime, markdownDeclared = false, snapshotDigest }: {
  runtime: InspectorRuntimeState | undefined; markdownDeclared?: boolean; snapshotDigest?: string;
}) {
  if (!runtime || runtime.status === 'pending') {
    return <div className="inspector-blank"><Activity aria-hidden="true" /><strong>Not run yet</strong><span>Execution data will appear here.</span></div>;
  }
  return (
    <div className="inspector-pane">
      <Section title="Execution">
        <KeyValueRows rows={[
          { label: 'Status', value: runtime.status === 'no-final-status' ? 'No final status' : runtime.status },
          { label: 'Duration', value: formatDuration(runtime.durationMs) },
          { label: 'Attempt', value: runtime.attempt },
          { label: 'Started', value: formatTime(runtime.startedAt) },
          { label: 'Finished', value: formatTime(runtime.finishedAt) },
          { label: 'Delay', value: runtime.delay },
          { label: 'Skip reason', value: runtime.skipReason },
        ]} />
      </Section>
      {runtime.error ? <Section title="Error" className="error-section"><pre>{runtime.error}</pre></Section> : null}
      {runtime.presentationDiagnostic ? <Section title="Code presentation"><p className="inspector-muted">{runtime.presentationDiagnostic}</p></Section> : null}
      {runtime.codePresentation || runtime.retainedPresentations?.length
        ? <Section title="Execution-resolved inputs"><p className="inspector-muted">Unavailable — not retained</p></Section> : null}
      {runtime.retainedPresentations?.map((occurrence, index) => <Section key={index} title="Retained occurrence">
        <KeyValueRows rows={Object.entries(occurrence.identity).map(([label, value]) => ({ label, value }))} />
        {occurrence.details.code_presentation && safeOutputs(occurrence.details.code_presentation, occurrence.output, occurrence.output_value_status).map(value =>
          <CodeValue key={value.name} label={value.name} text={value.text} descriptor={value.descriptor} status={value.status} />)}
      </Section>)}
      {runtime.codePresentation ? <Section title="Declared code outputs">
        {safeOutputs(runtime.codePresentation, runtime.output, runtime.outputValueStatus).map(value =>
          <CodeValue key={value.name} label={value.name} text={value.text} descriptor={value.descriptor} status={value.status} />)}
      </Section> : null}
      <Section title="Output" count={runtime.output ? Object.keys(runtime.output).length : 0}>
        <DataObject value={runtime.output && Object.fromEntries(Object.entries(runtime.output).filter(([name]) =>
          !runtime.codePresentation?.outputs.some(field => field.name === name && field.presentation)))} empty="No additional output was retained." />
      </Section>
      <Section title="Captures" count={runtime.captures ? Object.keys(runtime.captures).length : 0}>
        <DataObject value={runtime.captures} empty="No variables were captured." />
      </Section>
      {runtime.logs && runtime.logs.length > 0 ? (
        <Section title="Process output" count={runtime.logs.length}>
          <div className="runtime-log">{runtime.logs.map((entry, index) => <pre className={`stream-${entry.stream}`} key={`${index}:${entry.stream}`}>{entry.line}</pre>)}</div>
        </Section>
      ) : null}
      {runtime.evidence ? <Section title="Evidence"><pre className="json-block">{JSON.stringify(runtime.evidence, null, 2)}</pre></Section> : null}
    </div>
  );
}

function DebugPane({ controls, override }: { controls: ReactNode; override: Record<string, unknown> | undefined }) {
  return (
    <div className="inspector-pane">
      {override ? (
        <Section title="Applied override" className="debug-audit">
          <pre className="json-block">{JSON.stringify(override, null, 2)}</pre>
        </Section>
      ) : null}
      {controls}
    </div>
  );
}

export function RunOverview({
  document,
  runtimeNodes,
  executionNodeID,
  runID,
  runStatus,
  inputs,
  breakpointCount,
  diagnostics,
  activeNodeIDs,
}: {
  document: GraphDocument;
  runtimeNodes: Readonly<Record<string, InspectorRuntimeState>>;
  executionNodeID?: string;
  runID?: string;
  runStatus: string;
  inputs: InspectorInputValue[];
  breakpointCount: number;
  diagnostics: string;
  activeNodeIDs: ReadonlySet<string>;
}) {
  const counts = canonicalProgress(document, runtimeNodes, runStatus);
  const { completed, issues, running, skipped, remaining, total } = counts;
  const executionNode = document.nodes.find((node) => node.id === executionNodeID);
  const settled = completed + issues + skipped;
  const terminal = isExecutionEnded(runStatus);
  const progress = total === 0 ? 0 : Math.round((settled / total) * 100);
  return (
    <div className="run-overview">
      <header className="overview-header">
        <div className="inspector-icon"><Activity aria-hidden="true" /></div>
        <div><span>Run overview</span><h2>{document.runbook.name ?? document.runbook.id ?? 'Runbook'}</h2></div>
        <StatusBadge status={runStatus} />
      </header>
      <div
        className="overview-progress"
        role="progressbar"
        aria-label="Canonical step statuses"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={progress}
        aria-valuetext={`${settled} of ${total} canonical steps have final status`}
      >
        <span style={{ width: `${progress}%` }} />
      </div>
      <div className="overview-stats">
        <div><strong>{completed}</strong><span>Done</span></div>
        <div><strong>{issues}</strong><span>Issues</span></div>
        <div><strong>{skipped}</strong><span>Skipped</span></div>
        <div><strong>{running}</strong><span>Running</span></div>
        <div><strong>{remaining}</strong><span>{terminal ? 'No final status' : 'Remaining'}</span></div>
      </div>
      <p className="overview-hint">{total} canonical steps · latest observed status per step, including containers. Runtime child activity is listed separately.</p>
      {terminal && remaining > 0 ? <p role="status">Run ended, some steps lack final status.</p> : null}
      <Section title="Run">
        <KeyValueRows rows={[
          { label: terminal ? 'Last reached' : 'Current', value: executionNode?.data.title ?? executionNode?.id, code: true },
          { label: 'Run ID', value: runID, code: true },
          { label: 'Steps', value: document.nodes.length },
          { label: 'Breakpoints', value: breakpointCount },
          { label: 'Source', value: document.runbook.path, code: true },
        ]} />
      </Section>
      <Section title="Inputs" count={inputs.length}>
        <div className="named-value-list">
          {inputs.map((input) => <div className="named-value-row" key={input.name}><code>{input.name}</code><span className={input.secret ? 'redacted-value' : ''}>{input.secret && input.value ? 'set' : input.value || 'unset'}</span></div>)}
        </div>
      </Section>
      {diagnostics ? <Section title="Diagnostics"><pre className="runtime-log overview-diagnostics">{diagnostics}</pre></Section> : null}
      <p className="overview-hint">Select a step for definition and execution details.</p>
    </div>
  );
}

export function StepInspector({
  node,
  runtime,
  availableGraphRevisions = [],
  selectedGraphRevision,
  onGraphRevisionChange,
  requestedOccurrenceID,
  onOccurrenceChange,
  snapshotDigest,
  debugControls,
}: {
  node: GraphNode;
  runtime: InspectorRuntimeState | undefined;
  availableGraphRevisions?: readonly number[];
  selectedGraphRevision?: number;
  onGraphRevisionChange?(revision: number): void;
  requestedOccurrenceID?: string;
  onOccurrenceChange?(occurrence: InspectorRuntimeOccurrence): void;
  snapshotDigest?: string;
  debugControls: ReactNode;
}) {
  type InspectorTab = 'definition' | 'run' | 'debug';
  const tabOrder: InspectorTab[] = ['definition', 'run', 'debug'];
  const [tab, setTab] = useState<InspectorTab>(runtime && runtime.status !== 'pending' ? 'run' : 'definition');
  const [selectedOccurrenceID, setSelectedOccurrenceID] = useState<string>();
  const tabBaseID = useId();
  const tabRefs = useRef<Record<InspectorTab, HTMLButtonElement | null>>({ definition: null, run: null, debug: null });
  const occurrences: InspectorRuntimeOccurrence[] = runtime?.occurrences?.length ? runtime.occurrences :
    (runtime?.retainedPresentations ?? []).map(occurrence => ({
      occurrenceID: JSON.stringify(occurrence.identity), runID: runtime?.runID ?? '', segmentID: '',
      qualifiedNodeID: occurrence.identity.qualified_node_id, phase: 'execute',
      invocation: occurrence.identity.invocation ?? 1, retryAttempt: occurrence.identity.retry_attempt ?? 1,
      occurrenceSequence: occurrence.identity.occurrence_sequence ?? 0, executionSource: 'saved', status: 'retained',
      output: occurrence.output, codePresentation: occurrence.details.code_presentation,
      outputValueStatus: occurrence.output_value_status, displayPresentation: occurrence.display_presentation,
      retainedDetails: occurrence.details,
    }));
  const selectedOccurrence = occurrences.find((occurrence) => occurrence.occurrenceID === (selectedOccurrenceID ?? requestedOccurrenceID))
    ?? occurrences.find((occurrence) => occurrence.occurrenceID === runtime?.occurrenceID)
    ?? occurrences[occurrences.length - 1];
  const displayedRuntime = selectedOccurrence ?? runtime;
  useEffect(() => { setSelectedOccurrenceID(requestedOccurrenceID); }, [requestedOccurrenceID]);
  useEffect(() => {
    setTab(displayedRuntime && displayedRuntime.status !== 'pending' ? 'run' : 'definition');
    setSelectedOccurrenceID(undefined);
  }, [node.id]);
  useEffect(() => {
    if (displayedRuntime && displayedRuntime.status !== 'pending' && tab === 'definition') setTab('run');
  }, [displayedRuntime?.status]);
  const kind = String(node.data.kind ?? 'step');
  const title = String(node.data.title ?? node.data.step_id ?? node.id);
  const displayNodeID = String(node.data.original_node_id ?? node.data.step_id ?? node.id);
  const segmentOrdinal = typeof node.data.segment_ordinal === 'number' ? node.data.segment_ordinal : undefined;
  const segmentStatus = typeof node.data.segment_status === 'string' ? node.data.segment_status : undefined;
  const executionSource = selectedOccurrence?.executionSource ??
    (typeof node.data.execution_source === 'string' ? node.data.execution_source : undefined);
  const sessionRunID = selectedOccurrence?.runID ??
    (typeof node.data.run_id === 'string' ? node.data.run_id : undefined);
  const onTabKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>) => {
    let nextIndex = tabOrder.indexOf(tab);
    if (event.key === 'ArrowRight') nextIndex = (nextIndex + 1) % tabOrder.length;
    else if (event.key === 'ArrowLeft') nextIndex = (nextIndex - 1 + tabOrder.length) % tabOrder.length;
    else if (event.key === 'Home') nextIndex = 0;
    else if (event.key === 'End') nextIndex = tabOrder.length - 1;
    else return;
    event.preventDefault();
    const nextTab = tabOrder[nextIndex];
    setTab(nextTab);
    tabRefs.current[nextTab]?.focus();
  };
  const tabID = (value: InspectorTab) => `${tabBaseID}-${value}-tab`;
  const panelID = (value: InspectorTab) => `${tabBaseID}-${value}-panel`;
  return (
    <div className="step-inspector">
      <div className="inspector-sticky">
        <header className="step-inspector-header">
          <div className="inspector-icon"><KindIcon kind={kind} /></div>
          <div className="inspector-title"><span>{KIND_LABELS[kind] ?? kind}{segmentOrdinal ? ` | Segment ${segmentOrdinal}` : ''}</span><h2>{title}</h2><code title={node.id}>{displayNodeID}</code></div>
          <StatusBadge status={displayedRuntime?.status ?? 'pending'} />
        </header>
        <div className="inspector-metrics">
          <span><Clock3 aria-hidden="true" />{formatDuration(displayedRuntime?.durationMs)}</span>
          {selectedOccurrence ? <span>Invocation {selectedOccurrence.invocation}</span> : null}
          {selectedOccurrence?.retryAttempt && selectedOccurrence.retryAttempt > 1
            ? <span>Retry {selectedOccurrence.retryAttempt}</span>
            : displayedRuntime?.attempt ? <span>Attempt {displayedRuntime.attempt}</span> : null}
          {node.data.call_path && Array.isArray(node.data.call_path) && node.data.call_path.length > 0 ? <span>{node.data.call_path.length} levels deep</span> : null}
          {executionSource ? <span>{executionSource === 'saved' ? 'Saved result' : 'Live execution'}</span> : null}
          {segmentStatus ? <span>Segment {segmentStatus.replaceAll('_', ' ')}</span> : null}
          {sessionRunID ? <span title={sessionRunID}>Run {sessionRunID.slice(0, 8)}</span> : null}
        </div>
        {availableGraphRevisions.length > 1 && selectedGraphRevision !== undefined ? (
          <label className="revision-selector">
            <span>Graph revision</span>
            <select
              aria-label="Graph revision"
              value={selectedGraphRevision}
              onChange={(event) => onGraphRevisionChange?.(Number(event.target.value))}
            >
              {[...availableGraphRevisions].reverse().map((revision, index) => (
                <option key={revision} value={revision}>
                  Revision {revision}{index === 0 ? ' (latest)' : ''}
                </option>
              ))}
            </select>
          </label>
        ) : null}
        {occurrences.length > 1 ? (
          <label className="occurrence-selector">
            <span>Execution occurrence</span>
            <select
              aria-label="Execution occurrence"
              value={selectedOccurrence?.occurrenceID ?? ''}
              onChange={(event) => {
                setSelectedOccurrenceID(event.target.value);
                const occurrence = occurrences.find(value => value.occurrenceID === event.target.value);
                if (occurrence) onOccurrenceChange?.(occurrence);
              }}
            >
              {occurrences.map((occurrence) => (
                <option key={occurrence.occurrenceID} value={occurrence.occurrenceID}>
                  {occurrence.executionSource === 'saved' ? 'Saved' : 'Live'} | run {occurrence.runID.slice(0, 8)} | invocation {occurrence.invocation}
                </option>
              ))}
            </select>
          </label>
        ) : null}
        <div className="inspector-tabs" role="tablist" aria-label="Step information">
          <button ref={(element) => { tabRefs.current.definition = element; }} id={tabID('definition')} type="button" role="tab" aria-controls={panelID('definition')} aria-selected={tab === 'definition'} tabIndex={tab === 'definition' ? 0 : -1} className={tab === 'definition' ? 'active' : ''} onKeyDown={onTabKeyDown} onClick={() => setTab('definition')}>Definition</button>
          <button ref={(element) => { tabRefs.current.run = element; }} id={tabID('run')} type="button" role="tab" aria-controls={panelID('run')} aria-selected={tab === 'run'} tabIndex={tab === 'run' ? 0 : -1} className={tab === 'run' ? 'active' : ''} onKeyDown={onTabKeyDown} onClick={() => setTab('run')}>Run</button>
          <button ref={(element) => { tabRefs.current.debug = element; }} id={tabID('debug')} type="button" role="tab" aria-controls={panelID('debug')} aria-selected={tab === 'debug'} tabIndex={tab === 'debug' ? 0 : -1} className={tab === 'debug' ? 'active' : ''} onKeyDown={onTabKeyDown} onClick={() => setTab('debug')}>Debug</button>
        </div>
      </div>
      {tab === 'definition' ? <div id={panelID('definition')} role="tabpanel" aria-labelledby={tabID('definition')} tabIndex={0}><DefinitionPane details={node.data.details} /><SessionProvenance node={node} /></div> : null}
      {tab === 'run' ? <div id={panelID('run')} role="tabpanel" aria-labelledby={tabID('run')} tabIndex={0}><RuntimePane runtime={displayedRuntime}
        markdownDeclared={(selectedOccurrence?.retainedDetails ?? node.data.details)?.kind === 'display' &&
          (selectedOccurrence?.retainedDetails ?? node.data.details)?.format === 'markdown'}
        snapshotDigest={displayedRuntime?.directDisplaySnapshot ?? snapshotDigest ?? 'missing-binding'} /></div> : null}
      {tab === 'debug' ? <div id={panelID('debug')} role="tabpanel" aria-labelledby={tabID('debug')} tabIndex={0}><DebugPane controls={debugControls} override={displayedRuntime?.debugOverride} /></div> : null}
    </div>
  );
}

function SessionProvenance({ node }: { node: GraphNode }) {
  if (typeof node.data.session_id !== 'string') return null;
  const transitions = [
    ...recordList(node.data.incoming_transitions).map((transition) => ({ direction: 'From', transition })),
    ...recordList(node.data.outgoing_transitions).map((transition) => ({ direction: 'To', transition })),
  ];
  return (
    <div className="inspector-pane session-provenance">
      <Section title="Snapshot">
        <KeyValueRows rows={[
          { label: 'Runbook', value: node.data.runbook_name ?? node.data.runbook_id },
          { label: 'Runbook ID', value: node.data.runbook_id, code: true },
          { label: 'Graph revision', value: node.data.graph_revision },
          { label: 'Graph hash', value: node.data.graph_hash, code: true },
          { label: 'Plan hash', value: node.data.plan_hash, code: true },
          { label: 'Executable snapshot', value: node.data.executable_snapshot_hash, code: true },
          { label: 'Catalog', value: node.data.catalog_digest, code: true },
          { label: 'Package lock', value: node.data.package_lock_digest, code: true },
          { label: 'Profile', value: node.data.profile_digest, code: true },
        ]} />
      </Section>
      {transitions.length > 0 ? (
        <Section title="Handoffs" count={transitions.length}>
          <div className="compact-list">
            {transitions.map(({ direction, transition }) => (
              <div key={`${direction}:${String(transition.transition_id)}`}>
                <strong>{direction} {String(direction === 'From' ? transition.source_segment_id : transition.target_segment_id)}</strong>
                <span>{String(transition.reason_summary ?? transition.reason_code ?? transition.status ?? '')}</span>
              </div>
            ))}
          </div>
        </Section>
      ) : null}
    </div>
  );
}

function recordList(value: unknown): Array<Record<string, unknown>> {
  return Array.isArray(value)
    ? value.filter((item): item is Record<string, unknown> => typeof item === 'object' && item !== null && !Array.isArray(item))
    : [];
}

export function CommonDefinition({ common }: { common: CommonStepDetails | undefined }) {
  if (!common) return null;
  const executionRows = [
    { label: 'When', value: common.when, code: true, path: '/common/when' },
    { label: 'Timeout', value: common.timeout },
    { label: 'Delay', value: common.delay },
    { label: 'On error', value: common.on_error || (common.continue_on_fail ? 'continue' : undefined) },
    { label: 'Scope', value: common.scope },
    { label: 'Retry', value: common.retry ? `${common.retry.max} max${common.retry.interval ? ` · ${common.retry.interval}` : ''}${common.retry.backoff ? ` · ${common.retry.backoff}` : ''}` : undefined },
  ];
  const hasExecutionPolicy = executionRows.some((row) => row.value !== undefined && row.value !== '');
  return (
    <>
      {common.subtitle ? <p className="definition-summary">{common.subtitle}</p> : null}
      {hasExecutionPolicy ? <Section title="Execution"><KeyValueRows rows={executionRows} /></Section> : null}
      {common.captures && common.captures.length > 0 ? <Section title="Captures" count={common.captures.length}><div className="named-value-list">{common.captures.map((capture, index) =>
        <div className="named-value-row" key={capture.name}><code>{capture.name}</code><span><AuthoredValue value={capture.source} path={`/common/captures/${index}/source`} />
          {capture.has_default ? ` · default ${scalarText(capture.default)}` : ''}</span></div>)}</div></Section> : null}
      {common.exports && common.exports.length > 0 ? <Section title="Exports"><div className="tag-list">{common.exports.map((item) => <code key={item}>{item}</code>)}</div></Section> : null}
      {common.contract ? <Section title="Contract"><KeyValueRows rows={[
        { label: 'Effects', value: common.contract.effects?.join(', ') },
        { label: 'Reads', value: common.contract.reads?.join(', ') },
        { label: 'Writes', value: common.contract.writes?.join(', ') },
        { label: 'Idempotent', value: common.contract.idempotent ? 'yes' : undefined },
        { label: 'Deterministic', value: common.contract.deterministic ? 'yes' : undefined },
      ]} /></Section> : null}
      {common.evidence && common.evidence.length > 0 ? <Section title="Required evidence" count={common.evidence.length}><div className="compact-list">{common.evidence.map((item) => <div key={`${item.kind}:${item.name}`}><strong>{item.label || item.name}</strong><span>{item.kind}</span></div>)}</div></Section> : null}
    </>
  );
}

export function DefinitionPane({ details }: { details: StepDetails | undefined }) {
  if (!details) return <div className="inspector-blank"><Activity aria-hidden="true" /><strong>Definition unavailable</strong><span>Rebuild the Yawr helper to load enriched step details.</span></div>;
  return <AuthoredValues details={details}><div className="inspector-pane"><KindDefinition details={details} /><CommonDefinition common={details.common} /></div></AuthoredValues>;
}

function KindDefinition({ details }: { details: StepDetails }) {
  switch (details.kind) {
    case 'cli':
      return (
        <>
          <Section title="Command">
            {details.command || details.args?.length ? <pre className="command-block">{details.command ? <AuthoredValue value={details.command} path="/command" /> : null}
              {details.args?.map((arg, index) => <React.Fragment key={index}>{details.command || index ? ' ' : ''}<AuthoredValue value={arg} path={`/args/${index}`} /></React.Fragment>)}</pre> : <p className="inspector-muted">No command declared.</p>}
            {details.script ? <pre className="command-block"><AuthoredValue value={details.script} path="/script" /></pre> : null}
          </Section>
          <Section title="Process">
            <KeyValueRows rows={[
              { label: 'Shell', value: details.shell, code: true, path: '/shell' },
              { label: 'Working dir', value: details.workdir, code: true, path: '/workdir' },
              { label: 'Environment', value: details.env_names?.length ? `${details.env_names.length} variables` : undefined },
              { label: 'Stdin', value: details.stdin ? 'provided' : undefined },
            ]} />
            {details.env_names && details.env_names.length > 0 ? <div className="tag-list">{details.env_names.map((name) => <code key={name}>{name}</code>)}</div> : null}
          </Section>
        </>
      );
    case 'tool':
      return (
        <>
          <Section title="Tool action">
            <div className="definition-call"><Wrench aria-hidden="true" /><code><AuthoredValue value={details.tool || 'unknown'} path="/tool" /> / <AuthoredValue value={details.action || 'unknown'} path="/action" /></code></div>
            {details.version ? <KeyValueRows rows={[{ label: 'Version', value: details.version }]} /> : null}
          </Section>
          <Section title="Authored template" count={details.arguments?.length ?? 0}>
            {details.arguments?.map((item, index) => {
              const field = details.code_presentation?.arguments.find(f => f.name === item.name);
              return field?.presentation && typeof item.value === 'string'
                ? <CodeValue key={item.name} label={item.name} text={item.redacted ? undefined : item.value}
                    expressionPath={`/arguments/${index}/value`} descriptor={field.status === 'resolved' ? field.presentation : undefined} status={item.redacted ? 'redacted' : 'available'} />
                : <NamedValueRows key={item.name} values={[item]} path="/arguments" startIndex={index} />;
            })}
          </Section>
        </>
      );
    case 'include':
      return (
        <>
          <Section title="Target">
            <KeyValueRows rows={[
              { label: 'Runbook', value: details.reference, code: true, path: '/reference' },
              { label: 'Resolution', value: details.dynamic ? `dynamic · ${details.resolve_from || 'catalog'}` : 'static' },
              { label: 'Expansion', value: details.expand || 'inherited' },
              { label: 'Not found', value: details.on_not_found || 'fail' },
              { label: 'Child steps', value: details.steps },
            ]} />
          </Section>
          <Section title="Bindings" count={details.bindings?.length ?? 0}><NamedValueRows values={details.bindings} path="/bindings" /></Section>
          {details.stop_if && details.stop_if.length > 0 ? <Section title="Stop outcomes"><div className="tag-list">{details.stop_if.map((item) => <code key={item}>{item}</code>)}</div></Section> : null}
        </>
      );
    case 'choice':
      return (
        <>
          <Section title="Prompt"><p className="definition-copy"><AuthoredValue value={details.prompt || 'No prompt.'} path="/prompt" /></p></Section>
          <Section title="Selection">
            <KeyValueRows rows={[
              { label: 'Variable', value: details.variable, code: true },
              { label: 'Mode', value: details.multiple ? 'multiple' : 'single' },
              { label: 'Required', value: details.multiple ? `${details.min ?? 0}–${details.max ?? 'any'}` : undefined },
              { label: 'Default', value: details.default, code: true, path: '/default' },
            ]} />
          </Section>
          <Section title="Options" count={details.options?.length ?? 0}><div className="compact-list">{details.options?.map((option, index) => <div key={option.value}><strong><AuthoredValue value={option.label} path={`/options/${index}/label`} /></strong><code>{option.value}</code>{option.hint ? <span><AuthoredValue value={option.hint} path={`/options/${index}/hint`} /></span> : null}</div>)}</div></Section>
        </>
      );
    case 'decision':
      return (
        <>
          <Section title="Prompt"><p className="definition-copy"><AuthoredValue value={details.prompt || 'No prompt.'} path="/prompt" /></p><KeyValueRows rows={[{ label: 'Variable', value: details.variable, code: true }]} /></Section>
          <Section title="Routes" count={details.routes?.length ?? 0}><div className="compact-list">{details.routes?.map((route, index) => <div key={`${index}:${route.label}`}><strong><AuthoredValue value={route.label} path={`/routes/${index}/label`} /></strong><code>{route.goto || route.runbook || 'inline'}</code>{route.hint ? <span><AuthoredValue value={route.hint} path={`/routes/${index}/hint`} /></span> : null}</div>)}</div></Section>
        </>
      );
    case 'collector':
      return (
        <>
          <Section title="Prompt"><p className="definition-copy"><AuthoredValue value={details.prompt || 'No prompt.'} path="/prompt" /></p></Section>
          <Section title="Fields" count={details.fields?.length ?? 0}>
            <div className="field-list">{details.fields?.map((field, index) => {
              const name = typeof field.name === 'string' ? field.name : `field-${index + 1}`;
              const type = typeof field.type === 'string' ? field.type : 'text';
              const label = typeof field.label === 'string' ? field.label : name;
              return <div key={name}><div><strong><AuthoredValue value={label} path={`/fields/${index}/label`} /></strong><code>{name}</code></div><span>{type}{field.required === true ? ' · required' : ''}{field.multiple === true ? ' · multiple' : ''}{field.multiline === true ? ' · multiline' : ''}</span>
                {typeof field.hint === 'string' ? <p><AuthoredValue value={field.hint} path={`/fields/${index}/hint`} /></p> : null}
                <KeyValueRows rows={[{ label: 'When', value: field.when, path: `/fields/${index}/when`, code: true },
                  { label: 'Default', value: field.default, path: `/fields/${index}/default` }]} />
                {Array.isArray(field.options) ? <div className="compact-list">{field.options.map((option, optionIndex) => {
                  if (!option || typeof option !== 'object') return null;
                  return <div key={optionIndex}><strong><AuthoredValue value={option.label} path={`/fields/${index}/options/${optionIndex}/label`} /></strong>
                    {option.hint ? <span><AuthoredValue value={option.hint} path={`/fields/${index}/options/${optionIndex}/hint`} /></span> : null}</div>;
                })}</div> : null}</div>;
            })}</div>
          </Section>
        </>
      );
    case 'host_action':
      return (
        <>
          <Section title="Host capability"><div className="definition-call"><Puzzle aria-hidden="true" /><code>{details.capability || 'unknown'}</code></div></Section>
          <Section title="Request" count={details.request?.length ?? 0}><NamedValueRows values={details.request} path="/request" /></Section>
        </>
      );
    case 'branch':
      return <Section title="Ordered arms" count={details.arms?.length ?? 0}><div className="arm-list">{details.arms?.map((arm, index) => <div key={index}><span>{index + 1}</span><div><strong>{arm.label || (arm.else ? 'Otherwise' : `Arm ${index + 1}`)}</strong><code>{arm.else ? 'else' : <AuthoredValue value={arm.condition || 'always'} path={`/arms/${index}/condition`} />}</code></div><em>{arm.steps} steps</em></div>)}</div></Section>;
    case 'iterate':
      return (
        <>
          <Section title="Loop">
            <KeyValueRows rows={[
              { label: 'Over', value: details.over, code: true, path: '/over' },
              { label: 'As', value: details.as, code: true },
              { label: 'Until', value: details.until, code: true, path: '/until' },
              { label: 'Maximum', value: details.max },
              { label: 'Concurrency', value: details.concurrency || 1 },
              { label: 'Body', value: details.steps !== undefined ? `${details.steps} steps` : undefined },
            ]} />
          </Section>
          <Section title="Collect"><NamedValueRows values={details.collect} path="/collect" /></Section>
        </>
      );
    case 'parallel':
      return (
        <>
          <Section title="Branches"><KeyValueRows rows={[{ label: 'Count', value: details.branches }, { label: 'Wait for', value: details.wait_for || 'all' }, { label: 'On failure', value: details.on_failure || 'fail' }]} /></Section>
          {details.branch_labels && details.branch_labels.length > 0 ? <Section title="Branch labels"><div className="tag-list">{details.branch_labels.map((label, index) => <span key={`${index}:${label}`}>{label || `Branch ${index + 1}`}</span>)}</div></Section> : null}
        </>
      );
    case 'approve':
      return (
        <>
          <Section title="Approval policy"><KeyValueRows rows={[{ label: 'Required', value: details.required || 1 }, { label: 'Timeout', value: details.timeout }, { label: 'On timeout', value: details.on_timeout }, { label: 'Timezone', value: details.timezone }, { label: 'Calendar', value: details.business_calendar }]} /></Section>
          {details.roles && details.roles.length > 0 ? <Section title="Roles"><div className="tag-list">{details.roles.map((role) => <span key={role}>{role}</span>)}</div></Section> : null}
          {details.pool && details.pool.length > 0 ? <Section title="Approver pool"><div className="tag-list">{details.pool.map((member) => <span key={member}>{member}</span>)}</div></Section> : null}
          {details.escalate_to && details.escalate_to.length > 0 ? <Section title="Escalation"><div className="tag-list">{details.escalate_to.map((role) => <span key={role}>{role}</span>)}</div></Section> : null}
        </>
      );
    case 'assert':
      return <Section title="Assertions" count={details.assertions?.length ?? 0}><div className="assertion-list">{details.assertions?.map((assertion, index) => <div key={index}><CheckCircle2 aria-hidden="true" /><div><strong>{assertion.type}</strong><code><AuthoredValue value={assertion.subject} path={`/assertions/${index}/subject`} /></code>{assertion.expected ? <span>Expected: <AuthoredValue value={assertion.expected} path={`/assertions/${index}/expected`} /></span> : null}{assertion.path ? <span>Path: {assertion.path}</span> : null}</div></div>)}</div></Section>;
    case 'wait_for_event':
      return (
        <>
          <Section title="Event"><KeyValueRows rows={[{ label: 'Source', value: details.source }, { label: 'ID', value: details.event_id, code: true, path: '/event_id' }, { label: 'Schema', value: details.payload_schema, code: true }, { label: 'On timeout', value: details.on_timeout }]} /></Section>
          <Section title="Filter" count={details.filter?.length ?? 0}><NamedValueRows values={details.filter} path="/filter" /></Section>
        </>
      );
    case 'display':
      return <Section title="Rendered content"><KeyValueRows rows={[{ label: 'Format', value: details.format || 'text' }]} /><pre className="content-preview"><AuthoredValue value={details.content || 'No content.'} path="/content" /></pre></Section>;
    case 'end':
      return <Section title="Terminal outcome"><div className="outcome-display"><Flag aria-hidden="true" /><div><strong>{details.category || 'unspecified'}</strong><code>{details.code || 'no code'}</code></div></div></Section>;
    case 'compensate':
      return <Section title="Compensation"><KeyValueRows rows={[{ label: 'Trigger', value: details.on || 'failure' }, { label: 'Body', value: details.steps !== undefined ? `${details.steps} steps` : undefined }]} /></Section>;
    case 'noop':
      return <Section title="No-op"><p className="definition-copy">This step changes no external state. Execution policy, delay, and captures are shown below.</p></Section>;
    case 'assign':
      return <Section title="Atomic binding assignments" count={details.assign?.length}>
        <p className="definition-copy">Technical operation. All writes validate together; no external work is performed.</p>
        <NamedValueRows values={details.assign} path="/assign" />
      </Section>;
    case 'results':
      return <Section title="Results"><p className="definition-copy">
        Publishes this runbook's declared named outputs atomically and finishes this scope.
        The full structured result is shown only after canonical transport validation.
      </p></Section>;
    case 'extension':
      return <Section title="Extension"><p className="definition-copy">Execution is delegated to a registered Yawr extension.</p></Section>;
  }
}
