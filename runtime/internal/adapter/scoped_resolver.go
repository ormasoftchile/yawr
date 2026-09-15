package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

// FrozenScopedIncludeResolver resolves only executable candidates captured in
// the current execution context's immutable scope set. It holds no run state
// and can be shared across sessions, concurrent runs, and restored plans.
type FrozenScopedIncludeResolver struct{}

func NewFrozenScopedIncludeResolver() *FrozenScopedIncludeResolver {
	return &FrozenScopedIncludeResolver{}
}

func (resolver *FrozenScopedIncludeResolver) Resolve(ctx context.Context, ref string) (*executor.DynamicIncludeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reason, kind := pkgcatalog.ValidateRenderedRef(ref)
	switch kind {
	case pkgcatalog.RefValidationEmpty:
		return nil, errkit.New("DINC-002", "dynamic include: empty rendered ref")
	case pkgcatalog.RefValidationInvalid:
		return nil, errkit.New("DINC-001", fmt.Sprintf("dynamic include: invalid ref %q: %s", ref, reason))
	}
	scopes := engine.ToolScopesFromContext(ctx)
	if scopes == nil {
		return nil, errkit.New("SCOPE-002", "dynamic include requires immutable scopes in the execution context")
	}
	targets := scopes.Export().DynamicTargets
	qualified := ref
	if !strings.Contains(ref, "/") {
		var matches []string
		for candidate := range targets {
			if _, bare, ok := strings.Cut(candidate, "/"); ok && bare == ref {
				matches = append(matches, candidate)
			}
		}
		sort.Strings(matches)
		switch len(matches) {
		case 0:
			return nil, errkit.New("DINC-002", fmt.Sprintf("dynamic include: %q not captured", ref))
		case 1:
			qualified = matches[0]
		default:
			return nil, errkit.New("DINC-003", fmt.Sprintf("dynamic include: %q is ambiguous (candidates: %v)", ref, matches))
		}
	}
	_, ok := targets[qualified]
	if !ok {
		return nil, errkit.New("DINC-002", fmt.Sprintf("dynamic include: %q not captured", ref))
	}
	target, flow, err := plansnapshot.RestoreScopedDynamicTarget(scopes, qualified)
	if err != nil {
		return nil, errkit.Wrap("DINC-013", "dynamic include: invalid captured executable", err)
	}
	return &executor.DynamicIncludeResult{
		TargetScopeID: target.ScopeID,
		Flow:          flow, QualifiedID: qualified, RunbookID: target.RunbookID, RunbookName: target.RunbookName,
		ContentHash: target.ContentHash, AbsPath: target.AbsPath, PackageName: target.PackageName,
		PackageVersion: target.PackageVersion, PackageDigest: target.PackageDigest, FileDigest: target.SourceDigest,
		ChildInputs: target.Inputs, ChildBindings: target.Bindings, ChildOutputs: target.Outputs, ChildGovernance: target.Governance,
	}, nil
}

func captureScopedTargets(ctx context.Context, loader *ScopedRunbookLoader, snapshot *toolscope.Snapshot) error {
	for qualified, discoveryID := range loader.closure.Targets {
		entry, ok := loader.closure.Catalog.RunbookByQualified(qualified)
		scopeID := loader.discoveryScopes[discoveryID]
		document := loader.documents[scopeID]
		if !ok || document == nil || entry.FileDigest != document.Digest {
			return fmt.Errorf("scoped preparation: dynamic target %q lacks captured catalog metadata", qualified)
		}
		child, err := loader.LoadScope(ctx, document.Path, scopeID)
		if err != nil {
			return err
		}
		body, err := plansnapshot.EncodeScopedInvocationFlowClosure(child.Runbook.Flow,
			schema.InvocationForRunbook(child.Runbook.Bindings, child.Runbook.Outputs, child.Runbook.Flow), loader.scopes, scopeID)
		if err != nil {
			return err
		}
		var header struct {
			ClosureDigest string `json:"closure_digest"`
		}
		if err := json.Unmarshal(body, &header); err != nil {
			return err
		}
		hash, err := graphdoc.RunbookContentHash(child.Runbook)
		if err != nil {
			return err
		}
		snapshot.DynamicTargets[qualified] = toolscope.Target{
			SchemaVersion: toolscope.TargetVersion,
			DocumentID:    snapshot.Scopes[scopeID].DocumentID, ScopeID: scopeID, SourceDigest: document.Digest,
			ClosureDigest: header.ClosureDigest, ExecutableClosure: body,
			RunbookID: child.Runbook.ID, RunbookName: child.Runbook.Name, ContentHash: hash, AbsPath: document.Path,
			PackageName: entry.PackageName, PackageVersion: entry.PackageVersion, PackageDigest: entry.PackageDigest,
			Inputs: child.Runbook.Inputs, Bindings: child.Runbook.Bindings, Outputs: child.Runbook.Outputs, Governance: child.Runbook.Governance,
		}
	}
	return nil
}

var _ executor.DynamicIncludeResolver = (*FrozenScopedIncludeResolver)(nil)
