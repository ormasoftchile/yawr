package engine

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// DebugPhase identifies whether a debugger observes a step before dispatch or
// after execution but before the result is committed.
type DebugPhase string

const (
	DebugPhaseBefore DebugPhase = "before"
	DebugPhaseAfter  DebugPhase = "after"
)

// ErrDebugStopped identifies an operator-requested debugger stop across
// nested engine boundaries while callers still observe context cancellation.
var ErrDebugStopped = errors.New("engine: debugger stopped run")

// DebugAction controls how execution proceeds after a debug pause.
type DebugAction string

const (
	DebugActionContinue DebugAction = "continue"
	DebugActionStop     DebugAction = "stop"
	DebugActionStepInto DebugAction = "step_into"
	DebugActionStepOver DebugAction = "step_over"
	DebugActionStepOut  DebugAction = "step_out"
)

// DebugCallFrame identifies one container invocation in the path from the root
// runbook to a nested step.
type DebugCallFrame struct {
	StepID      string `json:"step_id" yaml:"step_id"`
	RunbookPath string `json:"runbook_path,omitempty" yaml:"runbook_path,omitempty"`
}

// DebugLocation uniquely identifies a step invocation within a debug run.
type DebugLocation struct {
	RunID       string           `json:"run_id"`
	RunbookPath string           `json:"runbook_path"`
	CallPath    []DebugCallFrame `json:"call_path,omitempty"`
	StepID      string           `json:"step_id"`
	Invocation  int              `json:"invocation"`
	Attempt     int              `json:"attempt"`
}

// DebugSnapshot is an isolated view of engine state supplied to a debugger.
// Actual is present only after execution and is never the executor-owned result.
type DebugSnapshot struct {
	Phase           DebugPhase     `json:"phase"`
	Location        DebugLocation  `json:"location"`
	Vars            map[string]any `json:"vars,omitempty"`
	ProtectedVars   []string       `json:"protected_vars,omitempty"`
	Actual          *StepResult    `json:"actual,omitempty"`
	OutputProtected bool           `json:"output_protected,omitempty"`
	CanStepInto     bool           `json:"can_step_into,omitempty"`
}

// DebugResultOverride alters selected fields on the effective result. OutputPatch
// uses JSON Merge Patch semantics; nil leaves executor output unchanged.
type DebugResultOverride struct {
	Status      StepStatus     `json:"status,omitempty"`
	OutputPatch map[string]any `json:"output_patch,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// DebugDecision is returned by a DebugController. Vars patch the live runtime
// scope. Result is valid only for an after-execution snapshot.
type DebugDecision struct {
	Action DebugAction          `json:"action"`
	Vars   map[string]any       `json:"vars,omitempty"`
	Result *DebugResultOverride `json:"result,omitempty"`
}

// DebugController is an optional, client-neutral execution hook. Implementations
// return immediately when a location has no matching breakpoint or profile.
type DebugController interface {
	Pause(ctx context.Context, snapshot DebugSnapshot) (DebugDecision, error)
}

type debugCallPathContextKey struct{}
type debugControllerContextKey struct{}
type debugInvocationTrackerContextKey struct{}

// DebugProtection is the engine's in-memory run redaction state shared by
// debugger snapshots, trace events, and structured protocol output.
// SecretValues must never be serialized, emitted, or attached to public context.
type DebugProtection struct {
	ProtectedVars     []string
	SecretValues      []string
	RedactionPatterns []*governance.RedactionPattern
}

// DebugInvocationTracker allocates monotonically increasing invocation numbers
// for qualified step identities across root and nested sub-engines.
type DebugInvocationTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewDebugInvocationTracker() *DebugInvocationTracker {
	return &DebugInvocationTracker{counts: make(map[string]int)}
}

func NewDebugInvocationTrackerFromCounts(counts map[string]int) *DebugInvocationTracker {
	tracker := NewDebugInvocationTracker()
	for key, count := range counts {
		tracker.counts[key] = count
	}
	return tracker
}

func (tracker *DebugInvocationTracker) Next(path []DebugCallFrame, stepID string) int {
	if tracker == nil {
		return 1
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.counts == nil {
		tracker.counts = make(map[string]int)
	}
	key := DebugNodeID(path, stepID)
	tracker.counts[key]++
	return tracker.counts[key]
}

func (tracker *DebugInvocationTracker) Rewind(path []DebugCallFrame, stepID string) {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	key := DebugNodeID(path, stepID)
	if tracker.counts[key] <= 1 {
		delete(tracker.counts, key)
		return
	}
	tracker.counts[key]--
}

func (tracker *DebugInvocationTracker) Current(path []DebugCallFrame, stepID string) int {
	if tracker == nil {
		return 0
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.counts[DebugNodeID(path, stepID)]
}

func (tracker *DebugInvocationTracker) Snapshot() map[string]int {
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	counts := make(map[string]int, len(tracker.counts))
	for key, count := range tracker.counts {
		counts[key] = count
	}
	return counts
}

// WithDebugCallPath returns a context carrying a defensive copy of path.
func WithDebugCallPath(ctx context.Context, path []DebugCallFrame) context.Context {
	return context.WithValue(ctx, debugCallPathContextKey{}, cloneDebugCallPath(path))
}

// DebugCallPathFromContext returns a defensive copy of the nested invocation path.
func DebugCallPathFromContext(ctx context.Context) []DebugCallFrame {
	path, _ := ctx.Value(debugCallPathContextKey{}).([]DebugCallFrame)
	return cloneDebugCallPath(path)
}

// WithDebugController carries the current run's controller into nested
// sub-engine construction without making it process- or engine-global.
func WithDebugController(ctx context.Context, controller DebugController) context.Context {
	if controller == nil {
		return ctx
	}
	return context.WithValue(ctx, debugControllerContextKey{}, controller)
}

// DebugControllerFromContext returns the explicitly enabled run debugger.
func DebugControllerFromContext(ctx context.Context) DebugController {
	controller, _ := ctx.Value(debugControllerContextKey{}).(DebugController)
	return controller
}

func WithDebugInvocationTracker(ctx context.Context, tracker *DebugInvocationTracker) context.Context {
	if tracker == nil {
		return ctx
	}
	return context.WithValue(ctx, debugInvocationTrackerContextKey{}, tracker)
}

func DebugInvocationTrackerFromContext(ctx context.Context) *DebugInvocationTracker {
	tracker, _ := ctx.Value(debugInvocationTrackerContextKey{}).(*DebugInvocationTracker)
	return tracker
}

// ExtendDebugProtection merges one runbook scope's secret declarations and
// governance redactions into inherited run-scoped debugger protection.
func ExtendDebugProtection(protection DebugProtection, vars map[string]any, inputs map[string]*schema.Input, policy governance.GovernancePolicy) DebugProtection {
	additional := DebugProtection{}
	for name, declaration := range inputs {
		if declaration == nil || declaration.Type != "secret" {
			continue
		}
		additional.ProtectedVars = append(additional.ProtectedVars, name)
		if secret, ok := vars[name].(string); ok && secret != "" {
			additional.SecretValues = append(additional.SecretValues, secret)
		}
	}
	if policy != nil {
		additional.RedactionPatterns = policy.RedactionPatterns()
	}
	return MergeDebugProtection(protection, additional)
}

// MergeDebugProtection returns the union of two protection sets without
// retaining duplicate variable names, values, or redaction rules.
func MergeDebugProtection(base, additional DebugProtection) DebugProtection {
	protected := make(map[string]bool, len(base.ProtectedVars)+len(additional.ProtectedVars))
	for _, name := range append(append([]string(nil), base.ProtectedVars...), additional.ProtectedVars...) {
		if name != "" {
			protected[name] = true
		}
	}
	protectedNames := make([]string, 0, len(protected))
	for name := range protected {
		protectedNames = append(protectedNames, name)
	}
	sort.Strings(protectedNames)

	patterns := make([]*governance.RedactionPattern, 0, len(base.RedactionPatterns)+len(additional.RedactionPatterns))
	seenPatterns := make(map[string]bool, cap(patterns))
	for _, pattern := range append(append([]*governance.RedactionPattern(nil), base.RedactionPatterns...), additional.RedactionPatterns...) {
		if pattern == nil {
			continue
		}
		key := pattern.Pattern + "\x00" + pattern.Replacement
		if seenPatterns[key] {
			continue
		}
		seenPatterns[key] = true
		copied := *pattern
		patterns = append(patterns, &copied)
	}
	return DebugProtection{
		ProtectedVars:     protectedNames,
		SecretValues:      uniqueDebugProtectionStrings(append(append([]string(nil), base.SecretValues...), additional.SecretValues...)),
		RedactionPatterns: patterns,
	}
}

func uniqueDebugProtectionStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

// DebugNodeID returns the stable graph/runtime identity for one invocation
// path. Top-level steps retain their existing raw IDs.
func DebugNodeID(path []DebugCallFrame, stepID string) string {
	if len(path) == 0 {
		return stepID
	}
	parts := make([]string, 0, len(path)+1)
	for _, frame := range path {
		parts = append(parts, escapeDebugNodeIDPart(frame.StepID))
	}
	parts = append(parts, escapeDebugNodeIDPart(stepID))
	return strings.Join(parts, "/")
}

func escapeDebugNodeIDPart(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

func cloneDebugCallPath(path []DebugCallFrame) []DebugCallFrame {
	if len(path) == 0 {
		return nil
	}
	out := make([]DebugCallFrame, len(path))
	copy(out, path)
	return out
}
