package engine

import (
	"context"
	"errors"

	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	"github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	otelPkg "github.com/ormasoftchile/yawr/runtime/pkg/otel"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// ErrNotImplemented is returned by stub implementations.
var ErrNotImplemented = errors.New("engine: not implemented")

// Engine manages the lifecycle of runbook executions.
// An Engine is constructed once with an EngineConfig and used
// to start or resume multiple runs.
type Engine interface {
	// Start initializes a run from a plan and returns a RunHandle.
	// The run is paused at step 0 until Next is called on the handle.
	Start(ctx context.Context, plan *ExecutionPlan, opts RunOptions) (RunHandle, error)

	// Resume restores a previously checkpointed run from the run store.
	Resume(ctx context.Context, runID string, opts RunOptions) (RunHandle, error)
}

// EngineConfig holds the dependencies required to construct an Engine.
// All fields are required unless explicitly marked optional.
type EngineConfig struct {
	// Executors maps step kinds to their executor implementations.
	// Required: the engine dispatches each step to the registered executor.
	Executors ExecutorRegistry

	// Dispatcher handles wait_for_event synchronization.
	// Required: enables steps to wait for external events with consume semantics.
	Dispatcher eventbus.EventDispatcher

	// TraceWriter writes trace events to the append-only JSONL file.
	// Required: all runtime events are written through this interface.
	TraceWriter trace.TraceWriter

	// Platform provides signal handling and environment access.
	// Required: used for graceful shutdown on SIGTERM/SIGINT.
	Platform platform.Platform

	// EventBus is the in-process event fan-out bus.
	// Optional: if nil, events are only written to TraceWriter.
	EventBus eventbus.EventBus

	// Store persists run state for checkpoint/resume.
	// Optional: if nil, runs cannot be resumed after process exit.
	Store RunStore

	// EvidenceHook is called after each step execution to collect evidence.
	// Optional: if nil, no evidence is collected beyond executor output.
	EvidenceHook EvidenceHook

	// OnEvent is an optional callback invoked for each runtime event.
	// Called synchronously after trace write but before step completion.
	OnEvent func(Event)

	// GovernanceEvaluator performs pre-flight governance checks on each step.
	// Optional: if nil, all steps are allowed (no governance).
	GovernanceEvaluator governance.PolicyEvaluator

	// ApprovalGate handles approval requests for steps requiring approval.
	// Optional: if nil, approval-required steps fail with an error.
	ApprovalGate governance.ApprovalGate

	// TransitionValidator verifies handoffs and terminal completion before
	// their durable state is committed. Optional outside strict replay.
	TransitionValidator TransitionValidator

	// Evaluator resolves template expressions in step fields.
	// Optional: if nil, template evaluation is skipped.
	Evaluator expr.Evaluator

	// ConditionEvaluator evaluates boolean expressions for step conditions.
	// Optional: if nil, conditions default to true.
	ConditionEvaluator expr.ConditionEvaluator

	// PromptProvider supplies interactive input for choice/decision/collector steps.
	// Optional: if nil, interactive steps are skipped.
	PromptProvider input.PromptProvider

	// InputProvider resolves prompt step inputs.
	// Optional: if nil, a default provider chain is used.
	InputProvider input.InputProvider

	// ToolRuntime invokes external tools via stdio/jsonrpc/mcp transports.
	// Optional: if nil, tool steps fail with ErrNotImplemented.
	ToolRuntime tool.ToolRuntime

	// ExtensionHost manages extension lifecycle and contributions.
	// Optional: if nil, no extensions are loaded.
	ExtensionHost extension.ExtensionHost

	// TracerProvider creates OTel-compatible tracers for span instrumentation.
	// Optional: if nil, a noop tracer is used (zero overhead).
	TracerProvider otelPkg.TracerProvider
}

// EvidenceHook collects evidence from a completed step.
// Implementations MUST NOT modify the StepResult; they return new evidence records.
type EvidenceHook interface {
	// Collect examines the step result and returns evidence records.
	// The run directory is provided for attachment storage.
	Collect(ctx context.Context, step ResolvedStep, result *StepResult, runDir string) ([]evidence.EvidenceRecord, error)
}

// Validate checks that all required fields in EngineConfig are set.
// Returns an error listing all missing required fields.
func (c *EngineConfig) Validate() error {
	var missing []string
	if c.Executors == nil {
		missing = append(missing, "Executors")
	}
	if c.Dispatcher == nil {
		missing = append(missing, "Dispatcher")
	}
	if c.TraceWriter == nil {
		missing = append(missing, "TraceWriter")
	}
	if c.Platform == nil {
		missing = append(missing, "Platform")
	}
	if len(missing) > 0 {
		return &ConfigError{Missing: missing}
	}
	return nil
}

// ConfigError is returned when EngineConfig validation fails.
type ConfigError struct {
	Missing []string
}

func (e *ConfigError) Error() string {
	return "engine: missing required config fields: " + joinStrings(e.Missing, ", ")
}

// joinStrings is a minimal join helper to avoid importing strings.
func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	result := ss[0]
	for _, s := range ss[1:] {
		result += sep + s
	}
	return result
}

// BranchExecutor executes parallel branches and collects results.
// Each branch runs in a goroutine; results are collected in declaration order.
// Trace events are buffered per branch and flushed in order at join.
//
// This interface is internal to the engine but exposed for testing.
// Production implementations use the engine's TraceWriter and Dispatcher.
type BranchExecutor interface {
	// ExecuteBranches runs all branches concurrently and waits for completion.
	// Returns branch results in declaration order (branches[0] → results[0]).
	// On any branch failure with fail-fast semantics, cancels remaining branches.
	ExecuteBranches(ctx context.Context, branches []BranchSpec, run *Run) ([]BranchResult, error)
}

// BranchSpec describes a single parallel branch to execute.
type BranchSpec struct {
	Label string
	Steps []ResolvedStep
}

// BranchResult holds the outcome of a single parallel branch.
type BranchResult struct {
	Label   string
	Results []*StepResult
	Error   error
}
