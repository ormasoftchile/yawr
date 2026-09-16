package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

// ScopedRunbookLoader owns the captured sources and their explicit lexical
// owners. Load rejects paths with multiple owners; LoadScope selects an owner
// without mutable ambient scope or filesystem rebinding.
type ScopedRunbookLoader struct {
	closure         *pkgcatalog.DependencyClosure
	scopes          *toolscope.Set
	documents       map[string]*pkgcatalog.DependencyDocument
	owners          map[string][]string
	discoveryScopes map[string]string
	pathAliases     map[string]string
}

func scopedPathKey(path string) string {
	path = filepath.Clean(path)
	if os.PathSeparator == '\\' {
		path = strings.ToLower(path)
	}
	return path
}

func (loader *ScopedRunbookLoader) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	identity := loader.capturedPath(path)
	parsed, err := loader.closure.Load(ctx, identity)
	if err != nil {
		return nil, err
	}
	owners := loader.owners[scopedPathKey(identity)]
	if len(owners) != 1 {
		return nil, fmt.Errorf("scoped loader: %q has %d owners; explicit scope required", path, len(owners))
	}
	parsed.Source, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return loader.loadParsed(ctx, parsed, owners[0])
}

func (loader *ScopedRunbookLoader) LoadScope(ctx context.Context, path, scopeID string) (*parser.ParsedRunbook, error) {
	if loader.documents[scopeID] == nil {
		return nil, fmt.Errorf("scoped loader: unknown scope %q", scopeID)
	}
	identity := loader.capturedPath(path)
	parsed, err := loader.closure.Load(ctx, identity)
	if err != nil {
		return nil, err
	}
	if scopedPathKey(identity) != scopedPathKey(loader.documents[scopeID].Path) {
		return nil, fmt.Errorf("scoped loader: path %q does not belong to scope %q", path, scopeID)
	}
	parsed.Source, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return loader.loadParsed(ctx, parsed, scopeID)
}

// Resolve only aliases captured before preflight. Parsed Source remains the
// requested display path; it is never used as the document ownership key.
func (loader *ScopedRunbookLoader) capturedPath(path string) string {
	key := scopedPathKey(path)
	if canonical, ok := loader.pathAliases[key]; ok {
		return canonical
	}
	for parent := filepath.Dir(key); ; parent = filepath.Dir(parent) {
		if canonical, ok := loader.pathAliases[parent]; ok {
			relative, err := filepath.Rel(parent, key)
			if err == nil {
				return filepath.Join(canonical, relative)
			}
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	return path
}

func (loader *ScopedRunbookLoader) loadParsed(ctx context.Context, parsed *parser.ParsedRunbook, scopeID string) (*parser.ParsedRunbook, error) {
	schema.CaptureIncludeAliases(parsed.Runbook)
	parsed.Runbook.LexicalScopeID = scopeID
	if err := loader.annotate(ctx, parsed.Runbook.Flow, scopeID); err != nil {
		return nil, err
	}
	return parsed, nil
}

func (loader *ScopedRunbookLoader) annotate(ctx context.Context, nodes []schema.FlowNode, scopeID string) error {
	for i := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		node := &nodes[i]
		if step := node.Step; step != nil {
			step.LexicalScopeID = scopeID
			if step.ToolCall != nil {
				bound, err := loader.scopes.Resolve(scopeID, step.ToolCall.Tool.Name, step.ToolCall.Tool.Action)
				if err != nil {
					return err
				}
				step.ToolBindingID = bound.BindingID
			}
			if include := step.IncludeSpec; include != nil && !include.Include.IsDynamic() {
				var targetScope string
				for _, edge := range loader.documents[scopeID].Includes {
					if edge.StepID == step.ID {
						if targetScope != "" {
							return fmt.Errorf("scoped loader: ambiguous include edge %q", step.ID)
						}
						targetScope = loader.discoveryScopes[edge.Target]
					}
				}
				target := loader.documents[targetScope]
				if target == nil {
					return fmt.Errorf("scoped loader: missing include edge %q", step.ID)
				}
				child, err := loader.LoadScope(ctx, target.Path, targetScope)
				if err != nil {
					return err
				}
				hash, err := graphdoc.RunbookContentHash(child.Runbook)
				if err != nil {
					return err
				}
				include.TargetScopeID = targetScope
				include.LazyRunbookPath = ""
				include.LazyRunbookDigest = target.Digest
				include.ResolvedRunbookPath = target.Path
				include.ResolvedSteps = child.Runbook.Flow
				include.ResolvedRunbookID, include.ResolvedRunbookName = child.Runbook.ID, child.Runbook.Name
				include.ResolvedRunbookContentHash = hash
				include.ResolvedInputs, include.ResolvedBindings = child.Runbook.Inputs, child.Runbook.Bindings
				include.ResolvedOutputs, include.ResolvedGovernance = child.Runbook.Outputs, child.Runbook.Governance
			}
			if step.BranchSpec != nil {
				for _, branch := range step.BranchSpec.Branches {
					if err := loader.annotate(ctx, branch.Steps, scopeID); err != nil {
						return err
					}
				}
			}
			if step.CompensateSpec != nil {
				if err := loader.annotate(ctx, step.CompensateSpec.Compensate.Steps, scopeID); err != nil {
					return err
				}
			}
		}
		if node.Iterate != nil {
			if err := loader.annotate(ctx, node.Iterate.Steps, scopeID); err != nil {
				return err
			}
		}
		if node.Parallel != nil {
			for _, branch := range node.Parallel.Branches {
				if err := loader.annotate(ctx, branch.Steps, scopeID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ScopedLazyLoader exposes the captured parser loader to the executor without
// changing the planner's Load return type.
type ScopedLazyLoader struct{ loader *ScopedRunbookLoader }

func (loader *ScopedLazyLoader) Load(ctx context.Context, path string) (*executor.LoadedRunbook, error) {
	parsed, err := loader.loader.Load(ctx, path)
	if err != nil {
		return nil, err
	}
	return loader.loaded(parsed)
}

func (loader *ScopedLazyLoader) LoadScoped(ctx context.Context, path, scopeID string) (*executor.LoadedRunbook, error) {
	parsed, err := loader.loader.LoadScope(ctx, path, scopeID)
	if err != nil {
		return nil, err
	}
	return loader.loaded(parsed)
}

func (loader *ScopedLazyLoader) loaded(parsed *parser.ParsedRunbook) (*executor.LoadedRunbook, error) {
	runbook := parsed.Runbook
	hash, err := graphdoc.RunbookContentHash(runbook)
	if err != nil {
		return nil, err
	}
	document := loader.loader.documents[runbook.LexicalScopeID]
	return &executor.LoadedRunbook{ID: runbook.ID, Name: runbook.Name, ContentHash: hash, Flow: runbook.Flow,
		Inputs: runbook.Inputs, Bindings: runbook.Bindings, Outputs: runbook.Outputs, Governance: runbook.Governance,
		RootScopeID: runbook.LexicalScopeID, SourceDigest: document.Digest}, nil
}

var _ executor.LazyRunbookLoader = (*ScopedLazyLoader)(nil)
var _ executor.ScopedLazyRunbookLoader = (*ScopedLazyLoader)(nil)
