package sessioncoordinator

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

func TestValidateHandoffInputsEnforcesDeclaredTypes(t *testing.T) {
	plan := &engine.ExecutionPlan{Inputs: map[string]*schema.Input{
		"name":    {Type: "string", Required: true},
		"enabled": {Type: "boolean", Required: true},
		"ratio":   {Type: "number", Required: true},
		"count":   {Type: "integer", Required: true},
		"items":   {Type: "array", Required: true},
		"labels":  {Type: "object", Required: true},
		"retries": {Type: "integer", Default: 3},
	}}
	valid := map[string]any{
		"name": "db01", "enabled": true, "ratio": 0.5, "count": 2,
		"items": []any{"a", "b"}, "labels": map[string]any{"region": "westus"},
	}
	resolved, err := validateHandoffInputs(plan, valid)
	if err != nil {
		t.Fatalf("validateHandoffInputs valid: %v", err)
	}
	if resolved["retries"] != 3 {
		t.Fatalf("default retries = %#v, want 3", resolved["retries"])
	}

	for _, test := range []struct {
		name  string
		input string
		value any
	}{
		{name: "string", input: "name", value: 7},
		{name: "boolean", input: "enabled", value: "true"},
		{name: "number", input: "ratio", value: "0.5"},
		{name: "integer string", input: "count", value: "2"},
		{name: "fractional integer", input: "count", value: 2.5},
		{name: "array", input: "items", value: map[string]any{}},
		{name: "object", input: "labels", value: []any{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := make(map[string]any, len(valid))
			for name, value := range valid {
				values[name] = value
			}
			values[test.input] = test.value
			_, err := validateHandoffInputs(plan, values)
			if err == nil || !strings.Contains(err.Error(), test.input) || strings.Contains(err.Error(), "db01") {
				t.Fatalf("validateHandoffInputs error = %v, want value-free type error for %s", err, test.input)
			}
		})
	}
}

func TestValidateHandoffInputsResolvesAndValidatesDefaults(t *testing.T) {
	plan := &engine.ExecutionPlan{Inputs: map[string]*schema.Input{
		"environment": {Type: "string", Required: true},
		"mode": {
			Type: "string", Default: "${environment}", Enum: schema.EnumConstraint{"prod"},
		},
		"retries": {Type: "integer", Default: 3},
	}}
	resolved, err := validateHandoffInputs(plan, map[string]any{"environment": "prod"})
	if err != nil {
		t.Fatalf("validateHandoffInputs valid defaults: %v", err)
	}
	if resolved["mode"] != "prod" || resolved["retries"] != 3 {
		t.Fatalf("resolved defaults = %#v", resolved)
	}

	for _, test := range []struct {
		name     string
		inputs   map[string]*schema.Input
		supplied map[string]any
	}{
		{
			name: "resolved enum mismatch",
			inputs: map[string]*schema.Input{
				"environment": {Type: "string", Required: true},
				"mode":        {Type: "string", Default: "${environment}", Enum: schema.EnumConstraint{"prod"}},
			},
			supplied: map[string]any{"environment": "stage"},
		},
		{
			name: "unresolved default",
			inputs: map[string]*schema.Input{
				"mode": {Type: "string", Default: "${missing}"},
			},
		},
		{
			name: "interpolated default",
			inputs: map[string]*schema.Input{
				"mode": {Type: "string", Default: "prefix-${environment}"},
			},
			supplied: map[string]any{"environment": "prod"},
		},
		{
			name: "wrong default type",
			inputs: map[string]*schema.Input{
				"enabled": {Type: "boolean", Default: "true"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateHandoffInputs(&engine.ExecutionPlan{Inputs: test.inputs}, test.supplied)
			if err == nil || strings.Contains(err.Error(), "stage") {
				t.Fatalf("validateHandoffInputs error = %v, want value-free default rejection", err)
			}
		})
	}
}

func TestValidateHandoffInputsRejectsSecretDefaults(t *testing.T) {
	plan := &engine.ExecutionPlan{Inputs: map[string]*schema.Input{
		"credential": {Type: "secret", Default: "must-never-persist"},
	}}
	_, err := validateHandoffInputs(plan, nil)
	if err == nil || strings.Contains(err.Error(), "must-never-persist") {
		t.Fatalf("secret default error = %v, want value-free refusal", err)
	}
}

func TestValidateHandoffInputsResolvesNestedDefaultsRecursively(t *testing.T) {
	plan := &engine.ExecutionPlan{Inputs: map[string]*schema.Input{
		"region": {Type: "string", Required: true},
		"config": {Type: "object", Default: map[string]any{
			"primary": "${region}", "replicas": []any{"${region}", "fixed"},
		}},
	}}
	resolved, err := validateHandoffInputs(plan, map[string]any{"region": "westus"})
	if err != nil {
		t.Fatalf("validate nested defaults: %v", err)
	}
	config := resolved["config"].(map[string]any)
	if config["primary"] != "westus" || config["replicas"].([]any)[0] != "westus" {
		t.Fatalf("nested defaults = %#v", config)
	}

	for _, test := range []struct {
		name         string
		defaultValue any
	}{
		{name: "nested interpolation", defaultValue: map[string]any{"value": "prefix-${region}"}},
		{name: "nested unresolved", defaultValue: []any{map[string]any{"value": "${missing}"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := &engine.ExecutionPlan{Inputs: map[string]*schema.Input{
				"region": {Type: "string", Required: true},
				"config": {Type: "object", Default: test.defaultValue},
			}}
			if _, err := validateHandoffInputs(invalid, map[string]any{"region": "westus"}); err == nil ||
				strings.Contains(err.Error(), "westus") {
				t.Fatalf("nested default error = %v", err)
			}
		})
	}
}

func TestValidateHandoffRequestAgainstStateRejectsSecretAlias(t *testing.T) {
	const secretValue = "private-value-never-log"
	plan := &engine.ExecutionPlan{Inputs: map[string]*schema.Input{
		"opaque": {Type: "secret", Required: true},
	}}
	state := engine.RunState{Vars: map[string]any{"opaque": secretValue}}
	request := engine.HandoffRequest{
		Context:         map[string]any{"server": secretValue},
		ContextBindings: map[string]string{"server": "opaque"},
	}
	err := validateHandoffRequestAgainstState(plan, state, request)
	if err == nil || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("validateHandoffRequestAgainstState error = %v, want value-free secret refusal", err)
	}
}

func TestValidateHandoffRequestAgainstStateUsesExactNestedFrameAndAuthoredSpec(t *testing.T) {
	frameID := session.DigestJSON("nested-frame")
	handoff := &schema.HandoffSpec{Handoff: schema.HandoffConfig{
		Runbook: "target.runbook.yaml",
		Reason:  schema.HandoffReason{Code: "continue", Summary: "Continue investigation"},
		With:    map[string]string{"server": "${child_server}"},
	}}
	plan := &engine.ExecutionPlan{
		RunbookPath: filepath.Join("workspace", "root.runbook.yaml"),
		Steps: []engine.ResolvedStep{
			{
				ID: "include-child", Kind: "include", Origin: filepath.Join("workspace", "root.runbook.yaml"),
				Spec: &schema.IncludeSpec{
					ResolvedRunbookPath: filepath.Join("workspace", "child.runbook.yaml"),
					ResolvedInputs:      map[string]*schema.Input{"child_server": {Type: "string"}},
				},
			},
			{
				ID: "continue", Kind: "handoff", Spec: handoff, Depth: 1,
				Origin:   filepath.Join("workspace", "child.runbook.yaml"),
				ParentID: "include-child", ParentKind: "include",
			},
		},
	}
	callPath := []engine.DebugCallFrame{{
		StepID: "include-child", RunbookPath: filepath.Join("workspace", "child.runbook.yaml"),
	}}
	state := engine.RunState{
		Vars: map[string]any{"root_only": "root"},
		ExecutionFrames: map[string]*engine.ExecutionFrameState{
			frameID: {
				SchemaVersion: engine.ExecutionFrameStateSchemaV1, FrameID: frameID,
				ParentQualifiedNodeID: "include-child", ParentStepID: "include-child", Kind: "include",
				CallPath: callPath, Invocation: 2, StepCount: 1, StepIDs: []string{"continue"},
				NextStepIndex: 0, WorkingVars: map[string]any{"child_server": "db01"},
				Status: engine.ExecutionFrameStatusActive,
			},
		},
	}
	request := engine.HandoffRequest{
		TargetRunbook: "target.runbook.yaml", ReasonCode: "continue", ReasonSummary: "Continue investigation",
		Context: map[string]any{"server": "db01"}, ContextBindings: map[string]string{"server": "child_server"},
		QualifiedNodeID: "include-child/continue", CallPath: callPath, StepID: "continue",
		FrameID: frameID, FrameStepIndex: 0, Invocation: 2, RetryAttempt: 1,
	}
	if err := validateHandoffRequestAgainstState(plan, state, request); err != nil {
		t.Fatalf("validate nested handoff: %v", err)
	}
	closurePlan := *plan
	closurePlan.Steps = []engine.ResolvedStep{plan.Steps[0]}
	closureInclude := *closurePlan.Steps[0].Spec.(*schema.IncludeSpec)
	closureInclude.ResolvedSteps = []schema.FlowNode{{Step: &schema.Step{
		ID: "continue", Type: schema.StepTypeHandoff, HandoffSpec: handoff,
	}}}
	closurePlan.Steps[0].Spec = &closureInclude
	if err := validateHandoffRequestAgainstState(&closurePlan, state, request); err != nil {
		t.Fatalf("validate immutable closure handoff: %v", err)
	}
	dynamicPlan := *plan
	dynamicPlan.Steps = []engine.ResolvedStep{{
		ID: "include-child", Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "${target}", ResolveFrom: schema.ResolveFromCatalog,
		}},
	}}
	dynamicClosure, err := plansnapshot.EncodeFlowClosure(closureInclude.ResolvedSteps)
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	dynamicState := state
	dynamicFrame := *state.ExecutionFrames[frameID]
	dynamicFrame.Invocation = 1
	dynamicState.ExecutionFrames = map[string]*engine.ExecutionFrameState{frameID: &dynamicFrame}
	dynamicRequest := request
	dynamicRequest.Invocation = 1
	dynamicState.DynamicIncludes = map[string]*engine.DynamicIncludeResolutionState{
		"resolution": {
			SchemaVersion: engine.DynamicIncludeResolutionStateSchemaV1, ResolutionID: "resolution",
			QualifiedNodeID: "include-child", StepID: "include-child", Invocation: 2,
			Pin: schema.LockedDynamicInclude{
				StepID: "include-child", QualifiedNodeID: "include-child", Invocation: 2,
				AbsPath:   filepath.Join("workspace", "child.runbook.yaml"),
				RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64),
				ExecutableClosure: dynamicClosure,
				ResolvedInputs:    map[string]*schema.Input{"child_server": {Type: "string"}},
			},
			Status: engine.DynamicIncludeResolutionStatusActive,
		},
		"other-resolution": {
			SchemaVersion: engine.DynamicIncludeResolutionStateSchemaV1, ResolutionID: "other-resolution",
			QualifiedNodeID: "include-child", StepID: "include-child", Invocation: 1,
			Pin: schema.LockedDynamicInclude{
				StepID: "include-child", AbsPath: filepath.Join("workspace", "child.runbook.yaml"),
				RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64),
				ExecutableClosure: dynamicClosure,
				ResolvedInputs:    map[string]*schema.Input{"child_server": {Type: "secret"}},
			},
			Status: engine.DynamicIncludeResolutionStatusCompleted,
		},
	}
	if err := validateHandoffRequestAgainstState(&dynamicPlan, dynamicState, dynamicRequest); err != nil {
		t.Fatalf("validate committed dynamic closure handoff: %v", err)
	}

	changed := request
	changed.TargetRunbook = "other.runbook.yaml"
	if err := validateHandoffRequestAgainstState(plan, state, changed); err == nil {
		t.Fatal("validation accepted a target different from the immutable handoff definition")
	}
	changed = request
	changed.ContextBindings = map[string]string{"server": "root_only"}
	changed.Context = map[string]any{"server": "root"}
	if err := validateHandoffRequestAgainstState(plan, state, changed); err == nil {
		t.Fatal("validation accepted bindings different from the immutable handoff definition")
	}

	secretPlan := *plan
	secretPlan.Steps = append([]engine.ResolvedStep(nil), plan.Steps...)
	secretInclude := *plan.Steps[0].Spec.(*schema.IncludeSpec)
	secretInclude.ResolvedInputs = map[string]*schema.Input{"child_server": {Type: "secret"}}
	secretPlan.Steps[0].Spec = &secretInclude
	err = validateHandoffRequestAgainstState(&secretPlan, state, request)
	if err == nil || strings.Contains(err.Error(), "db01") {
		t.Fatalf("nested secret alias error = %v, want value-free refusal", err)
	}
}

func TestValidateHandoffRequestAgainstStateDisambiguatesRepeatedNestedIncludes(t *testing.T) {
	rootPath := filepath.Join("workspace", "root.runbook.yaml")
	outerPath := filepath.Join("workspace", "outer.runbook.yaml")
	childPath := filepath.Join("workspace", "child.runbook.yaml")
	handoff := func(summary string) *schema.HandoffSpec {
		return &schema.HandoffSpec{Handoff: schema.HandoffConfig{
			Runbook: "target.runbook.yaml", Reason: schema.HandoffReason{Code: "continue", Summary: summary},
			With: map[string]string{"server": "${server}"},
		}}
	}
	outer := func(id string) engine.ResolvedStep {
		return engine.ResolvedStep{
			ID: id, Kind: "include", Origin: rootPath,
			Spec: &schema.IncludeSpec{ResolvedRunbookPath: outerPath},
		}
	}
	shared := func(parent string, inputType string) engine.ResolvedStep {
		return engine.ResolvedStep{
			ID: "shared", Kind: "include", Origin: outerPath, Depth: 1,
			ParentID: parent, ParentKind: "include",
			Spec: &schema.IncludeSpec{
				ResolvedRunbookPath: childPath,
				ResolvedInputs:      map[string]*schema.Input{"server": {Type: inputType}},
			},
		}
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: rootPath,
		Steps: []engine.ResolvedStep{
			outer("outer-a"),
			shared("outer-a", "secret"),
			{ID: "continue", Kind: "handoff", Spec: handoff("First"), Origin: childPath, Depth: 2,
				ParentID: "shared", ParentKind: "include"},
			outer("outer-b"),
			shared("outer-b", "string"),
			{ID: "continue", Kind: "handoff", Spec: handoff("Second"), Origin: childPath, Depth: 2,
				ParentID: "shared", ParentKind: "include"},
		},
	}
	callPath := []engine.DebugCallFrame{
		{StepID: "outer-b", RunbookPath: outerPath},
		{StepID: "shared", RunbookPath: childPath},
	}
	frameID := session.DigestJSON("outer-b/shared")
	state := engine.RunState{RunbookPath: rootPath, ExecutionFrames: map[string]*engine.ExecutionFrameState{
		frameID: {
			SchemaVersion: engine.ExecutionFrameStateSchemaV1, FrameID: frameID,
			ParentQualifiedNodeID: "outer-b/shared", ParentStepID: "shared", Kind: "include",
			CallPath: callPath, Invocation: 1, StepCount: 1, StepIDs: []string{"continue"},
			NextStepIndex: 0, WorkingVars: map[string]any{"server": "db02"},
			Status: engine.ExecutionFrameStatusActive,
		},
	}}
	request := engine.HandoffRequest{
		TargetRunbook: "target.runbook.yaml", ReasonCode: "continue", ReasonSummary: "Second",
		Context: map[string]any{"server": "db02"}, ContextBindings: map[string]string{"server": "server"},
		QualifiedNodeID: "outer-b/shared/continue", CallPath: callPath, StepID: "continue",
		FrameID: frameID, FrameStepIndex: 0, Invocation: 1, RetryAttempt: 1,
	}
	if err := validateHandoffRequestAgainstState(plan, state, request); err != nil {
		t.Fatalf("validate repeated nested handoff: %v", err)
	}
}

func TestSourceOccurrenceFromHandoffPreservesStructuralFrameIdentity(t *testing.T) {
	parentFrameID := session.DigestJSON("parent-frame")
	frameID := session.DigestJSON("child-frame")
	state := engine.RunState{ExecutionFrames: map[string]*engine.ExecutionFrameState{
		parentFrameID: {
			SchemaVersion: engine.ExecutionFrameStateSchemaV1, FrameID: parentFrameID,
			ParentQualifiedNodeID: "iterate", ParentStepID: "iterate", Kind: "iterate",
			CallPath: []engine.DebugCallFrame{{StepID: "iterate"}}, Invocation: 2,
			IterationIndex: 2, StepCount: 1, StepIDs: []string{"branch"}, NextStepIndex: 0,
			Status: engine.ExecutionFrameStatusActive,
		},
		frameID: {
			SchemaVersion: engine.ExecutionFrameStateSchemaV1, FrameID: frameID, ParentFrameID: parentFrameID,
			ParentQualifiedNodeID: "iterate/branch", ParentStepID: "branch", Kind: "branch",
			CallPath:    []engine.DebugCallFrame{{StepID: "iterate"}, {StepID: "branch"}},
			BranchLabel: "matched", IterationIndex: 2, Invocation: 2,
			StepCount: 1, StepIDs: []string{"continue"}, NextStepIndex: 0,
			Status: engine.ExecutionFrameStatusActive,
		},
	}}
	request := engine.HandoffRequest{
		QualifiedNodeID: "iterate/branch/continue",
		CallPath:        []engine.DebugCallFrame{{StepID: "iterate"}, {StepID: "branch"}}, StepID: "continue",
		FrameID: frameID, FrameStepIndex: 0, Invocation: 2, RetryAttempt: 1, OccurrenceSequence: 7,
	}
	occurrence, err := sourceOccurrenceFromHandoff("run-id", state, request)
	if err != nil {
		t.Fatalf("sourceOccurrenceFromHandoff: %v", err)
	}
	if occurrence.FrameID != frameID || occurrence.FrameStepIndex != 0 ||
		occurrence.BranchLabel != "matched" || occurrence.IterationIndex != 2 ||
		len(occurrence.FrameStack) != 2 || occurrence.FrameStack[0] != parentFrameID ||
		occurrence.FrameStack[1] != frameID {
		t.Fatalf("structural source occurrence = %#v", occurrence)
	}
}

func TestBindHandoffGraphToImmutableTargetPlan(t *testing.T) {
	plan := &engine.ExecutionPlan{
		RunbookPath: filepath.Join("root", "target.runbook.yaml"),
		Steps:       []engine.ResolvedStep{{ID: "inspect", Kind: "noop"}},
		Metadata: engine.PlanMetadata{
			RunbookID: "target", RunbookName: "Target", PlanHash: strings.Repeat("a", 64),
		},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	planDigest := session.DigestJSON("immutable target plan")
	graph := handoffValidationGraph(t, plan, "target")
	bound, err := bindHandoffGraph(plan, planDigest, graph)
	if err != nil {
		t.Fatalf("bindHandoffGraph: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(bound, &document); err != nil {
		t.Fatalf("decode bound graph: %v", err)
	}
	if document["execution_plan_hash"] != planDigest {
		t.Fatalf("execution_plan_hash = %#v", document["execution_plan_hash"])
	}
	if err := validateBoundHandoffGraph(plan, planDigest, bound); err != nil {
		t.Fatalf("validateBoundHandoffGraph: %v", err)
	}

	mismatched := handoffValidationGraph(t, plan, "different")
	if _, err := bindHandoffGraph(plan, planDigest, mismatched); err == nil {
		t.Fatal("bindHandoffGraph accepted a different runbook identity")
	}
	var emptyGraph map[string]any
	if err := json.Unmarshal(handoffValidationGraph(t, plan, "target"), &emptyGraph); err != nil {
		t.Fatalf("decode empty graph fixture: %v", err)
	}
	emptyGraph["nodes"] = []any{}
	emptyData, err := json.Marshal(emptyGraph)
	if err != nil {
		t.Fatalf("encode empty graph: %v", err)
	}
	if _, err := bindHandoffGraph(plan, planDigest, emptyData); err == nil {
		t.Fatal("bindHandoffGraph accepted an empty graph for a nonempty plan")
	}
	document["execution_plan_hash"] = strings.Repeat("b", 64)
	tampered, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode tampered graph: %v", err)
	}
	if err := validateBoundHandoffGraph(plan, planDigest, tampered); err == nil {
		t.Fatal("validateBoundHandoffGraph accepted a different plan hash")
	}
	document["execution_plan_hash"] = planDigest
	document["bound_content_hash"] = session.DigestJSON("forged graph content")
	tampered, err = json.Marshal(document)
	if err != nil {
		t.Fatalf("encode content-hash tamper: %v", err)
	}
	if err := validateBoundHandoffGraph(plan, planDigest, tampered); err == nil {
		t.Fatal("validateBoundHandoffGraph accepted a forged content hash")
	}

	forgedIdentity := handoffValidationGraph(t, plan, "target")
	var forgedDocument map[string]any
	if err := json.Unmarshal(forgedIdentity, &forgedDocument); err != nil {
		t.Fatalf("decode forged identity graph: %v", err)
	}
	forgedDocument["nodes"].([]any)[0].(map[string]any)["id"] = "forged"
	forgedIdentity, err = json.Marshal(forgedDocument)
	if err != nil {
		t.Fatalf("encode forged identity graph: %v", err)
	}
	if _, err := bindHandoffGraph(plan, planDigest, forgedIdentity); err == nil {
		t.Fatal("bindHandoffGraph accepted a node whose outer and runtime identities differ")
	}
	consistentForgery := handoffValidationGraph(t, plan, "target")
	if err := json.Unmarshal(consistentForgery, &forgedDocument); err != nil {
		t.Fatalf("decode consistent forgery graph: %v", err)
	}
	forgedNode := forgedDocument["nodes"].([]any)[0].(map[string]any)
	forgedNode["id"] = "forged/inspect"
	forgedData := forgedNode["data"].(map[string]any)
	forgedData["id"] = "forged/inspect"
	forgedData["call_path"] = []any{"forged"}
	consistentForgery, err = json.Marshal(forgedDocument)
	if err != nil {
		t.Fatalf("encode consistent forgery graph: %v", err)
	}
	if _, err := bindHandoffGraph(plan, planDigest, consistentForgery); err == nil {
		t.Fatal("bindHandoffGraph accepted a self-consistent but non-executable call path")
	}
	forgedFrame := handoffValidationGraph(t, plan, "target")
	if err := json.Unmarshal(forgedFrame, &forgedDocument); err != nil {
		t.Fatalf("decode forged frame graph: %v", err)
	}
	forgedDocument["frames"].([]any)[0].(map[string]any)["id"] = "frame:forged"
	forgedDocument["nodes"].([]any)[0].(map[string]any)["data"].(map[string]any)["frame_id"] = "frame:forged"
	forgedFrame, err = json.Marshal(forgedDocument)
	if err != nil {
		t.Fatalf("encode forged frame graph: %v", err)
	}
	if _, err := bindHandoffGraph(plan, planDigest, forgedFrame); err == nil {
		t.Fatal("bindHandoffGraph accepted a forged root frame identity")
	}

	inputPlan := *plan
	inputPlan.Inputs = map[string]*schema.Input{
		"server": {Type: "string", Required: true, Description: "Server name"},
	}
	missingInputs := handoffValidationGraph(t, &inputPlan, "target")
	if err := json.Unmarshal(missingInputs, &forgedDocument); err != nil {
		t.Fatalf("decode missing-input graph: %v", err)
	}
	delete(forgedDocument, "inputs")
	missingInputs, _ = rehashHandoffGraph(t, forgedDocument)
	if _, err := bindHandoffGraph(&inputPlan, planDigest, missingInputs); err == nil {
		t.Fatal("bindHandoffGraph accepted GraphJSON missing target input declarations")
	}
	largeDefaultPlan := *plan
	largeDefaultPlan.Inputs = map[string]*schema.Input{
		"sequence": {Type: "integer", Default: int64(9007199254740993)},
	}
	largeDefaultGraph := handoffValidationGraph(t, &largeDefaultPlan, "target")
	if err := json.Unmarshal(largeDefaultGraph, &forgedDocument); err != nil {
		t.Fatalf("decode large-default graph: %v", err)
	}
	forgedDocument["inputs"] = []any{map[string]any{
		"name": "sequence", "type": "integer", "default": int64(9007199254740993),
	}}
	largeDefaultGraph = encodeHandoffGraphWithHash(t, &largeDefaultPlan, forgedDocument)
	if _, err := bindHandoffGraph(&largeDefaultPlan, planDigest, largeDefaultGraph); err != nil {
		t.Fatalf("bindHandoffGraph changed a large integer default: %v", err)
	}
}

func TestBindHandoffGraphRejectsForgedTopologyAndHash(t *testing.T) {
	plan := &engine.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "first", Kind: "noop", Spec: &schema.NoopSpec{}},
			{ID: "second", Kind: "noop", Spec: &schema.NoopSpec{}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	node := func(id string, order int) map[string]any {
		return map[string]any{
			"id": id, "type": "step", "position": map[string]any{"x": 0, "y": 0},
			"data": map[string]any{
				"id": id, "step_id": id, "kind": "noop", "frame_id": "frame:root",
				"call_path": []any{}, "title": "", "group_id": "", "order": order,
			},
		}
	}
	document := map[string]any{
		"schema_version": "1", "hash": strings.Repeat("a", 64),
		"runbook": map[string]any{"id": "target", "name": "Target", "path": plan.RunbookPath},
		"frames": []any{map[string]any{
			"id": "frame:root", "runbook_id": "target", "runbook_path": plan.RunbookPath, "depth": 0,
		}},
		"nodes": []any{node("first", 0), node("second", 1)}, "groups": []any{}, "edges": []any{},
	}
	encoded := encodeHandoffGraphWithHash(t, plan, document)
	if _, err := bindHandoffGraph(plan, session.DigestJSON("plan"), encoded); err == nil {
		t.Fatal("bindHandoffGraph accepted a graph missing its sequence edge")
	}

	single := engine.ExecutionPlan{
		RunbookPath: plan.RunbookPath,
		Steps:       []engine.ResolvedStep{{ID: "inspect", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    plan.Metadata,
	}
	if err := planner.ValidateExecutionPlan(&single); err != nil {
		t.Fatalf("ValidateExecutionPlan single: %v", err)
	}
	forgedGroup := handoffValidationGraph(t, &single, "target")
	var forged map[string]any
	if err := json.Unmarshal(forgedGroup, &forged); err != nil {
		t.Fatalf("decode forged-group graph: %v", err)
	}
	forged["groups"] = []any{map[string]any{
		"id": "group:inspect:iterate-body:0", "kind": "iterate-body",
		"parent_node_id": "inspect", "frame_id": "frame:root",
	}}
	forgedGroup = encodeHandoffGraphWithHash(t, &single, forged)
	if _, err := bindHandoffGraph(&single, session.DigestJSON("plan"), forgedGroup); err == nil {
		t.Fatal("bindHandoffGraph accepted an impossible structural group")
	}

	arbitraryHash := handoffValidationGraph(t, &single, "target")
	if err := json.Unmarshal(arbitraryHash, &forged); err != nil {
		t.Fatalf("decode arbitrary-hash graph: %v", err)
	}
	forged["hash"] = strings.Repeat("f", 64)
	arbitraryHash, err := json.Marshal(forged)
	if err != nil {
		t.Fatalf("encode arbitrary-hash graph: %v", err)
	}
	if _, err := bindHandoffGraph(&single, session.DigestJSON("plan"), arbitraryHash); err == nil {
		t.Fatal("bindHandoffGraph accepted an arbitrary GraphJSON content hash")
	}
}

func TestBindHandoffGraphRejectsRehashedDefinitionForgery(t *testing.T) {
	plan := &engine.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "inspect", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata:    engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	graph := handoffValidationGraph(t, plan, "target")
	var document map[string]any
	if err := json.Unmarshal(graph, &document); err != nil {
		t.Fatalf("decode graph: %v", err)
	}
	nodeData := document["nodes"].([]any)[0].(map[string]any)["data"].(map[string]any)
	nodeData["title"] = "Forged title"
	nodeData["details"] = map[string]any{"kind": "noop", "content": "forged definition"}
	forged, _ := rehashHandoffGraph(t, document)
	if _, err := bindHandoffGraph(plan, session.DigestJSON("plan"), forged); err == nil {
		t.Fatal("bindHandoffGraph accepted rehashed definition metadata")
	}
	graph = handoffValidationGraph(t, plan, "target")
	if err := json.Unmarshal(graph, &document); err != nil {
		t.Fatalf("decode frame graph: %v", err)
	}
	document["frames"].([]any)[0].(map[string]any)["content_hash"] = strings.Repeat("f", 64)
	forged, _ = rehashHandoffGraph(t, document)
	if _, err := bindHandoffGraph(plan, session.DigestJSON("plan"), forged); err == nil {
		t.Fatal("bindHandoffGraph accepted rehashed frame content metadata")
	}
}

func TestBindHandoffGraphPreservesLargeDefinitionNumber(t *testing.T) {
	const sequence = int64(9007199254740993)
	plan := &engine.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "inspect", Kind: "tool", Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
				Name: "diagnostics", Action: "inspect", Args: map[string]any{"sequence": sequence},
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	document := map[string]any{
		"schema_version": "1", "runbook": map[string]any{
			"id": "target", "name": "Target", "path": plan.RunbookPath,
		},
		"frames": []any{map[string]any{
			"id": "frame:root", "runbook_id": "target", "runbook_path": plan.RunbookPath, "depth": 0,
		}},
		"nodes": []any{map[string]any{
			"id": "inspect", "type": "action", "position": map[string]any{"x": 0, "y": 0},
			"data": map[string]any{
				"id": "inspect", "step_id": "inspect", "kind": "tool", "title": "",
				"group_id": "", "frame_id": "frame:root", "call_path": []any{}, "order": 0,
				"tool_name": "diagnostics", "tool_action": "inspect",
				"details": map[string]any{
					"kind": "tool", "tool": "diagnostics", "action": "inspect",
					"arguments": []any{map[string]any{"name": "sequence", "value": sequence}},
				},
			},
		}},
		"groups": []any{}, "edges": []any{},
	}
	graph := encodeHandoffGraphWithHash(t, plan, document)
	if _, err := bindHandoffGraph(plan, session.DigestJSON("plan"), graph); err != nil {
		t.Fatalf("bindHandoffGraph large definition number: %v", err)
	}
}

func TestBindHandoffGraphAllowsConditionOnlyBranchArms(t *testing.T) {
	branch := &schema.BranchSpec{Branches: []schema.BranchArm{
		{Condition: "mode == 'primary'", Steps: []schema.FlowNode{{Step: &schema.Step{
			ID: "primary", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
		}}}},
		{Condition: "mode == 'secondary'", Steps: []schema.FlowNode{{Step: &schema.Step{
			ID: "secondary", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
		}}}},
	}}
	plan := &engine.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "route", Kind: "branch", Spec: branch, Depth: 0, DisplayOrder: 0},
			{ID: "primary", Kind: "noop", Spec: &schema.NoopSpec{}, Depth: 1, DisplayOrder: 1,
				ParentID: "route", ParentKind: "branch"},
			{ID: "secondary", Kind: "noop", Spec: &schema.NoopSpec{}, Depth: 1, DisplayOrder: 2,
				ParentID: "route", ParentKind: "branch"},
		},
		Metadata: engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	graph, err := finalizeExecutionPlanGraph(plan, nil, nil)
	if err != nil {
		t.Fatalf("finalizeExecutionPlanGraph: %v", err)
	}
	var rendered graphjson.Document
	if err := decodeHandoffJSON(graph, &rendered); err != nil {
		t.Fatalf("decode condition-only graph: %v", err)
	}
	if len(rendered.Groups) != 2 || rendered.Groups[0].Label != "mode == 'primary'" ||
		rendered.Groups[1].Label != "mode == 'secondary'" {
		t.Fatalf("condition-only groups = %#v", rendered.Groups)
	}
	if _, err := bindHandoffGraph(plan, session.DigestJSON("plan"), graph); err != nil {
		t.Fatalf("bindHandoffGraph condition-only branches: %v", err)
	}
}

func TestValidateHandoffTargetArtifactsRejectsProtectedContent(t *testing.T) {
	const protectedValue = "private-ticket-123"
	plan := &engine.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "inspect", Name: protectedValue, Kind: "noop", Spec: &schema.NoopSpec{},
		}},
		GovernanceSource: &schema.GovernanceConfig{Redact: []schema.RedactRule{{
			Pattern: `private-ticket-[0-9]+`, Replace: "<redacted>",
		}}},
		Metadata: engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	planBlob, err := snapshotBlob(plan)
	if err != nil {
		t.Fatalf("snapshotBlob: %v", err)
	}
	graph := handoffValidationGraph(t, plan, "target")
	boundGraph, err := bindHandoffGraph(plan, planBlob.Digest, graph)
	if err != nil {
		t.Fatalf("bindHandoffGraph: %v", err)
	}
	err = validateHandoffTargetArtifacts(engine.DebugProtection{}, plan, nil, planBlob.Data, boundGraph)
	if err == nil || strings.Contains(err.Error(), protectedValue) {
		t.Fatalf("target artifact error = %v, want value-free refusal", err)
	}
}

func TestValidateHandoffTargetArtifactsAllowsSharedRedactionPolicy(t *testing.T) {
	plan := &engine.ExecutionPlan{
		RunbookPath: "target.runbook.yaml",
		Steps:       []engine.ResolvedStep{{ID: "inspect", Kind: "noop", Spec: &schema.NoopSpec{}}},
		GovernanceSource: &schema.GovernanceConfig{Redact: []schema.RedactRule{{
			Pattern: "policy-marker", Replace: "<redacted>",
		}}},
		Metadata: engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	planBlob, err := snapshotBlob(plan)
	if err != nil {
		t.Fatalf("snapshotBlob: %v", err)
	}
	boundGraph, err := bindHandoffGraph(plan, planBlob.Digest, handoffValidationGraph(t, plan, "target"))
	if err != nil {
		t.Fatalf("bindHandoffGraph: %v", err)
	}
	sourceProtection := engine.ExtendDebugProtection(
		engine.DebugProtection{}, nil, nil, internalgovernance.BuildPolicy(plan.GovernanceSource),
	)
	if err := validateHandoffTargetArtifacts(
		sourceProtection, plan, nil, planBlob.Data, boundGraph,
	); err != nil {
		t.Fatalf("shared policy metadata rejected: %v", err)
	}
}

func TestValidateHandoffTargetArtifactsRejectsSensitiveDeclarations(t *testing.T) {
	for _, test := range []struct {
		name    string
		inputs  map[string]*schema.Input
		outputs map[string]*schema.Output
	}{
		{
			name: "nested input default",
			inputs: map[string]*schema.Input{"config": {
				Type: "object", Default: map[string]any{"password": "p@ss"},
			}},
		},
		{
			name: "non-secret credential output",
			outputs: map[string]*schema.Output{
				"api_token": {Type: "string", Value: "p@ss"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := &engine.ExecutionPlan{
				RunbookPath: "target.runbook.yaml",
				Steps:       []engine.ResolvedStep{{ID: "inspect", Kind: "noop", Spec: &schema.NoopSpec{}}},
				Inputs:      test.inputs, Outputs: test.outputs,
				Metadata: engine.PlanMetadata{RunbookID: "target", RunbookName: "Target"},
			}
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatalf("ValidateExecutionPlan: %v", err)
			}
			planBlob, err := snapshotBlob(plan)
			if err != nil {
				t.Fatalf("snapshotBlob: %v", err)
			}
			err = validateHandoffTargetArtifacts(
				engine.DebugProtection{}, plan, nil, planBlob.Data, json.RawMessage(`{}`),
			)
			if err == nil || strings.Contains(err.Error(), "p@ss") {
				t.Fatalf("sensitive declaration error = %v, want value-free refusal", err)
			}
		})
	}
}

func TestBindHandoffGraphAllowsRepeatedChildPathWithQualifiedFrames(t *testing.T) {
	rootPath := "root.runbook.yaml"
	childPath := "child.runbook.yaml"
	includeSpec := func() *schema.IncludeSpec {
		return &schema.IncludeSpec{
			Include: schema.IncludeConfig{Runbook: childPath}, ResolvedRunbookPath: childPath,
			ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{ID: "inspect", Type: schema.StepTypeNoop}}},
		}
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: rootPath,
		Steps: []engine.ResolvedStep{
			{ID: "include-a", Kind: "include", Spec: includeSpec(), Origin: rootPath, DisplayOrder: 0},
			{ID: "inspect", Kind: "noop", Spec: &schema.NoopSpec{}, Origin: childPath, Depth: 1,
				ParentID: "include-a", ParentKind: "include", DisplayOrder: 1},
			{ID: "include-b", Kind: "include", Spec: includeSpec(), Origin: rootPath, DisplayOrder: 2},
			{ID: "inspect", Kind: "noop", Spec: &schema.NoopSpec{}, Origin: childPath, Depth: 1,
				ParentID: "include-b", ParentKind: "include", DisplayOrder: 3},
		},
		Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	encoded, err := finalizeExecutionPlanGraph(plan, nil, nil)
	if err != nil {
		t.Fatalf("finalizeExecutionPlanGraph: %v", err)
	}
	var rendered graphjson.Document
	if err := decodeHandoffJSON(encoded, &rendered); err != nil {
		t.Fatalf("decode repeated child graph: %v", err)
	}
	if len(rendered.Frames) != 3 || rendered.Frames[1].ID != "frame:include-a" ||
		rendered.Frames[2].ID != "frame:include-b" {
		t.Fatalf("repeated child frames = %#v", rendered.Frames)
	}
	if _, err := bindHandoffGraph(plan, session.DigestJSON("plan"), encoded); err != nil {
		t.Fatalf("bind repeated child graph: %v", err)
	}
}

func TestHandoffTargetPathUsesInnermostDeclaringRunbook(t *testing.T) {
	root := filepath.Join("workspace", "root.runbook.yaml")
	request := engine.HandoffRequest{CallPath: []engine.DebugCallFrame{
		{StepID: "include", RunbookPath: filepath.Join("workspace", "sub", "child.runbook.yaml")},
	}}
	declaring := handoffDeclaringRunbookPath(root, request.CallPath)
	resolved, err := staticHandoffPath(declaring, "target.runbook.yaml")
	if err != nil {
		t.Fatalf("staticHandoffPath: %v", err)
	}
	want := filepath.Join("workspace", "sub", "target.runbook.yaml")
	if resolved != want {
		t.Fatalf("resolved path = %q, want %q", resolved, want)
	}
}

func handoffValidationGraph(t *testing.T, plan *engine.ExecutionPlan, runbookID string) json.RawMessage {
	t.Helper()
	encoded, err := executionPlanGraph(plan, nil)
	if err != nil {
		t.Fatalf("executionPlanGraph: %v", err)
	}
	var document map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode generated graph: %v", err)
	}
	plan.Metadata.GraphContentHash = document["hash"].(string)
	if runbookID == plan.Metadata.RunbookID {
		return encoded
	}
	document["runbook"].(map[string]any)["id"] = runbookID
	document["frames"].([]any)[0].(map[string]any)["runbook_id"] = runbookID
	forged, _ := rehashHandoffGraph(t, document)
	return forged
}

func encodeHandoffGraphWithHash(
	t *testing.T,
	plan *engine.ExecutionPlan,
	document map[string]any,
) json.RawMessage {
	t.Helper()
	encoded, hash := rehashHandoffGraph(t, document)
	plan.Metadata.GraphContentHash = hash
	return encoded
}

func rehashHandoffGraph(t *testing.T, document map[string]any) (json.RawMessage, string) {
	t.Helper()
	document["hash"] = strings.Repeat("0", 64)
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode graph for hashing: %v", err)
	}
	var rendered graphjson.Document
	if err := decodeHandoffJSON(encoded, &rendered); err != nil {
		t.Fatalf("decode graph for hashing: %v", err)
	}
	canonical, err := graphDocumentFromHandoffGraph(rendered)
	if err != nil {
		t.Fatalf("reconstruct graph for hashing: %v", err)
	}
	hash, err := canonical.ContentHash()
	if err != nil {
		t.Fatalf("hash graph: %v", err)
	}
	document["hash"] = hash
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatalf("encode hashed graph: %v", err)
	}
	return encoded, hash
}
