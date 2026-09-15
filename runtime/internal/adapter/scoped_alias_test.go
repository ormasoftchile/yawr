package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestPrepareScopedRunCapturesSourceLocalIncludeAliases(t *testing.T) {
	for _, kind := range []string{"dynamic", "substitution"} {
		t.Run(kind, func(t *testing.T) {
			ws, options := scopedFixture(t)
			candidate := filepath.Join(ws, "candidate")
			root := `imports: {wrong-parent: leaf.runbook.yaml}
`
			if kind == "dynamic" {
				root += `requires: [{package: fixture, version: "^1.0.0", path: candidate}]
flow:
  - step: {id: enter, type: include, include: {runbook_ref: fixture/child, resolve_from: catalog}}
`
				scopedWrite(t, filepath.Join(candidate, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta: {name: fixture, version: "1.0.0"}
exports:
  runbooks: [{id: child, path: child.runbook.yaml}]
`)
			} else {
				root += `toolRefs: [{name: investigate, path: candidate/investigate.tool.yaml}]
flow:
  - step: {id: enter, type: tool, tool: {name: investigate, action: run}}
`
				scopedWrite(t, filepath.Join(candidate, "investigate.tool.yaml"), scopedTool("investigate", "must-not-execute",
					"    execute: {kind: runbook, path: child.runbook.yaml}\n"))
			}
			scopedWrite(t, options.Entrypoint, scopedRunbook("root", root))
			scopedWrite(t, filepath.Join(candidate, "child.runbook.yaml"), scopedRunbook("child", `imports: {owned-leaf: leaf.runbook.yaml}
flow:
  - step: {id: nested, type: include, include: {runbook: leaf.runbook.yaml}}
`))
			scopedWrite(t, filepath.Join(candidate, "leaf.runbook.yaml"), scopedRunbook("leaf", `toolRefs: [{name: leaf, path: leaf.tool.yaml}]
flow:
  - step: {id: leaf_call, type: tool, tool: {name: leaf, action: run}}
`))
			scopedWrite(t, filepath.Join(candidate, "leaf.tool.yaml"), scopedTool("leaf", "leaf-must-not-execute", ""))
			prepared, err := PrepareScopedRun(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(candidate); err != nil {
				t.Fatal(err)
			}
			var flow []schema.FlowNode
			if kind == "dynamic" {
				result, err := prepared.Resolver.Resolve(engine.WithToolScopes(context.Background(), prepared.Scopes), "fixture/child")
				if err != nil {
					t.Fatal(err)
				}
				flow = result.Flow
			} else {
				bound, err := prepared.Scopes.Resolve(prepared.RootScopeID, "investigate", "run")
				if err != nil {
					t.Fatal(err)
				}
				frozen := bound.Definition.Declaration.Actions["run"].FrozenSubstitution
				flow, err = plansnapshot.RestoreScopedFlowClosure(frozen.ExecutableClosure, prepared.Scopes, frozen.TargetScopeID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(flow) != 1 || flow[0].Step.IncludeAlias != "owned-leaf" {
				t.Fatalf("captured alias was absent or inherited from caller: %+v", flow)
			}
			nested := flow[0].Step.IncludeSpec.ResolvedSteps
			if len(nested) != 1 || nested[0].Step.ToolBindingID == "" || nested[0].Step.LexicalScopeID == flow[0].Step.LexicalScopeID {
				t.Fatal("alias capture changed or lost the executable lexical boundary")
			}
		})
	}
}
