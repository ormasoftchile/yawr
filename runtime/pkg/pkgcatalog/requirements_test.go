package pkgcatalog

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestRequirementDocumentsIntersectWithFileProvenance(t *testing.T) {
	ws := t.TempDir()
	writeAcmePackage(t, filepath.Join(ws, "vendor", "acme"))
	parent := RequirementDocument{Path: filepath.Join(ws, "runbooks", "parent.yaml"),
		Requirements: []*schema.PackageRequirement{{Package: "acme.incident-tools", Version: ">=1.0.0", Path: "../vendor/acme"}}}
	child := RequirementDocument{Path: filepath.Join(ws, "children", "child.yaml"),
		Requirements: []*schema.PackageRequirement{{Package: "acme.incident-tools", Version: "<2.0.0", Path: "../vendor/acme"}},
		Locations:    []RequirementLocation{{Line: 5, Column: 3}}}
	opts := BuildOptions{WorkspaceRoot: ws, RequirementDocuments: []RequirementDocument{parent, child}, LexicalBindings: true}
	catalog, errs := Build(opts)
	if fatal, _ := errkit.SplitWarnings(errs); len(fatal) != 0 {
		t.Fatal(fatal)
	}
	if len(catalog.Packages) != 1 || len(catalog.Packages[0].ConstraintSources) != 2 {
		t.Fatalf("lost declaration provenance: %+v", catalog.Packages)
	}
	if !strings.Contains(strings.Join(catalog.Packages[0].ConstraintSources, ";"), "child.yaml:5:3") {
		t.Fatal("missing child source location")
	}
	opts.RequirementDocuments = []RequirementDocument{child, parent}
	reordered, errs := Build(opts)
	if len(errs) != 0 || reordered.CatalogDigest() != catalog.CatalogDigest() {
		t.Fatalf("order-dependent result: %v", errs)
	}
	if parent.Requirements[0].Version != ">=1.0.0" || child.Requirements[0].Path != "../vendor/acme" {
		t.Fatal("aggregation mutated caller-owned declarations")
	}
}

func TestRequirementDocumentsRejectConstraintAndSourceConflicts(t *testing.T) {
	for _, conflict := range []string{"version", "source"} {
		t.Run(conflict, func(t *testing.T) {
			ws := t.TempDir()
			writeAcmePackage(t, filepath.Join(ws, "one"))
			writeAcmePackage(t, filepath.Join(ws, "two"))
			child := &schema.PackageRequirement{Package: "acme.incident-tools", Version: "^1.0.0", Path: "../one"}
			code := "PKG-002"
			if conflict == "version" {
				child.Version = "^2.0.0"
			} else {
				child.Path, code = "../two", "PKG-024"
			}
			_, errs := Build(BuildOptions{WorkspaceRoot: ws, ProjectRequires: []*schema.PackageRequirement{
				{Package: "acme.incident-tools", Version: "^1.0.0", Path: "one"},
			}, RequirementDocuments: []RequirementDocument{{Path: filepath.Join(ws, "child", "runbook.yaml"),
				Requirements: []*schema.PackageRequirement{child}}}})
			text := ""
			for _, err := range errs {
				text += err.Error() + "\n"
			}
			if !strings.Contains(text, code) || !strings.Contains(text, "runbook.yaml") || !strings.Contains(text, "project") {
				t.Fatalf("expected %s with every owner: %s", code, text)
			}
		})
	}
}

func TestRequirementDocumentsDoNotRebaseChildPaths(t *testing.T) {
	ws := t.TempDir()
	writeAcmePackage(t, filepath.Join(ws, "nested", "vendor"))
	_, errs := Build(BuildOptions{WorkspaceRoot: ws, RequirementDocuments: []RequirementDocument{{
		Path:         filepath.Join(ws, "nested", "runbook.yaml"),
		Requirements: []*schema.PackageRequirement{{Package: "acme.incident-tools", Version: "^1.0.0", Path: "vendor"}},
	}}})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
}

func TestRequirementDocumentsRejectPackageOwnedDependencyEscape(t *testing.T) {
	ws := t.TempDir()
	writeAcmePackage(t, filepath.Join(ws, "outside"))
	_, errs := ResolveRequirements(BuildOptions{WorkspaceRoot: ws}, []RequirementDocument{{
		Path: filepath.Join(ws, "owner", "child.yaml"), PackageRoot: filepath.Join(ws, "owner"),
		Requirements: []*schema.PackageRequirement{{Package: "acme.incident-tools", Version: "^1.0.0", Path: "../outside"}},
	}})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "PKG-007") {
		t.Fatalf("expected package containment refusal: %v", errs)
	}
}

func TestRequirementDocumentsPreserveUnresolvedAndRejectDuplicates(t *testing.T) {
	ws := t.TempDir()
	req := &schema.PackageRequirement{Package: "missing", Version: "^1.0.0"}
	opts := BuildOptions{WorkspaceRoot: ws}
	doc := RequirementDocument{Path: filepath.Join(ws, "child.yaml"), Requirements: []*schema.PackageRequirement{req}}
	got, errs := ResolveRequirements(opts, []RequirementDocument{doc})
	if len(errs) != 0 || len(got) != 1 || got[0].ResolvedPath != "" {
		t.Fatalf("discovery must retain unresolved identity: %v, %v", got, errs)
	}
	doc.Requirements = append(doc.Requirements, req)
	_, errs = ResolveRequirements(opts, []RequirementDocument{doc})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "PKG-005") {
		t.Fatalf("expected duplicate-in-file refusal: %v", errs)
	}
}

func TestLexicalCatalogAllowsExplicitPackageDisambiguation(t *testing.T) {
	ws := t.TempDir()
	for _, name := range []string{"left", "right"} {
		root := filepath.Join(ws, name)
		writeFile(t, filepath.Join(root, schema.PackageManifestFilename),
			"apiVersion: yawr.tool-package/v1\nmeta: {name: "+name+", version: \"1.0.0\"}\n"+
				"exports:\n  tools: [{id: kubectl, path: kubectl.tool.yaml}]\n")
		writeFile(t, filepath.Join(root, "kubectl.tool.yaml"), kubectlToolYAML)
	}
	opts := BuildOptions{WorkspaceRoot: ws, LexicalBindings: true, ProjectRequires: []*schema.PackageRequirement{
		{Package: "left", Version: "^1.0.0", Path: "left"}, {Package: "right", Version: "^1.0.0", Path: "right"},
	}}
	catalog, errs := Build(opts)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, name := range []string{"left", "right"} {
		bound, errs := BindFile(catalog, filepath.Join(ws, name+".yaml"),
			[]*schema.ToolRef{{Name: "kubectl", Package: name}}, "")
		if len(errs) != 0 || len(bound) != 1 || bound[0].Def.PackageName != name {
			t.Fatalf("wrong lexical binding for %s: %v %v", name, bound, errs)
		}
	}
	_, errs = BindFile(catalog, filepath.Join(ws, "bare.yaml"), []*schema.ToolRef{{Name: "kubectl"}}, "")
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "PKG-006") {
		t.Fatalf("bare ambiguity was lost: %v", errs)
	}
	opts.LexicalBindings = false
	_, legacy := Build(opts)
	if len(legacy) != 1 || !strings.Contains(legacy[0].Error(), "PKG-006") {
		t.Fatalf("legacy construction changed: %v", legacy)
	}
	if !reflect.DeepEqual(opts.ProjectRequires[0], &schema.PackageRequirement{Package: "left", Version: "^1.0.0", Path: "left"}) {
		t.Fatal("catalog mutated input")
	}
}

func TestLexicalCatalogPathBindingDisambiguatesBuiltinProjectNames(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "tools", "kubectl.tool.yaml"), kubectlToolYAML)
	catalog, errs := Build(BuildOptions{WorkspaceRoot: ws, LexicalBindings: true,
		Builtins: []toolpkg.ToolDef{{Name: "kubectl", Source: "builtin://kubectl"}}})
	if len(errs) != 0 {
		t.Fatalf("bare collision should be deferred to binding: %v", errs)
	}
	bindings, errs := BindFile(catalog, filepath.Join(ws, "child.runbook.yaml"),
		[]*schema.ToolRef{{Name: "kubectl", Path: "tools/kubectl.tool.yaml"}}, "")
	if len(errs) != 0 || len(bindings) != 1 || bindings[0].Def.Command != "kubectl" {
		t.Fatalf("explicit path did not disambiguate: %v %v", bindings, errs)
	}
	_, errs = BindFile(catalog, filepath.Join(ws, "child.runbook.yaml"), []*schema.ToolRef{{Name: "kubectl"}}, "")
	if !hasCode(errs, "PKG-022") {
		t.Fatalf("unqualified cross-tier ambiguity was ignored: %v", errs)
	}
}

func TestLexicalCatalogQualifiedCollisionsDoNotMutateCallerEntries(t *testing.T) {
	entry := &Entry{Qualified: "extension/query", Bare: "query", Tier: TierProject, Def: toolpkg.ToolDef{Name: "query"}}
	copy := *entry
	_, errs := Build(BuildOptions{WorkspaceRoot: t.TempDir(), LexicalBindings: true, Dynamic: []*Entry{entry, &copy}})
	if !hasCode(errs, "PKG-006") {
		t.Fatalf("qualified collision did not fail: %v", errs)
	}
	if entry.Tier != TierProject || entry.Bare != "query" || copy.Tier != TierProject || copy.Bare != "query" {
		t.Fatalf("preflight mutated caller entries: %+v %+v", entry, copy)
	}
}
