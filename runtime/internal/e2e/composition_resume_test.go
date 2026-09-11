package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	internalserve "github.com/ormasoftchile/yawr/runtime/internal/serve"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestE2E_FrozenPublicCompositionDurableOperatorResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const runID = "composition-operator-resume"
	leaf := []schema.FlowNode{
		{Step: &schema.Step{ID: "inspect", Type: schema.StepTypeCollector, CollectorSpec: &schema.CollectorSpec{
			Prompt: "Submit actual inspection evidence; opening a UI is not a diagnosis",
			Fields: []schema.CollectorField{
				{Name: "status", Type: schema.FieldTypeText, Label: "Status", Required: true},
				{Name: "confirmed", Type: schema.FieldTypeBoolean, Label: "Confirmed", Required: true},
				{Name: "count", Type: schema.FieldTypeNumber, Label: "Count", Required: true},
			},
		}}},
		{Iterate: &schema.IterateNode{ID: "pack", Over: "1",
			Steps: []schema.FlowNode{{Step: &schema.Step{ID: "record", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}}}},
			CollectValues: map[string]any{"answers": map[string]any{
				"status": "${status}", "confirmed": "${confirmed}", "count": "${count}",
			}},
		}},
		{Step: &schema.Step{ID: "return", Type: schema.StepTypeEnd, EndSpec: &schema.EndSpec{}}},
		{Step: &schema.Step{ID: "forbidden_tail", Type: schema.StepTypeAssert, AssertSpec: &schema.AssertSpec{
			Assert: []schema.Assertion{{Type: "eq", Subject: "tail executed", Expected: "never"}},
		}}},
	}
	freeze := func(id string, flow []schema.FlowNode, expression string) *schema.ToolAction {
		closure, err := plansnapshot.EncodeFlowClosure(flow)
		if err != nil {
			t.Fatal(err)
		}
		return &schema.ToolAction{
			Execute: &schema.ExecuteSpec{Kind: "runbook", Path: id + ".runbook.yaml"},
			Outputs: map[string]*schema.ArgDef{"answer": {Type: "object", Required: true}},
			FrozenSubstitution: &schema.FrozenToolSubstitution{
				PackageName: "composition", RunbookPath: id + ".runbook.yaml", RunbookID: id, RunbookName: id,
				RunbookContentHash: strings.Repeat("a", 64), ExecutableClosure: closure,
				Outputs: map[string]*schema.Output{"answer": {Type: "object", ValueExpr: expression}},
			},
		}
	}
	call := func(id, action string) schema.FlowNode {
		return schema.FlowNode{Step: &schema.Step{
			ID: id, Type: schema.StepTypeTool,
			ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{Name: "composition", Action: action}},
			Capture:  map[string]string{"answer": "outputs.answer"},
		}}
	}
	definition := &schema.ToolDef{Name: "composition", Actions: map[string]*schema.ToolAction{
		"inner": freeze("inner", []schema.FlowNode{{Step: &schema.Step{
			ID: "included", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{
				Include: schema.IncludeConfig{Runbook: "leaf.runbook.yaml"}, ResolvedSteps: leaf,
			},
		}}}, "answers[0]"),
		"outer": freeze("outer", []schema.FlowNode{call("inner_call", "inner")}, "answer"),
	}}
	plan := &engine.ExecutionPlan{
		RunID: runID, RunbookPath: "public.runbook.yaml",
		Tools: map[string]*schema.ToolDef{"composition": definition},
		Steps: []engine.ResolvedStep{
			{ID: "public_call", Kind: "tool", Spec: call("public_call", "outer").Step.ToolCall,
				Capture: map[string]string{"answer": "outputs.answer"}},
			{ID: "assert_submission", Kind: "assert", Spec: &schema.AssertSpec{Assert: []schema.Assertion{{
				Type: "eq", Subject: `${answer.status == "blocked" and answer.confirmed == false and answer.count == 0}`, Expected: "true",
			}}}},
		},
		Metadata: engine.PlanMetadata{RunbookID: "public", RunbookName: "Public composition"},
	}
	if err := internalplanner.ValidateExecutionPlan(plan); err != nil {
		t.Fatal(err)
	}
	wire := func(root string, broker *internalserve.PromptBroker) engine.EngineConfig {
		parser, err := internalparser.New(platform.NewFakePlatform())
		if err != nil {
			t.Fatal(err)
		}
		config, shutdown, err := adapter.BuildEngineConfig(ctx, adapter.WireOptions{
			Mode: "real", RunDir: filepath.Join(root, "runs"), TraceFile: filepath.Join(root, "trace.jsonl"),
			ToolScanDir: root, PromptProviderOverride: broker, SubstitutionParser: parser,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(shutdown)
		return config
	}
	firstRoot := t.TempDir()
	firstBroker := internalserve.NewPromptBroker(8)
	firstBroker.Register(runID)
	firstConfig := wire(firstRoot, firstBroker)
	first, err := internalengine.New(firstConfig).Start(ctx, plan, engine.RunOptions{Store: firstConfig.Store, Mode: engine.RunModeReal})
	if err != nil {
		t.Fatal(err)
	}
	firstFrames, err := firstBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	firstNext := make(chan error, 1)
	go func() { _, err := first.Next(ctx); firstNext <- err }()
	var pending internalserve.PendingInteraction
	select {
	case frame := <-firstFrames:
		pending = frame.(internalserve.PendingInteraction)
	case err := <-firstNext:
		t.Fatalf("public call ended before interaction: %v state=%#v", err, first.State())
	case <-ctx.Done():
		t.Fatal("public interaction timed out")
	}
	const node = "public_call/inner_call/included/inspect"
	if pending.NodeID != node || pending.Kind != "collector" {
		t.Fatalf("not a nested public collector: %#v", pending)
	}
	waiting := first.State()
	if waiting.Status != engine.RunStatusWaiting || len(waiting.StepResults) != 0 {
		t.Fatalf("public tool produced a result before submission: %#v", waiting)
	}
	select {
	case err := <-firstNext:
		t.Fatalf("public call returned before submission: %v", err)
	default:
	}
	secondRoot := t.TempDir()
	copyTree(t, filepath.Join(firstRoot, "runs"), filepath.Join(secondRoot, "runs"))
	firstBroker.Unregister(runID)
	select {
	case <-firstNext:
	case <-ctx.Done():
		t.Fatal("first worker did not stop")
	}
	secondBroker := internalserve.NewPromptBroker(8)
	secondBroker.Register(runID)
	t.Cleanup(func() { secondBroker.Unregister(runID) })
	secondConfig := wire(secondRoot, secondBroker)
	second, err := internalengine.New(secondConfig).Resume(ctx, runID, engine.RunOptions{Store: secondConfig.Store, Mode: engine.RunModeReal})
	if err != nil {
		t.Fatal(err)
	}
	secondFrames, err := secondBroker.Subscribe(runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			_, err := second.Next(ctx)
			if err != nil {
				done <- err
				return
			}
		}
	}()
	resumed := waitPendingInteraction(t, secondFrames)
	if resumed.TurnID != pending.TurnID || resumed.NodeID != node {
		t.Fatalf("resume changed pending occurrence: %#v / %#v", pending, resumed)
	}
	if state := second.State(); len(state.StepResults) != 0 || state.Status != engine.RunStatusWaiting {
		t.Fatal("resume manufactured an operator result")
	}
	if err := secondBroker.Answer(runID, resumed.TurnID, internalserve.AnswerEnvelope{
		Kind: "collector", Values: map[string]any{"f:0": "blocked", "f:1": false, "f:2": 0},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("resume did not complete")
	}
	state := second.State()
	if state.Status != engine.RunStatusCompleted || len(state.Interactions) != 0 {
		t.Fatalf("submission did not complete public assertions: %#v", state)
	}
	for _, frame := range state.ExecutionFrames {
		if frame.Status != engine.ExecutionFrameStatusCompleted {
			t.Fatalf("frame did not unwind: %#v", frame)
		}
		for _, result := range frame.Results {
			if result.StepID == "forbidden_tail" && result.Status != engine.StepStatusSkipped {
				t.Fatalf("terminal tail executed after resume: %#v", result)
			}
		}
	}
	events, err := internaltrace.NewJSONLReader(filepath.Join(secondRoot, "trace.jsonl")).ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range events {
		if string(event.Kind) != "step/completed" {
			continue
		}
		var payload struct {
			Node string `json:"qualified_node_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		counts[payload.Node]++
	}
	for _, expected := range []string{node, "public_call/inner_call", "public_call", "assert_submission"} {
		if counts[expected] != 1 {
			t.Fatalf("expected exactly one completed %s: %v", expected, counts)
		}
	}
	// Submission is typed, not a UI-open signal or an inferred diagnosis.
	want, _ := json.Marshal(map[string]any{"status": "blocked", "confirmed": false, "count": 0})
	got, _ := json.Marshal(state.StepResults["public_call"].Output["answer"])
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("actual operator result %s, want %s", got, want)
	}
}
