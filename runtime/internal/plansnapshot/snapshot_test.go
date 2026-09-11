package plansnapshot_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestExecutionPlanSnapshotV1RoundTrip(t *testing.T) {
	dynamicClosure, err := plansnapshot.EncodeFlowClosure([]schema.FlowNode{{Step: &schema.Step{
		ID: "dynamic-child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}})
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	governanceSource := &schema.GovernanceConfig{
		AllowCommands: []string{"az *"},
		DenyCommands:  []string{"az account clear"},
		DenyEnvVars:   []string{"*_TOKEN"},
	}
	original := &engine.ExecutionPlan{
		RunID:       "run-plan-snapshot",
		RunbookPath: "runbooks/root.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{
				ID:   "inspect",
				Name: "Inspect database",
				Kind: "tool",
				Spec: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
					Name: "xts", Action: "inspect", Args: map[string]any{"server": "${server}"},
				}},
				Capture: map[string]string{"state": "outputs.state"},
				CaptureDefaults: map[string]any{
					"large_sequence": int64(9007199254740993),
				},
				Depth:        1,
				NestDepth:    2,
				DisplayOrder: 3,
				Origin:       "runbooks/root.runbook.yaml",
				When:         `environment == "prod"`,
			},
			{
				ID:   "invoke_child",
				Name: "Run child",
				Kind: "include",
				Spec: &schema.IncludeSpec{
					Include: schema.IncludeConfig{
						Runbook: "child.runbook.yaml",
						With:    map[string]string{"server": "${server}"},
						Gate:    &schema.GateSpec{StopIf: []string{"resolved"}},
					},
					ResolvedSteps: []schema.FlowNode{{Step: &schema.Step{
						ID: "child_done", Type: schema.StepTypeEnd,
						EndSpec: &schema.EndSpec{Outcome: &schema.OutcomeDeclaration{Category: "resolved", Code: "child-done"}},
					}}},
					ResolvedInputs: map[string]*schema.Input{
						"server": {Type: "string", Required: true, Default: int64(9007199254740993)},
					},
					ResolvedOutputs: map[string]*schema.Output{
						"credential": {Type: "secret", Optional: true},
					},
					ResolvedGovernance: &schema.GovernanceConfig{RequireApproval: true},
				},
				ParentID:     "route",
				ParentKind:   "branch",
				IncludeAlias: "child",
				BranchLabel:  "Child route",
			},
		},
		Tools: map[string]*schema.ToolDef{
			"xts": {
				APIVersion: "yawr.tool/v1",
				Name:       "xts",
				Version:    "1.0.0",
				Actions: map[string]*schema.ToolAction{
					"inspect": {Outputs: map[string]*schema.ArgDef{"state": {Type: "string", Required: true}}},
				},
			},
		},
		Providers: map[string]*schema.ProviderDef{
			"context": {APIVersion: "yawr.provider/v1", Name: "context"},
		},
		Governance:       internalgovernance.BuildPolicy(governanceSource),
		GovernanceSource: governanceSource,
		Metadata: engine.PlanMetadata{
			PlannedAt:      time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC),
			RunbookID:      "root",
			RunbookName:    "Root runbook",
			CatalogDigest:  "sha256:catalog",
			PackageDigests: map[string]string{"sql-livesite.xts": "sha256:package"},
			DynamicIncludes: []schema.LockedDynamicInclude{{
				StepID: "dynamic", RenderedRef: "pkg/child", QualifiedID: "pkg/child",
				RunbookID: "child", RunbookName: "Child", RunbookContentHash: strings.Repeat("a", 64),
				AbsPath: "C:/repo/child.runbook.yaml", PackageName: "pkg", PackageVersion: "1.0.0",
				FileDigest:        engine.InteractionPayloadDigest([]byte("file")),
				PackageDigest:     engine.InteractionPayloadDigest([]byte("package")),
				ExecutableClosure: dynamicClosure,
			}},
			Profile: &schema.RuntimeProfile{
				APIVersion: schema.RuntimeProfileAPIVersion,
				ID:         "operator",
				Context:    schema.ProfileContextVSCodeOperator,
				Attendance: schema.ProfileAttendanceAttended,
			},
		},
		Inputs: map[string]*schema.Input{
			"server": {Type: "string", Required: true},
		},
		Outputs: map[string]*schema.Output{
			"result": {Type: "string", Value: "${state}"},
		},
	}
	if err := planner.ValidateExecutionPlan(original); err != nil {
		t.Fatalf("validate original plan: %v", err)
	}

	snapshot, err := plansnapshot.FromExecutionPlan(original)
	if err != nil {
		t.Fatalf("FromExecutionPlan: %v", err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var decoded plansnapshot.SnapshotV1
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	restored, err := plansnapshot.Restore(decoded)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if restored.Validation == nil {
		t.Fatal("restored plan is not validated")
	}
	if restored.Metadata.PlanHash != original.Metadata.PlanHash {
		t.Fatalf("plan hash changed: got %q, want %q", restored.Metadata.PlanHash, original.Metadata.PlanHash)
	}
	if !reflect.DeepEqual(restored.Metadata, original.Metadata) {
		t.Fatalf("metadata changed:\n got: %#v\nwant: %#v", restored.Metadata, original.Metadata)
	}
	if !reflect.DeepEqual(restored.Tools, original.Tools) || !reflect.DeepEqual(restored.Providers, original.Providers) {
		t.Fatal("tool or provider definitions changed")
	}
	if !reflect.DeepEqual(restored.Inputs, original.Inputs) || !reflect.DeepEqual(restored.Outputs, original.Outputs) {
		t.Fatal("input or output declarations changed")
	}
	toolSpec, ok := restored.Steps[0].Spec.(*schema.ToolCallSpec)
	if !ok || toolSpec.Tool.Action != "inspect" {
		t.Fatalf("tool spec was not restored: %#v", restored.Steps[0].Spec)
	}
	if got, ok := restored.Steps[0].CaptureDefaults["large_sequence"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatalf("large integer changed: %#v", restored.Steps[0].CaptureDefaults["large_sequence"])
	}
	includeSpec, ok := restored.Steps[1].Spec.(*schema.IncludeSpec)
	if !ok || len(includeSpec.ResolvedSteps) != 1 || includeSpec.ResolvedSteps[0].Step.ID != "child_done" {
		t.Fatalf("resolved include closure was not restored: %#v", restored.Steps[1].Spec)
	}
	if got, ok := includeSpec.ResolvedInputs["server"].Default.(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatalf("resolved input default changed: %#v", includeSpec.ResolvedInputs["server"].Default)
	}
	if output := includeSpec.ResolvedOutputs["credential"]; output == nil || output.Type != "secret" || !output.Optional {
		t.Fatalf("resolved output declaration changed: %#v", output)
	}
	if allowed, _ := restored.Governance.CheckCommand("az account clear"); allowed {
		t.Fatal("restored governance policy lost its deny rule")
	}
}

func TestRestoreRejectsUnknownSnapshotVersion(t *testing.T) {
	_, err := plansnapshot.Restore(plansnapshot.SnapshotV1{SchemaVersion: "execution-plan/v2"})
	if err == nil {
		t.Fatal("Restore accepted an unknown snapshot version")
	}
}

func TestExecutionPlanSnapshotV1PreservesStaticHandoff(t *testing.T) {
	plan := &engine.ExecutionPlan{
		RunID: "handoff-snapshot", RunbookPath: "root.runbook.yaml",
		Steps: []engine.ResolvedStep{{
			ID: "continue", Kind: "handoff", Spec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
				Runbook: "GEODR0004.runbook.yaml",
				Reason:  schema.HandoffReason{Code: "active-update-slo", Summary: "Continue investigation"},
				With:    map[string]string{"server": "${server}"},
				Facts:   map[string]string{"workflow": "${workflow}"},
			}},
		}},
		Metadata: engine.PlanMetadata{RunbookID: "root", RunbookName: "Root"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("ValidateExecutionPlan: %v", err)
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatalf("FromExecutionPlan: %v", err)
	}
	restored, err := plansnapshot.Restore(snapshot)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	handoff, ok := restored.Steps[0].Spec.(*schema.HandoffSpec)
	if !ok || handoff.Handoff.Runbook != "GEODR0004.runbook.yaml" ||
		handoff.Handoff.Reason.Code != "active-update-slo" || handoff.Handoff.With["server"] != "${server}" {
		t.Fatalf("restored handoff = %#v", restored.Steps[0].Spec)
	}
}

func TestExecutionPlanSnapshotV1PreservesCompositeSpecs(t *testing.T) {
	leaf := func(id string, stepType schema.StepType, spec func(*schema.Step)) schema.FlowNode {
		step := &schema.Step{ID: id, Type: stepType}
		spec(step)
		return schema.FlowNode{Step: step}
	}
	choiceNode := leaf("choose", schema.StepTypeChoice, func(step *schema.Step) {
		step.ChoiceSpec = &schema.ChoiceSpec{
			Prompt: "Choose", Variable: "answer",
			Options: []schema.ChoiceOption{{Value: "yes", Label: "Yes"}},
		}
	})
	displayNode := leaf("show", schema.StepTypeDisplay, func(step *schema.Step) {
		step.DisplaySpec = &schema.DisplaySpec{Display: schema.DisplayConfig{Content: "${item}", Format: "text"}}
	})
	leftNode := leaf("left", schema.StepTypeNoop, func(step *schema.Step) {
		step.NoopSpec = &schema.NoopSpec{}
	})
	rightNode := leaf("right", schema.StepTypeEnd, func(step *schema.Step) {
		step.EndSpec = &schema.EndSpec{
			Outcome: &schema.OutcomeDeclaration{Category: "no_action", Code: "right-done"},
		}
	})
	undoNode := leaf("undo", schema.StepTypeCLI, func(step *schema.Step) {
		step.CLI = &schema.CLISpec{Command: "undo", Args: []string{"--safe"}}
	})
	branchSpec := &schema.BranchSpec{Branches: []schema.BranchArm{{
		Condition: `route == "a"`, Label: "A", Steps: []schema.FlowNode{choiceNode},
	}}}
	iterateSpec := &schema.IterateNode{
		ID: "iterate", Over: "${items}", As: "item", Max: 3, Steps: []schema.FlowNode{displayNode},
	}
	parallelSpec := &schema.ParallelNode{
		ID: "parallel", Join: &schema.ParallelJoin{WaitFor: "all"},
		Branches: []schema.ParallelBranch{
			{Label: "left", Steps: []schema.FlowNode{leftNode}},
			{Label: "right", Steps: []schema.FlowNode{rightNode}},
		},
	}
	compensateSpec := &schema.CompensateSpec{Compensate: schema.CompensateConfig{
		On: "failure", Steps: []schema.FlowNode{undoNode},
	}}
	plan := &engine.ExecutionPlan{
		RunID: "run-composites", RunbookPath: "composites.runbook.yaml",
		Steps: []engine.ResolvedStep{
			{ID: "branch", Name: "Branch", Kind: "branch", Spec: branchSpec},
			{ID: "iterate", Name: "Iterate", Kind: "iterate", Spec: iterateSpec},
			{ID: "parallel", Name: "Parallel", Kind: "parallel", Spec: parallelSpec},
			{ID: "compensate", Name: "Compensate", Kind: "compensate", Spec: compensateSpec},
		},
		Metadata: engine.PlanMetadata{RunbookID: "composites", RunbookName: "Composites"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("validate plan: %v", err)
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatalf("FromExecutionPlan: %v", err)
	}
	restored, err := plansnapshot.Restore(snapshot)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	branch := restored.Steps[0].Spec.(*schema.BranchSpec)
	if got := branch.Branches[0].Steps[0].Step.ChoiceSpec.Options[0].Value; got != "yes" {
		t.Fatalf("choice value = %q, want yes", got)
	}
	iterate := restored.Steps[1].Spec.(*schema.IterateNode)
	if got := iterate.Steps[0].Step.DisplaySpec.Display.Content; got != "${item}" {
		t.Fatalf("display content = %q, want ${item}", got)
	}
	parallel := restored.Steps[2].Spec.(*schema.ParallelNode)
	if parallel.Join == nil || parallel.Join.WaitFor != "all" || parallel.Branches[1].Steps[0].Step.EndSpec.Outcome.Code != "right-done" {
		t.Fatalf("parallel definition changed: %#v", parallel)
	}
	compensate := restored.Steps[3].Spec.(*schema.CompensateSpec)
	if got := compensate.Compensate.Steps[0].Step.CLI.Args[0]; got != "--safe" {
		t.Fatalf("compensation argument = %q, want --safe", got)
	}
}

func TestFlowClosureV1RoundTripAndTamperDetection(t *testing.T) {
	nodes := []schema.FlowNode{{Step: &schema.Step{
		ID: "child", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{Command: "echo captured"},
	}}}
	encoded, err := plansnapshot.EncodeFlowClosure(nodes)
	if err != nil {
		t.Fatalf("EncodeFlowClosure: %v", err)
	}
	restored, err := plansnapshot.RestoreFlowClosure(encoded)
	if err != nil {
		t.Fatalf("RestoreFlowClosure: %v", err)
	}
	if len(restored) != 1 || restored[0].Step == nil || restored[0].Step.CLI.Command != "echo captured" {
		t.Fatalf("restored closure = %#v", restored)
	}
	tampered := append(json.RawMessage(nil), encoded...)
	tampered = bytes.Replace(tampered, []byte("echo captured"), []byte("echo changed!"), 1)
	if _, err := plansnapshot.RestoreFlowClosure(tampered); err == nil {
		t.Fatal("RestoreFlowClosure accepted tampered executable content")
	}
}

func TestRestoreRejectsTamperedPlan(t *testing.T) {
	plan := &engine.ExecutionPlan{
		RunID: "run-tampered", RunbookPath: "tampered.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "ready", Name: "Ready", Kind: "noop", Spec: &schema.NoopSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "tampered", RunbookName: "Tampered"},
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		t.Fatalf("validate plan: %v", err)
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		t.Fatalf("FromExecutionPlan: %v", err)
	}
	snapshot.Steps[0].Name = "Changed after validation"
	if _, err := plansnapshot.Restore(snapshot); err == nil {
		t.Fatal("Restore accepted a plan whose content no longer matches its hash")
	}
}

type unsupportedSpec struct{}

func (*unsupportedSpec) StepKind() string { return "extension" }

func TestFromExecutionPlanRejectsUnsupportedConcreteSpec(t *testing.T) {
	plan := engine.ValidatedForTest(&engine.ExecutionPlan{
		RunID: "run-extension", RunbookPath: "extension.runbook.yaml",
		Steps:    []engine.ResolvedStep{{ID: "extension", Name: "Extension", Kind: "extension", Spec: &unsupportedSpec{}}},
		Metadata: engine.PlanMetadata{RunbookID: "extension", PlanHash: "sha256:validated"},
	})
	if _, err := plansnapshot.FromExecutionPlan(plan); err == nil {
		t.Fatal("FromExecutionPlan accepted an unsupported concrete step spec")
	}
}

func TestValidateResumeSafetyRejectsNestedUnpinnedInclude(t *testing.T) {
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{{
		ID: "route", Kind: "branch", Spec: &schema.BranchSpec{Branches: []schema.BranchArm{{
			Label: "nested", Steps: []schema.FlowNode{{Step: &schema.Step{
				ID: "dynamic", Type: schema.StepTypeInclude,
				IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{
					RunbookRef: "${next_runbook}", ResolveFrom: schema.ResolveFromCatalog,
				}},
			}}},
		}}},
	}}}
	if err := plansnapshot.ValidateResumeSafety(plan); !errors.Is(err, plansnapshot.ErrUnpinnedInclude) {
		t.Fatalf("ValidateResumeSafety error = %v, want ErrUnpinnedInclude", err)
	}
	if err := plansnapshot.ValidateResumeSafetyForState(plan, engine.RunState{}); !errors.Is(err, plansnapshot.ErrUnpinnedInclude) {
		t.Fatalf("legacy state-aware validation error = %v, want ErrUnpinnedInclude", err)
	}
	if err := plansnapshot.ValidateResumeSafetyForState(plan, engine.RunState{WriterEpoch: 1}); err != nil {
		t.Fatalf("writer-epoch state validation: %v", err)
	}
}

func TestValidateResumeSafetyRejectsLazyIncludeWithoutContentDigestForWriterEpoch(t *testing.T) {
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{{
		ID: "lazy", Kind: "include", Spec: &schema.IncludeSpec{
			Include:         schema.IncludeConfig{Runbook: "child.runbook.yaml", Expand: "lazy"},
			LazyRunbookPath: "C:/repo/child.runbook.yaml",
		},
	}}}
	if err := plansnapshot.ValidateResumeSafetyForState(plan, engine.RunState{WriterEpoch: 1}); !errors.Is(err, plansnapshot.ErrUnpinnedInclude) {
		t.Fatalf("writer-epoch lazy validation error = %v, want ErrUnpinnedInclude", err)
	}
}

func TestValidateResumeSafetyRejectsDigestOnlyLazyIncludeForWriterEpoch(t *testing.T) {
	plan := &engine.ExecutionPlan{Steps: []engine.ResolvedStep{{
		ID: "lazy", Kind: "include", Spec: &schema.IncludeSpec{
			Include:         schema.IncludeConfig{Runbook: "child.runbook.yaml", Expand: "lazy"},
			LazyRunbookPath: "C:/repo/child.runbook.yaml", LazyRunbookDigest: "sha256:" + strings.Repeat("a", 64),
		},
	}}}
	if err := plansnapshot.ValidateResumeSafetyForState(plan, engine.RunState{WriterEpoch: 1}); !errors.Is(err, plansnapshot.ErrUnpinnedInclude) {
		t.Fatalf("writer-epoch digest-only validation error = %v, want ErrUnpinnedInclude", err)
	}
}
