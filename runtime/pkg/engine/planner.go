package engine

import (
	"context"

	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
)

// Planner resolves a parsed runbook into a fully-resolved ExecutionPlan.
// The planner performs three jobs:
//  1. Import resolution — recursively resolves include steps to referenced runbooks
//  2. Tool discovery — matches tool_call step tool refs to known tool definitions
//  3. ExecutionPlan production — produces a flat, fully-resolved execution plan
//
// Implementations live in internal/planner; configuration (RunbookLoader, ToolRegistry)
// interfaces are defined in pkg/planner.
type Planner interface {
	// Plan validates and resolves a parsed runbook, returning an immutable
	// ExecutionPlan ready for the runtime.
	// The context carries cancellation and deadline.
	Plan(ctx context.Context, rb *parser.ParsedRunbook) (*ExecutionPlan, error)
}
