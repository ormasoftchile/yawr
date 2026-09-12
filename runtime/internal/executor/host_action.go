package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// HostActionExecutor delegates the narrow, typed host_action contract to a
// capable host. It never invokes a command or discovers host capabilities.
type HostActionExecutor struct {
	provider  hostaction.Provider
	evaluator expr.Evaluator
}

func NewHostActionExecutor(provider hostaction.Provider, evaluator expr.Evaluator) *HostActionExecutor {
	return &HostActionExecutor{provider: provider, evaluator: evaluator}
}

func (e *HostActionExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.HostActionSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("host action executor: invalid spec for step %s", step.ID)
	}

	request, err := resolveHostActionRequest(e.evaluator, spec.HostAction, vars)
	if err != nil {
		return nil, err
	}
	if err := hostaction.ValidatePayload(request.Payload); err != nil {
		return nil, err
	}
	if e.provider == nil {
		result := hostActionResult(step, hostaction.StatusUnsupported, "unsupported", engine.StepStatusCompleted, nil)
		result.Output["reason"] = "no host-action provider is registered"
		return result, nil
	}

	simulated, _ := e.provider.(interface{ IsRouteTestHostActionProvider() bool })
	if simulated == nil || !simulated.IsRouteTestHostActionProvider() {
		dispatch, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
			Classification: "unspecified", EndpointIdentity: "host-provider",
			RenderedRequest: map[string]any{"capability": string(request.Capability), "payload": request.Payload},
		})
		if err != nil {
			return nil, err
		}
		ctx = engine.WithPreparedDispatch(ctx, dispatch)
		engine.RecordRouteTestExternalDispatch(ctx)
	}
	response, err := e.provider.ExecuteHostAction(hostaction.WithStepID(ctx, step.ID), request)
	if err != nil {
		if engine.IsReplayBoundaryError(err) {
			return nil, err
		}
		if engine.IsRouteTestBoundaryError(err) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("host action executor: provider acknowledgement was not received: %w", err)
	}
	if !hostaction.IsKnownStatus(response.Status) {
		return nil, fmt.Errorf("host action executor: unknown acknowledgement status %q", response.Status)
	}
	if response.Status == hostaction.StatusCompleted && response.Result == nil ||
		response.Status != hostaction.StatusCompleted && response.Result != nil {
		return hostActionResult(step, hostaction.StatusFailed, "failed", engine.StepStatusCompleted, nil), nil
	}
	if response.Status == hostaction.StatusCompleted {
		if err := hostaction.ValidatePayload(response.Result); err != nil {
			return nil, fmt.Errorf("host action executor: invalid completed result: %w", err)
		}
	}
	if response.Status == hostaction.StatusExecutionNotStarted || response.Status == hostaction.StatusUnsupported {
		return hostActionResult(step, response.Status, "unsupported", engine.StepStatusSkipped, response.Result), nil
	}
	return hostActionResult(step, response.Status, hostActionOutcome(response.Status), engine.StepStatusCompleted, response.Result), nil
}

func resolveHostActionRequest(e expr.Evaluator, config schema.HostActionConfig, vars map[string]any) (hostaction.Request, error) {
	payload, err := resolveHostActionValue(e, config.Request, vars)
	if err != nil {
		return hostaction.Request{}, err
	}
	return hostaction.Request{
		Capability: hostaction.Capability(config.Capability),
		Payload:    payload.(map[string]any),
	}, nil
}

func resolveHostActionValue(e expr.Evaluator, value any, vars map[string]any) (any, error) {
	switch typed := value.(type) {
	case string:
		return resolveTemplate(e, typed, vars)
	case []any:
		resolved := make([]any, len(typed))
		for i, item := range typed {
			value, err := resolveHostActionValue(e, item, vars)
			if err != nil {
				return nil, err
			}
			resolved[i] = value
		}
		return resolved, nil
	case map[string]any:
		resolved := make(map[string]any, len(typed))
		for key, item := range typed {
			value, err := resolveHostActionValue(e, item, vars)
			if err != nil {
				return nil, err
			}
			resolved[key] = value
		}
		return resolved, nil
	default:
		return value, nil
	}
}

func hostActionOutcome(status hostaction.Status) string {
	switch status {
	// Generic yawr.host-action/v1 bridge outcomes:
	case hostaction.StatusCompleted:
		return "completed"
	case hostaction.StatusFailed:
		return "failed"
	case hostaction.StatusTimedOut:
		return "timed-out"
	default:
		return "failed"
	}
}

func hostActionResult(step engine.ResolvedStep, status hostaction.Status, outcome string, stepStatus engine.StepStatus, payload map[string]any) *engine.StepResult {
	result := newResult(step, stepStatus)
	result.Output["status"] = string(status)
	result.Output["outcome"] = outcome
	if payload == nil {
		payload = map[string]any{"status": string(status)}
	}
	result.Output["result"] = payload
	// Derive capability from the step spec so the output reflects the request.
	capability := ""
	if spec, ok := step.Spec.(*schema.HostActionSpec); ok && spec != nil {
		capability = spec.HostAction.Capability
	}
	result.Output["capability"] = capability
	return result
}
