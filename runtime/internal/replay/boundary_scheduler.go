package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/eventbus"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	sharedreplay "github.com/ormasoftchile/yawr/runtime/pkg/replay"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type replayBoundaryScheduler struct {
	mu           sync.Mutex
	initErr      error
	steps        map[string]sharedreplay.StepBinding
	hostActions  map[string]sharedreplay.HostActionBinding
	interactions map[string]sharedreplay.InteractionBinding
	approvals    map[string]sharedreplay.ApprovalBinding
	waitEvents   map[string]WaitEventBinding
	invocations  map[string]int
	consumed     map[string]bool
}

func newReplayBoundaryScheduler(scenario *Scenario) *replayBoundaryScheduler {
	scheduler := &replayBoundaryScheduler{
		steps: make(map[string]sharedreplay.StepBinding), hostActions: make(map[string]sharedreplay.HostActionBinding),
		interactions: make(map[string]sharedreplay.InteractionBinding), approvals: make(map[string]sharedreplay.ApprovalBinding),
		waitEvents: make(map[string]WaitEventBinding), invocations: make(map[string]int), consumed: make(map[string]bool),
	}
	if scenario == nil {
		scheduler.initErr = engine.NewReplayBoundaryError(errors.New("replay: scenario is required"))
		return scheduler
	}
	add := func(kind string, selector sharedreplay.Selector, runID string) (string, bool) {
		if scheduler.initErr != nil {
			return "", false
		}
		if err := validateReplaySelector(selector); err != nil {
			scheduler.initErr = engine.NewReplayBoundaryError(err)
			return "", false
		}
		if scenario.SourceRunID != "" && runID != scenario.SourceRunID {
			scheduler.initErr = engine.NewReplayBoundaryError(errors.New("replay: boundary fixture source run does not match scenario"))
			return "", false
		}
		if runID == "" {
			scheduler.initErr = engine.NewReplayBoundaryError(errors.New("replay: boundary fixture source run is required"))
			return "", false
		}
		key := replayBindingKey(kind, selector)
		if _, exists := scheduler.consumed[key]; exists {
			scheduler.initErr = engine.NewReplayBoundaryError(errors.New("replay: duplicate exact boundary fixture"))
			return "", false
		}
		scheduler.consumed[key] = false
		return key, true
	}
	for _, binding := range scenario.StepResponses {
		if key, ok := add(binding.Kind, binding.At, binding.Source.RunID); ok {
			scheduler.steps[key] = binding
		}
	}
	for _, binding := range scenario.HostActionResponses {
		if key, ok := add("host_action", binding.At, binding.Source.RunID); ok {
			scheduler.hostActions[key] = binding
		}
	}
	for _, binding := range scenario.InteractionAnswers {
		if key, ok := add(binding.Kind, binding.At, binding.Source.RunID); ok {
			scheduler.interactions[key] = binding
		}
	}
	for _, binding := range scenario.Approvals {
		if key, ok := add("approval", binding.At, binding.Source.RunID); ok {
			scheduler.approvals[key] = binding
		}
	}
	for _, binding := range scenario.WaitEvents {
		if key, ok := add("wait_for_event", binding.At, binding.Provenance.RunID); ok {
			scheduler.waitEvents[key] = binding
		}
	}
	for key := range scheduler.consumed {
		if _, ok := scheduler.steps[key]; ok {
			continue
		}
		if _, ok := scheduler.hostActions[key]; ok {
			continue
		}
		if _, ok := scheduler.interactions[key]; ok {
			continue
		}
		if _, ok := scheduler.approvals[key]; ok {
			continue
		}
		if _, ok := scheduler.waitEvents[key]; !ok {
			scheduler.initErr = engine.NewReplayBoundaryError(errors.New("replay: exact boundary fixture was not indexed"))
		}
	}
	for key := range scheduler.consumed {
		scheduler.consumed[key] = false
	}
	return scheduler
}

func validateReplaySelector(selector sharedreplay.Selector) error {
	if selector.Step == "" || selector.Phase != "execute" || selector.Invocation < 1 || selector.Attempt < 1 {
		return errors.New("replay: boundary fixture selector is invalid")
	}
	for _, stepID := range selector.CallPath {
		if stepID == "" {
			return errors.New("replay: boundary fixture call path is invalid")
		}
	}
	expected := selector.Step
	if len(selector.CallPath) > 0 {
		expected = selector.CallPath[0]
		for _, stepID := range selector.CallPath[1:] {
			expected += "/" + stepID
		}
		expected += "/" + selector.Step
	}
	if selector.QualifiedNodeID != "" && selector.QualifiedNodeID != expected {
		return errors.New("replay: boundary fixture qualified node is invalid")
	}
	for _, identity := range selector.StructuralPath {
		if identity.QualifiedNodeID == "" || identity.Kind == "" || identity.Invocation < 1 {
			return errors.New("replay: boundary fixture structural path is invalid")
		}
	}
	return nil
}

func replayBindingKey(kind string, selector sharedreplay.Selector) string {
	encoded, _ := json.Marshal(struct {
		Kind            string                               `json:"kind"`
		QualifiedNodeID string                               `json:"qualified_node_id"`
		CallPath        []string                             `json:"call_path,omitempty"`
		StructuralPath  []schema.DynamicIncludeFrameIdentity `json:"structural_path,omitempty"`
		Step            string                               `json:"step"`
		Phase           string                               `json:"phase"`
		Invocation      int                                  `json:"invocation"`
		Attempt         int                                  `json:"attempt"`
	}{kind, selector.QualifiedNodeID, selector.CallPath, selector.StructuralPath, selector.Step,
		selector.Phase, selector.Invocation, selector.Attempt})
	return string(encoded)
}

func (scheduler *replayBoundaryScheduler) nextSelector(ctx context.Context, kind, stepID string) sharedreplay.Selector {
	boundary, found := engine.DispatchExecutionBoundaryFromContext(ctx)
	callPath := engine.DebugCallPathFromContext(ctx)
	qualifiedNodeID := engine.DebugNodeID(callPath, stepID)
	attempt := 1
	invocation := 0
	if found {
		callPath = boundary.CallPath
		if boundary.QualifiedNodeID != "" {
			qualifiedNodeID = boundary.QualifiedNodeID
		}
		if boundary.RetryAttempt > 0 {
			attempt = boundary.RetryAttempt
		}
		if boundary.Invocation > 0 {
			invocation = boundary.Invocation
		}
	}
	parts := make([]string, len(callPath))
	for index, frame := range callPath {
		parts[index] = frame.StepID
	}
	structuralPath := engine.DynamicIncludeStructuralPathFromContext(ctx)
	base, _ := json.Marshal(struct {
		Kind            string                               `json:"kind"`
		QualifiedNodeID string                               `json:"qualified_node_id"`
		StructuralPath  []schema.DynamicIncludeFrameIdentity `json:"structural_path,omitempty"`
		Attempt         int                                  `json:"attempt"`
	}{kind, qualifiedNodeID, structuralPath, attempt})
	if invocation == 0 {
		scheduler.mu.Lock()
		scheduler.invocations[string(base)]++
		invocation = scheduler.invocations[string(base)]
		scheduler.mu.Unlock()
	}
	return sharedreplay.Selector{
		QualifiedNodeID: qualifiedNodeID, CallPath: parts, StructuralPath: structuralPath,
		Step: stepID, Phase: "execute", Invocation: invocation, Attempt: attempt,
	}
}

func (scheduler *replayBoundaryScheduler) consume(kind string, selector sharedreplay.Selector) (string, error) {
	key := replayBindingKey(kind, selector)
	if scheduler.initErr != nil {
		return "", scheduler.initErr
	}
	if scheduler.consumed[key] {
		return "", engine.NewReplayBoundaryError(fmt.Errorf("replay: %s fixture already consumed", kind))
	}
	return key, nil
}

func (scheduler *replayBoundaryScheduler) savedStep(
	ctx context.Context,
	kind string,
	step engine.ResolvedStep,
) (*engine.StepResult, error) {
	selector := scheduler.nextSelector(ctx, kind, step.ID)
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	key, err := scheduler.consume(kind, selector)
	if err != nil {
		return nil, err
	}
	binding, found := scheduler.steps[key]
	if !found {
		return nil, engine.NewReplayBoundaryError(fmt.Errorf("replay: no exact %s fixture for %s", kind, selector.QualifiedNodeID))
	}
	status := engine.StepStatus(binding.Status)
	var outcome engine.StepOutcome
	switch status {
	case engine.StepStatusCompleted:
		outcome = engine.StepOutcomeSuccess
	case engine.StepStatusSkipped:
		outcome = engine.StepOutcomeSkipped
	case engine.StepStatusDenied:
		outcome = engine.StepOutcomeDenied
	case engine.StepStatusFailed:
		outcome = engine.StepOutcomeFailed
	default:
		return nil, engine.NewReplayBoundaryError(errors.New("replay: saved step fixture has invalid status"))
	}
	if binding.Outcome != "" && engine.StepOutcome(binding.Outcome) != outcome {
		return nil, engine.NewReplayBoundaryError(errors.New("replay: saved step fixture outcome is inconsistent"))
	}
	now := time.Now()
	result := &engine.StepResult{
		StepID: step.ID, Status: status, Outcome: outcome,
		Output: cloneReplayMap(binding.Output), Vars: cloneReplayMap(binding.Vars),
		StartedAt: now, CompletedAt: now,
	}
	if status == engine.StepStatusFailed || status == engine.StepStatusDenied {
		result.Error = errors.New("replay: saved boundary result was not successful")
	}
	scheduler.consumed[key] = true
	return result, nil
}

func (scheduler *replayBoundaryScheduler) hasSavedStepKind(kind string) bool {
	for _, binding := range scheduler.steps {
		if binding.Kind == kind {
			return true
		}
	}
	return false
}

func (scheduler *replayBoundaryScheduler) PromptChoice(ctx context.Context, request input.ChoiceRequest) (*input.ChoiceResponse, error) {
	binding, _, err := scheduler.interaction(ctx, "choice", request.StepID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(request.Options))
	for _, option := range request.Options {
		allowed[option.Value] = true
	}
	invalid := !request.Multiple && len(binding.Selected) > 1 || request.Min > 0 && len(binding.Selected) < request.Min || request.Max > 0 && len(binding.Selected) > request.Max
	for _, selected := range binding.Selected {
		invalid = invalid || !allowed[selected]
	}
	if invalid {
		return nil, engine.NewReplayBoundaryError(errors.New("replay: saved choice does not match current options"))
	}
	return &input.ChoiceResponse{Selected: append([]string(nil), binding.Selected...)}, nil
}

func (scheduler *replayBoundaryScheduler) PromptDecision(ctx context.Context, request input.DecisionRequest) (*input.DecisionResponse, error) {
	binding, _, err := scheduler.interaction(ctx, "decision", request.StepID)
	if err != nil {
		return nil, err
	}
	valid := false
	for _, route := range request.Routes {
		valid = valid || route.Label == binding.Label
	}
	if !valid {
		return nil, engine.NewReplayBoundaryError(errors.New("replay: saved decision does not match current routes"))
	}
	return &input.DecisionResponse{Label: binding.Label}, nil
}

func (scheduler *replayBoundaryScheduler) PromptForm(ctx context.Context, request input.FormRequest) (*input.FormResponse, error) {
	binding, _, err := scheduler.interaction(ctx, "collector", request.StepID)
	if err != nil {
		return nil, err
	}
	response := &input.FormResponse{Values: cloneReplayMap(binding.Values)}
	if failures := input.ValidateFormResponse(request, *response); len(failures) > 0 {
		sort.Strings(failures)
		return nil, engine.NewReplayBoundaryError(fmt.Errorf("replay: saved form does not match current fields: %v", failures))
	}
	return response, nil
}

func (scheduler *replayBoundaryScheduler) interaction(ctx context.Context, kind, stepID string) (sharedreplay.InteractionBinding, string, error) {
	selector := scheduler.nextSelector(ctx, kind, stepID)
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	key, err := scheduler.consume(kind, selector)
	if err != nil {
		return sharedreplay.InteractionBinding{}, "", err
	}
	binding, found := scheduler.interactions[key]
	if !found {
		return sharedreplay.InteractionBinding{}, "", engine.NewReplayBoundaryError(fmt.Errorf("replay: no exact %s fixture for %s", kind, selector.QualifiedNodeID))
	}
	scheduler.consumed[key] = true
	return binding, key, nil
}

func (scheduler *replayBoundaryScheduler) RequestApproval(ctx context.Context, stepID, _ string) (governance.ApprovalRecord, error) {
	selector := scheduler.nextSelector(ctx, "approval", stepID)
	scheduler.mu.Lock()
	key, err := scheduler.consume("approval", selector)
	if err != nil {
		scheduler.mu.Unlock()
		return governance.ApprovalRecord{}, err
	}
	binding, found := scheduler.approvals[key]
	if !found {
		scheduler.mu.Unlock()
		return governance.ApprovalRecord{}, engine.NewReplayBoundaryError(fmt.Errorf("replay: no exact approval fixture for %s", selector.QualifiedNodeID))
	}
	scheduler.consumed[key] = true
	if !binding.Approved {
		scheduler.mu.Unlock()
		return governance.ApprovalRecord{}, engine.NewReplayBoundaryError(errors.New("replay: saved approval was denied"))
	}
	scheduler.mu.Unlock()
	approvedAt := binding.Review.ReviewedAt
	if approvedAt == "" {
		approvedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return governance.ApprovalRecord{Approver: binding.Approver, ApprovedAt: approvedAt, Token: "replay:" + binding.Source.InteractionID}, nil
}

func (scheduler *replayBoundaryScheduler) ExecuteHostAction(ctx context.Context, request hostaction.Request) (hostaction.Response, error) {
	stepID := hostaction.StepIDFromContext(ctx)
	selector := scheduler.nextSelector(ctx, "host_action", stepID)
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	key, err := scheduler.consume("host_action", selector)
	if err != nil {
		return hostaction.Response{}, engine.NewReplayBoundaryError(err)
	}
	binding, found := scheduler.hostActions[key]
	if !found {
		return hostaction.Response{}, engine.NewReplayBoundaryError(
			fmt.Errorf("replay: no exact host-action fixture for %s", selector.QualifiedNodeID),
		)
	}
	if binding.Capability != string(request.Capability) {
		return hostaction.Response{}, engine.NewReplayBoundaryError(errors.New("replay: saved host-action capability changed"))
	}
	status := hostaction.Status(binding.Response.Status)
	if !hostaction.IsKnownStatus(status) {
		return hostaction.Response{}, engine.NewReplayBoundaryError(errors.New("replay: saved host-action status is invalid"))
	}
	scheduler.consumed[key] = true
	return hostaction.Response{Status: status, Result: cloneReplayMap(binding.Response.Result)}, nil
}

func (*replayBoundaryScheduler) IsRouteTestHostActionProvider() bool { return true }

func (scheduler *replayBoundaryScheduler) Wait(ctx context.Context, stepID string, filter eventbus.EventFilter, _ time.Duration) (*eventbus.InboundEvent, error) {
	selector := scheduler.nextSelector(ctx, "wait_for_event", stepID)
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	key, err := scheduler.consume("wait_for_event", selector)
	if err != nil {
		return nil, err
	}
	binding, found := scheduler.waitEvents[key]
	if !found {
		return nil, engine.NewReplayBoundaryError(fmt.Errorf("replay: no exact wait-event fixture for %s", selector.QualifiedNodeID))
	}
	if binding.Source != string(filter.Source) && string(filter.Source) != "" || filter.ID != "" && binding.EventID != filter.ID {
		return nil, engine.NewReplayBoundaryError(errors.New("replay: saved event does not match current filter"))
	}
	for name, value := range filter.Payload {
		if fmt.Sprint(binding.Payload[name]) != value {
			return nil, engine.NewReplayBoundaryError(errors.New("replay: saved event payload does not match current filter"))
		}
	}
	scheduler.consumed[key] = true
	return &eventbus.InboundEvent{
		EventID: binding.EventID, Source: binding.Source, Channel: binding.Channel,
		Payload: cloneReplayMap(binding.Payload), FilterMatched: binding.FilterMatched,
	}, nil
}

func (*replayBoundaryScheduler) Dispatch(eventbus.InboundEvent) error {
	return engine.NewReplayBoundaryError(errors.New("replay: live event dispatch is disabled"))
}

func (*replayBoundaryScheduler) Cancel(string, string) {}

func cloneReplayMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
