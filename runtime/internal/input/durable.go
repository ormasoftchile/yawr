package input

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

type durablePromptProvider struct {
	delegate inputpkg.PromptProvider
}

type durableInteractionProvider interface {
	CommitsInteractionsDurably() bool
}

func NewDurablePromptProvider(provider inputpkg.PromptProvider) inputpkg.PromptProvider {
	if provider == nil {
		return nil
	}
	if durable, ok := provider.(durableInteractionProvider); ok && durable.CommitsInteractionsDurably() {
		return provider
	}
	return &durablePromptProvider{delegate: provider}
}

func (provider *durablePromptProvider) PromptChoice(
	ctx context.Context,
	request inputpkg.ChoiceRequest,
) (*inputpkg.ChoiceResponse, error) {
	committed, committer, durable, err := prepareTerminalInteraction(ctx, request.StepID, "choice", request)
	if err != nil {
		return nil, err
	}
	if durable && committed.Status == engine.InteractionStatusAnswered {
		var answer struct {
			Selected []string `json:"selected"`
		}
		if err := json.Unmarshal(committed.Answer, &answer); err != nil {
			return nil, fmt.Errorf("durable prompt: decode choice answer: %w", err)
		}
		response := &inputpkg.ChoiceResponse{Selected: answer.Selected}
		if err := validateChoiceResponse(request, response); err != nil {
			return nil, err
		}
		return response, nil
	}
	response, err := provider.delegate.PromptChoice(ctx, request)
	if err != nil || !durable {
		return response, err
	}
	if response == nil {
		return nil, errors.New("durable prompt: choice provider returned no response")
	}
	if err := validateChoiceResponse(request, response); err != nil {
		return nil, err
	}
	answer, err := json.Marshal(struct {
		Selected []string `json:"selected"`
	}{Selected: response.Selected})
	if err != nil {
		return nil, err
	}
	if _, err := committer.AcceptInteraction(ctx, committed.TurnID, engine.InteractionPayloadDigest(answer), answer); err != nil {
		return nil, err
	}
	return response, nil
}

func (provider *durablePromptProvider) PromptDecision(
	ctx context.Context,
	request inputpkg.DecisionRequest,
) (*inputpkg.DecisionResponse, error) {
	committed, committer, durable, err := prepareTerminalInteraction(ctx, request.StepID, "decision", request)
	if err != nil {
		return nil, err
	}
	if durable && committed.Status == engine.InteractionStatusAnswered {
		var answer struct {
			Label string `json:"label"`
		}
		if err := json.Unmarshal(committed.Answer, &answer); err != nil {
			return nil, fmt.Errorf("durable prompt: decode decision answer: %w", err)
		}
		response := &inputpkg.DecisionResponse{Label: answer.Label}
		if err := validateDecisionResponse(request, response); err != nil {
			return nil, err
		}
		return response, nil
	}
	response, err := provider.delegate.PromptDecision(ctx, request)
	if err != nil || !durable {
		return response, err
	}
	if response == nil {
		return nil, errors.New("durable prompt: decision provider returned no response")
	}
	if err := validateDecisionResponse(request, response); err != nil {
		return nil, err
	}
	answer, err := json.Marshal(struct {
		Label string `json:"label"`
	}{Label: response.Label})
	if err != nil {
		return nil, err
	}
	if _, err := committer.AcceptInteraction(ctx, committed.TurnID, engine.InteractionPayloadDigest(answer), answer); err != nil {
		return nil, err
	}
	return response, nil
}

func (provider *durablePromptProvider) PromptForm(
	ctx context.Context,
	request inputpkg.FormRequest,
) (*inputpkg.FormResponse, error) {
	committed, committer, durable, err := prepareTerminalInteraction(ctx, request.StepID, "collector", request)
	if err != nil {
		return nil, err
	}
	if durable && committed.Status == engine.InteractionStatusAnswered {
		var answer struct {
			Values map[string]any `json:"values"`
		}
		if err := json.Unmarshal(committed.Answer, &answer); err != nil {
			return nil, fmt.Errorf("durable prompt: decode form answer: %w", err)
		}
		response := &inputpkg.FormResponse{Values: answer.Values}
		if err := validateFormResponse(request, response); err != nil {
			return nil, err
		}
		return response, nil
	}
	response, err := provider.delegate.PromptForm(ctx, request)
	if err != nil || !durable {
		return response, err
	}
	if response == nil {
		return nil, errors.New("durable prompt: form provider returned no response")
	}
	if err := validateFormResponse(request, response); err != nil {
		return nil, err
	}
	answer, err := json.Marshal(struct {
		Values map[string]any `json:"values"`
	}{Values: response.Values})
	if err != nil {
		return nil, err
	}
	if _, err := committer.AcceptInteraction(ctx, committed.TurnID, engine.InteractionPayloadDigest(answer), answer); err != nil {
		return nil, err
	}
	return response, nil
}

func validateChoiceResponse(request inputpkg.ChoiceRequest, response *inputpkg.ChoiceResponse) error {
	selected := response.Selected
	if len(selected) == 0 && request.Default != "" {
		selected = []string{request.Default}
	}
	if !request.Multiple && len(selected) > 1 || request.Min > 0 && len(selected) < request.Min ||
		request.Max > 0 && len(selected) > request.Max {
		return errors.New("durable prompt: choice answer violates selection limits")
	}
	allowed := make(map[string]bool, len(request.Options))
	for _, option := range request.Options {
		allowed[option.Value] = true
	}
	for _, value := range selected {
		if !allowed[value] {
			return errors.New("durable prompt: choice answer is not a declared option")
		}
	}
	return nil
}

func validateDecisionResponse(request inputpkg.DecisionRequest, response *inputpkg.DecisionResponse) error {
	for _, route := range request.Routes {
		if route.Label == response.Label {
			return nil
		}
	}
	return errors.New("durable prompt: decision answer is not a declared route")
}

func validateFormResponse(request inputpkg.FormRequest, response *inputpkg.FormResponse) error {
	if failures := inputpkg.ValidateFormResponse(request, *response); len(failures) > 0 {
		return fmt.Errorf("durable prompt: form answer is invalid: %v", failures)
	}
	return nil
}

func prepareTerminalInteraction(
	ctx context.Context,
	stepID string,
	kind string,
	request any,
) (engine.InteractionState, engine.InteractionCommitter, bool, error) {
	committer := engine.InteractionCommitterFromContext(ctx)
	if committer == nil {
		return engine.InteractionState{}, nil, false, nil
	}
	tracker := engine.InteractionInvocationTrackerFromContext(ctx)
	if tracker == nil {
		return engine.InteractionState{}, nil, false, errors.New("durable prompt: interaction invocation tracker is required")
	}
	boundary, ok := engine.DispatchExecutionBoundaryFromContext(ctx)
	if !ok {
		boundary.StepID = stepID
		boundary.CallPath = engine.DebugCallPathFromContext(ctx)
		boundary.QualifiedNodeID = engine.DebugNodeID(boundary.CallPath, stepID)
		boundary.Invocation = 1
	}
	requestBytes, err := json.Marshal(struct {
		Kind    string `json:"kind"`
		Request any    `json:"request"`
	}{Kind: kind, Request: request})
	if err != nil {
		return engine.InteractionState{}, nil, false, err
	}
	proposed := engine.InteractionState{
		SchemaVersion: engine.InteractionStateSchemaV1, TurnID: uuid.NewString(),
		NodeID: boundary.QualifiedNodeID, StepID: stepID,
		FrameID: boundary.FrameID, FrameStepIndex: boundary.FrameStepIndex, Kind: kind,
		Ordinal:       tracker.NextOccurrence(boundary.QualifiedNodeID, kind, boundary.FrameID, boundary.FrameStepIndex),
		Status:        engine.InteractionStatusPending,
		RequestDigest: engine.InteractionPayloadDigest(requestBytes), Request: requestBytes,
	}
	committed, err := committer.PrepareInteraction(ctx, proposed)
	if err != nil {
		return engine.InteractionState{}, nil, false, err
	}
	return committed, committer, true, nil
}
