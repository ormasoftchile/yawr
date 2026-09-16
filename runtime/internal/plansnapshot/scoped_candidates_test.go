package plansnapshot

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

func invocationFixture(owner string) ([]schema.FlowNode, *schema.RunbookInvocation) {
	flow := []schema.FlowNode{{Step: &schema.Step{
		ID: "publish", Type: schema.StepTypeResults, LexicalScopeID: owner, ResultsSpec: &schema.ResultsSpec{},
	}}}
	invocation := &schema.RunbookInvocation{
		Bindings: []schema.Binding{
			{Name: "seed", Type: "int", Value: json.Number("7"), ValuePresent: true},
			{Name: "copy", Type: "int", Value: "${seed}", ValuePresent: true},
			{Name: "nullable", Type: "any", Value: nil, ValuePresent: true},
		},
		Outputs: map[string]*schema.Output{"payload": {
			Type: "object", ValueTreePresent: true,
			ValueTree: map[string]any{"count": json.Number("7"), "items": []any{nil, false, "original"}},
		}},
		Results: true,
	}
	return flow, invocation
}

func TestScopedInvocationRoundTripAndIntegrity(t *testing.T) {
	plan, owner := scopedFixture(t)
	flow, invocation := invocationFixture(owner)
	encoded, err := EncodeScopedInvocationFlowClosure(flow, invocation, plan.ToolScopes, owner)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ScopedInvocationFromClosure(encoded, plan.ToolScopes, owner)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, invocation) {
		t.Fatalf("invocation lost typed values/presence/order:\ngot %#v\nwant %#v", restored, invocation)
	}
	restored.Bindings[0].Value = json.Number("99")
	restored.Outputs["payload"].ValueTree.(map[string]any)["count"] = json.Number("99")
	again, err := ScopedInvocationFromClosure(encoded, plan.ToolScopes, owner)
	if err != nil || !reflect.DeepEqual(again, invocation) {
		t.Fatal("accessor returned mutable backing declarations")
	}
	invocation.Bindings[0].Value = json.Number("88")
	if bytes.Contains(encoded, []byte(`"value":88`)) {
		t.Fatal("encoder retained caller declaration backing")
	}

	for _, mutation := range []struct{ name, before, after string }{
		{"binding", `"value":7`, `"value":9`},
		{"output", `"original"`, `"modified"`},
		{"results", `"results":true`, `"results":false`},
		{"version", FlowClosureSchemaV4, FlowClosureSchemaV3},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			tampered := bytes.Replace(encoded, []byte(mutation.before), []byte(mutation.after), 1)
			if bytes.Equal(encoded, tampered) {
				t.Fatal("mutation did not touch fixture")
			}
			if _, err := ScopedInvocationFromClosure(tampered, plan.ToolScopes, owner); err == nil {
				t.Fatal("tampered invocation accepted")
			}
		})
	}
	var closure flowClosureV1
	if err := decodeStrictJSON(encoded, &closure); err != nil {
		t.Fatal(err)
	}
	closure.Invocation.Bindings = append(closure.Invocation.Bindings, closure.Invocation.Bindings[0])
	closure.ClosureDigest, err = flowClosureDigest(closure)
	if err != nil {
		t.Fatal(err)
	}
	invalid, err := json.Marshal(closure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScopedInvocationFromClosure(invalid, plan.ToolScopes, owner); err == nil {
		t.Fatal("digest-valid duplicate binding accepted")
	}
	if _, err := ScopedInvocationFromClosure(encoded, plan.ToolScopes, plan.RootScopeID); err == nil {
		t.Fatal("wrong owner accepted")
	}
	legacy, err := EncodeInvocationFlowClosure(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScopedInvocationFromClosure(legacy, plan.ToolScopes, owner); err == nil {
		t.Fatal("v3 invocation reinterpreted")
	}
}

func candidateFixture(t *testing.T) (*engine.ExecutionPlan, string, map[string]*schema.Runbook) {
	t.Helper()
	plan, owner := scopedFixture(t)
	flow, invocation := invocationFixture(owner)
	source := &schema.Runbook{ID: "candidate", Name: "Captured candidate", Flow: flow, Bindings: invocation.Bindings, Outputs: invocation.Outputs,
		Inputs:     map[string]*schema.Input{"mode": {Type: "string", Default: "read-only"}},
		Governance: &schema.GovernanceConfig{},
	}
	sources := map[string]*schema.Runbook{"child.yaml": source}
	encoded, err := EncodeScopedInvocationFlowClosure(source.Flow, schema.InvocationForRunbook(source.Bindings, source.Outputs, source.Flow), plan.ToolScopes, owner)
	if err != nil {
		t.Fatal(err)
	}
	closure, err := schema.DecodeScopedFlowClosure(encoded)
	if err != nil {
		t.Fatal(err)
	}
	tables := plan.ToolScopes.Export()
	documentID := tables.Scopes[owner].DocumentID
	tables.DynamicTargets = map[string]toolscope.Target{"pkg/candidate": {
		SchemaVersion: toolscope.TargetVersion, DocumentID: documentID, ScopeID: owner,
		SourceDigest: tables.Documents[documentID].SourceDigest, ClosureDigest: closure.ClosureDigest, ExecutableClosure: encoded,
		RunbookID: source.ID, RunbookName: source.Name, ContentHash: strings.Repeat("f", 64), AbsPath: "child.yaml",
		PackageName: "pkg", PackageVersion: "1.2.3", PackageDigest: "sha256:" + strings.Repeat("b", 64),
		Inputs: source.Inputs, Bindings: source.Bindings, Outputs: source.Outputs, Governance: source.Governance,
	}}
	before := plan.ToolScopes.ContextDigest()
	plan.ToolScopes, err = toolscope.New(tables)
	if err != nil {
		t.Fatal(err)
	}
	if before != plan.ToolScopes.ContextDigest() {
		t.Fatal("captured executable content made context digest recursive")
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	return plan, owner, sources
}

func TestCapturedDynamicCandidateSurvivesSourceEditsAndDeletion(t *testing.T) {
	plan, owner, sources := candidateFixture(t)
	snapshot, err := FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// The only source model is changed, then removed. Restoration receives
	// only persisted JSON, with no loader, registry, catalog, or source callback.
	sources["child.yaml"].Name = "Changed after preflight"
	sources["child.yaml"].Flow[0].Step.ID = "changed-step"
	sources["child.yaml"].Bindings[0].Value = json.Number("99")
	sources["child.yaml"].Inputs["mode"].Default = "mutating"
	sources["child.yaml"].Outputs["payload"].ValueTree.(map[string]any)["count"] = json.Number("99")
	delete(sources, "child.yaml")
	plan = nil
	snapshot = SnapshotV1{}
	if err := json.Unmarshal(persisted, &snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	target, flow, err := RestoreScopedDynamicTarget(restored.ToolScopes, "pkg/candidate")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 || target.ScopeID != owner || target.RunbookName != "Captured candidate" ||
		target.PackageVersion != "1.2.3" || target.ContentHash != strings.Repeat("f", 64) ||
		target.AbsPath != "child.yaml" || target.Inputs["mode"].Default != "read-only" ||
		len(flow) != 1 || flow[0].Step.ID != "publish" || target.Bindings[0].Value != json.Number("7") {
		t.Fatalf("candidate was not restored from captured data: %+v", target)
	}
	bound, err := restored.ToolScopes.Resolve(target.ScopeID, "query", "inspect")
	if err != nil || bound.Definition.Runtime.Command != "child.exe" {
		t.Fatal("candidate rebound its local alias")
	}
	target.Inputs["mode"].Default = "changed"
	target.Outputs["payload"].ValueTree.(map[string]any)["count"] = json.Number("100")
	target.ExecutableClosure[0] = '!'
	again, err := restored.ToolScopes.Target("pkg/candidate")
	if err != nil || again.Inputs["mode"].Default != "read-only" || !json.Valid(again.ExecutableClosure) {
		t.Fatal("target accessor returned mutable backing metadata/content")
	}
}

func TestCapturedDynamicCandidateRejectsCorruption(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*toolscope.Target)
	}{
		{"missing-content", func(target *toolscope.Target) { target.ExecutableClosure = nil }},
		{"wrong-version", func(target *toolscope.Target) { target.SchemaVersion = "future" }},
		{"missing-identity", func(target *toolscope.Target) { target.RunbookID = "" }},
		{"wrong-source", func(target *toolscope.Target) { target.AbsPath = "different.yaml" }},
		{"wrong-digest", func(target *toolscope.Target) { target.ClosureDigest = "sha256:" + strings.Repeat("0", 64) }},
		{"metadata-rebound", func(target *toolscope.Target) { target.Bindings[0].Value = json.Number("99") }},
		{"body-tampered", func(target *toolscope.Target) {
			target.ExecutableClosure = bytes.Replace(target.ExecutableClosure, []byte(`"publish"`), []byte(`"changed"`), 1)
		}},
		{"legacy-content", func(target *toolscope.Target) {
			target.ExecutableClosure = bytes.Replace(target.ExecutableClosure, []byte(FlowClosureSchemaV4), []byte(FlowClosureSchemaV3), 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, _, _ := candidateFixture(t)
			tables := plan.ToolScopes.Export()
			target := tables.DynamicTargets["pkg/candidate"]
			test.mutate(&target)
			tables.DynamicTargets["pkg/candidate"] = target
			if _, err := toolscope.New(tables); err == nil {
				t.Fatal("candidate integrity validation accepted corruption")
			}
		})
	}
	plan, _, _ := candidateFixture(t)
	tables := plan.ToolScopes.Export()
	target := tables.DynamicTargets["pkg/candidate"]
	target.RunbookName = "metadata edit"
	tables.DynamicTargets["pkg/candidate"] = target
	if _, err := toolscope.Restore(tables); err == nil {
		t.Fatal("outer digest omitted candidate metadata")
	}
	target.ExecutableClosure = bytes.Replace(target.ExecutableClosure, []byte(`"root_scope_id":"`), []byte(`"root_scope_id":"invalid-`), 1)
	tables.DynamicTargets["pkg/candidate"] = target
	if _, err := toolscope.New(tables); err == nil {
		t.Fatal("candidate accepted wrong closure owner")
	}
}

func TestCapturedTargetChecksRehashedClosureOwnershipAndOuterDigest(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*schema.ScopedFlowClosure, *engine.ExecutionPlan)
	}{
		{"owner", func(closure *schema.ScopedFlowClosure, plan *engine.ExecutionPlan) {
			closure.RootScopeID = plan.RootScopeID
		}},
		{"context", func(closure *schema.ScopedFlowClosure, _ *engine.ExecutionPlan) {
			closure.ScopeContextDigest = "different-context"
		}},
		{"missing-invocation", func(closure *schema.ScopedFlowClosure, _ *engine.ExecutionPlan) { closure.Invocation = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, _, _ := candidateFixture(t)
			tables := plan.ToolScopes.Export()
			target := tables.DynamicTargets["pkg/candidate"]
			closure, err := schema.DecodeScopedFlowClosure(target.ExecutableClosure)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&closure, plan)
			closure.ClosureDigest, err = schema.ScopedFlowClosureDigest(closure)
			if err != nil {
				t.Fatal(err)
			}
			target.ExecutableClosure, err = json.Marshal(closure)
			if err != nil {
				t.Fatal(err)
			}
			target.ClosureDigest = closure.ClosureDigest
			tables.DynamicTargets["pkg/candidate"] = target
			if _, err := toolscope.New(tables); err == nil {
				t.Fatal("digest-valid cross-reference mutation accepted")
			}
		})
	}
	plan, owner, _ := candidateFixture(t)
	tables := plan.ToolScopes.Export()
	target := tables.DynamicTargets["pkg/candidate"]
	flow, err := RestoreScopedFlowClosure(target.ExecutableClosure, plan.ToolScopes, owner)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := ScopedInvocationFromClosure(target.ExecutableClosure, plan.ToolScopes, owner)
	if err != nil {
		t.Fatal(err)
	}
	flow[0].Step.ID = "changed-publish"
	target.ExecutableClosure, err = EncodeScopedInvocationFlowClosure(flow, invocation, plan.ToolScopes, owner)
	if err != nil {
		t.Fatal(err)
	}
	closure, err := schema.DecodeScopedFlowClosure(target.ExecutableClosure)
	if err != nil {
		t.Fatal(err)
	}
	target.ClosureDigest = closure.ClosureDigest
	tables.DynamicTargets["pkg/candidate"] = target
	resealed, err := toolscope.New(tables)
	if err != nil {
		t.Fatal(err)
	}
	if resealed.ContextDigest() != plan.ToolScopes.ContextDigest() || resealed.Export().Digest == tables.Digest {
		t.Fatal("candidate body must change the full digest, not the nonrecursive context digest")
	}
	if _, err := toolscope.Restore(tables); err == nil {
		t.Fatal("old outer digest accepted a rehashed executable replacement")
	}
}
