package runstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

const (
	maxPlanSnapshotBytes       = 64 << 20
	maxRunStateBytes           = 16 << 20
	maxStateBlobBytes          = 256 << 20
	maxCompressedBlobBytes     = 64 << 20
	inlineStateValueBytes      = 64 << 10
	maxInteractions            = 128
	maxInteractionBytes        = 1 << 20
	maxStoredValueEntries      = 4096
	maxCheckpointExpandedBytes = 256 << 20
)

var (
	ErrPlanConflict       = errors.New("runstore: plan snapshot conflicts with existing run")
	ErrCheckpointConflict = errors.New("runstore: checkpoint sequence conflicts with existing run")
)

type runStateSnapshotV1 struct {
	TypedState                  map[string]storedJSONValueV1                     `json:"TypedState,omitempty"`
	ResultsID                   string                                           `json:"ResultsID,omitempty"`
	SchemaVersion               string                                           `json:"SchemaVersion,omitempty"`
	RunID                       string                                           `json:"RunID"`
	RunbookPath                 string                                           `json:"RunbookPath"`
	Mode                        engine.RunMode                                   `json:"Mode,omitempty"`
	WriterEpoch                 uint64                                           `json:"WriterEpoch,omitempty"`
	CheckpointSequence          int64                                            `json:"CheckpointSequence,omitempty"`
	CommittedTraceSequence      int64                                            `json:"CommittedTraceSequence,omitempty"`
	DurablePendingTraceEvents   []pendingTraceEventSnapshotV1                    `json:"DurablePendingTraceEvents,omitempty"`
	PlanSnapshotDigest          string                                           `json:"PlanSnapshotDigest,omitempty"`
	CursorSet                   *engine.ExecutionCursorSet                       `json:"CursorSet,omitempty"`
	Status                      engine.RunStatus                                 `json:"Status"`
	CurrentStep                 string                                           `json:"CurrentStep"`
	CurrentStepIndex            int                                              `json:"CurrentStepIndex"`
	DurableVars                 map[string]storedJSONValueV1                     `json:"DurableVars,omitempty"`
	DurableStepResults          map[string]*stepResultSnapshotV2                 `json:"DurableStepResults,omitempty"`
	Interactions                map[string]*engine.InteractionState              `json:"Interactions,omitempty"`
	ExecutionInvocationCounts   map[string]int                                   `json:"ExecutionInvocationCounts,omitempty"`
	InteractionInvocationCounts map[string]int                                   `json:"InteractionInvocationCounts,omitempty"`
	Dispatches                  map[string]*engine.DispatchState                 `json:"Dispatches,omitempty"`
	DurableExecutionFrames      map[string]*executionFrameSnapshotV1             `json:"DurableExecutionFrames,omitempty"`
	DynamicIncludes             map[string]*engine.DynamicIncludeResolutionState `json:"DynamicIncludes,omitempty"`
	PendingHandoff              *engine.HandoffRequest                           `json:"PendingHandoff,omitempty"`
	StartedAt                   time.Time                                        `json:"StartedAt"`
	UpdatedAt                   time.Time                                        `json:"UpdatedAt"`
	CompletedAt                 time.Time                                        `json:"CompletedAt"`
}

type stepResultSnapshotV2 struct {
	TerminalResults bool                         `json:"TerminalResults,omitempty"`
	PublicOutputs   map[string]storedJSONValueV1 `json:"PublicOutputs,omitempty"`
	ResultsID       string                       `json:"ResultsID,omitempty"`
	RequiredFailure bool                         `json:"RequiredFailure,omitempty"`
	StepID          string                       `json:"StepID"`
	Status          engine.StepStatus            `json:"Status"`
	Outcome         engine.StepOutcome           `json:"Outcome"`
	Output          map[string]storedJSONValueV1 `json:"Output,omitempty"`
	Vars            map[string]storedJSONValueV1 `json:"Vars,omitempty"`
	StartedAt       time.Time                    `json:"StartedAt"`
	CompletedAt     time.Time                    `json:"CompletedAt"`
	DurationMs      int64                        `json:"DurationMs"`
	Error           string                       `json:"Error,omitempty"`
	Evidence        []evidence.EvidenceRecord    `json:"Evidence,omitempty"`
	Indeterminate   *engine.IndeterminateRecord  `json:"Indeterminate,omitempty"`
}

type executionFrameSnapshotV1 struct {
	RunResultsID          string                           `json:"run_results_id,omitempty"`
	SchemaVersion         string                           `json:"schema_version"`
	FrameID               string                           `json:"frame_id"`
	ParentFrameID         string                           `json:"parent_frame_id,omitempty"`
	WriterEpoch           uint64                           `json:"writer_epoch"`
	ParentQualifiedNodeID string                           `json:"parent_qualified_node_id"`
	ParentStepID          string                           `json:"parent_step_id"`
	Kind                  string                           `json:"kind"`
	CallPath              []engine.DebugCallFrame          `json:"call_path"`
	BranchLabel           string                           `json:"branch_label,omitempty"`
	IterationIndex        int                              `json:"iteration_index,omitempty"`
	Invocation            int                              `json:"invocation"`
	DefinitionDigest      string                           `json:"definition_digest"`
	StepCount             int                              `json:"step_count"`
	StepIDs               []string                         `json:"step_ids"`
	NextStepIndex         int                              `json:"next_step_index"`
	DurableWorkingVars    map[string]storedJSONValueV1     `json:"working_vars,omitempty"`
	DurableResults        map[string]*stepResultSnapshotV2 `json:"results,omitempty"`
	Status                engine.ExecutionFrameStatus      `json:"status"`
	StartedAt             string                           `json:"started_at"`
	CompletedAt           string                           `json:"completed_at,omitempty"`
}

type pendingTraceEventSnapshotV1 struct {
	EventID        string                       `json:"event_id"`
	RunID          string                       `json:"run_id"`
	RunbookID      string                       `json:"runbook_id,omitempty"`
	Timestamp      string                       `json:"timestamp"`
	Kind           string                       `json:"kind"`
	Sequence       int64                        `json:"sequence"`
	DurablePayload map[string]storedJSONValueV1 `json:"payload,omitempty"`
}

type storedJSONValueV1 struct {
	Inline json.RawMessage `json:"inline,omitempty"`
	Blob   *stateBlobRefV1 `json:"blob,omitempty"`
}

type stateBlobRefV1 struct {
	Digest           string `json:"digest"`
	Encoding         string `json:"encoding"`
	UncompressedSize int64  `json:"uncompressed_size"`
}

// DirRunStore stores run state and traces under a base directory.
type DirRunStore struct {
	baseDir      string
	mu           sync.Mutex
	mutationMu   sync.RWMutex
	writers      map[string]*internaltrace.JSONLWriter
	plans        map[string]*engine.ExecutionPlan
	planDigests  map[string]string
	planVersions map[string]string
	leases       map[*fileRunLease]struct{}
	leaseEpochs  map[string]uint64
}

// NewDirRunStore constructs a DirRunStore rooted at baseDir.
func NewDirRunStore(baseDir string) *DirRunStore {
	if baseDir == "" {
		baseDir = ".runbook/runs"
	}
	return &DirRunStore{
		baseDir:      baseDir,
		writers:      make(map[string]*internaltrace.JSONLWriter),
		plans:        make(map[string]*engine.ExecutionPlan),
		planDigests:  make(map[string]string),
		planVersions: make(map[string]string),
		leases:       make(map[*fileRunLease]struct{}),
		leaseEpochs:  make(map[string]uint64),
	}
}

// RunDir returns the filesystem directory for the run.
func (s *DirRunStore) RunDir(runID string) string {
	return filepath.Join(s.baseDir, runID)
}

// TracePath returns the trace file path for the run.
func (s *DirRunStore) TracePath(runID string) string {
	return filepath.Join(s.RunDir(runID), "trace.jsonl")
}

// PlanPath returns the durable executable-plan snapshot path for the run.
func (s *DirRunStore) PlanPath(runID string) string {
	return filepath.Join(s.RunDir(runID), "plan.v1.json")
}

// RegisterPlan stores the execution plan for a run in memory.
func (s *DirRunStore) RegisterPlan(runID string, plan *engine.ExecutionPlan) {
	if runID == "" || plan == nil {
		return
	}
	s.mu.Lock()
	s.plans[runID] = plan
	s.mu.Unlock()
}

// Plan returns the cached plan for a run, if present.
func (s *DirRunStore) Plan(runID string) *engine.ExecutionPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plans[runID]
}

// SavePlan atomically persists a validated executable plan for process-restart resume.
func (s *DirRunStore) SavePlan(ctx context.Context, runID string, plan *engine.ExecutionPlan) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := validateRunID(runID); err != nil {
		return err
	}
	if plan == nil {
		return errors.New("runstore: plan is required")
	}
	if plan.RunID != "" && plan.RunID != runID {
		return fmt.Errorf("runstore: plan run id %q does not match %q", plan.RunID, runID)
	}
	snapshotPlan := *plan
	snapshotPlan.RunID = runID
	snapshot, err := plansnapshot.FromExecutionPlan(&snapshotPlan)
	if err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("runstore: encode plan: %w", err)
	}
	path := s.PlanPath(runID)
	if len(data) > maxPlanSnapshotBytes {
		return fmt.Errorf("runstore: plan snapshot exceeds %d bytes", maxPlanSnapshotBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existingData, readErr := readFileBounded(path, maxPlanSnapshotBytes); readErr == nil {
		var existing plansnapshot.SnapshotV1
		if err := decodeJSONDocument(existingData, &existing); err != nil {
			return fmt.Errorf("runstore: decode existing plan: %w", err)
		}
		if _, err := plansnapshot.Restore(existing); err != nil {
			return fmt.Errorf("runstore: validate existing plan: %w", err)
		}
		if existing.SnapshotDigest != snapshot.SnapshotDigest {
			return fmt.Errorf("%w: run %q", ErrPlanConflict, runID)
		}
		s.plans[runID] = plan
		s.planDigests[runID] = existing.SnapshotDigest
		s.planVersions[runID] = existing.SchemaVersion
		return nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := writeFileExclusive(path, data); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		existingData, readErr := readFileBounded(path, maxPlanSnapshotBytes)
		if readErr != nil {
			return readErr
		}
		var existing plansnapshot.SnapshotV1
		if decodeErr := decodeJSONDocument(existingData, &existing); decodeErr != nil {
			return fmt.Errorf("runstore: decode concurrently published plan: %w", decodeErr)
		}
		if _, restoreErr := plansnapshot.Restore(existing); restoreErr != nil {
			return fmt.Errorf("runstore: validate concurrently published plan: %w", restoreErr)
		}
		if existing.SnapshotDigest != snapshot.SnapshotDigest {
			return fmt.Errorf("%w: run %q", ErrPlanConflict, runID)
		}
		if syncErr := syncDirectory(filepath.Dir(path)); syncErr != nil {
			return syncErr
		}
	}
	s.plans[runID] = plan
	s.planDigests[runID] = snapshot.SnapshotDigest
	s.planVersions[runID] = snapshot.SchemaVersion
	return nil
}

// LoadPlan restores and validates the executable plan persisted by SavePlan.
func (s *DirRunStore) LoadPlan(ctx context.Context, runID string) (*engine.ExecutionPlan, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	data, err := readFileBounded(s.PlanPath(runID), maxPlanSnapshotBytes)
	if err != nil {
		return nil, err
	}
	var snapshot plansnapshot.SnapshotV1
	if err := decodeJSONDocument(data, &snapshot); err != nil {
		return nil, fmt.Errorf("runstore: decode plan: %w", err)
	}
	if snapshot.RunID != runID {
		return nil, fmt.Errorf("runstore: persisted plan run id %q does not match %q", snapshot.RunID, runID)
	}
	plan, err := plansnapshot.Restore(snapshot)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.plans[runID] = plan
	s.planDigests[runID] = snapshot.SnapshotDigest
	s.planVersions[runID] = snapshot.SchemaVersion
	s.mu.Unlock()
	return plan, nil
}

// PlanDigest returns the exact durable snapshot digest cached for a run.
func (s *DirRunStore) PlanDigest(runID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest, ok := s.planDigests[runID]
	return digest, ok
}

// SaveState checkpoints the current run state to disk.
func (s *DirRunStore) SaveState(ctx context.Context, state engine.RunState) error {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := validateRunID(state.RunID); err != nil {
		return err
	}
	if err := s.validateWriterEpoch(state.RunID, state.WriterEpoch); err != nil {
		return err
	}
	planDigest, err := s.planDigestForState(ctx, state.RunID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.PlanSnapshotDigest != "" && planDigest != "" && state.PlanSnapshotDigest != planDigest {
		return fmt.Errorf("%w: checkpoint for run %q", ErrPlanConflict, state.RunID)
	}
	if planDigest == "" {
		planDigest = state.PlanSnapshotDigest
	}
	if err := validateInteractionStates(state.Interactions); err != nil {
		return err
	}
	if err := validateExecutionInvocationCounts(state.ExecutionInvocationCounts); err != nil {
		return err
	}
	if err := validateInteractionInvocationCounts(state.InteractionInvocationCounts, state.Interactions); err != nil {
		return err
	}
	if err := validateDispatchStates(state.WriterEpoch, state.Dispatches); err != nil {
		return err
	}
	if err := validateExecutionFrameStates(state.WriterEpoch, state.ExecutionFrames); err != nil {
		return err
	}
	scopes, err := s.scopeSetForResolutions(ctx, state.RunID, state.DynamicIncludes)
	if err != nil {
		return err
	}
	if err := validateDynamicIncludeResolutions(state.WriterEpoch, state.DynamicIncludes, scopes); err != nil {
		return err
	}
	if err := validatePendingHandoff(state.Status, state.PendingHandoff); err != nil {
		return err
	}
	if err := validatePendingTraceEvents(state.RunID, state.CommittedTraceSequence, state.PendingTraceEvents); err != nil {
		return err
	}
	if err := validateExecutionStateRelationships(
		state.WriterEpoch, state.CursorSet, state.StepResults, state.Interactions, state.Dispatches, state.ExecutionFrames, state.DynamicIncludes,
	); err != nil {
		return err
	}
	typedValues := typedStateValues(state)
	if err := validateTypedPublications(state); err != nil {
		return err
	}
	if err := validateStateValueBudgets(state.Vars, state.StepResults, state.ExecutionFrames, state.PendingTraceEvents, typedValues); err != nil {
		return err
	}
	dir := filepath.Join(s.RunDir(state.RunID), "snapshots")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	filename := fmt.Sprintf("checkpoint-%020d.json", state.CheckpointSequence)
	finalPath := filepath.Join(dir, filename)

	durableVars, err := s.storeValueMap(state.RunID, state.Vars)
	if err != nil {
		return err
	}
	durableStepResults, err := s.snapshotStepResultsV2(state.RunID, state.StepResults)
	if err != nil {
		return err
	}
	durableExecutionFrames, err := s.snapshotExecutionFrames(state.RunID, state.ExecutionFrames)
	if err != nil {
		return err
	}
	durablePendingTraceEvents, err := s.snapshotPendingTraceEvents(state.RunID, state.PendingTraceEvents)
	if err != nil {
		return err
	}
	snapshot := runStateSnapshotV1{
		ResultsID:     publicationID(state.Results),
		SchemaVersion: runStateSchemaV3, RunID: state.RunID, RunbookPath: state.RunbookPath,
		Mode:               state.Mode,
		WriterEpoch:        state.WriterEpoch,
		CheckpointSequence: state.CheckpointSequence, CommittedTraceSequence: state.CommittedTraceSequence,
		DurablePendingTraceEvents: durablePendingTraceEvents, PlanSnapshotDigest: planDigest, CursorSet: state.CursorSet,
		Status: state.Status, CurrentStep: state.CurrentStep, CurrentStepIndex: state.CurrentStepIndex,
		DurableVars: durableVars, DurableStepResults: durableStepResults,
		Interactions:                state.Interactions,
		ExecutionInvocationCounts:   state.ExecutionInvocationCounts,
		InteractionInvocationCounts: state.InteractionInvocationCounts,
		Dispatches:                  state.Dispatches,
		DurableExecutionFrames:      durableExecutionFrames,
		DynamicIncludes:             state.DynamicIncludes,
		PendingHandoff:              state.PendingHandoff,
		StartedAt:                   state.StartedAt, UpdatedAt: state.UpdatedAt, CompletedAt: state.CompletedAt,
	}
	if len(typedValues) != 0 {
		snapshot.TypedState, err = s.storeValueMap(state.RunID, typedValues)
		if err != nil {
			return err
		}
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(data) > maxRunStateBytes {
		return fmt.Errorf("runstore: state snapshot exceeds %d bytes", maxRunStateBytes)
	}
	if state.CheckpointSequence > 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return saveSequencedCheckpoint(dir, finalPath, state.RunID, state.CheckpointSequence, data)
	}
	if err := writeFileAtomic(finalPath, data); err != nil {
		return err
	}
	return nil
}

type stateValueBudget struct {
	count int
	bytes int64
}

func (b *stateValueBudget) addEntries(count int) error {
	if count > maxStoredValueEntries-b.count {
		return fmt.Errorf("runstore: checkpoint value count exceeds %d", maxStoredValueEntries)
	}
	b.count += count
	return nil
}

func (b *stateValueBudget) addBytes(size int64) error {
	if size > maxCheckpointExpandedBytes-b.bytes {
		return fmt.Errorf("runstore: checkpoint expanded data exceeds %d bytes", maxCheckpointExpandedBytes)
	}
	b.bytes += size
	return nil
}

func validateStateValueBudgets(
	vars map[string]any,
	results map[string]*engine.StepResult,
	frames map[string]*engine.ExecutionFrameState,
	pendingEvents []engine.Event,
	typedValues ...map[string]any,
) error {
	if len(results) > maxStoredValueEntries {
		return fmt.Errorf("runstore: step result count exceeds %d", maxStoredValueEntries)
	}
	budget := stateValueBudget{}
	checkValues := func(values map[string]any) error {
		if err := budget.addEntries(len(values)); err != nil {
			return err
		}
		for name, value := range values {
			encoded, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("value %q: %w", name, err)
			}
			if len(encoded) > maxStateBlobBytes {
				return fmt.Errorf("value %q: state value exceeds %d bytes", name, maxStateBlobBytes)
			}
			if err := budget.addBytes(int64(len(encoded))); err != nil {
				return err
			}
		}
		return nil
	}
	if err := checkValues(vars); err != nil {
		return err
	}
	for _, values := range typedValues {
		if err := checkValues(values); err != nil {
			return err
		}
	}
	for stepID, result := range results {
		if result == nil {
			continue
		}
		if err := checkValues(result.Output); err != nil {
			return fmt.Errorf("runstore: encode result %s output: %w", stepID, err)
		}
		if err := checkValues(result.Vars); err != nil {
			return fmt.Errorf("runstore: encode result %s vars: %w", stepID, err)
		}
		if err := checkValues(result.PublicOutputs); err != nil {
			return err
		}
	}
	for frameID, frame := range frames {
		if frame == nil {
			continue
		}
		if err := checkValues(frame.WorkingVars); err != nil {
			return fmt.Errorf("runstore: encode frame %s vars: %w", frameID, err)
		}
		for resultID, result := range frame.Results {
			if result == nil {
				continue
			}
			if err := checkValues(result.Output); err != nil {
				return fmt.Errorf("runstore: encode frame %s result %s output: %w", frameID, resultID, err)
			}
			if err := checkValues(result.Vars); err != nil {
				return fmt.Errorf("runstore: encode frame %s result %s vars: %w", frameID, resultID, err)
			}
			if err := checkValues(result.PublicOutputs); err != nil {
				return err
			}
		}
	}
	for index, event := range pendingEvents {
		if err := checkValues(event.Payload); err != nil {
			return fmt.Errorf("runstore: encode pending trace event %d: %w", index, err)
		}
	}
	return nil
}

// LoadState restores run state from the latest snapshot on disk.
func (s *DirRunStore) LoadState(ctx context.Context, runID string) (engine.RunState, error) {
	if ctx.Err() != nil {
		return engine.RunState{}, ctx.Err()
	}
	if err := validateRunID(runID); err != nil {
		return engine.RunState{}, err
	}
	dir := filepath.Join(s.RunDir(runID), "snapshots")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return engine.RunState{}, err
	}
	var latestCheckpoint string
	var latestCheckpointSequence int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.HasPrefix(name, "checkpoint-") {
			sequence, parseErr := parseCheckpointSequence(name)
			if parseErr == nil && sequence >= 0 && (latestCheckpoint == "" || sequence > latestCheckpointSequence) {
				latestCheckpoint = name
				latestCheckpointSequence = sequence
			}
			continue
		}
	}
	latest := latestCheckpoint
	if latest == "" {
		return engine.RunState{}, os.ErrNotExist
	}
	data, err := readFileBounded(filepath.Join(dir, latest), maxRunStateBytes)
	if err != nil {
		return engine.RunState{}, err
	}
	var snapshot runStateSnapshotV1
	if err := decodeJSONDocument(data, &snapshot); err != nil {
		return engine.RunState{}, err
	}
	if latestCheckpoint != "" && snapshot.CheckpointSequence != latestCheckpointSequence {
		return engine.RunState{}, fmt.Errorf(
			"runstore: checkpoint sequence %d does not match filename sequence %d",
			snapshot.CheckpointSequence, latestCheckpointSequence,
		)
	}
	if err := validateCursorStepIndexPresence(data, snapshot.CursorSet); err != nil {
		return engine.RunState{}, err
	}
	if snapshot.SchemaVersion != runStateSchemaV3 {
		return engine.RunState{}, fmt.Errorf("runstore: unsupported state schema version %q", snapshot.SchemaVersion)
	}
	if snapshot.RunID != runID {
		return engine.RunState{}, fmt.Errorf("runstore: persisted state run id %q does not match %q", snapshot.RunID, runID)
	}
	if err := validateInteractionStates(snapshot.Interactions); err != nil {
		return engine.RunState{}, err
	}
	if err := validateExecutionInvocationCounts(snapshot.ExecutionInvocationCounts); err != nil {
		return engine.RunState{}, err
	}
	if err := validateInteractionInvocationCounts(snapshot.InteractionInvocationCounts, snapshot.Interactions); err != nil {
		return engine.RunState{}, err
	}
	if err := validateDispatchStates(snapshot.WriterEpoch, snapshot.Dispatches); err != nil {
		return engine.RunState{}, err
	}
	scopes, err := s.scopeSetForResolutions(ctx, runID, snapshot.DynamicIncludes)
	if err != nil {
		return engine.RunState{}, err
	}
	if err := validateDynamicIncludeResolutions(snapshot.WriterEpoch, snapshot.DynamicIncludes, scopes); err != nil {
		return engine.RunState{}, err
	}
	if err := validatePendingHandoff(snapshot.Status, snapshot.PendingHandoff); err != nil {
		return engine.RunState{}, err
	}
	var vars map[string]any
	var stepResults map[string]*engine.StepResult
	var executionFrames map[string]*engine.ExecutionFrameState
	var pendingTraceEvents []engine.Event
	var typedValues map[string]any
	loader := newStateValueLoader(s, runID)
	if err := loader.preflight(
		snapshot.DurableVars, snapshot.DurableStepResults, snapshot.DurableExecutionFrames,
		snapshot.DurablePendingTraceEvents,
		snapshot.TypedState,
	); err != nil {
		return engine.RunState{}, err
	}
	vars, err = loader.restoreValueMap(snapshot.DurableVars)
	if err != nil {
		return engine.RunState{}, err
	}
	executionFrames, err = loader.restoreExecutionFrames(snapshot.DurableExecutionFrames)
	if err != nil {
		return engine.RunState{}, err
	}
	if err := validateExecutionFrameStates(snapshot.WriterEpoch, executionFrames); err != nil {
		return engine.RunState{}, err
	}
	stepResults, err = loader.restoreStepResults(snapshot.DurableStepResults)
	if err != nil {
		return engine.RunState{}, err
	}
	pendingTraceEvents, err = loader.restorePendingTraceEvents(snapshot.DurablePendingTraceEvents)
	if err != nil {
		return engine.RunState{}, err
	}
	if err := validatePendingTraceEvents(snapshot.RunID, snapshot.CommittedTraceSequence, pendingTraceEvents); err != nil {
		return engine.RunState{}, err
	}
	typedValues, err = loader.restoreValueMap(snapshot.TypedState)
	if err != nil {
		return engine.RunState{}, err
	}
	if err := validateExecutionStateRelationships(
		snapshot.WriterEpoch, snapshot.CursorSet, stepResults, snapshot.Interactions, snapshot.Dispatches, executionFrames, snapshot.DynamicIncludes,
	); err != nil {
		return engine.RunState{}, err
	}
	state := engine.RunState{
		RunID: snapshot.RunID, RunbookPath: snapshot.RunbookPath, Mode: snapshot.Mode, Status: snapshot.Status,
		WriterEpoch:        snapshot.WriterEpoch,
		CheckpointSequence: snapshot.CheckpointSequence, CommittedTraceSequence: snapshot.CommittedTraceSequence,
		PendingTraceEvents: pendingTraceEvents,
		PlanSnapshotDigest: snapshot.PlanSnapshotDigest, CursorSet: snapshot.CursorSet,
		CurrentStep: snapshot.CurrentStep, CurrentStepIndex: snapshot.CurrentStepIndex,
		Vars: vars, StepResults: stepResults,
		Interactions:                snapshot.Interactions,
		ExecutionInvocationCounts:   snapshot.ExecutionInvocationCounts,
		InteractionInvocationCounts: snapshot.InteractionInvocationCounts,
		Dispatches:                  snapshot.Dispatches,
		ExecutionFrames:             executionFrames,
		DynamicIncludes:             snapshot.DynamicIncludes,
		PendingHandoff:              snapshot.PendingHandoff,
		StartedAt:                   snapshot.StartedAt, UpdatedAt: snapshot.UpdatedAt,
		CompletedAt: snapshot.CompletedAt,
	}
	if err := restoreTypedState(&state, typedValues, snapshot.ResultsID, snapshot.DurableExecutionFrames, snapshot.DurableStepResults); err != nil {
		return engine.RunState{}, err
	}
	if len(typedValues) != 0 || snapshot.ResultsID != "" || hasTypedReferences(snapshot) {
		plan, err := s.LoadPlan(ctx, runID)
		if err != nil {
			return engine.RunState{}, err
		}
		if err := validateTypedPlanOrigin(state, plan); err != nil {
			return engine.RunState{}, err
		}
	}
	return state, nil
}

func validatePendingHandoff(status engine.RunStatus, request *engine.HandoffRequest) error {
	if request == nil {
		if status == engine.RunStatusHandoffPending {
			return errors.New("runstore: handoff-pending state has no request")
		}
		return nil
	}
	if status != engine.RunStatusHandoffPending || request.TargetRunbook == "" || request.ReasonCode == "" ||
		request.ReasonSummary == "" || request.StepID == "" || request.QualifiedNodeID == "" ||
		request.Invocation < 1 || request.RetryAttempt < 1 || request.OccurrenceSequence < 1 ||
		request.QualifiedNodeID != engine.DebugNodeID(request.CallPath, request.StepID) {
		return errors.New("runstore: invalid pending handoff")
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 1<<20 {
		return errors.New("runstore: pending handoff exceeds limits")
	}
	return nil
}

func validatePendingTraceEvents(runID string, committedSequence int64, events []engine.Event) error {
	if committedSequence < 0 || len(events) > 16 {
		return errors.New("runstore: invalid committed trace projection state")
	}
	previous := int64(0)
	for index, event := range events {
		if event.EventID == "" || event.RunID != runID || event.Sequence < 1 || event.Sequence > committedSequence ||
			event.Sequence <= previous || event.Kind == "" {
			return fmt.Errorf("runstore: pending trace event %d is malformed", index)
		}
		if _, err := time.Parse(time.RFC3339Nano, event.Timestamp); err != nil {
			return fmt.Errorf("runstore: pending trace event %d has invalid timestamp", index)
		}
		payload, err := json.Marshal(event.Payload)
		if err != nil || len(payload) > maxStateBlobBytes {
			return fmt.Errorf("runstore: pending trace event %d has invalid payload", index)
		}
		previous = event.Sequence
	}
	if len(events) > 0 && events[len(events)-1].Sequence != committedSequence {
		return errors.New("runstore: pending trace events do not reach the committed sequence")
	}
	if len(events) > 0 && events[len(events)-1].Kind != string(tracepkg.EventKindExecutionCommitted) {
		return errors.New("runstore: pending trace events do not end in an execution commit")
	}
	return nil
}

func validateInteractionStates(interactions map[string]*engine.InteractionState) error {
	if len(interactions) > maxInteractions {
		return fmt.Errorf("runstore: interaction state exceeds %d turns", maxInteractions)
	}
	for turnID, interaction := range interactions {
		if interaction == nil {
			return fmt.Errorf("runstore: interaction %q is null", turnID)
		}
		if turnID == "" || interaction.TurnID != turnID {
			return fmt.Errorf("runstore: interaction map key %q does not match turn id %q", turnID, interaction.TurnID)
		}
		if interaction.SchemaVersion != engine.InteractionStateSchemaV1 || interaction.OwnerStepID == "" ||
			interaction.NodeID == "" || interaction.StepID == "" || interaction.Kind == "" || interaction.Ordinal < 1 ||
			interaction.ExecutionInvocation < 0 ||
			interaction.RequestDigest == "" || len(interaction.Request) == 0 || len(interaction.Request) > maxInteractionBytes ||
			!json.Valid(interaction.Request) {
			return fmt.Errorf("runstore: interaction %q is malformed", turnID)
		}
		if interaction.FrameID != "" && (!validSHA256Digest(interaction.FrameID) || interaction.FrameStepIndex < 0) ||
			interaction.FrameID == "" && interaction.FrameStepIndex != 0 {
			return fmt.Errorf("runstore: interaction %q has invalid frame binding", turnID)
		}
		if interaction.RequestDigest != engine.InteractionPayloadDigest(interaction.Request) {
			return fmt.Errorf("runstore: interaction %q request digest mismatch", turnID)
		}
		switch interaction.Status {
		case engine.InteractionStatusPending:
			if interaction.AnswerDigest != "" || len(interaction.Answer) != 0 ||
				interaction.AnswerCommandID != "" || interaction.AnswerCommandDigest != "" {
				return fmt.Errorf("runstore: pending interaction %q contains an answer", turnID)
			}
		case engine.InteractionStatusAnswered:
			if interaction.AnswerDigest == "" || len(interaction.Answer) == 0 || len(interaction.Answer) > maxInteractionBytes ||
				!json.Valid(interaction.Answer) {
				return fmt.Errorf("runstore: answered interaction %q has no valid answer", turnID)
			}
			if interaction.AnswerDigest != engine.InteractionPayloadDigest(interaction.Answer) {
				return fmt.Errorf("runstore: interaction %q answer digest mismatch", turnID)
			}
			if interaction.AcceptedAt == "" || interaction.AuditToken == "" {
				return fmt.Errorf("runstore: answered interaction %q has no audit metadata", turnID)
			}
			if len(interaction.AnswerCommandID) > 128 || strings.ContainsAny(interaction.AnswerCommandID, "\r\n\x00") {
				return fmt.Errorf("runstore: answered interaction %q has invalid command id", turnID)
			}
			if interaction.AnswerCommandID != "" && !validSHA256Digest(interaction.AnswerCommandDigest) ||
				interaction.AnswerCommandID == "" && interaction.AnswerCommandDigest != "" {
				return fmt.Errorf("runstore: answered interaction %q has invalid command digest", turnID)
			}
			if _, err := time.Parse(time.RFC3339Nano, interaction.AcceptedAt); err != nil {
				return fmt.Errorf("runstore: answered interaction %q has invalid acceptance time", turnID)
			}
		default:
			return fmt.Errorf("runstore: interaction %q has unsupported status %q", turnID, interaction.Status)
		}
	}
	return nil
}

func validateInteractionInvocationCounts(
	counts map[string]int,
	interactions map[string]*engine.InteractionState,
) error {
	if len(counts) > maxStoredValueEntries {
		return fmt.Errorf("runstore: interaction invocation count exceeds %d identities", maxStoredValueEntries)
	}
	for key, count := range counts {
		parts := strings.Split(key, "\x00")
		validKey := len(parts) == 2 && parts[0] != "" && parts[1] != ""
		if len(parts) == 4 && parts[0] != "" && parts[1] != "" && validSHA256Digest(parts[2]) {
			index, err := strconv.Atoi(parts[3])
			validKey = err == nil && index >= 0 && strconv.Itoa(index) == parts[3]
		}
		if !validKey || count < 1 {
			return errors.New("runstore: malformed interaction invocation count")
		}
	}
	ordinalsByIdentity := make(map[string][]int)
	for _, interaction := range interactions {
		if interaction == nil {
			continue
		}
		key := engine.InteractionInvocationCountKey(
			interaction.NodeID, interaction.Kind, interaction.FrameID, interaction.FrameStepIndex,
		)
		ordinalsByIdentity[key] = append(ordinalsByIdentity[key], interaction.Ordinal)
	}
	for key, ordinals := range ordinalsByIdentity {
		count := counts[key]
		sort.Ints(ordinals)
		firstOrdinal := count - len(ordinals) + 1
		if firstOrdinal < 1 {
			return errors.New("runstore: interaction invocation count does not cover active ordinals")
		}
		for index, ordinal := range ordinals {
			if ordinal != firstOrdinal+index {
				return errors.New("runstore: interaction invocation count does not cover active ordinals")
			}
		}
	}
	return nil
}

func validateExecutionInvocationCounts(counts map[string]int) error {
	if len(counts) > maxStoredValueEntries {
		return fmt.Errorf("runstore: execution invocation count exceeds %d identities", maxStoredValueEntries)
	}
	for key, count := range counts {
		if key == "" || count < 1 {
			return errors.New("runstore: malformed execution invocation count")
		}
	}
	return nil
}

func validateDispatchStates(writerEpoch uint64, dispatches map[string]*engine.DispatchState) error {
	if len(dispatches) > maxStoredValueEntries {
		return fmt.Errorf("runstore: dispatch journal exceeds %d occurrences", maxStoredValueEntries)
	}
	for occurrenceID, dispatch := range dispatches {
		if dispatch == nil || occurrenceID == "" || dispatch.OccurrenceID != occurrenceID {
			return fmt.Errorf("runstore: dispatch map key %q does not match occurrence", occurrenceID)
		}
		if dispatch.SchemaVersion != engine.DispatchStateSchemaV1 || dispatch.WriterEpoch == 0 ||
			dispatch.QualifiedNodeID == "" || dispatch.QualifiedNodeID != engine.DebugNodeID(dispatch.CallPath, dispatch.StepID) ||
			dispatch.StepID == "" || len(dispatch.CallPath) > 128 || dispatch.Phase != engine.ExecutionPhaseExecute ||
			dispatch.Invocation < 1 || dispatch.RetryAttempt < 1 || dispatch.OccurrenceSequence < 1 ||
			len(dispatch.EndpointIdentity) > 1024 || !validSHA256Digest(dispatch.OccurrenceID) ||
			!validSHA256Digest(dispatch.RequestDigest) || !validSHA256Digest(dispatch.IdempotencyKey) {
			return fmt.Errorf("runstore: dispatch %q is malformed", occurrenceID)
		}
		if dispatch.FrameID != "" && (!validSHA256Digest(dispatch.FrameID) || dispatch.FrameStepIndex < 0) ||
			dispatch.FrameID == "" && dispatch.FrameStepIndex != 0 {
			return fmt.Errorf("runstore: dispatch %q has invalid frame binding", occurrenceID)
		}
		switch dispatch.Classification {
		case "read-only", "mutating", "destructive", "unspecified":
		default:
			return fmt.Errorf("runstore: dispatch %q has invalid classification", occurrenceID)
		}
		if _, err := time.Parse(time.RFC3339Nano, dispatch.PreparedAt); err != nil {
			return fmt.Errorf("runstore: dispatch %q has invalid preparation time", occurrenceID)
		}
		switch dispatch.Status {
		case engine.DispatchStatusPrepared:
			if dispatch.WriterEpoch != writerEpoch || dispatch.ResultDigest != "" || dispatch.SettledAt != "" {
				return fmt.Errorf("runstore: prepared dispatch %q has inconsistent state", occurrenceID)
			}
		case engine.DispatchStatusSettled:
			if !validSHA256Digest(dispatch.ResultDigest) || !validDispatchSettledAt(dispatch.SettledAt) {
				return fmt.Errorf("runstore: settled dispatch %q has inconsistent state", occurrenceID)
			}
		case engine.DispatchStatusIndeterminate:
			if dispatch.ResultDigest != "" || !validDispatchSettledAt(dispatch.SettledAt) {
				return fmt.Errorf("runstore: indeterminate dispatch %q has inconsistent state", occurrenceID)
			}
		default:
			return fmt.Errorf("runstore: dispatch %q has unsupported status %q", occurrenceID, dispatch.Status)
		}
	}
	return nil
}

func validateExecutionFrameStates(writerEpoch uint64, frames map[string]*engine.ExecutionFrameState) error {
	if len(frames) > maxStoredValueEntries {
		return fmt.Errorf("runstore: execution frame count exceeds %d", maxStoredValueEntries)
	}
	for frameID, frame := range frames {
		if frame == nil || frameID == "" || frame.FrameID != frameID || !validSHA256Digest(frameID) {
			return fmt.Errorf("runstore: execution frame map key %q does not match frame", frameID)
		}
		if frame.ParentFrameID != "" && (!validSHA256Digest(frame.ParentFrameID) || frame.ParentFrameID == frameID) {
			return fmt.Errorf("runstore: execution frame %q has invalid parent frame", frameID)
		}
		if frame.SchemaVersion != engine.ExecutionFrameStateSchemaV1 || frame.WriterEpoch == 0 ||
			frame.ParentQualifiedNodeID == "" || frame.ParentStepID == "" || len(frame.CallPath) == 0 || len(frame.CallPath) > 128 ||
			frame.CallPath[len(frame.CallPath)-1].StepID != frame.ParentStepID || frame.Invocation < 1 ||
			!validSHA256Digest(frame.DefinitionDigest) || frame.StepCount < 1 || len(frame.StepIDs) != frame.StepCount ||
			frame.NextStepIndex < 0 || frame.NextStepIndex > frame.StepCount || frame.IterationIndex < 0 {
			return fmt.Errorf("runstore: execution frame %q is malformed", frameID)
		}
		for _, stepID := range frame.StepIDs {
			if stepID == "" || len(stepID) > 4096 {
				return fmt.Errorf("runstore: execution frame %q has invalid child identity", frameID)
			}
		}
		expectedParentID := engine.DebugNodeID(frame.CallPath[:len(frame.CallPath)-1], frame.ParentStepID)
		if frame.ParentQualifiedNodeID != expectedParentID {
			return fmt.Errorf("runstore: execution frame %q parent identity is inconsistent", frameID)
		}
		switch frame.Kind {
		case "branch", "include", "iterate", "parallel", "compensate", "tool-substitution":
		default:
			return fmt.Errorf("runstore: execution frame %q has invalid kind %q", frameID, frame.Kind)
		}
		if _, err := time.Parse(time.RFC3339Nano, frame.StartedAt); err != nil {
			return fmt.Errorf("runstore: execution frame %q has invalid start time", frameID)
		}
		if len(frame.Results) != frame.NextStepIndex {
			return fmt.Errorf("runstore: execution frame %q results do not match next step", frameID)
		}
		for index := 0; index < frame.NextStepIndex; index++ {
			result := frame.Results[strconv.Itoa(index)]
			if result == nil || result.StepID == "" {
				return fmt.Errorf("runstore: execution frame %q result %d is missing", frameID, index)
			}
		}
		switch frame.Status {
		case engine.ExecutionFrameStatusActive:
			if frame.WriterEpoch != writerEpoch || frame.CompletedAt != "" {
				return fmt.Errorf("runstore: active execution frame %q has inconsistent state", frameID)
			}
		case engine.ExecutionFrameStatusCompleted:
			if frame.NextStepIndex != frame.StepCount || !validDispatchSettledAt(frame.CompletedAt) {
				return fmt.Errorf("runstore: completed execution frame %q has inconsistent state", frameID)
			}
		case engine.ExecutionFrameStatusFailed, engine.ExecutionFrameStatusIndeterminate:
			if !validDispatchSettledAt(frame.CompletedAt) {
				return fmt.Errorf("runstore: terminal execution frame %q has inconsistent state", frameID)
			}
		default:
			return fmt.Errorf("runstore: execution frame %q has unsupported status %q", frameID, frame.Status)
		}
	}
	return nil
}

func validateDynamicIncludeResolutions(
	writerEpoch uint64,
	resolutions map[string]*engine.DynamicIncludeResolutionState,
	scopeSets ...*toolscope.Set,
) error {
	if len(resolutions) > maxStoredValueEntries {
		return fmt.Errorf("runstore: dynamic include resolution count exceeds %d", maxStoredValueEntries)
	}
	revisions := make(map[int64]bool, len(resolutions))
	for resolutionID, resolution := range resolutions {
		if resolution == nil || resolutionID == "" || resolution.ResolutionID != resolutionID || !validSHA256Digest(resolutionID) {
			return fmt.Errorf("runstore: dynamic include resolution map key %q does not match resolution", resolutionID)
		}
		if err := engine.ValidateDynamicIncludeResolutionVersion(*resolution); err != nil {
			return err
		}
		if resolution.WriterEpoch == 0 ||
			resolution.QualifiedNodeID == "" || resolution.StepID == "" || resolution.Invocation < 1 ||
			resolution.Revision < 1 || revisions[resolution.Revision] ||
			len(resolution.CallPath) > 128 || resolution.QualifiedNodeID != engine.DebugNodeID(resolution.CallPath, resolution.StepID) {
			return fmt.Errorf("runstore: dynamic include resolution %q is malformed", resolutionID)
		}
		revisions[resolution.Revision] = true
		if resolution.FrameID != "" && (!validSHA256Digest(resolution.FrameID) || resolution.FrameStepIndex < 0) ||
			resolution.FrameID == "" && resolution.FrameStepIndex != 0 {
			return fmt.Errorf("runstore: dynamic include resolution %q has invalid frame binding", resolutionID)
		}
		pin := resolution.Pin
		if pin.StepID != resolution.StepID || pin.QualifiedNodeID != resolution.QualifiedNodeID ||
			pin.Invocation != resolution.Invocation || pin.Revision != resolution.Revision ||
			!boundedRequiredString(pin.RenderedRef, 4096) ||
			!boundedRequiredString(pin.QualifiedID, 4096) || !boundedRequiredString(pin.AbsPath, 32768) ||
			!boundedRequiredString(pin.PackageName, 4096) || !boundedRequiredString(pin.PackageVersion, 4096) ||
			!validSHA256Digest(pin.FileDigest) || !validSHA256Digest(pin.PackageDigest) {
			return fmt.Errorf("runstore: dynamic include resolution %q has invalid pin", resolutionID)
		}
		if len(pin.ExecutableClosure) > maxPlanSnapshotBytes {
			return fmt.Errorf("runstore: dynamic include resolution %q closure is too large", resolutionID)
		}
		if resolution.SchemaVersion == engine.DynamicIncludeResolutionStateSchemaV2 {
			if len(scopeSets) == 0 || scopeSets[0] == nil {
				return fmt.Errorf("runstore: scoped resolution requires its immutable plan")
			}
			if err := plansnapshot.ValidateDynamicIncludePin(pin, scopeSets[0]); err != nil {
				return fmt.Errorf("runstore: dynamic include resolution %q has invalid scoped closure: %w", resolutionID, err)
			}
		} else if len(pin.ExecutableClosure) > 0 {
			if err := plansnapshot.ValidateFlowClosure(pin.ExecutableClosure); err != nil {
				return fmt.Errorf("runstore: dynamic include resolution %q has invalid closure: %w", resolutionID, err)
			}
		} else if pin.ResolvedInputs != nil || pin.ResolvedOutputs != nil || pin.ResolvedGovernance != nil {
			return fmt.Errorf("runstore: dynamic include resolution %q has metadata without a closure", resolutionID)
		}
		if _, err := time.Parse(time.RFC3339Nano, resolution.CommittedAt); err != nil {
			return fmt.Errorf("runstore: dynamic include resolution %q has invalid commit time", resolutionID)
		}
		switch resolution.Status {
		case engine.DynamicIncludeResolutionStatusActive:
			if resolution.WriterEpoch != writerEpoch || resolution.CompletedAt != "" {
				return fmt.Errorf("runstore: active dynamic include resolution %q has inconsistent state", resolutionID)
			}
		case engine.DynamicIncludeResolutionStatusCompleted, engine.DynamicIncludeResolutionStatusFailed:
			if !validDispatchSettledAt(resolution.CompletedAt) {
				return fmt.Errorf("runstore: terminal dynamic include resolution %q has inconsistent state", resolutionID)
			}
		default:
			return fmt.Errorf("runstore: dynamic include resolution %q has unsupported status %q", resolutionID, resolution.Status)
		}
	}
	for revision := int64(1); revision <= int64(len(resolutions)); revision++ {
		if !revisions[revision] {
			return fmt.Errorf("runstore: dynamic include resolution revision %d is missing", revision)
		}
	}
	return nil
}

func validateExecutionStateRelationships(
	writerEpoch uint64,
	cursorSet *engine.ExecutionCursorSet,
	stepResults map[string]*engine.StepResult,
	interactions map[string]*engine.InteractionState,
	dispatches map[string]*engine.DispatchState,
	frames map[string]*engine.ExecutionFrameState,
	resolutions map[string]*engine.DynamicIncludeResolutionState,
) error {
	for turnID, interaction := range interactions {
		if interaction == nil || interaction.FrameID == "" {
			continue
		}
		frame := frames[interaction.FrameID]
		if frame == nil || frame.Status != engine.ExecutionFrameStatusActive ||
			interaction.FrameStepIndex != frame.NextStepIndex || interaction.FrameStepIndex >= frame.StepCount ||
			interaction.FrameStepIndex >= len(frame.StepIDs) || frame.StepIDs[interaction.FrameStepIndex] != interaction.StepID ||
			interaction.NodeID != engine.DebugNodeID(frame.CallPath, interaction.StepID) ||
			interaction.OwnerStepID != interaction.NodeID {
			return fmt.Errorf("runstore: interaction %q has inconsistent execution frame", turnID)
		}
	}
	for frameID, frame := range frames {
		if frame == nil {
			continue
		}
		if frame.StepCount > maxStoredValueEntries {
			return fmt.Errorf("runstore: execution frame %q exceeds %d children", frameID, maxStoredValueEntries)
		}
		if frame.ParentFrameID == "" {
			continue
		}
		parent := frames[frame.ParentFrameID]
		if parent == nil || len(frame.CallPath) != len(parent.CallPath)+1 ||
			!sameDebugCallPath(frame.CallPath[:len(parent.CallPath)], parent.CallPath) ||
			frame.CallPath[len(frame.CallPath)-1].StepID != frame.ParentStepID {
			return fmt.Errorf("runstore: execution frame %q has inconsistent parent chain", frameID)
		}
		parentStepIndex := indexOfString(parent.StepIDs, frame.ParentStepID)
		if parentStepIndex < 0 || parentStepIndex >= parent.NextStepIndex+1 {
			return fmt.Errorf("runstore: execution frame %q parent is not at its declared child", frameID)
		}
		if frame.Status == engine.ExecutionFrameStatusActive &&
			(parent.Status != engine.ExecutionFrameStatusActive || parentStepIndex != parent.NextStepIndex) {
			return fmt.Errorf("runstore: active execution frame %q has inactive parent progress", frameID)
		}
	}
	for occurrenceID, dispatch := range dispatches {
		if dispatch == nil {
			continue
		}
		if dispatch.FrameID == "" {
			if len(dispatch.CallPath) != 0 || dispatch.QualifiedNodeID != engine.DebugNodeID(nil, dispatch.StepID) {
				return fmt.Errorf("runstore: dispatch %q has a nested call path without an execution frame", occurrenceID)
			}
			continue
		}
		frame := frames[dispatch.FrameID]
		if frame == nil || dispatch.FrameStepIndex < 0 || dispatch.FrameStepIndex >= frame.StepCount ||
			dispatch.FrameStepIndex >= len(frame.StepIDs) || frame.StepIDs[dispatch.FrameStepIndex] != dispatch.StepID ||
			!sameDebugCallPath(frame.CallPath, dispatch.CallPath) {
			return fmt.Errorf("runstore: dispatch %q has inconsistent execution frame", occurrenceID)
		}
		if dispatch.Status == engine.DispatchStatusPrepared {
			if frame.Status != engine.ExecutionFrameStatusActive || dispatch.FrameStepIndex != frame.NextStepIndex {
				return fmt.Errorf("runstore: prepared dispatch %q is not at active frame progress", occurrenceID)
			}
		} else if (dispatch.FrameStepIndex >= frame.NextStepIndex || frame.Results[strconv.Itoa(dispatch.FrameStepIndex)] == nil) &&
			!matchesActiveDynamicResolutionDispatch(dispatch, resolutions) {
			return fmt.Errorf("runstore: settled dispatch %q has no frame result", occurrenceID)
		}
	}
	for resolutionID, resolution := range resolutions {
		if resolution == nil {
			continue
		}
		if resolution.FrameID == "" {
			if len(resolution.CallPath) != 0 || resolution.QualifiedNodeID != engine.DebugNodeID(nil, resolution.StepID) {
				return fmt.Errorf("runstore: dynamic include resolution %q has a nested call path without an execution frame", resolutionID)
			}
			continue
		}
		frame := frames[resolution.FrameID]
		if frame == nil || resolution.FrameStepIndex < 0 || resolution.FrameStepIndex >= frame.StepCount ||
			resolution.FrameStepIndex >= len(frame.StepIDs) || frame.StepIDs[resolution.FrameStepIndex] != resolution.StepID ||
			!sameDebugCallPath(frame.CallPath, resolution.CallPath) {
			return fmt.Errorf("runstore: dynamic include resolution %q has inconsistent execution frame", resolutionID)
		}
		if resolution.Status == engine.DynamicIncludeResolutionStatusActive &&
			(frame.Status != engine.ExecutionFrameStatusActive || resolution.FrameStepIndex != frame.NextStepIndex) {
			return fmt.Errorf("runstore: active dynamic include resolution %q is not at frame progress", resolutionID)
		}
		if resolution.Status != engine.DynamicIncludeResolutionStatusActive && resolution.FrameStepIndex >= frame.NextStepIndex {
			return fmt.Errorf("runstore: terminal dynamic include resolution %q has no frame result", resolutionID)
		}
	}
	if err := validateDynamicResolutionDispatches(dispatches, resolutions, stepResults, frames); err != nil {
		return err
	}
	if cursorSet == nil {
		return nil
	}
	if cursorSet.SchemaVersion != engine.ExecutionCursorSchemaV1 || len(cursorSet.Cursors) == 0 ||
		len(cursorSet.Cursors) > maxStoredValueEntries {
		return errors.New("runstore: invalid execution cursor set")
	}
	seenFrames := make(map[string]bool, len(cursorSet.Cursors))
	for _, cursor := range cursorSet.Cursors {
		if cursor.FrameID == "" {
			if writerEpoch > 0 && len(cursor.CallPath) > 0 {
				return errors.New("runstore: nested execution cursor has no frame")
			}
			continue
		}
		if seenFrames[cursor.FrameID] {
			return errors.New("runstore: execution cursor frame is duplicated")
		}
		seenFrames[cursor.FrameID] = true
		frame := frames[cursor.FrameID]
		if frame == nil || frame.Status != engine.ExecutionFrameStatusActive ||
			frame.NextStepIndex < 0 || frame.NextStepIndex >= frame.StepCount || frame.NextStepIndex >= len(frame.StepIDs) ||
			frame.StepIDs[frame.NextStepIndex] != cursor.StepID || !sameDebugCallPath(frame.CallPath, cursor.CallPath) ||
			cursor.QualifiedNodeID != engine.DebugNodeID(cursor.CallPath, cursor.StepID) ||
			cursor.BranchLabel != frame.BranchLabel || cursor.IterationIndex != frame.IterationIndex {
			return errors.New("runstore: execution cursor does not match active frame child")
		}
		prepared := false
		for _, dispatch := range dispatches {
			if dispatch != nil && dispatch.Status == engine.DispatchStatusPrepared &&
				dispatch.FrameID == frame.FrameID && dispatch.FrameStepIndex == frame.NextStepIndex {
				prepared = true
				break
			}
		}
		if prepared && cursor.Phase != engine.ExecutionPhaseExecute || !prepared && cursor.Phase != engine.ExecutionPhaseBefore {
			return errors.New("runstore: execution cursor phase does not match frame dispatch state")
		}
	}
	return nil
}

func matchesActiveDynamicResolutionDispatch(
	dispatch *engine.DispatchState,
	resolutions map[string]*engine.DynamicIncludeResolutionState,
) bool {
	if dispatch == nil || dispatch.Status != engine.DispatchStatusSettled ||
		dispatch.EndpointIdentity != "dynamic-include-resolver" {
		return false
	}
	for _, resolution := range resolutions {
		if resolution == nil || resolution.Status != engine.DynamicIncludeResolutionStatusActive ||
			dispatch.QualifiedNodeID != resolution.QualifiedNodeID || dispatch.StepID != resolution.StepID ||
			dispatch.FrameID != resolution.FrameID || dispatch.FrameStepIndex != resolution.FrameStepIndex ||
			dispatch.Invocation != resolution.Invocation || dispatch.Phase != engine.ExecutionPhaseExecute ||
			!sameDebugCallPath(dispatch.CallPath, resolution.CallPath) {
			continue
		}
		encodedPin, err := json.Marshal(resolution.Pin)
		return err == nil && dispatch.ResultDigest == engine.InteractionPayloadDigest(encodedPin)
	}
	return false
}

func validateDynamicResolutionDispatches(
	dispatches map[string]*engine.DispatchState,
	resolutions map[string]*engine.DynamicIncludeResolutionState,
	stepResults map[string]*engine.StepResult,
	frames map[string]*engine.ExecutionFrameState,
) error {
	paired := make(map[string]bool, len(resolutions))
	matchedSkippedResults := make(map[string]string)
	for resolutionID, resolution := range resolutions {
		if resolution == nil {
			continue
		}
		encodedPin, err := json.Marshal(resolution.Pin)
		if err != nil {
			return fmt.Errorf("runstore: dynamic include resolution %q pin cannot be paired", resolutionID)
		}
		expectedResultDigest := engine.InteractionPayloadDigest(encodedPin)
		matched := ""
		for occurrenceID, dispatch := range dispatches {
			if dispatch == nil || dispatch.Status != engine.DispatchStatusSettled ||
				dispatch.Classification != "read-only" || dispatch.EndpointIdentity != "dynamic-include-resolver" ||
				dispatch.QualifiedNodeID != resolution.QualifiedNodeID || dispatch.StepID != resolution.StepID ||
				dispatch.FrameID != resolution.FrameID || dispatch.FrameStepIndex != resolution.FrameStepIndex ||
				dispatch.Invocation != resolution.Invocation || dispatch.Phase != engine.ExecutionPhaseExecute ||
				dispatch.ResultDigest != expectedResultDigest || !sameDebugCallPath(dispatch.CallPath, resolution.CallPath) {
				continue
			}
			if matched != "" {
				return fmt.Errorf("runstore: dynamic include resolution %q has ambiguous resolver dispatch provenance", resolutionID)
			}
			matched = occurrenceID
		}
		if matched == "" {
			return fmt.Errorf("runstore: dynamic include resolution %q has no settled resolver dispatch provenance", resolutionID)
		}
		paired[matched] = true
	}
	for occurrenceID, dispatch := range dispatches {
		if dispatch == nil || dispatch.Status != engine.DispatchStatusSettled ||
			dispatch.EndpointIdentity != "dynamic-include-resolver" || paired[occurrenceID] {
			continue
		}
		resultIdentity, matched := matchesSkippedDynamicIncludeResult(dispatch, stepResults, frames)
		if !matched {
			resultIdentity, matched = matchesHistoricalDynamicIncludeNotFound(dispatch, dispatches, resolutions)
		}
		if !matched {
			return fmt.Errorf("runstore: settled resolver dispatch %q has no dynamic include resolution", occurrenceID)
		}
		if previous := matchedSkippedResults[resultIdentity]; previous != "" {
			return fmt.Errorf("runstore: settled resolver dispatches %q and %q have ambiguous not-found result provenance", previous, occurrenceID)
		}
		matchedSkippedResults[resultIdentity] = occurrenceID
	}
	return nil
}

func matchesHistoricalDynamicIncludeNotFound(
	dispatch *engine.DispatchState,
	dispatches map[string]*engine.DispatchState,
	resolutions map[string]*engine.DynamicIncludeResolutionState,
) (string, bool) {
	canonical := &engine.StepResult{
		StepID: dispatch.StepID, Status: engine.StepStatusSkipped, Outcome: engine.StepOutcomeSkipped,
		Output: map[string]any{"skip_reason": "include_not_found"},
		Vars:   map[string]any{"runbook_found": false},
	}
	digest, ok := engine.DynamicIncludeNotFoundResultDigest(canonical)
	if !ok || dispatch.ResultDigest != digest {
		return "", false
	}
	for _, resolution := range resolutions {
		if resolution == nil || resolution.Invocation <= dispatch.Invocation ||
			resolution.QualifiedNodeID != dispatch.QualifiedNodeID || resolution.StepID != dispatch.StepID ||
			resolution.FrameID != dispatch.FrameID || resolution.FrameStepIndex != dispatch.FrameStepIndex ||
			!sameDebugCallPath(resolution.CallPath, dispatch.CallPath) {
			continue
		}
		successPaired := false
		for _, successor := range dispatches {
			if successor == nil || successor.Status != engine.DispatchStatusSettled ||
				successor.EndpointIdentity != "dynamic-include-resolver" || successor.Invocation != resolution.Invocation ||
				successor.QualifiedNodeID != resolution.QualifiedNodeID || successor.StepID != resolution.StepID ||
				successor.FrameID != resolution.FrameID || successor.FrameStepIndex != resolution.FrameStepIndex ||
				!sameDebugCallPath(successor.CallPath, resolution.CallPath) {
				continue
			}
			encodedPin, err := json.Marshal(resolution.Pin)
			if err == nil && successor.ResultDigest == engine.InteractionPayloadDigest(encodedPin) {
				successPaired = true
				break
			}
		}
		if !successPaired {
			continue
		}
		for invocation := dispatch.Invocation + 1; invocation < resolution.Invocation; invocation++ {
			foundNegative := false
			for _, intermediate := range dispatches {
				if intermediate == nil || intermediate.Status != engine.DispatchStatusSettled ||
					intermediate.EndpointIdentity != "dynamic-include-resolver" || intermediate.Invocation != invocation ||
					intermediate.QualifiedNodeID != dispatch.QualifiedNodeID || intermediate.StepID != dispatch.StepID ||
					intermediate.FrameID != dispatch.FrameID || intermediate.FrameStepIndex != dispatch.FrameStepIndex ||
					intermediate.ResultDigest != digest || !sameDebugCallPath(intermediate.CallPath, dispatch.CallPath) {
					continue
				}
				if foundNegative {
					return "", false
				}
				foundNegative = true
			}
			if !foundNegative {
				return "", false
			}
		}
		return fmt.Sprintf("historical:%s:%s:%d", dispatch.FrameID, dispatch.QualifiedNodeID, dispatch.Invocation), true
	}
	return "", false
}

func matchesSkippedDynamicIncludeResult(
	dispatch *engine.DispatchState,
	stepResults map[string]*engine.StepResult,
	frames map[string]*engine.ExecutionFrameState,
) (string, bool) {
	var result *engine.StepResult
	resultIdentity := ""
	if dispatch.FrameID == "" {
		if len(dispatch.CallPath) != 0 || dispatch.QualifiedNodeID != engine.DebugNodeID(nil, dispatch.StepID) {
			return "", false
		}
		result = stepResults[dispatch.StepID]
		resultIdentity = fmt.Sprintf("root:%s:%d", dispatch.StepID, dispatch.Invocation)
	} else if frame := frames[dispatch.FrameID]; frame != nil {
		if dispatch.FrameStepIndex < 0 || dispatch.FrameStepIndex >= len(frame.StepIDs) ||
			frame.StepIDs[dispatch.FrameStepIndex] != dispatch.StepID ||
			!sameDebugCallPath(dispatch.CallPath, frame.CallPath) ||
			dispatch.QualifiedNodeID != engine.DebugNodeID(frame.CallPath, dispatch.StepID) {
			return "", false
		}
		result = frame.Results[strconv.Itoa(dispatch.FrameStepIndex)]
		resultIdentity = fmt.Sprintf("frame:%s:%d:%d", dispatch.FrameID, dispatch.FrameStepIndex, dispatch.Invocation)
	}
	if result == nil || result.StepID != dispatch.StepID || result.Status != engine.StepStatusSkipped ||
		result.Outcome != engine.StepOutcomeSkipped || result.Output["skip_reason"] != "include_not_found" ||
		result.Vars["runbook_found"] != false || result.Error != nil {
		return "", false
	}
	digest, ok := engine.DynamicIncludeNotFoundResultDigest(result)
	return resultIdentity, ok && dispatch.ResultDigest == digest
}

func sameDebugCallPath(left, right []engine.DebugCallFrame) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func indexOfString(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func boundedRequiredString(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && !strings.ContainsAny(value, "\x00\r\n")
}

func validDispatchSettledAt(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func validSHA256Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func (s *DirRunStore) snapshotStepResultsV2(runID string, results map[string]*engine.StepResult) (map[string]*stepResultSnapshotV2, error) {
	if results == nil {
		return nil, nil
	}
	snapshots := make(map[string]*stepResultSnapshotV2, len(results))
	for stepID, result := range results {
		if result == nil {
			snapshots[stepID] = nil
			continue
		}
		output, err := s.storeValueMap(runID, result.Output)
		if err != nil {
			return nil, fmt.Errorf("runstore: encode result %s output: %w", stepID, err)
		}
		vars, err := s.storeValueMap(runID, result.Vars)
		if err != nil {
			return nil, fmt.Errorf("runstore: encode result %s vars: %w", stepID, err)
		}
		errorText := ""
		public, err := s.storeValueMap(runID, result.PublicOutputs)
		if err != nil {
			return nil, err
		}
		if result.Error != nil {
			errorText = result.Error.Error()
		}
		snapshots[stepID] = &stepResultSnapshotV2{
			PublicOutputs: public, ResultsID: publicationID(result.Results), RequiredFailure: result.RequiredFailure,
			TerminalResults: result.TerminalResults,
			StepID:          result.StepID, Status: result.Status, Outcome: result.Outcome,
			Output: output, Vars: vars, StartedAt: result.StartedAt,
			CompletedAt: result.CompletedAt, DurationMs: result.DurationMs, Error: errorText,
			Evidence: result.Evidence, Indeterminate: result.Indeterminate,
		}
	}
	return snapshots, nil
}

func (s *DirRunStore) snapshotExecutionFrames(
	runID string,
	frames map[string]*engine.ExecutionFrameState,
) (map[string]*executionFrameSnapshotV1, error) {
	if frames == nil {
		return nil, nil
	}
	snapshots := make(map[string]*executionFrameSnapshotV1, len(frames))
	for frameID, frame := range frames {
		if frame == nil {
			snapshots[frameID] = nil
			continue
		}
		workingVars, err := s.storeValueMap(runID, frame.WorkingVars)
		if err != nil {
			return nil, fmt.Errorf("runstore: encode frame %s vars: %w", frameID, err)
		}
		results, err := s.snapshotStepResultsV2(runID, frame.Results)
		if err != nil {
			return nil, fmt.Errorf("runstore: encode frame %s results: %w", frameID, err)
		}
		snapshots[frameID] = &executionFrameSnapshotV1{
			RunResultsID:  publicationID(frame.RunResults),
			SchemaVersion: frame.SchemaVersion, FrameID: frame.FrameID, ParentFrameID: frame.ParentFrameID,
			WriterEpoch: frame.WriterEpoch, ParentQualifiedNodeID: frame.ParentQualifiedNodeID,
			ParentStepID: frame.ParentStepID, Kind: frame.Kind,
			CallPath:    append([]engine.DebugCallFrame(nil), frame.CallPath...),
			BranchLabel: frame.BranchLabel, IterationIndex: frame.IterationIndex, Invocation: frame.Invocation,
			DefinitionDigest: frame.DefinitionDigest, StepCount: frame.StepCount,
			StepIDs: append([]string(nil), frame.StepIDs...), NextStepIndex: frame.NextStepIndex,
			DurableWorkingVars: workingVars, DurableResults: results, Status: frame.Status,
			StartedAt: frame.StartedAt, CompletedAt: frame.CompletedAt,
		}
	}
	return snapshots, nil
}

func (s *DirRunStore) snapshotPendingTraceEvents(
	runID string,
	events []engine.Event,
) ([]pendingTraceEventSnapshotV1, error) {
	if events == nil {
		return nil, nil
	}
	snapshots := make([]pendingTraceEventSnapshotV1, len(events))
	for index, event := range events {
		payload, err := s.storeValueMap(runID, event.Payload)
		if err != nil {
			return nil, fmt.Errorf("runstore: encode pending trace event %d: %w", index, err)
		}
		snapshots[index] = pendingTraceEventSnapshotV1{
			EventID: event.EventID, RunID: event.RunID, RunbookID: event.RunbookID,
			Timestamp: event.Timestamp, Kind: event.Kind, Sequence: event.Sequence, DurablePayload: payload,
		}
	}
	return snapshots, nil
}

type stateValueLoader struct {
	store        *DirRunStore
	runID        string
	encodedBlobs map[string][]byte
}

func newStateValueLoader(store *DirRunStore, runID string) *stateValueLoader {
	return &stateValueLoader{store: store, runID: runID, encodedBlobs: make(map[string][]byte)}
}

func (loader *stateValueLoader) preflight(
	vars map[string]storedJSONValueV1,
	results map[string]*stepResultSnapshotV2,
	frames map[string]*executionFrameSnapshotV1,
	pendingEvents []pendingTraceEventSnapshotV1,
	typedValues ...map[string]storedJSONValueV1,
) error {
	if len(results) > maxStoredValueEntries || len(frames) > maxStoredValueEntries {
		return fmt.Errorf("runstore: step result count exceeds %d", maxStoredValueEntries)
	}
	budget := stateValueBudget{}
	blobSizes := make(map[string]int64)
	checkValues := func(values map[string]storedJSONValueV1) error {
		if err := budget.addEntries(len(values)); err != nil {
			return err
		}
		for name, stored := range values {
			size, err := validateStoredJSONValue(stored)
			if err != nil {
				return fmt.Errorf("value %q: %w", name, err)
			}
			if err := budget.addBytes(size); err != nil {
				return err
			}
			if stored.Blob == nil {
				continue
			}
			if previousSize, ok := blobSizes[stored.Blob.Digest]; ok && previousSize != stored.Blob.UncompressedSize {
				return errors.New("runstore: conflicting state blob references")
			}
			blobSizes[stored.Blob.Digest] = stored.Blob.UncompressedSize
		}
		return nil
	}
	if err := checkValues(vars); err != nil {
		return err
	}
	for _, values := range typedValues {
		if err := checkValues(values); err != nil {
			return err
		}
	}
	for stepID, result := range results {
		if result == nil {
			continue
		}
		if err := checkValues(result.Output); err != nil {
			return fmt.Errorf("runstore: restore result %s output: %w", stepID, err)
		}
		if err := checkValues(result.Vars); err != nil {
			return fmt.Errorf("runstore: restore result %s vars: %w", stepID, err)
		}
		if err := checkValues(result.PublicOutputs); err != nil {
			return err
		}
	}
	for frameID, frame := range frames {
		if frame == nil {
			continue
		}
		if err := checkValues(frame.DurableWorkingVars); err != nil {
			return fmt.Errorf("runstore: restore frame %s vars: %w", frameID, err)
		}
		for resultID, result := range frame.DurableResults {
			if result == nil {
				continue
			}
			if err := checkValues(result.Output); err != nil {
				return fmt.Errorf("runstore: restore frame %s result %s output: %w", frameID, resultID, err)
			}
			if err := checkValues(result.Vars); err != nil {
				return fmt.Errorf("runstore: restore frame %s result %s vars: %w", frameID, resultID, err)
			}
			if err := checkValues(result.PublicOutputs); err != nil {
				return err
			}
		}
	}
	for index, event := range pendingEvents {
		if err := checkValues(event.DurablePayload); err != nil {
			return fmt.Errorf("runstore: restore pending trace event %d: %w", index, err)
		}
	}
	return nil
}

func (loader *stateValueLoader) restoreStepResults(snapshots map[string]*stepResultSnapshotV2) (map[string]*engine.StepResult, error) {
	if snapshots == nil {
		return nil, nil
	}
	if len(snapshots) > maxStoredValueEntries {
		return nil, fmt.Errorf("runstore: step result count exceeds %d", maxStoredValueEntries)
	}
	results := make(map[string]*engine.StepResult, len(snapshots))
	for stepID, snapshot := range snapshots {
		if snapshot == nil {
			results[stepID] = nil
			continue
		}
		output, err := loader.restoreValueMap(snapshot.Output)
		if err != nil {
			return nil, fmt.Errorf("runstore: restore result %s output: %w", stepID, err)
		}
		vars, err := loader.restoreValueMap(snapshot.Vars)
		if err != nil {
			return nil, fmt.Errorf("runstore: restore result %s vars: %w", stepID, err)
		}
		result := &engine.StepResult{
			RequiredFailure: snapshot.RequiredFailure,
			TerminalResults: snapshot.TerminalResults,
			StepID:          snapshot.StepID, Status: snapshot.Status, Outcome: snapshot.Outcome,
			Output: output, Vars: vars, StartedAt: snapshot.StartedAt,
			CompletedAt: snapshot.CompletedAt, DurationMs: snapshot.DurationMs,
			Evidence: snapshot.Evidence, Indeterminate: snapshot.Indeterminate,
		}
		result.PublicOutputs, err = loader.restoreValueMap(snapshot.PublicOutputs)
		if err != nil {
			return nil, err
		}
		if snapshot.Error != "" {
			result.Error = errors.New(snapshot.Error)
		}
		results[stepID] = result
	}
	return results, nil
}

func (loader *stateValueLoader) restoreExecutionFrames(
	snapshots map[string]*executionFrameSnapshotV1,
) (map[string]*engine.ExecutionFrameState, error) {
	if snapshots == nil {
		return nil, nil
	}
	frames := make(map[string]*engine.ExecutionFrameState, len(snapshots))
	for frameID, snapshot := range snapshots {
		if snapshot == nil {
			frames[frameID] = nil
			continue
		}
		workingVars, err := loader.restoreValueMap(snapshot.DurableWorkingVars)
		if err != nil {
			return nil, fmt.Errorf("runstore: restore frame %s vars: %w", frameID, err)
		}
		results, err := loader.restoreStepResults(snapshot.DurableResults)
		if err != nil {
			return nil, fmt.Errorf("runstore: restore frame %s results: %w", frameID, err)
		}
		frames[frameID] = &engine.ExecutionFrameState{
			SchemaVersion: snapshot.SchemaVersion, FrameID: snapshot.FrameID, ParentFrameID: snapshot.ParentFrameID,
			WriterEpoch: snapshot.WriterEpoch, ParentQualifiedNodeID: snapshot.ParentQualifiedNodeID,
			ParentStepID: snapshot.ParentStepID, Kind: snapshot.Kind,
			CallPath:    append([]engine.DebugCallFrame(nil), snapshot.CallPath...),
			BranchLabel: snapshot.BranchLabel, IterationIndex: snapshot.IterationIndex, Invocation: snapshot.Invocation,
			DefinitionDigest: snapshot.DefinitionDigest, StepCount: snapshot.StepCount,
			StepIDs: append([]string(nil), snapshot.StepIDs...), NextStepIndex: snapshot.NextStepIndex,
			WorkingVars: workingVars, Results: results, Status: snapshot.Status,
			StartedAt: snapshot.StartedAt, CompletedAt: snapshot.CompletedAt,
		}
	}
	return frames, nil
}

func (loader *stateValueLoader) restorePendingTraceEvents(
	snapshots []pendingTraceEventSnapshotV1,
) ([]engine.Event, error) {
	if snapshots == nil {
		return nil, nil
	}
	events := make([]engine.Event, len(snapshots))
	for index, snapshot := range snapshots {
		payload, err := loader.restoreValueMap(snapshot.DurablePayload)
		if err != nil {
			return nil, fmt.Errorf("runstore: restore pending trace event %d: %w", index, err)
		}
		events[index] = engine.Event{
			EventID: snapshot.EventID, RunID: snapshot.RunID, RunbookID: snapshot.RunbookID,
			Timestamp: snapshot.Timestamp, Kind: snapshot.Kind, Sequence: snapshot.Sequence, Payload: payload,
		}
	}
	return events, nil
}

func (s *DirRunStore) storeValueMap(runID string, values map[string]any) (map[string]storedJSONValueV1, error) {
	if values == nil {
		return nil, nil
	}
	stored := make(map[string]storedJSONValueV1, len(values))
	for name, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("value %q: %w", name, err)
		}
		if len(encoded) <= inlineStateValueBytes {
			stored[name] = storedJSONValueV1{Inline: encoded}
			continue
		}
		ref, err := s.writeStateBlob(runID, encoded)
		if err != nil {
			return nil, fmt.Errorf("value %q: %w", name, err)
		}
		stored[name] = storedJSONValueV1{Blob: ref}
	}
	return stored, nil
}

func (loader *stateValueLoader) restoreValueMap(stored map[string]storedJSONValueV1) (map[string]any, error) {
	if stored == nil {
		return nil, nil
	}
	values := make(map[string]any, len(stored))
	for name, value := range stored {
		decoded, err := loader.decodeValue(value)
		if err != nil {
			return nil, fmt.Errorf("value %q: %w", name, err)
		}
		values[name] = decoded
	}
	return values, nil
}

func (loader *stateValueLoader) decodeValue(stored storedJSONValueV1) (any, error) {
	if len(stored.Inline) > 0 {
		return decodeStoredJSON(stored.Inline)
	}
	ref := stored.Blob
	encoded, ok := loader.encodedBlobs[ref.Digest]
	if !ok {
		var err error
		encoded, err = loader.store.readStateBlob(loader.runID, ref)
		if err != nil {
			return nil, err
		}
		loader.encodedBlobs[ref.Digest] = encoded
	}
	return decodeStoredJSON(encoded)
}

func decodeStoredJSON(encoded []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (s *DirRunStore) writeStateBlob(runID string, data []byte) (*stateBlobRefV1, error) {
	if len(data) > maxStateBlobBytes {
		return nil, fmt.Errorf("state value exceeds %d bytes", maxStateBlobBytes)
	}
	digestBytes := sha256.Sum256(data)
	digestHex := fmt.Sprintf("%x", digestBytes)
	ref := &stateBlobRefV1{
		Digest: "sha256:" + digestHex, Encoding: "json+gzip", UncompressedSize: int64(len(data)),
	}
	directory := filepath.Join(s.RunDir(runID), "blobs")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, digestHex+".json.gz")
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() > maxCompressedBlobBytes {
		return nil, fmt.Errorf("compressed state value exceeds %d bytes", maxCompressedBlobBytes)
	}
	if err := writeFileExclusive(path, compressed.Bytes()); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if syncErr := syncDirectory(directory); syncErr != nil {
			return nil, syncErr
		}
	}
	return ref, nil
}

func validateStoredJSONValue(stored storedJSONValueV1) (int64, error) {
	if len(stored.Inline) > 0 && stored.Blob != nil || len(stored.Inline) == 0 && stored.Blob == nil {
		return 0, errors.New("invalid stored state value")
	}
	if len(stored.Inline) > 0 {
		if len(stored.Inline) > inlineStateValueBytes || !json.Valid(stored.Inline) {
			return 0, errors.New("invalid inline JSON value")
		}
		return int64(len(stored.Inline)), nil
	}
	if err := validateStateBlobRef(stored.Blob); err != nil {
		return 0, err
	}
	return stored.Blob.UncompressedSize, nil
}

func validateStateBlobRef(ref *stateBlobRefV1) error {
	if ref == nil || ref.Encoding != "json+gzip" || !strings.HasPrefix(ref.Digest, "sha256:") || len(ref.Digest) != 71 ||
		ref.UncompressedSize < 0 || ref.UncompressedSize > maxStateBlobBytes {
		return errors.New("invalid state blob reference")
	}
	hexDigest := strings.TrimPrefix(ref.Digest, "sha256:")
	decodedDigest, err := hex.DecodeString(hexDigest)
	if err != nil || len(decodedDigest) != sha256.Size || hex.EncodeToString(decodedDigest) != hexDigest {
		return errors.New("invalid state blob reference")
	}
	return nil
}

func (s *DirRunStore) readStateBlob(runID string, ref *stateBlobRefV1) ([]byte, error) {
	if err := validateStateBlobRef(ref); err != nil {
		return nil, err
	}
	hexDigest := strings.TrimPrefix(ref.Digest, "sha256:")
	compressed, err := readFileBounded(filepath.Join(s.RunDir(runID), "blobs", hexDigest+".json.gz"), maxCompressedBlobBytes)
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxStateBlobBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxStateBlobBytes || int64(len(data)) != ref.UncompressedSize {
		return nil, errors.New("state blob size mismatch")
	}
	digest := sha256.Sum256(data)
	if "sha256:"+fmt.Sprintf("%x", digest) != ref.Digest {
		return nil, errors.New("state blob digest mismatch")
	}
	if !json.Valid(data) {
		return nil, errors.New("state blob is not valid JSON")
	}
	return data, nil
}

func (s *DirRunStore) planDigestForState(ctx context.Context, runID string) (string, error) {
	if digest, ok := s.PlanDigest(runID); ok {
		return digest, nil
	}
	if _, err := s.LoadPlan(ctx, runID); err != nil {
		return "", err
	}
	digest, ok := s.PlanDigest(runID)
	if !ok || digest == "" {
		return "", errors.New("runstore: loaded plan has no snapshot digest")
	}
	return digest, nil
}

// WriteTrace appends a trace event to the run's JSONL file.
func (s *DirRunStore) WriteTrace(ctx context.Context, runID string, event engine.Event) error {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := validateRunID(runID); err != nil {
		return err
	}
	if err := s.validateWriterEpoch(runID, engine.RunWriterEpochFromContext(ctx)); err != nil {
		return err
	}
	writer, err := s.writerFor(runID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return err
	}
	traceEvent := tracepkg.TraceEvent{
		EventID:   event.EventID,
		RunID:     event.RunID,
		RunbookID: event.RunbookID,
		Timestamp: event.Timestamp,
		Kind:      tracepkg.EventKind(event.Kind),
		Sequence:  event.Sequence,
		Payload:   payload,
	}
	return writer.Append(traceEvent)
}

// ListRuns scans all run directories and returns a snapshot of each run's latest state.
// Runs without a valid snapshot are silently skipped.
func (s *DirRunStore) ListRuns(ctx context.Context) ([]engine.RunState, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var runs []engine.RunState
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		state, err := s.LoadState(ctx, runID)
		if err != nil {
			// Skip runs with missing or corrupt snapshots.
			continue
		}
		runs = append(runs, state)
	}
	return runs, nil
}

// DeleteRun removes the run directory and all its contents.
// It returns nil if the run directory does not exist.
func (s *DirRunStore) DeleteRun(ctx context.Context, runID string) error {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := validateRunID(runID); err != nil {
		return err
	}
	if err := os.MkdirAll(s.baseDir, 0o700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(s.writerLockPath(runID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	acquired, err := tryLockRunLeaseFile(lockFile)
	if err != nil {
		_ = lockFile.Close()
		return err
	}
	if !acquired {
		_ = lockFile.Close()
		return engine.ErrRunLeaseHeld
	}
	defer func() {
		_ = unlockRunLeaseFile(lockFile)
		_ = lockFile.Close()
	}()

	s.mu.Lock()
	writer := s.writers[runID]
	delete(s.writers, runID)
	delete(s.plans, runID)
	delete(s.planDigests, runID)
	delete(s.planVersions, runID)
	s.mu.Unlock()
	if writer != nil {
		if err := writer.Close(); err != nil {
			return fmt.Errorf("runstore: close trace for deletion %s: %w", runID, err)
		}
	}
	dir := s.RunDir(runID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("runstore: delete run %s: %w", runID, err)
	}
	return nil
}

func (s *DirRunStore) Close() error {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	s.mu.Lock()
	leases := make([]*fileRunLease, 0, len(s.leases))
	for lease := range s.leases {
		leases = append(leases, lease)
		delete(s.leases, lease)
	}
	writers := make([]*internaltrace.JSONLWriter, 0, len(s.writers))
	for runID, writer := range s.writers {
		writers = append(writers, writer)
		delete(s.writers, runID)
	}
	s.mu.Unlock()
	var firstErr error
	for _, lease := range leases {
		if err := lease.releaseLocked(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, writer := range writers {
		if err := writer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *DirRunStore) writerFor(runID string) (*internaltrace.JSONLWriter, error) {
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if writer, ok := s.writers[runID]; ok {
		return writer, nil
	}
	path := s.TracePath(runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	writer, err := internaltrace.NewJSONLWriter(path)
	if err != nil {
		return nil, err
	}
	s.writers[runID] = writer
	return writer, nil
}

func parseSnapshotIndex(name string) (int, error) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, "step-"), ".json")
	return strconv.Atoi(trimmed)
}

func parseCheckpointSequence(name string) (int64, error) {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, "checkpoint-"), ".json")
	return strconv.ParseInt(trimmed, 10, 64)
}

func saveSequencedCheckpoint(dir, finalPath, runID string, sequence int64, data []byte) error {
	highestSequence, err := highestCheckpointSequence(dir)
	if err != nil {
		return err
	}
	if sequence <= highestSequence {
		if sequence == highestSequence {
			existing, readErr := readFileBounded(finalPath, maxRunStateBytes)
			if readErr == nil && bytes.Equal(existing, data) {
				return nil
			}
		}
		return checkpointConflict(runID, sequence)
	}
	if sequence != highestSequence+1 {
		return checkpointConflict(runID, sequence)
	}

	if err := writeFileExclusive(finalPath, data); err == nil {
		return nil
	} else {
		publishErr := err
		if !errors.Is(publishErr, fs.ErrExist) {
			return publishErr
		}
		existing, readErr := readFileBounded(finalPath, maxRunStateBytes)
		if readErr != nil {
			return publishErr
		}
		currentHighest, scanErr := highestCheckpointSequence(dir)
		if scanErr != nil {
			return scanErr
		}
		if currentHighest == sequence && bytes.Equal(existing, data) {
			return syncDirectory(dir)
		}
		return checkpointConflict(runID, sequence)
	}
}

func highestCheckpointSequence(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var highest int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "checkpoint-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		sequence, parseErr := parseCheckpointSequence(entry.Name())
		if parseErr == nil && sequence > highest {
			highest = sequence
		}
	}
	return highest, nil
}

func checkpointConflict(runID string, sequence int64) error {
	return fmt.Errorf("%w: sequence %d for run %q", ErrCheckpointConflict, sequence, runID)
}

func validateCursorStepIndexPresence(data []byte, cursorSet *engine.ExecutionCursorSet) error {
	if cursorSet == nil {
		return nil
	}
	var document struct {
		CursorSet *struct {
			Cursors []map[string]json.RawMessage `json:"cursors"`
		} `json:"CursorSet"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	if document.CursorSet == nil || len(document.CursorSet.Cursors) != len(cursorSet.Cursors) {
		return errors.New("runstore: malformed cursor set")
	}
	for index, cursor := range document.CursorSet.Cursors {
		stepIndex, ok := cursor["step_index"]
		if !ok || bytes.Equal(bytes.TrimSpace(stepIndex), []byte("null")) {
			return fmt.Errorf("runstore: cursor %d is missing step_index", index)
		}
	}
	return nil
}

func writeFileAtomic(finalPath string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(finalPath), "."+filepath.Base(finalPath)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(finalPath))
}

func writeFileExclusive(finalPath string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(finalPath), "."+filepath.Base(finalPath)+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, finalPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(finalPath))
}

func validateRunID(runID string) error {
	if runID == "" || len(runID) > 128 {
		return errors.New("runstore: run id must contain between 1 and 128 characters")
	}
	if runID != strings.ToLower(runID) || strings.HasSuffix(runID, ".") {
		return fmt.Errorf("runstore: invalid non-canonical run id %q", runID)
	}
	for index, character := range runID {
		valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '.' || character == '_' || character == '-'
		if !valid || index == 0 && !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9') {
			return fmt.Errorf("runstore: invalid run id %q", runID)
		}
	}
	base := strings.SplitN(runID, ".", 2)[0]
	if base == "con" || base == "prn" || base == "aux" || base == "nul" ||
		len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9' {
		return fmt.Errorf("runstore: invalid reserved run id %q", runID)
	}
	return nil
}

func readFileBounded(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("runstore: %s exceeds %d bytes", filepath.Base(path), maximum)
	}
	return data, nil
}

func decodeJSONDocument(data []byte, target any) error {
	if err := plansnapshot.ValidateUniqueJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
