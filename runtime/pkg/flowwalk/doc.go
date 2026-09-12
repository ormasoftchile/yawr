// Package flowwalk implements a visitor-pattern traversal over the parsed
// runbook AST. It is the shared traversal mechanism used by:
//
//   - The planner (to flatten a runbook into an execution plan).
//   - The preview document builder (to produce a structure-preserving graph).
//   - Any future consumer that needs to walk the schema (linters, doc tools,
//     coverage analyzers).
//
// The walker handles the mechanical concerns once — flow-node dispatch, branch
// arms, parallel branches, iterate bodies, compensate bodies, and recursive
// include resolution with cycle detection and max-depth enforcement. Visitors
// only implement the methods they care about; the [Base] type provides empty
// defaults that consumers may embed.
//
// Each include hop is reported via [Visitor.EnterInclude]; the visitor decides
// whether to descend into the child runbook (planner: yes — flatten everything
// for execution; preview: typically no — keep the include as an opaque node
// whose body is referenced separately).
//
// flowwalk does not evaluate templates, conditions, or runtime expressions;
// it operates strictly on the static parse tree.
package flowwalk
