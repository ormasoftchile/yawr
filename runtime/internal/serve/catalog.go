package serve

import (
	"context"
	"os"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func (s *Server) contextWithPerRunCatalog(ctx context.Context, plan *engine.ExecutionPlan, parsed *parser.ParsedRunbook, runbookPath string) (context.Context, []parser.ParseWarning, error) {
	if parsed == nil || parsed.Runbook == nil {
		return ctx, nil, nil
	}
	workspaceRoot := s.cfg.WorkspaceRoot
	if workspaceRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ctx, nil, err
		}
		workspaceRoot = wd
	}

	cat, catErrs := adapter.BuildPackageCatalog(adapter.PackageCatalogOptions{
		WorkspaceRoot:    workspaceRoot,
		Builtins:         internaltool.NewBuiltinRegistry().All(),
		ProjectRequires:  s.cfg.ProjectRequires,
		ProjectToolPaths: s.cfg.ProjectToolPaths,
	}, runbookPath, parsed.Runbook.Requires)
	fatal, warn := errkit.SplitWarnings(catErrs)
	if len(fatal) > 0 {
		return ctx, nil, fatal[0]
	}

	warnings := make([]parser.ParseWarning, 0, len(warn))
	for _, w := range warn {
		warnings = append(warnings, parser.ParseWarning{Field: "requires", Message: w.Error()})
	}

	ctx = internalexecutor.WithRunResolver(ctx, adapter.NewCatalogIncludeResolver(cat, s.parser))
	ctx = internalexecutor.WithRunPinRecorder(ctx, internalexecutor.PinRecorder(func(pin internalexecutor.DynamicIncludePin) {
		if plan == nil {
			return
		}
		plan.Metadata.DynamicIncludes = append(plan.Metadata.DynamicIncludes, schema.LockedDynamicInclude{
			StepID:             pin.StepID,
			QualifiedNodeID:    pin.QualifiedNodeID,
			Invocation:         pin.Invocation,
			Revision:           pin.Revision,
			StructuralPath:     append([]schema.DynamicIncludeFrameIdentity(nil), pin.StructuralPath...),
			RenderedRef:        pin.RenderedRef,
			QualifiedID:        pin.QualifiedID,
			RunbookID:          pin.RunbookID,
			RunbookName:        pin.RunbookName,
			RunbookContentHash: pin.ContentHash,
			AbsPath:            pin.AbsPath,
			PackageName:        pin.PackageName,
			PackageVersion:     pin.PackageVersion,
			FileDigest:         pin.FileDigest,
			PackageDigest:      pin.PackageDigest,
			ExecutableClosure:  pin.ExecutableClosure,
			ResolvedInputs:     pin.ResolvedInputs,
			ResolvedBindings:   pin.ResolvedBindings,
			ResolvedOutputs:    pin.ResolvedOutputs,
			ResolvedGovernance: pin.ResolvedGovernance,
		})
	}))
	return ctx, warnings, nil
}
