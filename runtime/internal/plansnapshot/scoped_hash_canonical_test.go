package plansnapshot

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestScopedPlanHashNormalizesOmittedStructuralCollections(t *testing.T) {
	plan, _ := scopedFixture(t)
	original := plan.Metadata.PlanHash
	plan.Providers = map[string]*schema.ProviderDef{}
	plan.Inputs = map[string]*schema.Input{}
	plan.Outputs = map[string]*schema.Output{}
	plan.Bindings = []schema.Binding{}
	plan.Metadata.PackageDigests = map[string]string{}
	plan.Metadata.DynamicIncludes = []schema.LockedDynamicInclude{}
	plan.Steps[0].Capture = map[string]string{}
	plan.Steps[0].CaptureDefaults = map[string]any{}
	plan.Steps[0].Export = []string{}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	if plan.Metadata.PlanHash != original {
		t.Fatal("nil versus omitted empty metadata changed executable hash")
	}
	plan.Metadata.PlannedAt = time.Date(2026, 9, 15, 14, 0, 0, 123456789, time.FixedZone("fixture-zone", -7*60*60))
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
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
	if restored.Metadata.PlanHash != plan.Metadata.PlanHash || !restored.Metadata.PlannedAt.Equal(plan.Metadata.PlannedAt) {
		t.Fatal("durable JSON roundtrip changed hash or planned instant")
	}
	if restored.Providers != nil || restored.Steps[0].Capture != nil {
		t.Fatal("fixture failed to exercise omitted-empty normalization")
	}
}

func TestScopedHashPreservesAuthoredNullEmptyAndScalarValues(t *testing.T) {
	values := []struct {
		name  string
		value any
	}{
		{"null", nil}, {"empty-array", []any{}}, {"empty-object", map[string]any{}},
		{"false", false}, {"zero", json.Number("0")}, {"empty-string", ""},
		{"large-integer", json.Number("9007199254740993")},
		{"opaque-record", struct {
			Count int `json:"count"`
		}{}},
	}
	hashes := map[string]string{}
	for _, test := range values {
		t.Run(test.name, func(t *testing.T) {
			plan, _ := scopedFixture(t)
			plan.Steps[0].Spec.(*schema.ToolCallSpec).Tool.Args["payload"] = test.value
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
			if previous, exists := hashes[plan.Metadata.PlanHash]; exists {
				t.Fatalf("%s and %s collapsed to same hash", previous, test.name)
			}
			hashes[plan.Metadata.PlanHash] = test.name
			snapshot, err := FromExecutionPlan(plan)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := Restore(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if restored.Metadata.PlanHash != plan.Metadata.PlanHash {
				t.Fatal("authored value changed across snapshot")
			}
			actual, err := json.Marshal(restored.Steps[0].Spec.(*schema.ToolCallSpec).Tool.Args["payload"])
			if err != nil {
				t.Fatal(err)
			}
			expected, err := json.Marshal(test.value)
			if err != nil || string(actual) != string(expected) {
				t.Fatal("authored value/presence was not preserved")
			}
		})
	}
}

func TestScopedHashUsesTypedPresenceCodecs(t *testing.T) {
	plan, _ := scopedFixture(t)
	plan.Bindings = []schema.Binding{{Name: "nullable", Type: "any", ValuePresent: true, Value: nil}}
	plan.Outputs = map[string]*schema.Output{"payload": {
		Type: "object", ValueTreePresent: true,
		ValueTree: map[string]any{"null": nil, "array": []any{}, "object": map[string]any{}},
	}}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	snapshot, err := FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Bindings[0].ValuePresent || !restored.Outputs["payload"].ValueTreePresent {
		t.Fatal("custom codec presence flags lost")
	}
	plan.Bindings[0].ValuePresent = false
	if _, err := FromExecutionPlan(plan); err == nil {
		t.Fatal("typed presence mutation bypassed hash")
	}
}

func TestScopedHashRejectsGraphMetadataMutationUntilRevalidated(t *testing.T) {
	plan, _ := scopedFixture(t)
	before := plan.Metadata.PlanHash
	plan.Metadata.GraphContentHash = "finalized-graph-digest"
	if _, err := FromExecutionPlan(plan); err == nil {
		t.Fatal("post-validation graph mutation accepted")
	}

	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	if plan.Metadata.PlanHash == before {
		t.Fatal("graph hash excluded from immutable plan hash")
	}
	snapshot, err := FromExecutionPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestScopedHashRoundTripsImplicitBodylessFlowSpecs(t *testing.T) {
	hashes := map[string]schema.StepType{}
	for _, kind := range []schema.StepType{schema.StepTypeNoop, schema.StepTypeResults} {
		t.Run(string(kind), func(t *testing.T) {
			plan, child := scopedFixture(t)
			include := plan.Steps[1].Spec.(*schema.IncludeSpec)
			authored := &schema.Step{ID: "bodyless", Type: kind, LexicalScopeID: child}
			include.ResolvedSteps = append(include.ResolvedSteps, schema.FlowNode{Step: authored})
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatal(err)
			}
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
			if restored.Metadata.PlanHash != plan.Metadata.PlanHash {
				t.Fatal("implicit bodyless spec changed durable meaning")
			}
			if previous, exists := hashes[plan.Metadata.PlanHash]; exists {
				t.Fatalf("distinct step kinds %s and %s collapsed", previous, kind)
			}
			hashes[plan.Metadata.PlanHash] = kind
			if authored.NoopSpec != nil || authored.ResultsSpec != nil {
				t.Fatal("hashing mutated the caller's flow")
			}
		})
	}
}
