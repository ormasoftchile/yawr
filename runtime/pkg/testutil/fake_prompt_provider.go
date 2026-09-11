package testutil

import (
	"context"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// FakePromptProvider is a controllable PromptProvider for use in tests.
type FakePromptProvider struct {
	ChoiceResponses   map[string]*input.ChoiceResponse
	DecisionResponses map[string]*input.DecisionResponse
	FormResponses     map[string]*input.FormResponse

	DefaultChoice   *input.ChoiceResponse
	DefaultDecision *input.DecisionResponse
	DefaultForm     *input.FormResponse

	Calls []PromptCall
	mu    sync.Mutex
}

// PromptCall records a single prompt invocation.
type PromptCall struct {
	StepID string
	Kind   string
	At     time.Time
}

// Ensure FakePromptProvider implements input.PromptProvider.
var _ input.PromptProvider = (*FakePromptProvider)(nil)

func (f *FakePromptProvider) PromptChoice(ctx context.Context, req input.ChoiceRequest) (*input.ChoiceResponse, error) {
	f.record(req.StepID, "choice")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.ChoiceResponses != nil {
		if resp, ok := f.ChoiceResponses[req.StepID]; ok {
			return resp, nil
		}
	}
	if f.DefaultChoice != nil {
		return f.DefaultChoice, nil
	}
	if len(req.Options) > 0 {
		return &input.ChoiceResponse{Selected: []string{req.Options[0].Value}}, nil
	}
	return &input.ChoiceResponse{}, nil
}

func (f *FakePromptProvider) PromptDecision(ctx context.Context, req input.DecisionRequest) (*input.DecisionResponse, error) {
	f.record(req.StepID, "decision")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.DecisionResponses != nil {
		if resp, ok := f.DecisionResponses[req.StepID]; ok {
			return resp, nil
		}
	}
	if f.DefaultDecision != nil {
		return f.DefaultDecision, nil
	}
	if len(req.Routes) > 0 {
		return &input.DecisionResponse{Label: req.Routes[0].Label}, nil
	}
	return &input.DecisionResponse{}, nil
}

func (f *FakePromptProvider) PromptForm(ctx context.Context, req input.FormRequest) (*input.FormResponse, error) {
	f.record(req.StepID, "form")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.FormResponses != nil {
		if resp, ok := f.FormResponses[req.StepID]; ok {
			return resp, nil
		}
	}
	if f.DefaultForm != nil {
		return f.DefaultForm, nil
	}
	return &input.FormResponse{Values: map[string]any{}}, nil
}

func (f *FakePromptProvider) record(stepID string, kind string) {
	f.mu.Lock()
	f.Calls = append(f.Calls, PromptCall{StepID: stepID, Kind: kind, At: time.Now()})
	f.mu.Unlock()
}
