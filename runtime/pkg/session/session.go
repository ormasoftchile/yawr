package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

const (
	EventSchemaV1                         = "yawr.investigation-session-event/v1"
	ManifestSchemaV1                      = "yawr.investigation-session-manifest/v1"
	ManifestProjectionReceipts            = 256
	ExecutionMutationSchemaV1             = "yawr.execution-mutation/v1"
	RunProjectionSchemaV1                 = "yawr.session-run-projection/v1"
	ExecutionFrameProjectionSchemaV1      = "yawr.session-execution-frame-projection/v1"
	ExecutionFrameProjectionChunkSchemaV1 = "yawr.session-execution-frame-projection-chunk/v1"
	CreationIdentitySchemaV1              = "yawr.session-creation-identity/v1"
)

var (
	ErrCreationConflict  = errors.New("session: creation identity conflicts with existing session")
	ErrCommandConflict   = errors.New("session: command id conflicts with an accepted command")
	ErrSequenceConflict  = errors.New("session: expected sequence does not match journal")
	ErrSessionLeaseHeld  = errors.New("session: writer lease is already held")
	ErrSessionLeaseStale = errors.New("session: writer lease is stale")
	ErrProjection        = errors.New("session: manifest projection failed")
	ErrSessionClosed     = errors.New("session: investigation is already closed")
	ErrAttemptActive     = errors.New("session: run attempt is still active")
	ErrMutationConflict  = errors.New("session: execution mutation conflicts with journal history")
)

type Status string

const (
	StatusActive        Status = "active"
	StatusPaused        Status = "paused"
	StatusResolved      Status = "resolved"
	StatusEscalated     Status = "escalated"
	StatusFailed        Status = "failed"
	StatusCancelled     Status = "cancelled"
	StatusAbandoned     Status = "abandoned"
	StatusIndeterminate Status = "indeterminate"
)

type SegmentStatus string

const (
	SegmentStatusPrepared      SegmentStatus = "prepared"
	SegmentStatusActive        SegmentStatus = "active"
	SegmentStatusPaused        SegmentStatus = "paused"
	SegmentStatusHandedOff     SegmentStatus = "handed_off"
	SegmentStatusCompleted     SegmentStatus = "completed"
	SegmentStatusFailed        SegmentStatus = "failed"
	SegmentStatusCancelled     SegmentStatus = "cancelled"
	SegmentStatusIndeterminate SegmentStatus = "indeterminate"
)

type AttemptStatus string

const (
	AttemptStatusStarting         AttemptStatus = "starting"
	AttemptStatusRunning          AttemptStatus = "running"
	AttemptStatusWaiting          AttemptStatus = "waiting"
	AttemptStatusPausedAtBoundary AttemptStatus = "paused_at_boundary"
	AttemptStatusHandoffPending   AttemptStatus = "handoff_pending"
	AttemptStatusCompleted        AttemptStatus = "completed"
	AttemptStatusFailed           AttemptStatus = "failed"
	AttemptStatusCancelled        AttemptStatus = "cancelled"
	AttemptStatusIndeterminate    AttemptStatus = "indeterminate"
)

type TransitionStatus string

const (
	TransitionStatusPrepared  TransitionStatus = "prepared"
	TransitionStatusCommitted TransitionStatus = "committed"
	TransitionStatusAborted   TransitionStatus = "aborted"
)

type EventKind string

const (
	EventSessionCreated      EventKind = "session.created"
	EventSessionClosed       EventKind = "session.closed"
	EventSessionPaused       EventKind = "session.paused"
	EventSessionResumed      EventKind = "session.resumed"
	EventSessionDetached     EventKind = "session.detached"
	EventAttemptUpdated      EventKind = "attempt.updated"
	EventExecutionCommitted  EventKind = "execution.committed"
	EventTraceCommitted      EventKind = "trace.committed"
	EventSegmentRevised      EventKind = "segment.revised"
	EventTransitionPrepared  EventKind = "transition.prepared"
	EventTransitionCommitted EventKind = "transition.committed"
	EventTransitionAborted   EventKind = "transition.aborted"
)

type SessionRecord struct {
	SessionID             string `json:"session_id"`
	Status                Status `json:"status"`
	RootSegmentID         string `json:"root_segment_id"`
	ActiveSegmentID       string `json:"active_segment_id,omitempty"`
	ActiveRunID           string `json:"active_run_id,omitempty"`
	Sequence              int64  `json:"sequence"`
	ManifestRevision      int64  `json:"manifest_revision"`
	WriterEpoch           uint64 `json:"writer_epoch"`
	CreationCommandID     string `json:"creation_command_id"`
	CreationDigest        string `json:"creation_digest"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
	DerivedFromSessionID  string `json:"derived_from_session_id,omitempty"`
	DerivedFromScenarioID string `json:"derived_from_scenario_id,omitempty"`
}

type EntrySelector struct {
	Step string `json:"step"`
}

type SegmentRecord struct {
	SegmentID              string        `json:"segment_id"`
	Ordinal                int           `json:"ordinal"`
	RunbookID              string        `json:"runbook_id"`
	RunbookName            string        `json:"runbook_name"`
	EntrySelector          EntrySelector `json:"entry_selector"`
	Status                 SegmentStatus `json:"status"`
	PlanHash               string        `json:"plan_hash"`
	GraphHash              string        `json:"graph_hash"`
	ExecutableSnapshotHash string        `json:"executable_snapshot_hash"`
	CatalogDigest          string        `json:"catalog_digest,omitempty"`
	PackageLockDigest      string        `json:"package_lock_digest,omitempty"`
	ProfileDigest          string        `json:"profile_digest,omitempty"`
	AttemptRunIDs          []string      `json:"attempt_run_ids"`
	ExecutableRevision     int64         `json:"executable_revision"`
	GraphRevision          int64         `json:"graph_revision"`
}

type RunAttemptRecord struct {
	RunID                  string        `json:"run_id"`
	SegmentID              string        `json:"segment_id"`
	Ordinal                int           `json:"ordinal"`
	Mode                   string        `json:"mode"`
	Status                 AttemptStatus `json:"status"`
	Actor                  string        `json:"actor,omitempty"`
	Client                 string        `json:"client,omitempty"`
	PlanHash               string        `json:"plan_hash,omitempty"`
	StartedAt              string        `json:"started_at"`
	UpdatedAt              string        `json:"updated_at"`
	CompletedAt            string        `json:"completed_at,omitempty"`
	ContinuedFromRunID     string        `json:"continued_from_run_id,omitempty"`
	RunProjectionHash      string        `json:"run_projection_hash,omitempty"`
	ExecutionMutationHash  string        `json:"execution_mutation_hash,omitempty"`
	CheckpointSequence     int64         `json:"checkpoint_sequence,omitempty"`
	CommittedTraceSequence int64         `json:"committed_trace_sequence,omitempty"`
	JournaledTraceSequence int64         `json:"journaled_trace_sequence,omitempty"`
}

type CreationIdentity struct {
	SchemaVersion     string `json:"schema_version"`
	SessionID         string `json:"session_id"`
	SessionDigest     string `json:"session_digest"`
	SegmentDigest     string `json:"segment_digest"`
	AttemptDigest     string `json:"attempt_digest"`
	VarsDigest        string `json:"vars_digest"`
	RuntimeVarsDigest string `json:"runtime_vars_digest"`
	ScenarioDirDigest string `json:"scenario_dir_digest"`
}

type ExecutionMutation struct {
	SchemaVersion          string           `json:"schema_version"`
	RunID                  string           `json:"run_id"`
	CheckpointSequence     int64            `json:"checkpoint_sequence"`
	RunStatus              engine.RunStatus `json:"run_status"`
	AttemptStatus          AttemptStatus    `json:"attempt_status"`
	RunWriterEpoch         uint64           `json:"run_writer_epoch"`
	PreviousMutationHash   string           `json:"previous_mutation_hash,omitempty"`
	StateProjectionHash    string           `json:"state_projection_hash"`
	FrameProjectionHash    string           `json:"frame_projection_hash,omitempty"`
	CommittedTraceSequence int64            `json:"committed_trace_sequence"`
	TraceSequenceStart     int64            `json:"trace_sequence_start,omitempty"`
	TraceSequenceEnd       int64            `json:"trace_sequence_end,omitempty"`
}

type ExecutionFrameProjection struct {
	SchemaVersion          string   `json:"schema_version"`
	RunID                  string   `json:"run_id"`
	CheckpointSequence     int64    `json:"checkpoint_sequence"`
	CommittedTraceSequence int64    `json:"committed_trace_sequence"`
	ContentDigest          string   `json:"content_digest"`
	ContentSize            int64    `json:"content_size"`
	ChunkDigests           []string `json:"chunk_digests"`
}

type ExecutionFrameProjectionContent struct {
	Interactions       map[string]*engine.InteractionState `json:"interactions,omitempty"`
	PendingTraceEvents []engine.Event                      `json:"pending_trace_events,omitempty"`
}

type ExecutionFrameProjectionChunk struct {
	SchemaVersion string `json:"schema_version"`
	Data          []byte `json:"data"`
}

type SourceOccurrence struct {
	RunID              string                  `json:"run_id"`
	QualifiedNodeID    string                  `json:"qualified_node_id"`
	CallPath           []engine.DebugCallFrame `json:"call_path,omitempty"`
	StepID             string                  `json:"step"`
	FrameID            string                  `json:"frame_id,omitempty"`
	FrameStepIndex     int                     `json:"frame_step_index,omitempty"`
	FrameStack         []string                `json:"frame_stack,omitempty"`
	BranchLabel        string                  `json:"branch_label,omitempty"`
	IterationIndex     int                     `json:"iteration_index,omitempty"`
	Phase              string                  `json:"phase"`
	Invocation         int                     `json:"invocation"`
	RetryAttempt       int                     `json:"retry_attempt"`
	OccurrenceSequence int64                   `json:"occurrence_sequence"`
}

type ExecutionSource string

const (
	ExecutionSourceLive  ExecutionSource = "live"
	ExecutionSourceSaved ExecutionSource = "saved"
)

type OccurrenceRecord struct {
	SessionID          string                  `json:"session_id"`
	SegmentID          string                  `json:"segment_id"`
	RunID              string                  `json:"run_id"`
	QualifiedNodeID    string                  `json:"qualified_node_id"`
	CallPath           []engine.DebugCallFrame `json:"call_path,omitempty"`
	StepID             string                  `json:"step_id"`
	Phase              string                  `json:"phase"`
	Invocation         int                     `json:"invocation"`
	RetryAttempt       int                     `json:"retry_attempt"`
	OccurrenceSequence int64                   `json:"occurrence_sequence"`
	ExecutionSource    ExecutionSource         `json:"execution_source"`
	Status             string                  `json:"status"`
	EventSequence      int64                   `json:"event_sequence"`
}

func OccurrenceFromTraceEvent(
	sessionID string,
	segmentID string,
	source ExecutionSource,
	event engine.Event,
) (string, OccurrenceRecord, bool, error) {
	status := ""
	switch event.Kind {
	case "step/completed":
		status = "completed"
	case "step/failed":
		status = "failed"
	case "step/skipped":
		status = "skipped"
	case "step/indeterminate":
		status = "indeterminate"
	default:
		return "", OccurrenceRecord{}, false, nil
	}
	stepID, _ := event.Payload["step_id"].(string)
	qualifiedNodeID, _ := event.Payload["qualified_node_id"].(string)
	phase, _ := event.Payload["phase"].(string)
	if phase == "" {
		phase = string(engine.ExecutionPhaseExecute)
	}
	callPath, err := occurrenceCallPath(event.Payload["call_path"])
	if err != nil {
		return "", OccurrenceRecord{}, false, err
	}
	if qualifiedNodeID == "" && stepID != "" {
		qualifiedNodeID = engine.DebugNodeID(callPath, stepID)
	}
	invocation := occurrencePositiveInt(event.Payload["invocation"], 1)
	retryAttempt := occurrencePositiveInt(event.Payload["retry_attempt"], 1)
	occurrenceSequence := occurrencePositiveInt64(event.Payload["occurrence_sequence"], 1)
	if sessionID == "" || segmentID == "" || event.RunID == "" || stepID == "" ||
		qualifiedNodeID == "" || event.Sequence < 1 || invocation < 1 || retryAttempt < 1 ||
		occurrenceSequence < 1 || source != ExecutionSourceLive && source != ExecutionSourceSaved {
		return "", OccurrenceRecord{}, false, errors.New("session: terminal trace event has invalid occurrence metadata")
	}
	record := OccurrenceRecord{
		SessionID: sessionID, SegmentID: segmentID, RunID: event.RunID,
		QualifiedNodeID: qualifiedNodeID, CallPath: callPath, StepID: stepID, Phase: phase,
		Invocation: invocation, RetryAttempt: retryAttempt, OccurrenceSequence: occurrenceSequence,
		ExecutionSource: source, Status: status, EventSequence: event.Sequence,
	}
	return OccurrenceID(record), record, true, nil
}

func OccurrenceID(record OccurrenceRecord) string {
	return DigestJSON(struct {
		SessionID          string                  `json:"session_id"`
		SegmentID          string                  `json:"segment_id"`
		RunID              string                  `json:"run_id"`
		QualifiedNodeID    string                  `json:"qualified_node_id"`
		CallPath           []engine.DebugCallFrame `json:"call_path,omitempty"`
		Phase              string                  `json:"phase"`
		Invocation         int                     `json:"invocation"`
		RetryAttempt       int                     `json:"retry_attempt"`
		OccurrenceSequence int64                   `json:"occurrence_sequence"`
	}{
		record.SessionID, record.SegmentID, record.RunID, record.QualifiedNodeID,
		record.CallPath, record.Phase, record.Invocation, record.RetryAttempt, record.OccurrenceSequence,
	})
}

func occurrenceCallPath(value any) ([]engine.DebugCallFrame, error) {
	if value == nil {
		return nil, nil
	}
	if typed, ok := value.([]engine.DebugCallFrame); ok {
		return append([]engine.DebugCallFrame(nil), typed...), nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("session: occurrence call path is invalid")
	}
	var callPath []engine.DebugCallFrame
	if err := json.Unmarshal(encoded, &callPath); err != nil {
		return nil, errors.New("session: occurrence call path is invalid")
	}
	return callPath, nil
}

func occurrencePositiveInt(value any, fallback int) int {
	converted := occurrencePositiveInt64(value, int64(fallback))
	if converted > int64(^uint(0)>>1) {
		return fallback
	}
	return int(converted)
}

func occurrencePositiveInt64(value any, fallback int64) int64 {
	switch typed := value.(type) {
	case int:
		if typed > 0 {
			return int64(typed)
		}
	case int64:
		if typed > 0 {
			return typed
		}
	case float64:
		if typed > 0 && typed == float64(int64(typed)) {
			return int64(typed)
		}
	case json.Number:
		if converted, err := typed.Int64(); err == nil && converted > 0 {
			return converted
		}
	}
	return fallback
}

type TransitionRecord struct {
	TransitionID                 string            `json:"transition_id"`
	Status                       TransitionStatus  `json:"status"`
	SourceSegmentID              string            `json:"source_segment_id"`
	SourceOccurrence             SourceOccurrence  `json:"source_occurrence"`
	TargetSegmentID              string            `json:"target_segment_id"`
	TargetRunID                  string            `json:"target_run_id"`
	TargetRunbookID              string            `json:"target_runbook_id"`
	TargetRunbookName            string            `json:"target_runbook_name"`
	TargetPlanHash               string            `json:"target_plan_hash"`
	TargetGraphHash              string            `json:"target_graph_hash"`
	TargetExecutableSnapshotHash string            `json:"target_executable_snapshot_hash"`
	ReasonCode                   string            `json:"reason_code"`
	ReasonSummary                string            `json:"reason_summary"`
	ContextBindings              map[string]string `json:"context_bindings,omitempty"`
	FactRefs                     []FactRef         `json:"fact_refs,omitempty"`
	ContextDigest                string            `json:"context_digest"`
	IdempotencyKey               string            `json:"idempotency_key"`
	PreparedAt                   string            `json:"prepared_at"`
	CommittedAt                  string            `json:"committed_at,omitempty"`
	AbortedAt                    string            `json:"aborted_at,omitempty"`
}

type FactRef struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type HandoffContext struct {
	Inputs map[string]any `json:"inputs,omitempty"`
	Facts  map[string]any `json:"facts,omitempty"`
}

type CommandReceipt struct {
	CommandID           string    `json:"command_id"`
	CommandDigest       string    `json:"command_digest"`
	ClientCommandDigest string    `json:"client_command_digest"`
	EventKind           EventKind `json:"event_kind"`
	Sequence            int64     `json:"sequence"`
}

type Manifest struct {
	SchemaVersion    string                      `json:"schema_version"`
	Session          SessionRecord               `json:"session"`
	Segments         map[string]SegmentRecord    `json:"segments"`
	Attempts         map[string]RunAttemptRecord `json:"attempts"`
	Transitions      map[string]TransitionRecord `json:"transitions,omitempty"`
	Occurrences      map[string]OccurrenceRecord `json:"occurrences,omitempty"`
	AcceptedCommands map[string]CommandReceipt   `json:"accepted_commands"`
}

func ProjectManifest(manifest Manifest) Manifest {
	projected := manifest
	projected.AcceptedCommands = make(map[string]CommandReceipt, ManifestProjectionReceipts)
	minimumSequence := manifest.Session.Sequence - ManifestProjectionReceipts
	for commandID, receipt := range manifest.AcceptedCommands {
		if receipt.Sequence > minimumSequence {
			projected.AcceptedCommands[commandID] = receipt
		}
	}
	return projected
}

type Event struct {
	SchemaVersion       string          `json:"schema_version"`
	EventID             string          `json:"event_id"`
	SessionID           string          `json:"session_id"`
	Sequence            int64           `json:"sequence"`
	WriterEpoch         uint64          `json:"writer_epoch"`
	CommandID           string          `json:"command_id"`
	CommandDigest       string          `json:"command_digest"`
	ClientCommandDigest string          `json:"client_command_digest,omitempty"`
	Kind                EventKind       `json:"kind"`
	Timestamp           string          `json:"timestamp"`
	PreviousDigest      string          `json:"previous_digest,omitempty"`
	Digest              string          `json:"digest"`
	Payload             json.RawMessage `json:"payload"`
}

type JSONBlob struct {
	Digest string          `json:"digest"`
	Data   json.RawMessage `json:"-"`
}

func NewJSONBlob(data json.RawMessage) JSONBlob {
	copy := append(json.RawMessage(nil), data...)
	return JSONBlob{Digest: DigestBytes(copy), Data: copy}
}

func DigestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest)
}

func DigestJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return DigestBytes(encoded)
}

func NewCreationIdentity(
	sessionID string,
	sessionRecord SessionRecord,
	segment SegmentRecord,
	attempt RunAttemptRecord,
	vars map[string]string,
	runtimeVars map[string]any,
	scenarioDir string,
) (CreationIdentity, error) {
	identity := CreationIdentity{
		SchemaVersion:     CreationIdentitySchemaV1,
		SessionID:         sessionID,
		SessionDigest:     DigestJSON(canonicalCreationSession(sessionRecord)),
		SegmentDigest:     DigestJSON(canonicalCreationSegment(segment)),
		AttemptDigest:     DigestJSON(canonicalCreationAttempt(attempt, segment.PlanHash)),
		VarsDigest:        DigestJSON(vars),
		RuntimeVarsDigest: DigestJSON(runtimeVars),
		ScenarioDirDigest: DigestJSON(scenarioDir),
	}
	for _, digest := range []string{
		identity.SessionDigest, identity.SegmentDigest, identity.AttemptDigest,
		identity.VarsDigest, identity.RuntimeVarsDigest, identity.ScenarioDirDigest,
	} {
		if digest == "" {
			return CreationIdentity{}, errors.New("session: creation inputs are not JSON serializable")
		}
	}
	return identity, nil
}

func ValidateCreationIdentity(
	identity CreationIdentity,
	sessionID string,
	sessionRecord SessionRecord,
	segment SegmentRecord,
	attempt RunAttemptRecord,
) error {
	if identity.SchemaVersion != CreationIdentitySchemaV1 || identity.SessionID != sessionID ||
		identity.SessionDigest != DigestJSON(canonicalCreationSession(sessionRecord)) ||
		identity.SegmentDigest != DigestJSON(canonicalCreationSegment(segment)) ||
		identity.AttemptDigest != DigestJSON(canonicalCreationAttempt(attempt, segment.PlanHash)) {
		return errors.New("session: creation identity does not match topology")
	}
	for _, digest := range []string{
		identity.SessionDigest, identity.SegmentDigest, identity.AttemptDigest,
		identity.VarsDigest, identity.RuntimeVarsDigest, identity.ScenarioDirDigest,
	} {
		if !isSHA256Digest(digest) {
			return errors.New("session: creation identity has an invalid digest")
		}
	}
	return nil
}

func isSHA256Digest(value string) bool {
	if len(value) != 71 || value[:7] != "sha256:" {
		return false
	}
	decoded, err := hex.DecodeString(value[7:])
	return err == nil && len(decoded) == sha256.Size
}

func CreationDigest(identity CreationIdentity) string {
	return DigestJSON(identity)
}

func canonicalCreationSession(record SessionRecord) SessionRecord {
	return SessionRecord{
		Status: record.Status, DerivedFromSessionID: record.DerivedFromSessionID,
		DerivedFromScenarioID: record.DerivedFromScenarioID,
	}
}

func canonicalCreationSegment(record SegmentRecord) SegmentRecord {
	if record.EntrySelector.Step == "" {
		record.EntrySelector.Step = "$entry"
	}
	if record.ExecutableRevision == 0 {
		record.ExecutableRevision = 1
	}
	if record.GraphRevision == 0 {
		record.GraphRevision = 1
	}
	record.AttemptRunIDs = append([]string(nil), record.AttemptRunIDs...)
	return record
}

func canonicalCreationAttempt(record RunAttemptRecord, planHash string) RunAttemptRecord {
	record.StartedAt = ""
	record.UpdatedAt = ""
	record.CompletedAt = ""
	record.RunProjectionHash = ""
	record.ExecutionMutationHash = ""
	record.CheckpointSequence = 0
	record.CommittedTraceSequence = 0
	record.JournaledTraceSequence = 0
	if record.PlanHash == "" {
		record.PlanHash = planHash
	}
	return record
}

func AttemptUpdateDigest(runID string, status AttemptStatus) string {
	return DigestJSON(struct {
		RunID  string        `json:"run_id"`
		Status AttemptStatus `json:"status"`
	}{RunID: runID, Status: status})
}

func ExecutionMutationDigest(runID string, mutationHash string) string {
	return DigestJSON(struct {
		RunID        string `json:"run_id"`
		MutationHash string `json:"mutation_hash"`
	}{RunID: runID, MutationHash: mutationHash})
}

func TraceCommitDigest(runID string, traceHash string) string {
	return DigestJSON(struct {
		RunID     string `json:"run_id"`
		TraceHash string `json:"trace_hash"`
	}{RunID: runID, TraceHash: traceHash})
}

func SegmentRevisionDigest(
	segmentID string,
	runID string,
	executableRevision int64,
	graphRevision int64,
	planHash string,
	executableSnapshotHash string,
	graphHash string,
	resolution engine.DynamicIncludeResolutionState,
	dispatch engine.DispatchState,
) string {
	return DigestJSON(struct {
		SegmentID              string                               `json:"segment_id"`
		RunID                  string                               `json:"run_id"`
		ExecutableRevision     int64                                `json:"executable_revision"`
		GraphRevision          int64                                `json:"graph_revision"`
		PlanHash               string                               `json:"plan_hash"`
		ExecutableSnapshotHash string                               `json:"executable_snapshot_hash"`
		GraphHash              string                               `json:"graph_hash"`
		Resolution             engine.DynamicIncludeResolutionState `json:"resolution"`
		Dispatch               engine.DispatchState                 `json:"dispatch"`
	}{segmentID, runID, executableRevision, graphRevision, planHash, executableSnapshotHash, graphHash, resolution, dispatch})
}

func TransitionPrepareDigest(transition TransitionRecord, segment SegmentRecord, attempt RunAttemptRecord) string {
	transition.PreparedAt = ""
	transition.CommittedAt = ""
	transition.AbortedAt = ""
	attempt.StartedAt = ""
	attempt.UpdatedAt = ""
	attempt.CompletedAt = ""
	return DigestJSON(struct {
		Transition TransitionRecord `json:"transition"`
		Segment    SegmentRecord    `json:"segment"`
		Attempt    RunAttemptRecord `json:"attempt"`
	}{transition, segment, attempt})
}

func TransitionCommitDigest(transitionID string) string {
	return DigestJSON(struct {
		TransitionID string `json:"transition_id"`
	}{transitionID})
}

func TransitionAbortDigest(transitionID string, reason string) string {
	return DigestJSON(struct {
		TransitionID string `json:"transition_id"`
		Reason       string `json:"reason"`
	}{transitionID, reason})
}

func NewExecutionMutationBlob(mutation ExecutionMutation) (JSONBlob, error) {
	encoded, err := json.Marshal(mutation)
	if err != nil {
		return JSONBlob{}, err
	}
	return NewJSONBlob(encoded), nil
}

func CloseDigest(status Status) string {
	return DigestJSON(struct {
		Status Status `json:"status"`
	}{Status: status})
}

func PauseDigest(runID, reason string) string {
	return DigestJSON(struct {
		RunID  string `json:"run_id"`
		Reason string `json:"reason"`
	}{runID, reason})
}

func ResumeDigest(runID string) string {
	return DigestJSON(struct {
		RunID string `json:"run_id"`
	}{runID})
}

func DetachDigest(reason string) string {
	return DigestJSON(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
}

type CreateRequest struct {
	SessionID   string
	CommandID   string
	EntryDigest string
	Identity    CreationIdentity
	Session     SessionRecord
	Segment     SegmentRecord
	Attempt     RunAttemptRecord
	Blobs       []JSONBlob
}

type CloseRequest struct {
	SessionID           string
	CommandID           string
	WriterEpoch         uint64
	ExpectedSequence    int64
	ClientCommandDigest string
	Status              Status
}

type PauseRequest struct {
	SessionID           string
	CommandID           string
	WriterEpoch         uint64
	ExpectedSequence    int64
	ClientCommandDigest string
	Reason              string
}

type ResumeSessionRequest struct {
	SessionID           string
	CommandID           string
	WriterEpoch         uint64
	ExpectedSequence    int64
	ClientCommandDigest string
}

type DetachRequest struct {
	SessionID           string
	CommandID           string
	WriterEpoch         uint64
	ExpectedSequence    int64
	ClientCommandDigest string
	Reason              string
}

type AttemptUpdateRequest struct {
	SessionID        string
	CommandID        string
	WriterEpoch      uint64
	ExpectedSequence int64
	RunID            string
	Status           AttemptStatus
}

type ExecutionMutationRequest struct {
	SessionID           string
	CommandID           string
	WriterEpoch         uint64
	ExpectedSequence    int64
	ClientCommandDigest string
	Mutation            ExecutionMutation
	StateProjection     JSONBlob
	FrameProjection     JSONBlob
	ProjectionBlobs     []JSONBlob
	Occurrences         map[string]OccurrenceRecord
}

type TraceCommitRequest struct {
	SessionID        string
	CommandID        string
	WriterEpoch      uint64
	ExpectedSequence int64
	RunID            string
	Event            engine.Event
}

type SegmentRevisionRequest struct {
	SessionID          string
	CommandID          string
	WriterEpoch        uint64
	ExpectedSequence   int64
	SegmentID          string
	RunID              string
	ExecutableRevision int64
	GraphRevision      int64
	PlanHash           string
	ExecutableSnapshot JSONBlob
	Graph              JSONBlob
	Resolution         engine.DynamicIncludeResolutionState
	Dispatch           engine.DispatchState
}

type PrepareTransitionRequest struct {
	SessionID        string
	CommandID        string
	WriterEpoch      uint64
	ExpectedSequence int64
	Transition       TransitionRecord
	TargetSegment    SegmentRecord
	TargetAttempt    RunAttemptRecord
	Blobs            []JSONBlob
}

type CommitTransitionRequest struct {
	SessionID        string
	CommandID        string
	WriterEpoch      uint64
	ExpectedSequence int64
	TransitionID     string
}

type AbortTransitionRequest struct {
	SessionID        string
	CommandID        string
	WriterEpoch      uint64
	ExpectedSequence int64
	TransitionID     string
	Reason           string
}

type Lease interface {
	Epoch() uint64
	Release() error
}

type Store interface {
	AcquireSessionLease(ctx context.Context, sessionID string) (Lease, error)
	CreateSession(ctx context.Context, writerEpoch uint64, request CreateRequest) (Manifest, error)
	LoadManifest(ctx context.Context, sessionID string) (Manifest, error)
	ReadEvents(ctx context.Context, sessionID string, afterSequence int64) ([]Event, error)
	ReadBlob(ctx context.Context, sessionID string, digest string) (json.RawMessage, error)
	PauseSession(ctx context.Context, request PauseRequest) (Manifest, error)
	ResumeSession(ctx context.Context, request ResumeSessionRequest) (Manifest, error)
	RecordDetach(ctx context.Context, request DetachRequest) (Manifest, error)
	UpdateAttempt(ctx context.Context, request AttemptUpdateRequest) (Manifest, error)
	CommitExecutionMutation(ctx context.Context, request ExecutionMutationRequest) (Manifest, error)
	CommitTraceEvent(ctx context.Context, request TraceCommitRequest) (Manifest, error)
	CommitSegmentRevision(ctx context.Context, request SegmentRevisionRequest) (Manifest, error)
	PrepareTransition(ctx context.Context, request PrepareTransitionRequest) (Manifest, error)
	CommitTransition(ctx context.Context, request CommitTransitionRequest) (Manifest, error)
	AbortTransition(ctx context.Context, request AbortTransitionRequest) (Manifest, error)
	CloseSession(ctx context.Context, request CloseRequest) (Manifest, error)
	Close() error
}

func ParseTime(value string) error {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err
}
