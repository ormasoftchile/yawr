package plansnapshot

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func scopedFixture(t *testing.T) (*engine.ExecutionPlan, string) {
	t.Helper()
	snapshot := toolscope.Snapshot{Version: toolscope.Version, CatalogDigest: "sha256:" + strings.Repeat("c", 64), ProfileDigest: toolscope.ProfileDigest(nil),
		Documents: map[string]toolscope.Document{}, Scopes: map[string]toolscope.Scope{}, Bindings: map[string]toolscope.Binding{}, Definitions: map[string]tool.BoundDefinition{}}
	owners := []string{}
	for _, name := range []string{"parent", "child"} {
		document := toolscope.Document{SourceIdentity: name + ".yaml", SourceDigest: "sha256:" + strings.Repeat("a", 64), PackageIdentity: "workspace"}
		documentID := toolscope.DocumentID(document)
		owner := toolscope.ScopeID(documentID, snapshot.CatalogDigest, snapshot.ProfileDigest)
		document.ScopeID = owner
		snapshot.Documents[documentID] = document
		definition := tool.BoundDefinition{Runtime: tool.ToolDef{Name: name, Command: name + ".exe", Actions: map[string]*tool.ToolAction{"inspect": {}}},
			SourceDigest: "sha256:" + strings.Repeat("b", 64), PackageVersion: "1.0.0"}
		id, err := tool.DefinitionID(definition)
		if err != nil {
			t.Fatal(err)
		}
		bindingID := tool.BindingID(owner, "query", id)
		snapshot.Definitions[id] = definition
		snapshot.Bindings[bindingID] = toolscope.Binding{ScopeID: owner, LogicalName: "query", DefinitionID: id}
		snapshot.Scopes[owner] = toolscope.Scope{DocumentID: documentID, Bindings: map[string]string{"query": bindingID}}
		owners = append(owners, owner)
	}
	parentDocID := snapshot.Scopes[owners[0]].DocumentID
	parentDoc := snapshot.Documents[parentDocID]
	parentDoc.StaticEdges = []toolscope.Edge{{StepID: "include-child", DocumentID: snapshot.Scopes[owners[1]].DocumentID}}
	snapshot.Documents[parentDocID] = parentDoc
	scopes, err := toolscope.New(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	call := func(owner string) *schema.Step {
		bound, err := scopes.Resolve(owner, "query", "inspect")
		if err != nil {
			t.Fatal(err)
		}
		return &schema.Step{ID: "inspect", Type: schema.StepTypeTool, LexicalScopeID: owner, ToolBindingID: bound.BindingID,
			ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "query", Action: "inspect", Args: map[string]any{"limit": 3}}}}
	}
	rootCall, childCall := call(owners[0]), call(owners[1])
	plan := &engine.ExecutionPlan{RunbookPath: "parent.yaml", ToolScopes: scopes, RootScopeID: owners[0],
		Metadata: engine.PlanMetadata{RunbookID: "parent", CatalogDigest: snapshot.CatalogDigest},
		Steps: []engine.ResolvedStep{
			{ID: "inspect", Kind: "tool", LexicalScopeID: owners[0], ToolBindingID: rootCall.ToolBindingID, Spec: rootCall.ToolCall},
			{ID: "include-child", Kind: "include", LexicalScopeID: owners[0], Spec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "child.yaml"}, TargetScopeID: owners[1],
				ResolvedRunbookPath: "child.yaml", ResolvedRunbookID: "child", ResolvedRunbookName: "Child", ResolvedRunbookContentHash: strings.Repeat("a", 64),
				ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{ID: "branch", Type: schema.StepTypeBranch, LexicalScopeID: owners[1],
					BranchSpec: &schema.BranchSpec{Branches: []schema.BranchArm{{Else: true, Steps: []schema.FlowNode{{Step: childCall}}}}}}}}}},
		}}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	return plan, owners[1]
}

func TestScopedSnapshotRoundTripAndOwnedBindings(t *testing.T) {
	plan, child := scopedFixture(t)
	before := plan.Metadata.PlanHash
	snapshot, err := FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != SchemaVersionV4 {
		t.Fatal(snapshot.SchemaVersion)
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
	if restored.Metadata.PlanHash != before || restored.Steps[0].ID != "inspect" {
		t.Fatal("identity changed")
	}
	for _, test := range []struct{ owner, command string }{{restored.RootScopeID, "parent.exe"}, {child, "child.exe"}} {
		bound, err := restored.ToolScopes.Resolve(test.owner, "query", "inspect")
		if err != nil {
			t.Fatal(err)
		}
		if bound.Definition.Runtime.Command != test.command || bound.LogicalName != "query" {
			t.Fatal("rebound")
		}
		bound.Definition.Runtime.Command = "mutated.exe"
		again, _ := restored.ToolScopes.Resolve(test.owner, "query", "inspect")
		if again.Definition.Runtime.Command != test.command {
			t.Fatal("mutable invocation")
		}
	}
	exported := restored.ToolScopes.Export()
	delete(exported.Scopes, child)
	if !restored.ToolScopes.HasScope(child) {
		t.Fatal("mutable exported map")
	}
	if _, err := restored.ToolScopes.Resolve(child, "parent-only", "inspect"); err == nil {
		t.Fatal("ancestor fallback")
	}
}

func TestScopedIntegrityRejections(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*toolscope.Snapshot)
	}{
		{"missing-scope", func(s *toolscope.Snapshot) {
			for id := range s.Scopes {
				delete(s.Scopes, id)
				break
			}
		}},
		{"missing-binding", func(s *toolscope.Snapshot) {
			for id := range s.Bindings {
				delete(s.Bindings, id)
				break
			}
		}},
		{"missing-definition", func(s *toolscope.Snapshot) {
			for id := range s.Definitions {
				delete(s.Definitions, id)
				break
			}
		}},
		{"definition-mutation", func(s *toolscope.Snapshot) {
			for id, d := range s.Definitions {
				d.Runtime.Command = "changed"
				s.Definitions[id] = d
				break
			}
		}},
		{"unknown-version", func(s *toolscope.Snapshot) { s.Version = "future" }},
		{"cross-scope-binding", func(s *toolscope.Snapshot) {
			var ids []string
			for id := range s.Scopes {
				ids = append(ids, id)
			}
			a := s.Scopes[ids[0]]
			a.Bindings["query"] = s.Scopes[ids[1]].Bindings["query"]
			s.Scopes[ids[0]] = a
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, _ := scopedFixture(t)
			snapshot := plan.ToolScopes.Export()
			test.mutate(&snapshot)
			if _, err := toolscope.Restore(snapshot); err == nil {
				t.Fatal("accepted corrupt scopes")
			}
		})
	}
	plan, _ := scopedFixture(t)
	snapshot, err := FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.SchemaVersion = SchemaVersionV3
	snapshot.SnapshotDigest, _ = snapshotDigest(snapshot)
	if _, err := Restore(snapshot); err == nil {
		t.Fatal("v3 reinterpreted")
	}
	plan.Steps[0].ToolBindingID = "wrong"
	if err := planner.ValidateExecutionPlan(plan); err == nil {
		t.Fatal("wrong binding accepted")
	}
}

func TestScopedClosuresAndVersionedPins(t *testing.T) {
	plan, child := scopedFixture(t)
	include := plan.Steps[1].Spec.(*schema.IncludeSpec)
	encoded, err := EncodeScopedFlowClosure(include.ResolvedSteps, plan.ToolScopes, child)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreFlowClosure(encoded); err == nil {
		t.Fatal("legacy reader accepted v4")
	}
	if _, err := EncodeFlowClosure(include.ResolvedSteps); err == nil {
		t.Fatal("legacy writer accepted scoped nodes")
	}
	if _, err := RestoreScopedFlowClosure(encoded, plan.ToolScopes, plan.RootScopeID); err == nil {
		t.Fatal("wrong target accepted")
	}
	if _, err := RestoreScopedFlowClosure(bytes.Replace(encoded, []byte(`"limit":3`), []byte(`"limit":9`), 1), plan.ToolScopes, child); err == nil {
		t.Fatal("body mutation accepted")
	}
	nodes, err := RestoreScopedFlowClosure(encoded, plan.ToolScopes, child)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("restore: %v", err)
	}
	frozen := &schema.FrozenToolSubstitution{SchemaVersion: FrozenToolSubstitutionSchemaV2, TargetScopeID: child, RunbookPath: "child.yaml", RunbookID: "child", RunbookContentHash: strings.Repeat("a", 64), ExecutableClosure: encoded}
	if err := ValidateFrozenToolSubstitution(frozen, plan.ToolScopes); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFrozenToolSubstitution(frozen); err == nil {
		t.Fatal("legacy substitution accepted v2")
	}
	frozen.SchemaVersion = "future"
	if err := ValidateFrozenToolSubstitution(frozen, plan.ToolScopes); err == nil {
		t.Fatal("unknown substitution accepted")
	}
}

func TestScopedHashIncludesNestedBodyAndRuntimeProfile(t *testing.T) {
	plan, child := scopedFixture(t)
	first := plan.Metadata.PlanHash
	include := plan.Steps[1].Spec.(*schema.IncludeSpec)
	nested := include.ResolvedSteps[0].Step.BranchSpec.Branches[0].Steps[0].Step
	nested.ToolCall.Tool.Args["limit"] = 8
	if _, err := FromExecutionPlan(plan); err == nil {
		t.Fatal("stale validation accepted")
	}

	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	if first == plan.Metadata.PlanHash {
		t.Fatal("hash omitted body")
	}
	nested.LexicalScopeID = plan.RootScopeID
	if err := planner.ValidateExecutionPlan(plan); err == nil {
		t.Fatal("structural scope switch accepted")
	}
	nested.LexicalScopeID = child
	plan.Metadata.Profile = &schema.RuntimeProfile{}
	if err := planner.ValidateExecutionPlan(plan); err == nil {
		t.Fatal("profile rebound")
	}
}

func TestScopedDynamicPinSnapshotAndVersionRejections(t *testing.T) {
	plan, child := scopedFixture(t)
	include := plan.Steps[1].Spec.(*schema.IncludeSpec)
	encoded, err := EncodeScopedInvocationFlowClosure(include.ResolvedSteps, &schema.RunbookInvocation{}, plan.ToolScopes, child)
	if err != nil {
		t.Fatal(err)
	}
	var closure flowClosureV1
	if err := decodeStrictJSON(encoded, &closure); err != nil {
		t.Fatal(err)
	}
	tables := plan.ToolScopes.Export()
	documentID := tables.Scopes[child].DocumentID
	tables.DynamicTargets = map[string]toolscope.Target{"pkg/child": {
		SchemaVersion: toolscope.TargetVersion,
		DocumentID:    documentID, ScopeID: child, SourceDigest: tables.Documents[documentID].SourceDigest,
		ClosureDigest: closure.ClosureDigest, ExecutableClosure: encoded,
		RunbookID: "child", RunbookName: "Child", ContentHash: strings.Repeat("a", 64), AbsPath: "child.yaml",
		PackageName: "pkg", PackageVersion: "1.0.0", PackageDigest: "sha256:" + strings.Repeat("b", 64),
	}}
	plan.ToolScopes, err = toolscope.New(tables)
	if err != nil {
		t.Fatal(err)
	}
	pin := schema.LockedDynamicInclude{SchemaVersion: DynamicIncludePinSchemaV2, TargetScopeID: child,
		PackageName: "pkg", PackageVersion: "1.0.0",
		StepID: "dynamic", RenderedRef: "pkg/child", QualifiedID: "pkg/child", AbsPath: "child.yaml",
		FileDigest: tables.Documents[documentID].SourceDigest, PackageDigest: "sha256:" + strings.Repeat("b", 64),
		RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64), ExecutableClosure: encoded}
	if err := ValidateDynamicIncludePin(pin, plan.ToolScopes); err != nil {
		t.Fatal(err)
	}
	plan.Metadata.DynamicIncludes = []schema.LockedDynamicInclude{pin}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	snapshot, err := FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDynamicIncludePin(pin); err == nil {
		t.Fatal("legacy pin reader accepted scoped meaning")
	}
	state := engine.DynamicIncludeResolutionState{SchemaVersion: engine.DynamicIncludeResolutionStateSchemaV1, Pin: pin}
	if err := engine.ValidateDynamicIncludeResolutionVersion(state); err == nil {
		t.Fatal("legacy state accepted scoped pin")
	}
	state.SchemaVersion = engine.DynamicIncludeResolutionStateSchemaV2
	if err := engine.ValidateDynamicIncludeResolutionVersion(state); err != nil {
		t.Fatal(err)
	}
	pin.SchemaVersion = "future"
	if err := ValidateDynamicIncludePin(pin, plan.ToolScopes); err == nil {
		t.Fatal("unknown pin version accepted")
	}
	pin.SchemaVersion = DynamicIncludePinSchemaV2
	pin.TargetScopeID = plan.RootScopeID
	if err := ValidateDynamicIncludePin(pin, plan.ToolScopes); err == nil {
		t.Fatal("rebound dynamic target accepted")
	}
}
