package plansnapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	parserimpl "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestScopedTerminalResultsParsedFlowRoundTrip(t *testing.T) {
	parser, err := parserimpl.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatal(err)
	}
	for _, category := range []string{"resolved", "escalated", "no_action", "previously-unknown-category"} {
		t.Run(category, func(t *testing.T) {
			source := fmt.Sprintf(`apiVersion: yawr.runbook/v1
id: child
name: Child
outputs:
  result: {type: object, value_tree: {status: %q, code: exact-Code}}
flow:
  - step:
      id: child_branch
      type: branch
      branches:
        - else: true
          steps:
            - step: {id: child_end, type: end, publish_results: true, outcome: {category: %q, code: exact-Code}}
            - step: {id: child_tail, type: noop}
`, category, category)
			parsed, err := parser.ParseBytes(context.Background(), []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			plan, child := scopedFixture(t)
			branch := parsed.Runbook.Flow[0].Step
			branch.LexicalScopeID = child
			for _, node := range branch.BranchSpec.Branches[0].Steps {
				node.Step.LexicalScopeID = child
			}
			if branch.BranchSpec.Branches[0].Steps[1].Step.NoopSpec != nil {
				t.Fatal("fixture no longer exercises parser's implicit noop spec")
			}
			include := plan.Steps[1].Spec.(*schema.IncludeSpec)
			include.ResolvedSteps = parsed.Runbook.Flow
			include.ResolvedOutputs = parsed.Runbook.Outputs
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
				t.Fatal("terminal Results flow hash changed")
			}
			frozen := restored.Steps[1].Spec.(*schema.IncludeSpec)
			end := frozen.ResolvedSteps[0].Step.BranchSpec.Branches[0].Steps[0].Step.EndSpec
			if !end.PublishResults || end.Outcome.Category != category || !frozen.ResolvedOutputs["result"].ValueTreePresent {
				t.Fatal("terminal publication meaning changed")
			}
			end.Outcome.Code = "changed-after-validation"
			if _, err := FromExecutionPlan(restored); err == nil {
				t.Fatal("terminal outcome mutation bypassed immutable hash")
			}
		})
	}
}
