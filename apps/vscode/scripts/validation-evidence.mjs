import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { homedir } from 'node:os';
import { basename, join, relative, resolve } from 'node:path';

const ALLOWED_DIAGNOSTIC_FILES = new Set(['diagnostic-state.json']);
const MAX_EXCERPT_LENGTH = 4_096;
const ABSOLUTE_PATH_PREFIX = String.raw`(?:file:\/\/\/(?:[A-Za-z]:[\\/]|\/?)|[A-Za-z]:[\\/]|\\\\[^\\/\s"'` + '`' + String.raw`]+[\\/][^\\/\s"'` + '`' + String.raw`]+[\\/]*|\/)`;

export function failureEvidenceRoot(environment = process.env) {
  return resolve(environment.YAWR_FAILURE_EVIDENCE_ROOT || join(homedir(), '.yawr-validation-evidence'));
}

function isInside(parent, candidate) {
  const path = relative(resolve(parent), resolve(candidate));
  return path === '' || (!path.startsWith('..') && !path.includes(':'));
}

function redactText(value) {
  if (value === undefined || value === null) return null;
  let redacted = String(value);
  for (const quote of ['"', "'", '`']) {
    const escapedQuote = quote === '`' ? '\\x60' : quote;
    redacted = redacted.replace(
      new RegExp(`(${escapedQuote})${ABSOLUTE_PATH_PREFIX}[^${escapedQuote}\\r\\n]*\\1`, 'gi'),
      `${quote}<ABSOLUTE_PATH>${quote}`,
    );
  }
  return redacted
    .replace(/file:\/\/\/[^\s"'`]+/gi, '<ABSOLUTE_PATH>')
    .replace(/(?<![\\A-Za-z0-9])\\\\[^\\/\s"'`]+[\\/][^\\/\s"'`]+(?:[\\/][^\s"'`]+)*/g, '<ABSOLUTE_PATH>')
    .replace(/(?<![A-Za-z0-9])[A-Za-z]:[\\/][^\s"'`]+/g, '<ABSOLUTE_PATH>')
    .replace(/(?<![A-Za-z0-9/])\/(?!\/)[^\s"'`]+/g, '<ABSOLUTE_PATH>')
    .split(/\r?\n/)
    .filter((line) => !/^\s*at\s+(?:async\s+)?/.test(line))
    .join('\n')
    .slice(-MAX_EXCERPT_LENGTH);
}

function errorCategory(value) {
  if (!value) return null;
  const match = String(value).match(/^([A-Za-z][A-Za-z0-9]*(?:Error)?)(?::|\s|$)/);
  return match?.[1] ?? 'Error';
}

function safeString(value, maximum = 256) {
  const redacted = redactText(value);
  return redacted === null ? null : redacted.slice(0, maximum);
}

export function createDiagnosticSummary(diagnostic, failure = {}) {
  const tabs = Array.isArray(diagnostic.tabs) ? diagnostic.tabs.slice(0, 20).map((tab) => ({
    label: safeString(tab?.label),
    inputType: safeString(tab?.inputType),
    rawViewType: safeString(tab?.rawViewType),
    viewType: safeString(tab?.viewType),
  })) : [];
  return {
    schemaVersion: 1,
    validator: {
      label: safeString(diagnostic.label),
    },
    installedSource: {
      kind: diagnostic.installedSource === 'vsix' ? 'vsix' : 'unknown',
      extensionDirectory: safeString(diagnostic.installedExtensionDirectory),
      installComplete: diagnostic.installComplete === true,
    },
    activation: {
      extensionFound: diagnostic.extensionFound === true,
      activeBefore: diagnostic.extensionActiveBeforeActivation === true,
      activeAfter: diagnostic.extensionActiveAfterActivation === true,
      errorCategory: safeString(diagnostic.activationErrorCategory)
        ?? errorCategory(diagnostic.activationError),
    },
    command: {
      previewGraphPresent: diagnostic.previewGraphCommandPresent === true,
      previewGraphExecuted: diagnostic.previewGraphCommandExecuted === true,
      errorCategory: safeString(diagnostic.previewGraphCommandErrorCategory)
        ?? errorCategory(diagnostic.previewGraphCommandError),
      registeredYawrCommands: Array.isArray(diagnostic.yawrCommands)
        ? diagnostic.yawrCommands.slice(0, 50).map((command) => safeString(command))
        : [],
    },
    tabs,
    failure: {
      phase: safeString(failure.phase ?? failure.label ?? 'unknown'),
      reason: safeString(failure.reason ?? failure.kind ?? failure.code ?? failure.name ?? 'unknown'),
      stdoutExcerpt: redactText(failure.stdout ?? ''),
      stderrExcerpt: redactText(failure.stderr ?? failure.output ?? ''),
    },
  };
}

export async function preserveFailureEvidence(runRoot, options = {}) {
  const {
    environment = process.env,
    failure,
    repositoryRoot,
    sourceFiles = ['diagnostic-state.json'],
  } = options;
  for (const sourceFile of sourceFiles) {
    if (!ALLOWED_DIAGNOSTIC_FILES.has(sourceFile) || basename(sourceFile) !== sourceFile) {
      throw new Error(`failure evidence source is not allowlisted: ${sourceFile}`);
    }
  }

  const destinationRoot = failureEvidenceRoot(environment);
  if (repositoryRoot && isInside(repositoryRoot, destinationRoot)) {
    throw new Error('failure evidence root must be outside the repository');
  }

  let diagnostic = {};
  try {
    diagnostic = JSON.parse(await readFile(join(runRoot, 'diagnostic-state.json'), 'utf8'));
  } catch (error) {
    if (error?.code !== 'ENOENT') throw error;
  }

  const artifactId = `yawr-validation-${randomUUID()}`;
  const destination = join(destinationRoot, artifactId);
  const summary = {
    artifactId,
    ...createDiagnosticSummary(diagnostic, failure),
  };
  await mkdir(destination, { recursive: true });
  await writeFile(join(destination, 'diagnostic-summary.json'), `${JSON.stringify(summary, null, 2)}\n`, {
    flag: 'wx',
  });
  return { artifactId, destination };
}
