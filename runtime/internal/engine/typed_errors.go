package engine

import (
	"context"
	"encoding/json"
	"errors"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func typedErrorAt(err error, origin enginepkg.ResultsOrigin) error {
	var diagnostic *enginepkg.TypedDiagnostic
	if !errors.As(err, &diagnostic) {
		return err
	}
	copy := *diagnostic
	if copy.Origin.NodeID == "" {
		copy.Origin = origin
	}
	copy.Preview = "<redacted>"
	return &copy
}

func attachTypedDiagnostic(ctx context.Context, result *enginepkg.StepResult) {
	origin := enginepkg.ResultsOrigin{NodeID: enginepkg.DebugNodeID(enginepkg.DebugCallPathFromContext(ctx), result.StepID), Invocation: 1}
	if binding, ok := enginepkg.ExecutionFrameBindingFromContext(ctx); ok {
		origin.FrameID, origin.Invocation = binding.FrameID, binding.Invocation
	}
	result.Error = typedErrorAt(result.Error, origin)
	var diagnostic *enginepkg.TypedDiagnostic
	if !errors.As(result.Error, &diagnostic) {
		return
	}
	data, err := json.Marshal(diagnostic)
	if err != nil {
		return
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		return
	}
	output := make(map[string]any, len(result.Output)+1)
	for key, value := range result.Output {
		output[key] = value
	}
	output["__typed_diagnostic"] = value
	result.Output = output
}
