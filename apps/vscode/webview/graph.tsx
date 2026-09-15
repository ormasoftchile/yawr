import dagre from '@dagrejs/dagre';
import { ArrowLeft, ArrowRight, Bug, CheckCircle2, CircleDot, LocateFixed, PanelRight, Play, RotateCcw, Square, Workflow } from 'lucide-react';
import React, { createContext, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import ReactFlow, {
  applyNodeChanges,
  Background,
  BackgroundVariant,
  Controls,
  Handle,
  MarkerType,
  MiniMap,
  NodeToolbar,
  Position,
  ReactFlowProvider,
  type Edge,
  type Node,
  type NodeProps,
  type ReactFlowInstance,
  type Viewport,
} from 'reactflow';
import 'reactflow/dist/style.css';
import './graph.css';
import { terminalPresentation } from '../src/presentationProjection';
import { applyDirectRetainedDocument, applyDirectDisplaySummary, reconcileDirectDisplayState,
  beginDirectDisplayOccurrence, retainDirectDisplayWithdrawals, restoreDirectDisplayWithdrawals, type DisplayObservations } from '../src/displayObservations';
import { decodeDisplayWithdrawals } from '../src/displayObservationStorage';
import { projectWorkflow, workflowIssueIndex, type WorkflowIssue, type WorkflowMode } from '../src/workflowProjection';
import { preserveLayoutMeasurements } from '../src/graphLayoutMeasurements';
import { canonicalProgress, currentActivities, graphExecutionNodeID, compareOccurrences, directOccurrenceID, validProgressIdentity, producerStepKind, displayRuntimeStatuses, isExecutionEnded, normalizeRuntimeStatuses, type CurrentActivity } from '../src/executionProgress';
import { CurrentActivity as ActivityDetails } from './CurrentActivity';
import { currentExecutionNode, ordinaryVisualNodeID, executionViewMode, executionViewport, animateExecutionViewport } from '../src/executionView';
import { VisualStepPacer, type VisualStep } from '../src/visualStepPacer';
import { mergeExecutionGraph, executionHistory, executionReturnEdges, anchorExecutionLayout } from '../src/executionGraph';
import { decodeWorkflowPreference, mergeWorkflowPreference } from '../src/workflowView';
import type { ResultsAvailability } from '../src/typedResultsTypes';
import { ResultsViewer } from './ResultsViewer';
import type { RetainedPresentation } from '../src/presentationHistory';
import { configureHighlighting } from './highlighting/browserClient';
configureHighlighting(document.body.dataset.highlightingWorker ?? '');
import {
  RunOverview,
  StepInspector,
  type InspectorRuntimeState,
  type RuntimeLogLine,
} from './inspector';
import type {
  GraphDocument,
  GraphGroup,
  GraphNode,
  GraphNodeData,
} from '../src/directGraphPreview';
import type { DirectDebugBreakpoint, DirectDebugCallFrame, DirectDebugPhase } from '../src/directDebug';
import { activeGraphNodeIDs, edgeRuntimeState, withBranchMerges } from '../src/branchTopology';
import type { GraphEdge } from '../src/directGraphPreview';
import { isIssueStepStatus, isSettledStepStatus, isTerminalRunStatus } from '../src/runStatus';
import {
  buildRouteProjectionIndex,
  computeRouteProjection,
  computeSessionRouteProjection,
  projectRouteDocument,
  sessionRouteDocument,
  type RouteProjectionScope,
} from '../src/routeProjection';
import type { RouteTestArtifact } from '../src/routeTestTypes';
import { sessionGraphTopologyKey, type SessionGraphViewState } from '../src/sessionCompositeGraph';
import { parseHostActionResponse, type HostActionResponseEnvelope } from '../src/hostActionWebviewProtocol';
import { matchesXtsViewCheck, type XtsViewCheck } from '../src/xtsViewVerification';
import {
  collectorInputType,
  formatCollectorReviewValue as collectorReviewValue,
  normalizeCollectorValues as collectorValues,
} from '../src/collectorFieldValues';
import {
  RouteTestPane,
  routeTestMatchesTarget,
  type RouteTestOutcome,
  type SavedRouteTestView,
} from './routeTestPane';

type NodeStyle = 'smooth-curves' | 'minimalist' | 'header-badges';
const RENDER_TELEMETRY_SCHEMA = 'yawr.render-telemetry/v1';

type HostMessage =
  | { type: 'workflow-markdown.preference'; preference: unknown }
  | { type: 'highlighting'; enabled: boolean }
  | { type: 'loading' }
  | { type: 'preview.visibility'; visible: boolean }
  | {
      type: 'graph';
      document: GraphDocument;
      style: NodeStyle;
      routeTestContext?: { runbook: string; planHash: string };
      routeTests?: SavedRouteTestView[];
      testMode?: boolean;
    }
  | { type: 'style'; style: NodeStyle }
  | { type: 'graph.reload-state'; active: boolean }
  | { type: 'error'; message: string }
  | { type: 'run.starting'; routeTest?: boolean; minimumStepDisplayMs?: number }
  | { type: 'run.frame'; frame: StdioFrame }
  | { type: 'run.error'; message: string }
  | { type: 'run.stderr'; text: string }
  | { type: 'run.exit'; code: number | null; signal: string | null }
  | { type: 'session.starting'; sessionID: string; minimumStepDisplayMs?: number }
  | { type: 'session.reconnecting'; sessionID: string; minimumStepDisplayMs?: number }
  | { type: 'session.update'; state: SessionGraphViewState; style: NodeStyle; live?: boolean; liveSteps?: string[]; minimumStepDisplayMs?: number }
  | { type: 'session.graph-revision'; requestID: string; node?: GraphNode; error?: string }
  | { type: 'session.error'; message: string }
  | { type: 'session.stderr'; text: string }
  | { type: 'session.exit'; code: number | null; signal: string | null }
  | { type: 'route-tests'; routeTests: SavedRouteTestView[] }
  | { type: 'route-test.saved'; artifact: RouteTestArtifact; running: boolean }
  | { type: 'route-test.error'; message: string }
  | {
      type: 'test.action';
      action: 'select-history' | 'select-runbook' | 'sample-execution-transition' | 'inspect-graph-visibility' | 'inspect-results' | 'set-graph-viewport' | 'set-input' | 'run' | 'debug' | 'reset' | 'cancel' | 'answer' | 'toggle-breakpoint' | 'select-node' | 'inspect-expressions' | 'run-route-test' | 'save-route-test' | 'save-route-test-result' | 'click-button' | 'click-route-test-checkbox' | 'toggle-choice' | 'set-collector-field';
      name?: string;
      value?: string;
      answer?: Record<string, unknown>;
      artifact?: RouteTestArtifact;
    }
  | HostActionResponseEnvelope
  | XtsViewCheck;

interface StdioFrame {
  document?: GraphDocument;
  nodeIDs?: string[];
  type: string;
  version: 'yawr.stdio/v1';
  runID?: string;
  status?: string;
  resultsAvailability?: ResultsAvailability;
  event?: RuntimeEvent;
  interaction?: PendingInteraction;
  turnID?: string;
  steps?: Array<{
    step_id?: string;
    node_id?: string;
    qualified_node_id?: string;
    phase?: string;
    invocation?: number;
    retry_attempt?: number;
    occurrence_sequence?: number;
    frame_id?: string;
    frame_step_index?: number;
    status?: string;
    error?: string;
    duration_ms?: number;
    output?: Record<string, unknown>;
    display_presentation?: unknown;
    display_presentation_diagnostic?: unknown;
  }>;
  routeTest?: RouteTestOutcome;
}

interface RuntimeEvent {
  kind: string;
  run_id: string;
  sequence: number;
  timestamp?: string;
  payload?: Record<string, unknown>;
}

type RuntimeNodeState = InspectorRuntimeState;

interface DebugActualResult {
  status: string;
  output?: Record<string, unknown>;
  error?: string;
}

interface DebugWatchResult {
  expression: string;
  value?: unknown;
  error?: string;
}

interface DebugBreakPayload {
  phase: DirectDebugPhase;
  callPath?: DirectDebugCallFrame[];
  invocation: number;
  attempt: number;
  variables?: Record<string, unknown>;
  protectedVariables?: string[];
  actual?: DebugActualResult;
  outputProtected?: boolean;
  canStepInto?: boolean;
  watches?: DebugWatchResult[];
}

interface DebugBreakpointView extends DirectDebugBreakpoint {
  nodeID: string;
}

interface InputDecl {
  name: string;
  type?: string;
  required?: boolean;
  default?: unknown;
  description?: string;
  enum?: string[];
  enumRedacted?: boolean;
  enumMemberCount?: number;
}

interface InteractionOption {
  label?: string;
  display_label?: string;
  display_value?: string;
  value: string;
  hint?: string;
}

interface InteractionField {
  name: string;
  display_name?: string;
  type: string;
  label?: string;
  required?: boolean;
  default?: unknown;
  hint?: string;
  options?: InteractionOption[];
  multiple?: boolean;
  ephemeral?: boolean;
  validation?: {
    min_length?: number;
    max_length?: number;
    pattern?: string;
    format?: string;
    min?: number;
    max?: number;
    step?: number;
  };
}

interface PendingInteraction {
  type: 'pending';
  runID: string;
  turnID: string;
  stepID: string;
  nodeID?: string;
  kind: 'choice' | 'decision' | 'collector' | 'approval' | 'host_action' | 'debug_break';
  correlationID?: string;
  title?: string;
  prompt?: string;
  options?: InteractionOption[];
  routes?: InteractionOption[];
  fields?: InteractionField[];
  multiple?: boolean;
  min?: number;
  max?: number;
  host_action?: { capability: string; request: Record<string, unknown> };
  debug?: DebugBreakPayload;
}

interface VsCodeApi {
  postMessage(message: unknown): void;
  getState?(): unknown;
  setState?(state: unknown): void;
}

declare function acquireVsCodeApi(): VsCodeApi;

const vscode = acquireVsCodeApi();

const DEFAULT_INSPECTOR_RATIO = 0.31;
const MIN_INSPECTOR_RATIO = 0.2;
const MAX_INSPECTOR_RATIO = 0.65;

function isClosedSessionStatus(status: string): boolean {
  return ['resolved', 'escalated', 'cancelled', 'abandoned'].includes(status);
}

function revisionNodeKey(segmentID: string, revision: number, originalNodeID: string): string {
  return JSON.stringify([segmentID, revision, originalNodeID]);
}

function clampInspectorRatio(value: number): number {
  return Math.min(MAX_INSPECTOR_RATIO, Math.max(MIN_INSPECTOR_RATIO, value));
}

function restoredInspectorRatio(): number {
  const state = vscode.getState?.();
  if (!state || typeof state !== 'object' || Array.isArray(state)) return DEFAULT_INSPECTOR_RATIO;
  const value = (state as { inspectorRatio?: unknown }).inspectorRatio;
  return typeof value === 'number' && Number.isFinite(value)
    ? clampInspectorRatio(value)
    : DEFAULT_INSPECTOR_RATIO;
}

function persistInspectorRatio(inspectorRatio: number): void {
  const previous = vscode.getState?.();
  const state = previous && typeof previous === 'object' && !Array.isArray(previous)
    ? previous as Record<string, unknown>
    : {};
  vscode.setState?.({ ...state, inspectorRatio });
}

const kindLabels: Record<string, string> = {
  approve: 'Approval',
  assert: 'Assert',
  branch: 'Branch',
  choice: 'Choice',
  cli: 'CLI',
  collector: 'Collector',
  compensate: 'Compensate',
  decision: 'Decision',
  display: 'Display',
  end: 'End',
  extension: 'Extension',
  include: 'Include',
  iterate: 'Iterate',
  noop: 'No-op',
  assign: 'Assign bindings',
  results: 'Results',
  parallel: 'Parallel',
  tool: 'Tool',
  wait_for_event: 'Wait event',
};

const RuntimeNodesContext = createContext<Readonly<Record<string, RuntimeNodeState>>>({});
const DebugBreakpointsContext = createContext<ReadonlySet<string>>(new Set());
interface ExecutionPosition {
  nodeID?: string; terminal: boolean; progressing?: boolean; status?: 'waiting' | 'paused';
}
const ExecutionPositionContext = createContext<ExecutionPosition>({ terminal: false });

function breakpointKey(nodeID: string, phase: DirectDebugPhase): string {
  return `${nodeID}:${phase}`;
}

function StepNode({ data, selected }: NodeProps<GraphNodeData>) {
  const runtimeNodes = useContext(RuntimeNodesContext);
  const debugBreakpoints = useContext(DebugBreakpointsContext);
  const executionPosition = useContext(ExecutionPositionContext);
  const kind = typeof data.kind === 'string' ? data.kind : 'step';
  const id = typeof data.id === 'string' ? data.id : '';
  const title = typeof data.title === 'string' ? data.title : '';
  const isTerminal = kind === 'end';
  const runtime = runtimeNodes[id];
  const isCurrent = executionPosition.nodeID === id;
  const observedStatus = isCurrent && executionPosition.status ? executionPosition.status
    : runtime?.output?.outcome_category === 'blocked' ? 'blocked'
    : runtime?.status ?? (typeof data.status === 'string' ? data.status : 'pending');
  const status = executionPosition.terminal && ['running', 'delaying', 'waiting'].includes(observedStatus)
    ? 'no-final-status' : observedStatus;
  const error = runtime?.error ?? (typeof data.error === 'string' ? data.error : '');
  const hasBeforeBreakpoint = debugBreakpoints.has(breakpointKey(id, 'before'));
  const hasAfterBreakpoint = debugBreakpoints.has(breakpointKey(id, 'after'));
  const locatorText = title || id;
  const focusedLabel = `Focused step: ${locatorText}`;
  const currentLabel = `Current step: ${locatorText}`;
  const executionLabel = executionPosition.terminal ? 'Last reached' : 'Current';
  const executionAriaLabel = executionPosition.terminal ? `Last reached step: ${locatorText}` : currentLabel;

  return (
    <>
      <NodeToolbar isVisible={selected} position={Position.Left} align="center" offset={12}>
        <div className="node-locator focused" role="status" aria-label={focusedLabel} title={focusedLabel}>
          <span>Focused</span>
          <code title={id}>{locatorText}</code>
          <ArrowRight aria-hidden="true" />
        </div>
      </NodeToolbar>
      <NodeToolbar isVisible={isCurrent} position={Position.Right} align="center" offset={12}>
        <div className={`node-locator ${executionPosition.terminal ? 'last-reached' : 'current'}`} role="status" aria-label={executionAriaLabel} title={executionAriaLabel}>
          <ArrowLeft aria-hidden="true" />
          <span>{executionLabel}</span>
          <code title={id}>{locatorText}</code>
        </div>
      </NodeToolbar>
      <div aria-current={isCurrent && !executionPosition.terminal ? 'step' : undefined}
        className={`step-node kind-${kind} status-${status}${selected ? ' selected' : ''}${isCurrent ? executionPosition.terminal ? ' execution-last' : ' execution-current' : ''}${isCurrent && executionPosition.progressing ? ' execution-progress' : ''}`}>
        <Handle type="target" position={Position.Top} />
        <div className="step-heading">
          <span className="kind-mark" aria-hidden="true">{kind.slice(0, 2).toUpperCase()}</span>
          <span>{kindLabels[kind] ?? kind}</span>
        </div>
        <div className="debug-node-markers">
          {hasBeforeBreakpoint ? <span aria-label="Before breakpoint" title="Pause before execution"><CircleDot className="debug-before-marker" aria-hidden="true" /></span> : null}
          {hasAfterBreakpoint ? <span aria-label="After breakpoint" title="Pause after execution"><CircleDot className="debug-after-marker" aria-hidden="true" /></span> : null}
          {runtime?.debugOverride ? <span aria-label="Debug override applied" title="Debug override applied"><Bug className="debug-override-marker" aria-hidden="true" /></span> : null}
        </div>
        <div className="step-id">{id}</div>
        {title && title !== id ? <div className="step-title">{title}</div> : null}
        {status !== 'pending' ? <div className="step-status">{status === 'no-final-status' ? 'No final status' : status}{error ? `: ${error}` : ''}</div> : null}
        {!isTerminal ? <Handle type="source" position={Position.Bottom} /> : null}
      </div>
    </>
  );
}

function FrameNode({ data }: NodeProps<GraphNodeData>) {
  const segmentStatus = typeof data.segment_status === 'string' ? data.segment_status : '';
  return (
    <div className="frame-content">
      <span>{String(data.label ?? data.kind ?? '')}</span>
      {segmentStatus ? <em className={`segment-status status-${segmentStatus}`}>{segmentStatus.replaceAll('_', ' ')}</em> : null}
      {data.graph_loaded === false ? <em>Load on demand</em> : null}
      {data.empty ? <em>Empty route</em> : null}
    </div>
  );
}

function BranchMergeNode() {
  return (
    <div className="branch-merge-node" title="Branch merge">
      <Handle type="target" position={Position.Top} />
      <span aria-hidden="true" />
      <Handle type="source" position={Position.Bottom} />
    </div>
  );
}

function SessionEntryNode() {
  return (
    <div className="session-entry-node" title="Segment entry">
      <Handle type="target" position={Position.Top} />
      <span aria-hidden="true" />
      <Handle type="source" position={Position.Bottom} />
    </div>
  );
}

function TechnicalSegmentNode({ data }: NodeProps<GraphNodeData>) {
  return <div className="technical-segment" title="Visual grouping only, not execution evidence. Click to expand without rerunning.">
    <Handle type="target" position={Position.Top} />
    <button type="button" className="nodrag" onClick={() => (data.expand as (() => void) | undefined)?.()}>{String(data.title)}</button>
    <small>{String(data.memberSummary ?? 'Visual grouping — not skipped')}</small>
    <Handle type="source" position={Position.Bottom} />
  </div>;
}
const nodeTypes = { technicalSegment: TechnicalSegmentNode, yawrStep: StepNode, branchMerge: BranchMergeNode, sessionEntry: SessionEntryNode, frameBox: FrameNode };

function InputsForm({
  declarations,
  values,
  disabled,
  onChange,
}: {
  declarations: InputDecl[];
  values: Record<string, string>;
  disabled: boolean;
  onChange(name: string, value: string): void;
}) {
  if (declarations.length === 0) return null;
  return (
    <section className="run-inputs" aria-label="Runbook inputs">
      {declarations.map((declaration) => {
        const value = values[declaration.name] ?? '';
        const label = `${declaration.name}${declaration.required ? ' *' : ''}`;
        return (
          <label key={declaration.name} title={declaration.description}>
            <span>{label}</span>
            {declaration.enum && !declaration.enumRedacted ? (
              <select
                value={value}
                disabled={disabled}
                required={declaration.required}
                onChange={(event) => onChange(declaration.name, event.target.value)}
              >
                <option value="">{declaration.required ? 'Select...' : 'Unset'}</option>
                {declaration.enum.map((member) => <option key={member} value={member}>{member}</option>)}
              </select>
            ) : (
              <input
                type={declaration.type === 'secret' ? 'password' : 'text'}
                autoComplete={declaration.type === 'secret' ? 'off' : undefined}
                value={value}
                disabled={disabled}
                required={declaration.required}
                placeholder={declaration.enumRedacted && declaration.enumMemberCount
                  ? `One of ${declaration.enumMemberCount} permitted values`
                  : declaration.description}
                onChange={(event) => onChange(declaration.name, event.target.value)}
              />
            )}
          </label>
        );
      })}
    </section>
  );
}

function optionText(option: InteractionOption): string {
  return option.label ?? option.display_label ?? option.display_value ?? option.value;
}

const MAX_DEBUG_PATCH_BYTES = 64 * 1024;
const MAX_DEBUG_PATCH_DEPTH = 8;
const MAX_DEBUG_PATCH_NODES = 512;
const MAX_DEBUG_PATCH_STRING_BYTES = 4096;
const MAX_DEBUG_PATCH_KEY_BYTES = 256;
const MAX_DEBUG_PATCH_ARRAY_ITEMS = 64;
const MAX_DEBUG_PATCH_OBJECT_PROPERTIES = 64;

function parseObjectPatch(value: string, label: string): Record<string, unknown> {
  if (new TextEncoder().encode(value).byteLength > MAX_DEBUG_PATCH_BYTES) {
    throw new Error(`${label} exceeds 64 KiB.`);
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(value || '{}');
  } catch {
    throw new Error(`${label} must be valid JSON.`);
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    throw new Error(`${label} must be a JSON object.`);
  }
  validateDebugPatch(parsed, 0, { nodes: 0 }, label);
  return parsed as Record<string, unknown>;
}

function validateDebugPatch(
  value: unknown,
  depth: number,
  state: { nodes: number },
  label: string,
): void {
  if (depth > MAX_DEBUG_PATCH_DEPTH) throw new Error(`${label} exceeds depth ${MAX_DEBUG_PATCH_DEPTH}.`);
  state.nodes += 1;
  if (state.nodes > MAX_DEBUG_PATCH_NODES) throw new Error(`${label} exceeds ${MAX_DEBUG_PATCH_NODES} values.`);
  if (typeof value === 'string') {
    if (new TextEncoder().encode(value).byteLength > MAX_DEBUG_PATCH_STRING_BYTES) {
      throw new Error(`${label} contains a string larger than ${MAX_DEBUG_PATCH_STRING_BYTES} bytes.`);
    }
    return;
  }
  if (Array.isArray(value)) {
    if (value.length > MAX_DEBUG_PATCH_ARRAY_ITEMS) {
      throw new Error(`${label} contains an array larger than ${MAX_DEBUG_PATCH_ARRAY_ITEMS} items.`);
    }
    for (const item of value) validateDebugPatch(item, depth + 1, state, label);
    return;
  }
  if (typeof value === 'object' && value !== null) {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length > MAX_DEBUG_PATCH_OBJECT_PROPERTIES) {
      throw new Error(`${label} contains an object larger than ${MAX_DEBUG_PATCH_OBJECT_PROPERTIES} properties.`);
    }
    for (const [key, item] of entries) {
      if (new TextEncoder().encode(key).byteLength > MAX_DEBUG_PATCH_KEY_BYTES) {
        throw new Error(`${label} contains a key larger than ${MAX_DEBUG_PATCH_KEY_BYTES} bytes.`);
      }
      validateDebugPatch(item, depth + 1, state, label);
    }
  }
}

function DebugInteractionPane({
  interaction,
  onSubmit,
}: {
  interaction: PendingInteraction;
  onSubmit(answer: Record<string, unknown>): void;
}) {
  const debug = interaction.debug;
  const [variablePatch, setVariablePatch] = useState('{}');
  const [outputPatch, setOutputPatch] = useState('{}');
  const [effectiveStatus, setEffectiveStatus] = useState(debug?.actual?.status ?? 'completed');
  const [effectiveError, setEffectiveError] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [validationError, setValidationError] = useState<string>();
  const pauseHeadingRef = useRef<HTMLHeadingElement>(null);
  useEffect(() => { pauseHeadingRef.current?.focus(); }, [interaction.turnID]);
  if (!debug) return <div className="interaction-error">Debug pause payload is missing.</div>;

  const submit = (action: 'continue' | 'stop' | 'step_into' | 'step_over' | 'step_out', apply: boolean) => {
    try {
      let set: Record<string, unknown> | undefined;
      if (apply) {
        const vars = parseObjectPatch(variablePatch, 'Variable patch');
        const outputs = parseObjectPatch(outputPatch, 'Output patch');
        const protectedVariables = new Set(debug.protectedVariables ?? []);
        const protectedEdit = Object.keys(vars).find((name) => protectedVariables.has(name));
        if (protectedEdit) throw new Error(`${protectedEdit} is protected and cannot be overridden.`);
        if (debug.phase === 'before' && Object.keys(outputs).length > 0) {
          throw new Error('Output patches are available only after execution.');
        }
        set = {
          ...(Object.keys(vars).length > 0 ? { vars } : {}),
          ...(debug.phase === 'after' && Object.keys(outputs).length > 0 ? { output_patch: outputs } : {}),
          ...(debug.phase === 'after' && effectiveStatus !== debug.actual?.status ? { status: effectiveStatus } : {}),
          ...(debug.phase === 'after' && effectiveStatus === 'failed' && effectiveError ? { error: effectiveError } : {}),
        };
        if (Object.keys(set).length === 0) set = undefined;
      }
      setValidationError(undefined);
      setSubmitting(true);
      onSubmit({ kind: 'debug_break', action, ...(set ? { set } : {}) });
    } catch (error) {
      setValidationError(error instanceof Error ? error.message : String(error));
    }
  };
  const callPath = [...(debug.callPath ?? []).map((frame) => frame.step_id), interaction.stepID].join(' / ');

  return (
    <section className="interaction-pane debug-break" aria-label="Debug pause">
      <span className="interaction-kind">Debug pause · {debug.phase}</span>
      <h2 ref={pauseHeadingRef} tabIndex={-1}>{interaction.stepID}</h2>
      <div className="debug-breadcrumb">{callPath}</div>
      <dl className="debug-metadata">
        <dt>Invocation</dt><dd>{debug.invocation}</dd>
        <dt>Attempt</dt><dd>{debug.attempt}</dd>
      </dl>
      {debug.watches && debug.watches.length > 0 ? (
        <section className="debug-values" aria-label="Watch expressions">
          <h3>Watches</h3>
          {debug.watches.map((watch) => (
            <div key={watch.expression} className="debug-watch">
              <code>{watch.expression}</code>
              <span>{watch.error ?? JSON.stringify(watch.value)}</span>
            </div>
          ))}
        </section>
      ) : null}
      <details className="debug-values">
        <summary>Runtime variables</summary>
        <pre>{JSON.stringify(debug.variables ?? {}, null, 2)}</pre>
      </details>
      {debug.actual ? (
        <details className="debug-values" open>
          <summary>Actual result · {debug.actual.status}</summary>
          <pre>{JSON.stringify(debug.actual.output ?? {}, null, 2)}</pre>
          {debug.actual.error ? <p>{debug.actual.error}</p> : null}
        </details>
      ) : null}
      <label className="debug-field">
        <span>Variable patch</span>
        <textarea
          value={variablePatch}
          disabled={submitting}
          spellCheck={false}
          onChange={(event) => setVariablePatch(event.target.value)}
        />
      </label>
      {debug.phase === 'after' ? (
        <>
          <label className="debug-field">
            <span>Output patch</span>
            <textarea
              value={outputPatch}
              disabled={submitting || debug.outputProtected}
              spellCheck={false}
              onChange={(event) => setOutputPatch(event.target.value)}
            />
          </label>
          {debug.outputProtected ? <p className="debug-protected">Output is protected and cannot be overridden.</p> : null}
          <label className="debug-field compact">
            <span>Effective status</span>
            <select
              value={effectiveStatus}
              disabled={submitting}
              onChange={(event) => {
                setEffectiveStatus(event.target.value);
                if (event.target.value !== 'failed') setEffectiveError('');
              }}
            >
              <option value="completed">Completed</option>
              <option value="failed">Failed</option>
              <option value="skipped">Skipped</option>
            </select>
          </label>
          {effectiveStatus === 'failed' ? (
            <label className="debug-field compact">
              <span>Effective error</span>
              <input value={effectiveError} disabled={submitting} onChange={(event) => setEffectiveError(event.target.value)} />
            </label>
          ) : null}
        </>
      ) : null}
      {debug.protectedVariables && debug.protectedVariables.length > 0 ? (
        <p className="debug-protected">Protected: {debug.protectedVariables.join(', ')}</p>
      ) : null}
      {validationError ? <div className="interaction-error" role="alert">{validationError}</div> : null}
      <div className="debug-actions">
        <button type="button" disabled={submitting} onClick={() => submit('continue', false)}>Continue unchanged</button>
        <button type="button" className="primary" disabled={submitting} onClick={() => submit('continue', true)}>Apply and continue</button>
        <button type="button" disabled={submitting || !debug.canStepInto} onClick={() => submit('step_into', true)}>Step into</button>
        <button type="button" disabled={submitting} onClick={() => submit('step_over', true)}>Step over</button>
        <button type="button" disabled={submitting} onClick={() => submit('step_out', true)}>Step out</button>
        <button type="button" className="danger" disabled={submitting} onClick={() => submit('stop', false)}>Stop</button>
      </div>
    </section>
  );
}

function ApprovalInteractionPane({
  interaction,
  onSubmit,
}: {
  interaction: PendingInteraction;
  onSubmit(answer: Record<string, unknown>): void;
}) {
  const [approver, setApprover] = useState('');
  const [submitting, setSubmitting] = useState(false);
  return (
    <section className="interaction-pane approval-pane" aria-label="Governance approval">
      <span className="interaction-kind">Approval</span>
      <h2>{interaction.title ?? interaction.stepID}</h2>
      {interaction.prompt ? <p>{interaction.prompt}</p> : null}
      <label className="debug-field compact">
        <span>Approver identity</span>
        <input
          value={approver}
          disabled={submitting}
          required
          autoFocus
          placeholder="name or email"
          onChange={(event) => setApprover(event.target.value)}
        />
      </label>
      <div className="debug-actions">
        <button
          type="button"
          className="primary"
          disabled={submitting || approver.trim() === ''}
          onClick={() => {
            setSubmitting(true);
            onSubmit({ kind: 'approval', approved: true, approver: approver.trim() });
          }}
        >
          Approve
        </button>
        <button
          type="button"
          className="danger"
          disabled={submitting}
          onClick={() => {
            setSubmitting(true);
            onSubmit({ kind: 'approval', approved: false });
          }}
        >
          Deny
        </button>
      </div>
    </section>
  );
}

function InteractionPane({
  interaction,
  onSubmit,
  onConfirmHostAction,
  xtsOpened,
  xtsViewCheck,
  onVerifyXtsView,
}: {
  interaction: PendingInteraction;
  onSubmit(answer: Record<string, unknown>): void;
  onConfirmHostAction(interaction: PendingInteraction): void;
  xtsOpened: boolean;
  xtsViewCheck?: XtsViewCheck;
  onVerifyXtsView(status: 'opened' | 'failed'): void;
}) {
  const [selected, setSelected] = useState<string[]>([]);
  const [values, setValues] = useState<Record<string, unknown>>(() => {
    const initial: Record<string, unknown> = {};
    for (const field of interaction.fields ?? []) {
      if (field.default !== undefined) initial[field.name] = field.default;
    }
    return initial;
  });
  const [submitting, setSubmitting] = useState(false);
  const [verificationSubmitted, setVerificationSubmitted] = useState(false);
  const [validationError, setValidationError] = useState<string>();
  const [collectorReview, setCollectorReview] = useState<Record<string, unknown>>();
  const choiceMax = interaction.kind === 'choice' && interaction.multiple &&
    typeof interaction.max === 'number' && Number.isInteger(interaction.max) && interaction.max >= 0
    ? interaction.max
    : undefined;
  const choiceLimitID = `choice-limit-${interaction.turnID}`;
  const submit = (answer: Record<string, unknown>) => {
    setSubmitting(true);
    onSubmit(answer);
  };

  if (interaction.kind === 'host_action') {
    const isXts = interaction.host_action?.capability === 'xts.open-view';
    if (!isXts) return <div className="interaction-wait" role="status">Opening host view...</div>;
    return (
      <section className="interaction-pane host-action-pane" aria-label={isXts ? 'Open XTS view' : 'Open host view'}>
        <div className="actual-run-stepper" aria-label="Actual run progress"><strong>1 Open XTS</strong><span>2 Answer questions</span><span>3 Review</span></div>
        <span className="interaction-kind">{isXts ? 'Actual run · XTS' : 'Host action'}</span>
        <h2>{interaction.title ?? interaction.stepID}</h2>
        {interaction.prompt ? <p>{interaction.prompt}</p> : null}
        {isXts ? <p>VS Code will switch to XTS. Review the view, then return here to record your findings.</p> : null}
        {xtsViewCheck ? <>
          <p>XTS launch was requested, but readiness is not confirmed. Confirm only after the real view has loaded
            with the requested environment and parameters. Do not confirm a startup, authentication, or loading error.</p>
          <dl>
            <dt>View</dt><dd>{String(interaction.host_action?.request.view_path ?? '')}</dd>
            <dt>Environment</dt><dd>{String(interaction.host_action?.request.environment ?? '')}</dd>
            {Object.entries(recordValue(interaction.host_action?.request.parameters) ?? {}).map(([name, value]) =>
              <React.Fragment key={name}><dt>{name}</dt><dd>{String(value)}</dd></React.Fragment>)}
          </dl>
          <button type="button" className="primary" disabled={verificationSubmitted}
            onClick={() => { setVerificationSubmitted(true); onVerifyXtsView('opened'); }}>XTS view is ready</button>
          <button type="button" className="danger" disabled={verificationSubmitted}
            onClick={() => { setVerificationSubmitted(true); onVerifyXtsView('failed'); }}>XTS failed to open</button>
        </> : <button
          type="button"
          className="primary"
          disabled={submitting}
          onClick={() => {
            setSubmitting(true);
            onConfirmHostAction(interaction);
          }}
        >
          {submitting ? 'Opening XTS...' : <span>Open XTS</span>}
        </button>}
      </section>
    );
  }
  if (interaction.kind === 'approval') {
    return <ApprovalInteractionPane interaction={interaction} onSubmit={onSubmit} />;
  }
  if (interaction.kind === 'debug_break') {
    return <DebugInteractionPane interaction={interaction} onSubmit={onSubmit} />;
  }

  return (
    <section className="interaction-pane" aria-label="Runbook interaction">
      <span className="interaction-kind">{interaction.kind}</span>
      <h2>{interaction.title ?? interaction.stepID}</h2>
      {interaction.prompt ? <p>{interaction.prompt}</p> : null}

      {interaction.kind === 'choice' ? (
        <form onSubmit={(event) => {
          event.preventDefault();
          if (choiceMax !== undefined && selected.length > choiceMax) {
            setValidationError(`Select no more than ${choiceMax} options.`);
            return;
          }
          submit({ kind: 'choice', selected });
        }}>
          <fieldset disabled={submitting}>
            {(interaction.options ?? []).map((option) => {
              const checked = selected.includes(option.value);
              const disabledByMax = choiceMax !== undefined && !checked && selected.length >= choiceMax;
              const disabledTitle = disabledByMax
                ? `Maximum of ${choiceMax} options selected. Deselect one to choose another.`
                : undefined;
              return (
                <label className="interaction-option" key={option.value} aria-disabled={disabledByMax} title={disabledTitle}>
                  <input
                    type={interaction.multiple ? 'checkbox' : 'radio'}
                    name="choice"
                    checked={checked}
                    disabled={disabledByMax}
                    aria-describedby={choiceMax === undefined ? undefined : choiceLimitID}
                    onChange={() => {
                      setValidationError(undefined);
                      setSelected((current) => {
                        if (!interaction.multiple) return [option.value];
                        if (current.includes(option.value)) return current.filter((value) => value !== option.value);
                        if (choiceMax !== undefined && current.length >= choiceMax) return current;
                        return [...current, option.value];
                      });
                    }}
                  />
                  <span><strong>{optionText(option)}</strong>{option.hint ? <small>{option.hint}</small> : null}</span>
                </label>
              );
            })}
          </fieldset>
          {choiceMax !== undefined ? (
            <p id={choiceLimitID} className="choice-limit" role="status">
              {selected.length >= choiceMax
                ? `${selected.length} of ${choiceMax} selected. Deselect an option to choose another.`
                : `${selected.length} of ${choiceMax} selected. Select up to ${choiceMax} options.`}
            </p>
          ) : null}
          {validationError ? <p className="interaction-error" role="alert">{validationError}</p> : null}
          <button
            type="submit"
            disabled={submitting || selected.length < (interaction.min ?? 1) || (choiceMax !== undefined && selected.length > choiceMax)}
          >
            Continue
          </button>
        </form>
      ) : null}

      {interaction.kind === 'decision' ? (
        <div className="decision-options">
          {(interaction.routes ?? []).map((route) => (
            <button
              key={route.label}
              type="button"
              disabled={submitting}
              onClick={() => submit({ kind: 'decision', label: route.label })}
            >
              <strong>{optionText(route)}</strong>
              {route.hint ? <small>{route.hint}</small> : null}
            </button>
          ))}
        </div>
      ) : null}

      {interaction.kind === 'collector' && collectorReview ? (
        <section className="collector-review review-before-submit" aria-label="Review collected answers">
          {xtsOpened ? <div className="actual-run-stepper" aria-label="Actual run progress"><span>1 Open XTS</span><span>2 Answer questions</span><strong>3 Review</strong></div> : null}
          <span className="interaction-kind">Collected in this actual run</span>
          <h3>Review answers</h3>
          <dl>
            {(interaction.fields ?? []).map((field) => (
              <React.Fragment key={field.name}>
                <dt>{field.label ?? field.display_name ?? field.name}</dt>
                <dd>{collectorReviewValue(field, collectorReview[field.name])}</dd>
              </React.Fragment>
            ))}
          </dl>
          <p>Saving submits these answers and resumes the run. The next route may depend on them.</p>
          <div className="debug-actions">
            <button type="button" className="primary" disabled={submitting} onClick={() => submit({ kind: 'collector', values: collectorReview })}>Save answers and continue</button>
            <button type="button" disabled={submitting} onClick={() => setCollectorReview(undefined)}>Edit answers</button>
          </div>
        </section>
      ) : interaction.kind === 'collector' ? (
        <form onSubmit={(event) => {
          event.preventDefault();
          try {
            setValidationError(undefined);
            setCollectorReview(collectorValues(interaction.fields ?? [], values));
          } catch (error) {
            setValidationError(error instanceof Error ? error.message : String(error));
          }
        }}>
          {xtsOpened ? <div className="actual-run-stepper" aria-label="Actual run progress"><span>1 Open XTS</span><strong>2 Answer questions</strong><span>3 Review</span></div> : null}
          {(interaction.fields ?? []).map((field) => (
            <label className="collector-field" data-field-name={field.name} key={field.name}>
              <span>{field.label ?? field.display_name ?? field.name}{field.required ? ' *' : ''}</span>
              {field.type === 'boolean' ? (
                <input
                  type="checkbox"
                  disabled={submitting}
                  checked={Boolean(values[field.name])}
                  onChange={(event) => setValues((current) => ({ ...current, [field.name]: event.target.checked }))}
                />
              ) : field.options && field.options.length > 0 ? (
                <select
                  required={field.required}
                  multiple={field.multiple}
                  disabled={submitting}
                  value={field.multiple
                    ? (Array.isArray(values[field.name]) ? values[field.name] as string[] : [])
                    : String(values[field.name] ?? '')}
                  onChange={(event) => setValues((current) => ({
                    ...current,
                    [field.name]: field.multiple
                      ? Array.from(event.target.selectedOptions).map((option) => option.value)
                      : event.target.value,
                  }))}
                >
                  {!field.multiple ? <option value="">Select...</option> : null}
                  {field.options.map((option) => <option key={option.value} value={option.value}>{optionText(option)}</option>)}
                </select>
              ) : field.type === 'textarea' ? (
                <textarea
                  required={field.required}
                  disabled={submitting}
                  value={String(values[field.name] ?? '')}
                  placeholder={field.hint}
                  rows={3}
                  onChange={(event) => setValues((current) => ({ ...current, [field.name]: event.target.value }))}
                />
              ) : (
                <input
                  type={collectorInputType(field.type)}
                  step={field.type === 'integer' ? 1 : field.type === 'number' ? 'any' : undefined}
                  required={field.required}
                  disabled={submitting}
                  value={String(values[field.name] ?? '')}
                  placeholder={field.hint}
                  onChange={(event) => setValues((current) => ({ ...current, [field.name]: event.target.value }))}
                />
              )}
            </label>
          ))}
          {validationError ? <div className="interaction-error" role="alert">{validationError}</div> : null}
          <button type="submit" disabled={submitting}>Review answers</button>
        </form>
      ) : null}
    </section>
  );
}

interface Dimensions {
  width: number;
  height: number;
}

interface PlacedStep {
  source: GraphNode;
  x: number;
  y: number;
  width: number;
  height: number;
}

interface GroupBounds {
  group: GraphGroup;
  parentGroupID: string;
  depth: number;
  empty: boolean;
  x: number;
  y: number;
  width: number;
  height: number;
}

function nodeDimensions(kind: string, style: NodeStyle): Dimensions {
  if (kind === 'technical-segment') return { width: 200, height: 58 };
  if (kind === 'merge' || kind === 'session-entry') return { width: 14, height: 14 };
  if (style === 'minimalist') {
    if (kind === 'end') return { width: 164, height: 48 };
    if (kind === 'branch' || kind === 'decision' || kind === 'choice') return { width: 190, height: 66 };
    return { width: 184, height: 64 };
  }
  if (style === 'header-badges') {
    if (kind === 'end') return { width: 180, height: 58 };
    return { width: 200, height: 78 };
  }
  if (kind === 'end') return { width: 175, height: 52 };
  if (kind === 'branch' || kind === 'decision' || kind === 'choice') return { width: 210, height: 82 };
  return { width: 196, height: 74 };
}

function groupLabel(group: GraphGroup): string {
  if (group.kind === 'branch-arm' && group.fallback) {
    return group.label ? `Otherwise - ${group.label}` : 'Otherwise';
  }
  if (group.label) return group.label;
  const defaults: Record<string, string> = {
    'branch-arm': 'Branch',
    'compensate-body': 'Compensate',
    'include-frame': 'Include',
    'iterate-body': 'Iterate',
    'parallel-branch': 'Parallel',
  };
  return defaults[group.kind] ?? group.kind;
}

function computeGroupBounds(document: GraphDocument, placed: Map<string, PlacedStep>): Map<string, GroupBounds> {
  const nodeByID = new Map(document.nodes.map((node) => [node.id, node]));
  const parentByGroup = new Map<string, string>();
  for (const group of document.groups) {
    const parentNode = group.parent_node_id ? nodeByID.get(group.parent_node_id) : undefined;
    parentByGroup.set(group.id, String(parentNode?.data.group_id ?? ''));
  }

  const membersByGroup = new Map<string, string[]>();
  for (const node of document.nodes) {
    let groupID = String(node.data.group_id ?? '');
    const visited = new Set<string>();
    while (groupID && !visited.has(groupID)) {
      visited.add(groupID);
      const members = membersByGroup.get(groupID) ?? [];
      members.push(node.id);
      membersByGroup.set(groupID, members);
      groupID = parentByGroup.get(groupID) ?? '';
    }
  }

  const depthOf = (groupID: string) => {
    let depth = 0;
    let parentID = parentByGroup.get(groupID) ?? '';
    const visited = new Set<string>();
    while (parentID && !visited.has(parentID)) {
      visited.add(parentID);
      depth += 1;
      parentID = parentByGroup.get(parentID) ?? '';
    }
    return depth;
  };
  const maxDepth = document.groups.reduce((maximum, group) => Math.max(maximum, depthOf(group.id)), 0);
  const maximumStepY = Math.max(0, ...[...placed.values()].map((step) => step.y + step.height));
  const siblingsByParentNode = new Map<string, GraphGroup[]>();
  for (const group of document.groups) {
    const siblings = siblingsByParentNode.get(group.parent_node_id) ?? [];
    siblings.push(group);
    siblingsByParentNode.set(group.parent_node_id, siblings);
  }
  for (const siblings of siblingsByParentNode.values()) {
    siblings.sort((left, right) => (left.index ?? 0) - (right.index ?? 0) || left.id.localeCompare(right.id));
  }
  const result = new Map<string, GroupBounds>();

  for (const group of document.groups) {
    const memberIDs = membersByGroup.get(group.id) ?? [];
    const memberSteps = memberIDs.map((id) => placed.get(id)).filter((step): step is PlacedStep => step !== undefined);
    const depth = depthOf(group.id);
    if (memberSteps.length === 0) {
      const width = 176;
      const height = 62;
      const anchor = group.parent_node_id ? placed.get(group.parent_node_id) : undefined;
      const siblings = siblingsByParentNode.get(group.parent_node_id) ?? [group];
      const siblingIndex = Math.max(0, siblings.findIndex((candidate) => candidate.id === group.id));
      const columnOffset = siblingIndex * 18;
      result.set(group.id, {
        group,
        parentGroupID: parentByGroup.get(group.id) ?? '',
        depth,
        empty: true,
        x: anchor
          ? group.fallback
            ? anchor.x - width - 32 - columnOffset
            : anchor.x + anchor.width + 32 + columnOffset
          : siblingIndex * (width + 24),
        y: anchor ? anchor.y + anchor.height + 34 + siblingIndex * 10 : maximumStepY + 36,
        width,
        height,
      });
      continue;
    }
    const inverseDepth = maxDepth - depth;
    const paddingX = 18 + inverseDepth * 10;
    const paddingTop = 28 + inverseDepth * 10;
    const paddingBottom = 20 + inverseDepth * 10;
    const minX = Math.min(...memberSteps.map((step) => step.x));
    const minY = Math.min(...memberSteps.map((step) => step.y));
    const maxX = Math.max(...memberSteps.map((step) => step.x + step.width));
    const maxY = Math.max(...memberSteps.map((step) => step.y + step.height));
    result.set(group.id, {
      group,
      parentGroupID: parentByGroup.get(group.id) ?? '',
      depth,
      empty: false,
      x: minX - paddingX,
      y: minY - paddingTop,
      width: maxX - minX + paddingX * 2,
      height: maxY - minY + paddingTop + paddingBottom,
    });
  }

  for (const child of [...result.values()].sort((left, right) => right.depth - left.depth)) {
    if (!child.parentGroupID) continue;
    const parent = result.get(child.parentGroupID);
    if (!parent) continue;
    const padding = 14;
    const left = Math.min(parent.x, child.x - padding);
    const top = Math.min(parent.y, child.y - padding);
    const right = Math.max(parent.x + parent.width, child.x + child.width + padding);
    const bottom = Math.max(parent.y + parent.height, child.y + child.height + padding);
    parent.x = left;
    parent.y = top;
    parent.width = right - left;
    parent.height = bottom - top;
  }
  return result;
}

function layoutDocument(
  document: GraphDocument,
  style: NodeStyle,
): { nodes: Node<GraphNodeData>[]; edges: Edge<{ graphEdge: GraphEdge }>[] } {
  const graph = new dagre.graphlib.Graph();
  graph.setGraph({ rankdir: 'TB', nodesep: 42, ranksep: 72 });
  graph.setDefaultEdgeLabel(() => ({}));
  for (const node of document.nodes) {
    const dimensions = nodeDimensions(String(node.data.kind ?? ''), style);
    graph.setNode(node.id, dimensions);
  }
  for (const edge of document.edges) {
    graph.setEdge(edge.source, edge.target);
  }
  dagre.layout(graph);

  const placed = new Map<string, PlacedStep>();
  for (const node of document.nodes) {
    const position = graph.node(node.id) as { x?: number; y?: number; width?: number; height?: number } | undefined;
    const dimensions = nodeDimensions(String(node.data.kind ?? ''), style);
    placed.set(node.id, {
      source: node,
      x: (position?.x ?? 0) - dimensions.width / 2,
      y: (position?.y ?? 0) - dimensions.height / 2,
      width: position?.width ?? dimensions.width,
      height: position?.height ?? dimensions.height,
    });
  }
  const groupBounds = computeGroupBounds(document, placed);
  const frameNodes: Node<GraphNodeData>[] = [...groupBounds.values()]
    .sort((left, right) => left.depth - right.depth)
    .map((bounds) => {
      const parentBounds = bounds.parentGroupID ? groupBounds.get(bounds.parentGroupID) : undefined;
      return {
        id: bounds.group.id,
        type: 'frameBox',
        position: {
          x: bounds.x - (parentBounds?.x ?? 0),
          y: bounds.y - (parentBounds?.y ?? 0),
        },
        parentNode: parentBounds?.group.id,
        extent: parentBounds ? 'parent' : undefined,
        className: `${bounds.group.kind}${bounds.empty ? ' empty-group' : ''}`,
        style: { width: bounds.width, height: bounds.height, zIndex: bounds.depth },
        data: {
          id: bounds.group.id,
          kind: bounds.group.kind,
          label: groupLabel(bounds.group),
          frame_id: bounds.group.frame_id,
          parent_group_id: bounds.parentGroupID,
          empty: bounds.empty,
          segment_id: bounds.group.segment_id,
          segment_status: bounds.group.segment_status,
          run_id: bounds.group.run_id,
        },
        selectable: false,
        draggable: false,
        focusable: false,
      };
    });
  const stepNodes: Node<GraphNodeData>[] = document.nodes.map((node) => {
    const position = placed.get(node.id)!;
    const groupID = String(node.data.group_id ?? '');
    const parentBounds = groupID ? groupBounds.get(groupID) : undefined;
    return {
      id: node.id,
      type: node.data.kind === 'technical-segment' ? 'technicalSegment' : node.data.kind === 'session-entry' ? 'sessionEntry' : node.data.synthetic === true ? 'branchMerge' : 'yawrStep',
      position: {
        x: position.x - (parentBounds?.x ?? 0),
        y: position.y - (parentBounds?.y ?? 0),
      },
      parentNode: parentBounds?.group.id,
      extent: parentBounds ? 'parent' : undefined,
      style: { width: position.width, height: position.height, zIndex: 1000 },
      data: {
        ...node.data,
        id: node.data.id ?? node.id,
        graph_parent_node: node.parentNode ?? '',
        graph_extent: node.extent ?? '',
        nodeStyle: style,
      },
      selectable: node.data.synthetic !== true,
      focusable: node.data.synthetic !== true,
    };
  });
  const edgeType = style === 'minimalist' ? 'straight' : style === 'header-badges' ? 'step' : 'smoothstep';
  return {
    nodes: [...frameNodes, ...stepNodes],
    edges: document.edges.map((edge) => ({
      id: edge.id,
      source: edge.source,
      target: edge.target,
      label: edge.label,
      type: edgeType,
      markerEnd: { type: MarkerType.ArrowClosed },
      data: { graphEdge: edge },
    })),
  };
}

function refreshLayoutMetadata(
  layout: ReturnType<typeof layoutDocument>,
  document: GraphDocument,
  style: NodeStyle,
): ReturnType<typeof layoutDocument> {
  const nodeByID = new Map(document.nodes.map((node) => [node.id, node]));
  const groupByID = new Map(document.groups.map((group) => [group.id, group]));
  const edgeByID = new Map(document.edges.map((edge) => [edge.id, edge]));
  return {
    nodes: layout.nodes.map((node) => {
      if (node.type === 'frameBox') {
        const group = groupByID.get(node.id);
        return group ? {
          ...node,
          className: `${group.kind}${node.data.empty ? ' empty-group' : ''}`,
          data: {
            ...node.data,
            kind: group.kind,
            label: groupLabel(group),
            frame_id: group.frame_id,
            segment_id: group.segment_id,
            segment_status: group.segment_status,
            run_id: group.run_id,
            graph_loaded: group.graph_loaded,
          },
        } : node;
      }
      const source = nodeByID.get(node.id);
      return source ? {
        ...node,
        data: {
          ...source.data,
          id: source.data.id ?? source.id,
          graph_parent_node: source.parentNode ?? '',
          graph_extent: source.extent ?? '',
          nodeStyle: style,
        },
      } : node;
    }),
    edges: layout.edges.map((edge) => {
      const source = edgeByID.get(edge.id);
      return source ? { ...edge, label: source.label, data: { graphEdge: source } } : edge;
    }),
  };
}

function runtimeEdgeClass(edge: GraphEdge, runtimeNodes: Readonly<Record<string, RuntimeNodeState>>): string | undefined {
  const state = edgeRuntimeState(edge, runtimeNodes);
  if (state?.status === 'running' || state?.status === 'delaying') return 'edge-running';
  if (state?.status === 'completed') return 'edge-completed';
  if (state?.status === 'failed' || state?.status === 'cancelled' || state?.status === 'indeterminate') return 'edge-failed';
  const owner = edge.runtimeNodeID ? runtimeNodes[edge.runtimeNodeID] : undefined;
  if (edge.routeKind && owner && ['completed', 'failed', 'skipped', 'indeterminate'].includes(owner.status)) {
    return 'edge-route-inactive';
  }
  return undefined;
}

function eventNodeID(event: RuntimeEvent): string | undefined {
  const payload = event.payload ?? {};
  if (typeof payload.graph_node_id === 'string' && payload.graph_node_id) return payload.graph_node_id;
  if (typeof payload.qualified_node_id === 'string' && payload.qualified_node_id) return payload.qualified_node_id;
  const explicitNodeID = payload.node_id;
  if (typeof explicitNodeID === 'string' && explicitNodeID) return explicitNodeID;
  const stepID = payload.step_id;
  if (typeof stepID !== 'string' || !stepID) return undefined;
  const callPath = Array.isArray(payload.call_path)
    ? payload.call_path.filter((frame): frame is DirectDebugCallFrame => (
        typeof frame === 'object' && frame !== null && typeof (frame as { step_id?: unknown }).step_id === 'string'
      ))
    : [];
  const nodeID = debugNodeID(callPath, stepID);
  return typeof nodeID === 'string' && nodeID ? nodeID : undefined;
}

function debugNodeID(callPath: DirectDebugCallFrame[], stepID: string): string {
  if (callPath.length === 0) return stepID;
  const escapePart = (value: string) => value.replaceAll('~', '~0').replaceAll('/', '~1');
  return [...callPath.map((frame) => escapePart(frame.step_id)), escapePart(stepID)].join('/');
}

function recordValue(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined;
}

function appendRuntimeLog(current: RuntimeLogLine[] | undefined, payload: Record<string, unknown>): RuntimeLogLine[] | undefined {
  if (typeof payload.line !== 'string' || payload.line === '') return current;
  const next = [...(current ?? []), {
    stream: typeof payload.stream === 'string' ? payload.stream : 'stdout',
    line: payload.line,
  }];
  let total = 0;
  const kept: RuntimeLogLine[] = [];
  for (let index = next.length - 1; index >= 0 && kept.length < 50; index -= 1) {
    total += next[index].line.length;
    if (total > 16 * 1024) break;
    kept.unshift(next[index]);
  }
  return kept;
}

function applyRuntimeEvent(
  current: Readonly<Record<string, RuntimeNodeState>>,
  event: RuntimeEvent,
  expectedDisplaySnapshot = 'missing-binding',
): Record<string, RuntimeNodeState> {
  const nodeID = eventNodeID(event);
  if (!nodeID) return { ...current };
  let previous = current[nodeID] ?? { status: 'pending' };
  if (event.kind === 'debug/override_applied') {
    return {
      ...current,
      [nodeID]: { ...previous, debugOverride: event.payload ?? {} },
    };
  }
  if (event.kind === 'step/output') {
    const payload = event.payload ?? {};
    return {
      ...current,
      [nodeID]: { ...previous, logs: appendRuntimeLog(previous.logs, payload),
        lastActivityAt: event.timestamp ?? previous.lastActivityAt },
    };
  }
  let status = previous.status;
  if (event.kind === 'step/started' || event.kind === 'step/resumed') status = 'running';
  else if (event.kind === 'step/delaying') status = 'delaying';
  else if (event.kind === 'step/completed') status = 'completed';
  else if (event.kind === 'step/failed') status = 'failed';
  else if (event.kind === 'step/indeterminate') status = 'indeterminate';
  else if (event.kind === 'step/skipped') status = 'skipped';
  else if (event.kind === 'step/cancelled') status = 'cancelled';
  else if (event.kind === 'step/denied') status = 'denied';
  else if (event.kind === 'step/blocked') status = 'blocked';
  else return current as Record<string, RuntimeNodeState>;
  const payload = event.payload ?? {};
  const counter = (value: unknown, minimum = 1) =>
    typeof value === 'number' && Number.isSafeInteger(value) && value >= minimum ? value : undefined;
  const invocation = counter(payload.invocation);
  const retryAttempt = counter(payload.retry_attempt === undefined ? payload.attempt : payload.retry_attempt);
  const occurrenceID = directOccurrenceID(event.run_id, nodeID, payload) +
    (validProgressIdentity(payload) ? '' : `:uncertain:${event.sequence}`);
  const existingOccurrence = previous.occurrences?.find(item => item.occurrenceID === occurrenceID);
  const directDisplaySnapshot = existingOccurrence?.directDisplaySnapshot ?? expectedDisplaySnapshot;
  if (['step/started', 'step/resumed', 'step/delaying'].includes(event.kind) && !existingOccurrence && validProgressIdentity(payload)) {
    previous = beginDirectDisplayOccurrence(previous, payload, event.run_id, directDisplaySnapshot) as RuntimeNodeState;
  }
  if (event.run_id && existingOccurrence && isSettledStepStatus(existingOccurrence.status) &&
      ['step/started', 'step/resumed', 'step/delaying'].includes(event.kind)) return current as Record<string, RuntimeNodeState>;
  const retainedSameOccurrence = event.kind === 'step/started'
    ? previous.occurrences?.find(item => item.occurrenceID === occurrenceID &&
      (item.displayPresentation || item.displayPresentationDiagnostic)) : undefined;
  const presentation = event.kind === 'step/completed' || event.kind === 'step/failed'
    ? terminalPresentation(payload, existingOccurrence, directDisplaySnapshot) : undefined;
  if (existingOccurrence && isSettledStepStatus(existingOccurrence.status) &&
      (existingOccurrence.displayPresentation || existingOccurrence.displayPresentationDiagnostic ||
        presentation?.displayPresentation || presentation?.displayPresentationDiagnostic) &&
      (event.kind === 'step/completed' || event.kind === 'step/failed')) {
    const update = { output: recordValue(payload.output), displayPresentation: presentation?.displayPresentation,
      displayPresentationDiagnostic: presentation?.displayPresentationDiagnostic };
    return { ...current, [nodeID]: reconcileDirectDisplayState({
      ...previous, ...(previous.occurrenceID === occurrenceID ? update : {}),
      occurrences: previous.occurrences!.map(item => item.occurrenceID === occurrenceID ? { ...item, ...update } : item),
    }, previous) as RuntimeNodeState };
  }
  const value: RuntimeNodeState = {
      ...previous,
      directDisplaySnapshot,
      status,
      ...(event.kind === 'step/started' && !existingOccurrence ? {
        error: undefined, durationMs: undefined, startedAt: undefined, finishedAt: undefined,
      } : {}),
      lastActivityAt: event.timestamp ?? existingOccurrence?.lastActivityAt,
      stepKind: producerStepKind(payload) ?? previous.stepKind,
      error: typeof payload.error === 'string' ? payload.error
        : event.kind === 'step/started' && !existingOccurrence ? undefined : previous.error,
      durationMs: typeof payload.duration_ms === 'number' ? payload.duration_ms
        : event.kind === 'step/started' && !existingOccurrence ? undefined : previous.durationMs,
      attempt: typeof payload.attempt === 'number'
        ? payload.attempt
        : typeof payload.attempt_number === 'number'
          ? payload.attempt_number
          : previous.attempt,
      delay: typeof payload.delay === 'string' ? payload.delay : previous.delay,
      skipReason: typeof payload.reason === 'string' ? payload.reason : previous.skipReason,
      output: event.kind === 'step/completed' || event.kind === 'step/failed'
        ? recordValue(payload.output) : previous.output,
      ...((event.kind === 'step/completed' || event.kind === 'step/failed') ? {
        codePresentation: undefined, outputValueStatus: undefined, presentationDiagnostic: undefined,
        displayPresentation: undefined, displayPresentationDiagnostic: undefined, ...presentation,
      } : {}),
      ...(event.kind === 'step/started' ? { output: undefined, codePresentation: undefined, outputValueStatus: undefined, presentationDiagnostic: undefined,
        displayPresentation: undefined, displayPresentationDiagnostic: undefined } : {}),
      ...(retainedSameOccurrence ? {
        output: retainedSameOccurrence.output, displayPresentation: retainedSameOccurrence.displayPresentation,
        displayPresentationDiagnostic: retainedSameOccurrence.displayPresentationDiagnostic,
      } : {}),
      captures: recordValue(payload.captures) ?? previous.captures,
      evidence: payload.evidence ?? previous.evidence,
      ...(event.kind === 'step/started' && event.timestamp ? { startedAt: event.timestamp } : {}),
      ...((event.kind === 'step/completed' || event.kind === 'step/failed' || event.kind === 'step/indeterminate' || event.kind === 'step/skipped') && event.timestamp
        ? { finishedAt: event.timestamp }
        : {}),
  };
  const { occurrences: _history, retainedPresentations: _retained, displayObservations: _displayHistory, ...observation } = value;
  const occurrence = { ...observation, occurrenceID, runID: event.run_id, segmentID: '',
    qualifiedNodeID: typeof payload.qualified_node_id === 'string' ? payload.qualified_node_id : nodeID,
    phase: typeof payload.phase === 'string' ? payload.phase : undefined, invocation, retryAttempt,
    frameID: typeof payload.frame_id === 'string' ? payload.frame_id : undefined,
    frameStepIndex: counter(payload.frame_step_index, 0),
    dispatchOccurrenceID: typeof payload.dispatch_occurrence_id === 'string' ? payload.dispatch_occurrence_id : undefined,
    executionLane: typeof payload.execution_lane === 'string' ? payload.execution_lane : undefined,
    startedEventSequence: existingOccurrence?.startedEventSequence ??
      (event.kind === 'step/started' || event.kind === 'step/resumed' ? event.sequence : undefined),
    occurrenceSequence: counter(payload.occurrence_sequence) ?? existingOccurrence?.occurrenceSequence,
    executionSource: 'live' as const };
  const history = [...(previous.occurrences ?? [])];
  const index = history.findIndex(item => item.occurrenceID === occurrenceID);
  if (index < 0) history.push(occurrence);
  else if (!(event.kind === 'step/started' && ['completed', 'failed', 'denied', 'indeterminate', 'cancelled', 'blocked', 'skipped'].includes(history[index].status))) {
    history[index] = occurrence;
  }
  const latest = history.filter(item => item.runID === event.run_id)
    .reduce((left, right) => compareOccurrences(left, right) > 0 ? left : right);
  return { ...current, [nodeID]: reconcileDirectDisplayState({
    ...(latest.occurrenceID === occurrenceID ? value : previous),
    runID: latest.runID, occurrenceID: latest.occurrenceID, occurrences: history,
  }, previous) as RuntimeNodeState };
}

function applyTerminalSteps(
  current: Readonly<Record<string, RuntimeNodeState>>,
  steps: StdioFrame['steps'],
  expectedDisplaySnapshot = 'missing-binding',
  runID?: string,
): Record<string, RuntimeNodeState> {
  if (!steps) return { ...current };
  const next = { ...current };
  for (const step of steps) {
    const nodeID = step.node_id || step.step_id;
    if (!nodeID || !step.status) continue;
    const prior = next[nodeID];
    const display = applyDirectDisplaySummary(prior ?? {}, step as unknown as Record<string, unknown>, nodeID,
      runID ?? prior?.runID ?? '', expectedDisplaySnapshot);
    const frozenOutcome = prior && prior.runID === display.runID &&
      prior.directDisplaySnapshot === display.directDisplaySnapshot &&
      isSettledStepStatus(prior.status) && display.displayObservations?.[nodeID]?.length;
    next[nodeID] = {
      ...display,
      status: frozenOutcome ? prior.status : step.status,
      error: frozenOutcome ? prior.error : step.error || prior?.error,
      durationMs: frozenOutcome ? prior.durationMs : step.duration_ms ?? prior?.durationMs,
      // Summary previews have no slot classifications; keep the terminal event's
      // approved values and frozen metadata together instead of replacing them.
      output: display.displayPresentation || display.displayPresentationDiagnostic ? display.output :
        prior?.codePresentation || prior?.presentationDiagnostic ? prior.output : step.output ?? prior?.output,
    } as RuntimeNodeState;
  }
  return next;
}

function graphInputDeclarations(document: GraphDocument): InputDecl[] {
  if (!Array.isArray(document.inputs)) return [];
  return document.inputs.filter((value): value is InputDecl => (
    typeof value === 'object'
    && value !== null
    && !Array.isArray(value)
    && typeof (value as { name?: unknown }).name === 'string'
  ));
}

function debugTargetForNode(node: GraphNode): Omit<DebugBreakpointView, 'phase'> {
  const callPath = Array.isArray(node.data.call_path)
    ? node.data.call_path
        .filter((stepID): stepID is string => typeof stepID === 'string' && stepID.length > 0)
        .map((stepID) => ({ step_id: stepID }))
    : [];
  const step = typeof node.data.step_id === 'string' && node.data.step_id
    ? node.data.step_id
    : node.id;
  return { nodeID: node.id, step, ...(callPath.length > 0 ? { callPath } : {}) };
}

function debugTargetSupported(document: GraphDocument, node: GraphNode): boolean {
  const unsupportedKinds = new Set(['parallel', 'compensate', 'wait_for_event']);
  if (unsupportedKinds.has(String(node.data.kind ?? ''))) return false;
  const nodeByID = new Map(document.nodes.map((candidate) => [candidate.id, candidate]));
  const groupByID = new Map(document.groups.map((group) => [group.id, group]));
  let groupID = typeof node.data.group_id === 'string' ? node.data.group_id : '';
  const visited = new Set<string>();
  while (groupID && !visited.has(groupID)) {
    visited.add(groupID);
    const group = groupByID.get(groupID);
    if (!group) break;
    const parent = nodeByID.get(group.parent_node_id);
    if (!parent) break;
    const kind = String(parent.data.kind ?? '');
    if (kind === 'parallel' || kind === 'compensate') return false;
    if (kind === 'iterate' && parent.data.concurrent === true) return false;
    groupID = typeof parent.data.group_id === 'string' ? parent.data.group_id : '';
  }
  return true;
}

function toggleBreakpoint(
  current: DebugBreakpointView[],
  document: GraphDocument,
  nodeID: string,
  phase: DirectDebugPhase,
): DebugBreakpointView[] {
  const node = document.nodes.find((candidate) => candidate.id === nodeID);
  if (!node || !debugTargetSupported(document, node)) return current;
  const target = debugTargetForNode(node);
  const existing = current.findIndex((breakpoint) => breakpoint.nodeID === nodeID && breakpoint.phase === phase);
  if (existing >= 0) return current.filter((_, index) => index !== existing);
  return [...current, { ...target, phase }];
}

function DebugSelectionControls({
  document,
  node,
  breakpoints,
  watches,
  disabled,
  onToggle,
  onWatchesChange,
}: {
  document: GraphDocument;
  node: GraphNode;
  breakpoints: DebugBreakpointView[];
  watches: string;
  disabled: boolean;
  onToggle(nodeID: string, phase: DirectDebugPhase): void;
  onWatchesChange(value: string): void;
}) {
  const supported = debugTargetSupported(document, node);
  const hasBefore = breakpoints.some((breakpoint) => breakpoint.nodeID === node.id && breakpoint.phase === 'before');
  const hasAfter = breakpoints.some((breakpoint) => breakpoint.nodeID === node.id && breakpoint.phase === 'after');
  return (
    <section className="debug-controls" aria-label="Debugger settings">
      <h3><Bug aria-hidden="true" />Debugger</h3>
      {supported ? (
        <div className="breakpoint-options">
          <label>
            <input type="checkbox" checked={hasBefore} disabled={disabled} onChange={() => onToggle(node.id, 'before')} />
            <span>Pause before execution</span>
          </label>
          <label>
            <input type="checkbox" checked={hasAfter} disabled={disabled} onChange={() => onToggle(node.id, 'after')} />
            <span>Pause after execution</span>
          </label>
        </div>
      ) : <p className="debug-protected">Breakpoints are unavailable for this concurrent or structural step.</p>}
      <label className="debug-field">
        <span>Watch expressions</span>
        <textarea
          value={watches}
          disabled={disabled}
          spellCheck={false}
          placeholder="One expression per line"
          onChange={(event) => onWatchesChange(event.target.value)}
        />
      </label>
    </section>
  );
}

function GraphView({
  document,
  results,
  testMode,
  style,
  runtimeNodes: observedRuntimeNodes,
  executionNodeID,
  visualStep,
  breakpoints,
  watches,
  pending,
  sessionID,
  sessionStatus,
  sessionAttached,
  segmentGraphRevisions,
  unloadedSegmentIDs,
  revisionNodes,
  runID,
  runStatus,
  runStarting,
  reloading,
  runError,
  runDiagnostics,
  inputValues,
  onInputChange,
  onRun,
  onStartSession,
  onResumeSession,
  onCloseSession,
  onRequestGraphRevision,
  onDebugRun,
  onReset,
  onCancel,
  onSubmitInteraction,
  onConfirmHostAction,
  onToggleBreakpoint,
  onWatchesChange,
  onStyleChange,
  routeTestContext,
  routeTests,
  routeTestOutcome,
  routeTestRunning,
  routeTestError,
  onSaveRouteTest,
  onRunRouteTest,
  xtsOpened,
  xtsViewCheck,
  onVerifyXtsView,
}: {
  document: GraphDocument;
  results?: ResultsAvailability;
  testMode: boolean;
  style: NodeStyle;
  runtimeNodes: Readonly<Record<string, RuntimeNodeState>>;
  executionNodeID?: string;
  visualStep?: VisualStep;
  breakpoints: DebugBreakpointView[];
  watches: string;
  pending?: PendingInteraction;
  sessionID?: string;
  sessionStatus?: string;
  sessionAttached: boolean;
  segmentGraphRevisions: Readonly<Record<string, readonly number[]>>;
  unloadedSegmentIDs: readonly string[];
  revisionNodes: Readonly<Record<string, GraphNode>>;
  runID?: string;
  runStatus: string;
  runStarting: boolean;
  reloading: boolean;
  runError?: string;
  runDiagnostics: string;
  inputValues: Record<string, string>;
  onInputChange(name: string, value: string): void;
  onRun(): void;
  onStartSession(): void;
  onResumeSession(): void;
  onCloseSession(status: 'resolved' | 'escalated' | 'cancelled' | 'abandoned'): void;
  onRequestGraphRevision(requestID: string, segmentID: string, revision: number, originalNodeID: string): void;
  onDebugRun(): void;
  onReset(): void;
  onCancel(): void;
  onSubmitInteraction(answer: Record<string, unknown>): void;
  onConfirmHostAction(interaction: PendingInteraction): void;
  onToggleBreakpoint(nodeID: string, phase: DirectDebugPhase): void;
  onWatchesChange(value: string): void;
  onStyleChange(style: NodeStyle): void;
  routeTestContext?: { runbook: string; planHash: string };
  routeTests: SavedRouteTestView[];
  routeTestOutcome?: RouteTestOutcome;
  routeTestRunning: boolean;
  routeTestError?: string;
  onSaveRouteTest(artifact: RouteTestArtifact): void;
  onRunRouteTest(artifact: RouteTestArtifact): void;
  xtsOpened: boolean;
  xtsViewCheck?: XtsViewCheck;
  onVerifyXtsView(status: 'opened' | 'failed'): void;
}) {
  const runtimeNodes = useMemo(() => displayRuntimeStatuses(observedRuntimeNodes, runStatus, document), [observedRuntimeNodes, runStatus, document]);
  const [selectedId, setSelectedId] = useState<string>();
  useEffect(() => {
    const report = (event: ErrorEvent) => {
      vscode.postMessage({ type: 'render.telemetry.error', schema: RENDER_TELEMETRY_SCHEMA });
      if (testMode) vscode.postMessage({ type: 'test.error', message: event.message, stack: event.error?.stack });
    };
    window.addEventListener('error', report);
    return () => window.removeEventListener('error', report);
  }, [testMode]);
  const [viewPreference, setViewPreference] = useState(() =>
    decodeWorkflowPreference(recordValue(vscode.getState?.())?.workflowView));
  const effectiveWorkflowMode = executionViewMode(viewPreference.workflowMode, runStatus);
  const executionLayoutLocked = executionViewMode('workflow', runStatus) === 'all';
  const [expandedTechnicalIDs, setExpandedTechnicalIDs] = useState<ReadonlySet<string>>(new Set());
  const [issueSelection, setIssueSelection] = useState<WorkflowIssue>();
  const [issueNotice, setIssueNotice] = useState(false);
  const issues = useMemo(() => workflowIssueIndex(runtimeNodes), [runtimeNodes]);
  const changePreference = (patch: Partial<typeof viewPreference>) => {
    const preference = decodeWorkflowPreference({ ...viewPreference, ...patch });
    setViewPreference(preference);
    vscode.setState?.(mergeWorkflowPreference(vscode.getState?.(), preference));
  };
  useEffect(() => {
    const canonicalIDs = new Set(document.nodes.map(node => node.id));
    setExpandedTechnicalIDs(current => new Set([...current].filter(id => canonicalIDs.has(id))));
    setSelectedId(current => current && canonicalIDs.has(current) ? current : undefined);
  }, [document.hash, document.runbook.path, sessionID]);
  const [showPanel, setShowPanel] = useState(true);
  const visualPlayback = !!visualStep && (!isExecutionEnded(runStatus) || ['completed', 'resolved'].includes(runStatus));
  useEffect(() => {
    if (results?.state === 'available' && runStatus === 'completed' && !visualPlayback) {
      const origin = results.publication.origin.node_id;
      if (document.nodes.some(node => node.id === origin && node.data.kind === 'results')) {
        setSelectedId(origin); setShowPanel(true);
      }
    }
  }, [results, runStatus, visualPlayback]);
  const [inspectorRatio, setInspectorRatio] = useState(restoredInspectorRatio);
  const [routeTargetID, setRouteTargetID] = useState<string>();
  const [routeScope, setRouteScope] = useState<RouteProjectionScope>('through');
  const [routeTestEditor, setRouteTestEditor] = useState<{ artifact?: RouteTestArtifact; needsReview?: boolean; key: string; contextKey: string }>();
  const [locateNodeID, setLocateNodeID] = useState<string>();
  const [activityLocationNotice, setActivityLocationNotice] = useState<string>();
  const activities = useMemo(() => currentActivities(document, observedRuntimeNodes, runStatus, runID, pending),
    [document, observedRuntimeNodes, runStatus, runID, pending]);
  const history = useMemo(() => executionHistory(document, observedRuntimeNodes), [document, observedRuntimeNodes]);
  const previousExecutionRef = useRef<{ scope: string | GraphDocument; nodeID?: string }>();
  const executionScope = sessionID ?? document.runbook.path ?? document.hash ?? document;
  const liveCurrentNodeID = currentExecutionNode(activities, runStatus,
    new Set(document.nodes.filter(node => node.data.synthetic !== true).map(node => node.id)),
    graphExecutionNodeID(document, executionNodeID),
    previousExecutionRef.current?.scope === executionScope ? previousExecutionRef.current?.nodeID : undefined,
    pending?.nodeID ?? pending?.stepID);
  const currentNodeID = visualPlayback && visualStep
    ? graphExecutionNodeID(document, visualStep.nodeID)
    : isExecutionEnded(runStatus) ? liveCurrentNodeID : undefined;
  useLayoutEffect(() => {
    previousExecutionRef.current = { scope: executionScope, nodeID: liveCurrentNodeID };
  }, [executionScope, liveCurrentNodeID]);
  const progressCounts = useMemo(() => canonicalProgress(document, observedRuntimeNodes, runStatus), [document, observedRuntimeNodes, runStatus]);
  const [closeStatus, setCloseStatus] = useState<'resolved' | 'escalated' | 'cancelled' | 'abandoned'>('resolved');
  const [inspectionRevision, setInspectionRevision] = useState<{ nodeID: string; revision: number }>();
  const flowRef = useRef<ReactFlowInstance<GraphNodeData>>();
  const [flowReady, setFlowReady] = useState(false);
  const stopExecutionPanRef = useRef<() => void>();
  const canvasRef = useRef<HTMLElement>(null);
  const workspaceRef = useRef<HTMLDivElement>(null);
  const resizePointerIDRef = useRef<number>();
  const restoreViewportRef = useRef<Viewport>();
  const declarations = useMemo(() => graphInputDeclarations(document), [document]);
  const structuralDocument = useMemo(() => withBranchMerges(document), [document]);
  const routeSourceDocument = useMemo(
    () => sessionID ? sessionRouteDocument(structuralDocument, runtimeNodes, routeTargetID) : structuralDocument,
    [routeTargetID, runtimeNodes, sessionID, structuralDocument],
  );
  const routeIndex = useMemo(() => buildRouteProjectionIndex(routeSourceDocument), [routeSourceDocument]);
  const routeProjection = useMemo(
    () => routeTargetID
      ? sessionID
        ? computeSessionRouteProjection(routeSourceDocument, routeTargetID, routeScope, routeIndex)
        : computeRouteProjection(routeSourceDocument, routeTargetID, routeScope, routeIndex)
      : undefined,
    [routeIndex, routeScope, routeSourceDocument, routeTargetID, sessionID],
  );
  const prerequisiteProjection = useMemo(
    () => routeTargetID
      ? sessionID
        ? computeSessionRouteProjection(routeSourceDocument, routeTargetID, 'to', routeIndex)
        : computeRouteProjection(routeSourceDocument, routeTargetID, 'to', routeIndex)
      : undefined,
    [routeIndex, routeSourceDocument, routeTargetID, sessionID],
  );
  const routeDisplayDocument = useMemo(
    () => routeProjection ? projectRouteDocument(routeSourceDocument, routeProjection) : structuralDocument,
    [routeProjection, routeSourceDocument, structuralDocument],
  );
  const pinnedNodeIDs = useMemo(() => new Set([
    ...(selectedId ? [selectedId] : []), ...(locateNodeID ? [locateNodeID] : []),
    ...(currentNodeID ? [currentNodeID] : []),
    ...activities.filter(value => value.inGraph).map(value => value.nodeID),
    ...(pending?.nodeID ? [pending.nodeID] : []), ...breakpoints.map(value => value.nodeID),
  ]), [selectedId, locateNodeID, currentNodeID, pending?.nodeID, breakpoints, activities]);
  const issueContextDocument = useMemo(() => {
    const structuralIDs = new Set(structuralDocument.nodes.map(node => node.id));
    const canonicalIDs = new Set(document.nodes.map(node => node.id));
    return issues.some(issue => !structuralIDs.has(issue.nodeID) && canonicalIDs.has(issue.nodeID))
      ? document : structuralDocument;
  }, [document, structuralDocument, issues]);
  const workflow = useMemo(() => projectWorkflow(issueContextDocument, runtimeNodes, {
    mode: routeProjection ? 'all' : effectiveWorkflowMode,
    expandedNodeIDs: expandedTechnicalIDs, pinnedNodeIDs,
    collapsedGroupIDs: new Set(),
  }, routeDisplayDocument), [issueContextDocument, runtimeNodes, effectiveWorkflowMode, expandedTechnicalIDs, pinnedNodeIDs, routeDisplayDocument, routeProjection]);
  const displayDocument = workflow.document;
  useEffect(() => {
    if (!routeTargetID) return;
    const visible = new Set(routeDisplayDocument.nodes.map(node => node.id));
    if ([...workflow.forcedNodeIDs].some(id => !visible.has(id))) {
      setRouteTargetID(undefined); setRouteTestEditor(undefined); setIssueNotice(true);
    }
  }, [issues, routeTargetID, routeDisplayDocument, workflow]);
  const layoutTopologyKey = useMemo(() => sessionGraphTopologyKey(displayDocument), [displayDocument]);
  const previousLayout = useRef<{ scope: string; nodes: Node<GraphNodeData>[] }>();
  const layoutScope = `${document.runbook.path}:${runID}:${style}`;
  const layoutGeometry = useMemo(() => {
    const next = layoutDocument(displayDocument, style);
    if (previousLayout.current?.scope === layoutScope && document.nodes.some(node => node.data.execution_occurrence)) {
      next.nodes = anchorExecutionLayout(previousLayout.current.nodes, next.nodes, currentNodeID);
    }
    return next;
  }, [layoutTopologyKey, style]);
  useLayoutEffect(() => { previousLayout.current = { scope: layoutScope, nodes: layoutGeometry.nodes }; }, [layoutGeometry, layoutScope]);
  const layout = useMemo(
    () => refreshLayoutMetadata(layoutGeometry, displayDocument, style),
    [displayDocument, layoutGeometry, style],
  );
  const returnEdges = useMemo(() => executionReturnEdges(document, observedRuntimeNodes), [document, observedRuntimeNodes]);
  const runtimeEdges = useMemo(() => [...layout.edges.map((edge) => ({
    ...edge,
    className: edge.data?.graphEdge
      ? [
          edge.data.graphEdge.type === 'session-transition' ? 'edge-session-transition' : '',
          runtimeEdgeClass(edge.data.graphEdge, runtimeNodes) ?? '',
        ].filter(Boolean).join(' ') || undefined
      : undefined,
  })), ...returnEdges.map(edge => ({
    ...edge, type: 'smoothstep', className: 'edge-execution-return',
    markerEnd: { type: MarkerType.ArrowClosed },
  }))], [layout.edges, runtimeNodes, returnEdges]);
  const focusedNodeID = routeTargetID ?? selectedId;
  const displayNodes = useMemo(() => layout.nodes.map((node) => ({
    ...node,
    ...(workflow.segments.has(node.id) ? { data: {
      ...node.data,
      expand: () => setExpandedTechnicalIDs(current => new Set([...current, ...workflow.segments.get(node.id)!.memberNodeIDs])),
      memberSummary: (() => {
        const counts = new Map<string, number>();
        for (const id of workflow.segments.get(node.id)!.memberNodeIDs) {
          const status = runtimeNodes[id]?.status;
          if (status) counts.set(status, (counts.get(status) ?? 0) + 1);
        }
        return counts.size ? [...counts].map(([status, count]) => `${count} ${status === 'no-final-status' ? 'No final status' : status}`).join(', ') : 'Visual grouping — not skipped';
      })(),
    } } : {}),
    selected: node.id === focusedNodeID,
  })), [focusedNodeID, layout.nodes, workflow, runtimeNodes]);
  const [measuredNodes, setMeasuredNodes] = useState<Node<GraphNodeData>[]>([]);
  const renderNodes = useMemo(() => preserveLayoutMeasurements(displayNodes, measuredNodes), [displayNodes, measuredNodes]);
  const activeNodeIDs = useMemo(() => activeGraphNodeIDs(structuralDocument, runtimeNodes), [structuralDocument, runtimeNodes]);
  const executionNode = currentNodeID
    ? document.nodes.find((node) => node.id === currentNodeID)
    : undefined;
  const resolvedExecutionNodeID = executionNode?.id;
  const executionTerminal = isExecutionEnded(runStatus) && !visualPlayback;
  const executionStatus = executionTerminal ? undefined
    : ['paused', 'paused_at_boundary', 'handoff_pending'].includes(runStatus) || pending?.kind === 'debug_break' ? 'paused'
    : pending ? 'waiting' : undefined;
  const executionProgressing = !executionTerminal && !pending &&
    !['paused', 'paused_at_boundary', 'handoff_pending'].includes(runStatus) &&
    visualStep?.progressing === true;
  const executionPosition = useMemo<ExecutionPosition>(
    () => ({ nodeID: resolvedExecutionNodeID, terminal: executionTerminal, progressing: executionProgressing, status: executionStatus }),
    [executionTerminal, resolvedExecutionNodeID, executionProgressing, executionStatus],
  );
  const breakpointKeys = useMemo(
    () => new Set(breakpoints.map((breakpoint) => breakpointKey(breakpoint.nodeID, breakpoint.phase))),
    [breakpoints],
  );
  const requestedHistoricalNode = issueSelection?.segmentID && issueSelection.graphRevision !== undefined && issueSelection.qualifiedNodeID
    ? revisionNodes[revisionNodeKey(issueSelection.segmentID, issueSelection.graphRevision, issueSelection.qualifiedNodeID)] : undefined;
  const selected = document.nodes.find((node) => node.id === selectedId) ??
    (issueSelection?.nodeID === selectedId ? requestedHistoricalNode : undefined);
  const selectedSegmentID = typeof selected?.data.segment_id === 'string' ? selected.data.segment_id : '';
  const selectedOriginalNodeID = typeof selected?.data.original_node_id === 'string'
    ? selected.data.original_node_id
    : selected?.id ?? '';
  const availableGraphRevisions = selectedSegmentID ? segmentGraphRevisions[selectedSegmentID] ?? [] : [];
  const latestGraphRevision = availableGraphRevisions[availableGraphRevisions.length - 1];
  const selectedGraphRevision = inspectionRevision && inspectionRevision.nodeID === selected?.id
    ? inspectionRevision.revision
    : latestGraphRevision;
  const revisionRequestID = selected && selectedSegmentID && selectedGraphRevision !== undefined
    ? revisionNodeKey(selectedSegmentID, selectedGraphRevision, selectedOriginalNodeID)
    : '';
  const inspectedNode = selectedGraphRevision === latestGraphRevision
    ? selected
    : revisionNodes[revisionRequestID];
  const routeTarget = document.nodes.find((node) => node.id === routeTargetID);
  const routeTargetName = String(routeTarget?.data.title || routeTarget?.data.id || routeTarget?.id || 'selected step');
  const routeTestCandidates = useMemo(() => {
    const kinds = new Set(['cli', 'tool', 'host_action', 'collector', 'choice', 'decision', 'approve']);
    return document.nodes.filter((node) => (
      node.id !== routeTargetID && prerequisiteProjection?.nodeIDs.has(node.id) && kinds.has(String(node.data.kind ?? ''))
    ));
  }, [document, prerequisiteProjection, routeTargetID]);
  const routeTestBlockers = useMemo(() => document.nodes.flatMap((node) => {
    if (node.id === routeTargetID || !prerequisiteProjection?.nodeIDs.has(node.id)) return [];
    const kind = String(node.data.kind ?? '');
    const dynamicInclude = kind === 'include' && node.data.dynamic === true;
    if (!dynamicInclude && !['parallel', 'wait_for_event', 'extension', 'prompt'].includes(kind)) return [];
    return [`${String(node.data.title || node.data.id || node.id)} (${dynamicInclude ? 'dynamic include' : kind})`];
  }), [document, prerequisiteProjection, routeTargetID]);
  const savedRouteTests = routeTarget ? routeTests.filter(({ artifact }) => routeTestMatchesTarget(artifact, routeTarget)) : [];
  const sessionClosed = !!sessionID && isClosedSessionStatus(sessionStatus ?? '');
  const sessionPaused = !!sessionID && !!runID && ['paused', 'paused_at_boundary', 'handoff_pending'].includes(runStatus);
  const runActive = runStarting || (!!runID && !isTerminalRunStatus(runStatus) && !sessionPaused);
  const sessionSegmentCount = document.groups.filter((group) => group.kind === 'session-segment').length;
  const routeActionsDisabled = runActive && !sessionID;
  const canCloseSession = !!sessionID && sessionAttached && !sessionClosed &&
    (!runID || sessionPaused || ['completed', 'failed', 'cancelled', 'indeterminate'].includes(runStatus));
  const routeTestContextKey = `${document.hash}:${routeTestContext?.planHash ?? ''}`;
  const currentRouteTestEditor = routeTestEditor?.contextKey === routeTestContextKey ? routeTestEditor : undefined;
  const routeTestReviewOpen = currentRouteTestEditor !== undefined;
  const navigateIssue = (issue: WorkflowIssue) => {
    setIssueSelection(issue); setSelectedId(issue.nodeID); setShowPanel(true);
    setRouteTargetID(undefined); setRouteTestEditor(undefined); setIssueNotice(true); setLocateNodeID(issue.nodeID);
    const node = document.nodes.find(value => value.id === issue.nodeID);
    const segmentID = issue.segmentID || String(node?.data.segment_id ?? '');
    const originalID = issue.qualifiedNodeID || String(node?.data.original_node_id ?? '');
    if (sessionID && segmentID && issue.graphRevision !== undefined) {
      setInspectionRevision({ nodeID: issue.nodeID, revision: issue.graphRevision });
      if (unloadedSegmentIDs.includes(segmentID)) vscode.postMessage({ type: 'session.load-segment', segmentID, revision: issue.graphRevision });
      onRequestGraphRevision(revisionNodeKey(segmentID, issue.graphRevision, originalID), segmentID, issue.graphRevision, originalID);
    }
  };

  const showRoutesThrough = (nodeID: string) => {
    stopExecutionPanRef.current?.();
    if (!routeTargetID) restoreViewportRef.current = flowRef.current?.getViewport();
    setRouteScope('through');
    setRouteTargetID(nodeID);
    setSelectedId(nodeID);
    const target = document.nodes.find((node) => node.id === nodeID);
    if (sessionID && typeof target?.data.segment_id === 'string') {
      vscode.postMessage({ type: 'session.load-route', targetSegmentID: target.data.segment_id });
    }
  };
  const showFullGraph = () => {
    setSelectedId(routeTargetID);
    setRouteTargetID(undefined);
    setRouteTestEditor(undefined);
  };
  const locateExecutionNode = (activity: CurrentActivity) => {
    stopExecutionPanRef.current?.();
    setActivityLocationNotice(undefined);
    if (!activity.inGraph) {
      if (sessionID && activity.segmentID && activity.graphRevision !== undefined) {
        if (unloadedSegmentIDs.includes(activity.segmentID)) {
          vscode.postMessage({ type: 'session.load-segment', segmentID: activity.segmentID, revision: activity.graphRevision });
        }
        onRequestGraphRevision(revisionNodeKey(activity.segmentID, activity.graphRevision, activity.path),
          activity.segmentID, activity.graphRevision, activity.path);
        setInspectionRevision({ nodeID: activity.nodeID, revision: activity.graphRevision });
        setIssueSelection({ nodeID: activity.nodeID, qualifiedNodeID: activity.path, segmentID: activity.segmentID,
          graphRevision: activity.graphRevision, occurrenceID: activity.occurrenceID, status: activity.status, blockedOutcome: false });
        setSelectedId(activity.nodeID); setShowPanel(true);
        setActivityLocationNotice(`Requested existing segment revision ${activity.graphRevision}: ${activity.path}. The viewport is unchanged until this exact node is available.`);
      } else {
        setActivityLocationNotice(`No graph location is available for ${activity.path}. This is the exact runtime child path; no substitute node was selected.`);
      }
      return;
    }
    setRouteTargetID(undefined);
    setRouteTestEditor(undefined);
    setSelectedId(activity.nodeID);
    setShowPanel(true);
    setLocateNodeID(activity.nodeID);
  };

  const resizeInspectorFromClientX = (clientX: number) => {
    const bounds = workspaceRef.current?.getBoundingClientRect();
    if (!bounds || bounds.width <= 0) return;
    setInspectorRatio(clampInspectorRatio((bounds.right - clientX) / bounds.width));
  };

  const resizeInspectorByKey = (event: React.KeyboardEvent<HTMLDivElement>) => {
    const step = event.shiftKey ? 0.1 : 0.025;
    let next: number | undefined;
    if (event.key === 'ArrowLeft') next = inspectorRatio + step;
    else if (event.key === 'ArrowRight') next = inspectorRatio - step;
    else if (event.key === 'Home') next = MIN_INSPECTOR_RATIO;
    else if (event.key === 'End') next = MAX_INSPECTOR_RATIO;
    if (next === undefined) return;
    event.preventDefault();
    setInspectorRatio(clampInspectorRatio(next));
  };

  useEffect(() => persistInspectorRatio(inspectorRatio), [inspectorRatio]);

  useEffect(() => {
    if (!sessionID || !selectedSegmentID || selected?.data.graph_loaded !== false ||
        !unloadedSegmentIDs.includes(selectedSegmentID)) return;
    const revision = segmentGraphRevisions[selectedSegmentID]?.at(-1);
    if (revision) vscode.postMessage({ type: 'session.load-segment', segmentID: selectedSegmentID, revision });
  }, [selected?.id, selected?.data.graph_loaded, selectedSegmentID, sessionID, segmentGraphRevisions, unloadedSegmentIDs]);

  useEffect(() => {
    if (issueSelection?.nodeID !== selected?.id) setInspectionRevision(undefined);
  }, [selected?.id]);

  useEffect(() => {
    if (pending?.nodeID || pending?.stepID) setSelectedId(pending.nodeID ?? pending.stepID);
  }, [pending?.turnID, pending?.nodeID, pending?.stepID]);

  useEffect(() => {
    if (!testMode) return;
    const receiveTestAction = (event: MessageEvent<HostMessage>) => {
      const message = event.data;
      if (message?.type === 'test.action' && ['select-runbook', 'select-history'].includes(message.action) && message.value) {
        const select = window.document.querySelector<HTMLSelectElement>(message.action === 'select-runbook'
          ? 'select[aria-label="Inspect runbook"]' : 'select[aria-label="Inspect executed step"]');
        if (select) {
          select.value = message.value;
          select.dispatchEvent(new Event('change', { bubbles: true }));
        }
      } else if (message?.type === 'test.action' && message.action === 'select-node' && message.name) {
        const selectedNode = document.nodes.find((node) => node.id === message.name || node.data.id === message.name);
        setSelectedId(selectedNode?.id);
      } else if (message?.type === 'test.action' && message.action === 'inspect-results') {
        vscode.postMessage({
          type: 'typed-results.state',
          selectedID: selectedId, inspectedKind: inspectedNode?.data.kind, availability: results?.state, showPanel,
          state: window.document.querySelector('.typed-results')?.getAttribute('data-results-state'),
          unavailableReason: window.document.querySelector('.typed-results[data-results-state="unavailable"]')?.textContent,
          values: Array.from(window.document.querySelectorAll<HTMLElement>('.typed-results [data-result-name]')).map(value => ({
            name: value.dataset.resultName, text: value.querySelector('pre')?.textContent,
          })),
          forbiddenElements: window.document.querySelectorAll('.typed-results a, .typed-results img, .typed-results script').length,
        });
      } else if (message?.type === 'test.action' && message.action === 'inspect-expressions') {
        vscode.postMessage({
          type: 'inspector.expressions',
          inspectorText: window.document.querySelector('.step-inspector')?.textContent,
          markdownElements: window.document.querySelectorAll('.step-inspector .markdown-body, .step-inspector a[href], .step-inspector script, .step-inspector img').length,
          values: Array.from(window.document.querySelectorAll<HTMLElement>('.step-inspector [data-expression-path]')).map(value => ({
            path: value.dataset.expressionPath, text: value.textContent,
            tokens: Array.from(value.querySelectorAll<HTMLElement>('[data-expression-class]')).map(token => ({
              class: token.dataset.expressionClass, text: token.textContent, color: getComputedStyle(token).color,
            })),
          })),
        });
      }
    };
    window.addEventListener('message', receiveTestAction);
    return () => window.removeEventListener('message', receiveTestAction);
  }, [document.nodes, testMode, displayNodes, displayDocument, workflow, runtimeNodes, viewPreference, selectedId, issues]);

  useEffect(() => {
    if (!testMode || !selected) return;
    const frame = requestAnimationFrame(() => {
      vscode.postMessage({
        type: 'inspector.state',
        nodeID: selected.id,
        tabs: Array.from(window.document.querySelectorAll('.inspector-tabs [role="tab"]')).map((element) => element.textContent?.trim()),
        sections: Array.from(window.document.querySelectorAll('.inspector .section-heading h3')).map((element) => element.textContent?.trim()),
      });
    });
    return () => cancelAnimationFrame(frame);
  }, [selected?.id, testMode]);

  useEffect(() => {
    if (!testMode) return;
    let frame = 0;
    const receive = (event: MessageEvent<HostMessage>) => {
      if (event.data?.type !== 'test.action' || event.data.action !== 'sample-execution-transition') return;
      cancelAnimationFrame(frame);
      const samples: unknown[] = [];
      const includePlayback = event.data.value === 'include-pacing';
      const frameCount = includePlayback ? 1800 : event.data.value === 'pacing' ? 180 : 30;
      let seenCurrent = false;
      let drainedFrames = 0;
      const sample = () => {
        const canvas = canvasRef.current;
        const nodes = Array.from(canvas?.querySelectorAll<HTMLElement>('.react-flow__node-yawrStep') ?? []);
        samples.push({
          at: performance.now(),
          runStatus: window.document.querySelector<HTMLElement>('.app')?.dataset.runStatus,
          resultsState: window.document.querySelector<HTMLElement>('.app')?.dataset.resultsState,
          currentIDs: nodes.filter(node => node.querySelector('.execution-current')).map(node => node.dataset.id),
          selectedIDs: nodes.filter(node => node.querySelector('.selected')).map(node => node.dataset.id),
          progressing: nodes.filter(node => node.querySelector('.execution-progress')).map(node => node.dataset.id),
          reducedMotion: window.matchMedia('(prefers-reduced-motion: reduce)').matches,
          topLogCount: window.document.querySelectorAll('main > .current-activity, .execution-position-strip').length,
          canvas: canvas?.getBoundingClientRect().toJSON(),
          viewport: flowRef.current?.getViewport(),
          positions: flowRef.current?.getNodes().map(node => ({ id: node.id, position: node.position, width: node.width, height: node.height })),
        });
        const hasCurrent = nodes.some(node => node.querySelector('.execution-current'));
        seenCurrent ||= hasCurrent;
        drainedFrames = seenCurrent && !hasCurrent &&
          window.document.querySelector<HTMLElement>('.app')?.dataset.runStatus === 'completed' ? drainedFrames + 1 : 0;
        if (samples.length < frameCount && (!includePlayback || drainedFrames < 10)) frame = requestAnimationFrame(sample);
        else vscode.postMessage({ type: 'execution.transition-samples', samples });
      };
      frame = requestAnimationFrame(sample);
      vscode.postMessage({ type: 'execution.transition-sampling' });
    };
    window.addEventListener('message', receive);
    return () => {
      window.removeEventListener('message', receive);
      cancelAnimationFrame(frame);
    };
  }, [testMode]);

  useEffect(() => {
    if (!testMode) return;
    const receive = (event: MessageEvent<HostMessage>) => {
      const message = event.data;
      if (message?.type !== 'test.action') return;
      if (message.action === 'set-graph-viewport' && message.value) {
        stopExecutionPanRef.current?.();
        void flowRef.current?.setViewport(JSON.parse(message.value));
      }
      if (message.action !== 'inspect-graph-visibility') return;
      const canvas = canvasRef.current?.getBoundingClientRect();
      const rendered = Array.from(window.document.querySelectorAll<HTMLElement>('.react-flow__node')).map(element => {
        const box = element.getBoundingClientRect(), css = getComputedStyle(element);
        return { id: element.dataset.id, box: box.toJSON(), opacity: css.opacity, visibility: css.visibility,
          statusText: element.querySelector('.step-status')?.textContent,
          statusClass: element.querySelector('.step-node')?.className,
          intersects: !!canvas && box.right > canvas.left && box.left < canvas.right &&
            box.bottom > canvas.top && box.top < canvas.bottom && box.width > 0 && box.height > 0 &&
            css.opacity !== '0' && css.visibility !== 'hidden', transform: element.style.transform };
      });
      vscode.postMessage({ type: 'graph.visibility', canonicalIDs: document.nodes.map(node => node.id),
        runbooks: document.frames.map(frame => ({ ...frame, nodeIDs: document.nodes.filter(node => node.data.frame_id === frame.id).map(node => node.id) })),
        executionHistory: history,
        executionReturns: returnEdges,
        projectedIDs: displayDocument.nodes.map(node => node.id), edges: displayDocument.edges.map(edge => ({
          id: edge.id, source: edge.source, target: edge.target, label: edge.label })),
        nodes: rendered, canvas: canvas?.toJSON(), viewport: flowRef.current?.getViewport(),
        positions: flowRef.current?.getNodes().map(node => ({ id: node.id, position: node.position, parent: node.parentNode,
          width: node.width, height: node.height })),
        edgePaths: window.document.querySelectorAll('.react-flow__edge-path').length,
        edgeClasses: Array.from(window.document.querySelectorAll('.react-flow__edge')).map(edge => edge.getAttribute('class')),
        mode: effectiveWorkflowMode, selectedID: selectedId, routeTargetID,
        inspectedOccurrence: window.document.querySelector<HTMLSelectElement>('select[aria-label="Execution occurrence"]')?.value,
        runtimeStatuses: Object.fromEntries(Object.entries(runtimeNodes).map(([id, value]) => [id, value.status])),
        currentNodeID: resolvedExecutionNodeID,
        currentMarkerCount: window.document.querySelectorAll('.step-node.execution-current').length,
        topExecutionLogCount: window.document.querySelectorAll('main > .current-activity, .execution-position-strip').length,
        activity: { text: window.document.querySelector('.current-activity')?.textContent,
          items: activities.map(({ nodeID, path, title, label, container, inGraph, occurrenceID }) =>
            ({ nodeID, path, title, label, container, inGraph, occurrenceID })), counts: progressCounts,
          summary: window.document.querySelector('.workflow-summary')?.textContent,
          overview: window.document.querySelector('.overview-stats')?.textContent },
        issues, runStatus, runStarting, sessionID, topology: layoutTopologyKey });
    };
    window.addEventListener('message', receive);
    return () => window.removeEventListener('message', receive);
  }, [testMode, document, displayDocument, effectiveWorkflowMode, selectedId, routeTargetID,
    issues, runStatus, runStarting, sessionID, layoutTopologyKey, runtimeNodes, activities, progressCounts, resolvedExecutionNodeID]);

  useEffect(() => {
    if (!flowReady || !resolvedExecutionNodeID || executionTerminal || routeTargetID) return;
    let frame = 0;
    let attempts = 0;
    const reveal = () => {
      frame = requestAnimationFrame(() => {
        const flow = flowRef.current;
        const canvas = canvasRef.current;
        const target = Array.from(canvas?.querySelectorAll<HTMLElement>('.react-flow__node') ?? [])
          .find(element => element.dataset.id === resolvedExecutionNodeID);
        if (!flow || !canvas || !target || !flow.getNode(resolvedExecutionNodeID)?.width) {
          if (++attempts < 4) reveal();
          return;
        }
        const viewport = executionViewport(flow.getViewport(), canvas.getBoundingClientRect(), target.getBoundingClientRect());
        if (viewport) stopExecutionPanRef.current = animateExecutionViewport(
          flow.getViewport(), viewport, value => { void flow.setViewport(value, { duration: 0 }); },
          { now: () => performance.now(), requestFrame: callback => window.requestAnimationFrame(callback),
            cancelFrame: frame => window.cancelAnimationFrame(frame) },
          window.matchMedia('(prefers-reduced-motion: reduce)').matches);
      });
    };
    reveal();
    return () => {
      cancelAnimationFrame(frame);
      stopExecutionPanRef.current?.();
    };
  }, [flowReady, resolvedExecutionNodeID, executionTerminal, routeTargetID, layoutTopologyKey]);

  useEffect(() => {
    if (!flowRef.current) return;
    const frame = requestAnimationFrame(() => {
      if (routeTargetID) {
        void flowRef.current?.fitView({ padding: 0.2 });
        return;
      }
      const viewport = restoreViewportRef.current;
      if (viewport) {
        restoreViewportRef.current = undefined;
        void flowRef.current?.setViewport(viewport);
      }
    });
    return () => cancelAnimationFrame(frame);
  }, [layout.edges.length, layout.nodes.length, routeScope, routeTargetID]);

  useEffect(() => {
    if (!locateNodeID || routeTargetID) return;
    let frame = 0;
    let attempts = 0;
    const locate = () => {
      frame = requestAnimationFrame(() => {
        const target = flowRef.current?.getNode(locateNodeID);
        const targetReady = target && typeof target.width === 'number' && target.width > 0 &&
          typeof target.height === 'number' && target.height > 0;
        attempts += 1;
        if (!targetReady && attempts < 4) {
          locate();
          return;
        }
        setLocateNodeID(undefined);
        if (targetReady) flowRef.current?.fitView({ nodes: [target], padding: 1.2, duration: 250, maxZoom: 1.2 });
      });
    };
    locate();
    return () => cancelAnimationFrame(frame);
  }, [locateNodeID, routeTargetID]);

  useEffect(() => {
    if (!routeTargetID) return;
    let frame = 0;
    const refit = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => { void flowRef.current?.fitView({ padding: 0.2 }); });
    };
    const observer = new ResizeObserver(refit);
    if (canvasRef.current) observer.observe(canvasRef.current);
    window.addEventListener('resize', refit);
    return () => {
      observer.disconnect();
      window.removeEventListener('resize', refit);
      cancelAnimationFrame(frame);
    };
  }, [routeTargetID]);

  useEffect(() => {
    let frame = 0;
    let attempts = 0;
    const report = () => {
      frame = requestAnimationFrame(() => {
        const firstStep = window.document.querySelector<HTMLElement>('.step-node');
        const firstEdge = window.document.querySelector<SVGElement>('.react-flow__edge');
        const edgeClassName = firstEdge?.getAttribute('class') ?? '';
        const reactFlowRootCount = window.document.querySelectorAll('.react-flow').length;
        const stepNodes = Array.from(window.document.querySelectorAll<HTMLElement>('.react-flow__node-yawrStep'));
        const nodeCount = stepNodes.length;
        const frameCount = window.document.querySelectorAll('.react-flow__node-frameBox').length;
        const edgeCount = window.document.querySelectorAll('.react-flow__edge').length;
        const expectedNodeCount = layout.nodes.filter(node => node.type === 'yawrStep').length;
        const expectedFrameCount = layout.nodes.filter(node => node.type === 'frameBox').length;
        const rendered = reactFlowRootCount === 1 &&
          nodeCount === expectedNodeCount &&
          frameCount === expectedFrameCount &&
          edgeCount === runtimeEdges.length &&
          (runtimeEdges.length === 0 || edgeClassName.includes('react-flow__edge-'));
        if (++attempts < 120 && !rendered) {
          report();
          return;
        }
        vscode.postMessage({
          type: 'render.telemetry',
          schema: RENDER_TELEMETRY_SCHEMA,
          reactFlowRootCount,
          stepNodeCount: nodeCount,
          nodeIDs: stepNodes.map(node => node.dataset.id).filter((id): id is string => Boolean(id)).sort().slice(0, 256),
          frameCount,
          edgeCount,
        });
        if (!testMode) return;
        vscode.postMessage({
          type: 'rendered',
          nodeCount,
          frameCount,
          edgeCount,
          style,
          nodeBorderRadius: firstStep ? getComputedStyle(firstStep).borderRadius : '',
          edgeClassName,
        });
      });
    };
    report();
    return () => cancelAnimationFrame(frame);
  }, [document, layout.nodes.length, runtimeEdges.length, style, testMode]);

  return (
    <>
    <main className={`app style-${style}${showPanel ? '' : ' panel-hidden'} has-panel-content`}
      data-run-status={runStatus} data-results-state={results?.state}>
      <header className="toolbar">
        <div className="identity">
          <strong>{document.runbook.name ?? document.runbook.id ?? 'Runbook'}</strong>
          <span>{sessionID ? `${sessionSegmentCount} segments | ${document.nodes.length} steps` : `${document.nodes.length} steps`}</span>
        </div>
        <div className="run-actions">
          <div className="workflow-mode" role="group" aria-label="Graph detail">
            {(['workflow', 'all'] as WorkflowMode[]).map(mode => <button type="button" key={mode}
              aria-pressed={effectiveWorkflowMode === mode} disabled={executionLayoutLocked}
              title={executionLayoutLocked ? 'All steps stay expanded for execution and result review. Reset restores your saved view.' : undefined}
              onClick={() => changePreference({ workflowMode: mode })}>
              {mode === 'workflow' ? 'Workflow' : 'All steps'}</button>)}
          </div>
          <span className={`run-status status-${runStatus}`} role="status" aria-live="polite"
            title={isExecutionEnded(runStatus) && visualPlayback ? `Runtime ${runStatus}; ordered visual playback continues.` : undefined}>
            {runStarting ? 'starting' : runStatus}</span>
          {!runActive && !sessionID ? (
            <button
              className="primary"
              type="button"
              disabled={routeTestReviewOpen || reloading}
              title={reloading ? 'Wait for the runbook graph to finish reloading' : undefined}
              onClick={onRun}
            >
              <Play aria-hidden="true" />Run</button>
          ) : null}
          {!runActive && !sessionID ? (
            <button
              className="session-run"
              type="button"
              disabled={routeTestReviewOpen || reloading}
              title="Start a durable investigation session"
              onClick={onStartSession}
            >
              <Workflow aria-hidden="true" /><span>Start session</span>
            </button>
          ) : null}
          {!runActive && !sessionID ? (
            <button
              className="debug-run"
              type="button"
              disabled={routeTestReviewOpen || reloading || breakpoints.length === 0}
              title={reloading
                ? 'Wait for the runbook graph to finish reloading'
                : routeTestReviewOpen
                ? 'Close the route-test review before starting a debug run'
                : breakpoints.length === 0
                  ? 'Add a breakpoint to start a debug run'
                  : 'Start with debugger enabled'}
              onClick={onDebugRun}
            >
              <Bug aria-hidden="true" /><span>Debug Run</span>
            </button>
          ) : null}
          {!runActive && (sessionID
            ? sessionClosed || (!sessionAttached && !runStarting)
            : isTerminalRunStatus(runStatus) || routeTestOutcome !== undefined) ? (
            <button className="reset-run" type="button" onClick={() => {
              setRouteTestEditor(undefined);
              onReset();
            }}>
              <RotateCcw aria-hidden="true" />Reset
            </button>
          ) : null}
          {sessionID && sessionPaused && sessionAttached ? (
            <button className="primary" type="button" onClick={onResumeSession}>
              <Play aria-hidden="true" />Resume
            </button>
          ) : null}
          {canCloseSession ? (
            <div className="session-close-actions">
              <select
                aria-label="Investigation outcome"
                value={closeStatus}
                onChange={(event) => setCloseStatus(event.target.value as typeof closeStatus)}
              >
                <option value="resolved">Resolved</option>
                <option value="escalated">Escalated</option>
                <option value="cancelled">Cancelled</option>
                <option value="abandoned">Abandoned</option>
              </select>
              <button className="primary" type="button" onClick={() => onCloseSession(closeStatus)}>
                <CheckCircle2 aria-hidden="true" />Close
              </button>
            </div>
          ) : null}
          {runActive && runID ? <button className="danger" type="button" onClick={onCancel}><Square aria-hidden="true" />Cancel</button> : null}
          <select
            aria-label="Graph style"
            value={style}
            onChange={(event) => onStyleChange(event.target.value as NodeStyle)}
          >
            <option value="smooth-curves">Smooth</option>
            <option value="minimalist">Minimal</option>
            <option value="header-badges">Headers</option>
          </select>
          <button type="button" className={showPanel ? 'toggle active' : 'toggle'} onClick={() => setShowPanel((value) => !value)}>
            <PanelRight aria-hidden="true" />Panel
          </button>
        </div>
      </header>
      <InputsForm
        declarations={declarations}
        values={inputValues}
        disabled={runActive}
        onChange={onInputChange}
      />
      {runError ? <div className="run-error" role="alert">{runError}</div> : null}
      {issueNotice || issues.length ? <section className="workflow-issues" aria-label="Execution issues">
        <strong>{issues.length ? 'Showing issue context' : 'Showing selected step context'}</strong><span>Structural context, not a claim those alternatives executed.</span>
        {issues.map((issue, index) => <button type="button" key={`${issue.nodeID}:${issue.occurrenceID ?? index}`}
          onClick={() => navigateIssue(issue)} title={issue.nodeID}>
          {issue.status}{issue.blockedOutcome ? ' · blocked outcome' : ''}: {issue.qualifiedNodeID || issue.nodeID}
          {issue.occurrenceID ? ` · occurrence ${index + 1}` : ''}
        </button>)}
        {issueSelection && !selected ? <span role="status">Historical graph unavailable — occurrence remains in the issue index.</span> : null}
      </section> : null}
      <div className="workflow-summary" role="status">
        {progressCounts.total} canonical steps · {progressCounts.completed} done · {progressCounts.issues} issues · {progressCounts.skipped} skipped · {progressCounts.running} running · {progressCounts.remaining} {isExecutionEnded(runStatus) ? 'without final status' : 'remaining'}
        {' · '}{workflow.segments.size} visual technical groups · hidden is not skipped
      </div>
      {routeTargetID && routeProjection ? (
        <section className="route-view-strip" aria-label={`Routes through ${routeTargetName}`}>
          <div className="route-view-copy">
            <strong>Showing routes through: {routeTargetName}</strong>
            <span>
              {routeProjection.predecessorCount} steps lead to it, {routeProjection.successorCount} follow it, {routeProjection.boundaryEdges.length} hidden dependencies
            </span>
          </div>
          <div className="route-view-actions">
            <div className="route-scope" role="radiogroup" aria-label="Visible routes">
              <button type="button" role="radio" aria-checked={routeScope === 'through'} onClick={() => setRouteScope('through')}>Through this step</button>
              <button type="button" role="radio" aria-checked={routeScope === 'to'} onClick={() => setRouteScope('to')}>To this step</button>
              <button type="button" role="radio" aria-checked={routeScope === 'from'} onClick={() => setRouteScope('from')}>From this step</button>
            </div>
            <button type="button" disabled={routeActionsDisabled} onClick={showFullGraph}>Show full graph</button>
          </div>
        </section>
      ) : null}
      {currentRouteTestEditor ? (
        <div className="route-test-global-safety" role="status">
          {routeTestOutcome?.passed
            ? 'Route test completed - external actions were blocked'
            : routeTestRunning
              ? 'Testing route - XTS and external actions are blocked'
              : 'Reviewing route test - protected execution starts only when you run this route test'}
        </div>
      ) : null}
      <div
        ref={workspaceRef}
        className="workspace"
        style={{ '--inspector-width': `${inspectorRatio * 100}%` } as React.CSSProperties}
      >
        <section ref={canvasRef} className="canvas" aria-label="Runbook structure">
          <RuntimeNodesContext.Provider value={runtimeNodes}>
            <ExecutionPositionContext.Provider value={executionPosition}>
              <DebugBreakpointsContext.Provider value={breakpointKeys}>
                <ReactFlowProvider>
                <ReactFlow
                  nodes={renderNodes}
                  onNodesChange={changes => {
                    const measurements = changes.filter(change => change.type === 'dimensions');
                    if (measurements.length) setMeasuredNodes(current =>
                      applyNodeChanges(measurements, preserveLayoutMeasurements(displayNodes, current)));
                  }}
                  edges={runtimeEdges}
                  nodeTypes={nodeTypes}
                  fitView
                  fitViewOptions={{ padding: 0.2 }}
                  minZoom={0.2}
                  maxZoom={1.8}
                  nodesDraggable={false}
                  onInit={(instance) => { flowRef.current = instance; setFlowReady(true); }}
                  onMoveStart={(event) => { if (event) stopExecutionPanRef.current?.(); }}
                  onNodeClick={(_, node) => { if (node.data.synthetic !== true) { setIssueSelection(undefined); setSelectedId(node.id); } }}
                  onPaneClick={() => { setSelectedId(undefined); }}
                >
                  <Background variant={BackgroundVariant.Dots} gap={20} size={1} />
                  <Controls showInteractive={false} />
                  <MiniMap
                    pannable
                    zoomable
                    ariaLabel="Runbook overview"
                    nodeColor={(node) => node.id === resolvedExecutionNodeID
                      ? executionTerminal ? 'var(--vscode-charts-blue)' : 'var(--vscode-charts-green)'
                      : node.selected ? 'var(--vscode-charts-yellow)'
                      : ['running', 'delaying'].includes(runtimeNodes[node.id]?.status ?? '')
                        ? 'var(--vscode-charts-green)'
                        : 'var(--vscode-foreground)'}
                    nodeStrokeColor={(node) => node.selected ? 'var(--vscode-editor-background)' : 'transparent'}
                    nodeStrokeWidth={3}
                  />
                </ReactFlow>
                </ReactFlowProvider>
              </DebugBreakpointsContext.Provider>
            </ExecutionPositionContext.Provider>
          </RuntimeNodesContext.Provider>
        </section>
        {showPanel ? (
          <div
            className="inspector-resizer"
            role="separator"
            aria-label="Resize step details"
            aria-orientation="vertical"
            aria-controls="step-details-panel"
            aria-valuemin={MIN_INSPECTOR_RATIO * 100}
            aria-valuemax={MAX_INSPECTOR_RATIO * 100}
            aria-valuenow={Math.round(inspectorRatio * 100)}
            tabIndex={0}
            title="Drag to resize the details panel"
            onDoubleClick={() => setInspectorRatio(DEFAULT_INSPECTOR_RATIO)}
            onKeyDown={resizeInspectorByKey}
            onPointerDown={(event) => {
              event.preventDefault();
              resizePointerIDRef.current = event.pointerId;
              event.currentTarget.setPointerCapture(event.pointerId);
              resizeInspectorFromClientX(event.clientX);
            }}
            onPointerMove={(event) => {
              if (resizePointerIDRef.current === event.pointerId) resizeInspectorFromClientX(event.clientX);
            }}
            onPointerUp={(event) => {
              if (resizePointerIDRef.current === event.pointerId) resizePointerIDRef.current = undefined;
            }}
            onPointerCancel={(event) => {
              if (resizePointerIDRef.current === event.pointerId) resizePointerIDRef.current = undefined;
            }}
          />
        ) : null}
        <aside id="step-details-panel" className="inspector" aria-label="Step details">
          <ActivityDetails activities={activities} runStatus={runStatus} remaining={progressCounts.remaining}
            onLocate={locateExecutionNode} locationNotice={activityLocationNotice} />
          {document.frames.length > 1 ? (
            <section className="runbook-navigation" aria-label="Runbooks in this run">
              <label>Runbooks in this run
                <select aria-label="Inspect runbook" value="" onChange={event => {
                  const node = document.nodes.find(node => node.data.frame_id === event.target.value);
                  if (node) {
                    setIssueSelection(undefined);
                    setRouteTargetID(undefined); setSelectedId(node.id); setLocateNodeID(node.id); setShowPanel(true);
                  }
                }}>
                  <option value="">Choose a runbook to inspect</option>
                  {document.frames.map((frame, index) => <option key={frame.id} value={frame.id}>
                    {index + 1}. {frame.runbook_id} ({frame.runbook_path.split(/[\\/]/).pop()})
                  </option>)}
                </select>
              </label>
              <label>Execution history
                <select aria-label="Inspect executed step" value="" onChange={event => {
                  const entry = history[Number(event.target.value)];
                  if (entry) {
                    setIssueSelection({ nodeID: entry.nodeID, occurrenceID: entry.occurrenceID,
                      qualifiedNodeID: entry.path, status: entry.status, blockedOutcome: false });
                    setRouteTargetID(undefined); setSelectedId(entry.nodeID); setLocateNodeID(entry.nodeID); setShowPanel(true);
                  }
                }}>
                  <option value="">Choose a preceding step</option>
                  {history.map((entry, index) => <option key={`${entry.nodeID}:${entry.sequence}`} value={index} data-occurrence-id={entry.occurrenceID}>
                    {index + 1}. {entry.path} [{entry.status}]
                  </option>)}
                </select>
              </label>
              <small>{document.frames.find(frame => frame.id === (selected ?? executionNode)?.data.frame_id)?.runbook_path}</small>
            </section>
          ) : null}
          {pending ? (
            <InteractionPane
              key={`${pending.turnID}:${runError ?? ''}`}
              interaction={pending}
              onSubmit={onSubmitInteraction}
              onConfirmHostAction={onConfirmHostAction}
              xtsOpened={xtsOpened}
              xtsViewCheck={xtsViewCheck}
              onVerifyXtsView={onVerifyXtsView}
            />
          ) : currentRouteTestEditor && routeTarget && routeTestContext ? (
            <RouteTestPane
              key={currentRouteTestEditor.key}
              document={document}
              target={routeTarget}
              candidates={routeTestCandidates}
              blockers={routeTestBlockers}
              context={routeTestContext}
              initial={currentRouteTestEditor.artifact}
              needsReview={currentRouteTestEditor.needsReview}
              inputValues={inputValues}
              runStatus={runStatus}
              runStarting={runStarting}
              outcome={routeTestOutcome}
              error={routeTestError}
              onSave={onSaveRouteTest}
              onRun={onRunRouteTest}
              onStop={onCancel}
              onClose={() => setRouteTestEditor(undefined)}
            />
          ) : selected ? (
            <div className="selected-step-panel">
              {!routeTargetID || routeTargetID !== selected.id ? (
                <div className="route-context-action">
                  <button type="button" disabled={routeActionsDisabled} onClick={() => showRoutesThrough(selected.id)}>Show routes through this step</button>
                </div>
              ) : null}
              {routeTargetID === selected.id && routeTestContext ? (
                <section className="route-test-launcher" aria-label="Route tests">
                  <button
                    type="button"
                    className="primary"
                    disabled={runActive}
                    onClick={() => setRouteTestEditor({ key: `new:${selected.id}:${Date.now()}`, contextKey: routeTestContextKey })}
                  >Test reaching this step</button>
                  {savedRouteTests.length > 0 ? (
                    <div className="saved-route-tests">
                      <h3>Saved route tests</h3>
                      {savedRouteTests.map((saved) => (
                        <button
                          type="button"
                          key={saved.artifact.id}
                          disabled={runActive}
                          onClick={() => setRouteTestEditor({
                            artifact: saved.artifact,
                            needsReview: saved.needsReview,
                            key: `saved:${saved.artifact.id}:${Date.now()}`,
                            contextKey: routeTestContextKey,
                          })}
                        >
                          <span>{saved.artifact.name}</span>
                          <small>{saved.needsReview ? 'Needs review' : saved.artifact.last_result?.status ?? 'Draft'}</small>
                        </button>
                      ))}
                    </div>
                  ) : null}
                </section>
              ) : null}
              {inspectedNode?.data.kind === 'results' ? <ResultsViewer results={results} nodeID={inspectedNode.id} /> : null}
              {inspectedNode ? <StepInspector
                node={inspectedNode}
                runtime={runtimeNodes[selected.id]}
                requestedOccurrenceID={issueSelection?.nodeID === selected.id ? issueSelection.occurrenceID : undefined}
                snapshotDigest={typeof inspectedNode.data.display_plan_snapshot_digest === 'string'
                  ? inspectedNode.data.display_plan_snapshot_digest : typeof inspectedNode.data.executable_snapshot_hash === 'string'
                  ? inspectedNode.data.executable_snapshot_hash : document.display_plan_snapshot_digest ??
                    document.execution_plan_hash ?? document.presentation_state?.plan_snapshot_digest ?? 'missing-binding'}
                onOccurrenceChange={(occurrence) => {
                  setIssueSelection(undefined);
                  if (occurrence.graphRevision !== undefined && selectedSegmentID) {
                    setInspectionRevision({ nodeID: selected.id, revision: occurrence.graphRevision });
                    onRequestGraphRevision(revisionNodeKey(selectedSegmentID, occurrence.graphRevision, selectedOriginalNodeID),
                      selectedSegmentID, occurrence.graphRevision, selectedOriginalNodeID);
                  }
                }}
                availableGraphRevisions={availableGraphRevisions}
                selectedGraphRevision={selectedGraphRevision}
                onGraphRevisionChange={(revision) => {
                  setInspectionRevision({ nodeID: selected.id, revision });
                  if (revision !== latestGraphRevision) {
                    onRequestGraphRevision(
                      revisionNodeKey(selectedSegmentID, revision, selectedOriginalNodeID),
                      selectedSegmentID,
                      revision,
                      selectedOriginalNodeID,
                    );
                  }
                }}
                debugControls={(
                  selectedGraphRevision === latestGraphRevision ? <DebugSelectionControls
                    document={document}
                    node={selected}
                    breakpoints={breakpoints}
                    watches={watches}
                    disabled={runActive}
                    onToggle={onToggleBreakpoint}
                    onWatchesChange={onWatchesChange}
                  /> : <p className="debug-protected">Historical graph revisions are read-only.</p>
                )}
              /> : <div className="inspector-blank" role="status">Historical graph unavailable or loading — no current-source substitution.</div>}
            </div>
          ) : (
            <RunOverview
              document={document}
              runtimeNodes={runtimeNodes}
              executionNodeID={resolvedExecutionNodeID}
              runID={runID}
              runStatus={runStarting ? 'starting' : runStatus}
              inputs={declarations.map((declaration) => ({
                name: declaration.name,
                value: inputValues[declaration.name] ?? '',
                secret: declaration.type === 'secret',
              }))}
              breakpointCount={breakpoints.length}
              diagnostics={runDiagnostics}
              activeNodeIDs={activeNodeIDs}
            />
          )}
        </aside>
      </div>
    </main>
    </>
  );
}

function App() {
  const [document, setDocument] = useState<GraphDocument>();
  const [results, setResults] = useState<ResultsAvailability>();
  const [testMode, setTestMode] = useState(false);
  const [style, setStyle] = useState<NodeStyle>('smooth-curves');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>();
  const [runError, setRunError] = useState<string>();
  const [runDiagnostics, setRunDiagnostics] = useState('');
  const [sessionID, setSessionID] = useState<string>();
  const [sessionStatus, setSessionStatus] = useState<string>();
  const [sessionAttached, setSessionAttached] = useState(false);
  const [segmentGraphRevisions, setSegmentGraphRevisions] = useState<Record<string, readonly number[]>>({});
  const [unloadedSegmentIDs, setUnloadedSegmentIDs] = useState<readonly string[]>([]);
  const [revisionNodes, setRevisionNodes] = useState<Record<string, GraphNode>>({});
  const [runID, setRunID] = useState<string>();
  const [runStatus, setRunStatus] = useState('idle');
  const [runStarting, setRunStarting] = useState(false);
  const [reloading, setReloading] = useState(false);
  const [runtimeNodes, setRuntimeNodes] = useState<Record<string, RuntimeNodeState>>({});
  const [executionNodeID, setExecutionNodeID] = useState<string>();
  const [visualStep, setVisualStep] = useState<VisualStep>();
  const visualPacerRef = useRef<VisualStepPacer>();
  useLayoutEffect(() => {
    if (visualStep) visualPacerRef.current?.acknowledge(visualStep);
  }, [visualStep]);
  const sessionRuntimeRef = useRef<SessionGraphViewState['runtimeNodes']>({});
  const settledVisualOccurrencesRef = useRef(new Set<string>());
  const [breakpoints, setBreakpoints] = useState<DebugBreakpointView[]>([]);
  const [watches, setWatches] = useState('');
  const [pending, setPending] = useState<PendingInteraction>();
  const [inputValues, setInputValues] = useState<Record<string, string>>({});
  const [routeTestContext, setRouteTestContext] = useState<{ runbook: string; planHash: string }>();
  const [routeTests, setRouteTests] = useState<SavedRouteTestView[]>([]);
  const [routeTestOutcome, setRouteTestOutcome] = useState<RouteTestOutcome>();
  const [routeTestRunning, setRouteTestRunning] = useState(false);
  const [routeTestError, setRouteTestError] = useState<string>();
  const [xtsOpened, setXtsOpened] = useState(false);
  const [xtsViewCheck, setXtsViewCheck] = useState<XtsViewCheck>();
  const pendingRef = useRef<PendingInteraction>();
  const resolvedTurnsRef = useRef(new Set<string>());
  const runIDRef = useRef<string>();
  const sessionIDRef = useRef<string>();
  const sessionStatusRef = useRef<string>();
  const runFinishedRef = useRef(false);
  const directDocumentRef = useRef<GraphDocument>();
  const sourceDocumentRef = useRef<GraphDocument>();
  const directRunScopeRef = useRef<string>();
  const displayWithdrawalsRef = useRef<DisplayObservations>(
    decodeDisplayWithdrawals(recordValue(vscode.getState?.())?.displayWithdrawals));
  const hostSessionRef = useRef(globalThis.crypto.randomUUID());
  const hostRequestRef = useRef<{
    runID: string;
    turnID: string;
    capability: string;
    correlationID: string;
    requestID: string;
  }>();

  useEffect(() => { pendingRef.current = pending; }, [pending]);
  useEffect(() => { runIDRef.current = runID; }, [runID]);
  useEffect(() => { sessionIDRef.current = sessionID; }, [sessionID]);
  useEffect(() => { sessionStatusRef.current = sessionStatus; }, [sessionStatus]);
  useEffect(() => {
    displayWithdrawalsRef.current = retainDirectDisplayWithdrawals(displayWithdrawalsRef.current, runtimeNodes);
    vscode.setState?.({ ...recordValue(vscode.getState?.()), displayWithdrawals: displayWithdrawalsRef.current });
  }, [runtimeNodes]);

  const clearActiveRun = (preserveVisualPlayback = false) => {
    if (!preserveVisualPlayback) visualPacerRef.current?.bypass();
    hostRequestRef.current = undefined;
    setXtsViewCheck(undefined);
    pendingRef.current = undefined;
    runIDRef.current = undefined;
    setXtsOpened(false);
    setPending(undefined);
    setRunID(undefined);
  };

  useEffect(() => {
    const pacer = new VisualStepPacer(setVisualStep, {
      now: () => performance.now(),
      setTimeout: (callback, delay) => window.setTimeout(callback, delay),
      clearTimeout: timer => window.clearTimeout(timer),
    }, true);
    visualPacerRef.current = pacer;
    let hostVisible = true;
    const visibility = () => pacer.setVisible(hostVisible && !window.document.hidden);
    visibility();
    window.document.addEventListener('visibilitychange', visibility);
    const receive = (event: MessageEvent<HostMessage>) => {
      const message = event.data;
      if (!message || typeof message !== 'object') return;
      if (message.type === 'loading') {
        setError(undefined);
      } else if (message.type === 'preview.visibility') {
        hostVisible = message.visible;
        visibility();
      } else if (message.type === 'graph') {
        const previous = directDocumentRef.current;
        const sameGraph = previous?.hash !== undefined && previous.hash === message.document.hash &&
          previous.runbook.path === message.document.runbook.path && previous.runbook.id === message.document.runbook.id;
        if (!sameGraph) {
          pacer.bypass();
          setResults(undefined);
        }
        directDocumentRef.current = message.document;
        sourceDocumentRef.current = message.document;
        setDocument(message.document);
        if (message.document.presentation_state) {
          const retained = message.document.presentation_state;
          setRuntimeNodes(current => applyDirectRetainedDocument(
            restoreDirectDisplayWithdrawals(current, displayWithdrawalsRef.current, retained.run_id, retained.plan_snapshot_digest),
            message.document) as Record<string, RuntimeNodeState>);
        }
        setRouteTestContext(message.routeTestContext);
        setRouteTests(message.routeTests ?? []);
        setRouteTestOutcome(undefined);
        setRouteTestRunning(false);
        setRouteTestError(undefined);
        setTestMode(message.testMode === true);
        setBreakpoints((current) => current.flatMap((breakpoint) => {
          const node = message.document.nodes.find((candidate) => candidate.id === breakpoint.nodeID);
          if (!node || !debugTargetSupported(message.document, node)) return [];
          return [{ ...debugTargetForNode(node), phase: breakpoint.phase }];
        }));
        setStyle(message.style);
        setLoading(false);
        setError(undefined);
      } else if (message.type === 'highlighting') {
        window.document.body.dataset.highlightingEnabled = String(message.enabled);
      } else if (message.type === 'session.starting' || message.type === 'session.reconnecting') {
        directDocumentRef.current = undefined;
        clearActiveRun();
        pacer.start(message.minimumStepDisplayMs);
        sessionRuntimeRef.current = {};
        setSegmentGraphRevisions({});
        setUnloadedSegmentIDs([]);
        setRevisionNodes({});
        sessionIDRef.current = message.sessionID;
        sessionStatusRef.current = 'active';
        setSessionID(message.sessionID);
        setSessionStatus('active');
        setSessionAttached(false);
        setRunStarting(true);
        setRunStatus(message.type === 'session.starting' ? 'starting' : 'reconnecting');
        setRunError(undefined);
        setRunDiagnostics('');
        setRuntimeNodes({});
        setExecutionNodeID(undefined);
      } else if (message.type === 'session.update') {
        const state = message.state;
        if (message.minimumStepDisplayMs !== undefined) pacer.start(message.minimumStepDisplayMs);
        const urgentNode = Object.entries(state.runtimeNodes).find(([id, value]) =>
          (isIssueStepStatus(value.status) && value.status !== sessionRuntimeRef.current[id]?.status) ||
          (value.output?.outcome_category === 'blocked' && sessionRuntimeRef.current[id]?.output?.outcome_category !== 'blocked'))?.[0];
        sessionRuntimeRef.current = state.runtimeNodes;
        const position = state.pending?.nodeID ?? urgentNode ?? state.executionNodeID;
        const successful = ['completed', 'resolved'].includes(state.runStatus);
        if (!message.live || state.pending || urgentNode || (isExecutionEnded(state.runStatus) && !successful) ||
            ['paused', 'paused_at_boundary', 'handoff_pending'].includes(state.runStatus)) {
          pacer.bypass(!isExecutionEnded(state.runStatus) && position ? {
            nodeID: position, progressing: !state.pending && !urgentNode && state.runStatus === 'running',
          } : undefined);
        } else {
          for (const nodeID of message.liveSteps ?? []) {
            const canonicalID = ordinaryVisualNodeID(state.document, nodeID);
            if (canonicalID) pacer.ordinary(canonicalID);
          }
          if (successful) pacer.complete();
        }
        sessionIDRef.current = state.sessionID;
        sessionStatusRef.current = state.sessionStatus;
        runIDRef.current = state.activeRunID;
        pendingRef.current = state.pending as unknown as PendingInteraction | undefined;
        setSessionID(state.sessionID);
        setSessionStatus(state.sessionStatus);
        setSessionAttached(true);
        setSegmentGraphRevisions(state.segmentGraphRevisions ?? {});
        setUnloadedSegmentIDs(state.unloadedSegmentIDs ?? []);
        if (state.document) setDocument(state.document);
        setStyle(message.style);
        setRouteTestContext(undefined);
        setRouteTests([]);
        setRouteTestOutcome(undefined);
        setRouteTestRunning(false);
        setRouteTestError(undefined);
        setRuntimeNodes(normalizeRuntimeStatuses(state.runtimeNodes as Record<string, RuntimeNodeState>));
        setExecutionNodeID(state.executionNodeID);
        setPending(state.pending as unknown as PendingInteraction | undefined);
        setRunID(state.activeRunID);
        setRunStatus(state.runStatus);
        setRunStarting(false);
        setRunError(undefined);
        setLoading(false);
        setError(undefined);
        runFinishedRef.current = isClosedSessionStatus(state.sessionStatus);
      } else if (message.type === 'session.graph-revision') {
        if (message.node) {
          setRevisionNodes((current) => ({ ...current, [message.requestID]: message.node! }));
        } else if (message.error) {
          setRunError(message.error);
        }
      } else if (message.type === 'session.error') {
        pacer.bypass();
        setRunStarting(false);
        setRunError(message.message);
      } else if (message.type === 'session.stderr') {
        setRunDiagnostics((current) => `${current}${message.text}`.slice(-16 * 1024));
      } else if (message.type === 'session.exit') {
        if (!runFinishedRef.current || message.code !== 0) pacer.bypass();
        setSessionAttached(false);
        setRunStarting(false);
        if (!runFinishedRef.current && !['paused', 'indeterminate', 'failed'].includes(sessionStatusRef.current ?? '')) {
          setRunError((current) => current ?? (message.code === 0
            ? 'The session process exited before detaching.'
            : `The session process exited with code ${message.code ?? 'unknown'}`));
        }
      } else if (message.type === 'graph.reload-state') {
        setReloading(message.active);
      } else if (message.type === 'style') {
        setStyle(message.style);
      } else if (message.type === 'error') {
        pacer.bypass();
        setLoading(false);
        setError(message.message);
      } else if (message.type === 'run.starting') {
        if (sourceDocumentRef.current) {
          directDocumentRef.current = sourceDocumentRef.current;
          setDocument(sourceDocumentRef.current);
        }
        setResults(undefined);
        directRunScopeRef.current = undefined;
        clearActiveRun();
        pacer.start(message.minimumStepDisplayMs);
        settledVisualOccurrencesRef.current.clear();
        resolvedTurnsRef.current.clear();
        runFinishedRef.current = false;
        setRunStarting(true);
        setRouteTestRunning(message.routeTest === true);
        setRunStatus('starting');
        setRunError(undefined);
        setRunDiagnostics('');
        setRuntimeNodes({});
        setExecutionNodeID(undefined);
        if (message.routeTest) {
          setRouteTestOutcome(undefined);
          setRouteTestError(undefined);
        }
      } else if (message.type === 'run.frame') {
        const frame = message.frame;
        if (frame.type === 'run.graph' && frame.document && frame.nodeIDs) {
          if (!directDocumentRef.current || !runIDRef.current || frame.runID !== runIDRef.current || runFinishedRef.current) return;
          try {
            const graph = mergeExecutionGraph(directDocumentRef.current, frame.document, frame.nodeIDs);
            directDocumentRef.current = graph;
            setDocument(graph);
          } catch (error) {
            setRunError(error instanceof Error ? error.message : 'Execution graph could not be updated.');
          }
        } else if (frame.type === 'run.started') {
          if (runFinishedRef.current || !frame.runID || (runIDRef.current && frame.runID !== runIDRef.current)) return;
          runIDRef.current = frame.runID;
          directRunScopeRef.current = frame.runID;
          setRunID(frame.runID);
          setRunStarting(false);
          if (!pendingRef.current) setRunStatus('running');
        } else if (frame.type === 'run.event' && frame.event) {
          if (directRunScopeRef.current && frame.event.run_id !== directRunScopeRef.current) return;
          const graph = directDocumentRef.current;
          const binding = graph?.display_plan_snapshot_digest ?? graph?.execution_plan_hash ??
            graph?.presentation_state?.plan_snapshot_digest ?? 'missing-binding';
          setRuntimeNodes((current) => applyRuntimeEvent(
            restoreDirectDisplayWithdrawals(current, displayWithdrawalsRef.current, frame.event!.run_id, binding),
            frame.event!, binding));
          const visualNodeID = eventNodeID(frame.event);
          const visualOccurrence = visualNodeID && validProgressIdentity(frame.event.payload ?? {})
            ? directOccurrenceID(frame.event.run_id, visualNodeID, frame.event.payload ?? {}) : undefined;
          if (visualOccurrence && frame.event.kind.startsWith('step/') && isSettledStepStatus(frame.event.kind.slice(5))) {
            settledVisualOccurrencesRef.current.add(visualOccurrence);
          }
          if ((frame.event.kind === 'step/started' || frame.event.kind === 'step/resumed') &&
              (!visualOccurrence || !settledVisualOccurrencesRef.current.has(visualOccurrence))) {
            const reachedNodeID = eventNodeID(frame.event);
            if (reachedNodeID && !runFinishedRef.current) {
              const canonicalID = ordinaryVisualNodeID(graph, reachedNodeID);
              if (!pendingRef.current && canonicalID) pacer.ordinary(canonicalID);
              setExecutionNodeID(reachedNodeID);
            }
          }
          if (['step/failed', 'step/cancelled', 'step/blocked', 'step/denied', 'step/indeterminate'].includes(frame.event.kind) ||
              recordValue(frame.event.payload?.output)?.outcome_category === 'blocked') {
            const nodeID = eventNodeID(frame.event);
            pacer.bypass(nodeID ? { nodeID, progressing: false } : undefined);
          }
          if (frame.event.kind === 'run/started' && !pendingRef.current && !runFinishedRef.current) setRunStatus(current => isTerminalRunStatus(current) ? current : 'running');
          else if (frame.event.kind === 'run/completed') { clearActiveRun(true); pacer.complete(); runFinishedRef.current = true; setRunStatus('completed'); }
          else if (frame.event.kind === 'run/failed') { clearActiveRun(); runFinishedRef.current = true; setRunStatus('failed'); }
          else if (frame.event.kind === 'run/cancelled') { clearActiveRun(); runFinishedRef.current = true; setRunStatus('cancelled'); }
          else if (frame.event.kind === 'run/indeterminate') { clearActiveRun(); runFinishedRef.current = true; setRunStatus('indeterminate'); }
        } else if (frame.type === 'interaction.pending' && frame.interaction) {
          if (runFinishedRef.current || !runIDRef.current || frame.interaction.runID !== runIDRef.current ||
              typeof frame.interaction.turnID !== 'string' || !frame.interaction.turnID.trim() ||
              resolvedTurnsRef.current.has(frame.interaction.turnID) ||
              (pendingRef.current && pendingRef.current.turnID !== frame.interaction.turnID)) return;
          pendingRef.current = frame.interaction;
          const nodeID = frame.interaction.nodeID ?? frame.interaction.stepID;
          pacer.bypass(nodeID ? { nodeID, progressing: false } : undefined);
          setPending(frame.interaction);
          setExecutionNodeID(frame.interaction.nodeID ?? frame.interaction.stepID);
          setRunStatus('waiting');
        } else if (frame.type === 'interaction.resolved') {
          if (!pendingRef.current || pendingRef.current.turnID !== frame.turnID) return;
          if (runFinishedRef.current || (frame.runID && frame.runID !== runIDRef.current)) return;
          const nodeID = pendingRef.current.nodeID ?? pendingRef.current.stepID;
          pacer.bypass(nodeID ? { nodeID, progressing: false } : undefined);
          resolvedTurnsRef.current.add(frame.turnID!);
          if (hostRequestRef.current?.turnID === frame.turnID) hostRequestRef.current = undefined;
          pendingRef.current = undefined;
          setPending((current) => {
            if (current?.turnID !== frame.turnID) return current;
            return undefined;
          });
          setRunStatus((current) => isTerminalRunStatus(current) ? current : 'running');
        } else if (frame.type === 'run.finished') {
          if (frame.runID && directRunScopeRef.current && frame.runID !== directRunScopeRef.current) return;
          setResults(frame.resultsAvailability ?? { state: 'unavailable', reason: 'runtime-did-not-deliver-results' });
          const summaryRunID = frame.runID ?? directRunScopeRef.current;
          const successful = (frame.status ?? 'completed') === 'completed' &&
            !frame.steps?.some(step => isIssueStepStatus(step.status ?? '') || step.output?.outcome_category === 'blocked');
          clearActiveRun(successful);
          if (successful) pacer.complete();
          runFinishedRef.current = true;
          setRunStarting(false);
          setRouteTestRunning(false);
          const graph = directDocumentRef.current;
          const binding = graph?.display_plan_snapshot_digest ?? graph?.execution_plan_hash ??
            graph?.presentation_state?.plan_snapshot_digest ?? 'missing-binding';
          setRuntimeNodes((current) => applyTerminalSteps(
            summaryRunID ? restoreDirectDisplayWithdrawals(current, displayWithdrawalsRef.current, summaryRunID, binding) : current,
            frame.steps, binding, summaryRunID));
          setRunStatus(frame.status ?? 'completed');
          if (frame.routeTest) setRouteTestOutcome(frame.routeTest);
        } else if (frame.type === 'protocol.error') {
          clearActiveRun();
          setRunError('The run protocol rejected a command.');
        }
      } else if (message.type === 'run.error') {
        clearActiveRun();
        setRunStarting(false);
        setRouteTestRunning(false);
        setRunStatus('failed');
        setRunError(message.message);
      } else if (message.type === 'run.stderr') {
        setRunDiagnostics((current) => `${current}${message.text}`.slice(-16 * 1024));
      } else if (message.type === 'run.exit') {
        clearActiveRun(runFinishedRef.current && message.code === 0);
        setRunStarting(false);
        setRouteTestRunning(false);
        if (!runFinishedRef.current) {
          setRunStatus('failed');
          setRunError((current) => current ?? (message.code === 0
            ? 'Yawr exited before sending run.finished.'
            : `Yawr exited with code ${message.code ?? 'unknown'}`));
        }
      } else if (message.type === 'route-tests') {
        setRouteTests(message.routeTests);
      } else if (message.type === 'route-test.saved') {
        setRouteTestError(undefined);
      } else if (message.type === 'route-test.error') {
        setRouteTestError(message.message);
      } else if (message.type === 'yawr.xts.verify-view') {
        const hostRequest = hostRequestRef.current;
        const interaction = pendingRef.current;
        if (!hostRequest || !interaction || interaction.kind !== 'host_action' ||
            interaction.runID !== hostRequest.runID || interaction.turnID !== hostRequest.turnID ||
            runIDRef.current !== hostRequest.runID) return;
        if (matchesXtsViewCheck(message, {
          capability: hostRequest.capability, runId: hostRequest.runID, turnId: hostRequest.turnID,
          correlationId: hostRequest.correlationID, previewSessionId: hostSessionRef.current,
          requestId: hostRequest.requestID,
        })) setXtsViewCheck(message);
      } else if (message.type === 'yawr.host-action.ack' || message.type === 'yawr.host-action.cancel') {
        const response = parseHostActionResponse(message);
        if (!response) return;
        const interaction = pendingRef.current;
        const hostRequest = hostRequestRef.current;
        if (!interaction || interaction.kind !== 'host_action' || !interaction.host_action || !hostRequest) return;
        if (response.previewSessionId !== hostSessionRef.current ||
          interaction.runID !== hostRequest.runID ||
          interaction.turnID !== hostRequest.turnID ||
          interaction.host_action.capability !== hostRequest.capability ||
          response.correlationId !== hostRequest.correlationID ||
          response.requestId !== hostRequest.requestID) return;
        if (response.type === 'yawr.host-action.ack' && (
          response.runId !== hostRequest.runID ||
          response.turnId !== hostRequest.turnID ||
          response.capability !== hostRequest.capability
        )) return;
        if (runIDRef.current !== hostRequest.runID) return;
        setXtsViewCheck(undefined);
        if (response.type === 'yawr.host-action.ack' && response.status === 'completed' && response.result?.status === 'opened') {
          setXtsOpened(true);
        }
        const answer = {
              kind: 'host_action',
              runID: runIDRef.current,
              turnID: interaction.turnID,
              correlationID: hostRequest.correlationID,
              capability: interaction.host_action.capability,
              status: response.status,
              result: response.type === 'yawr.host-action.ack' ? response.result ?? undefined : undefined,
        };
        vscode.postMessage(sessionIDRef.current ? {
          type: 'session.command',
          command: {
            type: 'interaction.answer',
            runID: runIDRef.current,
            turnID: interaction.turnID,
            payload: answer,
          },
        } : {
          type: 'run.command',
          command: {
            type: 'interaction.answer',
            runID: runIDRef.current,
            turnID: interaction.turnID,
            answer,
          },
        });
        hostRequestRef.current = undefined;
      }
    };
    window.addEventListener('message', receive);
    vscode.postMessage({ type: 'ready' });
    return () => {
      window.removeEventListener('message', receive);
      window.document.removeEventListener('visibilitychange', visibility);
      pacer.dispose();
      visualPacerRef.current = undefined;
    };
  }, []);

  useEffect(() => {
    if (!document) return;
    const values: Record<string, string> = {};
    for (const declaration of graphInputDeclarations(document)) {
      values[declaration.name] = declaration.default === undefined || declaration.default === null
        ? ''
        : String(declaration.default);
    }
    setInputValues(values);
  }, [document?.hash]);

  const dispatchHostAction = (interaction: PendingInteraction, panelConfirmed: boolean) => {
    if (interaction.kind !== 'host_action' || !interaction.host_action || !runID ||
        runIDRef.current !== runID || pendingRef.current?.turnID !== interaction.turnID) return;
    const correlationID = interaction.correlationID ?? interaction.turnID;
    const requestID = `${hostSessionRef.current}:${correlationID}`;
    if (hostRequestRef.current?.requestID === requestID) return;
    hostRequestRef.current = {
      runID,
      turnID: interaction.turnID,
      capability: interaction.host_action.capability,
      correlationID,
      requestID,
    };
    const envelope = {
      version: 'yawr.host-action/v1' as const,
      capability: interaction.host_action.capability,
      runId: runID,
      turnId: interaction.turnID,
      correlationId: correlationID,
      previewSessionId: hostSessionRef.current,
      requestId: requestID,
    };
    if (panelConfirmed) {
      vscode.postMessage({ type: 'yawr.host-action.confirmed-request', ...envelope });
    }
    vscode.postMessage({
      type: 'yawr.host-action.request',
      ...envelope,
      request: interaction.host_action.request,
    });
  };

  /* Non-XTS capabilities preserve their existing automatic dispatch. XTS waits for explicit panel confirmation. */
  useEffect(() => {
    if (!pending || pending.kind !== 'host_action' || !pending.host_action || !runID || pending.host_action.capability === 'xts.open-view') return;
    dispatchHostAction(pending, false);
  }, [pending?.turnID, runID]);

  const startRun = (debugMode = false) => {
    if (!document) return;
    const declarations = graphInputDeclarations(document);
    const missing = declarations.find((declaration) => declaration.required && !inputValues[declaration.name]);
    if (missing) {
      setRunError(`${missing.name} is required.`);
      return;
    }
    if (debugMode && breakpoints.length === 0) {
      setRunError('Add at least one breakpoint before starting a debug run.');
      return;
    }
    const watchExpressions = watches.split(/\r?\n/).map((value) => value.trim()).filter(Boolean);
    if (watchExpressions.length > 32) {
      setRunError('Debug runs support at most 32 watch expressions.');
      return;
    }
    setRunError(undefined);
    hostRequestRef.current = undefined;
    pendingRef.current = undefined;
    runIDRef.current = undefined;
    setRunID(undefined);
    setRuntimeNodes({});
    setExecutionNodeID(undefined);
    setPending(undefined);
    vscode.postMessage({
      type: 'run.start',
      inputs: inputValues,
      ...(debugMode ? {
        debug: {
          enabled: true,
          breakpoints: breakpoints.map(({ step, phase, callPath }) => ({ step, phase, ...(callPath ? { callPath } : {}) })),
          ...(watchExpressions.length > 0 ? { watches: watchExpressions } : {}),
        },
      } : {}),
    });
  };

  const startSession = () => {
    if (!document || sessionID) return;
    const declarations = graphInputDeclarations(document);
    const missing = declarations.find((declaration) => declaration.required && !inputValues[declaration.name]);
    if (missing) {
      setRunError(`${missing.name} is required.`);
      return;
    }
    setRunError(undefined);
    vscode.postMessage({ type: 'session.start', inputs: inputValues });
  };

  const resumeSession = () => {
    if (!sessionID || !sessionAttached) return;
    vscode.postMessage({ type: 'session.command', command: { type: 'session.resume' } });
  };

  const closeSession = (status: 'resolved' | 'escalated' | 'cancelled' | 'abandoned') => {
    if (!sessionID || !sessionAttached) return;
    vscode.postMessage({
      type: 'session.command',
      command: { type: 'session.close', payload: { status } },
    });
  };

  const cancelRun = () => {
    visualPacerRef.current?.bypass();
    if (sessionID && sessionAttached) {
      vscode.postMessage({
        type: 'session.command',
        command: {
          type: 'session.cancel',
          ...(runID ? { runID } : {}),
          payload: { reason: 'operator cancelled' },
        },
      });
      return;
    }
    if (!runID) return;
    const hostRequest = hostRequestRef.current;
    if (hostRequest) {
      vscode.postMessage({
        type: 'yawr.host-action.cancel',
        version: 'yawr.host-action/v1',
        correlationId: hostRequest.correlationID,
        previewSessionId: hostSessionRef.current,
        requestId: hostRequest.requestID,
        status: 'execution-not-started',
        reason: 'run-replaced',
      });
    }
    hostRequestRef.current = undefined;
    vscode.postMessage({
      type: 'run.command',
      command: { type: 'run.cancel', runID, reason: 'operator cancelled' },
    });
  };

  const resetRun = () => {
    visualPacerRef.current?.bypass();
    if (sessionID) {
      vscode.postMessage({ type: 'session.reset' });
      clearActiveRun();
      sessionIDRef.current = undefined;
      sessionStatusRef.current = undefined;
      setSessionID(undefined);
      setSessionStatus(undefined);
      setSessionAttached(false);
      setSegmentGraphRevisions({});
      setUnloadedSegmentIDs([]);
      setRevisionNodes({});
      setRunStatus('idle');
      setRunStarting(false);
      setRunError(undefined);
      setRunDiagnostics('');
      setRuntimeNodes({});
      setExecutionNodeID(undefined);
      return;
    }
    if (runStarting || !isTerminalRunStatus(runStatus)) return;
    vscode.postMessage({ type: 'run.reset' });
    if (sourceDocumentRef.current) {
      directDocumentRef.current = sourceDocumentRef.current;
      setDocument(sourceDocumentRef.current);
    }
    setResults(undefined);
    clearActiveRun();
    runFinishedRef.current = true;
    setRunStatus('idle');
    setRunStarting(false);
    setRouteTestRunning(false);
    setRunError(undefined);
    setRunDiagnostics('');
    setRuntimeNodes({});
    setExecutionNodeID(undefined);
    setRouteTestOutcome(undefined);
    setRouteTestError(undefined);
  };

  const submitInteraction = (answer: Record<string, unknown>) => {
    if (!pending || !runID) return;
    vscode.postMessage(sessionID ? {
      type: 'session.command',
      command: {
        type: 'interaction.answer',
        runID,
        turnID: pending.turnID,
        payload: answer,
      },
    } : {
      type: 'run.command',
      command: {
        type: 'interaction.answer',
        runID,
        turnID: pending.turnID,
        answer,
      },
    });
  };

  useEffect(() => {
    if (!testMode) return;
    const receiveTestAction = (event: MessageEvent<HostMessage>) => {
      const message = event.data;
      if (!message || message.type !== 'test.action') return;
      if (message.action === 'set-input' && message.name) {
        setInputValues((current) => ({ ...current, [message.name!]: message.value ?? '' }));
      } else if (message.action === 'run') {
        startRun(false);
      } else if (message.action === 'debug') {
        startRun(true);
      } else if (message.action === 'reset') {
        resetRun();
      } else if (message.action === 'cancel') {
        cancelRun();
      } else if (message.action === 'answer' && message.answer) {
        submitInteraction(message.answer);
      } else if (message.action === 'run-route-test' && message.artifact) {
        vscode.postMessage({ type: 'route-test.run', artifact: message.artifact });
      } else if (message.action === 'save-route-test' && message.artifact) {
        vscode.postMessage({ type: 'route-test.save', artifact: message.artifact });
      } else if (message.action === 'save-route-test-result' && message.artifact) {
        vscode.postMessage({ type: 'route-test.save', artifact: { ...message.artifact, last_result: {} } });
      } else if (message.action === 'click-button' && message.name) {
        const button = Array.from(window.document.querySelectorAll<HTMLButtonElement>('button'))
          .find((candidate) => candidate.textContent?.trim() === message.name || candidate.title === message.name);
        button?.click();
        requestAnimationFrame(() => requestAnimationFrame(() => vscode.postMessage({
          type: 'test.dom.state',
          clicked: message.name,
          found: button !== undefined,
          collectorReviewVisible: window.document.querySelector('.collector-review') !== null,
          visibleButtons: Array.from(window.document.querySelectorAll<HTMLButtonElement>('button'))
            .map((candidate) => candidate.textContent?.trim()).filter(Boolean),
          disabledButtons: Array.from(window.document.querySelectorAll<HTMLButtonElement>('button:disabled'))
            .map((candidate) => candidate.textContent?.trim()).filter(Boolean),
          routeTestSafetyText: window.document.querySelector('.route-test-global-safety')?.textContent?.trim(),
        })));
      } else if (message.action === 'click-route-test-checkbox' && message.name) {
        const label = Array.from(window.document.querySelectorAll<HTMLLabelElement>('.route-test-pane label'))
          .find((candidate) => candidate.textContent?.includes(message.name!));
        const control = label?.querySelector<HTMLInputElement>('input[type="checkbox"]');
        control?.click();
        requestAnimationFrame(() => requestAnimationFrame(() => vscode.postMessage({
          type: 'test.dom.state',
          routeTestCheckbox: message.name,
          found: control !== undefined,
          checked: control?.checked,
        })));
      } else if (message.action === 'toggle-choice' && message.name) {
        const controls = Array.from(window.document.querySelectorAll<HTMLInputElement>('input[name="choice"]'));
        const control = controls.find((candidate) => (
          candidate.closest('label')?.querySelector('strong')?.textContent?.trim() === message.name
        ));
        control?.click();
        requestAnimationFrame(() => requestAnimationFrame(() => {
          const currentControls = Array.from(window.document.querySelectorAll<HTMLInputElement>('input[name="choice"]'));
          const labelFor = (candidate: HTMLInputElement) => candidate.closest('label')?.querySelector('strong')?.textContent?.trim();
          vscode.postMessage({
            type: 'test.dom.state',
            choice: message.name,
            found: control !== undefined,
            selectedChoices: currentControls.filter((candidate) => candidate.checked).map(labelFor).filter(Boolean),
            disabledChoices: currentControls.filter((candidate) => candidate.disabled).map(labelFor).filter(Boolean),
            continueDisabled: window.document.querySelector<HTMLButtonElement>('.interaction-pane button[type="submit"]')?.disabled,
            choiceLimitText: window.document.querySelector('.choice-limit')?.textContent?.trim(),
          });
        }));
      } else if (message.action === 'set-collector-field' && message.name) {
        const control = window.document.querySelector<HTMLInputElement | HTMLSelectElement>(
          `.collector-field[data-field-name="${CSS.escape(message.name)}"] input, .collector-field[data-field-name="${CSS.escape(message.name)}"] select`,
        );
        if (control) {
          const descriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(control), 'value');
          descriptor?.set?.call(control, message.value ?? '');
          control.dispatchEvent(new Event('change', { bubbles: true }));
          control.dispatchEvent(new Event('input', { bubbles: true }));
        }
        requestAnimationFrame(() => requestAnimationFrame(() => vscode.postMessage({
          type: 'test.dom.state',
          field: message.name,
          found: control !== null,
          value: control?.value,
          collectorReviewVisible: window.document.querySelector('.collector-review') !== null,
        })));
      } else if (message.action === 'toggle-breakpoint' && message.name && (message.value === 'before' || message.value === 'after') && document) {
        setBreakpoints((current) => toggleBreakpoint(current, document, message.name!, message.value as DirectDebugPhase));
      }
    };
    window.addEventListener('message', receiveTestAction);
    return () => window.removeEventListener('message', receiveTestAction);
  }, [document, inputValues, pending, runID, runStatus, runStarting, breakpoints, watches, testMode]);

  useEffect(() => {
    if (!testMode) return;
    const frame = requestAnimationFrame(() => {
      vscode.postMessage({
        type: 'ui.state',
        sessionID,
        sessionStatus,
        sessionAttached,
        runID,
        runStatus,
        runStarting,
        reloading,
        runError,
        pendingKind: pending?.kind,
        pendingDebugPhase: pending?.debug?.phase,
        pendingTurnID: pending?.turnID,
        inputCount: document ? graphInputDeclarations(document).length : 0,
        graphNodeIDs: document?.nodes.map((node) => node.id) ?? [],
        segmentCount: document?.groups.filter((group) => group.kind === 'session-segment').length ?? 0,
        handoffEdgeCount: document?.edges.filter((edge) => edge.type === 'session-transition').length ?? 0,
        visibleGraphNodeIDs: Array.from(window.document.querySelectorAll<HTMLElement>('.react-flow__node-yawrStep')).map((node) => node.dataset.id).filter(Boolean),
        inputValues: Object.fromEntries(Object.entries(inputValues).map(([name, value]) => {
          const declaration = document ? graphInputDeclarations(document).find((input) => input.name === name) : undefined;
          return [name, declaration?.type === 'secret' && value ? '<redacted>' : value];
        })),
        runButtonCount: window.document.querySelectorAll('.run-actions button.primary').length,
        runDiagnostics,
        debugRunButtonCount: window.document.querySelectorAll('.run-actions button.debug-run').length,
        resetButtonCount: window.document.querySelectorAll('.run-actions button.reset-run').length,
        cancelButtonCount: window.document.querySelectorAll('.run-actions button.danger').length,
        breakpointCount: breakpoints.length,
        debugOverrideCount: Object.values(runtimeNodes).filter((state) => state.debugOverride !== undefined).length,
        routeTestPassed: routeTestOutcome?.passed,
        routeTestTargetReached: routeTestOutcome?.targetReached,
        routeTestExternalDispatches: routeTestOutcome?.externalDispatches,
        routeTestPlanHash: routeTestContext?.planHash,
        routeTestError,
        savedRouteTestIDs: routeTests.map(({ artifact }) => artifact.id),
        savedRouteTestResults: Object.fromEntries(routeTests.map(({ artifact }) => [artifact.id, artifact.last_result?.status])),
        collectorReviewVisible: window.document.querySelector('.collector-review') !== null,
        visibleButtons: Array.from(window.document.querySelectorAll<HTMLButtonElement>('button')).map((button) => button.textContent?.trim()).filter(Boolean),
        nodeStatuses: Object.fromEntries(Object.entries(runtimeNodes).map(([id, state]) => [id, state.status])),
        executionNodeID,
        executionPositionLabel: executionNodeID
          ? isExecutionEnded(runStatus) ? 'Last reached' : 'Current step'
          : undefined,
        executionPositionTitle: document?.nodes.find((node) => node.id === executionNodeID)?.data.title,
        currentExecutionMarkerCount: window.document.querySelectorAll('.step-node.execution-current').length,
        lastExecutionMarkerCount: window.document.querySelectorAll('.step-node.execution-last').length,
      });
    });
    return () => cancelAnimationFrame(frame);
  }, [document, inputValues, sessionID, sessionStatus, sessionAttached, runID, runStatus, runStarting, reloading, runError, pending?.turnID, runtimeNodes, executionNodeID, visualStep, results, breakpoints, routeTestContext?.planHash, routeTestOutcome, routeTestError, routeTests, xtsViewCheck, testMode]);

  if (loading) return <div className="state" role="status">Loading runbook...</div>;
  if (error) return <div className="state error" role="alert">{error}</div>;
  if (!document) return <div className="state" role="status">No graph loaded</div>;
  return (
    <GraphView
      document={document}
      results={sessionAttached ? undefined : results}
      testMode={testMode}
      style={style}
      runtimeNodes={runtimeNodes}
      executionNodeID={executionNodeID}
      visualStep={visualStep}
      breakpoints={breakpoints}
      watches={watches}
      pending={pending}
      sessionID={sessionID}
      sessionStatus={sessionStatus}
      sessionAttached={sessionAttached}
      segmentGraphRevisions={segmentGraphRevisions}
      unloadedSegmentIDs={unloadedSegmentIDs}
      revisionNodes={revisionNodes}
      runID={runID}
      runStatus={runStatus}
      runStarting={runStarting}
      reloading={reloading}
      runError={runError}
      runDiagnostics={runDiagnostics}
      inputValues={inputValues}
      onInputChange={(name, value) => setInputValues((current) => ({ ...current, [name]: value }))}
      onRun={() => startRun(false)}
      onStartSession={startSession}
      onResumeSession={resumeSession}
      onCloseSession={closeSession}
      onRequestGraphRevision={(requestID, segmentID, revision, originalNodeID) => vscode.postMessage({
        type: 'session.graph-revision', requestID, segmentID, revision, originalNodeID,
      })}
      onDebugRun={() => startRun(true)}
      onReset={resetRun}
      onCancel={cancelRun}
      onSubmitInteraction={submitInteraction}
      onConfirmHostAction={(interaction) => dispatchHostAction(interaction, true)}
      onToggleBreakpoint={(nodeID, phase) => setBreakpoints((current) => toggleBreakpoint(current, document, nodeID, phase))}
      onWatchesChange={setWatches}
      onStyleChange={(nextStyle) => {
        setStyle(nextStyle);
        vscode.postMessage({ type: 'style.change', style: nextStyle });
      }}
      routeTestContext={routeTestContext}
      routeTests={routeTests}
      routeTestOutcome={routeTestOutcome}
      routeTestRunning={routeTestRunning}
      routeTestError={routeTestError}
      onSaveRouteTest={(artifact) => vscode.postMessage({ type: 'route-test.save', artifact })}
      onRunRouteTest={(artifact) => {
        setRouteTestOutcome(undefined);
        setRouteTestError(undefined);
        vscode.postMessage({ type: 'route-test.run', artifact });
      }}
      xtsOpened={xtsOpened}
      xtsViewCheck={xtsViewCheck}
      onVerifyXtsView={(status) => {
        if (!xtsViewCheck || xtsViewCheck.runId !== runIDRef.current ||
            xtsViewCheck.turnId !== pendingRef.current?.turnID ||
            xtsViewCheck.requestId !== hostRequestRef.current?.requestID) return;
        vscode.postMessage({ ...xtsViewCheck, type: 'yawr.xts.view-verified', status });
      }}
    />
  );
}

const root = document.getElementById('root');
if (!root) throw new Error('Missing webview root element');
createRoot(root).render(<App />);