package adapter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
)

// catalogIncludeResolver implements executor.DynamicIncludeResolver using
// the frozen pkgcatalog.Catalog as the sole resolution source.
type catalogIncludeResolver struct {
	catalog *pkgcatalog.Catalog
	parser  parserpkg.Parser
}

// NewCatalogIncludeResolver constructs a DynamicIncludeResolver backed by
// the given frozen catalog and parser.
func NewCatalogIncludeResolver(catalog *pkgcatalog.Catalog, p parserpkg.Parser) internalexecutor.DynamicIncludeResolver {
	return &catalogIncludeResolver{catalog: catalog, parser: p}
}

func (r *catalogIncludeResolver) Resolve(ctx context.Context, renderedRef string) (*internalexecutor.DynamicIncludeResult, error) {
	// Syntactic validation uses the package reference validator.
	reason, kind := pkgcatalog.ValidateRenderedRef(renderedRef)
	switch kind {
	case pkgcatalog.RefValidationEmpty:
		return nil, errkit.New("DINC-002",
			fmt.Sprintf("dynamic include: empty rendered ref %q", renderedRef))
	case pkgcatalog.RefValidationInvalid:
		return nil, errkit.New("DINC-001",
			fmt.Sprintf("dynamic include: rendered ref %q is not a valid runbook identity: %s", renderedRef, reason))
	}

	// Catalog lookup: qualified (contains "/") or bare.
	var entry *pkgcatalog.RunbookEntry
	if strings.Contains(renderedRef, "/") {
		e, ok := r.catalog.RunbookByQualified(renderedRef)
		if !ok {
			return nil, errkit.New("DINC-002",
				fmt.Sprintf("dynamic include: runbook %q not found in catalog", renderedRef))
		}
		entry = e
	} else {
		entries := r.catalog.RunbookByBare(renderedRef)
		switch len(entries) {
		case 0:
			return nil, errkit.New("DINC-002",
				fmt.Sprintf("dynamic include: runbook %q not found in catalog", renderedRef))
		case 1:
			entry = entries[0]
		default:
			candidates := make([]string, len(entries))
			for i, e := range entries {
				candidates[i] = e.Qualified
			}
			return nil, errkit.New("DINC-003",
				fmt.Sprintf("dynamic include: bare id %q is ambiguous in catalog (candidates: %v); use a package-qualified ref",
					renderedRef, candidates))
		}
	}

	// Read once, verify the frozen catalog digest, and parse those same bytes.
	// Reopening by path between verification and parsing would permit content
	// drift to execute under the catalog's original digest.
	source, err := os.ReadFile(entry.Path)
	if err != nil {
		return nil, errkit.Wrap("DINC-013",
			fmt.Sprintf("dynamic include: resolved runbook %q at path %q is missing or unreadable",
				entry.Qualified, entry.Path), err)
	}
	actualDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(source))
	if actualDigest != entry.FileDigest {
		return nil, errkit.New("DINC-013", fmt.Sprintf(
			"dynamic include: resolved runbook %q content changed after catalog freeze", entry.Qualified,
		))
	}
	if r.parser == nil {
		return nil, errkit.New("DINC-013", fmt.Sprintf("dynamic include: no parser for resolved runbook %q", entry.Qualified))
	}

	// Parse the child runbook.
	parsed, parseErr := r.parser.ParseBytes(ctx, source)
	if parseErr != nil {
		return nil, errkit.Wrap("DINC-013",
			fmt.Sprintf("dynamic include: parse error for resolved runbook %q at path %q",
				entry.Qualified, entry.Path), parseErr)
	}
	if parsed == nil || parsed.Runbook == nil {
		return nil, errkit.New("DINC-013",
			fmt.Sprintf("dynamic include: resolved runbook %q parsed to nil", entry.Qualified))
	}
	parsed.Source = entry.Path
	stampLazyIncludes(parsed.Runbook.Flow, filepath.Dir(entry.Path))
	contentHash, hashErr := graphdoc.RunbookContentHash(parsed.Runbook)
	if hashErr != nil {
		return nil, errkit.Wrap("DINC-013", "dynamic include: hash resolved runbook definition", hashErr)
	}

	// Validate child package requirements against the frozen catalog (DINC-010).
	for _, req := range parsed.Runbook.Requires {
		if !r.packageInCatalog(req.Package) {
			return nil, errkit.New("DINC-010",
				fmt.Sprintf("dynamic include: child runbook %q requires package %q which is not in the frozen catalog",
					entry.Qualified, req.Package))
		}
	}

	// Validate child tool references against the frozen catalog (DINC-011).
	for _, toolRef := range parsed.Runbook.ToolRefs {
		if !r.toolInCatalog(toolRef.Name) {
			return nil, errkit.New("DINC-011",
				fmt.Sprintf("dynamic include: child runbook %q references tool %q which is not in the frozen catalog",
					entry.Qualified, toolRef.Name))
		}
	}

	return &internalexecutor.DynamicIncludeResult{
		Flow:            parsed.Runbook.Flow,
		QualifiedID:     entry.Qualified,
		RunbookID:       parsed.Runbook.ID,
		RunbookName:     parsed.Runbook.Name,
		ContentHash:     contentHash,
		AbsPath:         entry.Path,
		PackageName:     entry.PackageName,
		PackageVersion:  entry.PackageVersion,
		FileDigest:      entry.FileDigest,
		PackageDigest:   entry.PackageDigest,
		ChildInputs:     parsed.Runbook.Inputs,
		ChildBindings:   parsed.Runbook.Bindings,
		ChildOutputs:    parsed.Runbook.Outputs,
		ChildGovernance: parsed.Runbook.Governance,
	}, nil
}

// packageInCatalog reports whether a package with the given name exists in
// the frozen catalog's locked package list.
func (r *catalogIncludeResolver) packageInCatalog(pkgName string) bool {
	for i := range r.catalog.Packages {
		if r.catalog.Packages[i].Name == pkgName {
			return true
		}
	}
	return false
}

// toolInCatalog reports whether a tool name is resolvable in the frozen catalog.
func (r *catalogIncludeResolver) toolInCatalog(toolName string) bool {
	if strings.Contains(toolName, "/") {
		_, ok := r.catalog.ByQualified(toolName)
		return ok
	}
	return len(r.catalog.ByBare(toolName)) > 0
}

// Ensure catalogIncludeResolver satisfies the interface at compile time.
var _ internalexecutor.DynamicIncludeResolver = (*catalogIncludeResolver)(nil)

// ResolverProxy is a DynamicIncludeResolver that forwards to a delegate
// set after construction. This allows a resolver to be registered with
// BuildEngineConfig before the frozen catalog is available (e.g. before
// the planner runs Phase C in cmd/yawr/run.go).
type ResolverProxy struct {
	delegate internalexecutor.DynamicIncludeResolver
}

// NewResolverProxy constructs an unset proxy. Call Set once the frozen
// catalog is available.
func NewResolverProxy() *ResolverProxy { return &ResolverProxy{} }

// Set wires the real resolver. Not safe for concurrent use; callers must
// call Set before execution begins.
func (p *ResolverProxy) Set(r internalexecutor.DynamicIncludeResolver) { p.delegate = r }

func (p *ResolverProxy) Resolve(ctx context.Context, renderedRef string) (*internalexecutor.DynamicIncludeResult, error) {
	if p.delegate == nil {
		return nil, fmt.Errorf("dynamic include: no catalog resolver configured for this run")
	}
	return p.delegate.Resolve(ctx, renderedRef)
}

var _ internalexecutor.DynamicIncludeResolver = (*ResolverProxy)(nil)
