// Package pkgcatalog implements the YAWR Tool Packages MVP catalog freeze
// and five-tier resolution algorithm defined in
// Tool discovery and catalog
// Construction, §Tool Packages, and §Digest, Evidence, Trace, Resume.
//
// The package exposes two phases mirroring the ratified architecture
// (AR-TP-2):
//
//   - Phase C (Build): produces a frozen, run-scoped Catalog from the
//     workspace's requires:/tool-paths declarations. Tiers 0-3.
//   - Phase B (BindFile): binds one runbook file's toolRefs entries against
//     the frozen catalog, adding that file's own tier-4 (toolRefs[].path)
//     entries and enforcing the precedence/collision rules.
package pkgcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgpath"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/semver"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

// Tier enumerates the five catalog source tiers
// Higher tiers win.
type Tier int

const (
	TierBuiltin Tier = iota // 0: built-in registry, bare names only
	TierPackage             // 1: requires:, <package>/<tool> + bare name
	TierProject             // 2: config.yaml tool-paths + <workspace>/tools/, bare names
	TierDynamic             // 3: extension contributions + MCP tools/list, qualified only
	TierRunbook             // 4: toolRefs[].path in the runbook being planned, file-local bare name
)

// Entry is one frozen catalog entry.
type Entry struct {
	Qualified   string // fully-qualified name (<origin>/<tool>), always set
	Bare        string // bare name; empty for tier 3 (dynamic, mandatory-qualified)
	Tier        Tier
	Def         toolpkg.ToolDef
	PackageName string // set for tier 1 entries
	Version     string // tool definition's own meta.version (schema.ToolDef.Version), empty if undeclared
	SourcePath  string // absolute path to the .tool.yaml this entry was loaded from
	Digest      string // sha256:<hex> file digest (or package digest for tier-1 grouping)
}

// LockedPackageInfo captures the resolved package identity used to emit a
// yawr.package-lock/v1 record.
// map / lock). Populated for every tier-1 package resolved into the catalog.
type LockedPackageInfo struct {
	Name    string
	Version string
	Root    string // workspace-relative POSIX, or "external:sha256:..."
	Digest  string
	Exports []schema.LockedExport
	// RunbookExports mirrors Exports for exports.runbooks[] entries.
	RunbookExports []schema.LockedExport
	// External mirrors the requires[].path resolution's own external bool
	// True when this package's root
	// resolved outside the workspace root. Carried here so callers can
	// emit the package/resolved trace event's external field without
	// re-deriving it from Root's "external:" string-prefix encoding.
	External bool
	// ConstraintSources lists the declaration sites that contributed a
	// version constraint to this package's effective (intersected)
	// requires[].version, in declaration order (project before runbook,
	// per mergeRequirements). Populated from mergeRequirements'
	// scopeByName/sourcesByName bookkeeping so PKG-002 and the
	// package/resolved trace event's constraintSources field (§4.4,
	// §7.4 of the ratified spec) both report real provenance instead of
	// an unconditionally-empty slice.
	ConstraintSources []string
}

// RunbookEntry is one frozen *runbook* catalog entry, sourced from a
// package manifest's exports.runbooks[]. Exported runbooks are the only
// targets a dynamic include (include.runbook_ref + resolve_from: catalog)
// may resolve to: resolution never consults an arbitrary runtime path.
type RunbookEntry struct {
	PackageRoot    string
	Qualified      string // "<package>/<id>"
	Bare           string // "<id>"
	PackageName    string
	PackageVersion string
	Path           string // absolute path to the exported .runbook.yaml
	// FileDigest is the sha256 of the exported runbook file's own bytes.
	FileDigest string
	// PackageDigest is the owning package's closure digest, mirroring the
	// tier-1 tool Entry.Digest convention (§7.3).
	PackageDigest string
}

// Catalog is the frozen result of Phase C (catalog construction).
type Catalog struct {
	source        *Source
	WorkspaceRoot string
	entries       []*Entry
	byQualified   map[string]*Entry
	byBare        map[string][]*Entry // grouped, all tiers, in tier ascending then declaration order
	runbooks      []*RunbookEntry
	rbByQualified map[string]*RunbookEntry
	rbByBare      map[string][]*RunbookEntry
	Packages      []LockedPackageInfo
	frozen        bool
}

// RunbookEntries returns every exported-runbook entry in deterministic
// (package declaration, then export declaration) order.
func (c *Catalog) RunbookEntries() []*RunbookEntry {
	out := make([]*RunbookEntry, len(c.runbooks))
	copy(out, c.runbooks)
	return out
}

// RunbookByQualified looks up an exported runbook by "<package>/<id>".
func (c *Catalog) RunbookByQualified(name string) (*RunbookEntry, bool) {
	e, ok := c.rbByQualified[name]
	return e, ok
}

// RunbookByBare returns every exported runbook sharing a bare id, in
// package declaration order. More than one entry means the bare id is
// ambiguous and callers MUST require a package-qualified reference.
func (c *Catalog) RunbookByBare(name string) []*RunbookEntry {
	return c.rbByBare[name]
}

func (c *Catalog) addRunbook(e *RunbookEntry) {
	c.runbooks = append(c.runbooks, e)
	if c.rbByQualified == nil {
		c.rbByQualified = map[string]*RunbookEntry{}
		c.rbByBare = map[string][]*RunbookEntry{}
	}
	c.rbByQualified[e.Qualified] = e
	c.rbByBare[e.Bare] = append(c.rbByBare[e.Bare], e)
}

// Entries returns all frozen catalog entries, in the deterministic order
// they were added (tier ascending, then declaration order within tier).
func (c *Catalog) Entries() []*Entry {
	out := make([]*Entry, len(c.entries))
	copy(out, c.entries)
	return out
}

// ByQualified looks up an entry by its fully-qualified name.
func (c *Catalog) ByQualified(name string) (*Entry, bool) {
	e, ok := c.byQualified[name]
	return e, ok
}

// ByBare returns every entry (across all tiers) sharing a bare name, in
// tier-ascending order.
func (c *Catalog) ByBare(name string) []*Entry {
	return c.byBare[name]
}

// BuildOptions supplies every input Phase C needs to construct the frozen
// base catalog (tiers 0-3). Tier 4 (toolRefs[].path) is added per file by
// BindFile and is intentionally excluded here.
type BuildOptions struct {
	Source *Source
	// RequirementDocuments opts into multi-file dependency aggregation. Nil
	// retains the legacy project/root-only catalog contract.
	RequirementDocuments []RequirementDocument
	// LexicalBindings defers bare-name ambiguity to a file binding site.
	// Qualified identity collisions remain fatal.
	LexicalBindings bool
	// WorkspaceRoot is the workspace root directory (absolute).
	WorkspaceRoot string
	// Builtins supplies tier-0 entries (host-compiled built-in registry).
	Builtins []toolpkg.ToolDef
	// ProjectRequires are .yawr/config.yaml's top-level requires: entries,
	// resolved relative to WorkspaceRoot (workspace-level path kind).
	ProjectRequires []*schema.PackageRequirement
	// RunbookRequires are the top-level runbook's requires: entries. Path
	// is resolved relative to RunbookPath's directory (workspace-level path
	// kind, but a different resolution base than ProjectRequires — see
	// tab:tool-path-bases).
	RunbookRequires []*schema.PackageRequirement
	// RunbookPath is the file that declared RunbookRequires. Required only
	// when RunbookRequires is non-empty.
	RunbookPath string
	// ProjectToolPaths are config.yaml's tool-paths[] entries, workspace-root
	// relative, processed in declaration order (tier 2, before the
	// conventional <workspace>/tools/ directory).
	ProjectToolPaths []string
	// Dynamic supplies tier-3 entries (extension contributions + MCP
	// tools/list), already qualified, in extension/MCP declaration order.
	Dynamic []*Entry
}

// Build runs Phase C: catalog construction. It is a pure function of its
// inputs and the workspace filesystem, and never mutates any file. Returns
// the frozen catalog plus every PKG-* error found; callers MUST separate
// fatal errors from PKG-W-class advisories (errkit.SplitWarnings) before
// deciding to abort; a
// workspace-level requires[].path resolving outside the workspace root is
// reported via PKG-W003 and MUST NOT itself be treated as a hard failure.
func Build(opts BuildOptions) (*Catalog, []error) {
	c := &Catalog{
		source:        opts.Source,
		WorkspaceRoot: opts.WorkspaceRoot,
		byQualified:   make(map[string]*Entry),
		byBare:        make(map[string][]*Entry),
		rbByQualified: make(map[string]*RunbookEntry),
		rbByBare:      make(map[string][]*RunbookEntry),
	}
	var errs []error

	// Tier 0: builtins.
	for _, def := range opts.Builtins {
		c.add(&Entry{Qualified: def.Name, Bare: def.Name, Tier: TierBuiltin, Def: def})
	}

	// Tier 1: requires: (project-level, then runbook-level; project entries
	// precede runbook entries for the same run).
	if opts.RequirementDocuments != nil {
		documents := append([]RequirementDocument(nil), opts.RequirementDocuments...)
		if len(opts.RunbookRequires) > 0 {
			documents = append(documents, RequirementDocument{Path: opts.RunbookPath, Requirements: opts.RunbookRequires})
		}
		requirements, mergeErrs := ResolveRequirements(opts, documents)
		errs = append(errs, mergeErrs...)
		for _, resolution := range requirements {
			sites := make([]string, len(resolution.Declarations))
			for index, site := range resolution.Declarations {
				sites[index] = site.String()
			}
			fileOpts := opts
			fileOpts.RunbookPath = resolution.owner
			errs = append(errs, c.loadPackage(&resolution.Requirement, resolution.kind, fileOpts, sites, resolution.pinned)...)
		}
	} else {
		merged, scopeByName, sourcesByName, resolvedByName, mergeErrs := mergeRequirements(
			opts.ProjectRequires, opts.RunbookRequires, opts.RunbookPath, opts.WorkspaceRoot,
		)
		errs = append(errs, mergeErrs...)
		for _, req := range merged {
			kind := pkgpath.KindRequiresProject
			if scopeByName[req.Package] == scopeRunbook {
				kind = pkgpath.KindRequiresRunbook
			}
			pkgErrs := c.loadPackage(req, kind, opts, sourcesByName[req.Package], resolvedByName[req.Package])
			errs = append(errs, pkgErrs...)
		}
	}

	// Tier 2: project tool-paths (declaration order), then <workspace>/tools/.
	dirs := append([]string(nil), opts.ProjectToolPaths...)
	dirs = append(dirs, "tools")
	scannedPaths := make(map[string]bool)
	for _, d := range dirs {
		abs := d
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(opts.WorkspaceRoot, d)
		}
		found, err := c.source.scanToolDir(opts.WorkspaceRoot, abs)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range found {
			key := pkgpath.NormalizeForComparison(e.SourcePath, os.PathSeparator == '\\')
			if opts.LexicalBindings && scannedPaths[key] {
				continue
			}
			scannedPaths[key] = true
			e.Tier = TierProject
			c.add(e)
		}
	}

	// Tier 3: dynamic (extensions, then MCP), in caller-supplied order.
	for _, e := range opts.Dynamic {
		if opts.LexicalBindings {
			if e == nil {
				errs = append(errs, errkit.New("PKG-004", "dynamic catalog entry is missing"))
				continue
			}
			copy := *e
			e = &copy
		}
		e.Tier = TierDynamic
		e.Bare = "" // tier 3 entries are mandatory-qualified; never bare.
		c.add(e)
	}

	// Legacy plans reject bare collisions globally; scoped preflight rejects
	// ambiguity at the binding site instead.
	if opts.LexicalBindings {
		seen := map[string]bool{}
		for _, entry := range c.entries {
			if entry.Tier != TierPackage && entry.Tier != TierDynamic {
				continue
			}
			if seen[entry.Qualified] {
				errs = append(errs, errkit.New("PKG-006", fmt.Sprintf("qualified tool identity %q is duplicated", entry.Qualified)))
			}
			seen[entry.Qualified] = true
		}
	} else {
		errs = append(errs, c.detectSameTierCollisions()...)
	}

	c.frozen = true
	return c, errs
}

func (c *Catalog) add(e *Entry) {
	c.entries = append(c.entries, e)
	c.byQualified[e.Qualified] = e
	if e.Bare != "" {
		c.byBare[e.Bare] = append(c.byBare[e.Bare], e)
	}
}

func (c *Catalog) detectSameTierCollisions() []error {
	var errs []error
	// Deterministic ordering: range over
	// c.byBare/byTier are unsorted Go maps, and their iteration order
	// leaking into the returned error slice made a governance-bearing
	// plan report vary run to run even though the catalog/digest
	// themselves are unaffected. Sort both keys before iterating.
	bareNames := make([]string, 0, len(c.byBare))
	for bare := range c.byBare {
		bareNames = append(bareNames, bare)
	}
	sort.Strings(bareNames)
	for _, bare := range bareNames {
		group := c.byBare[bare]
		byTier := make(map[Tier][]*Entry)
		for _, e := range group {
			byTier[e.Tier] = append(byTier[e.Tier], e)
		}
		tiers := make([]Tier, 0, len(byTier))
		for tier := range byTier {
			tiers = append(tiers, tier)
		}
		sort.Slice(tiers, func(i, j int) bool { return tiers[i] < tiers[j] })
		for _, tier := range tiers {
			es := byTier[tier]
			if len(es) > 1 {
				names := make([]string, 0, len(es))
				for _, e := range es {
					names = append(names, e.Qualified)
				}
				sort.Strings(names)
				errs = append(errs, errkit.New("PKG-006", fmt.Sprintf(
					"bare name %q collides at tier %d between %s; disambiguate with toolRefs[].package or the fully-qualified name",
					bare, tier, strings.Join(names, ", "))))
			}
		}
	}
	return errs
}

// requirementScope records which declaration scope's path: value is the
// one actually resolved for a merged requirement, so loadPackage can select
// the exact referencing_file Table tab:tool-path-bases assigns to that
// scope (project-scope requires[].path is workspace-root relative;
// runbook-scope requires[].path is declaring-runbook-file relative -- two
// different bases for the same field name at two different scopes).
type requirementScope int

const (
	scopeProject requirementScope = iota
	scopeRunbook
)

// mergeRequirements applies PKG-005 (duplicate package in one list) and
// intersects project+runbook constraints for the same package name,
// returning PKG-002 for an empty resulting intersection is deferred to
// loadPackage (which knows the resolved version). PKG-024 (conflicting
// path) is also checked here. The returned scope map records, per package
// name, which scope's path: value survived the merge (project is
// authoritative when both declare one). The returned sources map records,
// per package name, the ordered list of declaration-site labels (§4.4:
// "the list of declaration sites that contributed to the intersection")
// that supplied a non-empty version: constraint for that package --
// "project requires:" and/or "runbook requires: (<runbookPath>)".
func mergeRequirements(project, runbook []*schema.PackageRequirement, runbookPath, workspaceRoot string) ([]*schema.PackageRequirement, map[string]requirementScope, map[string][]string, map[string]*resolvedRequirementPath, []error) {
	var errs []error
	byName := make(map[string]*schema.PackageRequirement)
	scope := make(map[string]requirementScope)
	sources := make(map[string][]string)
	resolved := make(map[string]*resolvedRequirementPath)
	order := make([]string, 0, len(project)+len(runbook))

	runbookSite := "runbook requires:"
	if runbookPath != "" {
		runbookSite = fmt.Sprintf("runbook requires: (%s)", runbookPath)
	}

	seenProject := make(map[string]bool)
	for _, r := range project {
		if seenProject[r.Package] {
			errs = append(errs, errkit.New("PKG-005", fmt.Sprintf("duplicate package %q in project requires:", r.Package)))
			continue
		}
		seenProject[r.Package] = true
		cp := *r
		byName[r.Package] = &cp
		if cp.Path != "" {
			scope[r.Package] = scopeProject
		}
		if cp.Version != "" {
			sources[r.Package] = append(sources[r.Package], "project requires:")
		}
		order = append(order, r.Package)
	}

	seenRunbook := make(map[string]bool)
	for _, r := range runbook {
		if seenRunbook[r.Package] {
			errs = append(errs, errkit.New("PKG-005", fmt.Sprintf("duplicate package %q in runbook requires:", r.Package)))
			continue
		}
		seenRunbook[r.Package] = true
		existing, ok := byName[r.Package]
		if !ok {
			cp := *r
			byName[r.Package] = &cp
			if cp.Path != "" {
				scope[r.Package] = scopeRunbook
			}
			if cp.Version != "" {
				sources[r.Package] = append(sources[r.Package], runbookSite)
			}
			order = append(order, r.Package)
			continue
		}
		// Same package declared at both scopes: constraints intersect.
		if r.Path != "" && existing.Path != "" {
			comparison := compareRequirementPaths(existing.Path, r.Path, workspaceRoot, runbookPath)
			if comparison.runbookErr != nil {
				errs = append(errs, comparison.runbookErr)
			}
			if comparison.projectResolved != nil {
				resolved[r.Package] = comparison.projectResolved
			}
			if comparison.runbookErr != nil || !comparison.comparable {
				continue
			}
			if comparison.comparable && !comparison.equivalent {
				errs = append(errs, errkit.New("PKG-024", fmt.Sprintf(
					"package %q: runbook-level path %q conflicts with project-level path %q", r.Package, r.Path, existing.Path)))
				continue
			}
		}
		merged := *existing
		if merged.Version == "" {
			merged.Version = r.Version
		} else if r.Version != "" {
			merged.Version = merged.Version + " " + r.Version
		}
		if r.Version != "" {
			sources[r.Package] = append(sources[r.Package], runbookSite)
		}
		if merged.Path == "" && r.Path != "" {
			merged.Path = r.Path
			scope[r.Package] = scopeRunbook
		}
		byName[r.Package] = &merged
	}

	out := make([]*schema.PackageRequirement, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out, scope, sources, resolved, errs
}

type resolvedRequirementPath struct {
	path     string
	external bool
	identity os.FileInfo
}

type requirementPathComparison struct {
	equivalent      bool
	comparable      bool
	projectResolved *resolvedRequirementPath
	runbookErr      error
}

func compareRequirementPaths(projectPath, runbookRequirementPath, workspaceRoot, runbookPath string) requirementPathComparison {
	projectSyntaxErr := pkgpath.ValidateAuthoredSyntax(projectPath, pkgpath.KindRequiresProject.AllowsAbsolute())
	runbookSyntaxErr := pkgpath.ValidateAuthoredSyntax(runbookRequirementPath, pkgpath.KindRequiresRunbook.AllowsAbsolute())
	if projectSyntaxErr != nil || runbookSyntaxErr != nil {
		return requirementPathComparison{runbookErr: runbookSyntaxErr}
	}
	projectResolvedPath, projectExternal, projectErr := pkgpath.ResolveKind(
		pkgpath.KindRequiresProject, "", projectPath, workspaceRoot, "",
	)
	runbookResolved, _, runbookErr := pkgpath.ResolveKind(
		pkgpath.KindRequiresRunbook, runbookPath, runbookRequirementPath, workspaceRoot, "",
	)
	if projectErr != nil || runbookErr != nil {
		return requirementPathComparison{runbookErr: runbookErr}
	}
	projectInfo, projectErr := os.Stat(projectResolvedPath)
	runbookInfo, runbookErr := os.Stat(runbookResolved)
	if projectErr != nil || runbookErr != nil {
		return requirementPathComparison{comparable: true}
	}
	pinnedProject := &resolvedRequirementPath{
		path: projectResolvedPath, external: projectExternal, identity: projectInfo,
	}
	return requirementPathComparison{
		equivalent:      os.SameFile(projectInfo, runbookInfo),
		comparable:      true,
		projectResolved: pinnedProject,
	}
}

// loadPackage resolves and loads a single requires: entry (already merged
// across project/runbook scope) into the catalog as tier-1 entries. kind
// selects the exact referencing_file Table tab:tool-path-bases assigns to
// req's surviving scope (KindRequiresProject or KindRequiresRunbook).
// constraintSources is the ordered list of declaration sites (§4.4) that
// contributed a version: constraint to req's already-intersected
// req.Version, used to fill out PKG-002's message and the resulting
// LockedPackageInfo.ConstraintSources.
func (c *Catalog) loadPackage(req *schema.PackageRequirement, kind pkgpath.Kind, opts BuildOptions, constraintSources []string, pinned *resolvedRequirementPath) []error {
	var errs []error
	if req.Path == "" {
		return []error{errkit.New("PKG-001", fmt.Sprintf("package %q: no path declared and no project binding supplies one", req.Package))}
	}
	if err := pkgpath.ValidateAuthoredSyntax(req.Path, kind.AllowsAbsolute()); err != nil {
		return []error{err}
	}
	constraint, err := semver.ParseConstraint(req.Version)
	if err != nil {
		return []error{err}
	}
	// KindRequiresProject resolves against the workspace root itself;
	// KindRequiresRunbook resolves against the declaring runbook file
	// (Table tab:tool-path-bases) -- two different bases for the same
	// field name at two different declaration scopes, selected exactly by
	// kind rather than guessed by retrying both.
	referencingFile := opts.RunbookPath
	if kind == pkgpath.KindRequiresProject {
		referencingFile = opts.WorkspaceRoot
	}
	var resolved string
	var external bool
	if pinned != nil {
		resolved = pinned.path
		external = pinned.external
		if pinned.identity != nil {
			current, statErr := opts.Source.stat(resolved)
			if statErr != nil || !sameSourceIdentity(pinned.identity, current) {
				return []error{errkit.New("PKG-008", fmt.Sprintf(
					"package %q: resolved path identity changed during catalog construction", req.Package))}
			}
		}
	} else {
		var rerr error
		resolved, external, rerr = pkgpath.ResolveKind(kind, referencingFile, req.Path, opts.WorkspaceRoot, "")
		if rerr != nil {
			return []error{rerr}
		}
	}
	if external {
		// A workspace-level
		// path (requires[].path at either scope) resolving outside the
		// workspace root is operator configuration, never rejected for
		// that reason alone. It MUST be recorded as an external root
		// (workspaceRelativePOSIX below already does this for the lock)
		// and reported as PKG-W003, not PKG-007.
		errs = append(errs, errkit.New("PKG-W003", fmt.Sprintf(
			"package %q: path %q resolves outside the workspace root %q; recorded as an external root", req.Package, req.Path, opts.WorkspaceRoot)))
	}

	canonicalManifest := filepath.Join(resolved, schema.PackageManifestFilename)
	data, rerr2 := c.source.read(canonicalManifest)
	if rerr2 != nil {
		return []error{errkit.New("PKG-001", fmt.Sprintf("package %q: cannot read manifest: %v", req.Package, rerr2))}
	}
	manifestPath := canonicalManifest
	var manifest schema.PackageManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return []error{errkit.New("PKG-004", fmt.Sprintf("package %q: manifest parse error: %v", req.Package, err))}
	}
	if manifest.APIVersion != schema.PackageAPIVersion {
		errs = append(errs, errkit.New("PKG-004", fmt.Sprintf("package %q: apiVersion must be %q, got %q", req.Package, schema.PackageAPIVersion, manifest.APIVersion)))
	}
	if len(manifest.Dependencies) > 0 {
		errs = append(errs, errkit.New("PKG-016", fmt.Sprintf("package %q: non-empty dependencies: is out of MVP scope", req.Package)))
	}
	version, verr := semver.ParseVersion(manifest.Meta.Version)
	if verr != nil {
		errs = append(errs, verr)
	} else if !constraint.Satisfies(version) {
		sitesMsg := "none"
		if len(constraintSources) > 0 {
			sitesMsg = strings.Join(constraintSources, ", ")
		}
		errs = append(errs, errkit.New("PKG-002", fmt.Sprintf(
			"package %q: resolved version %s does not satisfy the effective (intersected) constraint %q; declaration sites: %s",
			req.Package, manifest.Meta.Version, req.Version, sitesMsg)))
	}
	if len(manifest.Exports.Tools) == 0 && len(manifest.Exports.Runbooks) == 0 {
		errs = append(errs, errkit.New("PKG-004", fmt.Sprintf("package %q: exports.tools must have at least one entry", req.Package)))
	}

	var lockRunbookExports []schema.LockedExport

	var lockExports []schema.LockedExport
	var digestLines []string
	if manifestDigest, derr := c.source.digest(manifestPath); derr == nil {
		digestLines = append(digestLines, manifestDigest+"  "+filepath.Base(manifestPath))
	}

	// pendingEntry defers catalog.add of each export until the package
	// digest is fully known (§7.3: tier-1 entries carry the PACKAGE
	// digest, not the exported file's own digest -- B3). closureVisited
	// is shared across every export in this package so a substitute
	// runbook or package-internal include reachable from more than one
	// export is only hashed once and cannot recurse into a cycle.
	type pendingEntry struct {
		qualified  string
		bare       string
		def        toolpkg.ToolDef
		version    string
		sourcePath string
	}
	var pending []pendingEntry
	closureVisited := map[string]bool{}

	seenExportID := make(map[string]bool)
	seenExportPath := make(map[string]bool)
	// seenExportIDNorm/seenExportPathNorm catch the PKG-018 case: two
	// exports whose id/path differ only by ASCII case or Unicode
	// normalisation form (NFC vs NFD) collide as PKG-018 even though the
	// literal strings differ (so the seenExportID/seenExportPath PKG-004
	// literal-duplicate check above doesn't catch them) -- exactly the
	// "works on Linux, breaks on Windows/macOS" class §6.1 rule 5 exists
	// to fail at authoring time, on every platform.
	seenExportIDNorm := make(map[string]string)
	seenExportPathNorm := make(map[string]string)
	for _, exp := range manifest.Exports.Tools {
		if seenExportID[exp.ID] || seenExportPath[exp.Path] {
			errs = append(errs, errkit.New("PKG-004", fmt.Sprintf("package %q: duplicate export id/path %q/%q", req.Package, exp.ID, exp.Path)))
			continue
		}
		idNorm := pkgpath.NormalizeForComparison(exp.ID, true)
		pathNorm := pkgpath.NormalizeForComparison(exp.Path, true)
		if prior, ok := seenExportIDNorm[idNorm]; ok {
			errs = append(errs, errkit.New("PKG-018", fmt.Sprintf(
				"package %q: export id %q collides with %q (differ only by ASCII case or Unicode normalisation form)", req.Package, exp.ID, prior)))
			continue
		}
		if prior, ok := seenExportPathNorm[pathNorm]; ok {
			errs = append(errs, errkit.New("PKG-018", fmt.Sprintf(
				"package %q: export path %q collides with %q (differ only by ASCII case or Unicode normalisation form)", req.Package, exp.Path, prior)))
			continue
		}
		seenExportID[exp.ID] = true
		seenExportPath[exp.Path] = true
		seenExportIDNorm[idNorm] = exp.ID
		seenExportPathNorm[pathNorm] = exp.Path

		if err := pkgpath.ValidateAuthoredSyntax(exp.Path, pkgpath.KindExportsTools.AllowsAbsolute()); err != nil {
			errs = append(errs, err)
			continue
		}
		toolAbs, external2, rerr3 := pkgpath.ResolveKind(pkgpath.KindExportsTools, manifestPath, exp.Path, opts.WorkspaceRoot, resolved)
		if rerr3 != nil {
			errs = append(errs, rerr3)
			continue
		}
		_ = external2
		toolDef, err := c.source.parse(toolAbs)
		if err != nil {
			errs = append(errs, errkit.New("PKG-010", fmt.Sprintf("package %q: export %q references missing/invalid file %s: %v", req.Package, exp.ID, exp.Path, err)))
			continue
		}
		if toolDef.Name != exp.ID {
			errs = append(errs, errkit.New("PKG-023", fmt.Sprintf("package %q: export id %q != tool definition name %q", req.Package, exp.ID, toolDef.Name)))
			continue
		}
		runtimeDef, err := c.source.runtime(toolDef)
		if err != nil {
			errs = append(errs, errkit.New("PKG-010", fmt.Sprintf("package %q: export %q: %v", req.Package, exp.ID, err)))
			continue
		}
		fileDigest, derr := c.source.digest(toolAbs)
		if derr != nil {
			errs = append(errs, errkit.New("PKG-010", fmt.Sprintf("package %q: export %q: cannot digest: %v", req.Package, exp.ID, derr)))
			continue
		}
		lockExports = append(lockExports, schema.LockedExport{ID: exp.ID, Path: exp.Path, Digest: fileDigest})
		closureVisited[toolAbs] = true
		digestLines = append(digestLines, fmt.Sprintf("%s  %s", fileDigest, relPathPosixNFC(resolved, toolAbs)))

		// §7.2 digest closure (B2): every file transitively referenced by
		// this exported tool via execute.path (a substitute runbook) and
		// package-internal includes within it MUST also contribute a
		// digest line, cycle-guarded via closureVisited. Files outside
		// this closure remain unhashed.
		closureLines, closureErrs := c.source.closureDigestLines(toolDef, toolAbs, opts.WorkspaceRoot, resolved, closureVisited)
		digestLines = append(digestLines, closureLines...)
		errs = append(errs, closureErrs...)

		// SourcePath/PackageRoot/PackageName are required to resolve a
		// substituted action's execute.path (contained within this
		// package's own root, never the workspace) and to identify the
		// substitution frame for cycle detection / trace provenance.
		runtimeDef.SourcePath = toolAbs
		runtimeDef.PackageRoot = resolved
		runtimeDef.PackageName = manifest.Meta.Name

		pending = append(pending, pendingEntry{
			qualified:  manifest.Meta.Name + "/" + exp.ID,
			bare:       exp.ID,
			def:        runtimeDef,
			version:    toolDef.Version,
			sourcePath: toolAbs,
		})
	}

	// exports.runbooks[]: the approved catalog surface for dynamic
	// includes. Same authored-syntax + package containment rules as
	// exports.tools[]; every exported file contributes a digest line so a
	// runbook edit changes the package (and therefore catalog) digest.
	type pendingRunbook struct {
		qualified  string
		bare       string
		path       string
		fileDigest string
	}
	var pendingRunbooks []pendingRunbook
	seenRunbookID := make(map[string]bool)
	seenRunbookPath := make(map[string]bool)
	seenRunbookIDNorm := make(map[string]string)
	for _, exp := range manifest.Exports.Runbooks {
		if exp.ID == "" || exp.Path == "" {
			errs = append(errs, errkit.New("PKG-004", fmt.Sprintf("package %q: exports.runbooks entry requires both id and path", req.Package)))
			continue
		}
		if seenRunbookID[exp.ID] || seenRunbookPath[exp.Path] {
			errs = append(errs, errkit.New("PKG-004", fmt.Sprintf("package %q: duplicate runbook export id/path %q/%q", req.Package, exp.ID, exp.Path)))
			continue
		}
		idNorm := pkgpath.NormalizeForComparison(exp.ID, true)
		if prior, ok := seenRunbookIDNorm[idNorm]; ok {
			errs = append(errs, errkit.New("PKG-018", fmt.Sprintf(
				"package %q: runbook export id %q collides with %q (differ only by ASCII case or Unicode normalisation form)", req.Package, exp.ID, prior)))
			continue
		}
		seenRunbookID[exp.ID] = true
		seenRunbookPath[exp.Path] = true
		seenRunbookIDNorm[idNorm] = exp.ID

		if strings.Contains(exp.ID, "/") {
			errs = append(errs, errkit.New("PKG-004", fmt.Sprintf(
				"package %q: runbook export id %q must not contain %q (the qualified identity is <package>/<id>)", req.Package, exp.ID, "/")))
			continue
		}
		if err := pkgpath.ValidateAuthoredSyntax(exp.Path, pkgpath.KindExportsRunbooks.AllowsAbsolute()); err != nil {
			errs = append(errs, err)
			continue
		}
		rbAbs, _, rerr4 := pkgpath.ResolveKind(pkgpath.KindExportsRunbooks, manifestPath, exp.Path, opts.WorkspaceRoot, resolved)
		if rerr4 != nil {
			errs = append(errs, rerr4)
			continue
		}
		fileDigest, derr := c.source.digest(rbAbs)
		if derr != nil {
			errs = append(errs, errkit.New("PKG-010", fmt.Sprintf("package %q: runbook export %q references missing/unreadable file %s: %v", req.Package, exp.ID, exp.Path, derr)))
			continue
		}
		lockRunbookExports = append(lockRunbookExports, schema.LockedExport{ID: exp.ID, Path: exp.Path, Digest: fileDigest})
		if !closureVisited[rbAbs] {
			closureVisited[rbAbs] = true
			digestLines = append(digestLines, fmt.Sprintf("%s  %s", fileDigest, relPathPosixNFC(resolved, rbAbs)))
		}
		pendingRunbooks = append(pendingRunbooks, pendingRunbook{
			qualified:  manifest.Meta.Name + "/" + exp.ID,
			bare:       exp.ID,
			path:       rbAbs,
			fileDigest: fileDigest,
		})
	}

	sort.Strings(digestLines)
	pkgDigest := "sha256:" + hex.EncodeToString(sha256Sum([]byte(strings.Join(digestLines, "\n")+func() string {
		if len(digestLines) > 0 {
			return "\n"
		}
		return ""
	}())))

	// Tier-1 (package-sourced) catalog entries carry the PACKAGE digest,
	// not the individual exported file's digest (§7.3 -- B3): a package
	// version bump or a substitute-runbook edit that doesn't touch the
	// exported file's own bytes must still change CatalogDigest().
	for _, pe := range pending {
		c.add(&Entry{
			Qualified:   pe.qualified,
			Bare:        pe.bare,
			Tier:        TierPackage,
			Def:         pe.def,
			PackageName: manifest.Meta.Name,
			Version:     pe.version,
			SourcePath:  pe.sourcePath,
			Digest:      pkgDigest,
		})
	}

	sort.Slice(lockExports, func(i, j int) bool { return lockExports[i].ID < lockExports[j].ID })
	sort.Slice(lockRunbookExports, func(i, j int) bool { return lockRunbookExports[i].ID < lockRunbookExports[j].ID })

	for _, pr := range pendingRunbooks {
		c.addRunbook(&RunbookEntry{
			PackageRoot:    resolved,
			Qualified:      pr.qualified,
			Bare:           pr.bare,
			PackageName:    manifest.Meta.Name,
			PackageVersion: manifest.Meta.Version,
			Path:           pr.path,
			FileDigest:     pr.fileDigest,
			PackageDigest:  pkgDigest,
		})
	}

	root := workspaceRelativePOSIX(opts.WorkspaceRoot, resolved)
	c.Packages = append(c.Packages, LockedPackageInfo{
		Name:              manifest.Meta.Name,
		Version:           manifest.Meta.Version,
		Root:              root,
		Digest:            pkgDigest,
		Exports:           lockExports,
		RunbookExports:    lockRunbookExports,
		External:          external,
		ConstraintSources: constraintSources,
	})

	return errs
}

// relPathPosixNFC computes path's root-relative, POSIX-separated, NFC-
// normalised form, per §7.2's digest line format
// (relpath_posix_nfc(f, R)). Falls back to path itself (still NFC/POSIX
// normalised) if it cannot be made relative to root.
func relPathPosixNFC(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	return norm.NFC.String(filepath.ToSlash(rel))
}

// closureDigestLines computes the §7.2 digest-closure lines for every
// execute.kind: runbook action declared on toolDef: the substitute
// runbook file itself, plus every package-internal include reachable from
// it (transitively), cycle-guarded via visited (shared across every
// export in the same package so a file reachable from multiple exports is
// only hashed once). Files outside this closure are not hashed.
func (source *Source) closureDigestLines(toolDef *schema.ToolDef, toolAbs, workspaceRoot, packageRoot string, visited map[string]bool) ([]string, []error) {
	var lines []string
	var errs []error
	// Deterministic action iteration order so any partial-failure error
	// ordering doesn't depend on Go map iteration order.
	actionNames := make([]string, 0, len(toolDef.Actions))
	for name := range toolDef.Actions {
		actionNames = append(actionNames, name)
	}
	sort.Strings(actionNames)
	for _, name := range actionNames {
		action := toolDef.Actions[name]
		if action == nil || action.Execute == nil || !action.Execute.IsSubstitution() {
			continue
		}
		subAbs, _, rerr := pkgpath.ResolveKind(pkgpath.KindExecutePath, toolAbs, action.Execute.Path, workspaceRoot, packageRoot)
		if rerr != nil {
			errs = append(errs, rerr)
			continue
		}
		subLines, subErrs := source.closeIncludeFile(subAbs, workspaceRoot, packageRoot, packageRoot, visited)
		lines = append(lines, subLines...)
		errs = append(errs, subErrs...)
	}
	return lines, errs
}

// closeIncludeFile digests path (unless already visited) and recurses into
// every package-internal include it declares. root is the package root
// used both as the digest line's relpath base and as KindPackageInternal-
// Include's containment root.
func (source *Source) closeIncludeFile(path, workspaceRoot, packageRoot, root string, visited map[string]bool) ([]string, []error) {
	if visited[path] {
		return nil, nil
	}
	visited[path] = true

	digest, derr := source.digest(path)
	if derr != nil {
		return nil, []error{errkit.New("PKG-010", fmt.Sprintf("digest closure: cannot digest %q: %v", path, derr))}
	}
	lines := []string{fmt.Sprintf("%s  %s", digest, relPathPosixNFC(root, path))}

	data, rerr := source.read(path)
	if rerr != nil {
		return lines, []error{errkit.New("PKG-010", fmt.Sprintf("digest closure: cannot read %q: %v", path, rerr))}
	}
	inclPaths, uerr := collectIncludePathsRaw(data)
	if uerr != nil {
		// Not every execute.path target need be a strictly valid runbook
		// for digest purposes at this layer; a parse failure here is
		// surfaced elsewhere (pkgsubst.Plan) with full context. The
		// closure still hashes the file itself (already appended above).
		return lines, nil
	}

	var errs []error
	for _, inclPath := range inclPaths {
		if err := pkgpath.ValidateAuthoredSyntax(inclPath, pkgpath.KindPackageInternalInclude.AllowsAbsolute()); err != nil {
			errs = append(errs, err)
			continue
		}
		inclAbs, _, rerr := pkgpath.ResolveKind(pkgpath.KindPackageInternalInclude, path, inclPath, workspaceRoot, packageRoot)
		if rerr != nil {
			errs = append(errs, rerr)
			continue
		}
		subLines, subErrs := source.closeIncludeFile(inclAbs, workspaceRoot, packageRoot, root, visited)
		lines = append(lines, subLines...)
		errs = append(errs, subErrs...)
	}
	return lines, errs
}

// collectIncludePathsRaw parses data's top-level flow: array as raw YAML
// nodes (never through schema.Runbook/schema.Step's yaml struct tags,
// which inline several type-specific spec structs onto Step and trip
// yaml.v3's duplicate-field detector when decoded generically) and returns
// every include step's runbook: path reachable from it, descending into
// branch arms, iterate/parallel bodies, and compensate bodies.
func collectIncludePathsRaw(data []byte) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	flowNode := yamlMapValue(doc.Content[0], "flow")
	if flowNode == nil || flowNode.Kind != yaml.SequenceNode {
		return nil, nil
	}
	return collectIncludePathsFromFlowNodes(flowNode.Content), nil
}

func collectIncludePathsFromFlowNodes(nodes []*yaml.Node) []string {
	var out []string
	for _, n := range nodes {
		if n == nil || n.Kind != yaml.MappingNode {
			continue
		}
		if stepNode := yamlMapValue(n, "step"); stepNode != nil {
			out = append(out, collectIncludePathsFromStepNode(stepNode)...)
		}
		if iterNode := yamlMapValue(n, "iterate"); iterNode != nil {
			if steps := yamlMapValue(iterNode, "steps"); steps != nil && steps.Kind == yaml.SequenceNode {
				out = append(out, collectIncludePathsFromFlowNodes(steps.Content)...)
			}
		}
		if parNode := yamlMapValue(n, "parallel"); parNode != nil {
			if branches := yamlMapValue(parNode, "branches"); branches != nil && branches.Kind == yaml.SequenceNode {
				for _, b := range branches.Content {
					if steps := yamlMapValue(b, "steps"); steps != nil && steps.Kind == yaml.SequenceNode {
						out = append(out, collectIncludePathsFromFlowNodes(steps.Content)...)
					}
				}
			}
		}
	}
	return out
}

func collectIncludePathsFromStepNode(stepNode *yaml.Node) []string {
	var out []string
	if inclNode := yamlMapValue(stepNode, "include"); inclNode != nil {
		if rbNode := yamlMapValue(inclNode, "runbook"); rbNode != nil {
			out = append(out, rbNode.Value)
		}
	}
	if branchesNode := yamlMapValue(stepNode, "branches"); branchesNode != nil && branchesNode.Kind == yaml.SequenceNode {
		for _, arm := range branchesNode.Content {
			if steps := yamlMapValue(arm, "steps"); steps != nil && steps.Kind == yaml.SequenceNode {
				out = append(out, collectIncludePathsFromFlowNodes(steps.Content)...)
			}
		}
	}
	if compNode := yamlMapValue(stepNode, "compensate"); compNode != nil {
		if steps := yamlMapValue(compNode, "steps"); steps != nil && steps.Kind == yaml.SequenceNode {
			out = append(out, collectIncludePathsFromFlowNodes(steps.Content)...)
		}
	}
	return out
}

// yamlMapValue looks up key in mapping node m, returning nil if m is not a
// mapping or the key is absent.
func yamlMapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// workspaceRelativePOSIX computes root's workspace-relative POSIX form, or
// the "external:sha256:..." form (per §Package map / lock) if root resolves
// outside the workspace.
func workspaceRelativePOSIX(workspaceRoot, root string) string {
	rel, err := filepath.Rel(workspaceRoot, root)
	if err != nil || strings.HasPrefix(rel, "..") {
		abs, _ := filepath.Abs(root)
		digest := sha256Sum([]byte(abs))
		return "external:sha256:" + hex.EncodeToString(digest)
	}
	return filepath.ToSlash(rel)
}

// scanToolDir recursively discovers *.tool.yaml under dir, sorted by
// workspace-relative POSIX path ascending (§Determinism of Enumeration).
func (source *Source) scanToolDir(workspaceRoot, dir string) ([]*Entry, error) {
	if _, err := source.stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type found struct {
		rel  string
		path string
	}
	var files []found
	walkErr := source.walk(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".tool.yaml") && !strings.HasSuffix(name, ".yawt") {
			return nil
		}
		rel, rerr := filepath.Rel(workspaceRoot, path)
		if rerr != nil {
			rel = path
		}
		files = append(files, found{rel: filepath.ToSlash(rel), path: path})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	out := make([]*Entry, 0, len(files))
	for _, f := range files {
		def, err := source.parse(f.path)
		if err != nil {
			if source != nil && source.MetadataOnly {
				return nil, err
			}
			continue // best-effort, matches ScanSchemaDir's tolerant behaviour
		}
		runtimeDef, err := source.runtime(def)
		if err != nil {
			continue
		}
		digest, _ := source.digest(f.path)
		// Tier-2 tools are ad-hoc (not package-exported): they have no
		// package root of their own, so their own directory is both their
		// execute.path referencing base and containment root.
		runtimeDef.SourcePath = f.path
		runtimeDef.PackageRoot = filepath.Dir(f.path)
		out = append(out, &Entry{Qualified: def.Name, Bare: def.Name, Def: runtimeDef, Version: def.Version, SourcePath: f.path, Digest: digest})
	}
	return out, nil
}

// FileDigest returns the sha256 file digest over path's raw bytes, encoded
// as "sha256:" + 64 lowercase hex characters. No normalisation of any kind.
func FileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(sha256Sum(data)), nil
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// CatalogDigest computes the order-independent catalog digest per
// Catalog digest lines use
// "qualifiedName  digest  tierNumber", sorted ascending, concatenated and
// hashed.
func (c *Catalog) CatalogDigest() string {
	lines := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		digest := e.Digest
		if digest == "" {
			digest = "sha256:" + strings.Repeat("0", 64)
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %d", e.Qualified, digest, e.Tier))
	}
	sort.Strings(lines)
	blob := strings.Join(lines, "\n")
	if len(lines) > 0 {
		blob += "\n"
	}
	return "sha256:" + hex.EncodeToString(sha256Sum([]byte(blob)))
}
