import { parseStepDetails, type StepDetails } from './stepDetails';
import { parseDisplayJSON } from './displayPresentationJSON';
import { decodePresentationState, type PresentationState } from './presentationHistory';

export interface GraphRunbookRef {
  id?: string;
  name?: string;
  path?: string;
}

export interface GraphNodeData {
  id?: string;
  kind?: string;
  title?: string;
  status?: string;
  tool_name?: string;
  tool_action?: string;
  details?: StepDetails;
  [key: string]: unknown;
}

export interface GraphNode {
  id: string;
  type?: string;
  data: GraphNodeData;
  parentNode?: string;
  extent?: 'parent';
  position: { x: number; y: number };
}

export interface GraphEdge {
  id: string;
  source: string;
  target: string;
  type?: string;
  label?: string;
  routeKind?: 'arm-entry' | 'arm-exit' | 'empty-arm' | 'no-match';
  runtimeNodeID?: string;
  runtimeExpectedStatus?: string;
  runtimeArmIndex?: number;
  runtimeArmLabel?: string;
  runtimeFallbackNodeID?: string;
}

export interface GraphFrame {
  id: string;
  runbook_id: string;
  runbook_path: string;
  parent_include_node_id?: string;
  depth: number;
  invocation?: GraphInvocation;
}

export interface GraphInvocation {
  bindings?: Array<{ name: string; type: string; mutable?: boolean; value: unknown; value_present: boolean; enum?: unknown }>;
  outputs?: Record<string, { type: string; description?: string; value?: string; value_expr?: string;
    value_tree?: unknown; value_tree_present?: boolean; optional?: boolean; enum?: unknown }>;
  results?: boolean;
}

function parseInvocation(value: unknown): GraphInvocation {
  const object = plainObject(value, 'frame invocation');
  const closed = (v: Record<string, unknown>, keys: string[]) => {
    if (Object.keys(v).some(key => !keys.includes(key))) throw new Error('unknown invocation field');
  };
  closed(object, ['bindings', 'outputs', 'results']);
  if (object.results !== undefined && typeof object.results !== 'boolean') throw new Error('invalid invocation results');
  if (object.bindings !== undefined) {
    if (!Array.isArray(object.bindings)) throw new Error('invalid invocation bindings');
    const names = new Set<string>();
    for (const raw of object.bindings) {
      const binding = plainObject(raw, 'invocation binding');
      closed(binding, ['name', 'type', 'mutable', 'value', 'value_present', 'enum']);
      const name = requiredString(binding, 'name', 'binding name');
      requiredString(binding, 'type', 'binding type');
      if (names.has(name) || !Object.hasOwn(binding, 'value') || typeof binding.value_present !== 'boolean' ||
          binding.mutable !== undefined && typeof binding.mutable !== 'boolean') throw new Error('invalid invocation binding');
      names.add(name);
    }
  }
  if (object.outputs !== undefined) {
    for (const [name, raw] of Object.entries(plainObject(object.outputs, 'invocation outputs'))) {
      if (!name) throw new Error('invalid invocation output name');
      const output = plainObject(raw, 'invocation output');
      closed(output, ['type', 'description', 'value', 'value_expr', 'value_tree', 'value_tree_present', 'optional', 'enum']);
      requiredString(output, 'type', 'output type');
      for (const key of ['description', 'value', 'value_expr']) optionalString(output, key, `output ${key}`);
      for (const key of ['value_tree_present', 'optional']) {
        if (output[key] !== undefined && typeof output[key] !== 'boolean') throw new Error('invalid output presence');
      }
      if (Object.hasOwn(output, 'value_tree') && output.value_tree_present !== true) throw new Error('invalid output tree presence');
      // Go omits a nil value_tree; its presence bit still denotes authored null.
      if (output.value_tree_present === true && !Object.hasOwn(output, 'value_tree')) output.value_tree = null;
    }
  }
  return object as GraphInvocation;
}

export interface GraphGroup {
  id: string;
  kind: string;
  parent_node_id: string;
  frame_id: string;
  label?: string;
  index?: number;
  fallback?: boolean;
  segment_id?: string;
  segment_status?: string;
  run_id?: string;
  graph_loaded?: boolean;
}

export interface GraphDocument {
  schema_version: '1' | '3';
  hash?: string;
  runbook: GraphRunbookRef;
  nodes: GraphNode[];
  edges: GraphEdge[];
  frames: GraphFrame[];
  groups: GraphGroup[];
  regions?: unknown;
  inputs?: unknown[];
  presentation_state?: PresentationState;
  execution_plan_hash?: string;
  display_plan_snapshot_digest?: string;
  bound_content_hash?: string;
}

export function validateSessionPresentationBinding(document: GraphDocument, snapshotDigest: string | undefined): void {
  const envelopes = document.nodes.flatMap(node => node.data.details?.code_presentation ? [node.data.details.code_presentation] : []);
  if (document.display_plan_snapshot_digest !== undefined &&
      (typeof document.display_plan_snapshot_digest !== 'string' || document.display_plan_snapshot_digest.length !== 71 ||
       !/^sha256:[a-f0-9]{64}$/.test(document.display_plan_snapshot_digest))) {
    throw new Error('session graph display snapshot binding is invalid');
  }
  if (!envelopes.length && !document.presentation_state && document.display_plan_snapshot_digest === undefined) return;
  if (!snapshotDigest || !/^sha256:[a-f0-9]{64}$/.test(snapshotDigest) ||
      document.execution_plan_hash !== snapshotDigest ||
      envelopes.some(envelope => envelope.origin !== 'frozen' || envelope.plan_snapshot_digest !== snapshotDigest) ||
      (document.presentation_state !== undefined && document.presentation_state.plan_snapshot_digest !== snapshotDigest)) {
    throw new Error('session graph presentation does not match its frozen execution-plan binding');
  }
}

export function graphPreviewArgs(runbookPath: string, packageMapPath?: string): string[] {
  return ['preview', '--format', 'graphjson', '--recurse',
    ...(packageMapPath ? ['--package-map', packageMapPath] : []), runbookPath];
}

export function savedRunPreviewArgs(runDir: string, runID: string): string[] {
  return ['preview', '--format', 'graphjson', '--run-dir', runDir, '--run-id', runID];
}

export type GraphPreviewExecutor = (
  binary: string,
  args: string[],
) => Promise<{ stdout: string }>;

export async function loadGraphDocument(
  binary: string,
  runbookPath: string,
  execute: GraphPreviewExecutor,
  packageMapPath?: string,
): Promise<GraphDocument> {
  const { stdout } = await execute(binary, graphPreviewArgs(runbookPath, packageMapPath));
  return parseGraphDocument(stdout);
}

export function parseGraphDocument(stdout: string): GraphDocument {
  let value: unknown;
  try {
    value = parseDisplayJSON(stdout);
  } catch {
    throw new Error('yawr preview did not return valid JSON');
  }
  const document = plainObject(value, 'yawr preview graph document');
  if (document.presentation_state !== undefined) document.presentation_state = decodePresentationState(document.presentation_state);
  if (document.schema_version !== '1' && document.schema_version !== '3') {
    throw new Error(`unsupported graph schema_version ${JSON.stringify(document.schema_version)}`);
  }
  if (!Array.isArray(document.nodes)) {
    throw new Error('yawr preview graph document nodes must be an array');
  }
  if (!Array.isArray(document.edges)) {
    throw new Error('yawr preview graph document edges must be an array');
  }
  if (!Array.isArray(document.frames)) {
    throw new Error('yawr preview graph document frames must be an array');
  }
  if (!Array.isArray(document.groups)) {
    throw new Error('yawr preview graph document groups must be an array');
  }

  const runbook = plainObject(document.runbook, 'graph runbook');
  requiredString(runbook, 'id', 'runbook.id');
  requiredString(runbook, 'name', 'runbook.name');
  optionalString(runbook, 'path', 'runbook.path');

  const frameIDs = new Set<string>();
  const frames = document.frames.map((value, index) => {
    const frame = plainObject(value, `frame ${index}`);
    const id = requiredString(frame, 'id', 'frame id');
    if (frameIDs.has(id)) throw new Error(`duplicate frame id ${JSON.stringify(id)}`);
    frameIDs.add(id);
    requiredString(frame, 'runbook_id', `frame ${id} runbook_id`);
    requiredString(frame, 'runbook_path', `frame ${id} runbook_path`);
    optionalString(frame, 'parent_include_node_id', `frame ${id} parent_include_node_id`);
    if (!Number.isInteger(frame.depth) || (frame.depth as number) < 0) {
      throw new Error(`frame ${id} depth must be a non-negative integer`);
    }
    if (frame.invocation !== undefined) {
      if (document.schema_version !== '3') throw new Error('invocation requires graph v3');
      frame.invocation = parseInvocation(frame.invocation);
    }
    return frame;
  });

  const nodeIDs = new Set<string>();
  const nodes = document.nodes.map((value, index) => {
    const node = plainObject(value, `node ${index}`);
    const id = requiredString(node, 'id', 'node id');
    if (nodeIDs.has(id)) throw new Error(`duplicate node id ${JSON.stringify(id)}`);
    nodeIDs.add(id);
    optionalString(node, 'type', `node ${id} type`);
    const data = plainObject(node.data, `node ${id} data`);
    const dataID = requiredString(data, 'id', `node ${id} data.id`);
    if (dataID !== id) throw new Error(`node ${id} data.id must match its node id`);
    const kind = requiredString(data, 'kind', `node ${id} data.kind`);
    optionalString(data, 'title', `node ${id} data.title`);
    optionalString(data, 'status', `node ${id} data.status`);
    optionalString(data, 'tool_name', `node ${id} data.tool_name`);
    optionalString(data, 'tool_action', `node ${id} data.tool_action`);
    if (data.details !== undefined) {
      data.details = parseStepDetails(data.details, kind, `node ${id} data`, document.schema_version === '3');
    }
    optionalString(data, 'group_id', `node ${id} data.group_id`);
    optionalString(data, 'frame_id', `node ${id} data.frame_id`);
    const position = plainObject(node.position, `node ${id} position`);
    if (!finiteNumber(position.x) || !finiteNumber(position.y)) {
      throw new Error(`node ${id} position x and y must be finite numbers`);
    }
    optionalString(node, 'parentNode', `node ${id} parentNode`);
    if (node.extent !== undefined && node.extent !== 'parent') {
      throw new Error(`node ${id} extent must be "parent" when present`);
    }
    return node;
  });

  const groupIDs = new Set<string>();
  const groups = document.groups.map((value, index) => {
    const group = plainObject(value, `group ${index}`);
    const id = requiredString(group, 'id', 'group id');
    if (groupIDs.has(id)) throw new Error(`duplicate group id ${JSON.stringify(id)}`);
    if (nodeIDs.has(id)) throw new Error(`group id ${JSON.stringify(id)} collides with node id`);
    groupIDs.add(id);
    requiredString(group, 'kind', `group ${id} kind`);
    const parentNodeID = optionalString(group, 'parent_node_id', `group ${id} parent_node_id`);
    const frameID = requiredString(group, 'frame_id', `group ${id} frame_id`);
    optionalString(group, 'label', `group ${id} label`);
    if (group.index !== undefined && (!Number.isInteger(group.index) || (group.index as number) < 0)) {
      throw new Error(`group ${id} index must be a non-negative integer`);
    }
    if (group.fallback !== undefined && typeof group.fallback !== 'boolean') {
      throw new Error(`group ${id} fallback must be a boolean`);
    }
    if (parentNodeID && !nodeIDs.has(parentNodeID)) {
      throw new Error(`group ${id} parent node ${JSON.stringify(parentNodeID)} is unknown`);
    }
    if (frameIDs.size > 0 && !frameIDs.has(frameID)) {
      throw new Error(`group ${id} frame ${JSON.stringify(frameID)} is unknown`);
    }
    return group;
  });

  for (const frame of frames) {
    const parent = frame.parent_include_node_id;
    if (typeof parent === 'string' && parent && !nodeIDs.has(parent)) {
      throw new Error(`frame ${String(frame.id)} parent include node ${JSON.stringify(parent)} is unknown`);
    }
  }

  for (const node of nodes) {
    const id = String(node.id);
    const data = node.data as Record<string, unknown>;
    const groupID = typeof data.group_id === 'string' ? data.group_id : '';
    const parentNode = typeof node.parentNode === 'string' ? node.parentNode : '';
    if (groupID && !groupIDs.has(groupID)) {
      throw new Error(`node ${id} references unknown group ${JSON.stringify(groupID)}`);
    }
    if (parentNode && !groupIDs.has(parentNode)) {
      throw new Error(`node ${id} parentNode references unknown group ${JSON.stringify(parentNode)}`);
    }
    if (parentNode !== groupID) {
      throw new Error(`node ${id} parentNode must match data.group_id`);
    }
    const frameID = typeof data.frame_id === 'string' ? data.frame_id : '';
    if (frameID && frameIDs.size > 0 && !frameIDs.has(frameID)) {
      throw new Error(`node ${id} references unknown frame ${JSON.stringify(frameID)}`);
    }
  }

  const nodeByID = new Map(nodes.map((node) => [String(node.id), node]));
  const parentByGroup = new Map<string, string>();
  for (const group of groups) {
    const parentNodeID = typeof group.parent_node_id === 'string' ? group.parent_node_id : '';
    const parentNode = parentNodeID ? nodeByID.get(parentNodeID) : undefined;
    const parentData = parentNode ? parentNode.data as Record<string, unknown> : undefined;
    parentByGroup.set(String(group.id), typeof parentData?.group_id === 'string' ? parentData.group_id : '');
  }
  for (const groupID of groupIDs) {
    const visited = new Set<string>([groupID]);
    let parentID = parentByGroup.get(groupID) ?? '';
    while (parentID) {
      if (visited.has(parentID)) {
        throw new Error(`group ownership cycle detected at ${JSON.stringify(parentID)}`);
      }
      visited.add(parentID);
      parentID = parentByGroup.get(parentID) ?? '';
    }
  }

  const edgeIDs = new Set<string>();
  for (const edge of document.edges) {
    const candidate = plainObject(edge, 'graph edge');
    const id = requiredString(candidate, 'id', 'edge id');
    if (edgeIDs.has(id)) throw new Error(`duplicate edge id ${JSON.stringify(id)}`);
    edgeIDs.add(id);
    const source = requiredString(candidate, 'source', `edge ${id} source`);
    const target = requiredString(candidate, 'target', `edge ${id} target`);
    optionalString(candidate, 'type', `edge ${id} type`);
    optionalString(candidate, 'label', `edge ${id} label`);
    if (!nodeIDs.has(source)) throw new Error(`edge ${id} references unknown source ${JSON.stringify(source)}`);
    if (!nodeIDs.has(target)) throw new Error(`edge ${id} references unknown target ${JSON.stringify(target)}`);
  }

  if (document.execution_plan_hash !== undefined) {
    validateSessionPresentationBinding(value as GraphDocument, document.execution_plan_hash as string);
  }
  return value as GraphDocument;
}

export function graphMayRequireMcpBridge(
  document: GraphDocument,
  vscodeMcpActions: Readonly<Record<string, unknown>>,
): boolean {
  return document.nodes.some((node) => {
    const kind = node.data.kind;
    if (kind === 'include' && node.data.dynamic === true) return true;
    if (kind !== 'tool') return false;
    const toolName = node.data.tool_name;
    const toolAction = node.data.tool_action;
    if (!toolName || !toolAction) return true;
    return `${toolName}/${toolAction}` in vscodeMcpActions;
  });
}

export function sessionMayRequireMcpBridge(
  entryDocument: GraphDocument,
  vscodeMcpActions: Readonly<Record<string, unknown>>,
): boolean {
  return Object.keys(vscodeMcpActions).length > 0 ||
    graphMayRequireMcpBridge(entryDocument, vscodeMcpActions);
}

function plainObject(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`);
  }
  return value as Record<string, unknown>;
}

function requiredString(record: Record<string, unknown>, key: string, label: string): string {
  const value = record[key];
  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(`${label} must be a non-empty string`);
  }
  return value;
}

function optionalString(record: Record<string, unknown>, key: string, label: string): string {
  const value = record[key];
  if (value === undefined) return '';
  if (typeof value !== 'string') throw new Error(`${label} must be a string`);
  return value;
}

function finiteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

export function createDirectGraphWebviewHtml(
  scriptUri: string,
  styleUri: string,
  cspSource: string,
  nonce: string,
  highlightingWorkerUri = '',
  highlightingEnabled = true,
): string {
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src ${cspSource} data:; style-src ${cspSource}; style-src-attr 'unsafe-inline'; script-src 'nonce-${nonce}'; connect-src ${cspSource}; worker-src blob:;">
  <link rel="stylesheet" href="${styleUri}">
  <title>Yawr runbook graph</title>
</head>
<body data-highlighting-worker="${highlightingWorkerUri}" data-highlighting-enabled="${highlightingEnabled}">
  <div id="root" role="application" aria-label="Runbook graph"></div>
  <script nonce="${nonce}" src="${scriptUri}"></script>
</body>
</html>`;
}