package engine

import "github.com/ormasoftchile/yawr/runtime/pkg/schema"

// ResultsInvocation derives declarations from the frozen declaring plan, not a
// structural suffix or the outputs of an included runbook.
func ResultsInvocation(plan *ExecutionPlan) *schema.RunbookInvocation {
	invocation := &schema.RunbookInvocation{Bindings: plan.Bindings, Outputs: plan.Outputs}
	for _, step := range plan.Steps {
		if step.Depth == 0 && (step.Kind == "results" || schema.PublishesResults(step.Spec)) {
			invocation.Results = true
		}
	}
	return invocation
}

func ValidRootPublicationOrigin(plan *ExecutionPlan, state RunState) bool {
	if state.Results == nil {
		return true
	}
	last := -1
	for index, step := range plan.Steps {
		if step.Depth == 0 {
			last = index
		}
	}
	for index, step := range plan.Steps {
		if step.Depth != 0 || state.Results.Origin.NodeID != DebugNodeID(nil, step.ID) {
			continue
		}
		result := state.StepResults[step.ID]
		if result == nil || result.Status != StepStatusCompleted || result.Results == nil ||
			result.Results.Digest != state.Results.Digest {
			return false
		}
		terminal, _ := result.Output["terminal"].(bool)
		return step.Kind == "results" && index == last ||
			terminal && result.TerminalResults && schema.PublishesResults(step.Spec)
	}
	return false
}
