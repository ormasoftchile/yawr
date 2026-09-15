package plansnapshot

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func TestLegacyFrozenFlowRetainsIncludeAlias(t *testing.T) {
	nodes := []schema.FlowNode{{Step: &schema.Step{ID: "call", Type: schema.StepTypeInclude, IncludeAlias: "check",
		IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{Runbook: "leaf.yaml"}, ResolvedRunbookPath: "leaf.yaml"}}}}
	body, err := EncodeFlowClosure(nodes)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreFlowClosure(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0].Step.ID != "call" || restored[0].Step.IncludeAlias != "check" {
		t.Fatalf("legacy frozen flow lost authored identity/presentation: %+v", restored)
	}
}

func TestIncludeAliasesRemainFileLocalThroughSnapshotAndFrozenBodies(t *testing.T) {
	plan, childOwner := scopedFixture(t)
	rootOwner := plan.RootScopeID
	tables := plan.ToolScopes.Export()
	leaf := toolscope.Document{SourceIdentity: "leaf.yaml", SourceDigest: "sha256:" + strings.Repeat("e", 64), PackageIdentity: "workspace"}
	leafID := toolscope.DocumentID(leaf)
	leaf.ScopeID = toolscope.ScopeID(leafID, tables.CatalogDigest, tables.ProfileDigest)
	tables.Documents[leafID] = leaf
	tables.Scopes[leaf.ScopeID] = toolscope.Scope{DocumentID: leafID, Bindings: map[string]string{}}
	for _, owner := range []string{rootOwner, childOwner} {
		id := tables.Scopes[owner].DocumentID
		document := tables.Documents[id]
		document.StaticEdges = append(document.StaticEdges, toolscope.Edge{StepID: "same-include", DocumentID: leafID})
		tables.Documents[id] = document
	}
	scopes, err := toolscope.New(tables)
	if err != nil {
		t.Fatal(err)
	}
	include := func(owner string) *schema.Step {
		return &schema.Step{ID: "same-include", Type: schema.StepTypeInclude, LexicalScopeID: owner,
			IncludeSpec: &schema.IncludeSpec{TargetScopeID: leaf.ScopeID, Include: schema.IncludeConfig{Runbook: "leaf.yaml"},
				ResolvedRunbookPath: "leaf.yaml", ResolvedRunbookID: "leaf", ResolvedRunbookName: "Leaf",
				ResolvedRunbookContentHash: strings.Repeat("e", 64)}}
	}
	left, right := include(rootOwner), include(childOwner)
	child := &schema.Runbook{ID: "child", Name: "Child", Imports: map[string]string{"right-check": "leaf.yaml"},
		Flow: []schema.FlowNode{{Iterate: &schema.IterateNode{ID: "repeat-loop", Over: "${items}", As: "item", Steps: []schema.FlowNode{{Step: right}}}}}}
	outer := &schema.Step{ID: "include-child", Type: schema.StepTypeInclude, LexicalScopeID: rootOwner,
		IncludeSpec: &schema.IncludeSpec{TargetScopeID: childOwner, Include: schema.IncludeConfig{Runbook: "child.yaml"},
			ResolvedSteps: child.Flow, ResolvedRunbookPath: "child.yaml", ResolvedRunbookID: "child",
			ResolvedRunbookName: "Child", ResolvedRunbookContentHash: strings.Repeat("a", 64)}}
	root := &schema.Runbook{Imports: map[string]string{"left-check": "leaf.yaml", "child-label": "child.yaml"},
		Flow: []schema.FlowNode{{Iterate: &schema.IterateNode{ID: "repeat-loop", Over: "${items}", As: "item", Steps: []schema.FlowNode{{Step: left}}}}, {Step: outer}}}
	schema.CaptureIncludeAliases(root)
	if right.IncludeAlias != "" {
		t.Fatal("caller aliases leaked across source boundary")
	}
	schema.CaptureIncludeAliases(child)
	root.Imports, child.Imports = nil, nil

	closure, err := EncodeScopedInvocationFlowClosure(child.Flow, &schema.RunbookInvocation{}, scopes, childOwner)
	if err != nil {
		t.Fatal(err)
	}
	header, err := schema.DecodeScopedFlowClosure(closure)
	if err != nil {
		t.Fatal(err)
	}
	tables = scopes.Export()
	childDocumentID := tables.Scopes[childOwner].DocumentID
	tables.DynamicTargets = map[string]toolscope.Target{"pkg/child": {
		SchemaVersion: toolscope.TargetVersion, DocumentID: childDocumentID, ScopeID: childOwner,
		SourceDigest: tables.Documents[childDocumentID].SourceDigest, ClosureDigest: header.ClosureDigest, ExecutableClosure: closure,
		RunbookID: "child", RunbookName: "Child", ContentHash: strings.Repeat("a", 64), AbsPath: "child.yaml",
		PackageName: "pkg", PackageVersion: "1.0.0", PackageDigest: "sha256:" + strings.Repeat("b", 64),
	}}
	plan.ToolScopes, err = toolscope.New(tables)
	if err != nil {
		t.Fatal(err)
	}
	plan.Steps = []engine.ResolvedStep{
		{ID: "repeat-loop", Kind: "iterate", LexicalScopeID: rootOwner, Spec: root.Flow[0].Iterate},
		{ID: outer.ID, Kind: "include", LexicalScopeID: rootOwner, IncludeAlias: outer.IncludeAlias, Spec: outer.IncludeSpec},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := planner.FinalizeMaterializedPlan(plan); err != nil {
		t.Fatal(err)
	}
	check := func(plan *engine.ExecutionPlan) {
		t.Helper()
		found := map[string]string{}
		for _, step := range plan.Steps {
			if step.ID == "same-include" {
				found[step.LexicalScopeID] = step.IncludeAlias
			}
		}
		if len(found) != 2 || found[rootOwner] != "left-check" || found[childOwner] != "right-check" {
			t.Fatalf("same authored ID/path rebound labels across files: %v", found)
		}
	}
	check(plan)
	left.IncludeAlias, right.IncludeAlias = "", ""
	if err := planner.FinalizeMaterializedPlan(plan); err != nil {
		t.Fatal(err)
	}
	check(plan)
	snapshot, err := FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SnapshotV1
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := planner.FinalizeMaterializedPlan(restored); err != nil {
		t.Fatal(err)
	}
	check(restored)
	_, candidate, err := RestoreScopedDynamicTarget(restored.ToolScopes, "pkg/child")
	if err != nil {
		t.Fatal(err)
	}
	if candidate[0].Iterate.Steps[0].Step.IncludeAlias != "right-check" {
		t.Fatal("frozen dynamic candidate dropped alias")
	}
	checkFrozenMaterialization := func(nodes []schema.FlowNode) {
		t.Helper()
		frozenPlan := *restored
		frozenPlan.RootScopeID, frozenPlan.RunbookPath = childOwner, "child.yaml"
		frozenPlan.Steps = []engine.ResolvedStep{{ID: "repeat-loop", Kind: "iterate", LexicalScopeID: childOwner, Spec: nodes[0].Iterate}}
		if err := planner.ValidateExecutionPlan(&frozenPlan); err != nil {
			t.Fatal(err)
		}
		if err := planner.FinalizeMaterializedPlan(&frozenPlan); err != nil {
			t.Fatal(err)
		}
		if len(frozenPlan.Steps) != 2 || frozenPlan.Steps[1].ID != "same-include" ||
			frozenPlan.Steps[1].LexicalScopeID != childOwner || frozenPlan.Steps[1].IncludeAlias != "right-check" {
			t.Fatalf("frozen materialization lost local identity/label: %+v", frozenPlan.Steps)
		}
	}
	checkFrozenMaterialization(candidate)
	frozen := &schema.FrozenToolSubstitution{SchemaVersion: FrozenToolSubstitutionSchemaV2, TargetScopeID: childOwner,
		RunbookPath: "child.yaml", RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64), ExecutableClosure: closure}
	if err := ValidateFrozenToolSubstitution(frozen, restored.ToolScopes); err != nil {
		t.Fatal(err)
	}
	substitution, err := RestoreScopedFlowClosure(frozen.ExecutableClosure, restored.ToolScopes, childOwner)
	if err != nil {
		t.Fatal(err)
	}
	if substitution[0].Iterate.Steps[0].Step.IncludeAlias != "right-check" {
		t.Fatal("frozen substitution dropped alias")
	}
	checkFrozenMaterialization(substitution)
	tampered := bytes.Replace(closure, []byte("right-check"), []byte("left-check"), 1)
	if bytes.Equal(tampered, closure) {
		t.Fatal("fixture contains no alias")
	}
	if _, err := RestoreScopedFlowClosure(tampered, restored.ToolScopes, childOwner); err == nil {
		t.Fatal("alias mutation bypassed closure integrity")
	}
}
