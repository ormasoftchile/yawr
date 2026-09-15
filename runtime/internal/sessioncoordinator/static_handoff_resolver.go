package sessioncoordinator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
)

// StaticHandoffResolverConfig supplies the same parser, catalog/profile-aware
// planner, and include loader used by the initiating runtime.
type StaticHandoffResolverConfig struct {
	Prepare         func(context.Context, string) (*engine.ExecutionPlan, error)
	Parser          parser.Parser
	Planner         planner.Planner
	Loader          flowwalk.Loader
	MaxIncludeDepth int
}

type staticHandoffResolver struct {
	prepare         func(context.Context, string) (*engine.ExecutionPlan, error)
	parser          parser.Parser
	planner         planner.Planner
	loader          flowwalk.Loader
	maxIncludeDepth int
}

// NewStaticHandoffResolver constructs the production filesystem resolver for
// V1 static handoff targets.
func NewStaticHandoffResolver(config StaticHandoffResolverConfig) (HandoffResolver, error) {
	if config.Prepare == nil && (config.Parser == nil || config.Planner == nil || config.Loader == nil) {
		return nil, errors.New("session coordinator: static handoff resolver requires parser, planner, and loader")
	}
	return &staticHandoffResolver{
		prepare: config.Prepare,
		parser:  config.Parser, planner: config.Planner, loader: config.Loader,
		maxIncludeDepth: config.MaxIncludeDepth,
	}, nil
}

func (resolver *staticHandoffResolver) ResolveHandoff(
	ctx context.Context,
	request HandoffResolveRequest,
) (HandoffTarget, error) {
	if request.SourcePlan == nil {
		return HandoffTarget{}, errors.New("session coordinator: handoff source plan is required")
	}
	declaringPath := handoffDeclaringRunbookPath(request.SourcePlan.RunbookPath, request.Handoff.CallPath)
	targetPath, err := staticHandoffPath(declaringPath, request.Handoff.TargetRunbook)
	if err != nil {
		return HandoffTarget{}, err
	}
	if err := validateStaticHandoffContainment(declaringPath, targetPath); err != nil {
		return HandoffTarget{}, err
	}
	if resolver.prepare != nil {
		plan, err := resolver.prepare(ctx, targetPath)
		if err != nil {
			return HandoffTarget{}, err
		}
		if plan == nil || plan.Validation == nil || plan.ToolScopes == nil || filepath.Clean(plan.RunbookPath) != targetPath {
			return HandoffTarget{}, errors.New("session coordinator: preparation returned an invalid scoped handoff target")
		}
		graph, err := BuildExecutionPlanGraph(plan)
		if err != nil {
			return HandoffTarget{}, err
		}
		return HandoffTarget{Plan: plan, Graph: graph}, nil
	}
	if request.SourcePlan.ToolScopes != nil {
		return HandoffTarget{}, errors.New("session coordinator: scoped handoff requires frozen dependency preparation")
	}
	parsed, err := resolver.parser.Parse(ctx, targetPath)
	if err != nil {
		return HandoffTarget{}, fmt.Errorf("session coordinator: parse handoff target: %w", err)
	}
	if parsed == nil || parsed.Runbook == nil || filepath.Clean(parsed.Source) != targetPath {
		return HandoffTarget{}, errors.New("session coordinator: parser returned a different handoff target")
	}
	plan, err := resolver.planner.Plan(ctx, parsed)
	if err != nil {
		return HandoffTarget{}, fmt.Errorf("session coordinator: plan handoff target: %w", err)
	}
	if plan == nil || plan.Validation == nil || filepath.Clean(plan.RunbookPath) != targetPath {
		return HandoffTarget{}, errors.New("session coordinator: planner returned an invalid handoff target")
	}
	document, err := (&graphdoc.Builder{
		Loader: resolver.loader, Recurse: true, MaxDepth: resolver.maxIncludeDepth,
	}).Build(ctx, parsed)
	if err != nil {
		return HandoffTarget{}, fmt.Errorf("session coordinator: build handoff target GraphJSON: %w", err)
	}
	plan.Metadata.GraphContentHash = document.Hash
	encodedGraph, err := graphjson.Render(document)
	if err != nil {
		return HandoffTarget{}, fmt.Errorf("session coordinator: render handoff target GraphJSON: %w", err)
	}
	return HandoffTarget{Plan: plan, Graph: encodedGraph}, nil
}

func validateStaticHandoffContainment(sourcePath string, targetPath string) error {
	basePath, err := filepath.EvalSymlinks(filepath.Dir(sourcePath))
	if err != nil {
		return fmt.Errorf("session coordinator: resolve handoff base: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return fmt.Errorf("session coordinator: resolve handoff target: %w", err)
	}
	relative, err := filepath.Rel(basePath, realTarget)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("session coordinator: handoff target escapes the declaring directory")
	}
	return nil
}

var _ HandoffResolver = (*staticHandoffResolver)(nil)
