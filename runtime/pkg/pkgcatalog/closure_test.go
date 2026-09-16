package pkgcatalog

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func closureOptions(t *testing.T, workspace string) ClosureOptions {
	t.Helper()
	parser, err := internalparser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatal(err)
	}
	return ClosureOptions{Catalog: BuildOptions{WorkspaceRoot: workspace}, Entrypoint: filepath.Join(workspace, "root.runbook.yaml"), Parser: parser}
}

func closureRunbook(body string) string {
	return "apiVersion: yawr.runbook/v1\nid: closure-test\nname: Closure test\n" + body
}

func closureDocument(t *testing.T, closure *DependencyClosure, path string) *DependencyDocument {
	t.Helper()
	key, err := closurePath(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range closure.Documents {
		if document.Path == key {
			return document
		}
	}
	t.Fatalf("no captured document %s", path)
	return nil
}

func requireClosure(t *testing.T, options ClosureOptions) *DependencyClosure {
	t.Helper()
	closure, errs := BuildClosure(context.Background(), options)
	fatal, _ := errkit.SplitWarnings(errs)
	if len(fatal) != 0 || closure == nil {
		t.Fatalf("BuildClosure: %v", errs)
	}
	return closure
}

func TestClosureStaticModesDiscoverChildDeclarations(t *testing.T) {
	for _, expansion := range []string{"eager", "lazy", "auto"} {
		t.Run(expansion, func(t *testing.T) {
			ws := t.TempDir()
			options := closureOptions(t, ws)
			child := filepath.Join(ws, "nested", "child.runbook.yaml")
			writeFile(t, options.Entrypoint, closureRunbook(fmt.Sprintf(`flow:
  - step: {id: enter, type: include, include: {runbook: nested/child.runbook.yaml, expand: %s}}
  - step: {id: done, type: noop}
`, expansion)))
			writeAcmePackage(t, filepath.Join(ws, "nested", "vendor"))
			writeFile(t, child, closureRunbook(`requires:
  - {package: acme.incident-tools, version: "^1.0.0", path: vendor}
toolRefs:
  - {name: kubectl, package: acme.incident-tools}
flow:
  - step: {id: query, type: tool, tool: {name: kubectl, action: drain, args: {node: fixture}}}
`))
			closure := requireClosure(t, options)
			if len(closure.Documents) != 2 || len(closure.Catalog.Packages) != 1 {
				t.Fatalf("incomplete static closure: %#v", closure)
			}
			root := closureDocument(t, closure, options.Entrypoint)
			nested := closureDocument(t, closure, child)
			if len(root.Bindings) != 0 || len(nested.Bindings) != 1 ||
				nested.Bindings[0].Def.PackageName != "acme.incident-tools" ||
				len(root.Includes) != 1 || root.Includes[0].Target != nested.ID {
				t.Fatalf("scope leakage or lost include: root=%+v child=%+v", root, nested)
			}
			if !strings.Contains(closure.Catalog.Packages[0].ConstraintSources[0], ":5:") {
				t.Fatalf("missing YAML declaration location: %v", closure.Catalog.Packages[0].ConstraintSources)
			}
		})
	}
}

func TestClosureNeverInheritsParentAliases(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	writeAcmePackage(t, filepath.Join(ws, "vendor"))
	writeFile(t, options.Entrypoint, closureRunbook(`requires:
  - {package: acme.incident-tools, version: "^1.0.0", path: vendor}
toolRefs:
  - {name: kubectl, package: acme.incident-tools}
flow:
  - step: {id: enter, type: include, include: {runbook: child.runbook.yaml}}
`))
	child := filepath.Join(ws, "child.runbook.yaml")
	for _, name := range []string{"kubectl", "acme.incident-tools/kubectl"} {
		writeFile(t, child, closureRunbook(fmt.Sprintf(`flow:
  - step: {id: query, type: tool, tool: {name: %s, action: drain, args: {node: fixture}}}
`, name)))
		closure, errs := BuildClosure(context.Background(), options)
		if name == "kubectl" {
			if closure != nil || !hasCode(errs, "SCOPE-001") {
				t.Fatalf("child inherited parent alias: %v %v", closure, errs)
			}
		} else if closure == nil || len(errs) != 0 {
			t.Fatalf("qualified child call failed: %v", errs)
		}
	}
}

func TestClosureDynamicEligibilityReachesDependencyFixedPoint(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	writeFile(t, options.Entrypoint, closureRunbook(`requires:
  - {package: first, version: "^1.0.0", path: first}
flow:
  - step: {id: enter, type: include, include: {runbook_ref: "first/child", resolve_from: catalog}}
`))
	writeFile(t, filepath.Join(ws, "first", "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: first, version: "1.0.0"}
exports:
  runbooks: [{id: child, path: child.runbook.yaml}]
`)
	writeFile(t, filepath.Join(ws, "first", "child.runbook.yaml"), closureRunbook(`requires:
  - {package: second, version: "^1.0.0", path: vendor}
flow:
  - step: {id: done, type: noop}
`))
	writeFile(t, filepath.Join(ws, "first", "vendor", "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: second, version: "1.0.0"}
exports:
  runbooks: [{id: leaf, path: leaf.runbook.yaml}]
`)
	writeFile(t, filepath.Join(ws, "first", "vendor", "leaf.runbook.yaml"), closureRunbook(`flow:
  - step: {id: done, type: noop}
`))
	closure := requireClosure(t, options)
	if len(closure.Documents) != 3 || len(closure.Catalog.Packages) != 2 || len(closure.Targets) != 2 ||
		closure.Targets["second/leaf"] == "" || closure.Targets["first/child"] == "" {
		t.Fatalf("fixed point did not include newly eligible exports: %+v", closure)
	}
}

func TestClosureDynamicRejectsInvalidUnselectedExportOnlyWhenEligible(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	writeFile(t, filepath.Join(ws, "package", "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: fixture, version: "1.0.0"}
exports:
  runbooks:
    - {id: selected, path: selected.runbook.yaml}
    - {id: unselected, path: unselected.runbook.yaml}
`)
	writeFile(t, filepath.Join(ws, "package", "selected.runbook.yaml"), closureRunbook("flow:\n  - step: {id: done, type: noop}\n"))
	writeFile(t, filepath.Join(ws, "package", "unselected.runbook.yaml"), closureRunbook(`requires:
  - {package: absent, version: "^1.0.0"}
flow:
  - step: {id: done, type: noop}
`))
	header := `requires:
  - {package: fixture, version: "^1.0.0", path: package}
flow:
`
	writeFile(t, options.Entrypoint, closureRunbook(header+"  - step: {id: done, type: noop}\n"))
	static := requireClosure(t, options)
	if len(static.Documents) != 1 {
		t.Fatalf("static-only preflight traversed unused exports: %d", len(static.Documents))
	}
	writeFile(t, options.Entrypoint, closureRunbook(header+
		`  - step: {id: enter, type: include, include: {runbook_ref: "fixture/selected", resolve_from: catalog}}`+"\n"))
	closure, errs := BuildClosure(context.Background(), options)
	if closure != nil || !hasCode(errs, "PKG-001") {
		t.Fatalf("invalid unselected candidate allowed execution: %v %v", closure, errs)
	}
}

func TestClosureSnapshotReadsOnceAndLoadsFreshTrees(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	child := filepath.Join(ws, "child.runbook.yaml")
	writeFile(t, options.Entrypoint, closureRunbook(`flow:
  - step: {id: left, type: include, include: {runbook: child.runbook.yaml}}
  - step: {id: right, type: include, include: {runbook: child.runbook.yaml}}
`))
	writeFile(t, child, closureRunbook("flow:\n  - step: {id: original, type: noop}\n"))
	reads := map[string]int{}
	options.Catalog.Source = &Source{ReadFile: func(path string) ([]byte, error) {
		reads[path]++
		return os.ReadFile(path)
	}, MetadataOnly: true}
	closure := requireClosure(t, options)
	if len(closure.Documents) != 2 || len(closureDocument(t, closure, options.Entrypoint).Includes) != 2 {
		t.Fatal("diamond expansion lost occurrences or failed to deduplicate the definition")
	}
	for path, count := range reads {
		if count != 1 {
			t.Fatalf("source %s read %d times", path, count)
		}
	}
	writeFile(t, child, closureRunbook("flow:\n  - step: {id: edited, type: noop}\n"))
	first, err := closure.Load(context.Background(), child)
	if err != nil || first.Runbook.Flow[0].Step.ID != "original" {
		t.Fatalf("frozen load consulted edited source: %v %v", first, err)
	}
	if first.Source != child {
		t.Fatalf("canonical cache identity changed the requested display path: got %q, want %q", first.Source, child)
	}
	first.Runbook.Flow[0].Step.ID = "planner-mutated"
	second, err := closure.Load(context.Background(), child)
	if err != nil || second.Runbook.Flow[0].Step.ID != "original" {
		t.Fatalf("planner mutation escaped its parse tree: %v %v", second, err)
	}
	if _, err := closure.Load(context.Background(), filepath.Join(ws, "outside.runbook.yaml")); !errors.Is(err, errkit.ErrSCOPE002) {
		t.Fatalf("unknown source load did not refuse: %v", err)
	}
	unseen := filepath.Join(ws, "new-source.runbook.yaml")
	writeFile(t, unseen, closureRunbook("flow:\n  - step: {id: new, type: noop}\n"))
	if _, err := closure.snapshot.read(unseen); !errors.Is(err, errkit.ErrSCOPE002) {
		t.Fatalf("captured catalog accepted new bytes after sealing: %v", err)
	}
	if _, err := closure.snapshot.stat(unseen); !errors.Is(err, errkit.ErrSCOPE002) {
		t.Fatalf("captured catalog accepted new source metadata after sealing: %v", err)
	}
	if len(reads) != 2 {
		t.Fatalf("frozen lookup performed a new source read: %v", reads)
	}
	if next := requireClosure(t, options); next.Digest == closure.Digest {
		t.Fatal("new preflight failed to capture edited source")
	}
}

func TestClosureCancellationAndIncludeCycle(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closure, errs := BuildClosure(ctx, options)
	if closure != nil || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("canceled closure: %v %v", closure, errs)
	}
	writeFile(t, options.Entrypoint, closureRunbook(`flow:
  - step: {id: recurse, type: include, include: {runbook: root.runbook.yaml, expand: lazy}}
`))
	closure, errs = BuildClosure(context.Background(), options)
	if closure != nil || len(errs) != 1 || !errors.Is(errs[0], flowwalk.ErrIncludeCycle) {
		t.Fatalf("lazy cycle was not preflighted: %v %v", closure, errs)
	}
}

func TestClosureBuiltinAncestorMigrationGuard(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	options.Catalog.Builtins = []toolpkg.ToolDef{{Name: "kubectl", Source: "builtin://kubectl", Actions: map[string]*toolpkg.ToolAction{"drain": {}}}}
	writeAcmePackage(t, filepath.Join(ws, "vendor"))
	writeFile(t, options.Entrypoint, closureRunbook(`requires:
  - {package: acme.incident-tools, version: "^1.0.0", path: vendor}
toolRefs:
  - {name: kubectl, package: acme.incident-tools}
flow:
  - step: {id: enter, type: include, include: {runbook: child.runbook.yaml}}
`))
	child := filepath.Join(ws, "child.runbook.yaml")
	writeFile(t, child, closureRunbook(`flow:
  - step: {id: query, type: tool, tool: {name: kubectl, action: drain, args: {node: fixture}}}
`))
	closure, errs := BuildClosure(context.Background(), options)
	if closure != nil || !hasCode(errs, "SCOPE-001") || !strings.Contains(fmt.Sprint(errs), "ancestor") {
		t.Fatalf("silently rebound ancestor alias to builtin: %v %v", closure, errs)
	}
	writeFile(t, child, closureRunbook(`toolRefs:
  - {name: kubectl, package: acme.incident-tools}
flow:
  - step: {id: query, type: tool, tool: {name: kubectl, action: drain, args: {node: fixture}}}
`))
	requireClosure(t, options)
}

func TestClosureSubstitutionDiscoversRunbookRequirements(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	writeFile(t, options.Entrypoint, closureRunbook(`toolRefs:
  - {name: investigate, path: local/investigate.tool.yaml}
flow:
  - step: {id: inspect, type: tool, tool: {name: investigate, action: run}}
`))
	writeFile(t, filepath.Join(ws, "local", "investigate.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: investigate, version: "1.0.0"}
transport: {mode: native, command: must-not-execute}
actions:
  - name: run
    outputs: {result: {type: string}}
    execute: {kind: runbook, path: child.runbook.yaml}
`)
	child := filepath.Join(ws, "local", "child.runbook.yaml")
	writeFile(t, child, closureRunbook(`requires:
  - {package: acme.incident-tools, version: "^1.0.0", path: vendor}
toolRefs:
  - {name: kubectl, package: acme.incident-tools}
outputs: {result: {type: string, value: fixture}}
flow:
  - step: {id: query, type: tool, tool: {name: kubectl, action: drain, args: {node: fixture}}}
`))
	writeAcmePackage(t, filepath.Join(ws, "local", "vendor"))
	closure := requireClosure(t, options)
	root := closureDocument(t, closure, options.Entrypoint)
	nested := closureDocument(t, closure, child)
	if len(root.Substitutions) != 1 || root.Substitutions[0].Target != nested.ID ||
		len(nested.Bindings) != 1 || nested.Bindings[0].Def.PackageName != "acme.incident-tools" {
		t.Fatalf("substitution dependencies not closed: root=%+v child=%+v", root, nested)
	}
}

func TestClosureDefersPathlessRequirementUntilDescendantSuppliesSource(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	options.Catalog.ProjectRequires = []*schema.PackageRequirement{{Package: "acme.incident-tools", Version: "^1.0.0"}}
	writeFile(t, options.Entrypoint, closureRunbook(`flow:
  - step: {id: enter, type: include, include: {runbook: child.runbook.yaml}}
`))
	writeFile(t, filepath.Join(ws, "child.runbook.yaml"), closureRunbook(`requires:
  - {package: acme.incident-tools, version: "^1.0.0", path: vendor}
flow:
  - step: {id: done, type: noop}
`))
	writeAcmePackage(t, filepath.Join(ws, "vendor"))
	closure := requireClosure(t, options)
	if len(closure.Catalog.Packages) != 1 || len(closure.Catalog.Packages[0].ConstraintSources) != 2 {
		t.Fatalf("pathless requirement lost provenance: %+v", closure.Catalog.Packages)
	}
}

func TestClosureParallelSiblingsKeepDistinctBindings(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	writeFile(t, options.Entrypoint, closureRunbook(`flow:
  - parallel:
      id: lanes
      branches:
        - label: left
          steps:
            - step: {id: enter_left, type: include, include: {runbook: left.runbook.yaml}}
        - label: right
          steps:
            - step: {id: enter_right, type: include, include: {runbook: right.runbook.yaml}}
`))
	for _, name := range []string{"left", "right"} {
		writeFile(t, filepath.Join(ws, name, "yawr-package.yaml"), fmt.Sprintf(`apiVersion: yawr.tool-package/v1
meta: {name: %s, version: "1.0.0"}
exports:
  tools: [{id: kubectl, path: kubectl.tool.yaml}]
`, name))
		writeFile(t, filepath.Join(ws, name, "kubectl.tool.yaml"),
			strings.Replace(kubectlToolYAML, "command: kubectl", "command: "+name+"-must-not-execute", 1))
		writeFile(t, filepath.Join(ws, name+".runbook.yaml"), closureRunbook(fmt.Sprintf(`requires:
  - {package: %s, version: "^1.0.0", path: %s}
toolRefs:
  - {name: kubectl, package: %s}
flow:
  - step: {id: query, type: tool, tool: {name: kubectl, action: drain, args: {node: fixture}}}
`, name, name, name)))
	}
	closure := requireClosure(t, options)
	if len(closureDocument(t, closure, options.Entrypoint).Includes) != 2 {
		t.Fatal("parallel include sites were not discovered")
	}
	for _, name := range []string{"left", "right"} {
		doc := closureDocument(t, closure, filepath.Join(ws, name+".runbook.yaml"))
		if len(doc.Bindings) != 1 || doc.Bindings[0].Def.Command != name+"-must-not-execute" {
			t.Fatalf("%s resolved against sibling binding: %+v", name, doc.Bindings)
		}
	}
}

func TestClosureDepthIncludesCachedDiamondDescendants(t *testing.T) {
	for _, chainLength := range []int{8, 9} {
		t.Run(fmt.Sprint(chainLength), func(t *testing.T) {
			ws := t.TempDir()
			options := closureOptions(t, ws)
			writeFile(t, options.Entrypoint, closureRunbook(`flow:
  - step: {id: direct, type: include, include: {runbook: shared.runbook.yaml}}
  - step: {id: nested, type: include, include: {runbook: chain-0.runbook.yaml}}
`))
			writeFile(t, filepath.Join(ws, "shared.runbook.yaml"), closureRunbook(`flow:
  - step: {id: leaf, type: include, include: {runbook: leaf.runbook.yaml}}
`))
			writeFile(t, filepath.Join(ws, "leaf.runbook.yaml"), closureRunbook("flow:\n  - step: {id: done, type: noop}\n"))
			for index := 0; index < chainLength; index++ {
				next := "shared.runbook.yaml"
				if index+1 < chainLength {
					next = fmt.Sprintf("chain-%d.runbook.yaml", index+1)
				}
				writeFile(t, filepath.Join(ws, fmt.Sprintf("chain-%d.runbook.yaml", index)), closureRunbook(fmt.Sprintf(
					"flow:\n  - step: {id: next, type: include, include: {runbook: %s, expand: lazy}}\n", next)))
			}
			closure, errs := BuildClosure(context.Background(), options)
			if chainLength == 8 {
				if closure == nil || len(errs) != 0 {
					t.Fatalf("depth %d must be allowed: %v", flowwalk.DefaultMaxIncludeDepth, errs)
				}
			} else if closure != nil || len(errs) != 1 || !errors.Is(errs[0], flowwalk.ErrMaxDepthExceeded) {
				t.Fatalf("cached descendant bypassed depth guard: %v %v", closure, errs)
			}
		})
	}
}

func TestClosureSourceBudgetBoundaries(t *testing.T) {
	for _, budget := range []string{"files", "bytes", "overlay-bytes"} {
		t.Run(budget, func(t *testing.T) {
			ws := t.TempDir()
			path := filepath.Join(ws, "source.yaml")
			writeFile(t, path, "abc")
			source := &closureSource{ctx: context.Background(), files: map[string]closureFile{}}
			switch budget {
			case "files":
				for index := 0; index < MaxClosureDocuments-1; index++ {
					source.files[fmt.Sprint(index)] = closureFile{}
				}
			case "bytes":
				source.bytes = MaxClosureBytes - 3
			case "overlay-bytes":
				source.bytes = MaxClosureBytes - 3
				source.base = &Source{ReadFile: func(string) ([]byte, error) { return []byte("abcd"), nil }}
			}
			data, err := source.read(path)
			if budget == "overlay-bytes" {
				if len(data) != 0 || !errors.Is(err, errkit.ErrSCOPE003) {
					t.Fatalf("overlay exceeded remaining byte budget: %q %v", data, err)
				}
				return
			}
			if err != nil || string(data) != "abc" {
				t.Fatalf("exact boundary should succeed: %q %v", data, err)
			}
			extra := filepath.Join(ws, "extra.yaml")
			writeFile(t, extra, "x")
			if _, err := source.read(extra); !errors.Is(err, errkit.ErrSCOPE003) {
				t.Fatalf("budget+1 should fail with SCOPE-003: %v", err)
			}
		})
	}
}

func TestClosurePackageLimitBeforeManifestReads(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	var body strings.Builder
	body.WriteString("requires:\n")
	for index := 0; index <= MaxClosurePackages; index++ {
		fmt.Fprintf(&body, "  - {package: fixture-%d, version: \"^1.0.0\"}\n", index)
	}
	body.WriteString("flow:\n  - step: {id: done, type: noop}\n")
	writeFile(t, options.Entrypoint, closureRunbook(body.String()))
	closure, errs := BuildClosure(context.Background(), options)
	if closure != nil || len(errs) != 1 || !errors.Is(errs[0], errkit.ErrSCOPE003) {
		t.Fatalf("package budget not checked before loading: %v %v", closure, errs)
	}
}

func TestClosureRetainsExternalIncludeWarningAcrossDiscoveryRounds(t *testing.T) {
	ws := t.TempDir()
	workspace := filepath.Join(ws, "workspace")
	options := closureOptions(t, workspace)
	writeFile(t, options.Entrypoint, closureRunbook(`flow:
  - step: {id: enter, type: include, include: {runbook: ../external/child.runbook.yaml}}
`))
	writeFile(t, filepath.Join(ws, "external", "child.runbook.yaml"), closureRunbook("flow:\n  - step: {id: done, type: noop}\n"))
	closure, errs := BuildClosure(context.Background(), options)
	fatal, warnings := errkit.SplitWarnings(errs)
	if closure == nil || len(fatal) != 0 || len(warnings) != 1 || !hasCode(warnings, "PKG-W003") {
		t.Fatalf("external include warning lost or made fatal: %v %v", closure, errs)
	}
}

func TestClosureCapturedDiscoveryDoesNotRescanChangingToolDirectory(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	if err := os.MkdirAll(filepath.Join(ws, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, options.Entrypoint, closureRunbook(`toolRefs:
  - {name: investigate, path: local/investigate.tool.yaml}
flow:
  - step: {id: inspect, type: tool, tool: {name: investigate, action: run}}
`))
	writeFile(t, filepath.Join(ws, "local", "investigate.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: investigate, version: "1.0.0"}
transport: {mode: native, command: must-not-execute}
actions:
  - name: run
    outputs: {result: {type: string}}
    execute: {kind: runbook, path: child.runbook.yaml}
`)
	child := filepath.Join(ws, "local", "child.runbook.yaml")
	writeFile(t, child, closureRunbook(`outputs: {result: {type: string, value: fixture}}
flow:
  - step: {id: done, type: noop}
`))
	scans := 0
	options.Catalog.Source = &Source{
		WalkDir: func(path string, fn fs.WalkDirFunc) error {
			scans++
			return filepath.WalkDir(path, fn)
		},
		ReadFile: func(path string) ([]byte, error) {
			if filepath.Base(path) == "child.runbook.yaml" {
				writeFile(t, filepath.Join(ws, "tools", "kubectl.tool.yaml"), kubectlToolYAML)
			}
			return os.ReadFile(path)
		},
	}
	closure := requireClosure(t, options)
	if scans != 1 || len(closure.Catalog.ByBare("kubectl")) != 0 {
		t.Fatalf("catalog rescan accepted a mid-preflight tool: scans=%d entries=%+v", scans, closure.Catalog.Entries())
	}
}

func TestClosureDigestCoversBuiltinDefinition(t *testing.T) {
	ws := t.TempDir()
	options := closureOptions(t, ws)
	writeFile(t, options.Entrypoint, closureRunbook(`flow:
  - step: {id: inspect, type: tool, tool: {name: inspect, action: run}}
`))
	options.Catalog.Builtins = []toolpkg.ToolDef{{Name: "inspect", Source: "builtin://inspect", Command: "first", Actions: map[string]*toolpkg.ToolAction{"run": {}}}}
	first := requireClosure(t, options)
	if repeat := requireClosure(t, options); repeat.Digest != first.Digest {
		t.Fatal("unchanged closure is nondeterministic")
	}
	options.Catalog.Builtins[0].Command = "second"
	if second := requireClosure(t, options); second.Digest == first.Digest {
		t.Fatal("definition change was omitted from closure identity")
	}
}
