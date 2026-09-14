package executor

import (
	"context"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestEndPublishingOutcomeGIS(t *testing.T) {
	for _, test := range []struct {
		name, category, code   string
		vars                   map[string]any
		wantCategory, wantCode string
		publish, fail          bool
	}{
		{"forward", "${__run_outcome_category}", "${__run_outcome_code}", map[string]any{"__run_outcome_category": "custom", "__run_outcome_code": "Exact-${literal}"}, "custom", "Exact-${literal}", true, false},
		{"committed capture", "${child.status}", "${child.code}", map[string]any{"child": map[string]any{"status": "escalated", "code": "Exact"}}, "escalated", "Exact", true, false},
		{"literal paths", `custom\new`, `C:\new\test`, nil, `custom\new`, `C:\new\test`, true, false},
		{"legacy literal", "${category}", "${code}", nil, "${category}", "${code}", false, false},
		{"missing", "${missing}", "exact", nil, "", "", true, true},
		{"wrong type", "resolved", "${42}", nil, "", "", true, true},
		{"null", "resolved", "${null}", nil, "", "", true, true},
		{"impure", "${now()}", "exact", nil, "", "", true, true},
		{"syntax", "${broken", "exact", nil, "", "", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := &schema.EndSpec{PublishResults: test.publish, Outcome: &schema.OutcomeDeclaration{Category: test.category, Code: test.code}}
			result, err := NewEndExecutor().Execute(context.Background(), engine.ResolvedStep{ID: "end", Kind: "end", Spec: spec}, test.vars)
			if err != nil {
				t.Fatal(err)
			}
			if test.fail {
				if result.Status != engine.StepStatusFailed || result.Error == nil || len(result.Vars) != 0 || len(result.Output) != 0 || result.Results != nil {
					t.Fatalf("outcome failure was not atomic: %#v", result)
				}
			} else if result.Status != engine.StepStatusCompleted || result.Output["outcome_category"] != test.wantCategory ||
				result.Output["outcome_code"] != test.wantCode || result.Vars["__run_outcome_category"] != test.wantCategory ||
				result.Vars["__run_outcome_code"] != test.wantCode {
				t.Fatalf("outcome mismatch: %#v", result)
			}
			if spec.Outcome.Category != test.category || spec.Outcome.Code != test.code {
				t.Fatal("mutated frozen outcome expressions")
			}
		})
	}
}
