package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	internaltrace "github.com/ormasoftchile/yawr/runtime/internal/trace"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	evidencepkg "github.com/ormasoftchile/yawr/runtime/pkg/evidence"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/planner"
	sharedreplay "github.com/ormasoftchile/yawr/runtime/pkg/replay"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// ReplayEngine drives replay execution from trace or scenario data.
type ReplayEngine struct {
	cfg           engine.EngineConfig
	parser        parser.Parser
	planner       planner.Planner
	frozenPlan    *engine.ExecutionPlan
	patternReplay bool
	// pins are the dynamic include records from the original run, used to
	// re-bind without re-resolving during replay.
	// Set via WithPins, typically loaded from the run store by the caller.
	pins []schema.LockedDynamicInclude
}

// NewReplayEngine constructs a ReplayEngine from dependencies.
func NewReplayEngine(cfg engine.EngineConfig, parser parser.Parser, planner planner.Planner) *ReplayEngine {
	return &ReplayEngine{cfg: cfg, parser: parser, planner: planner}
}

// WithPins attaches dynamic-include pin records to this engine so that
// ReplayFromTrace re-binds every dynamic include to its pinned AbsPath
// instead of re-resolving against the current catalog. Pins are
// populated by the executor at run time and persisted via
// PlanMetadata.DynamicIncludes; callers load them from the run store
// (engine.RunStore.LoadState) and pass them here before replaying.
// Returns e for chaining.
func (r *ReplayEngine) WithPins(pins []schema.LockedDynamicInclude) *ReplayEngine {
	r.pins = pins
	return r
}

func (r *ReplayEngine) WithFrozenPlan(plan *engine.ExecutionPlan) *ReplayEngine {
	r.frozenPlan = plan
	return r
}

func (r *ReplayEngine) WithPatternFixtureReplay() *ReplayEngine {
	r.patternReplay = true
	return r
}

// ReplayFromTrace replays a run using a trace file and optional scenario override.
func (r *ReplayEngine) ReplayFromTrace(ctx context.Context, tracePath string, scenarioPath string, opts engine.RunOptions) (engine.RunHandle, error) {
	if r == nil {
		return nil, errors.New("replay: engine is nil")
	}
	if tracePath == "" {
		return nil, errors.New("replay: trace path is required")
	}

	events, err := loadTraceEvents(ctx, tracePath)
	if err != nil {
		return nil, err
	}
	sourceRunID, runbookPath, err := replayTraceSource(events, true)
	if err != nil {
		return nil, err
	}
	sourceLock, sourceLockFields, err := dependencyLockFromTrace(events, sourceRunID)
	if err != nil {
		return nil, err
	}

	var scenario *Scenario
	var evidenceRecords map[string][]evidencepkg.EvidenceRecord
	if scenarioPath != "" {
		scenario, err = LoadScenario(scenarioPath)
		if err != nil {
			return nil, err
		}
	}
	patternFixtures := scenario != nil && scenario.PatternFixtures
	if patternFixtures && !r.patternReplay {
		return nil, engine.NewReplayBoundaryError(errors.New("replay: pattern-fixture replay must be explicitly enabled"))
	}
	if !patternFixtures && r.patternReplay {
		return nil, engine.NewReplayBoundaryError(errors.New("replay: pattern-fixture replay cannot execute an exact replay"))
	}
	if !patternFixtures {
		if r.frozenPlan == nil {
			return nil, engine.NewReplayBoundaryError(errors.New("replay: strict replay requires a frozen plan"))
		}
		if err := validateStrictDependencyRecord(sourceLock, sourceLockFields); err != nil {
			return nil, err
		}
		if scenario != nil && scenario.DependencyLock != sourceLock {
			return nil, engine.NewReplayBoundaryError(errors.New("replay: scenario dependency lock does not match source trace"))
		}
	} else if r.frozenPlan == nil && (r.parser == nil || r.planner == nil) {
		return nil, errors.New("replay: pattern-fixture parser and planner are required")
	}

	var plan *engine.ExecutionPlan
	if r.frozenPlan != nil {
		snapshot, snapshotErr := plansnapshot.FromExecutionPlan(r.frozenPlan)
		if snapshotErr != nil {
			return nil, fmt.Errorf("replay: snapshot frozen plan: %w", snapshotErr)
		}
		plan, err = plansnapshot.Restore(snapshot)
		if err != nil {
			return nil, fmt.Errorf("replay: restore frozen plan: %w", err)
		}
		if !patternFixtures {
			for index, pin := range plan.Metadata.DynamicIncludes {
				if len(pin.ExecutableClosure) == 0 {
					return nil, engine.NewReplayBoundaryError(fmt.Errorf(
						"replay: strict dynamic include pin %d requires an executable closure", index,
					))
				}
			}
		}
		if filepath.Clean(plan.RunbookPath) != filepath.Clean(runbookPath) {
			return nil, errors.New("replay: frozen plan runbook path does not match trace")
		}
	} else {
		parsed, parseErr := r.parser.Parse(ctx, runbookPath)
		if parseErr != nil {
			return nil, parseErr
		}
		plan, err = r.planner.Plan(ctx, parsed)
		if err != nil {
			return nil, err
		}
	}

	if scenario != nil {
		evidenceRecords = evidenceRecordsFromScenario(scenario)
	} else {
		scenarioPlan := plan
		scenario, evidenceRecords, err = buildScenarioFromTrace(scenarioPlan, events)
		if err != nil {
			return nil, err
		}
	}
	if scenario.SourceRunID != "" && scenario.SourceRunID != sourceRunID {
		return nil, errors.New("replay: scenario source run does not match trace")
	}
	scenario.SourceRunID = sourceRunID
	if scenarioPath == "" {
		scenario.PatternFixtures = false
		scenario.StrictExactFixtures = true
	}
	lock := scenario.DependencyLock
	if scenario.StrictExactFixtures {
		lock = sourceLock
	}
	if err := validateReplayDependencyLock(plan, lock, scenario.StrictExactFixtures); err != nil {
		return nil, err
	}
	pins := r.pins
	if scenario.StrictExactFixtures {
		pins = plan.Metadata.DynamicIncludes
	}

	cfg := r.cfg
	// R5 (barbara-enum-mvp-implementation-gate.md): the plan's tool
	// definitions and the configured evaluator are available here (the
	// plan already exists), so replayed tool-call steps get the same
	// ENUM-008 enforcement the real ToolExecutor performs, instead of
	// silently bypassing it.
	// §8.3 (barbara-dynamic-include-contract.md): if pin records were
	// provided via WithPins, wire the DynamicIncludeReplayExecutor so
	// that dynamic include steps re-bind from their pinned AbsPath instead
	// of re-resolving against the current catalog.
	replayRegistry := NewReplayExecutorRegistryWithPins(
		cfg.Executors, scenario, cfg.Evaluator, plan.Tools,
		cfg.ConditionEvaluator, pins, cfg.TraceWriter, plan.RunID,
	)
	cfg.Executors = replayRegistry
	cfg.Dispatcher = replayRegistry.scheduler
	cfg.ApprovalGate = replayRegistry.scheduler
	if scenario.StrictExactFixtures {
		cfg.ExtensionHost = nil
		cfg.EvidenceHook = newReplayEvidenceHook(evidenceRecords, nil)
	} else {
		cfg.EvidenceHook = newReplayEvidenceHook(evidenceRecords, cfg.EvidenceHook)
	}
	replayRegistry.WithEvidenceHook(cfg.EvidenceHook)
	var transitionValidator *replayTransitionValidator
	if scenario.StrictExactFixtures {
		transitionValidator = &replayTransitionValidator{
			expected: append([]sharedreplay.ExpectedTransition(nil), scenario.ExpectedTransitions...),
		}
		cfg.TransitionValidator = transitionValidator
	}

	opts.Mode = engine.RunModeReplay
	eng := internalengine.New(cfg)
	handle, err := eng.Start(ctx, engine.ValidatedForTest(plan), opts)
	if err != nil {
		return nil, err
	}
	return handle, nil
}

type replayTransitionValidator struct {
	mu       sync.Mutex
	expected []sharedreplay.ExpectedTransition
	next     int
}

func (validator *replayTransitionValidator) ValidateHandoff(_ context.Context, request engine.HandoffRequest) error {
	validator.mu.Lock()
	defer validator.mu.Unlock()
	if validator.next >= len(validator.expected) {
		return engine.NewReplayBoundaryError(errors.New("replay: unexpected handoff"))
	}
	if !replayHandoffMatches(validator.expected[validator.next], request) {
		return engine.NewReplayBoundaryError(errors.New("replay: handoff transition drift"))
	}
	validator.next++
	return nil
}

func (validator *replayTransitionValidator) ValidateCompletion(context.Context) error {
	validator.mu.Lock()
	defer validator.mu.Unlock()
	if validator.next != len(validator.expected) {
		return engine.NewReplayBoundaryError(errors.New("replay: expected handoff was not reached"))
	}
	return nil
}

func replayHandoffMatches(expected sharedreplay.ExpectedTransition, request engine.HandoffRequest) bool {
	if expected.TargetRunbook != request.TargetRunbook || expected.ReasonCode != request.ReasonCode ||
		expected.At.Step != request.StepID || expected.At.Phase != "execute" ||
		expected.At.Invocation != request.Invocation || expected.At.Attempt != request.RetryAttempt ||
		expected.At.QualifiedNodeID != "" && expected.At.QualifiedNodeID != request.QualifiedNodeID ||
		len(expected.At.CallPath) != len(request.CallPath) ||
		len(expected.At.StructuralPath) != len(request.StructuralPath) {
		return false
	}
	for index, stepID := range expected.At.CallPath {
		if stepID != request.CallPath[index].StepID {
			return false
		}
	}
	for index, identity := range expected.At.StructuralPath {
		if identity != request.StructuralPath[index] {
			return false
		}
	}
	return true
}

func loadTraceEvents(ctx context.Context, path string) ([]tracepkg.TraceEvent, error) {
	reader := internaltrace.NewJSONLReader(path)
	return reader.ReadAllStrict(ctx)
}

func replayTraceSource(events []tracepkg.TraceEvent, required bool) (string, string, error) {
	found := false
	runID, runbookPath := "", ""
	for _, event := range events {
		if event.Kind != tracepkg.EventKindRunStarted {
			continue
		}
		if found {
			return "", "", errors.New("replay: trace contains multiple run starts")
		}
		found = true
		if event.RunID == "" {
			return "", "", errors.New("replay: run start has no run id")
		}
		var payload struct {
			RunbookPath string `json:"runbook_path"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return "", "", fmt.Errorf("replay: decode run start: %w", err)
		}
		if payload.RunbookPath == "" {
			return "", "", errors.New("replay: run start has no runbook path")
		}
		runID, runbookPath = event.RunID, payload.RunbookPath
	}
	if !found && required {
		return "", "", errors.New("replay: run start not found in trace")
	}
	return runID, runbookPath, nil
}

func dependencyLockFromTrace(events []tracepkg.TraceEvent, sourceRunID string) (sharedreplay.DependencyLock, map[string]bool, error) {
	for _, event := range events {
		if event.Kind != tracepkg.EventKindRunStarted || event.RunID != sourceRunID {
			continue
		}
		var payload struct {
			PlanHash          string `json:"plan_hash"`
			GraphHash         string `json:"graph_hash"`
			CatalogDigest     string `json:"catalog_digest"`
			PackageLockDigest string `json:"package_lock_digest"`
			ToolDigest        string `json:"tool_digest"`
			ProfileDigest     string `json:"profile_digest"`
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(event.Payload, &fields); err != nil {
			return sharedreplay.DependencyLock{}, nil, fmt.Errorf("replay: decode dependency lock: %w", err)
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return sharedreplay.DependencyLock{}, nil, fmt.Errorf("replay: decode dependency lock: %w", err)
		}
		present := make(map[string]bool, len(fields))
		for name := range fields {
			present[name] = true
		}
		return sharedreplay.DependencyLock(payload), present, nil
	}
	return sharedreplay.DependencyLock{}, nil, nil
}

func validateStrictDependencyRecord(lock sharedreplay.DependencyLock, fields map[string]bool) error {
	for _, field := range []string{
		"plan_hash", "graph_hash", "catalog_digest", "package_lock_digest", "tool_digest", "profile_digest",
	} {
		if !fields[field] {
			return engine.NewReplayBoundaryError(fmt.Errorf("replay: strict dependency metadata is missing %s", field))
		}
	}
	if lock.PlanHash == "" || lock.GraphHash == "" || lock.CatalogDigest == "" ||
		lock.PackageLockDigest == "" || lock.ToolDigest == "" || lock.ProfileDigest == "" {
		return engine.NewReplayBoundaryError(errors.New("replay: strict dependency metadata contains an empty required digest"))
	}
	return nil
}

func validateReplayDependencyLock(
	plan *engine.ExecutionPlan,
	lock sharedreplay.DependencyLock,
	strict bool,
) error {
	if lock.PlanHash == "" {
		return nil
	}
	current, err := sharedreplay.DependencyLockForPlan(plan)
	if err != nil {
		return fmt.Errorf("replay: build current dependency lock: %w", err)
	}
	for name, expected := range map[string]string{
		"plan": lock.PlanHash, "graph": lock.GraphHash, "catalog": lock.CatalogDigest,
		"package lock": lock.PackageLockDigest, "tools": lock.ToolDigest, "profile": lock.ProfileDigest,
	} {
		if !strict && expected == "" {
			continue
		}
		actual := map[string]string{
			"plan": current.PlanHash, "graph": current.GraphHash, "catalog": current.CatalogDigest,
			"package lock": current.PackageLockDigest, "tools": current.ToolDigest, "profile": current.ProfileDigest,
		}[name]
		if actual != expected {
			return engine.NewReplayBoundaryError(fmt.Errorf("replay: %s dependency drift", name))
		}
	}
	return nil
}

func buildScenarioFromTrace(plan *engine.ExecutionPlan, events []tracepkg.TraceEvent) (*Scenario, map[string][]evidencepkg.EvidenceRecord, error) {
	if plan == nil {
		return nil, nil, errors.New("replay: plan is required")
	}
	stepByID := make(map[string]engine.ResolvedStep, len(plan.Steps))
	for _, step := range plan.Steps {
		stepByID[step.ID] = step
	}
	dynamicSites, err := replayDynamicIncludeSites(plan)
	if err != nil {
		return nil, nil, err
	}
	stepSites, err := replayStepSites(plan)
	if err != nil {
		return nil, nil, err
	}
	sourceRunID, _, err := replayTraceSource(events, false)
	if err != nil {
		return nil, nil, err
	}

	scenario := &Scenario{
		Tools: make(map[string]ToolFixture), Evidence: make(map[string]map[string]EvidenceFixture),
		SourceRunID: sourceRunID,
	}
	scenario.DependencyLock, _, err = dependencyLockFromTrace(events, sourceRunID)
	if err != nil {
		return nil, nil, err
	}
	recordsByStep := make(map[string][]evidencepkg.EvidenceRecord)
	committedNotFoundSkips := make(map[string]int)
	for index, event := range events {
		if sourceRunID != "" && event.RunID != sourceRunID {
			continue
		}
		if event.Kind != tracepkg.EventKindStepSkipped {
			continue
		}
		var payload struct {
			StepID          string                               `json:"step_id"`
			QualifiedNodeID string                               `json:"qualified_node_id"`
			Invocation      int                                  `json:"invocation"`
			StructuralPath  []schema.DynamicIncludeFrameIdentity `json:"structural_path"`
			Reason          string                               `json:"reason"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, nil, fmt.Errorf("replay: decode committed step skip: %w", err)
		}
		if payload.Reason != "include_not_found" {
			continue
		}
		if payload.StepID == "" || payload.QualifiedNodeID == "" || payload.Invocation < 1 ||
			!validDynamicNotFoundOccurrence(payload.StepID, payload.QualifiedNodeID, payload.StructuralPath) {
			return nil, nil, errors.New("replay: committed dynamic include skip has invalid occurrence identity")
		}
		key := dynamicNotFoundOccurrenceKey(
			event.RunID, payload.StepID, payload.QualifiedNodeID, payload.Invocation, payload.StructuralPath,
		)
		if _, found := committedNotFoundSkips[key]; found {
			return nil, nil, errors.New("replay: committed dynamic include skip occurrence is duplicated")
		}
		committedNotFoundSkips[key] = index
	}
	consumedNotFoundSkips := make(map[string]bool)

	for _, ev := range events {
		if sourceRunID != "" && ev.RunID != sourceRunID {
			continue
		}
		if ev.Kind == tracepkg.EventKind("handoff/requested") {
			var payload exactTraceResultPayload
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				return nil, nil, fmt.Errorf("replay: decode expected handoff: %w", err)
			}
			selector, ok := exactSelectorFromTrace(payload)
			site, found := stepSites.lookup(selector)
			if !ok || !found || site.Kind != "handoff" || payload.TargetRunbook == "" || payload.ReasonCode == "" {
				return nil, nil, errors.New("replay: expected handoff has invalid occurrence identity")
			}
			scenario.ExpectedTransitions = append(scenario.ExpectedTransitions, sharedreplay.ExpectedTransition{
				At: selector, TargetRunbook: payload.TargetRunbook, ReasonCode: payload.ReasonCode,
			})
			continue
		}
		if ev.Kind == tracepkg.EventKindIncludeNotFound {
			var payload struct {
				StepID          string                               `json:"step_id"`
				QualifiedNodeID string                               `json:"qualified_node_id"`
				Invocation      int                                  `json:"invocation"`
				StructuralPath  []schema.DynamicIncludeFrameIdentity `json:"structural_path"`
				RenderedRef     string                               `json:"rendered_ref"`
				Reason          string                               `json:"reason"`
				ErrorCode       string                               `json:"error_code"`
				Continued       bool                                 `json:"continued"`
			}
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				return nil, nil, fmt.Errorf("replay: decode dynamic include not-found event: %w", err)
			}
			if !payload.Continued || payload.ErrorCode != "DINC-002" {
				continue
			}
			if !dynamicSites[payload.QualifiedNodeID] || payload.QualifiedNodeID == "" || payload.Invocation < 1 ||
				payload.RenderedRef == "" || payload.Reason == "" ||
				!validDynamicNotFoundOccurrence(payload.StepID, payload.QualifiedNodeID, payload.StructuralPath) {
				return nil, nil, errors.New("replay: dynamic include not-found event has invalid occurrence identity")
			}
			key := dynamicNotFoundOccurrenceKey(
				ev.RunID, payload.StepID, payload.QualifiedNodeID, payload.Invocation, payload.StructuralPath,
			)
			_, committed := committedNotFoundSkips[key]
			if !committed {
				continue
			}
			if consumedNotFoundSkips[key] {
				return nil, nil, errors.New("replay: dynamic include not-found occurrence is duplicated")
			}
			consumedNotFoundSkips[key] = true
			scenario.DynamicIncludeNotFound = append(scenario.DynamicIncludeNotFound, DynamicIncludeNotFoundFixture{
				StepID: payload.StepID, QualifiedNodeID: payload.QualifiedNodeID, Invocation: payload.Invocation,
				StructuralPath: append([]schema.DynamicIncludeFrameIdentity(nil), payload.StructuralPath...),
				RenderedRef:    payload.RenderedRef, Reason: payload.Reason,
			})
			continue
		}
		if ev.Kind == tracepkg.EventKindEventReceived {
			var payload exactTraceResultPayload
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				return nil, nil, fmt.Errorf("replay: decode saved event result: %w", err)
			}
			selector, ok := exactSelectorFromTrace(payload)
			site, found := stepSites.lookup(selector)
			wait, waitOK := site.Spec.(*schema.WaitForEventSpec)
			if !ok || !found || !waitOK || wait == nil || ev.RunID == "" {
				return nil, nil, errors.New("replay: saved event result has invalid occurrence identity")
			}
			scenario.WaitEvents = append(scenario.WaitEvents, WaitEventBinding{
				At: selector, EventID: wait.Event.ID, Source: string(wait.Event.Source), Payload: cloneReplayMap(payload.EventPayload),
				Provenance: sharedreplay.Source{Kind: "prior-run", RunID: ev.RunID, InteractionID: ev.EventID},
			})
			continue
		}
		if ev.Kind == tracepkg.EventKindGovernanceApprovalReceived {
			var payload exactTraceResultPayload
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				return nil, nil, fmt.Errorf("replay: decode saved approval: %w", err)
			}
			selector, ok := exactSelectorFromTrace(payload)
			if _, found := stepSites.lookup(selector); !ok || !found || ev.RunID == "" || payload.Approver == "" {
				return nil, nil, errors.New("replay: saved approval has invalid occurrence identity")
			}
			scenario.Approvals = append(scenario.Approvals, sharedreplay.ApprovalBinding{
				At: selector, Approved: true, Approver: payload.Approver,
				Source: sharedreplay.Source{Kind: "prior-run", RunID: ev.RunID, InteractionID: payload.Token},
			})
			continue
		}
		if ev.Kind != tracepkg.EventKindStepCompleted && ev.Kind != tracepkg.EventKindStepFailed {
			continue
		}
		var payload exactTraceResultPayload
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		if selector, exact := exactSelectorFromTrace(payload); exact && ev.RunID != "" {
			site, found := stepSites.lookup(selector)
			if !found || payload.Kind != "" && payload.Kind != site.Kind {
				return nil, nil, errors.New("replay: saved step result has invalid occurrence identity")
			}
			kind := site.Kind
			if isReplayBoundaryKind(kind) {
				status := payload.Status
				if status == "" {
					if ev.Kind == tracepkg.EventKindStepCompleted {
						status = string(engine.StepStatusCompleted)
					} else {
						status = string(engine.StepStatusFailed)
					}
				}
				source := sharedreplay.Source{Kind: "prior-run", RunID: ev.RunID, InteractionID: ev.EventID}
				switch kind {
				case "choice", "decision", "collector":
					binding, err := interactionBindingFromTrace(selector, kind, payload.Output, source)
					if err != nil {
						return nil, nil, err
					}
					scenario.InteractionAnswers = append(scenario.InteractionAnswers, binding)
				case "host_action":
					binding, err := hostActionBindingFromTrace(selector, payload.Output, source)
					if err != nil {
						return nil, nil, err
					}
					scenario.HostActionResponses = append(scenario.HostActionResponses, binding)
				default:
					scenario.StepResponses = append(scenario.StepResponses, sharedreplay.StepBinding{
						At: selector, Kind: kind, Status: status, Outcome: payload.Outcome,
						Output: cloneReplayMap(payload.Output), Vars: cloneReplayMap(payload.Captures), Source: source,
					})
				}
			}
		}
		step, ok := stepByID[payload.StepID]
		if !ok || ev.Kind != tracepkg.EventKindStepCompleted {
			continue
		}

		if len(payload.Evidence) > 0 {
			recordsByStep[payload.StepID] = payload.Evidence
			stepEvidence := make(map[string]EvidenceFixture)
			for _, record := range payload.Evidence {
				fixture := EvidenceFixture{Kind: string(record.Kind)}
				switch record.Kind {
				case evidencepkg.EvidenceKindText:
					fixture.Value = record.Value
				case evidencepkg.EvidenceKindChecklist:
					fixture.Items = record.Items
				}
				stepEvidence[record.Name] = fixture
			}
			if len(stepEvidence) > 0 {
				scenario.Evidence[payload.StepID] = stepEvidence
			}
		}

		switch step.Kind {
		case "cli":
			fixture := CommandFixture{
				Argv:     extractArgv(step),
				Stdout:   asString(payload.Output["stdout"]),
				Stderr:   asString(payload.Output["stderr"]),
				ExitCode: asInt(payload.Output["exit_code"]),
			}
			scenario.Commands = append(scenario.Commands, fixture)
		case "tool":
			name := extractToolName(step)
			if name == "" {
				continue
			}
			scenario.Tools[name] = ToolFixture{
				Response: asString(payload.Output["response"]),
				ExitCode: asInt(payload.Output["exit_code"]),
			}
		}
	}

	return scenario, recordsByStep, nil
}

func interactionBindingFromTrace(
	selector sharedreplay.Selector,
	kind string,
	output map[string]any,
	source sharedreplay.Source,
) (sharedreplay.InteractionBinding, error) {
	interaction, _ := output["interaction"].(map[string]any)
	answer, _ := interaction["answer"].(map[string]any)
	binding := sharedreplay.InteractionBinding{At: selector, Kind: kind, Source: source}
	switch kind {
	case "choice":
		selected, ok := stringSliceValue(answer["selected"])
		if !ok {
			return binding, errors.New("replay: saved choice answer is invalid")
		}
		binding.Selected = selected
	case "decision":
		binding.Label, _ = answer["label"].(string)
		if binding.Label == "" {
			return binding, errors.New("replay: saved decision answer is invalid")
		}
	case "collector":
		if answer == nil {
			return binding, errors.New("replay: saved collector answer is invalid")
		}
		binding.Values = cloneReplayMap(answer)
	}
	return binding, nil
}

func hostActionBindingFromTrace(
	selector sharedreplay.Selector,
	output map[string]any,
	source sharedreplay.Source,
) (sharedreplay.HostActionBinding, error) {
	status, _ := output["status"].(string)
	capability, _ := output["capability"].(string)
	result, _ := output["result"].(map[string]any)
	if status == "" || capability == "" {
		return sharedreplay.HostActionBinding{}, errors.New("replay: saved host-action result is invalid")
	}
	return sharedreplay.HostActionBinding{
		At: selector, Capability: capability, Source: source,
		Response: sharedreplay.HostActionResponse{Status: status, Result: cloneReplayMap(result)},
	}, nil
}

func stringSliceValue(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...), true
	case []any:
		result := make([]string, len(typed))
		for index, item := range typed {
			var ok bool
			result[index], ok = item.(string)
			if !ok {
				return nil, false
			}
		}
		return result, true
	default:
		return nil, false
	}
}

type exactTraceResultPayload struct {
	StepID          string                               `json:"step_id"`
	Kind            string                               `json:"kind"`
	QualifiedNodeID string                               `json:"qualified_node_id"`
	CallPath        []engine.DebugCallFrame              `json:"call_path"`
	StructuralPath  []schema.DynamicIncludeFrameIdentity `json:"structural_path"`
	Invocation      int                                  `json:"invocation"`
	RetryAttempt    int                                  `json:"retry_attempt"`
	Status          string                               `json:"status"`
	Outcome         string                               `json:"outcome"`
	Output          map[string]any                       `json:"output"`
	EventPayload    map[string]any                       `json:"payload"`
	Captures        map[string]any                       `json:"captures"`
	Evidence        []evidencepkg.EvidenceRecord         `json:"evidence"`
	Approver        string                               `json:"approver"`
	Token           string                               `json:"token"`
	TargetRunbook   string                               `json:"target_runbook"`
	ReasonCode      string                               `json:"reason_code"`
}

func exactSelectorFromTrace(payload exactTraceResultPayload) (sharedreplay.Selector, bool) {
	if payload.StepID == "" || payload.QualifiedNodeID == "" || payload.Invocation < 1 || payload.RetryAttempt < 1 {
		return sharedreplay.Selector{}, false
	}
	callPath := make([]string, len(payload.CallPath))
	for index, frame := range payload.CallPath {
		if frame.StepID == "" {
			return sharedreplay.Selector{}, false
		}
		callPath[index] = frame.StepID
	}
	selector := sharedreplay.Selector{
		QualifiedNodeID: payload.QualifiedNodeID, CallPath: callPath,
		StructuralPath: append([]schema.DynamicIncludeFrameIdentity(nil), payload.StructuralPath...),
		Step:           payload.StepID, Phase: "execute", Invocation: payload.Invocation, Attempt: payload.RetryAttempt,
	}
	return selector, validateReplaySelector(selector) == nil
}

func isReplayBoundaryKind(kind string) bool {
	switch kind {
	case "cli", "tool", "choice", "decision", "collector", "manual", "prompt", "host_action", "approve":
		return true
	default:
		return false
	}
}

type replayStepSiteIndex struct {
	entries map[string]engine.ResolvedStep
}

func replayStepSites(plan *engine.ExecutionPlan) (*replayStepSiteIndex, error) {
	sites := &replayStepSiteIndex{entries: make(map[string]engine.ResolvedStep)}
	for _, step := range plan.Steps {
		if step.ParentID == "" {
			if err := indexReplayStepSite(sites, step.ID, nil, 0, step); err != nil {
				return nil, err
			}
		}
	}
	for _, pin := range plan.Metadata.DynamicIncludes {
		if len(pin.ExecutableClosure) == 0 {
			continue
		}
		flow, err := plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
		if err != nil {
			return nil, errors.New("replay: pinned dynamic include closure is invalid")
		}
		root := pin.QualifiedNodeID
		if root == "" {
			root = pin.StepID
		}
		path := append(append([]schema.DynamicIncludeFrameIdentity(nil), pin.StructuralPath...), schema.DynamicIncludeFrameIdentity{
			QualifiedNodeID: root, Kind: "include", Invocation: pin.Invocation,
		})
		if err := indexReplayFlowSites(sites, root, path, len(path), flow); err != nil {
			return nil, err
		}
	}
	return sites, nil
}

func indexReplayStepSite(
	sites *replayStepSiteIndex,
	qualifiedNodeID string,
	structuralPath []schema.DynamicIncludeFrameIdentity,
	exactPrefix int,
	step engine.ResolvedStep,
) error {
	if qualifiedNodeID == "" || step.ID == "" || step.Kind == "" {
		return errors.New("replay: immutable step site is invalid")
	}
	key := replayStepSiteKey(qualifiedNodeID, structuralPath, exactPrefix)
	if _, found := sites.entries[key]; found {
		return errors.New("replay: immutable qualified step site is ambiguous")
	}
	sites.entries[key] = step
	switch typed := step.Spec.(type) {
	case *schema.IncludeSpec:
		if typed != nil {
			path := appendReplayStructuralIdentity(structuralPath, qualifiedNodeID, "include", "")
			return indexReplayFlowSites(sites, qualifiedNodeID, path, exactPrefix, typed.ResolvedSteps)
		}
	case *schema.BranchSpec:
		if typed != nil {
			for _, branch := range typed.Branches {
				path := appendReplayStructuralIdentity(structuralPath, qualifiedNodeID, "branch", branch.Label)
				if err := indexReplayFlowSites(sites, qualifiedNodeID, path, exactPrefix, branch.Steps); err != nil {
					return err
				}
			}
		}
	case *schema.IterateNode:
		if typed != nil {
			path := appendReplayStructuralIdentity(structuralPath, qualifiedNodeID, "iterate", "")
			return indexReplayFlowSites(sites, qualifiedNodeID, path, exactPrefix, typed.Steps)
		}
	case *schema.ParallelNode:
		if typed != nil {
			for _, branch := range typed.Branches {
				path := appendReplayStructuralIdentity(structuralPath, qualifiedNodeID, "parallel", branch.Label)
				if err := indexReplayFlowSites(sites, qualifiedNodeID, path, exactPrefix, branch.Steps); err != nil {
					return err
				}
			}
		}
	case *schema.CompensateSpec:
		if typed != nil {
			path := appendReplayStructuralIdentity(structuralPath, qualifiedNodeID, "compensate", "")
			return indexReplayFlowSites(sites, qualifiedNodeID, path, exactPrefix, typed.Compensate.Steps)
		}
	}
	return nil
}

func indexReplayFlowSites(
	sites *replayStepSiteIndex,
	parent string,
	structuralPath []schema.DynamicIncludeFrameIdentity,
	exactPrefix int,
	nodes []schema.FlowNode,
) error {
	for _, node := range nodes {
		var step engine.ResolvedStep
		switch {
		case node.Step != nil:
			step = engine.ResolvedStep{ID: node.Step.ID, Kind: string(node.Step.Type), Spec: replayStepSpec(node.Step)}
		case node.Iterate != nil:
			step = engine.ResolvedStep{ID: node.Iterate.ID, Kind: "iterate", Spec: node.Iterate}
		case node.Parallel != nil:
			step = engine.ResolvedStep{ID: node.Parallel.ID, Kind: "parallel", Spec: node.Parallel}
		default:
			continue
		}
		if err := indexReplayStepSite(sites, parent+"/"+step.ID, structuralPath, exactPrefix, step); err != nil {
			return err
		}
	}
	return nil
}

func appendReplayStructuralIdentity(
	path []schema.DynamicIncludeFrameIdentity,
	qualifiedNodeID string,
	kind string,
	branchLabel string,
) []schema.DynamicIncludeFrameIdentity {
	return append(append([]schema.DynamicIncludeFrameIdentity(nil), path...), schema.DynamicIncludeFrameIdentity{
		QualifiedNodeID: qualifiedNodeID, Kind: kind, BranchLabel: branchLabel,
	})
}

func replayStepSiteKey(
	qualifiedNodeID string,
	path []schema.DynamicIncludeFrameIdentity,
	exactPrefix int,
) string {
	normalized := append([]schema.DynamicIncludeFrameIdentity(nil), path...)
	if exactPrefix < 0 {
		exactPrefix = 0
	}
	if exactPrefix > len(normalized) {
		exactPrefix = len(normalized)
	}
	for index := exactPrefix; index < len(normalized); index++ {
		normalized[index].Invocation = 0
		normalized[index].IterationIndex = 0
	}
	encoded, _ := json.Marshal(struct {
		QualifiedNodeID string                               `json:"qualified_node_id"`
		StructuralPath  []schema.DynamicIncludeFrameIdentity `json:"structural_path,omitempty"`
		ExactPrefix     int                                  `json:"exact_prefix"`
	}{qualifiedNodeID, normalized, exactPrefix})
	return string(encoded)
}

func (sites *replayStepSiteIndex) lookup(selector sharedreplay.Selector) (engine.ResolvedStep, bool) {
	if sites == nil {
		return engine.ResolvedStep{}, false
	}
	for exactPrefix := len(selector.StructuralPath); exactPrefix >= 0; exactPrefix-- {
		if step, found := sites.entries[replayStepSiteKey(
			selector.QualifiedNodeID, selector.StructuralPath, exactPrefix,
		)]; found {
			return step, true
		}
	}
	return engine.ResolvedStep{}, false
}

func replayDynamicIncludeSites(plan *engine.ExecutionPlan) (map[string]bool, error) {
	sites := make(map[string]bool)
	for _, step := range plan.Steps {
		if step.ParentID == "" {
			indexReplayDynamicStep(sites, step.ID, step.Spec)
		}
	}
	for _, pin := range plan.Metadata.DynamicIncludes {
		if len(pin.ExecutableClosure) == 0 {
			continue
		}
		flow, err := plansnapshot.RestoreFlowClosure(pin.ExecutableClosure)
		if err != nil {
			return nil, errors.New("replay: pinned dynamic include closure is invalid")
		}
		root := pin.QualifiedNodeID
		if root == "" {
			root = pin.StepID
		}
		indexReplayDynamicFlow(sites, root, flow)
	}
	return sites, nil
}

func indexReplayDynamicStep(sites map[string]bool, qualifiedNodeID string, spec engine.StepSpec) {
	switch typed := spec.(type) {
	case *schema.IncludeSpec:
		if typed == nil {
			return
		}
		if typed.Include.IsDynamic() {
			sites[qualifiedNodeID] = true
		}
		indexReplayDynamicFlow(sites, qualifiedNodeID, typed.ResolvedSteps)
	case *schema.BranchSpec:
		if typed != nil {
			for _, branch := range typed.Branches {
				indexReplayDynamicFlow(sites, qualifiedNodeID, branch.Steps)
			}
		}
	case *schema.IterateNode:
		if typed != nil {
			indexReplayDynamicFlow(sites, qualifiedNodeID, typed.Steps)
		}
	case *schema.ParallelNode:
		if typed != nil {
			for _, branch := range typed.Branches {
				indexReplayDynamicFlow(sites, qualifiedNodeID, branch.Steps)
			}
		}
	case *schema.CompensateSpec:
		if typed != nil {
			indexReplayDynamicFlow(sites, qualifiedNodeID, typed.Compensate.Steps)
		}
	}
}

func indexReplayDynamicFlow(sites map[string]bool, parent string, nodes []schema.FlowNode) {
	for _, node := range nodes {
		switch {
		case node.Step != nil:
			qualified := parent + "/" + node.Step.ID
			indexReplayDynamicStep(sites, qualified, replayStepSpec(node.Step))
		case node.Iterate != nil:
			qualified := parent + "/" + node.Iterate.ID
			indexReplayDynamicStep(sites, qualified, node.Iterate)
		case node.Parallel != nil:
			qualified := parent + "/" + node.Parallel.ID
			indexReplayDynamicStep(sites, qualified, node.Parallel)
		}
	}
}

func replayStepSpec(step *schema.Step) engine.StepSpec {
	if step == nil {
		return nil
	}
	switch step.Type {
	case schema.StepTypeCLI:
		return step.CLI
	case schema.StepTypeTool:
		return step.ToolCall
	case schema.StepTypeInclude:
		return step.IncludeSpec
	case schema.StepTypeChoice:
		return step.ChoiceSpec
	case schema.StepTypeDecision:
		return step.DecisionSpec
	case schema.StepTypeCollector:
		return step.CollectorSpec
	case schema.StepTypeHostAction:
		return step.HostActionSpec
	case schema.StepTypeHandoff:
		return step.HandoffSpec
	case schema.StepTypeBranch:
		return step.BranchSpec
	case schema.StepTypeParallel:
		return step.ParallelSpec
	case schema.StepTypeApprove:
		return step.ApproveSpec
	case schema.StepTypeAssert:
		return step.AssertSpec
	case schema.StepTypeCompensate:
		return step.CompensateSpec
	case schema.StepTypeWaitForEvent:
		return step.WaitForEventSpec
	case schema.StepTypeEnd:
		return step.EndSpec
	case schema.StepTypeNoop:
		return step.NoopSpec
	case schema.StepTypeDisplay:
		return step.DisplaySpec
	default:
		return nil
	}
}

func dynamicNotFoundOccurrenceKey(
	runID string,
	stepID string,
	qualifiedNodeID string,
	invocation int,
	structuralPath []schema.DynamicIncludeFrameIdentity,
) string {
	encoded, _ := json.Marshal(struct {
		RunID           string                               `json:"run_id,omitempty"`
		StepID          string                               `json:"step_id"`
		QualifiedNodeID string                               `json:"qualified_node_id"`
		Invocation      int                                  `json:"invocation"`
		StructuralPath  []schema.DynamicIncludeFrameIdentity `json:"structural_path,omitempty"`
	}{runID, stepID, qualifiedNodeID, invocation, structuralPath})
	return string(encoded)
}

func validDynamicNotFoundOccurrence(
	stepID string,
	qualifiedNodeID string,
	structuralPath []schema.DynamicIncludeFrameIdentity,
) bool {
	expected := stepID
	for index, identity := range structuralPath {
		if identity.QualifiedNodeID == "" || identity.Kind == "" {
			return false
		}
		if index > 0 {
			parent := structuralPath[index-1].QualifiedNodeID + "/"
			if len(identity.QualifiedNodeID) <= len(parent) || identity.QualifiedNodeID[:len(parent)] != parent {
				return false
			}
		}
		expected = identity.QualifiedNodeID + "/" + stepID
	}
	return qualifiedNodeID == expected
}

func evidenceRecordsFromScenario(s *Scenario) map[string][]evidencepkg.EvidenceRecord {
	if s == nil || len(s.Evidence) == 0 {
		return nil
	}
	now := time.Now()
	recordsByStep := make(map[string][]evidencepkg.EvidenceRecord)
	for stepID, entries := range s.Evidence {
		var records []evidencepkg.EvidenceRecord
		for name, fixture := range entries {
			record := evidencepkg.EvidenceRecord{
				Name:       name,
				Kind:       evidencepkg.EvidenceKind(fixture.Kind),
				Value:      fixture.Value,
				Items:      fixture.Items,
				CapturedAt: now,
			}
			records = append(records, record)
		}
		recordsByStep[stepID] = records
	}
	return recordsByStep
}

type replayEvidenceHook struct {
	records  map[string][]evidencepkg.EvidenceRecord
	fallback engine.EvidenceHook
}

func newReplayEvidenceHook(records map[string][]evidencepkg.EvidenceRecord, fallback engine.EvidenceHook) engine.EvidenceHook {
	if records == nil {
		return fallback
	}
	return &replayEvidenceHook{records: records, fallback: fallback}
}

func (h *replayEvidenceHook) Collect(ctx context.Context, step engine.ResolvedStep, result *engine.StepResult, runDir string) ([]evidencepkg.EvidenceRecord, error) {
	if h == nil {
		return nil, nil
	}
	if records, ok := h.records[step.ID]; ok {
		return records, nil
	}
	if h.fallback != nil {
		return h.fallback.Collect(ctx, step, result, runDir)
	}
	return nil, nil
}

func asString(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	default:
		return ""
	}
}

func asInt(raw any) int {
	switch v := raw.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}
