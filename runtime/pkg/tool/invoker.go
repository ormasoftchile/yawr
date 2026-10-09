package tool

import (
	"context"
	"errors"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ErrIdempotencyConflict reports that a caller-supplied idempotency key was
// reused with a request whose canonical digest differs from the original.
var ErrIdempotencyConflict = errors.New("dispatch: idempotency key reused with a different request")

// InvocationRequest is a single ad-hoc tool invocation.
//
// Trust boundary. The fields fall into two classes and a host MUST keep them
// apart:
//
//   - Untrusted invocation content — Tool, Action, Arguments. These may come
//     straight from an agent or model. They are validated against the
//     registered action's argument schema but confer no authority.
//   - Trusted execution context — Authority, CorrelationID, IdempotencyKey.
//     These MUST be constructed by the hosting application from its own
//     authenticated state (session, activity, role policy). A host must never
//     deserialize them from agent output or from an MCP tools/call payload.
//
// ToolInvoker is an in-process Go API; it cannot tell where a value came
// from, so this separation is the host's responsibility.
type InvocationRequest struct {
	// Tool names the registered tool. Untrusted.
	Tool string `json:"tool"`

	// Action names the action on the tool. Untrusted.
	Action string `json:"action"`

	// Arguments supplies action-defined arguments. Untrusted.
	Arguments map[string]any `json:"arguments"`

	// Authority is the host-issued execution envelope. Trusted.
	Authority InvocationAuthority `json:"-"`

	// CorrelationID is a host-issued trace identifier. Trusted.
	CorrelationID string `json:"-"`

	// IdempotencyKey is a host-issued operation identity. Trusted.
	//
	// When set, the invocation is deduplicated: an identical retry returns the
	// recorded outcome without repeating the effect, a retry with different
	// arguments is rejected, and a retry after an interrupted attempt returns
	// InvocationStatusIndeterminate. Keys are scoped by Authority.Actor.
	//
	// When empty, every call is a distinct occurrence (its intent is still
	// journaled before execution) and no deduplication happens.
	IdempotencyKey string `json:"-"`
}

// InvocationAuthority is the trusted envelope a host grants one invocation.
// See InvocationRequest for the trust boundary.
//
// WorkspaceRoot is advisory metadata for tools that honour it. The invoker
// does not and cannot confine what an arbitrary native/stdio process touches
// on disk; containment requires a transport with OS-level isolation (today
// only native-file-only on Windows AMD64) or a tool that enforces its own
// boundary.
type InvocationAuthority struct {
	// Actor identifies the principal, e.g. "a4c:activity/123:copilot".
	Actor string

	// Context selects the profile tier (e.g. production, ci, test). The test
	// context relaxes classification approval exactly as it does for
	// workflows; hosts should not use it for real effects.
	Context schema.ProfileContext

	// Attendance selects attended (approval prompts possible) or unattended.
	Attendance schema.ProfileAttendance

	// Effect allowances. An action whose classification is not allowed is
	// denied before policy evaluation. Unclassified actions are always denied.
	AllowRead        bool
	AllowMutating    bool
	AllowDestructive bool

	// AllowedTools restricts invocable tools by exact name or "prefix*".
	// Empty means no tool-name restriction (classification limits still apply).
	AllowedTools []string

	// WorkspaceRoot is advisory; see the type comment.
	WorkspaceRoot string

	// ApprovalGate overrides the invoker's gate for this invocation.
	ApprovalGate governance.ApprovalGate

	// PolicyEvaluator replaces the default profile evaluator. Allowance and
	// allowlist checks still run first.
	PolicyEvaluator governance.PolicyEvaluator
}

// InvocationStatus is the terminal state of an invocation.
type InvocationStatus string

const (
	// InvocationStatusCompleted: the tool ran and its outcome is known.
	InvocationStatusCompleted InvocationStatus = "completed"

	// InvocationStatusDenied: blocked by authority, policy or an approver
	// before any external effect.
	InvocationStatusDenied InvocationStatus = "denied"

	// InvocationStatusApprovalRequired: policy requires approval but no
	// ApprovalGate is available. Nothing executed. An adapter can surface this
	// as a pending outcome and retry once approval is obtained out of band.
	InvocationStatusApprovalRequired InvocationStatus = "approval_required"

	// InvocationStatusFailed: validation failed, or the tool ran and reported
	// failure (outcome known).
	InvocationStatusFailed InvocationStatus = "failed"

	// InvocationStatusIndeterminate: an effect may or may not have happened
	// (interrupted or cancelled after the intent was journaled). Requires
	// explicit operator resolution; never retried automatically.
	InvocationStatusIndeterminate InvocationStatus = "indeterminate"
)

// InvocationResult is the structured outcome of an invocation.
type InvocationResult struct {
	Status         InvocationStatus `json:"status"`
	Classification string           `json:"classification,omitempty"`

	Output   map[string]any `json:"output,omitempty"`
	Stdout   string         `json:"stdout,omitempty"`
	Stderr   string         `json:"stderr,omitempty"`
	ExitCode int            `json:"exit_code"`

	// OccurrenceID identifies the journaled dispatch occurrence.
	OccurrenceID string `json:"occurrence_id,omitempty"`
	// IdempotencyKey echoes the caller's key (unscoped).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// CorrelationID echoes the caller's correlation identifier.
	CorrelationID string `json:"correlation_id,omitempty"`
	// Replayed is true when the result was served from the journal and the
	// tool was not executed again.
	Replayed bool `json:"replayed,omitempty"`

	// Governance is the policy evaluator's evidence for this decision.
	Governance *governance.Evidence `json:"governance,omitempty"`
	// ApprovalRecord is set when an approver granted the invocation.
	ApprovalRecord *governance.ApprovalRecord `json:"approval_record,omitempty"`
	// Evidence holds captured execution evidence (stdout/stderr, approval).
	Evidence []evidence.EvidenceRecord `json:"evidence,omitempty"`

	Error        error  `json:"-"`
	ErrorMessage string `json:"error,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
}

// ToolInvoker executes one registered tool action under host authority.
// Construct the production implementation with pkg/invoke.New or pkg/run.NewToolInvoker.
type ToolInvoker interface {
	Invoke(ctx context.Context, req InvocationRequest) (*InvocationResult, error)
}

// InvokerOptions configures a ToolInvoker.
type InvokerOptions struct {
	Registry      ToolRegistry
	Runtime       ToolRuntime
	ApprovalGate  governance.ApprovalGate
	DispatchStore any    // StandaloneDispatchStore or engine.DispatchJournal
	StateDir      string // If DispatchStore is nil and StateDir is non-empty, a persistent store is used
}

type invokerFactoryFunc func(opts InvokerOptions) (ToolInvoker, error)

var (
	invokerFactoryMu sync.RWMutex
	invokerFactory   invokerFactoryFunc
)

// RegisterInvokerFactory registers the default constructor implementation.
func RegisterInvokerFactory(factory invokerFactoryFunc) {
	invokerFactoryMu.Lock()
	defer invokerFactoryMu.Unlock()
	invokerFactory = factory
}

// NewInvoker constructs a ToolInvoker using the registered factory.
func NewInvoker(opts InvokerOptions) (ToolInvoker, error) {
	invokerFactoryMu.RLock()
	factory := invokerFactory
	invokerFactoryMu.RUnlock()
	if factory == nil {
		return nil, errors.New("tool: no invoker factory registered (import runtime/internal/tool or runtime/pkg/run)")
	}
	return factory(opts)
}
