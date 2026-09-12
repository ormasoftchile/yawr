import * as fs from 'fs';
import * as path from 'path';

export type ExecFileFn = (
  file: string,
  args: readonly string[],
  options: { env: NodeJS.ProcessEnv; maxBuffer: number },
  callback: (error: Error | null, stdout: string, stderr: string) => void,
) => void;

export interface RunPackageMapResolution {
  path?: string;
  source: 'run-sibling' | 'setting' | 'convention' | 'absent';
  warning?: string;
}

export interface RunHandoffOptions {
  bin: string;
  runbookPath: string;
  varPairArgs: readonly string[];
  projectRoot: string;
  packageMapSetting: string | undefined;
  bridgeVars: Record<string, string>;
  baseEnv: NodeJS.ProcessEnv;
  execFile: ExecFileFn;
}

export interface RunHandoffResult {
  stdout: string;
  stderr: string;
  packageMap: RunPackageMapResolution;
}

function isFile(p: string): boolean {
  try {
    return fs.statSync(p).isFile();
  } catch {
    return false;
  }
}

function resolveAgainstProject(projectRoot: string, value: string): string {
  return path.isAbsolute(value) ? value : path.join(projectRoot, value);
}

function siblingRunMapForServeMap(serveMapPath: string): string | undefined {
  const suffix = '.serve-package-map.yaml';
  if (!serveMapPath.endsWith(suffix)) return undefined;
  return serveMapPath.slice(0, -suffix.length) + '.package-map.yaml';
}

export function resolveRunPackageMapPath(
  projectRoot: string,
  settingValue: string | undefined,
): RunPackageMapResolution {
  if (settingValue && settingValue.trim()) {
    const configured = resolveAgainstProject(projectRoot, settingValue);
    const sibling = siblingRunMapForServeMap(configured);
    if (sibling && isFile(sibling)) {
      return { path: sibling, source: 'run-sibling' };
    }
    if (isFile(configured)) {
      return {
        path: configured,
        source: 'setting',
        warning: sibling
          ? `yawr.packageMap points at a serve map but no run package-map sibling was found: ${sibling}`
          : undefined,
      };
    }
    return {
      source: 'absent',
      warning: `yawr.packageMap "${settingValue}" not found in ${projectRoot}`,
    };
  }

  const convention = path.join(projectRoot, 'package-map.yaml');
  if (isFile(convention)) {
    return { path: convention, source: 'convention' };
  }
  return { source: 'absent' };
}

export function buildRunArgs(
  runbookPath: string,
  varPairArgs: readonly string[],
  packageMapPath?: string,
): string[] {
  const args = ['run'];
  if (packageMapPath) {
    args.push('--package-map', packageMapPath);
  }
  args.push(...varPairArgs.flatMap((p) => ['--var', p]), runbookPath);
  return args;
}

export function executeRunHandoff(options: RunHandoffOptions): Promise<RunHandoffResult> {
  const packageMap = resolveRunPackageMapPath(options.projectRoot, options.packageMapSetting);
  const args = buildRunArgs(options.runbookPath, options.varPairArgs, packageMap.path);
  return new Promise((resolve, reject) => {
    options.execFile(
      options.bin,
      args,
      { env: { ...options.baseEnv, ...options.bridgeVars }, maxBuffer: 10 * 1024 * 1024 },
      (error, stdout, stderr) => {
        if (error) {
          Object.assign(error, { stdout, stderr, packageMap });
          reject(error);
          return;
        }
        resolve({ stdout, stderr, packageMap });
      },
    );
  });
}
