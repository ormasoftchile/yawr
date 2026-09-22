import * as fs from 'node:fs';
import * as path from 'node:path';
import {
  type GraphDocument,
  type GraphPreviewExecutor,
  parseGraphDocument,
  savedRunPreviewArgs,
} from './directGraphPreview';
import { type ResultsAvailability } from './typedResultsTypes';
import { canonicalResultsJSON, validateResults } from './typedResults';

export interface RuntimeEvent {
  kind: string;
  run_id: string;
  sequence: number;
  timestamp?: string;
  event_id?: string;
  payload?: Record<string, unknown>;
}

export interface SavedRunStepSummary {
  step_id?: string;
  node_id?: string;
  qualified_node_id?: string;
  phase?: string;
  invocation?: number;
  retry_attempt?: number;
  occurrence_sequence?: number;
  frame_id?: string;
  frame_step_index?: number;
  status?: string;
  error?: string;
  duration_ms?: number;
  output?: Record<string, unknown>;
  display_presentation?: unknown;
  display_presentation_diagnostic?: unknown;
}

export interface SavedRunSummary {
  runID: string;
  runDir: string;
  fullRunPath: string;
  runbookPath: string;
  status: string;
  startedAt?: string;
  completedAt?: string;
  stepCount?: number;
  hasTrace: boolean;
  hasSnapshots: boolean;
}

export interface LoadedSavedRun {
  document: GraphDocument;
  runID: string;
  status: string;
  error?: string;
  events: RuntimeEvent[];
  steps: SavedRunStepSummary[];
  resultsAvailability?: ResultsAvailability;
}

interface DurableStepResultJSON {
  StepID?: string;
  Status?: string;
  Outcome?: string;
  Output?: Record<string, unknown>;
  DurationMs?: number;
  StartedAt?: string;
  CompletedAt?: string;
  Error?: { message?: string } | string;
}

interface DurableFrameJSON {
  parent_qualified_node_id?: string;
  parent_step_id?: string;
  step_ids?: string[];
  results?: Record<string, DurableStepResultJSON>;
  status?: string;
}

interface CheckpointJSON {
  RunID?: string;
  RunbookPath?: string;
  Status?: string;
  StartedAt?: string;
  CompletedAt?: string;
  UpdatedAt?: string;
  Results?: Record<string, unknown>;
  results?: Record<string, unknown>;
  DurableStepResults?: Record<string, DurableStepResultJSON>;
  DurableExecutionFrames?: Record<string, DurableFrameJSON>;
}

/**
 * Discovers saved runs across provided directories (e.g. `.runbook/runs`).
 */
export async function discoverSavedRuns(candidateDirs: string[]): Promise<SavedRunSummary[]> {
  const seenRunIDs = new Set<string>();
  const summaries: SavedRunSummary[] = [];

  for (const baseDir of candidateDirs) {
    if (!fs.existsSync(baseDir)) continue;
    let entries: fs.Dirent[];
    try {
      entries = fs.readdirSync(baseDir, { withFileTypes: true });
    } catch {
      continue;
    }

    for (const entry of entries) {
      if (!entry.isDirectory()) continue;
      const runID = entry.name;
      if (seenRunIDs.has(runID)) continue;

      const fullRunPath = path.join(baseDir, runID);
      const snapDir = path.join(fullRunPath, 'snapshots');
      const tracePath = path.join(fullRunPath, 'trace.jsonl');
      const planPath = path.join(fullRunPath, 'plan.v1.json');

      const hasSnapshots = fs.existsSync(snapDir);
      const hasTrace = fs.existsSync(tracePath);
      const hasPlan = fs.existsSync(planPath);

      if (!hasSnapshots && !hasTrace && !hasPlan) continue;

      let status = 'unknown';
      let runbookPath = '';
      let startedAt: string | undefined;
      let completedAt: string | undefined;
      let stepCount: number | undefined;

      // 1. Try reading latest checkpoint
      if (hasSnapshots) {
        try {
          const files = fs.readdirSync(snapDir).filter(f => f.startsWith('checkpoint-') && f.endsWith('.json')).sort();
          if (files.length > 0) {
            const lastFile = files[files.length - 1];
            const cp: CheckpointJSON = JSON.parse(fs.readFileSync(path.join(snapDir, lastFile), 'utf8'));
            if (cp.Status) status = cp.Status;
            if (cp.RunbookPath) runbookPath = cp.RunbookPath;
            if (cp.StartedAt) startedAt = cp.StartedAt;
            if (cp.CompletedAt) completedAt = cp.CompletedAt;
            const rootSteps = Object.keys(cp.DurableStepResults || {}).length;
            let frameSteps = 0;
            for (const frame of Object.values(cp.DurableExecutionFrames || {})) {
              frameSteps += Object.keys(frame.results || {}).length;
            }
            stepCount = rootSteps + frameSteps;
          }
        } catch {}
      }

      // 2. Try reading trace if status or runbookPath missing
      if (hasTrace && (status === 'unknown' || !runbookPath || !startedAt)) {
        try {
          const lines = fs.readFileSync(tracePath, 'utf8').trim().split('\n');
          let traceStepCount = 0;
          for (const line of lines) {
            if (!line.trim()) continue;
            const event = JSON.parse(line);
            if (event.kind === 'run/started') {
              if (!startedAt && event.ts) startedAt = event.ts;
              if (event.payload?.runbook_path) runbookPath = String(event.payload.runbook_path);
            } else if (event.kind === 'run/completed') {
              status = 'completed';
              if (event.ts) completedAt = event.ts;
            } else if (event.kind === 'run/failed') {
              status = 'failed';
              if (event.ts) completedAt = event.ts;
            } else if (event.kind === 'run/cancelled') {
              status = 'cancelled';
              if (event.ts) completedAt = event.ts;
            } else if (event.kind === 'step/completed' || event.kind === 'step/failed') {
              traceStepCount++;
            }
          }
          if (stepCount === undefined && traceStepCount > 0) stepCount = traceStepCount;
        } catch {}
      }

      // 3. Fallback to plan.v1.json if runbookPath still missing
      if (!runbookPath && hasPlan) {
        try {
          const plan = JSON.parse(fs.readFileSync(planPath, 'utf8'));
          if (plan.runbook_path) runbookPath = plan.runbook_path;
          else if (plan.runbook?.path) runbookPath = plan.runbook.path;
        } catch {}
      }

      seenRunIDs.add(runID);
      summaries.push({
        runID,
        runDir: baseDir,
        fullRunPath,
        runbookPath,
        status,
        startedAt,
        completedAt,
        stepCount,
        hasTrace,
        hasSnapshots,
      });
    }
  }

  // Sort newest first
  summaries.sort((a, b) => {
    const timeA = a.completedAt || a.startedAt || '';
    const timeB = b.completedAt || b.startedAt || '';
    if (timeA && timeB) return timeB.localeCompare(timeA);
    if (timeA) return -1;
    if (timeB) return 1;
    return b.runID.localeCompare(a.runID);
  });

  return summaries;
}

/**
 * Resolves runDir and runID from a selected path (folder, trace.jsonl, checkpoint-*.json, etc.).
 */
export function resolveRunIdentityFromPath(selectedPath: string): { runDir: string; runID: string } | undefined {
  if (!selectedPath) return undefined;
  let stat: fs.Stats;
  try {
    stat = fs.statSync(selectedPath);
  } catch {
    return undefined;
  }

  if (stat.isFile()) {
    const base = path.basename(selectedPath);
    const parent = path.dirname(selectedPath);
    if (base === 'trace.jsonl' || base === 'plan.v1.json') {
      return {
        runID: path.basename(parent),
        runDir: path.dirname(parent),
      };
    }
    if (path.basename(parent) === 'snapshots' && base.startsWith('checkpoint-') && base.endsWith('.json')) {
      const runDirName = path.dirname(parent);
      return {
        runID: path.basename(runDirName),
        runDir: path.dirname(runDirName),
      };
    }
    // Any file directly inside a run directory
    if (fs.existsSync(path.join(parent, 'snapshots')) || fs.existsSync(path.join(parent, 'trace.jsonl'))) {
      return {
        runID: path.basename(parent),
        runDir: path.dirname(parent),
      };
    }
    return undefined;
  }

  if (stat.isDirectory()) {
    // Check if the directory itself is a run directory
    if (fs.existsSync(path.join(selectedPath, 'snapshots')) ||
        fs.existsSync(path.join(selectedPath, 'trace.jsonl')) ||
        fs.existsSync(path.join(selectedPath, 'plan.v1.json'))) {
      return {
        runID: path.basename(selectedPath),
        runDir: path.dirname(selectedPath),
      };
    }

    // Check if the directory contains run subdirectories (is a runs container directory)
    try {
      const children = fs.readdirSync(selectedPath, { withFileTypes: true });
      const runChildren = children.filter(child => {
        if (!child.isDirectory()) return false;
        const childPath = path.join(selectedPath, child.name);
        return fs.existsSync(path.join(childPath, 'snapshots')) ||
               fs.existsSync(path.join(childPath, 'trace.jsonl')) ||
               fs.existsSync(path.join(childPath, 'plan.v1.json'));
      });
      if (runChildren.length > 0) {
        // Pick the latest run child by mtime
        runChildren.sort((a, b) => {
          try {
            const timeA = fs.statSync(path.join(selectedPath, a.name)).mtimeMs;
            const timeB = fs.statSync(path.join(selectedPath, b.name)).mtimeMs;
            return timeB - timeA;
          } catch {
            return 0;
          }
        });
        return {
          runID: runChildren[0].name,
          runDir: selectedPath,
        };
      }
    } catch {}
  }

  return undefined;
}

/**
 * Loads the complete saved run state from disk and preview CLI.
 */
export async function loadSavedRunState(
  binary: string,
  runDir: string,
  runID: string,
  execute: GraphPreviewExecutor,
): Promise<LoadedSavedRun> {
  const actualRunDir = path.basename(runDir) === runID ? path.dirname(runDir) : runDir;
  const fullRunPath = path.join(actualRunDir, runID);
  const previewArgs = savedRunPreviewArgs(actualRunDir, runID);
  const { stdout } = await execute(binary, previewArgs);
  const document = parseGraphDocument(stdout);

  const tracePath = path.join(fullRunPath, 'trace.jsonl');
  const snapDir = path.join(fullRunPath, 'snapshots');

  const events: RuntimeEvent[] = [];
  if (fs.existsSync(tracePath)) {
    try {
      const lines = fs.readFileSync(tracePath, 'utf8').trim().split('\n');
      for (let index = 0; index < lines.length; index++) {
        const line = lines[index];
        if (!line.trim()) continue;
        try {
          const raw = JSON.parse(line);
          events.push({
            kind: String(raw.kind || ''),
            run_id: String(raw.run_id || runID),
            sequence: typeof raw.seq === 'number' ? raw.seq : (typeof raw.sequence === 'number' ? raw.sequence : index + 1),
            timestamp: typeof raw.ts === 'string' ? raw.ts : (typeof raw.timestamp === 'string' ? raw.timestamp : undefined),
            event_id: typeof raw.event_id === 'string' ? raw.event_id : undefined,
            payload: typeof raw.payload === 'object' && raw.payload !== null && !Array.isArray(raw.payload)
              ? (raw.payload as Record<string, unknown>) : undefined,
          });
        } catch {}
      }
    } catch {}
  }

  let checkpoint: CheckpointJSON | null = null;
  if (fs.existsSync(snapDir)) {
    try {
      const files = fs.readdirSync(snapDir).filter(f => f.startsWith('checkpoint-') && f.endsWith('.json')).sort();
      if (files.length > 0) {
        checkpoint = JSON.parse(fs.readFileSync(path.join(snapDir, files[files.length - 1]), 'utf8'));
      }
    } catch {}
  }

  // Determine final status
  let status = checkpoint?.Status;
  let runError: string | undefined;

  if (!status) {
    for (const e of events) {
      if (e.kind === 'run/completed') status = 'completed';
      else if (e.kind === 'run/failed') {
        status = 'failed';
        if (typeof e.payload?.error === 'string') runError = e.payload.error;
        else if (e.payload?.error && typeof (e.payload.error as { message?: string }).message === 'string') {
          runError = (e.payload.error as { message?: string }).message;
        }
      } else if (e.kind === 'run/cancelled') status = 'cancelled';
      else if (e.kind === 'run/indeterminate') status = 'indeterminate';
    }
  }

  if (!status) status = 'completed';

  // Extract typed results availability
  const typedResults = checkpoint?.Results ?? checkpoint?.results;
  let resultsAvailability: ResultsAvailability = { state: 'unavailable', reason: 'runtime-did-not-deliver-results' };
  if (typedResults && typeof typedResults === 'object') {
    try {
      const publication = validateResults(typedResults);
      resultsAvailability = { state: 'available', publication, canonicalJSON: canonicalResultsJSON(publication) };
    } catch {
      resultsAvailability = { state: 'unavailable', reason: 'invalid-publication' };
    }
  }

  // Reconstruct terminal steps
  const stepsByNode = new Map<string, SavedRunStepSummary>();

  // 1. Trace events provide richest data (code presentation, output, captures, etc.)
  for (const event of events) {
    if (event.kind === 'step/completed' || event.kind === 'step/failed' || event.kind === 'step/skipped') {
      const p = event.payload ?? {};
      const nodeID = String(p.qualified_node_id || p.node_id || p.step_id || '');
      const stepID = String(p.step_id || p.node_id || nodeID);
      if (!nodeID) continue;

      let stepErr: string | undefined;
      if (typeof p.error === 'string') stepErr = p.error;
      else if (p.error && typeof (p.error as { message?: string }).message === 'string') {
        stepErr = (p.error as { message?: string }).message;
      }

      stepsByNode.set(nodeID, {
        node_id: nodeID,
        step_id: stepID,
        qualified_node_id: typeof p.qualified_node_id === 'string' ? p.qualified_node_id : nodeID,
        phase: typeof p.phase === 'string' ? p.phase : undefined,
        invocation: typeof p.invocation === 'number' ? p.invocation : undefined,
        retry_attempt: typeof p.retry_attempt === 'number' ? p.retry_attempt : undefined,
        occurrence_sequence: typeof p.occurrence_sequence === 'number' ? p.occurrence_sequence : undefined,
        frame_id: typeof p.frame_id === 'string' ? p.frame_id : undefined,
        frame_step_index: typeof p.frame_step_index === 'number' ? p.frame_step_index : undefined,
        status: typeof p.status === 'string' ? p.status : (event.kind === 'step/completed' ? 'completed' : event.kind === 'step/failed' ? 'failed' : 'skipped'),
        duration_ms: typeof p.duration_ms === 'number' ? p.duration_ms : undefined,
        output: typeof p.output === 'object' && p.output !== null && !Array.isArray(p.output)
          ? (p.output as Record<string, unknown>) : undefined,
        error: stepErr,
      });
    }
  }

  // 2. Checkpoint results supplement any nodes not seen in trace
  if (checkpoint?.DurableStepResults) {
    for (const [stepID, res] of Object.entries(checkpoint.DurableStepResults)) {
      if (!stepsByNode.has(stepID)) {
        let errStr: string | undefined;
        if (typeof res.Error === 'string') errStr = res.Error;
        else if (res.Error && typeof res.Error === 'object' && 'message' in res.Error) {
          errStr = String((res.Error as { message?: string }).message);
        }
        stepsByNode.set(stepID, {
          node_id: stepID,
          step_id: res.StepID || stepID,
          status: res.Status || 'completed',
          duration_ms: res.DurationMs,
          output: res.Output,
          error: errStr,
        });
      }
    }
  }

  if (checkpoint?.DurableExecutionFrames) {
    for (const frame of Object.values(checkpoint.DurableExecutionFrames)) {
      const parentID = frame.parent_qualified_node_id || frame.parent_step_id;
      if (frame.results) {
        for (const [idxStr, res] of Object.entries(frame.results)) {
          const idx = Number(idxStr);
          const substepID = (frame.step_ids && frame.step_ids[idx]) || res.StepID || idxStr;
          const nodeID = parentID ? `${parentID}/${substepID}` : substepID;
          if (!stepsByNode.has(nodeID)) {
            let errStr: string | undefined;
            if (typeof res.Error === 'string') errStr = res.Error;
            else if (res.Error && typeof res.Error === 'object' && 'message' in res.Error) {
              errStr = String((res.Error as { message?: string }).message);
            }
            stepsByNode.set(nodeID, {
              node_id: nodeID,
              step_id: substepID,
              status: res.Status || 'completed',
              duration_ms: res.DurationMs,
              output: res.Output,
              error: errStr,
            });
          }
        }
      }
    }
  }

  const steps = Array.from(stepsByNode.values());

  return {
    document,
    runID,
    status,
    error: runError,
    events,
    steps,
    resultsAvailability,
  };
}
