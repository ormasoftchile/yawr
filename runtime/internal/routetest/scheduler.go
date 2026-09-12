package routetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type Scheduler struct {
	mu                    sync.Mutex
	target                Selector
	hostBindings          map[string]HostActionBinding
	interactionBindings   map[string]InteractionBinding
	stepBindings          map[string]StepBinding
	approvalBindings      map[string]ApprovalBinding
	invocations           map[string]int
	consumed              map[string]bool
	targetReached         bool
	externalDispatchCount int
}

func NewScheduler(scenario Scenario) (*Scheduler, error) {
	if err := validateSelector("target", scenario.Target); err != nil {
		return nil, err
	}
	if scenario.Target.Phase != "before" {
		return nil, fmt.Errorf("route test: target phase must be before")
	}
	scheduler := &Scheduler{
		target:              scenario.Target,
		hostBindings:        make(map[string]HostActionBinding, len(scenario.HostActionResponses)),
		interactionBindings: make(map[string]InteractionBinding, len(scenario.InteractionAnswers)),
		stepBindings:        make(map[string]StepBinding, len(scenario.StepResponses)),
		approvalBindings:    make(map[string]ApprovalBinding, len(scenario.TestApprovals)),
		invocations:         make(map[string]int),
		consumed:            make(map[string]bool),
	}
	for _, binding := range scenario.HostActionResponses {
		if err := validateSelector("host action response", binding.At); err != nil {
			return nil, err
		}
		if binding.At.Phase != "execute" {
			return nil, fmt.Errorf("route test: host action response phase must be execute")
		}
		if err := validateReview("host action response", binding.Review); err != nil {
			return nil, err
		}
		status := hostaction.Status(binding.Response.Status)
		if !hostaction.IsKnownStatus(status) {
			return nil, fmt.Errorf("route test: host action response at %s has unknown status %q", selectorLabel(binding.At), status)
		}
		if status == hostaction.StatusCompleted && binding.Response.Result == nil {
			return nil, fmt.Errorf("route test: completed host action response at %s requires result", selectorLabel(binding.At))
		}
		if status != hostaction.StatusCompleted && binding.Response.Result != nil {
			return nil, fmt.Errorf("route test: host action response at %s with status %q must not include result", selectorLabel(binding.At), status)
		}
		key := bindingKey("host_action", binding.At)
		if _, exists := scheduler.hostBindings[key]; exists {
			return nil, fmt.Errorf("route test: duplicate host action response at %s", selectorLabel(binding.At))
		}
		scheduler.hostBindings[key] = binding
	}
	for _, binding := range scenario.InteractionAnswers {
		if err := validateSelector("interaction answer", binding.At); err != nil {
			return nil, err
		}
		if binding.At.Phase != "execute" {
			return nil, fmt.Errorf("route test: interaction answer phase must be execute")
		}
		if binding.Kind != "choice" && binding.Kind != "decision" && binding.Kind != "collector" {
			return nil, fmt.Errorf("route test: interaction answer at %s has unsupported kind %q", selectorLabel(binding.At), binding.Kind)
		}
		if err := validateReview("interaction answer", binding.Review); err != nil {
			return nil, err
		}
		key := bindingKey(binding.Kind, binding.At)
		if _, exists := scheduler.interactionBindings[key]; exists {
			return nil, fmt.Errorf("route test: duplicate %s answer at %s", binding.Kind, selectorLabel(binding.At))
		}
		scheduler.interactionBindings[key] = binding
	}
	for _, binding := range scenario.StepResponses {
		if err := validateSelector("step response", binding.At); err != nil {
			return nil, err
		}
		if binding.At.Phase != "execute" {
			return nil, fmt.Errorf("route test: step response phase must be execute")
		}
		if binding.Kind != "cli" && binding.Kind != "tool" {
			return nil, fmt.Errorf("route test: step response at %s has unsupported kind %q", selectorLabel(binding.At), binding.Kind)
		}
		if !knownStepStatus(engine.StepStatus(binding.Status)) {
			return nil, fmt.Errorf("route test: %s response at %s has unsupported status %q", binding.Kind, selectorLabel(binding.At), binding.Status)
		}
		if binding.Outcome != "" && engine.StepOutcome(binding.Outcome) != outcomeForStatus(engine.StepStatus(binding.Status)) {
			return nil, fmt.Errorf(
				"route test: %s response at %s has outcome %q inconsistent with status %q",
				binding.Kind, selectorLabel(binding.At), binding.Outcome, binding.Status,
			)
		}
		if err := validateReview("step response", binding.Review); err != nil {
			return nil, err
		}
		key := bindingKey(binding.Kind, binding.At)
		if _, exists := scheduler.stepBindings[key]; exists {
			return nil, fmt.Errorf("route test: duplicate %s response at %s", binding.Kind, selectorLabel(binding.At))
		}
		scheduler.stepBindings[key] = binding
	}
	for _, binding := range scenario.TestApprovals {
		if err := validateSelector("test approval", binding.At); err != nil {
			return nil, err
		}
		if binding.At.Phase != "execute" {
			return nil, fmt.Errorf("route test: test approval phase must be execute")
		}
		if binding.Approver == "" {
			return nil, fmt.Errorf("route test: approval at %s requires approver", selectorLabel(binding.At))
		}
		if err := validateReview("test approval", binding.Review); err != nil {
			return nil, err
		}
		key := bindingKey("approval", binding.At)
		if _, exists := scheduler.approvalBindings[key]; exists {
			return nil, fmt.Errorf("route test: duplicate approval at %s", selectorLabel(binding.At))
		}
		scheduler.approvalBindings[key] = binding
	}
	return scheduler, nil
}

// WrapRegistry preserves pure Yawr executors while replacing every executor
// boundary that could dispatch externally.
func (scheduler *Scheduler) WrapRegistry(inner engine.ExecutorRegistry) engine.ExecutorRegistry {
	return &routeTestRegistry{inner: inner, scheduler: scheduler}
}

func (scheduler *Scheduler) ExecuteHostAction(ctx context.Context, request hostaction.Request) (hostaction.Response, error) {
	stepID := hostaction.StepIDFromContext(ctx)
	selector := scheduler.nextSelector("host_action", engine.DebugCallPathFromContext(ctx), stepID)
	key := bindingKey("host_action", selector)

	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	binding, ok := scheduler.hostBindings[key]
	if !ok {
		return hostaction.Response{}, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: no host action response for %s", selectorLabel(selector)))
	}
	if scheduler.consumed[key] {
		return hostaction.Response{}, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: host action response already consumed at %s", selectorLabel(selector)))
	}
	if binding.Capability != "" && binding.Capability != string(request.Capability) {
		return hostaction.Response{}, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: host action capability at %s is %q, want %q", selectorLabel(selector), request.Capability, binding.Capability))
	}
	scheduler.consumed[key] = true
	return hostaction.Response{Status: hostaction.Status(binding.Response.Status), Result: cloneMap(binding.Response.Result)}, nil
}

func (scheduler *Scheduler) PromptChoice(ctx context.Context, request input.ChoiceRequest) (*input.ChoiceResponse, error) {
	binding, key, err := scheduler.pendingInteraction(ctx, "choice", request.StepID)
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
		return nil, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: choice answer at %s does not match declared options or selection limits", selectorLabel(binding.At)))
	}
	scheduler.consumeKey(key)
	return &input.ChoiceResponse{Selected: append([]string(nil), binding.Selected...)}, nil
}

func (scheduler *Scheduler) PromptDecision(ctx context.Context, request input.DecisionRequest) (*input.DecisionResponse, error) {
	binding, key, err := scheduler.pendingInteraction(ctx, "decision", request.StepID)
	if err != nil {
		return nil, err
	}
	valid := false
	for _, route := range request.Routes {
		valid = valid || route.Label == binding.Label
	}
	if !valid {
		return nil, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: decision answer at %s is not a declared route", selectorLabel(binding.At)))
	}
	scheduler.consumeKey(key)
	return &input.DecisionResponse{Label: binding.Label}, nil
}

func (scheduler *Scheduler) PromptForm(ctx context.Context, request input.FormRequest) (*input.FormResponse, error) {
	binding, key, err := scheduler.pendingInteraction(ctx, "collector", request.StepID)
	if err != nil {
		return nil, err
	}
	response := &input.FormResponse{Values: cloneMap(binding.Values)}
	if failures := input.ValidateFormResponse(request, *response); len(failures) > 0 {
		return nil, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: collector answer at %s is invalid: %s", selectorLabel(binding.At), strings.Join(failures, "; ")))
	}
	scheduler.consumeKey(key)
	return response, nil
}

func (scheduler *Scheduler) RequestApproval(ctx context.Context, stepID string, _ string) (governance.ApprovalRecord, error) {
	selector := scheduler.nextSelector("approval", engine.DebugCallPathFromContext(ctx), stepID)
	key := bindingKey("approval", selector)
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	binding, ok := scheduler.approvalBindings[key]
	if !ok {
		return governance.ApprovalRecord{}, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: no test approval for %s", selectorLabel(selector)))
	}
	if scheduler.consumed[key] {
		return governance.ApprovalRecord{}, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: test approval already consumed at %s", selectorLabel(selector)))
	}
	scheduler.consumed[key] = true
	if !binding.Approved {
		return governance.ApprovalRecord{}, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: test approval denied at %s", selectorLabel(selector)))
	}
	return governance.ApprovalRecord{
		Approver:   binding.Approver,
		ApprovedAt: binding.Review.ReviewedAt,
		Token:      "route-test:" + key,
	}, nil
}

func (scheduler *Scheduler) BeforeStep(_ context.Context, location engine.DebugLocation, step engine.ResolvedStep) (engine.RouteTestDecision, error) {
	if selectorMatchesLocation(scheduler.target, location) {
		scheduler.mu.Lock()
		scheduler.targetReached = true
		scheduler.mu.Unlock()
		return engine.RouteTestTargetReached, nil
	}
	switch step.Kind {
	case "wait_for_event", "extension", "prompt":
		return "", engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: %s step %s has no deterministic zero-dispatch adapter", step.Kind, step.ID))
	case "include":
		if spec, ok := step.Spec.(*schema.IncludeSpec); ok && spec != nil && spec.Include.IsDynamic() {
			return "", engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: dynamic include step %s cannot resolve during a route test", step.ID))
		}
	default:
		return engine.RouteTestContinue, nil
	}
	return engine.RouteTestContinue, nil
}

func (scheduler *Scheduler) TargetReached() bool {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return scheduler.targetReached
}

func (scheduler *Scheduler) ExternalDispatchCount() int {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return scheduler.externalDispatchCount
}

func (scheduler *Scheduler) RecordExternalDispatch() {
	scheduler.mu.Lock()
	scheduler.externalDispatchCount++
	scheduler.mu.Unlock()
}

func (scheduler *Scheduler) IsRouteTestHostActionProvider() bool { return true }

func (scheduler *Scheduler) Verify() error {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	var failures []string
	if !scheduler.targetReached {
		failures = append(failures, "target was not reached")
	}
	for key := range scheduler.hostBindings {
		if !scheduler.consumed[key] {
			failures = append(failures, "unused "+key)
		}
	}
	for key := range scheduler.interactionBindings {
		if !scheduler.consumed[key] {
			failures = append(failures, "unused "+key)
		}
	}
	for key := range scheduler.stepBindings {
		if !scheduler.consumed[key] {
			failures = append(failures, "unused "+key)
		}
	}
	for key := range scheduler.approvalBindings {
		if !scheduler.consumed[key] {
			failures = append(failures, "unused "+key)
		}
	}
	sort.Strings(failures)
	if len(failures) > 0 {
		return fmt.Errorf("route test verification failed: %s", strings.Join(failures, "; "))
	}
	return nil
}

type routeTestRegistry struct {
	inner     engine.ExecutorRegistry
	scheduler *Scheduler
}

func (registry *routeTestRegistry) Register(kind string, executor engine.StepExecutor) {
	if registry.inner != nil {
		registry.inner.Register(kind, executor)
	}
}

func (registry *routeTestRegistry) Lookup(kind string) engine.StepExecutor {
	if registry.inner == nil {
		return nil
	}
	inner := registry.inner.Lookup(kind)
	if inner == nil {
		return nil
	}
	switch kind {
	case "cli":
		return &scheduledStepExecutor{kind: kind, scheduler: registry.scheduler}
	case "tool":
		validator, _ := inner.(savedResultValidator)
		return &scheduledStepExecutor{kind: kind, scheduler: registry.scheduler, validator: validator, inner: inner}
	case "include", "choice", "decision", "collector", "host_action", "branch", "iterate", "parallel", "approve", "assert", "compensate", "end", "noop", "display":
		return inner
	default:
		return &blockedExecutor{kind: kind}
	}
}

type scheduledStepExecutor struct {
	kind      string
	scheduler *Scheduler
	validator savedResultValidator
	inner     engine.StepExecutor
}

type savedResultValidator interface {
	ValidateSavedResult(context.Context, engine.ResolvedStep, map[string]any, *engine.StepResult) (*engine.StepResult, error)
}

func (executor *scheduledStepExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	if pure, ok := executor.inner.(interface {
		IsPureRunbookSubstitution(context.Context, engine.ResolvedStep, map[string]any) (bool, error)
	}); ok {
		isPure, err := pure.IsPureRunbookSubstitution(ctx, step, vars)
		if err != nil {
			return nil, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: resolve tool boundary: %w", err))
		}
		if isPure {
			return executor.inner.Execute(ctx, step, vars)
		}
	}
	selector := executor.scheduler.nextSelector(executor.kind, engine.DebugCallPathFromContext(ctx), step.ID)
	key := bindingKey(executor.kind, selector)
	executor.scheduler.mu.Lock()
	binding, ok := executor.scheduler.stepBindings[key]
	if !ok {
		executor.scheduler.mu.Unlock()
		return nil, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: no %s response for %s", executor.kind, selectorLabel(selector)))
	}
	if executor.scheduler.consumed[key] {
		executor.scheduler.mu.Unlock()
		return nil, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: %s response already consumed at %s", executor.kind, selectorLabel(selector)))
	}

	now := time.Now()
	outcome := engine.StepOutcome(binding.Outcome)
	if outcome == "" {
		outcome = outcomeForStatus(engine.StepStatus(binding.Status))
	}
	result := &engine.StepResult{
		StepID: step.ID, Status: engine.StepStatus(binding.Status), Outcome: outcome,
		Output: cloneMap(binding.Output), Vars: make(map[string]any), StartedAt: now, CompletedAt: now,
	}
	if executor.kind != "tool" {
		executor.scheduler.consumed[key] = true
		executor.scheduler.mu.Unlock()
		return result, nil
	}
	if executor.validator == nil {
		executor.scheduler.mu.Unlock()
		return nil, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: tool step %s has no output-contract validator", step.ID))
	}
	validated, err := executor.validator.ValidateSavedResult(ctx, step, vars, result)
	if err != nil {
		executor.scheduler.mu.Unlock()
		return nil, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: saved tool result at %s: %w", selectorLabel(selector), err))
	}
	if validated == nil || validated.Status != engine.StepStatus(binding.Status) || validated.Outcome != outcome || validated.Error != nil {
		executor.scheduler.mu.Unlock()
		return nil, engine.NewRouteTestBoundaryError(fmt.Errorf(
			"route test safety failure: saved tool result at %s violates its runtime contract", selectorLabel(selector),
		))
	}
	executor.scheduler.consumed[key] = true
	executor.scheduler.mu.Unlock()
	return validated, nil
}

type blockedExecutor struct{ kind string }

func (executor *blockedExecutor) Execute(_ context.Context, step engine.ResolvedStep, _ map[string]any) (*engine.StepResult, error) {
	return nil, engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: %s step %s has no zero-dispatch adapter", executor.kind, step.ID))
}

func (scheduler *Scheduler) pendingInteraction(ctx context.Context, kind, stepID string) (InteractionBinding, string, error) {
	selector := scheduler.nextSelector(kind, engine.DebugCallPathFromContext(ctx), stepID)
	key := bindingKey(kind, selector)
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	binding, ok := scheduler.interactionBindings[key]
	if !ok {
		return InteractionBinding{}, "", engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: no %s answer for %s", kind, selectorLabel(selector)))
	}
	if scheduler.consumed[key] {
		return InteractionBinding{}, "", engine.NewRouteTestBoundaryError(fmt.Errorf("route test safety failure: %s answer already consumed at %s", kind, selectorLabel(selector)))
	}
	return binding, key, nil
}

func (scheduler *Scheduler) consumeKey(key string) {
	scheduler.mu.Lock()
	scheduler.consumed[key] = true
	scheduler.mu.Unlock()
}

func (scheduler *Scheduler) nextSelector(kind string, path []engine.DebugCallFrame, stepID string) Selector {
	callPath := make([]string, len(path))
	for index, frame := range path {
		callPath[index] = frame.StepID
	}
	counterKey := bindingKey(kind, Selector{CallPath: callPath, Step: stepID, Invocation: 0})
	scheduler.mu.Lock()
	scheduler.invocations[counterKey]++
	invocation := scheduler.invocations[counterKey]
	scheduler.mu.Unlock()
	return Selector{CallPath: callPath, Step: stepID, Phase: "execute", Invocation: invocation, Attempt: 1}
}

func validateSelector(name string, selector Selector) error {
	if selector.Step == "" {
		return fmt.Errorf("route test: %s step is required", name)
	}
	if selector.Invocation < 1 {
		return fmt.Errorf("route test: %s at %s requires a positive invocation", name, selectorLabel(selector))
	}
	if selector.Phase != "before" && selector.Phase != "execute" {
		return fmt.Errorf("route test: %s at %s requires phase before or execute", name, selectorLabel(selector))
	}
	if selector.Attempt != 1 {
		return fmt.Errorf("route test: %s at %s currently requires attempt 1", name, selectorLabel(selector))
	}
	for _, part := range selector.CallPath {
		if part == "" {
			return fmt.Errorf("route test: %s at %s contains an empty call path", name, selectorLabel(selector))
		}
	}
	return nil
}

func validateReview(name string, review Review) error {
	if review.State != "reviewed" || review.ReviewedBy == "" || !review.SensitivityReviewed {
		return fmt.Errorf("route test: %s must be reviewed", name)
	}
	if _, err := time.Parse(time.RFC3339, review.ReviewedAt); err != nil {
		return fmt.Errorf("route test: %s reviewed_at must be RFC3339", name)
	}
	return nil
}

func selectorMatchesLocation(selector Selector, location engine.DebugLocation) bool {
	if selector.Phase != "before" || selector.Step != location.StepID || selector.Invocation != location.Invocation || selector.Attempt != location.Attempt || len(selector.CallPath) != len(location.CallPath) {
		return false
	}
	for index, part := range selector.CallPath {
		if part != location.CallPath[index].StepID {
			return false
		}
	}
	return true
}

func bindingKey(kind string, selector Selector) string {
	path := []byte("[]")
	if len(selector.CallPath) > 0 {
		path, _ = json.Marshal(selector.CallPath)
	}
	return fmt.Sprintf("%s:%s:%s:%s:%d:%d", kind, path, selector.Step, selector.Phase, selector.Invocation, selector.Attempt)
}

func selectorLabel(selector Selector) string {
	path := strings.Join(append(append([]string(nil), selector.CallPath...), selector.Step), "/")
	return fmt.Sprintf("%s %s invocation %d attempt %d", path, selector.Phase, selector.Invocation, selector.Attempt)
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func knownStepStatus(status engine.StepStatus) bool {
	switch status {
	case engine.StepStatusCompleted, engine.StepStatusFailed, engine.StepStatusSkipped, engine.StepStatusDenied:
		return true
	default:
		return false
	}
}

func outcomeForStatus(status engine.StepStatus) engine.StepOutcome {
	switch status {
	case engine.StepStatusCompleted:
		return engine.StepOutcomeSuccess
	case engine.StepStatusSkipped:
		return engine.StepOutcomeSkipped
	case engine.StepStatusDenied:
		return engine.StepOutcomeDenied
	default:
		return engine.StepOutcomeFailed
	}
}
