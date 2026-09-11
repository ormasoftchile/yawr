import type * as vscode from 'vscode';
export { environmentValue, runtimeEnvironment } from './environmentIdentity';

export const NAMESPACE = 'yawr';
export const EXTENSION_ID = 'ormasoftchile.yawr-preview';

const stringEnums: Readonly<Record<string, readonly string[]>> = {
  'preview.nodeStyle': ['smooth-curves', 'minimalist', 'header-badges'],
  'preview.openLocation': ['beside', 'sameGroup'],
};

function validSetting(suffix: string, value: unknown, fallback: unknown): boolean {
  if (stringEnums[suffix]) return typeof value === 'string' && stringEnums[suffix].includes(value);
  if (Array.isArray(fallback)) return Array.isArray(value);
  if (fallback !== null && typeof fallback === 'object') {
    return value !== null && typeof value === 'object' && !Array.isArray(value);
  }
  return typeof value === typeof fallback;
}

export function getSetting<T>(suffix: string, resource: vscode.Uri | undefined, fallback: T): T {
  const workspace = (require('vscode') as typeof vscode).workspace;
  const value = workspace.getConfiguration(NAMESPACE, resource).get<T>(suffix);
  if (value === undefined) return fallback;
  if (!validSetting(suffix, value, fallback)) {
    throw new Error(`Invalid explicit Yawr setting "yawr.${suffix}".`);
  }
  return value;
}

export function affectsSetting(
  event: vscode.ConfigurationChangeEvent,
  suffix: string,
  resource?: vscode.Uri,
): boolean {
  return event.affectsConfiguration(`${NAMESPACE}.${suffix}`, resource);
}
