package main

import (
	"context"
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func scopedServePreparation(p parser.Parser, workspace string, requirements []*schema.PackageRequirement, paths []string, policy expand.Policy) func(context.Context, string) (*engine.ExecutionPlan, *parser.ParsedRunbook, error) {
	return func(ctx context.Context, path string) (*engine.ExecutionPlan, *parser.ParsedRunbook, error) {
		entrypoint, err := filepath.Abs(path)
		if err != nil {
			return nil, nil, err
		}
		prepared, err := adapter.PrepareScopedRun(ctx, adapter.ScopedRunOptions{
			Catalog: pkgcatalog.BuildOptions{WorkspaceRoot: workspace, Builtins: internaltool.NewBuiltinRegistry().All(),
				ProjectRequires: requirements, ProjectToolPaths: paths},
			Entrypoint: entrypoint, Parser: p,
		})
		if err != nil {
			return nil, nil, err
		}
		for _, warning := range prepared.Warnings {
			prepared.Root.Warnings = append(prepared.Root.Warnings, parser.ParseWarning{Field: "dependencies", Message: warning.Error()})
		}
		plan, err := prepared.Plan(ctx, policy)
		return plan, prepared.Root, err
	}
}
