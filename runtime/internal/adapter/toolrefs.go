package adapter

import (
	"fmt"
	"path/filepath"

	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// ResolveToolRefs loads tool definitions referenced in a runbook's toolRefs field.
// Returns a slice of ToolDef to register into the tool registry.
func ResolveToolRefs(runbookPath string, refs []*schema.ToolRef) ([]toolpkg.ToolDef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	runbookDir := filepath.Dir(runbookPath)
	defs := make([]toolpkg.ToolDef, 0, len(refs))
	for _, ref := range refs {
		if ref.Path == "" {
			continue // skip refs without path (might be builtin or remote)
		}
		// Resolve relative path
		absPath := filepath.Join(runbookDir, ref.Path)
		schemaDef, err := internaltool.ParseToolFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("resolve tool %s at %s: %w", ref.Name, ref.Path, err)
		}
		runtimeDef, err := internaltool.RuntimeToolDef(schemaDef)
		if err != nil {
			return nil, fmt.Errorf("convert tool %s: %w", ref.Name, err)
		}
		defs = append(defs, runtimeDef)
	}
	return defs, nil
}

// PackageCatalogOptions supplies the workspace-level inputs needed to build
// a package catalog (pkgcatalog.Build's tiers 0-3) once per run/workspace,
// independent of any single runbook file. Callers (e.g. the CLI, or an
// eventual pkg/run wiring) construct this once from .yawr/config.yaml and
// pass it into ResolveToolRefsViaCatalog for every runbook file that needs
// binding, so the catalog freeze happens exactly once per run
// (design/yawr/sections/06-tool-runtime.tex §Tool Discovery: "A runtime
// MUST NOT mutate the catalog after Phase C completes.").
type PackageCatalogOptions struct {
	WorkspaceRoot    string
	Builtins         []toolpkg.ToolDef
	ProjectRequires  []*schema.PackageRequirement
	ProjectToolPaths []string
	Dynamic          []*pkgcatalog.Entry
}

// BuildPackageCatalog runs Phase C (pkgcatalog.Build) once for a workspace.
// The returned *pkgcatalog.Catalog is immutable and safe to reuse across
// every runbook file bound via ResolveToolRefsViaCatalog in the same run.
func BuildPackageCatalog(opts PackageCatalogOptions, runbookPath string, runbookRequires []*schema.PackageRequirement) (*pkgcatalog.Catalog, []error) {
	return pkgcatalog.Build(pkgcatalog.BuildOptions{
		WorkspaceRoot:    opts.WorkspaceRoot,
		Builtins:         opts.Builtins,
		ProjectRequires:  opts.ProjectRequires,
		RunbookRequires:  runbookRequires,
		RunbookPath:      runbookPath,
		ProjectToolPaths: opts.ProjectToolPaths,
		Dynamic:          opts.Dynamic,
	})
}

// ResolveToolRefsViaCatalog binds one runbook file's toolRefs entries
// against an already-frozen package catalog (see BuildPackageCatalog),
// exposing the YAWR Tool Packages MVP's package:/version:-aware binding
// (pkgcatalog.BindFile) through the same ToolDef-registration shape as the
// legacy path-only ResolveToolRefs, so callers (CLI wiring, pkg/run) can
// switch between the two without changing how results are consumed.
//
// Unlike ResolveToolRefs, this path supports bare-name resolution against
// the frozen catalog (tiers 0-3) in addition to toolRefs[].path (tier 4)
// and toolRefs[].package pins, and returns PKG-* typed errors
// (pkg/errkit) for every collision/ambiguity/constraint violation defined
// in the ratified spec.
func ResolveToolRefsViaCatalog(cat *pkgcatalog.Catalog, runbookPath string, refs []*schema.ToolRef) ([]toolpkg.ToolDef, []error) {
	if len(refs) == 0 {
		return nil, nil
	}
	bindings, errs := pkgcatalog.BindFile(cat, runbookPath, refs, "")
	fatal, warnings := errkit.SplitWarnings(errs)
	// Advisory catalog warnings must not prevent otherwise-successful
	// bindings from being registered. Only fatal errors abort resolution.
	if len(fatal) > 0 {
		return nil, fatal
	}
	defs := make([]toolpkg.ToolDef, 0, len(bindings))
	for _, b := range bindings {
		defs = append(defs, b.Def)
	}
	return defs, warnings
}
