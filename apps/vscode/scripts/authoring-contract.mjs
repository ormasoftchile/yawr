import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const run = promisify(execFile);
const options = { timeout: 5000, maxBuffer: 8 * 1024 * 1024, windowsHide: true };

async function invoke(binary, args) {
  try {
    const { stdout, stderr } = await run(binary, args, options);
    return { exitCode: 0, stdout, stderr };
  } catch (error) {
    return {
      exitCode: typeof error.code === 'number' ? error.code : -1,
      stdout: error.stdout ?? '',
      stderr: error.stderr ?? '',
    };
  }
}

export async function probeAuthoringContract(binary) {
  const current = await invoke(binary, ['authoring', 'capabilities', '--v3']);
  if (current.exitCode !== 0 || current.stderr !== '') {
    throw new Error(`${binary} rejected current authoring v3 capabilities: ${current.stderr}`);
  }
  let capabilities;
  try {
    capabilities = JSON.parse(current.stdout);
  } catch {
    throw new Error(`${binary} returned invalid authoring v3 capability JSON`);
  }
  if (capabilities.schema_version !== 'authoring-capabilities/v3') {
    throw new Error(`${binary} returned ${capabilities.schema_version ?? 'no schema version'} for authoring v3`);
  }

  const obsolete = await invoke(binary, ['authoring', 'capabilities']);
  if (obsolete.exitCode === 0 || obsolete.stdout !== '' || obsolete.stderr.trim() !== 'authoring: unsupported-version') {
    throw new Error(`${binary} accepted obsolete authoring capabilities`);
  }
  return {
    current: capabilities,
    obsolete: { exitCode: obsolete.exitCode, stderr: obsolete.stderr.trim() },
  };
}

export function assertAuthoringParity(source, packaged) {
  if (JSON.stringify(source) !== JSON.stringify(packaged)) {
    throw new Error('source and packaged authoring behavior differ');
  }
}
