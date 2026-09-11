// Package planner defines the planning pipeline interfaces.
//
// The Planner takes a parsed runbook from the parser and produces a fully-resolved
// ExecutionPlan for the engine. It performs three jobs:
//  1. Import resolution — recursively resolves include steps to their target runbooks
//  2. Tool discovery — matches tool_call steps to registered tool definitions
//  3. ExecutionPlan production — flattens the runbook tree into an ordered step list
//
// # Interfaces
//
//   - Planner: main interface that transforms ParsedRunbook → ExecutionPlan
//   - RunbookLoader: abstraction for loading runbooks (file system, test fixtures, etc.)
//   - ToolRegistry: abstraction for looking up tool definitions by name/action
//
// # Usage
//
// The Planner is typically constructed by the engine with a concrete RunbookLoader
// (backed by the filesystem and parser) and a concrete ToolRegistry (backed by
// the tool directory scanner). Tests can inject in-memory implementations.
//
// Errors from the planner are structured as *PlanError with specific error codes
// for import cycles, missing tools, unresolved references, etc.
package planner
