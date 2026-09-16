package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// BranchExecutor evaluates conditions and executes the first matching arm.
type BranchExecutor struct {
	condition expr.ConditionEvaluator
	runner    SubStepRunner
}

// NewBranchExecutor constructs a BranchExecutor.
func NewBranchExecutor(cond expr.ConditionEvaluator, runner SubStepRunner) *BranchExecutor {
	return &BranchExecutor{condition: cond, runner: runner}
}

func (e *BranchExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.BranchSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("branch executor: invalid spec for step %s", step.ID)
	}
	if e.runner == nil {
		return nil, fmt.Errorf("branch executor: SubStepRunner is required")
	}

	var elseArm *schema.BranchArm
	elseArmIndex := -1
	var matched *schema.BranchArm
	matchedArmIndex := -1
	for i := range spec.Branches {
		arm := &spec.Branches[i]
		if arm.Else {
			elseArm = arm
			elseArmIndex = i
			continue
		}
		ok, err := evalCondition(e.condition, arm.Condition, vars)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = arm
			matchedArmIndex = i
			break
		}
	}
	if matched == nil {
		matched = elseArm
		matchedArmIndex = elseArmIndex
	}
	if matched == nil {
		return newResult(step, engine.StepStatusSkipped), nil
	}

	results, err := e.runner(ctx, SubStepParent{
		ID:          step.ID,
		RootScopeID: step.LexicalScopeID,
		Kind:        "branch",
		BranchLabel: matched.Label,
		NestDepth:   step.NestDepth + 1,
	}, matched.Steps, copyVars(vars))
	if err != nil {
		if errors.Is(err, ErrSubRunFailed) {
			// Child sub-run terminated as failed. Return a failed StepResult
			// so the parent engine applies resolveOnError on this container step.
			result := newResult(step, engine.StepStatusFailed)
			result.Error = err
			mergeChildVars(result, results)
			if matched.Label != "" {
				result.Output["matched_arm"] = matched.Label
			}
			result.Output["matched_arm_index"] = matchedArmIndex
			return result, nil
		}
		return nil, err
	}
	mergedVars := map[string]any{}
	terminal := false
	terminalResults := false
	var terminalOutcomeCat, terminalOutcomeCode any
	for _, res := range results {
		if res == nil {
			continue
		}
		for k, v := range res.Vars {
			mergedVars[k] = v
		}
		// Propagate terminal flag from any child (e.g. an include
		// whose outcome gate fired, or a nested end step). Without
		// this the parent engine never sees the terminal signal
		// and continues with the next step in the parent flow.
		if res.Output != nil {
			if t, ok := res.Output["terminal"].(bool); ok && t {
				terminal = true
				terminalResults = terminalResults || res.TerminalResults
				if v, ok := res.Output["outcome_category"]; ok {
					terminalOutcomeCat = v
				}
				if v, ok := res.Output["outcome_code"]; ok {
					terminalOutcomeCode = v
				}
			}
		}
	}
	// If SubStepRunner returned without error, the sub-engine completed its
	// flow — any individual step failures were tolerated. Report Completed.
	status := engine.StepStatusCompleted

	result := newResult(step, status)
	for _, child := range results {
		if child != nil {
			result.RequiredFailure = result.RequiredFailure || child.RequiredFailure ||
				child.Status == engine.StepStatusFailed || child.Status == engine.StepStatusDenied ||
				child.Status == engine.StepStatusIndeterminate || child.Status == engine.StepStatusWaiting ||
				child.Status == engine.StepStatusRunning || child.Status == engine.StepStatusPending
		}
	}
	result.Vars = mergedVars
	if matched.Label != "" {
		result.Output["matched_arm"] = matched.Label
	}
	result.Output["matched_arm_index"] = matchedArmIndex
	if terminal {
		result.TerminalResults = terminalResults
		result.Output["terminal"] = true
		if terminalOutcomeCat != nil {
			result.Output["outcome_category"] = terminalOutcomeCat
		}
		if terminalOutcomeCode != nil {
			result.Output["outcome_code"] = terminalOutcomeCode
		}
	}
	return result, nil
}
