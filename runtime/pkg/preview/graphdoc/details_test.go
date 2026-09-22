package graphdoc

import (
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestDetailsForStepEmitsKindSpecificOperatorData(t *testing.T) {
	tests := []struct {
		name string
		step *schema.Step
		want map[string]any
	}{
		{
			name: "cli",
			step: &schema.Step{ID: "probe", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{
				Command: "az", Args: []string{"resource", "show"}, Shell: "pwsh", Workdir: "./ops", Env: map[string]string{"TOKEN": "${access_token}"}, Stdin: "payload",
			}},
			want: map[string]any{"kind": "cli", "command": "az", "args": []any{"resource", "show"}, "shell": "pwsh", "workdir": "./ops", "env_names": []any{"TOKEN"}, "stdin": true},
		},
		{
			name: "tool",
			step: &schema.Step{ID: "inspect", Type: schema.StepTypeTool, ToolCall: &schema.ToolCallSpec{Tool: schema.ToolInvocation{
				Name: "icm", Action: "get-incident", Version: "1.2.0", Args: map[string]any{"incident_id": "${icm_id}", "access_token": "literal-secret"},
			}}},
			want: map[string]any{"kind": "tool", "tool": "icm", "action": "get-incident", "version": "1.2.0", "arguments": []any{
				map[string]any{"name": "access_token", "value": "<redacted>", "redacted": true},
				map[string]any{"name": "incident_id", "value": "${icm_id}"},
			}},
		},
		{
			name: "include",
			step: &schema.Step{ID: "child", Type: schema.StepTypeInclude, IncludeSpec: &schema.IncludeSpec{Include: schema.IncludeConfig{
				Runbook: "child.runbook.yaml", With: map[string]string{"region": "${region}"}, Expand: "eager", Gate: &schema.GateSpec{StopIf: []string{"resolved"}},
			}}},
			want: map[string]any{"kind": "include", "reference": "child.runbook.yaml", "bindings": []any{map[string]any{"name": "region", "value": "${region}"}}, "expand": "eager", "stop_if": []any{"resolved"}},
		},
		{
			name: "choice",
			step: &schema.Step{ID: "choose", Type: schema.StepTypeChoice, ChoiceSpec: &schema.ChoiceSpec{
				Prompt: "Continue?", Variable: "answer", Default: "yes", Multiple: true, MinSelections: 1, MaxSelections: 2,
				Options: []schema.ChoiceOption{{Label: "Yes", Value: "yes", Hint: "Proceed"}},
			}},
			want: map[string]any{"kind": "choice", "prompt": "Continue?", "variable": "answer", "default": "yes", "multiple": true, "min": float64(1), "max": float64(2), "options": []any{map[string]any{"label": "Yes", "value": "yes", "hint": "Proceed"}}},
		},
		{
			name: "decision",
			step: &schema.Step{ID: "route", Type: schema.StepTypeDecision, DecisionSpec: &schema.DecisionSpec{
				Prompt: "Route?", Variable: "route", Routes: []schema.DecisionRoute{{Label: "Primary", Goto: "primary", Hint: "Preferred"}},
			}},
			want: map[string]any{"kind": "decision", "prompt": "Route?", "variable": "route", "routes": []any{map[string]any{"label": "Primary", "goto": "primary", "hint": "Preferred"}}},
		},
		{
			name: "collector",
			step: &schema.Step{ID: "collect", Type: schema.StepTypeCollector, CollectorSpec: &schema.CollectorSpec{
				Prompt: "Record findings", Fields: []schema.CollectorField{{Name: "notes", Type: schema.FieldTypeText, Label: "Notes", Required: true, Multiline: true, Validation: &schema.FieldValidation{MinLength: intPtr(3)}}},
			}},
			want: map[string]any{"kind": "collector", "prompt": "Record findings", "fields": []any{map[string]any{"name": "notes", "type": "text", "label": "Notes", "required": true, "multiline": true, "validation": map[string]any{"min_length": float64(3)}}}},
		},
		{
			name: "host action",
			step: &schema.Step{ID: "open", Type: schema.StepTypeHostAction, HostActionSpec: &schema.HostActionSpec{HostAction: schema.HostActionConfig{
				Capability: "external-view.open", Request: map[string]any{"view_path": "health.view", "token": "must-hide"},
			}}},
			want: map[string]any{"kind": "host_action", "capability": "external-view.open", "request": []any{
				map[string]any{"name": "token", "value": "<redacted>", "redacted": true},
				map[string]any{"name": "view_path", "value": "health.view"},
			}},
		},
		{
			name: "branch",
			step: &schema.Step{ID: "route", Type: schema.StepTypeBranch, BranchSpec: &schema.BranchSpec{Branches: []schema.BranchArm{{Condition: "healthy", Label: "Healthy", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "done", Type: schema.StepTypeNoop}}}}, {Else: true, Label: "Fallback"}}}},
			want: map[string]any{"kind": "branch", "arms": []any{map[string]any{"condition": "healthy", "label": "Healthy", "steps": float64(1)}, map[string]any{"else": true, "label": "Fallback", "steps": float64(0)}}},
		},
		{
			name: "approve",
			step: &schema.Step{ID: "approve", Type: schema.StepTypeApprove, ApproveSpec: &schema.ApproveSpec{Approvals: schema.ApprovalGate{Roles: []string{"owner"}, Pool: []string{"alice"}, Required: 1, Timeout: "1h"}, OnTimeout: "fail", Timezone: "UTC"}},
			want: map[string]any{"kind": "approve", "roles": []any{"owner"}, "pool": []any{"alice"}, "required": float64(1), "timeout": "1h", "on_timeout": "fail", "timezone": "UTC"},
		},
		{
			name: "assert",
			step: &schema.Step{ID: "assert", Type: schema.StepTypeAssert, AssertSpec: &schema.AssertSpec{Assert: []schema.Assertion{{Type: "equals", Subject: "${status}", Expected: "Active"}}}},
			want: map[string]any{"kind": "assert", "assertions": []any{map[string]any{"type": "equals", "subject": "${status}", "expected": "Active"}}},
		},
		{
			name: "wait",
			step: &schema.Step{ID: "wait", Type: schema.StepTypeWaitForEvent, WaitForEventSpec: &schema.WaitForEventSpec{Event: schema.WaitEventConfig{Source: schema.EventSourceWebhook, ID: "incident", Filter: map[string]string{"state": "active"}, PayloadSchema: "yawr.event/v1"}, OnTimeout: "continue"}},
			want: map[string]any{"kind": "wait_for_event", "source": "webhook", "event_id": "incident", "filter": []any{map[string]any{"name": "state", "value": "active"}}, "payload_schema": "yawr.event/v1", "on_timeout": "continue"},
		},
		{
			name: "display",
			step: &schema.Step{ID: "show", Type: schema.StepTypeDisplay, DisplaySpec: &schema.DisplaySpec{Display: schema.DisplayConfig{Content: "Status: ${status}", Format: "markdown"}}},
			want: map[string]any{"kind": "display", "content": "Status: ${status}", "format": "markdown"},
		},
		{
			name: "end",
			step: &schema.Step{ID: "done", Type: schema.StepTypeEnd, EndSpec: &schema.EndSpec{Outcome: &schema.OutcomeDeclaration{Category: "resolved", Code: "healthy"}}},
			want: map[string]any{"kind": "end", "category": "resolved", "code": "healthy"},
		},
		{
			name: "compensate",
			step: &schema.Step{ID: "rollback", Type: schema.StepTypeCompensate, CompensateSpec: &schema.CompensateSpec{Compensate: schema.CompensateConfig{On: "failure", Steps: []schema.FlowNode{{Step: &schema.Step{ID: "undo", Type: schema.StepTypeNoop}}}}}},
			want: map[string]any{"kind": "compensate", "on": "failure", "steps": float64(1)},
		},
		{
			name: "noop",
			step: &schema.Step{ID: "pause", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{}, Delay: "5s"},
			want: map[string]any{"kind": "noop"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			details := detailsForStep(test.step)
			encoded, err := json.Marshal(details)
			if err != nil {
				t.Fatalf("marshal details: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("decode details: %v", err)
			}
			for key, want := range test.want {
				if !jsonEqual(got[key], want) {
					t.Fatalf("%s = %#v, want %#v; all=%s", key, got[key], want, encoded)
				}
			}
			if containsString(encoded, "literal-secret") || containsString(encoded, "must-hide") {
				t.Fatalf("sensitive authored value leaked: %s", encoded)
			}
		})
	}
}

func TestDetailsForStructuralNodes(t *testing.T) {
	iterate := detailsForIterate(&schema.IterateNode{Over: "items", As: "item", Max: 10, Until: "done", Collect: map[string]string{"results": "item"}, Concurrency: 2, Steps: []schema.FlowNode{{Step: &schema.Step{ID: "body", Type: schema.StepTypeNoop}}}})
	parallel := detailsForParallel(&schema.ParallelNode{Branches: []schema.ParallelBranch{{Label: "A"}, {Label: "B"}}, Join: &schema.ParallelJoin{WaitFor: "all", OnFailure: "cancel"}})
	assertJSONField(t, iterate, "kind", "iterate")
	assertJSONField(t, iterate, "concurrency", float64(2))
	assertJSONField(t, parallel, "kind", "parallel")
	assertJSONField(t, parallel, "branches", float64(2))
}

func TestDetailsForStepRedactsCredentialLikeCLIValues(t *testing.T) {
	details := detailsForStep(&schema.Step{ID: "login", Type: schema.StepTypeCLI, CLI: &schema.CLISpec{
		Command: "client",
		Args:    []string{"login", "--password", "literal-password", "--token=literal-token", "--region", "westus"},
		Run:     "client login --api-key literal-api-key && echo safe",
	}})
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"literal-password", "literal-token", "literal-api-key"} {
		if containsString(encoded, secret) {
			t.Fatalf("credential-like CLI value leaked: %s", encoded)
		}
	}
	for _, visible := range []string{"client", "login", "--password", "--token", "--region", "westus", "echo safe"} {
		if !containsString(encoded, visible) {
			t.Fatalf("safe CLI context %q missing: %s", visible, encoded)
		}
	}
}

func TestDetailsForStepRedactsCredentialAssignmentsInAuthoredText(t *testing.T) {
	steps := []*schema.Step{
		{
			ID: "show", Type: schema.StepTypeDisplay, Subtitle: "password: subtitle-secret",
			DisplaySpec: &schema.DisplaySpec{Display: schema.DisplayConfig{Content: "Authorization: Bearer bearer-secret\nRegion: westus", Format: "markdown"}},
		},
		{
			ID: "route", Type: schema.StepTypeBranch,
			BranchSpec: &schema.BranchSpec{Branches: []schema.BranchArm{{Condition: "token=condition-secret", Label: "Safe route"}}},
		},
	}
	encoded, err := json.Marshal([]*StepDetails{detailsForStep(steps[0]), detailsForStep(steps[1])})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"subtitle-secret", "bearer-secret", "condition-secret"} {
		if containsString(encoded, secret) {
			t.Fatalf("authored credential value leaked: %s", encoded)
		}
	}
	for _, visible := range []string{"password", "Authorization", "Region", "westus", "Safe route"} {
		if !containsString(encoded, visible) {
			t.Fatalf("safe authored context %q missing: %s", visible, encoded)
		}
	}
}

func TestSafeAuthoredValueRedactsStructuredTextWithoutLookalikeFalsePositives(t *testing.T) {
	value := safeAuthoredValue("config", map[string]string{
		"headers":      `{"token": "json-secret", "x-api-key": "header-secret"}`,
		"passwordless": "true",
		"tokenizer":    "bert",
		"clientSecret": "map-secret",
	})
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"json-secret", "header-secret", "map-secret"} {
		if containsString(encoded, secret) {
			t.Fatalf("structured authored credential leaked: %s", encoded)
		}
	}
	for _, visible := range []string{"token", "x-api-key", "passwordless", "true", "tokenizer", "bert", "clientSecret"} {
		if !containsString(encoded, visible) {
			t.Fatalf("safe structured context %q missing: %s", visible, encoded)
		}
	}
}

func TestSensitiveDetailNameUsesTokenBoundaries(t *testing.T) {
	for _, name := range []string{"token", "access_token", "clientSecret", "x-api-key", "Authorization", "private.key"} {
		if !sensitiveDetailName(name) {
			t.Errorf("sensitiveDetailName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"passwordless", "tokenizer", "secretary", "authentication", "monkey"} {
		if sensitiveDetailName(name) {
			t.Errorf("sensitiveDetailName(%q) = true, want false", name)
		}
	}
}

func intPtr(value int) *int { return &value }

func assertJSONField(t *testing.T, value any, key string, want any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(object[key], want) {
		t.Fatalf("%s = %#v, want %#v", key, object[key], want)
	}
}

func jsonEqual(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func containsString(data []byte, value string) bool {
	for index := 0; index+len(value) <= len(data); index++ {
		if string(data[index:index+len(value)]) == value {
			return true
		}
	}
	return false
}
