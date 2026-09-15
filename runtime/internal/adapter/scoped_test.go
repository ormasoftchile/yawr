package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func scopedFixture(t *testing.T) (string, ScopedRunOptions) {
	t.Helper()
	ws, err := filepath.Abs(".scoped-fixture-" + strings.ReplaceAll(t.Name(), "/", "-"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ws) })
	p, err := internalparser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatal(err)
	}
	return ws, ScopedRunOptions{Catalog: pkgcatalog.BuildOptions{WorkspaceRoot: ws},
		Parser: p, Entrypoint: filepath.Join(ws, "root.runbook.yaml")}
}

func scopedWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func scopedRunbook(id, body string) string {
	return "apiVersion: yawr.runbook/v1\nid: " + id + "\nname: " + id + "\n" + body
}

func scopedTool(name, command, extra string) string {
	return fmt.Sprintf("apiVersion: yawr.tool/v1\nmeta: {name: %s, version: '1.0.0'}\ntransport: {mode: native, command: %s}\nactions:\n  - name: run\n%s", name, command, extra)
}

type rejectingScopedRegistry struct{ t *testing.T }

func (registry rejectingScopedRegistry) Lookup(context.Context, string, string) (*schema.ToolDef, error) {
	registry.t.Fatal("scoped planner consulted the name registry")
	return nil, plannerpkg.ErrToolNotFound
}

func scopedPlan(t *testing.T, prepared *PreparedScopedRun, root *parser.ParsedRunbook) *engine.ExecutionPlan {
	t.Helper()
	p := internalplanner.New(plannerpkg.Config{Loader: prepared.Loader, Tools: rejectingScopedRegistry{t},
		Profile: prepared.Profile})
	plan, err := p.Plan(engine.WithToolScopes(context.Background(), prepared.Scopes), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) == 0 || len(plan.Tools) != 0 {
		t.Fatalf("empty or flattened scoped plan: %+v", plan)
	}
	if err := internalplanner.ValidateToolScopes(plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPrepareScopedRunStaticAliasesCaptured(t *testing.T) {
	ws, options := scopedFixture(t)
	options.Profile = &schema.RuntimeProfile{APIVersion: schema.RuntimeProfileAPIVersion, ID: "effective",
		Context: schema.ProfileContextCLIOperator, Attendance: schema.ProfileAttendanceAttended}
	scopedWrite(t, options.Entrypoint, scopedRunbook("root", `toolRefs: [{name: local, path: root.tool.yaml}]
flow:
  - step: {id: root_call, type: tool, tool: {name: local, action: run}}
  - step: {id: left, type: include, include: {runbook: left.runbook.yaml}}
  - step: {id: right, type: include, include: {runbook: right.runbook.yaml, expand: lazy}}
`))
	scopedWrite(t, filepath.Join(ws, "root.tool.yaml"), scopedTool("root-tool", "root-must-not-execute", ""))
	for _, side := range []string{"left", "right", "grandchild"} {
		nested := ""
		if side == "left" {
			nested = "  - step: {id: nested, type: include, include: {runbook: grandchild.runbook.yaml}}\n"
		}
		scopedWrite(t, filepath.Join(ws, side+".runbook.yaml"), scopedRunbook(side, fmt.Sprintf(`toolRefs: [{name: local, path: %s.tool.yaml}]
flow:
  - step: {id: %s_call, type: tool, tool: {name: local, action: run}}
`, side, side)+nested))
		scopedWrite(t, filepath.Join(ws, side+".tool.yaml"), scopedTool(side+"-tool", side+"-must-not-execute", ""))
	}
	prepared, err := PrepareScopedRun(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	scopedPlan(t, prepared, prepared.Root)
	ids := map[string]bool{}
	for _, name := range []string{"root", "left", "right", "grandchild"} {
		path := filepath.Join(ws, name+".runbook.yaml")
		loaded, err := prepared.Loader.Load(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		scopeID := loaded.Runbook.LexicalScopeID
		bound, err := prepared.Scopes.Resolve(scopeID, "local", "run")
		if err != nil || bound.Definition.Runtime.Command != name+"-must-not-execute" || bound.Definition.Runtime.Name != name+"-tool" {
			t.Fatalf("wrong lexical binding: %+v %v", bound, err)
		}
		ids[bound.BindingID] = true
		bound.Definition.Runtime.Command = "mutated"
		scopedPlan(t, prepared, loaded)
		if name != "root" {
			if _, err := prepared.Loader.LoadScope(context.Background(), path, prepared.RootScopeID); err == nil {
				t.Fatal("loader accepted mismatched owner")
			}
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if len(ids) != 4 {
		t.Fatal("aliases shared a binding")
	}
	loaded, err := prepared.Loader.LoadScope(context.Background(), options.Entrypoint, prepared.RootScopeID)
	if err != nil {
		t.Fatal(err)
	}
	scopedPlan(t, prepared, loaded)
	runtimeRoot, err := prepared.LazyLoader.LoadScoped(context.Background(), options.Entrypoint, prepared.RootScopeID)
	if err != nil || runtimeRoot.RootScopeID != prepared.RootScopeID || runtimeRoot.SourceDigest == "" ||
		runtimeRoot.Flow[0].Step.ToolBindingID == "" {
		t.Fatalf("executor loader lost captured ownership: %+v %v", runtimeRoot, err)
	}
	for _, mode := range []expand.Mode{expand.ModeEager, expand.ModeLazy, expand.ModeAuto} {
		durable, err := prepared.Plan(context.Background(), expand.Policy{Default: mode})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if internalplanner.HasDeferredStaticIncludes(durable) || len(durable.Steps) != 7 || durable.Metadata.Profile == nil {
			t.Fatalf("%s: durable plan did not materialize all captured static bodies with profile: %+v", mode, durable.Steps)
		}
	}
}

func TestPrepareScopedRunDynamicCaptured(t *testing.T) {
	ws, options := scopedFixture(t)
	scopedWrite(t, options.Entrypoint, scopedRunbook("root", `requires:
  - {package: first, version: "^1.0.0", path: first}
  - {package: second, version: "^1.0.0", path: second}
flow:
  - step: {id: enter, type: include, include: {runbook_ref: first/child, resolve_from: catalog}}
`))
	for _, name := range []string{"first", "second"} {
		scopedWrite(t, filepath.Join(ws, name, "yawr-package.yaml"), fmt.Sprintf(`apiVersion: yawr.tool-package/v1
meta: {name: %s, version: "1.0.0"}
exports:
  runbooks: [{id: child, path: child.runbook.yaml}]
`, name))
		scopedWrite(t, filepath.Join(ws, name, "child.runbook.yaml"), scopedRunbook(name, `toolRefs: [{name: local, path: local.tool.yaml}]
bindings:
  - {name: captured_flag, type: boolean, value: false}
flow:
  - step: {id: call, type: tool, tool: {name: local, action: run}}
`))
		scopedWrite(t, filepath.Join(ws, name, "local.tool.yaml"), scopedTool(name, name+"-must-not-execute", ""))
	}
	prepared, err := PrepareScopedRun(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	scopedPlan(t, prepared, prepared.Root)
	scopedContext := engine.WithToolScopes(context.Background(), prepared.Scopes)
	if _, err := prepared.Resolver.Resolve(context.Background(), "first/child"); err == nil {
		t.Fatal("stateless resolver accepted missing run context")
	}
	otherOptions := options
	otherOptions.Profile = &schema.RuntimeProfile{APIVersion: schema.RuntimeProfileAPIVersion, ID: "other",
		Context: schema.ProfileContextCLIOperator, Attendance: schema.ProfileAttendanceAttended}
	other, err := PrepareScopedRun(context.Background(), otherOptions)
	if err != nil {
		t.Fatal(err)
	}
	otherContext := engine.WithToolScopes(context.Background(), other.Scopes)
	firstTarget, err := prepared.Resolver.Resolve(scopedContext, "first/child")
	if err != nil {
		t.Fatal(err)
	}
	otherTarget, err := prepared.Resolver.Resolve(otherContext, "first/child")
	if err != nil || otherTarget.TargetScopeID == firstTarget.TargetScopeID {
		t.Fatalf("stateless resolver retained another run's scopes: %+v %v", otherTarget, err)
	}
	for _, name := range []string{"first", "second"} {
		if err := os.RemoveAll(filepath.Join(ws, name)); err != nil {
			t.Fatal(err)
		}
		result, err := prepared.Resolver.Resolve(scopedContext, name+"/child")
		if err != nil {
			t.Fatal(err)
		}
		if result.RunbookID != name || len(result.Flow) != 1 || result.Flow[0].Step.ToolBindingID == "" || len(result.ChildBindings) != 1 {
			t.Fatalf("candidate lost metadata/body: %+v", result)
		}
		scopedPlan(t, prepared, &parser.ParsedRunbook{Source: result.AbsPath, Runbook: &schema.Runbook{
			ID: result.RunbookID, Name: result.RunbookName, Flow: result.Flow, LexicalScopeID: result.TargetScopeID,
			Inputs: result.ChildInputs, Bindings: result.ChildBindings, Outputs: result.ChildOutputs, Governance: result.ChildGovernance,
		}})
		result.Flow[0].Step.ID = "mutated"
		again, err := prepared.Resolver.Resolve(scopedContext, name+"/child")
		if err != nil || again.Flow[0].Step.ID != "call" {
			t.Fatalf("candidate was mutable: %+v %v", again, err)
		}
	}
	for _, ref := range []string{"child", "../child", "first/missing", " first/child"} {
		if _, err := prepared.Resolver.Resolve(scopedContext, ref); err == nil {
			t.Fatalf("resolver accepted %q", ref)
		}
	}
	if _, err := toolscope.Restore(prepared.Scopes.Export()); err != nil {
		t.Fatal(err)
	}
	target := prepared.Scopes.Export().DynamicTargets["first/child"]
	invocation, err := plansnapshot.ScopedInvocationFromClosure(target.ExecutableClosure, prepared.Scopes, target.ScopeID)
	if err != nil || invocation == nil || len(invocation.Bindings) != 1 || invocation.Bindings[0].Name != "captured_flag" {
		t.Fatalf("captured invocation metadata did not round trip: %+v %v", invocation, err)
	}
	if prepared.Catalog == nil || len(prepared.Catalog.Packages) != 2 ||
		prepared.Catalog.CatalogDigest() != prepared.Scopes.Export().CatalogDigest {
		t.Fatal("captured catalog evidence was not exposed")
	}
	durable, err := prepared.Plan(context.Background(), expand.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range prepared.Catalog.Packages {
		if durable.Metadata.PackageDigests[pkg.Name] != pkg.Digest {
			t.Fatalf("plan lost captured package digest for %s", pkg.Name)
		}
	}
	recorded := durable.Metadata.PackageDigests["first"]
	prepared.Catalog.Packages[0].Digest = "mutated-metadata-view"
	durable.Metadata.PackageDigests["first"] = "mutated-returned-plan"
	again, err := prepared.Plan(context.Background(), expand.Policy{})
	if err != nil || again.Metadata.PackageDigests["first"] != recorded {
		t.Fatalf("metadata consumer mutated authoritative captured package digests: %v", err)
	}
	hash, err := internalplanner.ScopedPlanHash(again)
	if err != nil || hash != again.Metadata.PlanHash {
		t.Fatalf("package metadata was added after plan hashing: %v", err)
	}
}

func TestPrepareScopedRunSubstitutionCaptured(t *testing.T) {
	ws, options := scopedFixture(t)
	scopedWrite(t, options.Entrypoint, scopedRunbook("root", `toolRefs: [{name: local, path: local.tool.yaml}]
flow:
  - step: {id: call, type: tool, tool: {name: local, action: run}}
`))
	scopedWrite(t, filepath.Join(ws, "local.tool.yaml"), scopedTool("canonical", "must-not-execute",
		"    execute: {kind: runbook, path: substitute.runbook.yaml}\n"))
	scopedWrite(t, filepath.Join(ws, "substitute.runbook.yaml"), scopedRunbook("substitute", `toolRefs: [{name: leaf, path: leaf.tool.yaml}]
flow:
  - step: {id: leaf_call, type: tool, tool: {name: leaf, action: run}}
`))
	scopedWrite(t, filepath.Join(ws, "leaf.tool.yaml"), scopedTool("leaf", "leaf-must-not-execute", ""))
	prepared, err := PrepareScopedRun(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	scopedPlan(t, prepared, prepared.Root)
	if err := os.Remove(filepath.Join(ws, "substitute.runbook.yaml")); err != nil {
		t.Fatal(err)
	}
	bound, err := prepared.Scopes.Resolve(prepared.RootScopeID, "local", "run")
	if err != nil {
		t.Fatal(err)
	}
	frozen := bound.Definition.Runtime.Actions["run"].SchemaAction().FrozenSubstitution
	if err := plansnapshot.ValidateFrozenToolSubstitution(frozen, prepared.Scopes); err != nil {
		t.Fatal(err)
	}
	flow, err := plansnapshot.RestoreScopedFlowClosure(frozen.ExecutableClosure, prepared.Scopes, frozen.TargetScopeID)
	if err != nil || len(flow) != 1 || flow[0].Step.ID != "leaf_call" || flow[0].Step.LexicalScopeID == prepared.RootScopeID {
		t.Fatalf("substitution not captured with lexical owner: %v %v", flow, err)
	}
	snapshot := prepared.Scopes.Export()
	flow[0].Step.ID = "changed_body"
	body, err := plansnapshot.EncodeScopedFlowClosure(flow, prepared.Scopes, frozen.TargetScopeID)
	if err != nil {
		t.Fatal(err)
	}
	definition := snapshot.Definitions[bound.DefinitionID]
	definition.Declaration.Actions["run"].FrozenSubstitution.ExecutableClosure = body
	definition.Runtime.Actions["run"].SchemaAction().FrozenSubstitution.ExecutableClosure = body
	snapshot.Definitions[bound.DefinitionID] = definition
	changed, err := toolscope.New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	changedBound, err := changed.Resolve(prepared.RootScopeID, "local", "run")
	if err != nil || changedBound.BindingID != bound.BindingID || changed.Export().Digest == prepared.Scopes.Export().Digest {
		t.Fatalf("generated body hashing changed binding identity or escaped full digest: %v", err)
	}
}

func TestPrepareScopedRunFreezesProfile(t *testing.T) {
	ws, options := scopedFixture(t)
	options.Profile = &schema.RuntimeProfile{APIVersion: schema.RuntimeProfileAPIVersion, ID: "production",
		Context: schema.ProfileContextCLIOperator, Attendance: schema.ProfileAttendanceAttended,
		Tools: map[string]*schema.ProfileToolOverride{"local": {Endpoint: "https://stage.example.test/mcp", Provider: "managed-identity"}}}
	scopedWrite(t, options.Entrypoint, scopedRunbook("root", `toolRefs: [{name: local, path: local.tool.yaml}]
flow:
  - step: {id: call, type: tool, tool: {name: local, action: run}}
`))
	scopedWrite(t, filepath.Join(ws, "local.tool.yaml"), `apiVersion: yawr.tool/v1
meta: {name: canonical, version: "1.0.0"}
transport:
  mode: mcp-http
  url: https://prod.example.test/mcp
  auth:
    provider: azure-cli
    scope: fixture
    allowed_hosts: [prod.example.test, stage.example.test]
actions:
  - name: run
`)
	prepared, err := PrepareScopedRun(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}

	options.Profile.Tools["local"].Endpoint = "https://evil.example.test/mcp"
	bound, err := prepared.Scopes.Resolve(prepared.RootScopeID, "local", "run")
	if err != nil || bound.Definition.Runtime.URL != "https://stage.example.test/mcp" ||
		bound.Definition.Runtime.Auth.Provider != "managed-identity" {
		t.Fatalf("configuration not frozen: %+v %v", bound, err)
	}
	scopedPlan(t, prepared, prepared.Root)
	if _, err := PrepareScopedRun(context.Background(), options); err == nil {
		t.Fatal("unapproved endpoint accepted")
	}
	options.Profile.Tools["local"] = &schema.ProfileToolOverride{Provider: "unknown"}
	if _, err := PrepareScopedRun(context.Background(), options); err == nil {
		t.Fatal("unknown provider accepted")
	}
	options.Profile.Tools["local"] = &schema.ProfileToolOverride{Mode: "native"}
	if _, err := PrepareScopedRun(context.Background(), options); err == nil {
		t.Fatal("transport rewrite accepted")
	}
}

func TestPrepareScopedRunAmbiguousFileRequiresOwner(t *testing.T) {
	ws, options := scopedFixture(t)
	scopedWrite(t, options.Entrypoint, scopedRunbook("root", `requires:
  - {package: fixture, version: "^1.0.0", path: package}
flow:
  - step: {id: static_child, type: include, include: {runbook: package/child.runbook.yaml}}
  - step: {id: dynamic_child, type: include, include: {runbook_ref: fixture/child, resolve_from: catalog}}
`))
	scopedWrite(t, filepath.Join(ws, "package", "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: fixture, version: "1.0.0"}
exports:
  runbooks: [{id: child, path: child.runbook.yaml}]
`)
	path := filepath.Join(ws, "package", "child.runbook.yaml")
	scopedWrite(t, path, scopedRunbook("child", "flow:\n  - step: {id: captured, type: noop}\n"))
	prepared, err := PrepareScopedRun(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Loader.Load(context.Background(), path); err == nil {
		t.Fatal("ambiguous ordinary load silently chose an owner")
	}
	owners := 0
	for _, document := range prepared.Scopes.Export().Documents {
		if scopedPathKey(document.SourceIdentity) != scopedPathKey(path) {
			continue
		}
		owners++
		loaded, err := prepared.Loader.LoadScope(context.Background(), path, document.ScopeID)
		if err != nil || loaded.Runbook.LexicalScopeID != document.ScopeID {
			t.Fatalf("explicit owner load failed: %+v %v", loaded, err)
		}
	}
	if owners != 2 {
		t.Fatalf("fixture expected two owners, got %d", owners)
	}
}

func TestPrepareScopedRunRejectsSubstitutionCycle(t *testing.T) {
	ws, options := scopedFixture(t)
	body := `toolRefs: [{name: local, path: local.tool.yaml}]
flow:
  - step: {id: call, type: tool, tool: {name: local, action: run}}
`
	scopedWrite(t, options.Entrypoint, scopedRunbook("root", body))
	scopedWrite(t, filepath.Join(ws, "substitute.runbook.yaml"), scopedRunbook("substitute", body))
	scopedWrite(t, filepath.Join(ws, "local.tool.yaml"), scopedTool("canonical", "must-not-execute",
		"    execute: {kind: runbook, path: substitute.runbook.yaml}\n"))
	if _, err := PrepareScopedRun(context.Background(), options); err == nil || !strings.Contains(err.Error(), "PKG-015") {
		t.Fatalf("substitution cycle was not rejected: %v", err)
	}
}

func TestPrepareScopedRunDistinctDefinitionsAreNotSubstitutionCycle(t *testing.T) {
	ws, options := scopedFixture(t)
	body := `toolRefs: [{name: local, path: local.tool.yaml}]
flow:
  - step: {id: call, type: tool, tool: {name: local, action: run}}
`
	scopedWrite(t, options.Entrypoint, scopedRunbook("root", body))
	scopedWrite(t, filepath.Join(ws, "local.tool.yaml"), scopedTool("canonical", "outer-must-not-execute",
		"    execute: {kind: runbook, path: nested/substitute.runbook.yaml}\n"))
	scopedWrite(t, filepath.Join(ws, "nested", "substitute.runbook.yaml"), scopedRunbook("substitute", body))
	scopedWrite(t, filepath.Join(ws, "nested", "local.tool.yaml"), scopedTool("canonical", "inner-must-not-execute",
		"    execute: {kind: runbook, path: leaf.runbook.yaml}\n"))
	scopedWrite(t, filepath.Join(ws, "nested", "leaf.runbook.yaml"), scopedRunbook("leaf",
		"flow:\n  - step: {id: reached, type: noop}\n"))
	prepared, err := PrepareScopedRun(context.Background(), options)
	if err != nil {
		t.Fatalf("distinct source definitions were treated as one recursive name: %v", err)
	}
	scopedPlan(t, prepared, prepared.Root)
}

func TestPrepareScopedRunProfileExpansionModes(t *testing.T) {
	for _, mode := range []expand.Mode{expand.ModeEager, expand.ModeLazy, expand.ModeAuto} {
		t.Run(string(mode), func(t *testing.T) {
			ws, options := scopedFixture(t)
			options.Profile = &schema.RuntimeProfile{APIVersion: schema.RuntimeProfileAPIVersion, ID: "effective",
				Context: schema.ProfileContextCLIOperator, Attendance: schema.ProfileAttendanceAttended}
			scopedWrite(t, options.Entrypoint, scopedRunbook("root", fmt.Sprintf(`flow:
  - step: {id: enter, type: include, include: {runbook: child.runbook.yaml, expand: %s}}
`, mode)))
			childPath := filepath.Join(ws, "child.runbook.yaml")
			scopedWrite(t, childPath, scopedRunbook("child", `toolRefs: [{name: local, path: local.tool.yaml}]
inputs:
  items: {type: array, required: false, default: [first]}
flow:
  - parallel:
      id: branches
      branches:
        - label: first
          steps:
            - iterate:
                id: each_item
                over: items
                as: item
                steps:
                  - step: {id: first_call, type: tool, tool: {name: local, action: run}}
        - label: second
          steps:
            - step: {id: second_call, type: tool, tool: {name: local, action: run}}
`))
			scopedWrite(t, filepath.Join(ws, "local.tool.yaml"), scopedTool("canonical", "must-not-execute", ""))
			prepared, err := PrepareScopedRun(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(childPath); err != nil {
				t.Fatal(err)
			}
			plan, err := prepared.Plan(context.Background(), expand.Policy{Default: mode})
			if err != nil {
				t.Fatal(err)
			}
			if internalplanner.HasDeferredStaticIncludes(plan) || len(plan.Steps) != 5 || len(plan.Tools) != 0 {
				t.Fatalf("incomplete scoped plan: %+v", plan.Steps)
			}
			if err := internalplanner.ValidateToolScopes(plan); err != nil {
				t.Fatal(err)
			}
			if plan.Metadata.Profile == nil || plan.Metadata.Profile.ID != "effective" {
				t.Fatal("effective profile was lost")
			}
			originalOwner := plan.Steps[1].LexicalScopeID
			plan.Steps[1].LexicalScopeID = plan.RootScopeID
			if err := internalplanner.ValidateExecutionPlan(plan); err == nil {
				t.Fatal("wrong nonempty composite owner bypassed complete plan validation")
			}
			if err := internalplanner.ValidateToolScopes(plan); err == nil {
				t.Fatal("wrong nonempty composite owner bypassed validation")
			}
			plan.Steps[1].LexicalScopeID = originalOwner
			plan.Steps[3].LexicalScopeID = ""
			if err := internalplanner.ValidateExecutionPlan(plan); err == nil {
				t.Fatal("missing authored tool owner bypassed complete plan validation")
			}
			if err := internalplanner.ValidateToolScopes(plan); err == nil {
				t.Fatal("missing authored tool owner bypassed validation")
			}
		})
	}
}

func TestPrepareScopedRunRejectsMetadataOnlySource(t *testing.T) {
	_, options := scopedFixture(t)
	options.Catalog.Source = &pkgcatalog.Source{MetadataOnly: true, ReadFile: func(string) ([]byte, error) {
		t.Fatal("metadata-only execution attempted to read source")
		return nil, nil
	}}
	if _, err := PrepareScopedRun(context.Background(), options); err == nil || !strings.Contains(err.Error(), "complete runtime metadata") {
		t.Fatalf("metadata-only source accepted for execution: %v", err)
	}
}
