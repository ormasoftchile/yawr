package pkgcatalog

import (
	"fmt"
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgpath"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/semver"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// Binding is the resolved, file-local name -> tool-definition mapping
// produced by Phase B for one runbook file. Tool-name binding is lexically
// scoped to the declaring file and is never inherited by an included child
// Includes use lexical scoping and
// Global Package Set).
type Binding struct {
	Name string
	Def  toolpkg.ToolDef
}

// BindFile resolves runbookPath's toolRefs entries against the frozen base
// catalog, adding this file's own tier-4 (toolRefs[].path) entries.
// Returns one Binding per toolRefs entry plus every PKG-* error found.
//
// packageRoot is "" for an ordinary, top-level runbook (toolRefs[].path is
// workspace-level: containment root = workspace root, escapes are only
// reported, never rejected). When runbookPath is itself package-internal
// (reached only via a package's exports.tools[].path or an execute.path,
// never run directly -- e.g. a substitute runbook), callers MUST pass that
// package's root here: Table tab:tool-path-bases's blanket package rule
// ("packages MUST NOT contain runbooks that include files outside the
// package root") overrides the general workspace-level classification for
// that file's own toolRefs[].path entries, making them package-internal
// (containment root = package root, escapes are PKG-007).
//
// This does not mutate base; a new, file-scoped view (base entries plus
// this file's tier-4 entries) is used internally and discarded after the
// call, per §Tool Discovery: "A runtime MUST NOT mutate the catalog after
// Phase C completes."
func BindFile(base *Catalog, runbookPath string, refs []*schema.ToolRef, packageRoot string) ([]Binding, []error) {
	var errs []error
	var bindings []Binding
	runbookDir := filepath.Dir(runbookPath)

	// This file's own tier-4 entries (toolRefs[].path), added to a
	// file-scoped bare-name index so cross-tier shadow detection can see
	// them without polluting the frozen base catalog.
	fileBare := make(map[string][]*Entry)
	for k, v := range base.byBare {
		fileBare[k] = append([]*Entry(nil), v...)
	}

	for _, ref := range refs {
		if ref.Package != "" && ref.Path != "" {
			errs = append(errs, errkit.New("PKG-029", fmt.Sprintf(
				"toolRefs[%s]: package and path are mutually exclusive", ref.Name)))
			continue
		}

		switch {
		case ref.Path != "":
			b, warnings, err := base.source.bindByPath(runbookDir, ref, base.WorkspaceRoot, packageRoot)
			errs = append(errs, warnings...)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			bindings = append(bindings, *b)

		case ref.Package != "":
			b, err := bindByPackage(base, ref)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			bindings = append(bindings, *b)

		default:
			b, err := bindByBareName(base, fileBare, ref)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			bindings = append(bindings, *b)
		}

	}
	return bindings, errs
}

// bindByPath resolves a tier-4 toolRefs[].path entry using the exact
// per-kind base of Table tab:tool-path-bases: KindToolRefsPath (workspace
// root containment, escape merely reported) for an ordinary runbook, or
// KindToolRefsPathPackageInternal (package root containment, escape is
// PKG-007) when packageRoot is non-empty.
func (source *Source) bindByPath(runbookDir string, ref *schema.ToolRef, workspaceRoot, packageRoot string) (*Binding, []error, error) {
	kind := pkgpath.KindToolRefsPath
	if packageRoot != "" {
		kind = pkgpath.KindToolRefsPathPackageInternal
	}
	if err := pkgpath.ValidateAuthoredSyntax(ref.Path, kind.AllowsAbsolute()); err != nil {
		return nil, nil, err
	}
	referencingFile := filepath.Join(runbookDir, "_") // any file within runbookDir; only its dirname is used
	abs, external, rerr := pkgpath.ResolveKind(kind, referencingFile, ref.Path, workspaceRoot, packageRoot)
	if rerr != nil {
		return nil, nil, rerr
	}
	if _, err := source.stat(abs); err != nil {
		return nil, nil, errkit.New("PKG-030", fmt.Sprintf("toolRefs[%s]: path %q does not resolve to an existing file: %v", ref.Name, ref.Path, err))
	}
	var warnings []error
	if external {
		// The path kind is
		// KindToolRefsPath (workspace-level) here -- packageRoot=="" was
		// the precondition for choosing it over
		// KindToolRefsPathPackageInternal above -- so an out-of-workspace
		// resolution is operator configuration, reported as PKG-W003, not
		// rejected as PKG-007.
		warnings = append(warnings, errkit.New("PKG-W003", fmt.Sprintf(
			"toolRefs[%s]: path %q resolves outside the workspace root %q; recorded as an external root", ref.Name, ref.Path, workspaceRoot)))
	}
	def, err := source.parse(abs)
	if err != nil {
		return nil, warnings, errkit.New("PKG-030", fmt.Sprintf("toolRefs[%s]: cannot parse tool file %q: %v", ref.Name, ref.Path, err))
	}
	runtimeDef, err := source.runtime(def)
	if err != nil {
		return nil, warnings, errkit.New("PKG-030", fmt.Sprintf("toolRefs[%s]: %v", ref.Name, err))
	}
	if ref.Version != "" {
		if err := checkVersionConstraint(ref.Name, "", ref.Version, def.Version); err != nil {
			return nil, warnings, err
		}
	}
	runtimeDef.Name = ref.Name
	// SourcePath/PackageRoot are required to resolve a substituted action's
	// execute.path (KindExecutePath resolves relative to the declaring
	// .tool.yaml, contained within the exporting package's root). An
	// ad-hoc (non-package) toolRefs[].path tool has no package root of its
	// own, so its own directory is both its referencing base and its
	// containment root.
	runtimeDef.SourcePath = abs
	if packageRoot != "" {
		runtimeDef.PackageRoot = packageRoot
	} else {
		runtimeDef.PackageRoot = filepath.Dir(abs)
	}
	return &Binding{Name: ref.Name, Def: runtimeDef}, warnings, nil
}

// bindByPackage resolves a toolRefs entry that pins package (and optionally
// version), which "makes a collision impossible by construction."
func bindByPackage(base *Catalog, ref *schema.ToolRef) (*Binding, error) {
	candidates := base.ByBare(ref.Name)
	var match *Entry
	for _, e := range candidates {
		if e.Tier == TierPackage && e.PackageName == ref.Package {
			match = e
			break
		}
	}
	if match == nil {
		// Fall back to a fully-qualified lookup in case the caller supplied
		// the tool's own bare name that differs from ref.Name (defensive).
		if e, ok := base.ByQualified(ref.Package + "/" + ref.Name); ok {
			match = e
		}
	}
	if match == nil {
		return nil, errkit.New("PKG-011", fmt.Sprintf(
			"toolRefs[%s]: tool is not exported by package %q", ref.Name, ref.Package))
	}
	if ref.Version != "" {
		if err := checkVersionConstraint(ref.Name, ref.Package, ref.Version, match.Version); err != nil {
			return nil, err
		}
	}
	def := match.Def
	def.Name = ref.Name
	return &Binding{Name: ref.Name, Def: def}, nil
}

// bindByBareName resolves an unqualified toolRefs entry (no package, no
// path) by looking up the bare name across all tiers. A single tier match
// binds directly; multiple entries within the SAME tier is PKG-006 (already
// caught by Build's frozen-catalog check, but re-validated defensively
// here); multiple tiers matching is an undeclared cross-tier shadow —
// PKG-022 — because the caller did not disambiguate with package/path.
func bindByBareName(base *Catalog, fileBare map[string][]*Entry, ref *schema.ToolRef) (*Binding, error) {
	group := fileBare[ref.Name]
	if len(group) == 0 {
		return nil, errkit.New("PKG-030", fmt.Sprintf("toolRefs[%s]: name resolves to no catalog entry", ref.Name))
	}

	tiers := make(map[Tier][]*Entry)
	for _, e := range group {
		tiers[e.Tier] = append(tiers[e.Tier], e)
	}
	if len(tiers) > 1 {
		return nil, errkit.New("PKG-022", fmt.Sprintf(
			"toolRefs[%s]: bare name is shadowed across tiers without a declared toolRefs[].package or .path; qualify the reference", ref.Name))
	}
	// Exactly one tier: still must be a single entry (same-tier collisions
	// are a hard error, normally already reported by Build).
	var only Tier
	for t := range tiers {
		only = t
	}
	if len(tiers[only]) > 1 {
		return nil, errkit.New("PKG-006", fmt.Sprintf("toolRefs[%s]: bare name collides at tier %d", ref.Name, only))
	}
	match := tiers[only][0]
	def := match.Def
	def.Name = ref.Name
	return &Binding{Name: ref.Name, Def: def}, nil
}

// checkVersionConstraint parses ref's version constraint and validates it
// against the resolved tool's own declared meta.version. Per
// toolRefs[].version is checked
// against the resolved tool definition's declared version when a package
// pin makes the resolved tool unambiguous. If the resolved tool declares no
// version (declaredVersion == ""), the constraint cannot be evaluated and
// is treated as vacuously satisfied (nothing to contradict it) — this
// mirrors requires[].version's PKG-002 check, which does have a concrete
// manifest version to compare against; toolRefs[].version is the weaker,
// best-effort form of that same check for individually pinned bindings.
// checkVersionConstraint reports PKG-002 with the package name (when
// known -- toolRefs[].package pins bind entirely by construction, so it is
// always known for bindByPackage's caller; ad-hoc path-pinned toolRefs
// have no package identity, hence pkgName may be "") and its declaration
// site (§4.4: "the list of declaration sites that contributed to the
// intersection" -- for toolRefs this is always the single toolRefs[name]
// entry itself, since toolRefs versions are not merged with requires:).
func checkVersionConstraint(refName, pkgName, wantVersion, declaredVersion string) error {
	constraint, err := semver.ParseConstraint(wantVersion)
	if err != nil {
		return err
	}
	if declaredVersion == "" {
		return nil
	}
	v, err := semver.ParseVersion(declaredVersion)
	if err != nil {
		return err
	}
	if !constraint.Satisfies(v) {
		pkgPart := ""
		if pkgName != "" {
			pkgPart = fmt.Sprintf("package %q: ", pkgName)
		}
		return errkit.New("PKG-002", fmt.Sprintf(
			"%stoolRefs[%s]: resolved tool version %s does not satisfy constraint %q; declaration site: toolRefs[%s].version",
			pkgPart, refName, declaredVersion, wantVersion, refName))
	}
	return nil
}
