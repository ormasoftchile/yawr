import { promises as fs } from 'fs';
import * as path from 'path';
import { createHash } from 'crypto';
import { dump, JSON_SCHEMA, load } from 'js-yaml';
import type {
  RouteTestApproval,
  RouteTestArtifact,
  RouteTestHostActionResponse,
  RouteTestInteractionAnswer,
  RouteTestReview,
  RouteTestSelector,
  RouteTestSource,
  RouteTestStepResponse,
  SavedRouteTest,
} from './routeTestTypes';
import type { GraphDocument, GraphNode } from './directGraphPreview';

const MAX_ARTIFACT_BYTES = 1024 * 1024;
const ROUTE_TEST_DIRECTORY = path.join('.yawr', 'route-tests');
const ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

export function parseRouteTestArtifact(value: unknown): RouteTestArtifact {
  const artifact = record(value, 'route test');
  exactFields(artifact, [
    'apiVersion', 'id', 'name', 'runbook', 'plan_hash', 'sensitivity_reviewed', 'target', 'inputs',
    'step_responses', 'host_action_responses', 'interaction_answers', 'test_approvals', 'last_result',
  ], 'route test');
  if (artifact.apiVersion !== 'yawr.route-test/v1') throw new Error('route test apiVersion must be yawr.route-test/v1');
  const id = stringValue(artifact.id, 'route test id', 128);
  if (!ID_PATTERN.test(id)) throw new Error('route test id must use letters, numbers, dots, underscores, or hyphens');
  const parsed: RouteTestArtifact = {
    apiVersion: 'yawr.route-test/v1',
    id,
    name: stringValue(artifact.name, 'route test name', 256),
    runbook: normalizedRelativePath(stringValue(artifact.runbook, 'route test runbook', 4096)),
    plan_hash: stringValue(artifact.plan_hash, 'route test plan_hash', 256),
    sensitivity_reviewed: true,
    target: parseSelector(artifact.target, 'route test target'),
  };
  if (artifact.sensitivity_reviewed !== true) throw new Error('route test sensitivity_reviewed must be true before persistence');
  if (parsed.target.phase !== 'before') throw new Error('route test target phase must be before');
  if (artifact.inputs !== undefined) parsed.inputs = parseStringMap(artifact.inputs, 'route test inputs');
  if (artifact.step_responses !== undefined) {
    parsed.step_responses = arrayValue(artifact.step_responses, 'step_responses', 512)
      .map((item, index) => parseStepResponse(item, `step_responses[${index}]`));
  }
  if (artifact.host_action_responses !== undefined) {
    parsed.host_action_responses = arrayValue(artifact.host_action_responses, 'host_action_responses', 512)
      .map((item, index) => parseHostResponse(item, `host_action_responses[${index}]`));
  }
  if (artifact.interaction_answers !== undefined) {
    parsed.interaction_answers = arrayValue(artifact.interaction_answers, 'interaction_answers', 512)
      .map((item, index) => parseInteraction(item, `interaction_answers[${index}]`));
  }
  if (artifact.test_approvals !== undefined) {
    parsed.test_approvals = arrayValue(artifact.test_approvals, 'test_approvals', 512)
      .map((item, index) => parseApproval(item, `test_approvals[${index}]`));
  }
  if (parsed.inputs && Object.keys(parsed.inputs).length === 0) delete parsed.inputs;
  if (parsed.step_responses?.length === 0) delete parsed.step_responses;
  if (parsed.host_action_responses?.length === 0) delete parsed.host_action_responses;
  if (parsed.interaction_answers?.length === 0) delete parsed.interaction_answers;
  if (parsed.test_approvals?.length === 0) delete parsed.test_approvals;
  if (artifact.last_result !== undefined) {
    const result = record(artifact.last_result, 'last_result');
    exactFields(result, ['status', 'target_reached', 'external_dispatches', 'ran_at', 'conditions_digest'], 'last_result');
    const status = stringValue(result.status, 'last_result.status', 32);
    if (!['reached', 'route-changed', 'runtime-failed', 'safety-failed', 'stopped'].includes(status)) throw new Error('last_result.status is not supported');
    if (typeof result.target_reached !== 'boolean') throw new Error('last_result.target_reached must be a boolean');
    if (!Number.isInteger(result.external_dispatches) || (result.external_dispatches as number) < 0) throw new Error('last_result.external_dispatches must be a non-negative integer');
    const ranAt = stringValue(result.ran_at, 'last_result.ran_at', 64);
    const conditionsDigest = stringValue(result.conditions_digest, 'last_result.conditions_digest', 128);
    if (Number.isNaN(Date.parse(ranAt))) throw new Error('last_result.ran_at must be a timestamp');
    parsed.last_result = { status: status as NonNullable<RouteTestArtifact['last_result']>['status'], target_reached: result.target_reached, external_dispatches: result.external_dispatches as number, ran_at: ranAt, conditions_digest: conditionsDigest };
    if (parsed.last_result.status === 'reached' && (!parsed.last_result.target_reached || parsed.last_result.external_dispatches !== 0)) {
      throw new Error('a reached result requires target_reached and zero external dispatches');
    }
  }
  for (const binding of [
    ...(parsed.step_responses ?? []),
    ...(parsed.host_action_responses ?? []),
    ...(parsed.interaction_answers ?? []),
    ...(parsed.test_approvals ?? []),
  ]) {
    if (binding.at.phase !== 'execute') throw new Error('route test response phase must be execute');
  }
    rejectSensitiveArtifactData(parsed);
    if (parsed.last_result && parsed.last_result.conditions_digest !== routeTestConditionsDigest(parsed as unknown as Record<string, unknown>)) {
      throw new Error('last_result does not match the current route-test conditions');
    }
  boundedJSON(parsed, 'route test');
  return parsed;
}

export async function saveRouteTestArtifact(projectRoot: string, value: unknown): Promise<string> {
  const artifact = parseRouteTestArtifact(value);
  const directory = path.resolve(projectRoot, ROUTE_TEST_DIRECTORY);
  const destination = path.resolve(directory, `${artifact.id}.route-test.yaml`);
  if (path.dirname(destination) !== directory) throw new Error('route test path escaped the project directory');
  await fs.mkdir(directory, { recursive: true });
  const yaml = dump(artifact, { noRefs: true, lineWidth: 120, sortKeys: false, schema: JSON_SCHEMA });
  if (Buffer.byteLength(yaml, 'utf8') > MAX_ARTIFACT_BYTES) throw new Error('route test exceeds 1 MiB');
  const temporary = `${destination}.${process.pid}.${Date.now()}.tmp`;
  await fs.writeFile(temporary, yaml, { encoding: 'utf8', mode: 0o600 });
  try {
    await fs.rename(temporary, destination);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'EEXIST' && (error as NodeJS.ErrnoException).code !== 'EPERM') throw error;
    await fs.rm(destination, { force: true });
    await fs.rename(temporary, destination);
  } finally {
    await fs.rm(temporary, { force: true });
  }
  return destination;
}

export function validateRouteTestAgainstDocument(artifact: RouteTestArtifact, document: GraphDocument): void {
  const nodes = new Map(document.nodes.map((node) => [selectorKey(selectorForNode(node)), node]));
  resolveNode(artifact.target, nodes, 'target');
  const declaredInputs = new Map((document.inputs ?? []).flatMap((raw) => {
    const input = typeof raw === 'object' && raw !== null ? raw as Record<string, unknown> : undefined;
    return typeof input?.name === 'string' ? [[input.name, input]] as const : [];
  }));
  for (const name of Object.keys(artifact.inputs ?? {})) {
    const declaration = declaredInputs.get(name);
    if (!declaration) throw new Error(`Input ${name} is not declared by this runbook.`);
    if (declaration.type === 'secret') throw new Error(`Secret input ${name} cannot be saved in a route test.`);
  }
  for (const binding of artifact.step_responses ?? []) {
    const node = resolveNode(binding.at, nodes, `${binding.kind} result`);
    if (nodeKind(node) !== binding.kind) throw new Error(`${binding.kind} result is bound to a ${nodeKind(node)} step.`);
  }
  for (const binding of artifact.host_action_responses ?? []) {
    const node = resolveNode(binding.at, nodes, 'host action result');
    if (nodeKind(node) !== 'host_action') throw new Error(`Host action result is bound to a ${nodeKind(node)} step.`);
    const details = node.data.details as Record<string, unknown> | undefined;
    if (binding.capability && details?.capability !== binding.capability) throw new Error('Host action capability does not match the selected step.');
  }
  for (const binding of artifact.interaction_answers ?? []) {
    const node = resolveNode(binding.at, nodes, `${binding.kind} answer`);
    if (nodeKind(node) !== binding.kind) throw new Error(`${binding.kind} answer is bound to a ${nodeKind(node)} step.`);
    if (binding.kind === 'collector') {
      const details = node.data.details as Record<string, unknown> | undefined;
      const fields = Array.isArray(details?.fields) ? details.fields as Array<Record<string, unknown>> : [];
      for (const name of Object.keys(binding.values ?? {})) {
        const field = fields.find((candidate) => candidate.name === name);
        if (!field) throw new Error(`Collector field ${name} is not declared.`);
        if (field.ephemeral === true) throw new Error(`Collector field ${name} is ephemeral and cannot be saved in a route test.`);
      }
      validateCollectorValues(fields, binding.values ?? {});
    } else if (binding.kind === 'choice') {
      validateChoiceAnswer(node, binding.selected ?? []);
    } else if (binding.kind === 'decision') {
      const details = node.data.details as Record<string, unknown> | undefined;
      const labels = new Set((Array.isArray(details?.routes) ? details.routes : []).flatMap((raw) => {
        const route = raw as Record<string, unknown>;
        return typeof route.label === 'string' ? [route.label] : [];
      }));
      if (!binding.label || !labels.has(binding.label)) throw new Error('Decision answer is not a declared route.');
    }
  }
  for (const binding of artifact.test_approvals ?? []) resolveNode(binding.at, nodes, 'test approval');
}

export async function loadRouteTestArtifacts(
  projectRoot: string,
  runbook: string,
): Promise<{ artifacts: SavedRouteTest[]; warnings: string[] }> {
  const directory = path.resolve(projectRoot, ROUTE_TEST_DIRECTORY);
  let entries: string[];
  try {
    entries = (await fs.readdir(directory)).filter((name) => name.endsWith('.route-test.yaml')).sort();
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return { artifacts: [], warnings: [] };
    throw error;
  }
  const expectedRunbook = normalizedRelativePath(runbook);
  const artifacts: SavedRouteTest[] = [];
  const warnings: string[] = [];
  for (const name of entries) {
    const filePath = path.join(directory, name);
    try {
      const text = await fs.readFile(filePath, 'utf8');
      if (Buffer.byteLength(text, 'utf8') > MAX_ARTIFACT_BYTES) throw new Error('artifact exceeds 1 MiB');
      const artifact = parseRouteTestArtifact(load(text, { schema: JSON_SCHEMA }));
      if (artifact.runbook === expectedRunbook) artifacts.push({ filePath, artifact });
    } catch (error) {
      warnings.push(`${filePath}: ${error instanceof Error ? error.message : String(error)}`);
    }
  }
  return { artifacts, warnings };
}

function parseStepResponse(value: unknown, label: string): RouteTestStepResponse {
  const item = record(value, label);
  exactFields(item, ['at', 'kind', 'status', 'outcome', 'output', 'source', 'review'], label);
  const kind = stringValue(item.kind, `${label}.kind`, 32);
  if (kind !== 'cli' && kind !== 'tool') throw new Error(`${label}.kind must be cli or tool`);
  return {
    at: parseSelector(item.at, `${label}.at`), kind,
    status: stringValue(item.status, `${label}.status`, 64),
    ...(item.outcome === undefined ? {} : { outcome: stringValue(item.outcome, `${label}.outcome`, 64) }),
    ...(item.output === undefined || Object.keys(jsonRecord(item.output, `${label}.output`)).length === 0 ? {} : { output: jsonRecord(item.output, `${label}.output`) }),
    source: parseSource(item.source, `${label}.source`),
    review: parseReview(item.review, `${label}.review`),
  };
}

function parseHostResponse(value: unknown, label: string): RouteTestHostActionResponse {
  const item = record(value, label);
  exactFields(item, ['at', 'capability', 'response', 'source', 'review'], label);
  const response = record(item.response, `${label}.response`);
  exactFields(response, ['status', 'result'], `${label}.response`);
  const status = stringValue(response.status, `${label}.response.status`, 64);
  if (!['completed', 'failed', 'timed-out', 'execution-not-started', 'unsupported'].includes(status)) {
    throw new Error(`${label}.response.status is not supported`);
  }
  return {
    at: parseSelector(item.at, `${label}.at`),
    ...(item.capability === undefined ? {} : { capability: stringValue(item.capability, `${label}.capability`, 256) }),
    response: {
      status,
      ...(response.result === undefined || Object.keys(jsonRecord(response.result, `${label}.response.result`)).length === 0 ? {} : { result: jsonRecord(response.result, `${label}.response.result`) }),
    },
    source: parseSource(item.source, `${label}.source`),
    review: parseReview(item.review, `${label}.review`),
  };
}

function parseInteraction(value: unknown, label: string): RouteTestInteractionAnswer {
  const item = record(value, label);
  exactFields(item, ['at', 'kind', 'selected', 'label', 'values', 'source', 'review'], label);
  const kind = stringValue(item.kind, `${label}.kind`, 32);
  if (kind !== 'choice' && kind !== 'decision' && kind !== 'collector') throw new Error(`${label}.kind is not supported`);
  return {
    at: parseSelector(item.at, `${label}.at`), kind,
    ...(item.selected === undefined || arrayValue(item.selected, `${label}.selected`, 64).length === 0 ? {} : { selected: stringArray(item.selected, `${label}.selected`) }),
    ...(item.label === undefined ? {} : { label: stringValue(item.label, `${label}.label`, 4096) }),
    ...(item.values === undefined || Object.keys(jsonRecord(item.values, `${label}.values`)).length === 0 ? {} : { values: jsonRecord(item.values, `${label}.values`) }),
    source: parseSource(item.source, `${label}.source`, kind === 'collector'),
    review: parseReview(item.review, `${label}.review`),
  } as RouteTestInteractionAnswer;
}

function parseSelector(value: unknown, label: string): RouteTestSelector {
  const selector = record(value, label);
  exactFields(selector, ['call_path', 'step', 'phase', 'invocation', 'attempt'], label);
  const invocation = selector.invocation;
  if (!Number.isInteger(invocation) || (invocation as number) < 1) throw new Error(`${label}.invocation must be a positive integer`);
  if (selector.phase !== 'before' && selector.phase !== 'execute') throw new Error(`${label}.phase must be before or execute`);
  if (selector.attempt !== 1) throw new Error(`${label}.attempt must be 1`);
  return {
    ...(selector.call_path === undefined || arrayValue(selector.call_path, `${label}.call_path`, 64).length === 0
      ? {}
      : { call_path: stringArray(selector.call_path, `${label}.call_path`) }),
    step: stringValue(selector.step, `${label}.step`, 256), phase: selector.phase, invocation: invocation as number, attempt: 1,
  };
}

function parseReview(value: unknown, label: string): RouteTestReview {
  const review = record(value, label);
  exactFields(review, ['state', 'reviewed_by', 'reviewed_at', 'sensitivity_reviewed'], label);
  if (review.state !== 'draft' && review.state !== 'reviewed') throw new Error(`${label}.state must be draft or reviewed`);
  if (typeof review.sensitivity_reviewed !== 'boolean') throw new Error(`${label}.sensitivity_reviewed must be a boolean`);
  const result: RouteTestReview = { state: review.state, sensitivity_reviewed: review.sensitivity_reviewed };
  if (!result.sensitivity_reviewed) throw new Error(`${label} requires sensitivity review before persistence`);
  if (review.reviewed_by !== undefined) result.reviewed_by = stringValue(review.reviewed_by, `${label}.reviewed_by`, 256);
  if (review.reviewed_at !== undefined) {
    result.reviewed_at = stringValue(review.reviewed_at, `${label}.reviewed_at`, 64);
    if (Number.isNaN(Date.parse(result.reviewed_at))) throw new Error(`${label}.reviewed_at must be a timestamp`);
  }
  if (result.state === 'reviewed' && (!result.reviewed_by || !result.reviewed_at || !result.sensitivity_reviewed)) {
    throw new Error(`${label} requires reviewer, review time, and sensitivity review`);
  }
  return result;
}

function parseSource(value: unknown, label: string, requireAnswerDigest = false): RouteTestSource {
  const source = record(value, label);
  exactFields(source, ['kind', 'run_id', 'interaction_id', 'answer_digest', 'copied_at'], label);
  if (source.kind !== 'manual' && source.kind !== 'prior-run') throw new Error(`${label}.kind must be manual or prior-run`);
  const result = {
    kind: source.kind,
    ...optionalStrings(source, ['run_id', 'interaction_id', 'answer_digest', 'copied_at'], label),
  } as RouteTestSource;
  if (result.kind === 'prior-run' && (!result.run_id || !result.interaction_id || !result.copied_at)) {
    throw new Error(`${label} prior-run provenance requires run_id, interaction_id, and copied_at`);
  }
  if (result.kind === 'prior-run' && requireAnswerDigest && !result.answer_digest) {
    throw new Error(`${label} prior-run collector provenance requires answer_digest`);
  }
  return result;
}

function parseStringMap(value: unknown, label: string): Record<string, string> {
  const source = record(value, label);
  if (Object.keys(source).length > 64) throw new Error(`${label} exceeds 64 entries`);
  return Object.fromEntries(Object.entries(source).map(([key, item]) => [
    stringValue(key, `${label} key`, 256), stringValue(item, `${label}.${key}`, 64 * 1024),
  ]));
}

function optionalStrings(source: Record<string, unknown>, keys: string[], label: string): Record<string, string> {
  return Object.fromEntries(keys.flatMap((key) => source[key] === undefined
    ? [] : [[key, stringValue(source[key], `${label}.${key}`, 4096)]]));
}

function normalizedRelativePath(value: string): string {
  const normalized = value.replaceAll('\\', '/').replace(/^\.\//, '');
  if (!normalized || path.posix.isAbsolute(normalized) || normalized.startsWith('//') || normalized.includes(':') || normalized.split('/').includes('..')) {
    throw new Error('route test runbook must be a project-relative path');
  }
  return normalized;
}

function selectorForNode(node: GraphNode): RouteTestSelector {
  const callPath = Array.isArray(node.data.call_path)
    ? node.data.call_path.filter((item): item is string => typeof item === 'string')
    : [];
  const step = typeof node.data.step_id === 'string' && node.data.step_id ? node.data.step_id : node.id;
  return { ...(callPath.length ? { call_path: callPath } : {}), step, phase: 'execute', invocation: 1, attempt: 1 };
}

function selectorKey(selector: RouteTestSelector): string {
  return JSON.stringify([selector.call_path ?? [], selector.step]);
}

function resolveNode(selector: RouteTestSelector, nodes: Map<string, GraphNode>, label: string): GraphNode {
  const node = nodes.get(selectorKey(selector));
  if (!node) throw new Error(`Route test ${label} does not resolve in the current graph.`);
  return node;
}

function nodeKind(node: GraphNode): string {
  return typeof node.data.kind === 'string' ? node.data.kind : '';
}

function exactFields(value: Record<string, unknown>, allowed: string[], label: string): void {
  const accepted = new Set(allowed);
  for (const key of Object.keys(value)) if (!accepted.has(key)) throw new Error(`${label} has unknown field ${key}`);
}

function record(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new Error(`${label} must be an object`);
  return value as Record<string, unknown>;
}

function jsonRecord(value: unknown, label: string): Record<string, unknown> {
  const result = record(value, label);
  boundedJSON(result, label);
  return result;
}

function boundedJSON(value: unknown, label: string): void {
  let encoded: string;
  try { encoded = JSON.stringify(value); } catch { throw new Error(`${label} must be JSON-compatible`); }
  if (encoded === undefined || Buffer.byteLength(encoded, 'utf8') > MAX_ARTIFACT_BYTES) throw new Error(`${label} exceeds 1 MiB`);
}

function stringValue(value: unknown, label: string, maximum: number): string {
  if (typeof value !== 'string' || value.length === 0 || Buffer.byteLength(value, 'utf8') > maximum) {
    throw new Error(`${label} must be a non-empty string no longer than ${maximum} bytes`);
  }
  return value;
}

function stringArray(value: unknown, label: string): string[] {
  return arrayValue(value, label, 64).map((item, index) => stringValue(item, `${label}[${index}]`, 256));
}

function arrayValue(value: unknown, label: string, maximum: number): unknown[] {
  if (!Array.isArray(value) || value.length > maximum) throw new Error(`${label} must be an array of at most ${maximum} items`);
  return value;
}

function parseApproval(value: unknown, label: string): RouteTestApproval {
  const item = record(value, label);
  exactFields(item, ['at', 'approved', 'approver', 'source', 'review'], label);
  if (typeof item.approved !== 'boolean') throw new Error(`${label}.approved must be a boolean`);
  return {
    at: parseSelector(item.at, `${label}.at`),
    approved: item.approved,
    approver: stringValue(item.approver, `${label}.approver`, 256),
    source: parseSource(item.source, `${label}.source`),
    review: parseReview(item.review, `${label}.review`),
  };
}

function rejectSensitiveArtifactData(artifact: RouteTestArtifact): void {
  for (const name of Object.keys(artifact.inputs ?? {})) if (sensitiveKey(name)) throw new Error(`Sensitive input key ${name} cannot be persisted.`);
  for (const binding of artifact.step_responses ?? []) validatePersistedValue(binding.output, 'Saved step result');
  for (const binding of artifact.host_action_responses ?? []) validatePersistedValue(binding.response.result, 'Saved host result');
  for (const binding of artifact.interaction_answers ?? []) validatePersistedValue(binding.values, 'Saved interaction answer');
}

function validatePersistedValue(value: unknown, label: string): void {
  const encoded = JSON.stringify(value);
  if (encoded !== undefined && Buffer.byteLength(encoded, 'utf8') > 64 * 1024) throw new Error(`${label} exceeds 64 KiB.`);
  const state = { nodes: 0 };
  rejectSensitiveValue(value, label, 1, state);
}

function rejectSensitiveValue(value: unknown, label: string, depth: number, state: { nodes: number }): void {
  if (depth > 8) throw new Error(`${label} exceeds depth 8.`);
  state.nodes += 1;
  if (state.nodes > 512) throw new Error(`${label} exceeds 512 values.`);
  if (Array.isArray(value)) {
    if (value.length > 64) throw new Error(`${label} contains an array over 64 items.`);
    value.forEach((item) => rejectSensitiveValue(item, label, depth + 1, state));
    return;
  }
  if (typeof value === 'string') {
    if (Buffer.byteLength(value, 'utf8') > 4096) throw new Error(`${label} contains a string over 4096 bytes.`);
    if (/\r|\n/.test(value)) throw new Error(`${label} contains multiline raw output; store an evidence reference instead.`);
    return;
  }
  if (typeof value === 'number' && !Number.isFinite(value)) throw new Error(`${label} contains a non-finite number.`);
  if (typeof value !== 'object' || value === null) return;
  const entries = Object.entries(value as Record<string, unknown>);
  if (entries.length > 64) throw new Error(`${label} contains an object over 64 properties.`);
  for (const [key, item] of entries) {
    if (sensitiveKey(key)) throw new Error(`Sensitive result key ${key} cannot be persisted.`);
    rejectSensitiveValue(item, label, depth + 1, state);
  }
}

function sensitiveKey(key: string): boolean {
  const tokens = key
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/([A-Z])([A-Z][a-z])/g, '$1 $2')
    .split(/[^A-Za-z0-9]+/)
    .filter(Boolean)
    .map((token) => token.toLowerCase());
  return tokens.some((token) => ['secret', 'password', 'passwd', 'token', 'credential', 'key', 'authorization', 'auth'].includes(token));
}

function validateChoiceAnswer(node: GraphNode, selected: string[]): void {
  const details = node.data.details as Record<string, unknown> | undefined;
  const allowed = new Set((Array.isArray(details?.options) ? details.options : []).flatMap((raw) => {
    const option = raw as Record<string, unknown>;
    return typeof option.value === 'string' ? [option.value] : [];
  }));
  const multiple = details?.multiple === true;
  const minimum = typeof details?.min === 'number' ? details.min : 0;
  const maximum = typeof details?.max === 'number' && details.max > 0 ? details.max : Number.MAX_SAFE_INTEGER;
  if ((!multiple && selected.length > 1) || selected.length < minimum || selected.length > maximum || selected.some((value) => !allowed.has(value))) {
    throw new Error('Choice answer does not match the declared options or selection limits.');
  }
}

function validateCollectorValues(fields: Array<Record<string, unknown>>, values: Record<string, unknown>): void {
  for (const field of fields) {
    if (typeof field.name !== 'string') continue;
    const value = values[field.name];
    const empty = value === undefined || value === null || value === '' || (Array.isArray(value) && value.length === 0);
    if (field.required === true && empty) throw new Error(`${String(field.label ?? field.name)} is required.`);
    if (empty) continue;
    const multiple = field.multiple === true;
    const items = multiple ? (Array.isArray(value) ? value : undefined) : [value];
    if (!items) throw new Error(`${String(field.label ?? field.name)} must be a list.`);
    const options = new Set((Array.isArray(field.options) ? field.options : []).flatMap((raw) => {
      const option = raw as Record<string, unknown>;
      return typeof option.value === 'string' ? [option.value] : [];
    }));
    for (const item of items) {
      if (field.type === 'boolean' && typeof item !== 'boolean') throw new Error(`${String(field.label ?? field.name)} must be true or false.`);
      if ((field.type === 'number' || field.type === 'integer') && (typeof item !== 'number' && (typeof item !== 'string' || item.trim() === '' || !Number.isFinite(Number(item))))) throw new Error(`${String(field.label ?? field.name)} must be numeric.`);
      if (field.type === 'integer' && !Number.isInteger(Number(item))) throw new Error(`${String(field.label ?? field.name)} must be an integer.`);
      if (options.size > 0 && (typeof item !== 'string' || !options.has(item))) throw new Error(`${String(field.label ?? field.name)} must use a declared option.`);
    }
  }
}

export function stampRouteTestResultDigest(value: Record<string, unknown>): Record<string, unknown> {
  const lastResult = record(value.last_result, 'last_result');
  const conditions = { ...value };
  delete conditions.last_result;
  const parsedConditions = parseRouteTestArtifact(conditions);
  return {
    ...parsedConditions,
    last_result: { ...lastResult, conditions_digest: routeTestConditionsDigest(parsedConditions as unknown as Record<string, unknown>) },
  };
}

function routeTestConditionsDigest(value: Record<string, unknown>): string {
  const conditions = { ...value };
  delete conditions.last_result;
  const canonical = JSON.stringify(canonicalValue(conditions))
    .replaceAll('&', '\\u0026')
    .replaceAll('<', '\\u003c')
    .replaceAll('>', '\\u003e')
    .replaceAll('\u2028', '\\u2028')
    .replaceAll('\u2029', '\\u2029');
  return `sha256:${createHash('sha256').update(canonical).digest('hex')}`;
}

function canonicalValue(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(canonicalValue);
  if (typeof value !== 'object' || value === null) return value;
  return Object.fromEntries(Object.keys(value as Record<string, unknown>).sort().flatMap((key) => {
    const item = (value as Record<string, unknown>)[key];
    if (item === undefined) return [];
    return [[key, canonicalValue(item)]];
  }));
}