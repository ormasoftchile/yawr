package executor

import (
	"context"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type AssignExecutor struct{}
type ResultsExecutor struct{}

func (*AssignExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	scope := engine.BindingScopeFromContext(ctx)
	spec, ok := step.Spec.(*schema.AssignSpec)
	if !ok || scope == nil || !scope.Initialized || scope.Invocation == nil {
		return nil, fmt.Errorf("assign %s: initialized declaring scope required", step.ID)
	}
	result := newResult(step, engine.StepStatusCompleted)
	writes, err := AssignBindings(scope.Invocation.Bindings, spec.Assign, vars)
	if err != nil {
		result.Status, result.Outcome, result.Error = engine.StepStatusFailed, engine.StepOutcomeFailed, err
	} else {
		result.Vars = writes
	}
	return result, nil
}

func (*ResultsExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	scope := engine.BindingScopeFromContext(ctx)
	if scope == nil || !scope.Initialized || scope.Invocation == nil || !scope.Invocation.Results {
		return nil, fmt.Errorf("results %s: initialized declaring scope required", step.ID)
	}
	frame, _ := engine.ExecutionFrameBindingFromContext(ctx)
	if frame.FrameID != scope.FrameID {
		return nil, fmt.Errorf("results %s: structural subframes cannot publish the declaring scope", step.ID)
	}
	result := newResult(step, engine.StepStatusCompleted)
	outputs, err := EvaluateNamedOutputs(scope.Invocation.Outputs, vars)
	if err != nil {
		result.Status, result.Outcome, result.Error = engine.StepStatusFailed, engine.StepOutcomeFailed, err
	} else {
		result.Results = &engine.RunResults{SchemaVersion: engine.RunResultsSchemaV1, Outputs: outputs}
	}
	return result, nil
}

func ValidateBindingWrites(scope *engine.BindingScopeState, writes map[string]any) error {
	if scope == nil || scope.Invocation == nil {
		return nil
	}
	for _, binding := range scope.Invocation.Bindings {
		value, written := writes[binding.Name]
		if !written {
			continue
		}
		if !binding.Mutable {
			return fmt.Errorf("capture.%s: immutable binding", binding.Name)
		}
		if err := ValidateStrictValue(value, binding.Type, binding.Enum); err != nil {
			return typedDiagnostic("capture."+binding.Name, binding.Type, value, err)
		}
	}
	return nil
}
