package pkgcatalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgpath"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/semver"
)

// RequirementDocument preserves the base and containment of each declaration.
// Locations, when supplied, correspond to Requirements by index.
type RequirementDocument struct {
	Path         string
	PackageRoot  string
	Requirements []*schema.PackageRequirement
	Locations    []RequirementLocation
}

type RequirementLocation struct {
	Line   int `json:"line,omitempty"`
	Column int `json:"column,omitempty"`
}

type RequirementDeclaration struct {
	File       string `json:"file"`
	Index      int    `json:"index"`
	Origin     string `json:"origin"`
	Constraint string `json:"constraint,omitempty"`
	Path       string `json:"path,omitempty"`
	RequirementLocation
}

func (d RequirementDeclaration) String() string {
	location := d.File
	if d.Line > 0 {
		location = fmt.Sprintf("%s:%d:%d", location, d.Line, d.Column)
	}
	return fmt.Sprintf("%s requires[%d] (%s), constraint %q", d.Origin, d.Index, location, d.Constraint)
}

// PackageRequirementResolution is metadata only; it does not load a manifest.
type PackageRequirementResolution struct {
	Requirement  schema.PackageRequirement
	Declarations []RequirementDeclaration
	ResolvedPath string
	External     bool
	kind         pkgpath.Kind
	owner        string
	pinned       *resolvedRequirementPath
}

// ResolveRequirements aggregates a complete discovery round without mutating
// its inputs. Missing paths remain unresolved for the caller's fixed-point
// discovery; malformed syntax and conflicting source claims are always errors.
func ResolveRequirements(opts BuildOptions, documents []RequirementDocument) ([]PackageRequirementResolution, []error) {
	var errs []error
	byName := map[string]*PackageRequirementResolution{}
	groups := []RequirementDocument{{Path: filepath.Join(opts.WorkspaceRoot, ".yawr", "config.yaml"), Requirements: opts.ProjectRequires}}
	files := append([]RequirementDocument(nil), documents...)
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Path == files[j].Path {
			return files[i].PackageRoot < files[j].PackageRoot
		}
		return files[i].Path < files[j].Path
	})
	groups = append(groups, files...)
	for groupIndex, document := range groups {
		origin := "runbook"
		kind := pkgpath.KindRequiresRunbook
		if groupIndex == 0 {
			origin, kind = "project", pkgpath.KindRequiresProject
		} else if document.Path == "" || !filepath.IsAbs(document.Path) {
			errs = append(errs, errkit.New("PKG-007", "requirement document needs an absolute declaring-file path"))
			continue
		} else if document.PackageRoot != "" {
			kind = pkgpath.KindRequiresPackageInternal
			if !filepath.IsAbs(document.PackageRoot) {
				errs = append(errs, errkit.New("PKG-007", "requirement document needs an absolute package root"))
				continue
			}
		}
		seen := map[string]bool{}
		for index, requirement := range document.Requirements {
			if requirement == nil || requirement.Package == "" {
				errs = append(errs, errkit.New("PKG-004", fmt.Sprintf("%s requires[%d]: package identity is required", document.Path, index)))
				continue
			}
			if seen[requirement.Package] {
				errs = append(errs, errkit.New("PKG-005", fmt.Sprintf("%s: duplicate package %q in requires:", document.Path, requirement.Package)))
				continue
			}
			seen[requirement.Package] = true
			site := RequirementDeclaration{File: document.Path, Index: index, Origin: origin,
				Constraint: requirement.Version, Path: requirement.Path}
			if index < len(document.Locations) {
				site.RequirementLocation = document.Locations[index]
			}
			if _, err := semver.ParseConstraint(requirement.Version); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", site, err))
				continue
			}
			resolution := byName[requirement.Package]
			if resolution == nil {
				resolution = &PackageRequirementResolution{Requirement: schema.PackageRequirement{Package: requirement.Package}}
				byName[requirement.Package] = resolution
			}
			resolution.Declarations = append(resolution.Declarations, site)
			if requirement.Version != "" {
				resolution.Requirement.Version = strings.TrimSpace(resolution.Requirement.Version + " " + requirement.Version)
			}
			if requirement.Path == "" {
				continue
			}
			if err := pkgpath.ValidateAuthoredSyntax(requirement.Path, kind.AllowsAbsolute()); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", site, err))
				continue
			}
			resolved, external, err := pkgpath.ResolveKind(kind, document.Path, requirement.Path, opts.WorkspaceRoot, document.PackageRoot)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", site, err))
				continue
			}
			identity, statErr := opts.Source.stat(resolved)
			if statErr != nil && !os.IsNotExist(statErr) {
				errs = append(errs, errkit.Wrap("PKG-008", fmt.Sprintf("%s: cannot inspect package source", site), statErr))
				continue
			}
			if resolution.pinned != nil {
				same := pkgpath.NormalizeForComparison(filepath.Clean(resolved), os.PathSeparator == '\\') ==
					pkgpath.NormalizeForComparison(filepath.Clean(resolution.ResolvedPath), os.PathSeparator == '\\')
				if identity != nil && resolution.pinned.identity != nil {
					same = same || os.SameFile(identity, resolution.pinned.identity)
				}
				if !same {
					sites := make([]string, len(resolution.Declarations))
					for i, declaration := range resolution.Declarations {
						sites[i] = declaration.String() + fmt.Sprintf(", path %q", declaration.Path)
					}
					errs = append(errs, errkit.New("PKG-024", fmt.Sprintf("package %q has conflicting source claims: %s",
						requirement.Package, strings.Join(sites, "; "))))
				}
				continue
			}
			resolution.Requirement.Path = requirement.Path
			resolution.ResolvedPath, resolution.External = resolved, external
			resolution.kind, resolution.owner = kind, document.Path
			resolution.pinned = &resolvedRequirementPath{path: resolved, external: external, identity: identity}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]PackageRequirementResolution, 0, len(names))
	for _, name := range names {
		out = append(out, *byName[name])
	}
	return out, errs
}
