package sessioncoordinator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type rejectingRunbookLoader struct{}

func (rejectingRunbookLoader) Load(context.Context, string) (*parserpkg.ParsedRunbook, error) {
	return nil, errors.New("unexpected static include load")
}

type rejectingToolRegistry struct{}

func (rejectingToolRegistry) Lookup(context.Context, string, string) (*schema.ToolDef, error) {
	return nil, plannerpkg.ErrToolNotFound
}

func TestExecutionPlanGraphMatchesProductionBuilderForStructuralDynamicFlow(t *testing.T) {
	source := []byte(`apiVersion: yawr.runbook/v1
id: structured
name: Structured
flow:
  - iterate:
      id: each_item
      over: "${items}"
      as: item
      steps:
        - step:
            id: dynamic_child
            type: include
            include:
              runbook_ref: "${target}"
              resolve_from: catalog
  - parallel:
      id: fan_out
      branches:
        - label: nested
          steps:
            - iterate:
                id: nested_each
                over: "${nested_items}"
                as: nested_item
                steps:
                  - step:
                      id: nested_work
                      type: noop
        - label: leaf
          steps:
            - step:
                id: leaf_work
                type: noop
      join:
        wait_for: all
        on_failure: fail
  - step:
      id: route
      type: branch
      branches:
        - condition: "mode == 'primary'"
          steps:
            - step:
                id: primary
                type: noop
        - else: true
          steps:
            - step:
                id: secondary
                type: noop
`)
	parserImpl, err := internalparser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New parser: %v", err)
	}
	parsed, err := parserImpl.ParseBytes(context.Background(), source)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	parsed.Source = "structured.runbook.yaml"
	plannerImpl := planner.New(plannerpkg.Config{
		Loader: rejectingRunbookLoader{}, Tools: rejectingToolRegistry{},
	})
	plan, err := plannerImpl.Plan(context.Background(), parsed)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	document, err := (&graphdoc.Builder{Recurse: true}).Build(context.Background(), parsed)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	productionGraph, err := graphjson.Render(document)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := finalizeExecutionPlanGraph(plan, nil, productionGraph); err != nil {
		t.Fatalf("production graph parity: %v", err)
	}
}

func TestDynamicRevisionPlanSupportsNestedAndRepeatedResolutions(t *testing.T) {
	t.Run("nested dynamic include", func(t *testing.T) {
		outerPath := "C:/runbooks/outer.runbook.yaml"
		innerPath := "C:/runbooks/inner.runbook.yaml"
		outerClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
			ID: "inner", Type: schema.StepTypeInclude,
			IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "${inner}", ResolveFrom: schema.ResolveFromCatalog,
			}},
		}}})
		innerClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
			ID: "child_work", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
		}}})
		plan := dynamicRevisionTestPlan(t, "outer")
		state := engine.RunState{Plan: plan, DynamicIncludes: map[string]*engine.DynamicIncludeResolutionState{
			"outer": dynamicRevisionTestResolution(
				"outer", nil, 1, 1, "pkg/outer", outerPath, outerClosure, time.Unix(1, 0),
			),
			"inner": dynamicRevisionTestResolution(
				"inner", []engine.DebugCallFrame{{StepID: "outer", RunbookPath: outerPath}},
				1, 2, "pkg/inner", innerPath, innerClosure, time.Unix(2, 0),
			),
		}}
		revised, resolutions, err := dynamicRevisionPlan(state)
		if err != nil {
			t.Fatalf("dynamicRevisionPlan: %v", err)
		}
		if len(resolutions) != 2 || len(revised.Metadata.DynamicIncludes) != 2 || len(revised.Steps) != 3 ||
			revised.Steps[0].ID != "outer" || revised.Steps[1].ID != "inner" || revised.Steps[2].ID != "child_work" {
			t.Fatalf("nested revised plan = %#v, resolutions = %#v", revised.Steps, resolutions)
		}
	})

	t.Run("repeated authored site", func(t *testing.T) {
		firstClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
			ID: "first_child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
		}}})
		secondClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
			ID: "second_child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
		}}})
		plan := dynamicRevisionTestPlan(t, "child")
		staticInclude := &schema.IncludeSpec{
			Include:             schema.IncludeConfig{Runbook: "static.runbook.yaml"},
			ResolvedRunbookPath: "C:/runbooks/static.runbook.yaml",
			ResolvedRunbookID:   "static", ResolvedRunbookName: "Static",
			ResolvedRunbookContentHash: strings.Repeat("b", 64),
			ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{
				ID: "first_child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
			}}},
		}
		plan.Steps = append(plan.Steps,
			engine.ResolvedStep{ID: "child@r1", Kind: "include", Spec: staticInclude, DisplayOrder: 1},
			engine.ResolvedStep{ID: "first_child", Kind: "noop", Spec: &schema.NoopSpec{},
				Depth: 1, DisplayOrder: 2, ParentID: "child@r1", ParentKind: "include",
				Origin: staticInclude.ResolvedRunbookPath},
		)
		plan.Metadata.PlanHash = ""
		plan.Validation = nil
		if err := planner.ValidateExecutionPlan(plan); err != nil {
			t.Fatalf("ValidateExecutionPlan collision plan: %v", err)
		}
		state := engine.RunState{Plan: plan, DynamicIncludes: map[string]*engine.DynamicIncludeResolutionState{
			"first": dynamicRevisionTestResolution(
				"child", nil, 1, 1, "pkg/first", "C:/runbooks/first.runbook.yaml",
				firstClosure, time.Unix(1, 0),
			),
			"second": dynamicRevisionTestResolution(
				"child", nil, 2, 2, "pkg/second", "C:/runbooks/second.runbook.yaml",
				secondClosure, time.Unix(1, 0),
			),
		}}
		revised, resolutions, err := dynamicRevisionPlan(state)
		if err != nil {
			t.Fatalf("dynamicRevisionPlan: %v", err)
		}
		if len(resolutions) != 2 || len(revised.Metadata.DynamicIncludes) != 2 {
			t.Fatalf("repeated revised plan = %#v, resolutions = %#v", revised.Steps, resolutions)
		}
		graph, err := executionPlanGraph(revised, resolutions)
		if err != nil {
			t.Fatalf("executionPlanGraph: %v", err)
		}
		if !strings.Contains(string(graph), "first_child") || !strings.Contains(string(graph), "second_child") {
			t.Fatalf("latest dynamic graph dropped prior definition: %s", graph)
		}
		var document graphjson.Document
		if err := decodeHandoffJSON(graph, &document); err != nil {
			t.Fatalf("decode repeated graph: %v", err)
		}
		seen := make(map[string]bool, len(document.Nodes))
		firstChildren := 0
		for _, node := range document.Nodes {
			if seen[node.ID] {
				t.Fatalf("repeated graph contains duplicate node ID %q", node.ID)
			}
			seen[node.ID] = true
			if node.Data["step_id"] == "first_child" {
				firstChildren++
			}
		}
		if firstChildren != 2 {
			t.Fatalf("first_child occurrences = %d, want static and historical dynamic", firstChildren)
		}
	})
}

func TestDynamicRevisionPlanPreservesInterleavedNestedOccurrences(t *testing.T) {
	outerStep := func() *schema.Step {
		return &schema.Step{ID: "outer", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{RunbookRef: "${target}", ResolveFrom: schema.ResolveFromCatalog},
		}}
	}
	iterate := &schema.IterateNode{
		ID: "each", Over: "${items}", As: "item", Steps: []schema.FlowNode{{Step: outerStep()}},
	}
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "each", Kind: "iterate", Spec: iterate},
			{ID: "outer", Kind: "include", Spec: outerStep().IncludeSpec,
				Depth: 1, ParentID: "each", ParentKind: "iterate"},
		},
		Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	outerAPath := "C:/runbooks/outer-a.runbook.yaml"
	outerAClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
		ID: "inner", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "${inner}", ResolveFrom: schema.ResolveFromCatalog,
		}},
	}}})
	outerBClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
		ID: "b-work", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	innerClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
		ID: "inner-work", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	outerA := dynamicRevisionTestResolution(
		"outer", []engine.DebugCallFrame{{StepID: "each"}}, 1, 1,
		"pkg/outer-a", outerAPath, outerAClosure, time.Unix(1, 0),
	)
	outerA.Pin.StructuralPath = []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 1, Invocation: 1,
	}}
	outerB := dynamicRevisionTestResolution(
		"outer", []engine.DebugCallFrame{{StepID: "each"}}, 1, 2,
		"pkg/outer-b", "C:/runbooks/outer-b.runbook.yaml", outerBClosure, time.Unix(2, 0),
	)
	outerB.Pin.StructuralPath = []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 2, Invocation: 1,
	}}
	innerA := dynamicRevisionTestResolution(
		"inner", []engine.DebugCallFrame{{StepID: "each"}, {StepID: "outer", RunbookPath: outerAPath}}, 1, 3,
		"pkg/inner-a", "C:/runbooks/inner-a.runbook.yaml", innerClosure, time.Unix(3, 0),
	)
	innerA.Pin.StructuralPath = []schema.DynamicIncludeFrameIdentity{
		{QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 1, Invocation: 1},
		{QualifiedNodeID: "each/outer", Kind: "include", Invocation: 1},
	}
	revised, resolutions, err := dynamicRevisionPlan(engine.RunState{
		Plan: plan, DynamicIncludes: map[string]*engine.DynamicIncludeResolutionState{
			"outer-a": outerA, "outer-b": outerB, "inner-a": innerA,
		},
	})
	if err != nil {
		t.Fatalf("dynamicRevisionPlan: %v", err)
	}
	if len(resolutions) != 3 || len(revised.Metadata.DynamicIncludes) != 3 {
		t.Fatalf("interleaved revisions = %#v, metadata = %#v", resolutions, revised.Metadata.DynamicIncludes)
	}
	graph, err := executionPlanGraph(revised, resolutions)
	if err != nil {
		t.Fatalf("executionPlanGraph: %v", err)
	}
	if !strings.Contains(string(graph), "b-work") || !strings.Contains(string(graph), "inner-work") {
		t.Fatalf("interleaved graph lost an occurrence: %s", graph)
	}
	var document graphjson.Document
	if err := decodeHandoffJSON(graph, &document); err != nil {
		t.Fatalf("decode interleaved graph: %v", err)
	}
	innerNodeID, innerWorkID := "", ""
	for _, node := range document.Nodes {
		switch node.Data["step_id"] {
		case "inner":
			if innerNodeID != "" {
				t.Fatal("interleaved graph contains multiple historical inner parents")
			}
			innerNodeID = node.ID
		case "inner-work":
			innerWorkID = node.ID
		}
	}
	if innerNodeID == "" || innerWorkID == "" {
		t.Fatalf("interleaved graph nodes = %#v", document.Nodes)
	}
	for _, edge := range document.Edges {
		if edge.Target == innerWorkID {
			if edge.Source != innerNodeID {
				t.Fatalf("inner-work parent = %q, want A inner %q", edge.Source, innerNodeID)
			}
			return
		}
	}
	t.Fatal("interleaved graph has no edge into inner-work")
}

func TestDynamicRevisionPlanHandlesNestedThenRepeatedOuter(t *testing.T) {
	outerPath := "C:/runbooks/outer-a.runbook.yaml"
	outerClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
		ID: "inner", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{
			RunbookRef: "${inner}", ResolveFrom: schema.ResolveFromCatalog,
		}},
	}}})
	innerClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
		ID: "inner-work", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	outerBClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
		ID: "b-work", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	outerA := dynamicRevisionTestResolution(
		"outer", nil, 1, 1, "pkg/outer-a", outerPath, outerClosure, time.Unix(1, 0),
	)
	innerA := dynamicRevisionTestResolution(
		"inner", []engine.DebugCallFrame{{StepID: "outer", RunbookPath: outerPath}}, 1, 2,
		"pkg/inner-a", "C:/runbooks/inner-a.runbook.yaml", innerClosure, time.Unix(2, 0),
	)
	innerA.Pin.StructuralPath = []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "outer", Kind: "include", Invocation: 1,
	}}
	outerB := dynamicRevisionTestResolution(
		"outer", nil, 2, 3, "pkg/outer-b", "C:/runbooks/outer-b.runbook.yaml", outerBClosure, time.Unix(3, 0),
	)
	revised, resolutions, err := dynamicRevisionPlan(engine.RunState{
		Plan: dynamicRevisionTestPlan(t, "outer"),
		DynamicIncludes: map[string]*engine.DynamicIncludeResolutionState{
			"outer-a": outerA, "inner-a": innerA, "outer-b": outerB,
		},
	})
	if err != nil {
		t.Fatalf("dynamicRevisionPlan: %v", err)
	}
	if len(revised.Steps) != 2 || revised.Steps[1].ID != "b-work" || len(revised.Metadata.DynamicIncludes) != 3 {
		t.Fatalf("latest executable view = %#v, metadata = %#v", revised.Steps, revised.Metadata.DynamicIncludes)
	}
	graph, err := executionPlanGraph(revised, resolutions)
	if err != nil {
		t.Fatalf("executionPlanGraph: %v", err)
	}
	if !strings.Contains(string(graph), "inner-work") || !strings.Contains(string(graph), "b-work") {
		t.Fatalf("historical graph lost an occurrence: %s", graph)
	}
}

func TestDynamicRevisionPlanMatchesRuntimeOnlyStructuralContainers(t *testing.T) {
	dynamicStep := func() *schema.Step {
		return &schema.Step{
			ID: "child", Type: schema.StepTypeInclude,
			IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "${target}", ResolveFrom: schema.ResolveFromCatalog,
			}},
		}
	}
	closure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
		ID: "child_work", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	for _, test := range []struct {
		name       string
		parentKind string
		parentSpec engine.StepSpec
		label      string
	}{
		{
			name: "parallel", parentKind: "parallel", label: "left",
			parentSpec: &schema.ParallelNode{ID: "container", Branches: []schema.ParallelBranch{{
				Label: "left", Steps: []schema.FlowNode{{Step: dynamicStep()}},
			}}},
		},
		{
			name: "compensate", parentKind: "compensate",
			parentSpec: &schema.CompensateSpec{Compensate: schema.CompensateConfig{
				On: "failure", Steps: []schema.FlowNode{{Step: dynamicStep()}},
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := &engine.ExecutionPlan{
				RunbookPath: "root.runbook.yaml",
				Steps: []engine.ResolvedStep{
					{ID: "container", Kind: test.parentKind, Spec: test.parentSpec, DisplayOrder: 0},
					{ID: "child", Kind: "include", Spec: dynamicStep().IncludeSpec,
						Depth: 1, DisplayOrder: 1, ParentID: "container", ParentKind: test.parentKind,
						BranchLabel: test.label},
				},
				Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
			}
			if err := planner.ValidateExecutionPlan(plan); err != nil {
				t.Fatalf("ValidateExecutionPlan: %v", err)
			}
			callPath := []engine.DebugCallFrame{{StepID: "container"}}
			resolution := dynamicRevisionTestResolution(
				"child", callPath, 1, 1, "pkg/child", "C:/runbooks/child.runbook.yaml",
				closure, time.Unix(1, 0),
			)
			resolution.Pin.StructuralPath = []schema.DynamicIncludeFrameIdentity{{
				QualifiedNodeID: "container", Kind: test.parentKind, BranchLabel: test.label,
			}}
			revised, _, err := dynamicRevisionPlan(engine.RunState{
				Plan: plan, DynamicIncludes: map[string]*engine.DynamicIncludeResolutionState{"resolution": resolution},
			})
			if err != nil {
				t.Fatalf("dynamicRevisionPlan: %v", err)
			}
			if len(revised.Steps) != 3 || revised.Steps[2].ID != "child_work" {
				t.Fatalf("revised steps = %#v", revised.Steps)
			}
		})
	}
}

func TestSettledDynamicRevisionDispatchSelectsExactRepeatedOccurrence(t *testing.T) {
	firstClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
		ID: "first_child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	secondClosure := dynamicRevisionTestClosure(t, []schema.FlowNode{{Step: &schema.Step{
		ID: "second_child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	first := dynamicRevisionTestResolution(
		"child", nil, 1, 1, "pkg/first", "C:/runbooks/first.runbook.yaml", firstClosure, time.Unix(1, 0),
	)
	second := dynamicRevisionTestResolution(
		"child", nil, 2, 2, "pkg/second", "C:/runbooks/second.runbook.yaml", secondClosure, time.Unix(1, 0),
	)
	dispatch := func(resolution *engine.DynamicIncludeResolutionState, occurrence string) *engine.DispatchState {
		encodedPin, err := json.Marshal(resolution.Pin)
		if err != nil {
			t.Fatalf("marshal pin: %v", err)
		}
		return &engine.DispatchState{
			OccurrenceID: occurrence, QualifiedNodeID: resolution.QualifiedNodeID,
			CallPath: resolution.CallPath, StepID: resolution.StepID,
			FrameID: resolution.FrameID, FrameStepIndex: resolution.FrameStepIndex,
			Invocation: resolution.Invocation, EndpointIdentity: "dynamic-include-resolver",
			Status: engine.DispatchStatusSettled, ResultDigest: engine.InteractionPayloadDigest(encodedPin),
		}
	}
	state := engine.RunState{Dispatches: map[string]*engine.DispatchState{
		"first": dispatch(first, "first"), "second": dispatch(second, "second"),
	}}
	matched, err := settledDynamicRevisionDispatch(state, *second)
	if err != nil {
		t.Fatalf("settledDynamicRevisionDispatch: %v", err)
	}
	if matched.OccurrenceID != "second" {
		t.Fatalf("matched dispatch = %#v, want second occurrence", matched)
	}
}

func dynamicRevisionTestPlan(t *testing.T, stepID string) *engine.ExecutionPlan {
	t.Helper()
	plan := &engine.ExecutionPlan{
		RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: stepID, Kind: "include", Spec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				RunbookRef: "${target}", ResolveFrom: schema.ResolveFromCatalog,
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	return plan
}

func dynamicRevisionTestClosure(t *testing.T, nodes []schema.FlowNode) []byte {
	t.Helper()
	closure, err := plansnapshot.EncodeFlowClosure(nodes)
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	return closure
}

func dynamicRevisionTestResolution(
	stepID string,
	callPath []engine.DebugCallFrame,
	invocation int,
	revision int64,
	qualifiedID string,
	absPath string,
	closure []byte,
	committedAt time.Time,
) *engine.DynamicIncludeResolutionState {
	return &engine.DynamicIncludeResolutionState{
		SchemaVersion:   engine.DynamicIncludeResolutionStateSchemaV1,
		ResolutionID:    engine.InteractionPayloadDigest([]byte(qualifiedID + committedAt.String())),
		QualifiedNodeID: engine.DebugNodeID(callPath, stepID), CallPath: callPath,
		StepID: stepID, Invocation: invocation, Revision: revision,
		Pin: schema.LockedDynamicInclude{
			StepID: stepID, QualifiedNodeID: engine.DebugNodeID(callPath, stepID),
			Invocation: invocation, Revision: revision,
			RenderedRef: qualifiedID, QualifiedID: qualifiedID, AbsPath: absPath,
			RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64),
			PackageName: "pkg", PackageVersion: "1.0.0",
			FileDigest:        engine.InteractionPayloadDigest([]byte(absPath)),
			PackageDigest:     engine.InteractionPayloadDigest([]byte("package")),
			ExecutableClosure: closure,
		},
		Status:      engine.DynamicIncludeResolutionStatusActive,
		CommittedAt: committedAt.UTC().Format(time.RFC3339Nano),
	}
}
