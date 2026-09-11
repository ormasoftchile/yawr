package expr

import "testing"

func TestNativeSelectorEngineFor(t *testing.T) {
	t.Parallel()
	sel := nativeSelector{}
	for _, kind := range []OperationKind{EvalCondition, Interpolate, ResolveCapturePath} {
		kind := kind
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			if got := sel.EngineFor(Operation{Kind: kind, Source: "source"}); got != EngineNative {
				t.Fatalf("EngineFor() = %s, want %s", got, EngineNative)
			}
		})
	}
}

func TestSelectionAuditHook(t *testing.T) {
	restoreSelector := setSelectorForTest(nativeSelector{})
	defer restoreSelector()

	var gotOp Operation
	var gotEngine Engine
	restoreHook := setAuditHookForTest(func(op Operation, engine Engine) {
		gotOp = op
		gotEngine = engine
	})
	defer restoreHook()

	_, err := (&SimpleConditionEvaluator{}).EvalBool("ready", map[string]any{"ready": true})
	if err != nil {
		t.Fatalf("EvalBool: %v", err)
	}
	if gotOp.Kind != EvalCondition || gotEngine != EngineNative {
		t.Fatalf("audit hook got (%s, %s), want (%s, %s)", gotOp.Kind, gotEngine, EvalCondition, EngineNative)
	}
}
