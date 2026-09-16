package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func prepareScopedCLI(ctx context.Context, parser parserpkg.Parser, path, packageMap string, profile *schema.RuntimeProfile) (*adapter.PreparedScopedRun, []packageBindingOrigin, error) {
	workspace, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	entrypoint, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	project, origins, err := pkgcatalog.ReadProjectBindings(nil, workspace, packageMap)
	if err != nil {
		return nil, nil, err
	}
	prepared, err := adapter.PrepareScopedRun(ctx, adapter.ScopedRunOptions{
		Catalog: pkgcatalog.BuildOptions{WorkspaceRoot: workspace, Builtins: internaltool.NewBuiltinRegistry().All(),
			ProjectRequires: project.Requires, ProjectToolPaths: append(append([]string(nil), project.ToolPaths...), ".")},
		Entrypoint: entrypoint, Parser: parser, Profile: profile,
	})
	return prepared, origins, err
}
