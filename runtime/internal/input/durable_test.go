package input

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

type durablePromptStub struct {
	called   int
	prepared *bool
}

type nilPromptStub struct{}

func (*nilPromptStub) PromptChoice(context.Context, inputpkg.ChoiceRequest) (*inputpkg.ChoiceResponse, error) {
	return nil, nil
}
func (*nilPromptStub) PromptDecision(context.Context, inputpkg.DecisionRequest) (*inputpkg.DecisionResponse, error) {
	return nil, nil
}
func (*nilPromptStub) PromptForm(context.Context, inputpkg.FormRequest) (*inputpkg.FormResponse, error) {
	return nil, nil
}

func (stub *durablePromptStub) PromptChoice(context.Context, inputpkg.ChoiceRequest) (*inputpkg.ChoiceResponse, error) {
	if stub.prepared == nil || !*stub.prepared {
		return nil, errors.New("prompt ran before pending commit")
	}
	stub.called++
	return &inputpkg.ChoiceResponse{Selected: []string{"one"}}, nil
}
func (*durablePromptStub) PromptDecision(context.Context, inputpkg.DecisionRequest) (*inputpkg.DecisionResponse, error) {
	return nil, errors.New("unexpected decision")
}
func (*durablePromptStub) PromptForm(context.Context, inputpkg.FormRequest) (*inputpkg.FormResponse, error) {
	return nil, errors.New("unexpected form")
}

type durablePromptCommitter struct {
	prepared bool
	accepted bool
	restored *engine.InteractionState
}

func (committer *durablePromptCommitter) PrepareInteraction(_ context.Context, proposed engine.InteractionState) (engine.InteractionState, error) {
	committer.prepared = true
	if committer.restored != nil {
		return *committer.restored, nil
	}
	return proposed, nil
}

func (committer *durablePromptCommitter) AcceptInteraction(
	_ context.Context,
	_ string,
	_ string,
	answer json.RawMessage,
) (engine.InteractionState, error) {
	committer.accepted = true
	return engine.InteractionState{Status: engine.InteractionStatusAnswered, Answer: answer}, nil
}

func TestDurablePromptProviderCommitsBeforePromptAndReturn(t *testing.T) {
	committer := &durablePromptCommitter{}
	provider := &durablePromptStub{prepared: &committer.prepared}
	durable := NewDurablePromptProvider(provider)
	tracker := engine.NewInteractionInvocationTracker()
	ctx := engine.WithInteractionInvocationTracker(
		engine.WithInteractionCommitter(engine.WithRunID(context.Background(), "run-terminal-choice"), committer),
		tracker,
	)
	response, err := durable.PromptChoice(ctx, inputpkg.ChoiceRequest{
		StepID: "choose", Prompt: "Choose", Options: []inputpkg.Option{{Label: "One", Value: "one"}},
	})
	if err != nil {
		t.Fatalf("PromptChoice: %v", err)
	}
	if provider.called != 1 || !committer.accepted || len(response.Selected) != 1 || response.Selected[0] != "one" {
		t.Fatalf("called=%d accepted=%v response=%#v", provider.called, committer.accepted, response)
	}
}

func TestDurablePromptProviderReusesSavedAnswerWithoutPrompt(t *testing.T) {
	answer, _ := json.Marshal(struct {
		Selected []string `json:"selected"`
	}{Selected: []string{"saved"}})
	committer := &durablePromptCommitter{restored: &engine.InteractionState{
		Status: engine.InteractionStatusAnswered, Answer: answer,
	}}
	provider := &durablePromptStub{prepared: &committer.prepared}
	durable := NewDurablePromptProvider(provider)
	ctx := engine.WithInteractionInvocationTracker(
		engine.WithInteractionCommitter(context.Background(), committer),
		engine.NewInteractionInvocationTracker(),
	)
	response, err := durable.PromptChoice(ctx, inputpkg.ChoiceRequest{
		StepID: "choose", Prompt: "Choose", Options: []inputpkg.Option{{Label: "Saved", Value: "saved"}},
	})
	if err != nil {
		t.Fatalf("PromptChoice: %v", err)
	}
	if provider.called != 0 || len(response.Selected) != 1 || response.Selected[0] != "saved" {
		t.Fatalf("called=%d response=%#v", provider.called, response)
	}
}

func TestDurablePromptProviderRejectsNilDelegateResponses(t *testing.T) {
	tests := []struct {
		name string
		call func(inputpkg.PromptProvider, context.Context) error
	}{
		{name: "choice", call: func(provider inputpkg.PromptProvider, ctx context.Context) error {
			_, err := provider.PromptChoice(ctx, inputpkg.ChoiceRequest{StepID: "choose"})
			return err
		}},
		{name: "decision", call: func(provider inputpkg.PromptProvider, ctx context.Context) error {
			_, err := provider.PromptDecision(ctx, inputpkg.DecisionRequest{StepID: "decide"})
			return err
		}},
		{name: "form", call: func(provider inputpkg.PromptProvider, ctx context.Context) error {
			_, err := provider.PromptForm(ctx, inputpkg.FormRequest{StepID: "collect"})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			committer := &durablePromptCommitter{}
			ctx := engine.WithInteractionInvocationTracker(
				engine.WithInteractionCommitter(context.Background(), committer),
				engine.NewInteractionInvocationTracker(),
			)
			if err := test.call(NewDurablePromptProvider(&nilPromptStub{}), ctx); err == nil {
				t.Fatal("nil delegate response was accepted")
			}
			if committer.accepted {
				t.Fatal("nil delegate response committed an answer")
			}
		})
	}
}

func TestDurablePromptProviderRejectsMalformedRestoredAnswers(t *testing.T) {
	tests := []struct {
		name   string
		answer json.RawMessage
		invoke func(inputpkg.PromptProvider, context.Context) error
	}{
		{
			name:   "choice",
			answer: json.RawMessage(`{"selected":["not-declared"]}`),
			invoke: func(provider inputpkg.PromptProvider, ctx context.Context) error {
				_, err := provider.PromptChoice(ctx, inputpkg.ChoiceRequest{
					StepID: "choose", Options: []inputpkg.Option{{Label: "One", Value: "one"}},
				})
				return err
			},
		},
		{
			name:   "decision",
			answer: json.RawMessage(`{"label":"not-declared"}`),
			invoke: func(provider inputpkg.PromptProvider, ctx context.Context) error {
				_, err := provider.PromptDecision(ctx, inputpkg.DecisionRequest{
					StepID: "decide", Routes: []inputpkg.Route{{Label: "known"}},
				})
				return err
			},
		},
		{
			name:   "form",
			answer: json.RawMessage(`{"values":{}}`),
			invoke: func(provider inputpkg.PromptProvider, ctx context.Context) error {
				_, err := provider.PromptForm(ctx, inputpkg.FormRequest{
					StepID: "collect", Fields: []inputpkg.FormField{{Name: "required", Type: "text", Required: true}},
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			committer := &durablePromptCommitter{restored: &engine.InteractionState{
				Status: engine.InteractionStatusAnswered, Answer: test.answer,
			}}
			ctx := engine.WithInteractionInvocationTracker(
				engine.WithInteractionCommitter(context.Background(), committer),
				engine.NewInteractionInvocationTracker(),
			)
			if err := test.invoke(NewDurablePromptProvider(&nilPromptStub{}), ctx); err == nil {
				t.Fatal("malformed restored answer was accepted")
			}
		})
	}
}
