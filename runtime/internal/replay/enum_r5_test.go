package replay

import (
	"context"
	"strings"
	"testing"

	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// enumConstrainedToolDefs builds a tools map with one action ("action")
// declaring one enum-constrained arg ("strategy": ["graceful", "force"]),
// matching the shape checkToolCallEnums expects at S1 (tool action args).
func enumConstrainedToolDefs() map[string]*schema.ToolDef {
	return map[string]*schema.ToolDef{
		"tool": {
			Name: "tool",
			Actions: map[string]*schema.ToolAction{
				"action": {
					Args: map[string]*schema.ArgDef{
						"strategy": {Type: "string", Enum: schema.EnumConstraint{"graceful", "force"}},
					},
				},
			},
		},
	}
}

// TestReplayExecutor_Enum008_RejectsNonMemberArg covers R5
// (barbara-enum-mvp-implementation-gate.md): a replayed tool-call step
// whose materialised arg value is not a declared enum member must fail
// with ENUM-008 before any scenario fixture is consulted -- the same
// outcome the real (non-replay) ToolExecutor produces for the identical
// binding, so a recorded scenario cannot launder a value the real path
// would reject.
func TestReplayExecutor_Enum008_RejectsNonMemberArg(t *testing.T) {
	scenario := &Scenario{
		Tools: map[string]ToolFixture{
			// Present and matching -- if the enum check were bypassed,
			// this fixture would let the step "succeed".
			"tool/action": {Response: "{\"ok\":true}", ExitCode: 0},
		},
	}
	exec := NewReplayExecutor("tool", scenario).WithEnumChecks(&internalexpr.TemplateEvaluator{}, enumConstrainedToolDefs())
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
			Name:   "tool",
			Action: "action",
			Args:   map[string]any{"strategy": "${strategy}"},
		}},
	}
	result, err := exec.Execute(context.Background(), step, map[string]any{"strategy": "aggressive"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusFailed {
		t.Fatalf("expected failed status for an off-enum replayed arg, got %s", result.Status)
	}
	if result.Error == nil || !strings.Contains(result.Error.Error(), "ENUM-008") {
		t.Fatalf("expected ENUM-008 error, got %v", result.Error)
	}
}

// TestReplayExecutor_Enum008_AcceptsMemberArg is the corresponding happy
// path: a declared-member materialised value must not be rejected and the
// recorded fixture must still be consulted normally.
func TestReplayExecutor_Enum008_AcceptsMemberArg(t *testing.T) {
	scenario := &Scenario{
		Tools: map[string]ToolFixture{
			"tool/action": {Response: "{\"ok\":true}", ExitCode: 0},
		},
	}
	exec := NewReplayExecutor("tool", scenario).WithEnumChecks(&internalexpr.TemplateEvaluator{}, enumConstrainedToolDefs())
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
			Name:   "tool",
			Action: "action",
			Args:   map[string]any{"strategy": "${strategy}"},
		}},
	}
	result, err := exec.Execute(context.Background(), step, map[string]any{"strategy": "force"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed status, got %s (error: %v)", result.Status, result.Error)
	}
	if result.Output["response"] != "{\"ok\":true}" {
		t.Fatalf("unexpected response: %#v", result.Output)
	}
}

// TestReplayExecutor_Enum008_NoBypassWithoutToolsMap documents the honest
// no-op boundary: when a ReplayExecutor is constructed without a tools map
// (e.g. the internal/adapter wiring-time "replay" mode branch, which runs
// before any plan exists -- see internal/adapter/wire.go), there is no
// declaration to check against, so the fixture path runs unchanged. This
// is not a silent bypass of a known declaration; internal/replay.ReplayEngine
// (the one live replay entry point) always supplies a tools map.
func TestReplayExecutor_Enum008_NoBypassWithoutToolsMap(t *testing.T) {
	scenario := &Scenario{
		Tools: map[string]ToolFixture{
			"tool/action": {Response: "{\"ok\":true}", ExitCode: 0},
		},
	}
	exec := NewReplayExecutor("tool", scenario) // no WithEnumChecks call
	step := engine.ResolvedStep{
		ID:   "step-1",
		Kind: "tool",
		Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
			Name:   "tool",
			Action: "action",
			Args:   map[string]any{"strategy": "aggressive"},
		}},
	}
	result, err := exec.Execute(context.Background(), step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed status (no tools map means no check), got %s", result.Status)
	}
}
