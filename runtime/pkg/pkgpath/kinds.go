package pkgpath

import "path/filepath"

// Kind enumerates the exact path kinds of
// Current path base rules.
// (§Resolution base per path kind). Each kind has its own referencing-file
// (resolution base) and containment-root rule; the two are not always the
// same, which is why Kind exists as a distinct concept from Class.
type Kind int

const (
	// KindRequiresProject is requires[].path declared at project scope
	// (.yawr/config.yaml). referencing_file = the workspace root itself
	// (p is workspace-relative, not relative to .yawr/); containmentRoot =
	// workspace root; workspace-level.
	KindRequiresProject Kind = iota
	// KindRequiresRunbook is requires[].path declared at runbook scope.
	// referencing_file = the declaring runbook file; containmentRoot =
	// workspace root; workspace-level.
	KindRequiresRunbook
	// KindExportsTools is exports.tools[].path. referencing_file =
	// yawr-package.yaml (i.e. the package root); containmentRoot = package
	// root; package-internal.
	KindExportsTools
	// KindExecutePath is execute.path. referencing_file = the declaring
	// .tool.yaml; containmentRoot = package root of the package that
	// exports the declaring tool; package-internal. Resolution base and
	// containment root are deliberately NOT the same directory: a tool
	// nested at tools/subdir/foo.tool.yaml resolves execute.path relative
	// to tools/subdir/, then checks containment against the package root.
	KindExecutePath
	// KindToolRefsPath is toolRefs[].path (tier 4). referencing_file = the
	// declaring runbook file; containmentRoot = workspace root;
	// workspace-level for an ordinary top-level runbook. When the
	// declaring runbook file is itself package-internal (reached only via
	// exports.tools[].path or execute.path, never run directly), the
	// package rule overrides this classification: containmentRoot becomes
	// the package root and the class becomes package-internal. Callers
	// select KindToolRefsPath vs KindToolRefsPathPackageInternal based on
	// that fact.
	KindToolRefsPath
	// KindToolRefsPathPackageInternal is KindToolRefsPath's package-internal
	// override, used when the declaring runbook file is itself reached only
	// via a package (see above).
	KindToolRefsPathPackageInternal
	// KindToolPaths is tool-paths[] (project config). referencing_file =
	// the workspace root itself; containmentRoot = workspace root;
	// workspace-level.
	KindToolPaths
	// KindPackageInternalInclude is an include/import path inside a package
	// runbook. referencing_file = the including file; containmentRoot =
	// package root; package-internal.
	KindPackageInternalInclude
	// KindExportsRunbooks is exports.runbooks[].path. referencing_file =
	// yawr-package.yaml (i.e. the package root); containmentRoot = package
	// root; package-internal. Identical resolution rules to
	// KindExportsTools — an exported runbook is package-internal content
	// published under a stable identity, never an arbitrary path.
	KindExportsRunbooks
	// KindRequiresPackageInternal anchors a package-owned runbook's dependency
	// path at its declaring file without allowing escape from its owner.
	KindRequiresPackageInternal
)

// Class reports the containment class for k.
func (k Kind) Class() Class {
	switch k {
	case KindExportsTools, KindExecutePath, KindToolRefsPathPackageInternal, KindPackageInternalInclude, KindExportsRunbooks, KindRequiresPackageInternal:
		return PackageInternal
	default:
		return WorkspaceLevel
	}
}

// AllowsAbsolute reports whether authored syntax may use a leading "/"
// for this kind (§Syntax rules, rule 2): permitted for workspace-level
// operator configuration kinds, rejected for package-internal references.
func (k Kind) AllowsAbsolute() bool {
	return k.Class() == WorkspaceLevel
}

// ResolveKind resolves p (already syntax-validated via ValidateAuthoredSyntax
// with AllowsAbsolute(k)) using the exact referencing-file/containment-root
// pair Table tab:tool-path-bases assigns to kind k, rather than a heuristic
// or caller-supplied guess.
//
//   - referencingFile is the file that anchors relative resolution for this
//     kind: the workspace root itself for KindRequiresProject/KindToolPaths,
//     the declaring runbook file for KindRequiresRunbook/KindToolRefsPath*,
//     the package's yawr-package.yaml for KindExportsTools, the declaring
//     .tool.yaml for KindExecutePath, or the including file for
//     KindPackageInternalInclude.//   - workspaceRoot and packageRoot are the two possible containment roots;
//     only the one k.Class() selects is used.
func ResolveKind(k Kind, referencingFile, p, workspaceRoot, packageRoot string) (resolved string, external bool, err error) {
	referencingDir := referencingFile
	if referencingFile != "" {
		referencingDir = filepath.Dir(referencingFile)
	}
	// KindRequiresProject and KindToolPaths anchor directly at the
	// workspace root, not at a dirname of it.
	if k == KindRequiresProject || k == KindToolPaths {
		referencingDir = workspaceRoot
	}

	containmentRoot := workspaceRoot
	if k.Class() == PackageInternal {
		containmentRoot = packageRoot
	}
	return Resolve(referencingDir, p, containmentRoot, k.Class())
}
