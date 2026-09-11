package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"gopkg.in/yaml.v3"
)

const projectConfigRelPath = ".yawr/config.yaml"

// loadProjectConfig loads workspaceRoot's .yawr/config.yaml, if present.
// A missing file is not an error: project-level package bindings
// (requires:, tool-paths:) are entirely optional. A present-but-malformed
// file (bad YAML, wrong apiVersion) is a hard error.
func loadProjectConfig(workspaceRoot string) (*schema.ProjectConfig, error) {
	canonical := filepath.Join(workspaceRoot, filepath.FromSlash(projectConfigRelPath))
	return loadConfigFile(canonical, true)
}

// loadPackageMap loads a --package-map file. It uses the identical
// Yawr config shape as .yawr/config.yaml: a requires: list of
// package bindings (plus optional tool-paths:), applied as CLI-supplied
// overrides over the project's own bindings — see mergePackageBindings.
// This lets the same, unchanged runbook resolve its toolRefs against the
// project's real package bindings by default, or against a --package-map
// file's mock/test bindings when one is supplied on the command line.
func loadPackageMap(path string) (*schema.ProjectConfig, error) {
	return loadConfigFile(path, false)
}

func loadConfigFile(path string, optional bool) (*schema.ProjectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return parseConfigFile(path, data)
}

func parseConfigFile(path string, data []byte) (*schema.ProjectConfig, error) {
	var cfg schema.ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: parse: %w", path, err)
	}
	if cfg.APIVersion != schema.ProjectConfigAPIVersion {
		return nil, fmt.Errorf("%s: apiVersion must be %q, got %q", path, schema.ProjectConfigAPIVersion, cfg.APIVersion)
	}
	return &cfg, nil
}

// packageBindingOrigin records where a package requirement's effective
// value came from, for --package-map override provenance.
type packageBindingOrigin = pkgcatalog.BindingOrigin

// mergePackageBindings merges a --package-map file's requires: entries
// (override) over the project's own requires: entries (project), per
// package name. An override entry fully replaces the project entry for
// that package name (CLI precedence); a project package not mentioned in
// override is preserved unchanged. Declaration order is project entries
// first (in their original order), then any package-map-only additions in
// their own declaration order — matching Build's existing
// project-before-runbook declaration-order contract
// (pkg/pkgcatalog.Build/mergeRequirements).
func mergePackageBindings(project, override []*schema.PackageRequirement) ([]*schema.PackageRequirement, []packageBindingOrigin) {
	return pkgcatalog.MergePackageBindings(project, override)
}

// mergeToolPaths appends package-map tool-paths after the project's own
// tool-paths, preserving declaration order within each source (project
// paths are still processed before <workspace>/tools/ per §Tool
// Discovery's tier-2 ordering; package-map paths are appended after the
// project's own, before the conventional tools/ directory).
func mergeToolPaths(project, override []string) []string {
	if len(override) == 0 {
		return project
	}
	out := make([]string, 0, len(project)+len(override))
	out = append(out, project...)
	out = append(out, override...)
	return out
}

// schemaToolDefFromRuntime converts a resolved, catalog-bound runtime
// toolpkg.ToolDef (produced by pkg/pkgcatalog.BindFile) back into a
// schema.ToolDef, so the planner's own schema-typed tool registry
// (plannerToolRegistry, keyed by "<name>/<action>") can validate
// package/bare-name-resolved toolRefs the same way it already validates
// path-resolved ones. This is the inverse of
// internal/tool.RuntimeToolDef; it is a pure, local data conversion and
// does not read any file or touch pkg/pkgcatalog's internals.
func schemaToolDefFromRuntime(def toolpkg.ToolDef) *schema.ToolDef {
	actions := make(map[string]*schema.ToolAction, len(def.Actions))
	for name, a := range def.Actions {
		if a == nil {
			continue
		}
		args := make(map[string]*schema.ArgDef, len(a.Args))
		for argName, arg := range a.Args {
			if arg == nil {
				continue
			}
			args[argName] = &schema.ArgDef{
				Presentation: arg.Presentation,
				Type:         arg.Type,
				Required:     arg.Required,
				Description:  arg.Description,
				Default:      arg.Default,
				// Enum MUST be carried through: internal/planner/enumplan.go's
				// plan-time ENUM-006 (S1 default-not-member) and ENUM-007
				// (S1 literal-not-member) checks read this exact
				// plan.Tools[name].Actions[action].Args[arg].Enum field --
				// dropping it here silently defeated both checks for every
				// catalog/toolRefs-resolved tool (this corpus's dominant
				// fixture shape), a genuine pre-existing gap directly
				// coupled to this MVP's R1/R2 enum-enforcement scope.
				Enum: arg.Enum,
			}
		}
		outputs := make(map[string]*schema.ArgDef, len(a.Outputs))
		for outName, out := range a.Outputs {
			if out == nil {
				continue
			}
			outputs[outName] = &schema.ArgDef{
				Presentation: out.Presentation,
				From:         out.From,
				Optional:     out.Optional,
				Type:         out.Type,
				Required:     out.Required,
				Description:  out.Description,
				Default:      out.Default,
				// Enum: see the Args loop above -- the S2 (tool action
				// outputs.<name>) enum metadata/default check needs this
				// too.
				Enum: out.Enum,
			}
		}
		schemaAct := &schema.ToolAction{
			Description: a.Description,
			Argv:        a.Argv,
			Args:        args,
			Returns:     a.Returns,
			// Execute and Outputs must be carried through: B1/B5 (Barbara's
			// gate review) both depend on the planner's own tool registry
			// exposing whether a catalog/toolRefs-resolved action is a
			// substitution (execute.kind: runbook) -- without this, every
			// such action silently reads as a plain, non-substituted one
			// to internal/planner/validate.go's step-context check and to
			// any plan-time pkgsubst.Plan lookup keyed off plan.Tools.
			Execute: a.Execute,
			Outputs: outputs,
		}
		// Carry Classification from the original parsed schema action.
		// The runtime ToolAction does not store Classification directly, but
		// it retains a reference to the original schema action for exactly
		// this kind of fidelity requirement (SchemaAction() was added for
		// substitution planning; re-using it here avoids adding a new field
		// to the runtime type solely for governance display).
		if sa := a.SchemaAction(); sa != nil {
			schemaAct.Classification = sa.Classification
		}
		actions[name] = schemaAct
	}
	return &schema.ToolDef{
		Name:       def.Name,
		Actions:    actions,
		Governance: def.Governance,
	}
}
