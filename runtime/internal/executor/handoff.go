package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/sensitive"
)

type HandoffExecutor struct {
	evaluator expr.Evaluator
}

func NewHandoffExecutor(evaluator expr.Evaluator) *HandoffExecutor {
	return &HandoffExecutor{evaluator: evaluator}
}

func (executor *HandoffExecutor) Execute(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
) (*engine.StepResult, error) {
	if engine.ConcurrentExecutionFromContext(ctx) {
		return nil, errors.New("handoff executor: handoff cannot execute beneath a concurrent ancestor")
	}
	spec, ok := step.Spec.(*schema.HandoffSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("handoff executor: invalid spec for step %s", step.ID)
	}
	contextBindings, err := executor.bindingSources(ctx, spec.Handoff.With)
	if err != nil {
		return nil, fmt.Errorf("handoff executor: context: %w", err)
	}
	factBindings, err := executor.bindingSources(ctx, spec.Handoff.Facts)
	if err != nil {
		return nil, fmt.Errorf("handoff executor: facts: %w", err)
	}
	contextValues, err := executor.resolveValues(contextBindings, vars)
	if err != nil {
		return nil, fmt.Errorf("handoff executor: context: %w", err)
	}
	facts, err := executor.resolveValues(factBindings, vars)
	if err != nil {
		return nil, fmt.Errorf("handoff executor: facts: %w", err)
	}
	request := engine.HandoffRequest{
		TargetRunbook: spec.Handoff.Runbook, ReasonCode: spec.Handoff.Reason.Code,
		ReasonSummary: spec.Handoff.Reason.Summary, Context: contextValues, Facts: facts,
		ContextBindings: contextBindings, FactBindings: factBindings,
		StepID: step.ID, Invocation: 1, RetryAttempt: 1,
	}
	request.StructuralPath = append(
		[]schema.DynamicIncludeFrameIdentity(nil), engine.DynamicIncludeStructuralPathFromContext(ctx)...,
	)
	if boundary, found := engine.DispatchExecutionBoundaryFromContext(ctx); found {
		request.QualifiedNodeID = boundary.QualifiedNodeID
		request.CallPath = append([]engine.DebugCallFrame(nil), boundary.CallPath...)
		request.StepID = boundary.StepID
		request.FrameID = boundary.FrameID
		request.FrameStepIndex = boundary.FrameStepIndex
		request.Invocation = boundary.Invocation
		request.RetryAttempt = boundary.RetryAttempt
	}
	if err := internaldebugprotect.ValidateHandoffRequestFromContext(ctx, request); err != nil {
		return nil, fmt.Errorf("handoff executor: protected values: %w", err)
	}
	return nil, engine.NewHandoffRequestError(request)
}

func (executor *HandoffExecutor) bindingSources(ctx context.Context, bindings map[string]string) (map[string]string, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	protection := internaldebugprotect.ProtectionFromContext(ctx)
	if internaldebugprotect.ProtectAllFromContext(ctx) {
		return nil, fmt.Errorf("handoff values are protected")
	}
	protected := make(map[string]bool, len(protection.ProtectedVars))
	for _, name := range protection.ProtectedVars {
		protected[name] = true
	}
	sources := make(map[string]string, len(bindings))
	for name, expression := range bindings {
		source, ok := schema.HandoffBindingSource(expression)
		if !ok {
			return nil, fmt.Errorf("%s must be an exact variable reference", name)
		}
		segments := strings.Split(source, ".")
		if segments[0] == "vars" {
			return nil, fmt.Errorf("%s references the complete variable scope", name)
		}
		for _, segment := range segments {
			if sensitive.Name(segment) || protected[segment] {
				return nil, fmt.Errorf("%s references a protected variable", name)
			}
		}
		sources[name] = source
	}
	return sources, nil
}

func (executor *HandoffExecutor) resolveValues(sources map[string]string, vars map[string]any) (map[string]any, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	resolved := make(map[string]any, len(sources))
	for name, source := range sources {
		segments := strings.Split(source, ".")
		value, found := vars[segments[0]]
		if !found {
			return nil, fmt.Errorf("%s source is unavailable", name)
		}
		for _, segment := range segments[1:] {
			nested, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s source path is unavailable", name)
			}
			value, found = nested[segment]
			if !found {
				return nil, fmt.Errorf("%s source path is unavailable", name)
			}
		}
		if err := internaldebugprotect.ValidateHandoffValues(
			engine.DebugProtection{}, map[string]any{name: value}, nil,
		); err != nil {
			return nil, fmt.Errorf("%s is not a safe handoff value", name)
		}
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > 64*1024 {
			return nil, fmt.Errorf("%s is not a bounded JSON value", name)
		}
		var normalized any
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.UseNumber()
		if err := decoder.Decode(&normalized); err != nil {
			return nil, fmt.Errorf("%s is not a bounded JSON value", name)
		}
		resolved[name] = normalizeHandoffNumber(normalized, value)
	}
	return resolved, nil
}

func normalizeHandoffNumber(normalized any, original any) any {
	if _, ok := normalized.(json.Number); !ok {
		return normalized
	}
	switch original.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return original
	default:
		return normalized
	}
}
