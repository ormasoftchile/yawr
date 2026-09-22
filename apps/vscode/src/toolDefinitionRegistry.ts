// toolDefinitionRegistry.ts — builds the MCP bridge registry from workspace
// *.tool.yaml definitions at runtime, so the registered MCP tool name and the
// declared output contract come from the tool definitions rather than from
// extension source code.
//
// vscode_tool precedence (matching core):
//   1. action-level vscode_tool
//   2. transport-level vscode_tool
//   3. logical tool name

import * as fs from 'fs';
import * as path from 'path';
import * as yaml from 'js-yaml';
import type { FieldType, OutputFieldSpec, ToolActionSpec } from './mcpBridge';

// ─── Raw YAML shapes ─────────────────────────────────────────────────────────

interface RawOutputField {
  type?: unknown;
  required?: unknown;
  description?: unknown;
  [key: string]: unknown;
}

interface RawAction {
  name?: unknown;
  vscode_tool?: unknown;
  outputs?: unknown;
  [key: string]: unknown;
}

interface RawTransport {
  mode?: unknown;
  vscode_tool?: unknown;
  [key: string]: unknown;
}

interface RawToolDef {
  meta?: unknown;
  transport?: unknown;
  actions?: unknown;
  [key: string]: unknown;
}

interface RawPackageRequirement {
  package?: unknown;
  path?: unknown;
}

interface RawProjectConfig {
  apiVersion?: unknown;
  requires?: unknown;
  'tool-paths'?: unknown;
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

const VALID_FIELD_TYPES = new Set<string>(['string', 'number', 'boolean', 'object', 'array']);

function toFieldType(v: unknown): FieldType | undefined {
  return typeof v === 'string' && VALID_FIELD_TYPES.has(v) ? (v as FieldType) : undefined;
}

function buildOutputFields(
  raw: unknown,
): Record<string, OutputFieldSpec> {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return {};
  const map = raw as Record<string, unknown>;
  const result: Record<string, OutputFieldSpec> = {};
  for (const [field, def] of Object.entries(map)) {
    if (!def || typeof def !== 'object' || Array.isArray(def)) continue;
    const d = def as RawOutputField;
    const type = toFieldType(d.type);
    if (!type) continue;
    // required defaults to true when not explicitly set to false
    const required = d.required !== false;
    result[field] = { type, required };
  }
  return result;
}

/**
 * Recursively collect all *.tool.yaml file paths under dir.
 */
export function findToolYamls(dir: string): string[] {
  const results: string[] = [];
  let entries: fs.Dirent[];
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return results;
  }
  for (const entry of entries) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      results.push(...findToolYamls(full));
    } else if (entry.isFile() && (entry.name.endsWith('.tool.yaml') || entry.name.endsWith('.yawt'))) {
      results.push(full);
    }
  }
  return results;
}

/**
 * Build a registry of (tool/action) → ToolActionSpec by reading all
 * *.tool.yaml files found recursively under dir.
 *
 * Only tool definitions with transport.mode set to 'vscode-mcp' are included.
 * The registry reads the current tool shape:
 *
 *   1. meta.name identifies the tool.
 *   2. actions is a sequence whose entries declare name.
 *   3. transport.mode identifies the transport.
 *
 * registeredName resolution order (matching core):
 *   1. action-level  vscode_tool
 *   2. transport-level vscode_tool
 *   3. logical tool name
 */
export function buildRegistryFromDir(dir: string): Record<string, ToolActionSpec> {
  const registry: Record<string, ToolActionSpec> = {};
  for (const file of findToolYamls(dir)) {
    let raw: unknown;
    try {
      raw = yaml.load(fs.readFileSync(file, 'utf8'));
    } catch {
      continue;
    }
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) continue;
    const def = raw as RawToolDef;

    const meta = def.meta as { name?: unknown } | undefined;
    const toolName = meta && typeof meta.name === 'string' ? meta.name : undefined;
    if (!toolName) continue;

    const transport = def.transport as RawTransport | undefined;
    if (!transport) continue;

    if (transport.mode !== 'vscode-mcp') continue;

    const transportVscodeTool = typeof transport.vscode_tool === 'string'
      ? transport.vscode_tool
      : undefined;

    if (!Array.isArray(def.actions)) continue;
    const actionEntries: Array<[string, RawAction]> = (def.actions as RawAction[])
      .filter((action) => typeof action.name === 'string' && action.name)
      .map((action) => [action.name as string, action]);

    for (const [actionName, rawAction] of actionEntries) {
      const logicalKey = `${toolName}/${actionName}`;
      const actionVscodeTool = typeof rawAction.vscode_tool === 'string'
        ? rawAction.vscode_tool
        : undefined;

      // Precedence: action-level > transport-level > logical tool name
      const registeredName: string =
        actionVscodeTool ?? transportVscodeTool ?? toolName;

      const outputFields = buildOutputFields(rawAction.outputs);
      registry[logicalKey] = { registeredName, outputFields };
    }
  }
  return registry;
}

export function buildRegistryForRun(
  projectRoot: string,
  packageMapPath?: string,
): Record<string, ToolActionSpec> {
  const registry = buildRegistryFromDir(projectRoot);
  const project = readProjectConfig(path.join(projectRoot, '.yawr', 'config.yaml'), true);
  const override = packageMapPath ? readProjectConfig(packageMapPath, false) : undefined;
  const requirements = new Map<string, string>();
  for (const requirement of project?.requires ?? []) {
    requirements.set(requirement.package, requirement.path);
  }
  for (const requirement of override?.requires ?? []) {
    requirements.set(requirement.package, requirement.path);
  }
  const roots = [
    ...(project?.toolPaths ?? []),
    ...(override?.toolPaths ?? []),
    ...requirements.values(),
  ];
  for (const root of roots) {
    Object.assign(registry, buildRegistryFromDir(path.resolve(projectRoot, root)));
  }
  return registry;
}

function readProjectConfig(
  filePath: string,
  optional: boolean,
): { requires: Array<{ package: string; path: string }>; toolPaths: string[] } | undefined {
  let content: string;
  try {
    content = fs.readFileSync(filePath, 'utf8');
  } catch (error) {
    if (optional && (error as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
    throw error;
  }
  const raw = yaml.load(content) as RawProjectConfig | undefined;
  if (!raw || typeof raw !== 'object' || raw.apiVersion !== 'yawr.config/v1') {
    throw new Error(`${filePath}: expected apiVersion yawr.config/v1`);
  }
  const requires = Array.isArray(raw.requires)
    ? raw.requires.flatMap((value) => {
        if (!value || typeof value !== 'object' || Array.isArray(value)) return [];
        const requirement = value as RawPackageRequirement;
        return typeof requirement.package === 'string' && requirement.package &&
          typeof requirement.path === 'string' && requirement.path
          ? [{ package: requirement.package, path: requirement.path }]
          : [];
      })
    : [];
  const toolPaths = Array.isArray(raw['tool-paths'])
    ? raw['tool-paths'].filter((value): value is string => typeof value === 'string' && value.length > 0)
    : [];
  return { requires, toolPaths };
}
