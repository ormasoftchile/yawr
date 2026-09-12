import { decodeEnvelope, type PresentationEnvelope } from './presentationProtocol';
import { decodeExpressionPresentation, type ExpressionPresentation } from './expressionPresentationProtocol';

export interface NamedDetailValue {
  [key: string]: unknown;
  name: string;
  value?: unknown;
  redacted?: boolean;
}

export interface CaptureDetails {
  [key: string]: unknown;
  name: string;
  source: string;
  default?: unknown;
  has_default?: boolean;
}

export interface CommonStepDetails {
  [key: string]: unknown;
  subtitle?: string;
  when?: string;
  timeout?: string;
  delay?: string;
  retry?: {
    max: number;
    interval?: string;
    backoff?: string;
    max_interval?: string;
    jitter?: boolean;
  };
  on_error?: string;
  continue_on_fail?: boolean;
  scope?: string;
  exports?: string[];
  captures?: CaptureDetails[];
  contract?: {
    effects?: string[];
    reads?: string[];
    writes?: string[];
    idempotent?: boolean;
    deterministic?: boolean;
  };
  evidence?: Array<{ kind: string; name: string; label?: string; items?: string[] }>;
}

interface DetailsBase<K extends string> {
  [key: string]: unknown;
  kind: K;
  role?: 'technical' | 'operator';
  common?: CommonStepDetails;
  code_presentation?: PresentationEnvelope;
  expression_presentation?: ExpressionPresentation;
}

export type StepDetails =
  | (DetailsBase<'cli'> & { command?: string; args?: string[]; script?: unknown; shell?: string; workdir?: string; env_names?: string[]; stdin?: boolean })
  | (DetailsBase<'tool'> & { tool?: string; action?: string; version?: string; arguments?: NamedDetailValue[] })
  | (DetailsBase<'include'> & { reference?: string; dynamic?: boolean; resolve_from?: string; on_not_found?: string; expand?: string; bindings?: NamedDetailValue[]; stop_if?: string[]; steps?: number })
  | (DetailsBase<'choice'> & { prompt?: string; variable?: string; default?: unknown; multiple?: boolean; min?: number; max?: number; options?: Array<{ label: string; value: string; hint?: string }> })
  | (DetailsBase<'decision'> & { prompt?: string; variable?: string; routes?: Array<{ label: string; hint?: string; goto?: string; runbook?: string }> })
  | (DetailsBase<'collector'> & { prompt?: string; fields?: Array<Record<string, unknown>> })
  | (DetailsBase<'host_action'> & { capability?: string; request?: NamedDetailValue[] })
  | (DetailsBase<'branch'> & { arms?: Array<{ label?: string; condition?: string; else?: boolean; steps: number }> })
  | (DetailsBase<'approve'> & { roles?: string[]; pool?: string[]; required?: number; timeout?: string; on_timeout?: string; timezone?: string; business_calendar?: string; escalate_to?: string[] })
  | (DetailsBase<'assert'> & { assertions?: Array<{ type: string; subject: string; expected?: string; path?: string }> })
  | (DetailsBase<'wait_for_event'> & { source?: string; event_id?: string; filter?: NamedDetailValue[]; payload_schema?: string; on_timeout?: string })
  | (DetailsBase<'display'> & { content?: string; format?: string })
  | (DetailsBase<'end'> & { category?: string; code?: string })
  | (DetailsBase<'compensate'> & { on?: string; steps?: number })
  | DetailsBase<'noop'>
  | (DetailsBase<'assign'> & { assign?: NamedDetailValue[] })
  | DetailsBase<'results'>
  | (DetailsBase<'iterate'> & { over?: string; as?: string; max?: number; until?: string; collect?: NamedDetailValue[]; concurrency?: number; steps?: number })
  | (DetailsBase<'parallel'> & { branches?: number; branch_labels?: string[]; wait_for?: string; on_failure?: string })
  | DetailsBase<'extension'>;

const FIELDS_BY_KIND: Record<string, readonly string[]> = {
  cli: ['command', 'args', 'script', 'shell', 'workdir', 'env_names', 'stdin'],
  tool: ['tool', 'action', 'version', 'arguments'],
  include: ['reference', 'dynamic', 'resolve_from', 'on_not_found', 'expand', 'bindings', 'stop_if', 'steps'],
  choice: ['prompt', 'variable', 'default', 'multiple', 'min', 'max', 'options'],
  decision: ['prompt', 'variable', 'routes'],
  collector: ['prompt', 'fields'],
  host_action: ['capability', 'request'],
  branch: ['arms'],
  approve: ['roles', 'pool', 'required', 'timeout', 'on_timeout', 'timezone', 'business_calendar', 'escalate_to'],
  assert: ['assertions'],
  wait_for_event: ['source', 'event_id', 'filter', 'payload_schema', 'on_timeout'],
  display: ['content', 'format'],
  end: ['category', 'code'],
  compensate: ['on', 'steps'],
  noop: [],
  assign: ['assign'],
  results: [],
  iterate: ['over', 'as', 'max', 'until', 'collect', 'concurrency', 'steps'],
  parallel: ['branches', 'branch_labels', 'wait_for', 'on_failure'],
  extension: [],
};

const KNOWN_DETAIL_FIELDS = new Set(Object.values(FIELDS_BY_KIND).flat());
const STRING_ARRAY_FIELDS = new Set(['args', 'env_names', 'stop_if', 'roles', 'pool', 'escalate_to', 'branch_labels']);
const STRING_FIELDS = new Set([
  'command', 'shell', 'workdir', 'tool', 'action', 'version', 'reference', 'resolve_from', 'on_not_found',
  'expand', 'prompt', 'variable', 'capability', 'timeout', 'on_timeout', 'timezone', 'business_calendar',
  'source', 'event_id', 'payload_schema', 'content', 'format', 'category', 'code', 'on', 'over', 'as', 'until',
  'wait_for', 'on_failure',
]);
const NUMBER_FIELDS = new Set(['min', 'max', 'required', 'steps', 'concurrency', 'branches']);
const BOOLEAN_FIELDS = new Set(['stdin', 'dynamic', 'multiple']);

export function parseStepDetails(value: unknown, nodeKind: string, label: string, typed = false): StepDetails {
  const details = plainRecord(value, `${label}.details`);
  if (typeof details.kind !== 'string' || details.kind.length === 0) {
    throw new Error(`${label}.details.kind must be a non-empty string`);
  }
  if (details.kind !== nodeKind) {
    throw new Error(`${label}.details kind must match node kind`);
  }
  if (details.role !== undefined && details.role !== 'technical' && details.role !== 'operator') throw new Error('invalid detail role');
  if (typed && (nodeKind === 'assign' || nodeKind === 'results') &&
      details.role !== (nodeKind === 'assign' ? 'technical' : 'operator')) throw new Error('invalid typed operation role');
  const kindFields = FIELDS_BY_KIND[details.kind];
  if (!kindFields) throw new Error(`${label}.details kind is unsupported`);
  const allowed = new Set(kindFields);
  for (const [key, field] of Object.entries(details)) {
    if (key === 'kind' || key === 'common') continue;
    if (KNOWN_DETAIL_FIELDS.has(key) && !allowed.has(key)) {
      throw new Error(`${label}.details.${key} is not valid for ${details.kind}`);
    }
    if (!allowed.has(key)) continue;
    const fieldLabel = `${label}.details.${key}`;
    if (STRING_ARRAY_FIELDS.has(key)) validateStringArray(field, fieldLabel);
    if (STRING_FIELDS.has(key)) validateOptionalString(field, fieldLabel);
    if (NUMBER_FIELDS.has(key)) validateOptionalNumber(field, fieldLabel);
    if (BOOLEAN_FIELDS.has(key)) validateOptionalBoolean(field, fieldLabel);
  }
  if (details.common !== undefined) validateCommon(details.common, label);
  if (details.code_presentation !== undefined) details.code_presentation = decodeEnvelope(details.code_presentation);
  validateDetailObjects(details, label);
  if (details.kind === 'assign' && details.assign !== undefined) {
    validateRecordArray(details.assign, `${label}.details.assign`, (write, itemLabel) => {
      validateRequiredString(write.name, `${itemLabel}.name`);
      if (!Object.prototype.hasOwnProperty.call(write, 'value')) throw new Error(`${itemLabel}.value is required`);
      if (typed && (Object.keys(write).some(key => !['name', 'value', 'value_present'].includes(key)) ||
          typeof write.value_present !== 'boolean')) throw new Error('invalid assignment presence');
    });
  }
  if (details.expression_presentation !== undefined) {
    const expressions = decodeExpressionPresentation(details.expression_presentation);
    if (expressions) details.expression_presentation = expressions;
    else delete details.expression_presentation;
  }
  return details as unknown as StepDetails;
}

function validateCommon(value: unknown, label: string): void {
  const common = plainRecord(value, `${label}.details.common`);
  const commonLabel = `${label}.details.common`;
  for (const key of ['subtitle', 'when', 'timeout', 'delay', 'on_error', 'scope']) {
    validateOptionalString(common[key], `${commonLabel}.${key}`);
  }
  validateOptionalBoolean(common.continue_on_fail, `${commonLabel}.continue_on_fail`);
  if (common.exports !== undefined) validateStringArray(common.exports, `${commonLabel}.exports`);
  if (common.retry !== undefined) {
    const retry = plainRecord(common.retry, `${commonLabel}.retry`);
    validateRequiredNumber(retry.max, `${commonLabel}.retry.max`);
    for (const key of ['interval', 'backoff', 'max_interval']) {
      validateOptionalString(retry[key], `${commonLabel}.retry.${key}`);
    }
    validateOptionalBoolean(retry.jitter, `${commonLabel}.retry.jitter`);
  }
  if (common.captures !== undefined) {
    validateRecordArray(common.captures, `${commonLabel}.captures`, (capture, itemLabel) => {
      validateRequiredString(capture.name, `${itemLabel}.name`);
      validateRequiredString(capture.source, `${itemLabel}.source`);
      validateOptionalBoolean(capture.has_default, `${itemLabel}.has_default`);
    });
  }
  if (common.contract !== undefined) {
    const contract = plainRecord(common.contract, `${commonLabel}.contract`);
    for (const key of ['effects', 'reads', 'writes']) {
      if (contract[key] !== undefined) validateStringArray(contract[key], `${commonLabel}.contract.${key}`);
    }
    validateOptionalBoolean(contract.idempotent, `${commonLabel}.contract.idempotent`);
    validateOptionalBoolean(contract.deterministic, `${commonLabel}.contract.deterministic`);
  }
  if (common.evidence !== undefined) {
    validateRecordArray(common.evidence, `${commonLabel}.evidence`, (evidence, itemLabel) => {
      validateRequiredString(evidence.kind, `${itemLabel}.kind`);
      validateRequiredString(evidence.name, `${itemLabel}.name`);
      validateOptionalString(evidence.label, `${itemLabel}.label`);
      if (evidence.items !== undefined) validateStringArray(evidence.items, `${itemLabel}.items`);
    });
  }
}

function validateDetailObjects(details: Record<string, unknown>, label: string): void {
  const detailsLabel = `${label}.details`;
  for (const key of ['arguments', 'bindings', 'request', 'filter', 'collect']) {
    if (details[key] === undefined) continue;
    validateRecordArray(details[key], `${detailsLabel}.${key}`, (entry, itemLabel) => {
      validateRequiredString(entry.name, `${itemLabel}.name`);
      validateOptionalBoolean(entry.redacted, `${itemLabel}.redacted`);
      if (entry.redacted === true) delete entry.value;
    });
  }
  if (details.options !== undefined) validateOptions(details.options, `${detailsLabel}.options`);
  if (details.routes !== undefined) {
    validateRecordArray(details.routes, `${detailsLabel}.routes`, (route, itemLabel) => {
      validateRequiredString(route.label, `${itemLabel}.label`);
      for (const key of ['hint', 'goto', 'runbook']) validateOptionalString(route[key], `${itemLabel}.${key}`);
    });
  }
  if (details.fields !== undefined) {
    validateRecordArray(details.fields, `${detailsLabel}.fields`, (field, itemLabel) => {
      validateRequiredString(field.name, `${itemLabel}.name`);
      validateRequiredString(field.type, `${itemLabel}.type`);
      for (const key of ['label', 'when', 'hint', 'from_step']) validateOptionalString(field[key], `${itemLabel}.${key}`);
      for (const key of ['required', 'has_default', 'multiple', 'multiline', 'ephemeral']) validateOptionalBoolean(field[key], `${itemLabel}.${key}`);
      if (field.options !== undefined) validateOptions(field.options, `${itemLabel}.options`);
      if (field.options_from !== undefined) {
        const optionsFrom = plainRecord(field.options_from, `${itemLabel}.options_from`);
        validateRequiredString(optionsFrom.provider, `${itemLabel}.options_from.provider`);
        validateRequiredString(optionsFrom.field, `${itemLabel}.options_from.field`);
      }
      if (field.validation !== undefined) {
        const validation = plainRecord(field.validation, `${itemLabel}.validation`);
        for (const key of ['min_length', 'max_length', 'step']) validateOptionalNumber(validation[key], `${itemLabel}.validation.${key}`);
        for (const key of ['pattern', 'format']) validateOptionalString(validation[key], `${itemLabel}.validation.${key}`);
      }
    });
  }
  if (details.arms !== undefined) {
    validateRecordArray(details.arms, `${detailsLabel}.arms`, (arm, itemLabel) => {
      validateOptionalString(arm.label, `${itemLabel}.label`);
      validateOptionalString(arm.condition, `${itemLabel}.condition`);
      validateOptionalBoolean(arm.else, `${itemLabel}.else`);
      validateRequiredNumber(arm.steps, `${itemLabel}.steps`);
    });
  }
  if (details.assertions !== undefined) {
    validateRecordArray(details.assertions, `${detailsLabel}.assertions`, (assertion, itemLabel) => {
      validateRequiredString(assertion.type, `${itemLabel}.type`);
      validateRequiredString(assertion.subject, `${itemLabel}.subject`);
      validateOptionalString(assertion.expected, `${itemLabel}.expected`);
      validateOptionalString(assertion.path, `${itemLabel}.path`);
    });
  }
}

function validateOptions(value: unknown, label: string): void {
  validateRecordArray(value, label, (option, itemLabel) => {
    validateRequiredString(option.label, `${itemLabel}.label`);
    validateRequiredString(option.value, `${itemLabel}.value`);
    validateOptionalString(option.hint, `${itemLabel}.hint`);
  });
}

function validateRecordArray(
  value: unknown,
  label: string,
  validateItem: (item: Record<string, unknown>, itemLabel: string) => void,
): void {
  if (!Array.isArray(value)) throw new Error(`${label} must be an array`);
  value.forEach((item, index) => {
    const itemLabel = `${label}[${index}]`;
    validateItem(plainRecord(item, itemLabel), itemLabel);
  });
}

function validateStringArray(value: unknown, label: string): void {
  if (!Array.isArray(value)) throw new Error(`${label} must be an array`);
  value.forEach((item, index) => validateRequiredString(item, `${label}[${index}]`));
}

function validateRequiredString(value: unknown, label: string): void {
  if (typeof value !== 'string') throw new Error(`${label} must be a string`);
}

function validateOptionalString(value: unknown, label: string): void {
  if (value !== undefined) validateRequiredString(value, label);
}

function validateRequiredNumber(value: unknown, label: string): void {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    throw new Error(`${label} must be a finite number`);
  }
}

function validateOptionalNumber(value: unknown, label: string): void {
  if (value !== undefined) validateRequiredNumber(value, label);
}

function validateOptionalBoolean(value: unknown, label: string): void {
  if (value !== undefined && typeof value !== 'boolean') {
    throw new Error(`${label} must be a boolean`);
  }
}

function plainRecord(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`);
  }
  return value as Record<string, unknown>;
}
