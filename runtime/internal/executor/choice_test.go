package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

type errPromptProvider struct{ err error }

func (e errPromptProvider) PromptChoice(ctx context.Context, req input.ChoiceRequest) (*input.ChoiceResponse, error) {
	return nil, e.err
}
func (e errPromptProvider) PromptDecision(ctx context.Context, req input.DecisionRequest) (*input.DecisionResponse, error) {
	return nil, e.err
}
func (e errPromptProvider) PromptForm(ctx context.Context, req input.FormRequest) (*input.FormResponse, error) {
	return nil, e.err
}

func TestChoiceExecutor_SingleSelect(t *testing.T) {
	provider := &testutil.FakePromptProvider{
		ChoiceResponses: map[string]*input.ChoiceResponse{
			"choice": {Selected: []string{"blue"}},
		},
	}
	exec := NewChoiceExecutor(provider, nil)
	step := engine.ResolvedStep{ID: "choice", Kind: "choice", Spec: &schema.ChoiceSpec{
		Prompt:   "Pick",
		Options:  []schema.ChoiceOption{{Label: "Blue", Value: "blue"}},
		Variable: "color",
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Vars["color"] != "blue" {
		t.Fatalf("expected color=blue, got %v", res.Vars["color"])
	}
}

func TestChoiceExecutor_MultiSelect(t *testing.T) {
	provider := &testutil.FakePromptProvider{
		ChoiceResponses: map[string]*input.ChoiceResponse{
			"choice": {Selected: []string{"a", "b"}},
		},
	}
	exec := NewChoiceExecutor(provider, nil)
	step := engine.ResolvedStep{ID: "choice", Kind: "choice", Spec: &schema.ChoiceSpec{
		Prompt:   "Pick",
		Options:  []schema.ChoiceOption{{Label: "A", Value: "a"}, {Label: "B", Value: "b"}},
		Variable: "items",
		Multiple: true,
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	values, ok := res.Vars["items"].([]string)
	if !ok || len(values) != 2 {
		t.Fatalf("expected 2 selections, got %v", res.Vars["items"])
	}
}

func TestChoiceExecutor_Cancel(t *testing.T) {
	exec := NewChoiceExecutor(errPromptProvider{err: errors.New("cancelled")}, nil)
	step := engine.ResolvedStep{ID: "choice", Kind: "choice", Spec: &schema.ChoiceSpec{
		Prompt:   "Pick",
		Options:  []schema.ChoiceOption{{Label: "A", Value: "a"}},
		Variable: "item",
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusFailed {
		t.Fatalf("expected failed status, got %s", res.Status)
	}
}

func TestChoiceExecutor_RejectsUnknownAndInvalidMultiplicity(t *testing.T) {
	tests := []struct {
		name     string
		multiple bool
		selected []string
	}{
		{name: "unknown value", selected: []string{"not-declared"}},
		{name: "too many for single choice", selected: []string{"a", "b"}},
		{name: "too few selections", multiple: true, selected: []string{"a"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &testutil.FakePromptProvider{ChoiceResponses: map[string]*input.ChoiceResponse{
				"choice": {Selected: test.selected},
			}}
			spec := &schema.ChoiceSpec{
				Prompt: "Pick", Variable: "item", Multiple: test.multiple,
				Options: []schema.ChoiceOption{{Label: "A", Value: "a"}, {Label: "B", Value: "b"}},
			}
			if test.multiple {
				spec.MinSelections = 2
			}
			result, err := NewChoiceExecutor(provider, nil).Execute(context.Background(), engine.ResolvedStep{ID: "choice", Kind: "choice", Spec: spec}, nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Status != engine.StepStatusFailed || len(result.Vars) != 0 {
				t.Fatalf("result = %#v, want failed with no vars", result)
			}
		})
	}
}

func TestDecisionExecutor_RejectsUnknownRoute(t *testing.T) {
	provider := &testutil.FakePromptProvider{DecisionResponses: map[string]*input.DecisionResponse{
		"decision": {Label: "Not declared"},
	}}
	result, err := NewDecisionExecutor(provider, nil).Execute(context.Background(), engine.ResolvedStep{
		ID: "decision", Kind: "decision", Spec: &schema.DecisionSpec{
			Prompt: "Pick", Variable: "route", Routes: []schema.DecisionRoute{{Label: "Declared"}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != engine.StepStatusFailed || len(result.Vars) != 0 {
		t.Fatalf("result = %#v, want failed with no vars", result)
	}
}

// Regression tests for the vars.all_passed bug
// These tests verify that choice executors correctly set variables
// that branch executors check in conditions

func TestChoiceExecutor_SetsVariableFromSingleSelection_DnsFail(t *testing.T) {
	provider := &testutil.FakePromptProvider{
		ChoiceResponses: map[string]*input.ChoiceResponse{
			"choice": {Selected: []string{"dns_fail"}},
		},
	}
	exec := NewChoiceExecutor(provider, nil)
	step := engine.ResolvedStep{ID: "choice", Kind: "choice", Spec: &schema.ChoiceSpec{
		Prompt: "What failed?",
		Options: []schema.ChoiceOption{
			{Label: "DNS failure", Value: "dns_fail"},
			{Label: "Routing failure", Value: "routing_fail"},
			{Label: "All passed", Value: "all_pass"},
		},
		Variable: "all_passed",
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Vars["all_passed"] != "dns_fail" {
		t.Fatalf("expected all_passed=dns_fail, got %v", res.Vars["all_passed"])
	}
}

func TestChoiceExecutor_SetsVariableFromSingleSelection_AllPass(t *testing.T) {
	provider := &testutil.FakePromptProvider{
		ChoiceResponses: map[string]*input.ChoiceResponse{
			"choice": {Selected: []string{"all_pass"}},
		},
	}
	exec := NewChoiceExecutor(provider, nil)
	step := engine.ResolvedStep{ID: "choice", Kind: "choice", Spec: &schema.ChoiceSpec{
		Prompt: "What failed?",
		Options: []schema.ChoiceOption{
			{Label: "DNS failure", Value: "dns_fail"},
			{Label: "All passed", Value: "all_pass"},
		},
		Variable: "all_passed",
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Vars["all_passed"] != "all_pass" {
		t.Fatalf("expected all_passed=all_pass, got %v", res.Vars["all_passed"])
	}
}

func TestChoiceExecutor_SetsVariableFromMultiSelection(t *testing.T) {
	provider := &testutil.FakePromptProvider{
		ChoiceResponses: map[string]*input.ChoiceResponse{
			"choice": {Selected: []string{"opt1", "opt2"}},
		},
	}
	exec := NewChoiceExecutor(provider, nil)
	step := engine.ResolvedStep{ID: "choice", Kind: "choice", Spec: &schema.ChoiceSpec{
		Prompt:   "Pick multiple",
		Options:  []schema.ChoiceOption{{Label: "Option 1", Value: "opt1"}, {Label: "Option 2", Value: "opt2"}},
		Variable: "selected_options",
		Multiple: true,
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	values, ok := res.Vars["selected_options"].([]string)
	if !ok {
		t.Fatalf("expected []string, got %T", res.Vars["selected_options"])
	}
	if len(values) != 2 || values[0] != "opt1" || values[1] != "opt2" {
		t.Fatalf("expected [opt1 opt2], got %v", values)
	}
}

func TestChoiceExecutor_EmptySelection(t *testing.T) {
	provider := &testutil.FakePromptProvider{
		ChoiceResponses: map[string]*input.ChoiceResponse{
			"choice": {Selected: []string{}},
		},
	}
	exec := NewChoiceExecutor(provider, nil)
	step := engine.ResolvedStep{ID: "choice", Kind: "choice", Spec: &schema.ChoiceSpec{
		Prompt:   "Pick",
		Options:  []schema.ChoiceOption{{Label: "A", Value: "a"}},
		Variable: "item",
	}}

	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Vars["item"] != "" {
		t.Fatalf("expected empty string, got %v", res.Vars["item"])
	}
}
