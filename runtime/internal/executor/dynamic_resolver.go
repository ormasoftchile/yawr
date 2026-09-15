package executor

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	govpkg "github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// DynamicIncludeResult carries everything the IncludeExecutor needs after a
// successful dynamic include resolution.
type DynamicIncludeResult struct {
	TargetScopeID   string
	Flow            []schema.FlowNode
	QualifiedID     string
	RunbookID       string
	RunbookName     string
	ContentHash     string
	AbsPath         string
	PackageName     string
	PackageVersion  string
	FileDigest      string
	PackageDigest   string
	ChildInputs     map[string]*schema.Input
	ChildBindings   []schema.Binding
	ChildOutputs    map[string]*schema.Output
	ChildGovernance *schema.GovernanceConfig
}

// DynamicIncludeResolver resolves a rendered runbook_ref against the frozen
// catalog and returns a parsed, validated DynamicIncludeResult.
type DynamicIncludeResolver interface {
	Resolve(ctx context.Context, renderedRef string) (*DynamicIncludeResult, error)
}

// LazyRunbookSnapshotLoader parses already captured runbook bytes. Durable
// starts use this instead of reopening a path after its content was bound.
type LazyRunbookSnapshotLoader interface {
	LoadSnapshot(ctx context.Context, absPath string, source []byte) (*LoadedRunbook, error)
}

// LazyIncludeMaterializer replaces deferred static include paths with their
// complete executable closure before a durable plan snapshot is written.
type LazyIncludeMaterializer interface {
	MaterializeLazyIncludes(ctx context.Context, plan *engine.ExecutionPlan) error
}

func MaterializeLazyIncludes(ctx context.Context, registry engine.ExecutorRegistry, plan *engine.ExecutionPlan) error {
	if registry == nil {
		return nil
	}
	if materializer, ok := registry.(LazyIncludeMaterializer); ok {
		return materializer.MaterializeLazyIncludes(ctx, plan)
	}
	includeExecutor := registry.Lookup("include")
	if materializer, ok := includeExecutor.(LazyIncludeMaterializer); ok {
		return materializer.MaterializeLazyIncludes(ctx, plan)
	}
	return nil
}

// DynamicIncludePin records one resolved dynamic include site for
// replay/resume determinism. The replay layer maps this to
// schema.LockedDynamicInclude in PlanMetadata.DynamicIncludes.
type DynamicIncludePin struct {
	SchemaVersion      string
	TargetScopeID      string
	StepID             string
	QualifiedNodeID    string
	Invocation         int
	Revision           int64
	StructuralPath     []schema.DynamicIncludeFrameIdentity
	RenderedRef        string
	QualifiedID        string
	RunbookID          string
	RunbookName        string
	ContentHash        string
	AbsPath            string
	PackageName        string
	PackageVersion     string
	FileDigest         string
	PackageDigest      string
	ExecutableClosure  json.RawMessage
	ResolvedInputs     map[string]*schema.Input
	ResolvedBindings   []schema.Binding
	ResolvedOutputs    map[string]*schema.Output
	ResolvedGovernance *schema.GovernanceConfig
}

// PinRecorder is called after each successful dynamic include resolution.
// Nil is safe: pinning is skipped.
type PinRecorder func(DynamicIncludePin)

// includeChainKey is the context key for the runtime include chain (ordered
// list of qualified IDs on the current call stack, used for cycle + depth
// detection).
type includeChainKey struct{}

func withIncludeChain(ctx context.Context, chain []string) context.Context {
	return context.WithValue(ctx, includeChainKey{}, chain)
}

func includeChainFromCtx(ctx context.Context) []string {
	chain, _ := ctx.Value(includeChainKey{}).([]string)
	return chain
}

// dynGovKey is the context key for the composed effective governance
// accumulated along the dynamic include call stack.
type dynGovKey struct{}

func withDynGov(ctx context.Context, gov *tracepkg.EffectiveGovernancePayload) context.Context {
	return context.WithValue(ctx, dynGovKey{}, gov)
}

func dynGovFromCtx(ctx context.Context) *tracepkg.EffectiveGovernancePayload {
	gov, _ := ctx.Value(dynGovKey{}).(*tracepkg.EffectiveGovernancePayload)
	return gov
}

// govPolicyKey is the context key for the compiled GovernancePolicy that
// constrains child steps within a dynamic include scope.
type govPolicyKey struct{}

func withGovPolicy(ctx context.Context, pol govpkg.GovernancePolicy) context.Context {
	return context.WithValue(ctx, govPolicyKey{}, pol)
}

func govPolicyFromCtx(ctx context.Context) govpkg.GovernancePolicy {
	pol, _ := ctx.Value(govPolicyKey{}).(govpkg.GovernancePolicy)
	return pol
}

// effectiveGovToSchemaConfig projects an EffectiveGovernancePayload into a
// *schema.GovernanceConfig so internal/governance.BuildPolicy can compile it.
func effectiveGovToSchemaConfig(eg *tracepkg.EffectiveGovernancePayload) *schema.GovernanceConfig {
	if eg == nil {
		return nil
	}
	cfg := &schema.GovernanceConfig{
		RequireApproval: eg.RequireApproval,
		AllowCommands:   eg.AllowCommands,
		DenyCommands:    eg.DenyCommands,
		DenyEnvVars:     eg.DenyEnvVars,
	}
	for _, pat := range eg.Redact {
		cfg.Redact = append(cfg.Redact, schema.RedactRule{Pattern: pat})
	}
	return cfg
}

// ComposeGovernance returns the effective governance for a dynamic include
// child, applying monotonic restriction against the parent,
// rulings B-8 and B-9). The result is the governance that actually applies
// inside the child runbook's execution scope.
//
// Non-widening invariant: the child can tighten restrictions but never relax
// what the parent forbade. Specifically:
//   - require_approval: OR (either side can mandate approval)
//   - deny_commands, deny_env_vars: set-union (either side can forbid)
//   - allow_commands, capabilities: intersection (nil = unrestricted)
//   - redact: set-union of patterns
func ComposeGovernance(parent *tracepkg.EffectiveGovernancePayload, child *schema.GovernanceConfig) *tracepkg.EffectiveGovernancePayload {
	out := &tracepkg.EffectiveGovernancePayload{}

	// RequireApproval: OR
	if parent != nil && parent.RequireApproval {
		out.RequireApproval = true
	}
	if child != nil && child.RequireApproval {
		out.RequireApproval = true
	}

	// DenyCommands: set-union (B-8: deduplicated, sorted)
	var pDeny, cDeny []string
	if parent != nil {
		pDeny = parent.DenyCommands
	}
	if child != nil {
		cDeny = child.DenyCommands
	}
	out.DenyCommands = sortedUnion(pDeny, cDeny)

	// DenyEnvVars: set-union
	var pDenyEnv, cDenyEnv []string
	if parent != nil {
		pDenyEnv = parent.DenyEnvVars
	}
	if child != nil {
		cDenyEnv = child.DenyEnvVars
	}
	out.DenyEnvVars = sortedUnion(pDenyEnv, cDenyEnv)

	// AllowCommands: intersection (B-9; nil = no ceiling imposed)
	var pAllow, cAllow []string
	if parent != nil {
		pAllow = parent.AllowCommands
	}
	if child != nil {
		cAllow = child.AllowCommands
	}
	out.AllowCommands = intersectOrInherit(pAllow, cAllow)

	// Redact: set-union of patterns
	var pRedact []string
	if parent != nil {
		pRedact = parent.Redact
	}
	var cRedact []string
	if child != nil {
		for _, r := range child.Redact {
			cRedact = append(cRedact, r.Pattern)
		}
	}
	out.Redact = sortedUnion(pRedact, cRedact)

	// Capabilities: intersection (same semantics as AllowCommands; GovernanceConfig
	// has no capabilities field so child capabilities are always nil, meaning the
	// child cannot widen the parent's capability ceiling).
	var pCap []string
	if parent != nil {
		pCap = parent.Capabilities
	}
	out.Capabilities = intersectOrInherit(pCap, nil)

	return out
}

// sortedUnion returns the sorted, deduplicated union of a and b, or nil if
// both are empty.
func sortedUnion(a, b []string) []string {
	m := map[string]struct{}{}
	for _, s := range a {
		m[s] = struct{}{}
	}
	for _, s := range b {
		m[s] = struct{}{}
	}
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// intersectOrInherit implements B-9 allow_commands / capabilities semantics.
// nil = unrestricted (no ceiling imposed by this side).
//
//   - (nil, nil) → nil
//   - (nil, child) → child (child imposes a ceiling on unrestricted parent)
//   - (parent, nil) → parent (child inherits parent's ceiling)
//   - (parent, child) → intersection
func intersectOrInherit(parent, child []string) []string {
	if parent == nil && child == nil {
		return nil
	}
	if parent == nil {
		cp := make([]string, len(child))
		copy(cp, child)
		sort.Strings(cp)
		return cp
	}
	if child == nil {
		cp := make([]string, len(parent))
		copy(cp, parent)
		sort.Strings(cp)
		return cp
	}
	childSet := map[string]struct{}{}
	for _, s := range child {
		childSet[s] = struct{}{}
	}
	var out []string
	for _, s := range parent {
		if _, ok := childSet[s]; ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
