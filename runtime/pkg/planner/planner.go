package planner

import (
	"context"
	"errors"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// Planner resolves a parsed runbook into a fully-resolved ExecutionPlan.
// It validates references, resolves includes, discovers tools, and produces
// an immutable plan suitable for the engine runtime.
type Planner interface {
	// Plan transforms a parsed runbook into an execution plan.
	// Returns an error if the runbook has unresolved references, import cycles,
	// missing tools, or other planning-time validation failures.
	Plan(ctx context.Context, rb *parser.ParsedRunbook) (*engine.ExecutionPlan, error)
}

// RunbookLoader loads parsed runbooks by path.
// The planner uses this interface to resolve include steps.
// Implementations may read from the filesystem, cache, or test fixtures.
type RunbookLoader interface {
	// Load returns a parsed runbook at the given path.
	// Path resolution is relative to the base directory configured in the loader.
	// Returns an error if the runbook cannot be found or fails to parse.
	Load(ctx context.Context, path string) (*parser.ParsedRunbook, error)
}

// ToolRegistry looks up tool definitions by reference.
// The planner uses this interface to resolve tool_call steps.
// Implementations scan tool directories, parse .tool.yaml files, or use test fixtures.
type ToolRegistry interface {
	// Lookup returns the tool definition matching the given name and action.
	// Returns ErrToolNotFound if no matching tool exists.
	// Returns ErrActionNotFound if the tool exists but lacks the requested action.
	Lookup(ctx context.Context, name string, action string) (*schema.ToolDef, error)
}

// Config configures a Planner instance.
type Config struct {
	// Loader provides the RunbookLoader for resolving include steps.
	// Required — planner will panic if nil.
	Loader RunbookLoader

	// Tools provides the ToolRegistry for resolving tool_call steps.
	// Required — planner will panic if nil.
	Tools ToolRegistry

	// BaseDir is the directory for resolving relative runbook paths.
	// Defaults to the directory of the root runbook if empty.
	BaseDir string

	// MaxIncludeDepth limits recursion depth for nested includes.
	// Defaults to 10 if zero.
	MaxIncludeDepth int

	// ExpandPolicy controls when included sub-runbooks are materialized
	// into the plan. Zero value (Default=ModeUnset) preserves today's
	// fully-eager behavior. Per-runbook and per-include `expand:` fields
	// take precedence over this default. See pkg/expand.
	ExpandPolicy expand.Policy

	// Profile is the runtime profile to apply during planning for Tier 0
	// static preflight checks (AllowedEnvironments, attendance, test-context
	// transport rules). Nil profile disables all profile-aware checks and
	// preserves existing behavior exactly. Profile-aware checks consume
	// resolved tool definitions from the catalog (post-package-map); they
	// never re-resolve toolRefs independently.
	Profile *schema.RuntimeProfile
}

// Error codes for planner failures.
var (
	// ErrNotImplemented is returned by stub implementations.
	ErrNotImplemented = errors.New("planner: not implemented")

	// ErrToolNotFound indicates the requested tool does not exist.
	ErrToolNotFound = errors.New("planner: tool not found")

	// ErrActionNotFound indicates the tool exists but lacks the action.
	ErrActionNotFound = errors.New("planner: action not found")

	// ErrRunbookNotFound indicates the included runbook does not exist.
	ErrRunbookNotFound = errors.New("planner: runbook not found")

	// ErrImportCycle indicates a circular include dependency.
	ErrImportCycle = errors.New("planner: import cycle detected")

	// ErrMaxDepthExceeded indicates include nesting exceeded the limit.
	ErrMaxDepthExceeded = errors.New("planner: max include depth exceeded")

	// ErrContextMismatch (PLAN-010) indicates a tool's AllowedEnvironments
	// does not include the active profile's context. This is the portability
	// gap Cristiano reported: the tool exists and is correctly authored, but
	// it has not been configured to run in the requested runtime context.
	ErrContextMismatch = errors.New("planner: tool not allowed in this context")

	// ErrAttendanceMismatch (PLAN-011) indicates the profile declares
	// attendance: attended but the context is structurally unattended (ci,
	// headless-server). Detected against declared state only — no TTY seam
	// exists in this codebase.
	ErrAttendanceMismatch = errors.New("planner: attendance mismatch")

	// ErrTestContextBinding (PLAN-012) indicates a test-context profile is
	// bound to a transport that the test context does not permit.
	// mcp-http: never allowed, no override.
	// mcp subprocess: blocked by default; allowed with
	// transport.allow_subprocess_in_test: true (auditable author assertion,
	// NOT a sandbox guarantee — the subprocess inherits the full parent env).
	ErrTestContextBinding = errors.New("planner: non-native transport binding in test context")

	// ErrEndpointHostNotAllowed (PLAN-013) indicates a profile's per-tool
	// endpoint override routes to a host not declared in the tool definition's
	// auth.allowed_hosts. Enforced at planning time (before execution) so no
	// override executes unvalidated. Ratified Rule A: a profile substitutes
	// who acquires the token; it never changes where the token may be sent.
	ErrEndpointHostNotAllowed = errors.New("planner: profile endpoint override host not in allowed_hosts")
)

// PlanError wraps a planner error with context about the failing element.
type PlanError struct {
	// Code is one of the Err* sentinel values.
	Code error
	// StepID is the step that caused the error (may be empty for runbook-level errors).
	StepID string
	// Path is the runbook path involved (for include/import errors).
	Path string
	// Detail provides additional context.
	Detail string
}

// Error implements the error interface.
func (e *PlanError) Error() string {
	if e.StepID != "" {
		return fmt.Sprintf("%s: step %s: %s", e.Code, e.StepID, e.Detail)
	}
	if e.Path != "" {
		return fmt.Sprintf("%s: %s: %s", e.Code, e.Path, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

// Unwrap returns the underlying error code for errors.Is matching.
func (e *PlanError) Unwrap() error {
	return e.Code
}
