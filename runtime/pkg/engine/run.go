package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	regschema "github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// ErrIndeterminate is returned by Next when the run is halted because a step's
// completion could not be confirmed. The caller must not treat this as a normal
// failure; the step may have already executed.
var ErrIndeterminate = errors.New("engine: step completion indeterminate; run halted pending verification")

// ErrIndeterminateAcknowledgmentRequired is returned by Resume when the persisted
// run status is RunStatusIndeterminate and RunOptions.AcknowledgeIndeterminate was
// not set to true. The operator must pass --acknowledge-indeterminate to confirm
// they have verified the external state before resuming.
var ErrIndeterminateAcknowledgmentRequired = errors.New("engine: run halted in indeterminate state; pass --acknowledge-indeterminate to resume")

// ErrDebugResumeUnsupported prevents a debugger from resuming without the
// ephemeral child protection state accumulated by the original process.
var ErrDebugResumeUnsupported = errors.New("engine: debugger-enabled resume is unsupported; start a new debug run")

var ErrRouteTestResumeUnsupported = errors.New("engine: route-test resume is unsupported; start the route test again")

var ErrHandoffPending = errors.New("engine: run has a pending handoff; resume through the session coordinator")

// ErrCheckpointCommit means a completed execution result could not be durably
// committed. The run must stop rather than execute another step.
var ErrCheckpointCommit = errors.New("engine: checkpoint commit failed")

// ErrTraceCommit means a required audit event could not be durably appended.
var ErrTraceCommit = errors.New("engine: trace commit failed")

// ErrRunLeaseHeld means another process currently owns the run's writer lease.
var ErrRunLeaseHeld = errors.New("engine: run writer lease is already held")

// ErrRunLeaseStale means a prior writer attempted to commit after takeover.
var ErrRunLeaseStale = errors.New("engine: run writer lease epoch is stale")

// RunLease grants exclusive mutation rights for one run until released.
// Implementations must also release ownership when the holding process exits.
type RunLease interface {
	Epoch() uint64
	Release() error
}

// RunHandle is the control surface for a single active run.
// The caller (adapter) drives execution by calling Next in a loop.
type RunHandle interface {
	// Next advances the run by one step. Blocks until the step completes
	// or is paused waiting for evidence/approval.
	// Returns io.EOF when the handle stops because the run completed or reached
	// a durable resumable boundary. Inspect State to distinguish those cases.
	Next(ctx context.Context) (*StepResult, error)

	// Approve records an approval decision for the current approval gate.
	Approve(ctx context.Context, decision ApprovalDecision) error

	// SubmitEvidence records user-supplied evidence for interactive steps.
	SubmitEvidence(ctx context.Context, stepID string, ev map[string]*EvidenceValue) error

	// Cancel requests a graceful stop of the run.
	Cancel(ctx context.Context, reason string) error

	// State returns a read-only snapshot of the current run state.
	State() RunState

	// Events returns a read-only channel of structured events emitted during execution.
	// The channel is closed when the handle enters a terminal or durable paused state.
	Events() <-chan Event
}

type DetachableRunHandle interface {
	RunHandle
	Detach(ctx context.Context, reason string) error
}

// StartupConfigurableRunHandle accepts private inputs before the first
// execution boundary. Implementations must reject configuration after Next.
type StartupConfigurableRunHandle interface {
	RunHandle
	ConfigureStartup(ctx context.Context, vars map[string]string) error
}

// RunOptions configures a run at start or resume time.
type RunOptions struct {
	Mode  RunMode
	Actor string
	Vars  map[string]string
	// RuntimeVars preserves typed variables when an engine executes nested steps.
	RuntimeVars map[string]any
	ScenarioDir string
	Store       RunStore
	OnEvent     func(Event)
	// PreStartEvents is the complete ordered prefix already emitted by the
	// caller's configured trace writer. Start links it into the durable store
	// under the new writer lease, without emitting it again to live sinks.
	// Resume does not accept pre-start events.
	PreStartEvents []Event
	// Debugger enables debugger hooks for this run only. Nil keeps normal runs
	// completely outside the debug path.
	Debugger DebugController
	// RouteTest enforces exact saved responses and stops successfully at the
	// selected step's before boundary. It is independent from live debugging.
	RouteTest RouteTestController
	// Client identifies the runtime surface that initiated this run.
	// Values: "cli", "server", "mobile-ios", "mobile-android"
	Client string
	// AcknowledgeIndeterminate must be true to resume a run that was halted
	// with RunStatusIndeterminate. When false (the default), Resume returns
	// ErrIndeterminateAcknowledgmentRequired so the run cannot be silently
	// continued after a step whose completion could not be established.
	// Corresponds to the --acknowledge-indeterminate CLI flag.
	AcknowledgeIndeterminate bool
}

// RunMode controls execution behaviour.
type RunMode string

const (
	RunModeReal      RunMode = "real"
	RunModeDryRun    RunMode = "dry-run"
	RunModeReplay    RunMode = "replay"
	RunModeRouteTest RunMode = "route-test"
)

const ExecutionCursorSchemaV1 = "yawr.execution-cursor/v1"

type ExecutionPhase string

const (
	ExecutionPhaseBefore  ExecutionPhase = "before"
	ExecutionPhaseExecute ExecutionPhase = "execute"
	ExecutionPhaseAfter   ExecutionPhase = "after"
)

// ExecutionCursor identifies the next execution boundary for one active flow.
type ExecutionCursor struct {
	QualifiedNodeID string           `json:"qualified_node_id,omitempty"`
	CallPath        []DebugCallFrame `json:"call_path,omitempty"`
	StepID          string           `json:"step_id,omitempty"`
	StepIndex       int              `json:"step_index"`
	FrameID         string           `json:"frame_id,omitempty"`
	BranchLabel     string           `json:"branch_label,omitempty"`
	IterationIndex  int              `json:"iteration_index,omitempty"`
	Phase           ExecutionPhase   `json:"phase"`
	Invocation      int              `json:"invocation"`
	RetryAttempt    int              `json:"retry_attempt"`
	AtEnd           bool             `json:"at_end,omitempty"`
}

// ExecutionCursorSet supports one serial cursor today and multiple parallel
// cursors once durable branch/join restoration is implemented.
type ExecutionCursorSet struct {
	SchemaVersion string            `json:"schema_version"`
	Cursors       []ExecutionCursor `json:"cursors"`
}

const DispatchStateSchemaV1 = "yawr.dispatch-state/v1"

type DispatchStatus string

const (
	DispatchStatusPrepared      DispatchStatus = "prepared"
	DispatchStatusSettled       DispatchStatus = "settled"
	DispatchStatusIndeterminate DispatchStatus = "indeterminate"
)

// DispatchState durably records one external execution occurrence without
// storing rendered request data or credentials.
type DispatchState struct {
	SchemaVersion                  string           `json:"schema_version"`
	OccurrenceID                   string           `json:"occurrence_id"`
	WriterEpoch                    uint64           `json:"writer_epoch"`
	QualifiedNodeID                string           `json:"qualified_node_id"`
	CallPath                       []DebugCallFrame `json:"call_path,omitempty"`
	StepID                         string           `json:"step_id"`
	FrameID                        string           `json:"frame_id,omitempty"`
	FrameStepIndex                 int              `json:"frame_step_index,omitempty"`
	Phase                          ExecutionPhase   `json:"phase"`
	Invocation                     int              `json:"invocation"`
	RetryAttempt                   int              `json:"retry_attempt"`
	OccurrenceSequence             int64            `json:"occurrence_sequence"`
	Classification                 string           `json:"classification"`
	EndpointIdentity               string           `json:"endpoint_identity,omitempty"`
	RequestDigest                  string           `json:"request_digest"`
	IdempotencyKey                 string           `json:"idempotency_key"`
	ProviderSupportsReconciliation bool             `json:"provider_supports_reconciliation"`
	Status                         DispatchStatus   `json:"status"`
	ResultDigest                   string           `json:"result_digest,omitempty"`
	PreparedAt                     string           `json:"prepared_at"`
	SettledAt                      string           `json:"settled_at,omitempty"`
}

const InteractionStateSchemaV1 = "yawr.interaction-state/v1"

type InteractionStatus string

const (
	InteractionStatusPending  InteractionStatus = "pending"
	InteractionStatusAnswered InteractionStatus = "answered"
)

// InteractionState is the durable execution state for one operator or host
// interaction. Request and Answer use transport-neutral JSON envelopes owned
// by the configured interaction provider.
type InteractionState struct {
	SchemaVersion       string            `json:"schema_version"`
	TurnID              string            `json:"turn_id"`
	CorrelationID       string            `json:"correlation_id,omitempty"`
	OwnerStepID         string            `json:"owner_step_id"`
	NodeID              string            `json:"node_id"`
	StepID              string            `json:"step_id"`
	FrameID             string            `json:"frame_id,omitempty"`
	FrameStepIndex      int               `json:"frame_step_index,omitempty"`
	Kind                string            `json:"kind"`
	Ordinal             int               `json:"ordinal"`
	ExecutionInvocation int               `json:"execution_invocation,omitempty"`
	Status              InteractionStatus `json:"status"`
	RequestDigest       string            `json:"request_digest"`
	Request             json.RawMessage   `json:"request"`
	AnswerDigest        string            `json:"answer_digest,omitempty"`
	Answer              json.RawMessage   `json:"answer,omitempty"`
	AnswerCommandID     string            `json:"answer_command_id,omitempty"`
	AnswerCommandDigest string            `json:"answer_command_digest,omitempty"`
	AcceptedAt          string            `json:"accepted_at,omitempty"`
	AuditToken          string            `json:"audit_token,omitempty"`
}

// RunState is an immutable snapshot of the mutable run state.
// Used for external inspection and checkpoint/resume.
type RunState struct {
	Results      *RunResults        `json:"results,omitempty"`
	BindingScope *BindingScopeState `json:"binding_scope,omitempty"`
	RunID        string
	RunbookPath  string
	Mode         RunMode `json:"mode,omitempty"`
	// WriterEpoch fences this state mutation to the active run lease.
	WriterEpoch uint64 `json:"writer_epoch,omitempty"`
	// CheckpointSequence orders durable commits independently of plan position.
	CheckpointSequence int64 `json:"checkpoint_sequence,omitempty"`
	// CommittedTraceSequence reserves event sequence numbers in the same
	// checkpoint that commits execution state, preventing reuse after restart.
	CommittedTraceSequence int64 `json:"committed_trace_sequence,omitempty"`
	// PendingTraceEvents are exact committed events awaiting trace projection.
	// They remain recoverable from the checkpoint if projection is interrupted.
	PendingTraceEvents []Event `json:"pending_trace_events,omitempty"`
	// PlanSnapshotDigest binds this checkpoint to the exact durable executable plan.
	PlanSnapshotDigest string              `json:"plan_snapshot_digest,omitempty"`
	CursorSet          *ExecutionCursorSet `json:"cursor_set,omitempty"`
	Status             RunStatus
	CurrentStep        string
	// CurrentStepIndex is the index of CurrentStep in the execution plan.
	CurrentStepIndex            int
	Vars                        map[string]any
	StepResults                 map[string]*StepResult
	Interactions                map[string]*InteractionState
	ExecutionInvocationCounts   map[string]int
	InteractionInvocationCounts map[string]int
	Dispatches                  map[string]*DispatchState
	ExecutionFrames             map[string]*ExecutionFrameState
	DynamicIncludes             map[string]*DynamicIncludeResolutionState
	PendingHandoff              *HandoffRequest
	StartedAt                   time.Time
	UpdatedAt                   time.Time
	// CompletedAt is when the run reached a terminal state.
	// Zero value until Status is completed/failed/cancelled.
	CompletedAt time.Time
	// Plan is the execution plan being executed.
	Plan *ExecutionPlan
}

// RunStatus is the current execution status of a run.
type RunStatus string

const (
	RunStatusPending RunStatus = "pending"
	RunStatusRunning RunStatus = "running"
	RunStatusWaiting RunStatus = "waiting"
	// RunStatusPausedAtBoundary is a durable nonterminal stop immediately
	// before one exact execution occurrence. Resume continues at that boundary.
	RunStatusPausedAtBoundary RunStatus = "paused_at_boundary"
	RunStatusHandoffPending   RunStatus = "handoff_pending"
	RunStatusCompleted        RunStatus = "completed"
	RunStatusFailed           RunStatus = "failed"
	RunStatusCancelled        RunStatus = "cancelled"
	// RunStatusIndeterminate means the run was halted because a step's completion
	// could not be established. The run is neither succeeded nor failed — an
	// operator must verify external state before resuming.
	RunStatusIndeterminate RunStatus = "indeterminate"
)

// Run is the mutable internal state of a runbook execution.
// Engine implementations use this to track execution progress.
type Run struct {
	Results      *RunResults
	BindingScope *BindingScopeState
	// ID uniquely identifies this run (UUID v4).
	ID string

	// Status is the current state machine state.
	Status RunStatus

	// Plan is the immutable execution plan being executed.
	Plan *ExecutionPlan

	// Vars holds runtime variables accumulated during execution.
	// Keys are variable names; values are typed (string, int, bool, etc.).
	Vars map[string]any

	// StepResults maps step ID to its execution result.
	// Populated as steps complete (success, skip, or fail).
	StepResults map[string]*StepResult

	// Interactions retains pending and accepted interaction state until the
	// owning step result commits.
	Interactions map[string]*InteractionState

	// Dispatches is the durable intent/result journal for external occurrences.
	Dispatches map[string]*DispatchState

	// ExecutionFrames retain exact progress through structural containers.
	ExecutionFrames map[string]*ExecutionFrameState

	// DynamicIncludes retain immutable resolution pins for active/history occurrences.
	DynamicIncludes map[string]*DynamicIncludeResolutionState
	PendingHandoff  *HandoffRequest

	// CurrentStepIndex is the index into Plan.Steps of the step being executed.
	// -1 indicates no step has started yet.
	CurrentStepIndex int

	// CheckpointSequence is the last successfully committed checkpoint number.
	CheckpointSequence int64

	// CommittedTraceSequence and PendingTraceEvents bind recoverable trace
	// projection records to the same mutation as execution state.
	CommittedTraceSequence int64
	PendingTraceEvents     []Event

	// WriterEpoch is the active durable store lease epoch.
	WriterEpoch uint64

	// StartedAt is when the run was started (run/started event emitted).
	StartedAt time.Time

	// CompletedAt is when the run reached a terminal state.
	// Zero value until Status is completed/failed/cancelled.
	CompletedAt time.Time

	// Error holds the error that caused failure, if Status is RunStatusFailed.
	Error error

	// Sequence is the monotonically increasing event sequence number.
	// Incremented before each trace event emission.
	Sequence int64

	// Actor is the identity of the user/system that initiated the run.
	Actor string

	// Mode is the execution mode (real, dry-run, replay).
	Mode RunMode

	// Client identifies the runtime surface that initiated this run.
	// Values: "cli", "server", "mobile-ios", "mobile-android"
	Client string
}

// NewRun creates a new Run in pending state from an ExecutionPlan.
func NewRun(id string, plan *ExecutionPlan, opts RunOptions) *Run {
	run := &Run{
		ID:               id,
		Status:           RunStatusPending,
		Plan:             plan,
		Vars:             make(map[string]any),
		StepResults:      make(map[string]*StepResult),
		Interactions:     make(map[string]*InteractionState),
		Dispatches:       make(map[string]*DispatchState),
		ExecutionFrames:  make(map[string]*ExecutionFrameState),
		DynamicIncludes:  make(map[string]*DynamicIncludeResolutionState),
		CurrentStepIndex: -1,
		Actor:            opts.Actor,
		Mode:             opts.Mode,
		Client:           opts.Client,
	}
	for k, v := range opts.Vars {
		run.Vars[k] = v
	}
	for k, v := range opts.RuntimeVars {
		run.Vars[k] = v
	}
	return run
}

// IndeterminateRecord carries trace-safe evidence about a step whose completion
// could not be established (e.g., a mutating action that timed out).
//
// Non-nil on StepResult means: the step was dispatched but the engine cannot
// confirm whether it completed, partially executed, or had no effect.
//
// Security invariant: EndpointHost holds ONLY the parsed hostname. Credentials,
// tokens, Authorization headers, and full URLs must never appear in this struct,
// its serialized form, trace events, or error messages.
type IndeterminateRecord struct {
	// RunID identifies the run in which the indeterminate event occurred.
	RunID string `json:"run_id"`
	// StepID identifies the step that could not be confirmed.
	StepID string `json:"step_id"`
	// ToolName is the logical tool name from the runbook step.
	ToolName string `json:"tool_name"`
	// ActionName is the logical action name from the runbook step.
	ActionName string `json:"action_name"`
	// Classification is the action's declared classification:
	// "read-only", "mutating", "destructive", or "unspecified" when absent.
	Classification string `json:"classification"`
	// EndpointHost is the parsed hostname of the tool endpoint — host only.
	// Credentials, tokens, and query parameters are NEVER included.
	EndpointHost string `json:"endpoint_host,omitempty"`
	// AttemptNumber is the 1-based dispatch attempt count when the loss occurred.
	AttemptNumber int `json:"attempt_number"`
	// Deadline is the context deadline that was in force, if any.
	// Zero value means no explicit deadline was set.
	Deadline time.Time `json:"deadline,omitempty"`
	// FailureTime is when the engine detected the loss.
	FailureTime time.Time `json:"failure_time"`
	// TransportErrCategory categorises the error:
	// "context-deadline-exceeded", "context-canceled", or "network-error".
	TransportErrCategory string `json:"transport_err_category"`
}

// StepResult is returned by RunHandle.Next after a step completes.
type StepResult struct {
	PublicOutputs   map[string]any `json:"public_outputs,omitempty"`
	Results         *RunResults    `json:"results,omitempty"`
	RequiredFailure bool           `json:"required_failure,omitempty"`
	// StepID is the ID of the step that was executed.
	StepID string

	// Status is the final state of this step.
	Status StepStatus

	// Outcome describes the semantic result (deprecated: use Status).
	Outcome StepOutcome

	// Output holds step-produced data (e.g., CLI stdout, tool response).
	Output map[string]any

	// Vars holds variables produced by this step for subsequent steps.
	Vars map[string]any

	// StartedAt is when step execution began.
	StartedAt time.Time

	// CompletedAt is when step execution ended.
	CompletedAt time.Time

	// DurationMs is the step execution time in milliseconds.
	DurationMs int64

	// Error holds the error if Status is StepStatusFailed.
	Error error

	// Evidence holds structured evidence collected during this step.
	// nil if no evidence was collected.
	Evidence []evidence.EvidenceRecord `json:"evidence,omitempty"`

	// Indeterminate is non-nil when Status is StepStatusIndeterminate.
	// It carries trace-safe evidence about the invocation. A non-nil pointer
	// is an unambiguous signal that completion could not be established — this
	// is intentionally a pointer rather than a sentinel value in an existing
	// field, because a tool that legitimately returns nothing also yields
	// Output == nil, making nil-output ambiguous.
	Indeterminate *IndeterminateRecord `json:"indeterminate,omitempty"`
}

// StepStatus is the state machine state for a single step.
type StepStatus string

const (
	StepStatusPending   StepStatus = "pending"
	StepStatusRunning   StepStatus = "running"
	StepStatusCompleted StepStatus = "completed"
	StepStatusFailed    StepStatus = "failed"
	StepStatusSkipped   StepStatus = "skipped"
	StepStatusWaiting   StepStatus = "waiting"
	StepStatusDenied    StepStatus = "denied"
	// StepStatusIndeterminate means the step was dispatched but its completion
	// (success or failure) could not be confirmed. The action may have already
	// executed. The run is halted; StepResult.Indeterminate carries trace evidence.
	StepStatusIndeterminate StepStatus = "indeterminate"
)

// StepOutcome describes the result of a single step execution.
type StepOutcome string

const (
	StepOutcomeSuccess StepOutcome = "success"
	StepOutcomeSkipped StepOutcome = "skipped"
	StepOutcomeFailed  StepOutcome = "failed"
	StepOutcomeWaiting StepOutcome = "waiting"
	StepOutcomeDenied  StepOutcome = "denied"
)

// ApprovalDecision is submitted by an approver via RunHandle.Approve.
type ApprovalDecision struct {
	Approver string
	Role     string
	Decision string // "approved" | "rejected"
	Comment  string
}

// EvidenceValue is a single piece of evidence submitted for an interactive step.
type EvidenceValue struct {
	Type  string
	Value any
}

// Event is a structured runtime event emitted during execution.
type Event struct {
	EventID   string         `json:"event_id"`
	RunID     string         `json:"run_id"`
	RunbookID string         `json:"runbook_id"`
	Timestamp string         `json:"timestamp"`
	Kind      string         `json:"kind"`
	Sequence  int64          `json:"sequence"`
	Payload   map[string]any `json:"payload"`
}

// RunStore persists run state and trace events between sessions.
type RunStore interface {
	// SaveState checkpoints the current run state.
	SaveState(ctx context.Context, state RunState) error

	// LoadState restores run state from a prior checkpoint.
	LoadState(ctx context.Context, runID string) (RunState, error)

	// WriteTrace appends a trace event to the run's JSONL file.
	WriteTrace(ctx context.Context, runID string, event Event) error

	// Close flushes and closes the store.
	Close() error
}

// DurableRunStore persists the immutable executable plan together with run
// state. Engines require this complete contract whenever persistence is enabled.
type DurableRunStore interface {
	RunStore

	// AcquireRunLease exclusively fences mutations for one active run handle.
	AcquireRunLease(ctx context.Context, runID string) (RunLease, error)

	// SavePlan publishes the immutable executable plan for a run.
	SavePlan(ctx context.Context, runID string, plan *ExecutionPlan) error

	// LoadPlan restores the exact executable plan published for a run.
	LoadPlan(ctx context.Context, runID string) (*ExecutionPlan, error)

	// PlanDigest returns the digest of the plan most recently saved or loaded.
	PlanDigest(runID string) (string, bool)
}

// ExecutionPlan is a resolved, immutable plan ready for the runtime.
// (Defined here to avoid an import cycle between engine and planner.)
type ExecutionPlan struct {
	RunID       string
	RunbookPath string
	Steps       []ResolvedStep
	Tools       map[string]*schema.ToolDef
	Providers   map[string]*schema.ProviderDef
	Governance  governance.GovernancePolicy
	// GovernanceSource is the raw runbook governance config, carried alongside
	// the compiled Governance policy so the engine can construct a per-run
	// PolicyEvaluator at Start() time (BuildEvaluator requires the config, not
	// the compiled policy). Nil when the runbook has no governance: block.
	GovernanceSource *schema.GovernanceConfig
	Metadata         PlanMetadata
	// Inputs is the root runbook's own declared inputs.<name> block (S3),
	// carried onto the plan so plan-time enum default validation
	// (ENUM-006) and ValidatedPlan enum metadata carriage (AR-ENUM-10) can
	// see it without re-reading the source file.
	Inputs map[string]*schema.Input
	// Outputs is the root runbook's own declared outputs.<name> block
	// (S4), carried for the same reason.
	Outputs  map[string]*schema.Output
	Bindings []schema.Binding
	// Validation records successful parse-gate validation metadata.
	Validation *ValidatedPlan
}

// ResolvedStep is a single step in the execution plan, fully resolved.
type ResolvedStep struct {
	ID               string
	Name             string // Human-readable title from runbook (falls back to ID if empty)
	Subtitle         string
	Kind             string
	Spec             StepSpec
	Capture          map[string]string
	CaptureDefaults  map[string]any
	Depth            int
	NestDepth        int // Visual nesting level for display (0 = top-level, 1 = inside one include, etc.)
	DisplayOrder     int // Pre-order index for correct display ordering (parents before children)
	Origin           string
	OnError          string // Error routing: "continue", "stop", or "goto:<step_id>"
	ContinueOnFail   bool   // Legacy field - treated as on_error: continue
	Timeout          string // Execution timeout duration from the runbook common step field.
	Delay            string // Pre-execution delay duration (e.g. "3s"); honored by the engine before invoking the step's executor.
	When             string // GXL condition from the runbook common step field.
	Retry            *schema.RetryConfig
	Scope            string
	Export           []string
	Contract         *schema.Contract
	RequiredEvidence []schema.EvidenceRequirement

	// Structural metadata (used by UIs to render hierarchy).
	// Empty when the step is top-level.
	ParentID     string // ID of the structural parent (iterate/branch/parallel/include/compensate); empty if top-level
	ParentKind   string // "iterate" | "branch" | "parallel" | "include" | "compensate"; empty if top-level
	IncludeAlias string // For include steps: the alias from the parent runbook's `imports:` map (empty if not aliased)
	BranchLabel  string // For direct children of a branch arm: the arm's label (empty otherwise)
}

// PlanMetadata carries non-execution metadata about an execution plan.
type PlanMetadata struct {
	PlanHash           string
	GraphContentHash   string
	RunbookContentHash string
	Regions            *regschema.Manifest
	PlannedAt          time.Time
	RunbookID          string
	RunbookName        string
	Extensions         []*schema.ExtensionRef
	// CatalogDigest is pkgcatalog.Catalog.CatalogDigest() computed for this
	// plan's frozen package catalog.
	// 13-evidence-tracing-resumption.tex §Package Resumption Contract).
	// Persisted as part of RunState.Plan (engine.RunStore.SaveState), so
	// a resume/replay path can recompute the current catalog digest and
	// compare it against this recorded value via pkg/pkgdrift.Evaluate.
	// Empty for plans that never resolved a package catalog (no requires:).
	CatalogDigest string
	// PackageDigests maps each resolved package name to its
	// pkgcatalog.LockedPackageInfo.Digest, for per-package (not just
	// whole-catalog) drift comparison on resume.
	PackageDigests map[string]string
	// DynamicIncludes records every dynamic include resolution that occurred
	// during execution, in resolution order. Used by replay to re-bind
	// without re-resolving, and by resume to detect drift.
	DynamicIncludes []schema.LockedDynamicInclude
	// Profile is the runtime profile loaded from --profile, if one was
	// supplied. Nil when no profile was provided. Static preflight enforces
	// AllowedEnvironments, attendance, and test-context transport rules during
	// Plan(). Execution consumes this field for approval gate decisions.
	// The profile is never used to re-resolve or override which package or
	// tool definition was selected by --package-map.
	Profile *schema.RuntimeProfile
}

// Ensure io is used — RunHandle.Next returns io.EOF at end of run.
var _ = io.EOF
