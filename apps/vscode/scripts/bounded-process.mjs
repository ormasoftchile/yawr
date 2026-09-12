import { spawn } from 'node:child_process';

export class BoundedProcessError extends Error {
  constructor(kind, message, details = {}) {
    super(message);
    this.name = 'BoundedProcessError';
    this.kind = kind;
    Object.assign(this, details);
  }
}

function commandForPlatform(command, shell) {
  if (shell) return command;
  if (process.platform !== 'win32') return command;
  if (command === 'npm' || command === 'npx') return `${command}.cmd`;
  return command;
}

export async function terminateProcessTree(pid) {
  if (!Number.isInteger(pid) || pid <= 0) return;
  if (process.platform === 'win32') {
    await new Promise((resolve) => {
      const killer = spawn('taskkill.exe', ['/PID', String(pid), '/T', '/F'], {
        stdio: 'ignore',
        windowsHide: true,
      });
      killer.once('error', () => resolve());
      killer.once('close', () => resolve());
    });
    return;
  }
  try {
    process.kill(-pid, 'SIGKILL');
  } catch (error) {
    if (error?.code !== 'ESRCH') throw error;
  }
}

export function runBoundedProcess(command, args, options = {}) {
  const {
    cwd,
    env = process.env,
    timeoutMs,
    stallMs,
    label = command,
    stdout = process.stdout,
    stderr = process.stderr,
    shell = false,
  } = options;
  if (!(timeoutMs > 0) || !(stallMs > 0)) {
    throw new Error('runBoundedProcess requires positive timeoutMs and stallMs');
  }

  return new Promise((resolve, reject) => {
    const useShell = shell || (process.platform === 'win32' && (command === 'npm' || command === 'npx'));
    const child = spawn(commandForPlatform(command, useShell), args, {
      cwd,
      env,
      detached: process.platform !== 'win32',
      stdio: ['ignore', 'pipe', 'pipe'],
      windowsHide: true,
      shell: useShell,
    });
    let settled = false;
    let stopping = false;
    let output = '';
    let stdoutOutput = '';
    let stderrOutput = '';
    let stallTimer;

    const remember = (chunk, destination, stream) => {
      destination?.write(chunk);
      output = `${output}${chunk}`.slice(-64 * 1024);
      if (stream === 'stdout') stdoutOutput = `${stdoutOutput}${chunk}`.slice(-64 * 1024);
      else stderrOutput = `${stderrOutput}${chunk}`.slice(-64 * 1024);
      clearTimeout(stallTimer);
      stallTimer = setTimeout(() => stop('stall', `${label} produced no output for ${stallMs}ms`), stallMs);
      stallTimer.unref?.();
    };
    const finish = (callback) => {
      if (settled) return;
      settled = true;
      clearTimeout(timeoutTimer);
      clearTimeout(stallTimer);
      callback();
    };
    const stop = async (kind, message) => {
      if (settled || stopping) return;
      stopping = true;
      await terminateProcessTree(child.pid);
      finish(() => reject(new BoundedProcessError(kind, message, {
        command,
        pid: child.pid,
        output,
        stdout: stdoutOutput,
        stderr: stderrOutput,
      })));
    };

    child.stdout.on('data', (chunk) => remember(chunk, stdout, 'stdout'));
    child.stderr.on('data', (chunk) => remember(chunk, stderr, 'stderr'));
    child.once('error', (error) => finish(() => reject(new BoundedProcessError(
      'spawn',
      `${label} could not start: ${error.message}`,
      { command, args, label, cause: error, output, stdout: stdoutOutput, stderr: stderrOutput },
    ))));
    child.once('close', (code, signal) => {
      if (stopping) return;
      finish(() => {
      if (code === 0) resolve({ code, signal, output });
      else reject(new BoundedProcessError(
        'exit',
        `${label} exited with code ${code ?? 'unknown'}${signal ? ` (${signal})` : ''}`,
        { command, args, label, code, signal, output, stdout: stdoutOutput, stderr: stderrOutput },
      ));
      });
    });

    const timeoutTimer = setTimeout(
      () => stop('timeout', `${label} exceeded ${timeoutMs}ms`),
      timeoutMs,
    );
    timeoutTimer.unref?.();
    stallTimer = setTimeout(
      () => stop('stall', `${label} produced no output for ${stallMs}ms`),
      stallMs,
    );
    stallTimer.unref?.();
  });
}

export async function runWithRetry(operation, options = {}) {
  const { retries = 0, shouldRetry = () => false, onRetry = () => {} } = options;
  let attempt = 0;
  while (true) {
    try {
      return await operation(attempt);
    } catch (error) {
      if (attempt >= retries || !shouldRetry(error)) throw error;
      attempt += 1;
      await onRetry(error, attempt);
    }
  }
}

export async function runWithCleanup(operation, cleanup) {
  let operationError;
  try {
    return await operation();
  } catch (error) {
    operationError = error;
  } finally {
    try {
      await cleanup();
    } catch (cleanupError) {
      if (operationError) {
        throw new AggregateError([operationError, cleanupError], 'operation and cleanup failed');
      }
      throw cleanupError;
    }
  }
  throw operationError;
}
