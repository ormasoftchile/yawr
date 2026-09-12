export interface CollectorFieldOption {
  value: string;
  label?: string;
  display_label?: string;
  display_value?: string;
}

export interface CollectorFieldDefinition {
  name: string;
  display_name?: string;
  type: string;
  label?: string;
  required?: boolean;
  options?: readonly CollectorFieldOption[];
  multiple?: boolean;
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

function fieldLabel(field: CollectorFieldDefinition): string {
  return field.label ?? field.display_name ?? field.name;
}

export function collectorInputType(type: string): string {
  if (type === 'integer' || type === 'number') return 'number';
  if (type === 'date') return 'date';
  if (type === 'datetime') return 'datetime-local';
  if (type === 'secret' || type === 'password') return 'password';
  if (type === 'email') return 'email';
  if (type === 'url') return 'url';
  return 'text';
}

export function normalizeCollectorValues(
  fields: readonly CollectorFieldDefinition[],
  values: Readonly<Record<string, unknown>>,
): Record<string, unknown> {
  const result: Record<string, unknown> = {};
  for (const field of fields) {
    const value = values[field.name];
    const empty = value === undefined || value === null || value === '' || (Array.isArray(value) && value.length === 0);
    if (field.required && empty) {
      throw new Error(`${fieldLabel(field)} is required.`);
    }
    if (empty) continue;
    const rawValues = field.multiple ? (Array.isArray(value) ? value : [value]) : [value];
    const normalized = rawValues.map((rawValue) => normalizeCollectorFieldValue(field, rawValue));
    result[field.name] = field.multiple ? normalized : normalized[0];
  }
  return result;
}

function normalizeCollectorFieldValue(field: CollectorFieldDefinition, value: unknown): unknown {
  let normalized: unknown;
  if (field.type === 'number') {
    const number = Number(value);
    if (!Number.isFinite(number)) throw new Error(`${fieldLabel(field)} must be a number.`);
    normalized = number;
  } else if (field.type === 'integer') {
    const number = Number(value);
    if (!Number.isInteger(number)) throw new Error(`${fieldLabel(field)} must be an integer.`);
    normalized = number;
  } else if (field.type === 'boolean') {
    if (typeof value !== 'boolean') throw new Error(`${fieldLabel(field)} must be true or false.`);
    normalized = value;
  } else {
    if (typeof value !== 'string') throw new Error(`${fieldLabel(field)} must be text.`);
    normalized = value;
  }

  if (field.options?.length && typeof normalized === 'string' && !field.options.some((option) => option.value === normalized)) {
    throw new Error(`${fieldLabel(field)} must use a declared option.`);
  }
  const validation = field.validation;
  if (typeof normalized === 'string' && validation) {
    if (validation.min_length !== undefined && normalized.length < validation.min_length) throw new Error(`${fieldLabel(field)} is too short.`);
    if (validation.max_length !== undefined && normalized.length > validation.max_length) throw new Error(`${fieldLabel(field)} is too long.`);
    if (validation.pattern) {
      let pattern: RegExp;
      try { pattern = new RegExp(validation.pattern); } catch { throw new Error(`${fieldLabel(field)} has an invalid validation pattern.`); }
      if (!pattern.test(normalized)) throw new Error(`${fieldLabel(field)} has an invalid format.`);
    }
  }
  if (typeof normalized === 'number' && validation) {
    if (validation.min !== undefined && normalized < validation.min) throw new Error(`${fieldLabel(field)} is below the minimum.`);
    if (validation.max !== undefined && normalized > validation.max) throw new Error(`${fieldLabel(field)} is above the maximum.`);
    if (validation.step !== undefined && validation.step !== 0 &&
        Math.abs(((normalized - (validation.min ?? 0)) / validation.step) - Math.round((normalized - (validation.min ?? 0)) / validation.step)) > 1e-9) {
      throw new Error(`${fieldLabel(field)} does not match the required step.`);
    }
  }
  return normalized;
}

export function formatCollectorReviewValue(field: CollectorFieldDefinition, value: unknown): string {
  if (field.type === 'secret' || field.type === 'password') return '<redacted>';
  if (field.type === 'boolean') return value ? 'Yes' : 'No';
  const option = field.options?.find((candidate) => candidate.value === value);
  if (option) return option.label ?? option.display_label ?? option.display_value ?? option.value;
  if (Array.isArray(value)) return value.join(', ');
  if (value === undefined || value === null || value === '') return 'Not set';
  return String(value);
}

export function formatMultipleCollectorText(value: unknown): string {
  return Array.isArray(value) ? value.map(String).join('\n') : String(value ?? '');
}

export function parseMultipleCollectorText(value: string): string[] {
  return value.split(/\r?\n/).filter((item) => item.length > 0);
}