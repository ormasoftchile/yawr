package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	"github.com/ormasoftchile/yawr/runtime/pkg/capture"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	gcpparser "github.com/ormasoftchile/yawr/runtime/pkg/gcp/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgsubst"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// ToolExecutor invokes tool actions. Plain process-backed actions are
// dispatched straight to the runtime's Invoke; an action whose
// execute.kind is "runbook" is instead planned via pkg/pkgsubst and its
// substitute runbook is run in-process through SubStepRunner (see
// executeSubstitution) -- substitution requires runner/parser to be
// configured via NewToolExecutorWithSubstitution.
type ToolExecutor struct {
	runtime      tool.ToolRuntime
	evaluator    expr.Evaluator
	runner       SubStepRunner
	parser       parserpkg.Parser
	approvalGate governance.ApprovalGate
}

// NewToolExecutor constructs a ToolExecutor with no substitution support:
// an execute.kind: runbook action encountered by this executor fails with
// a clear step error. Existing callers/tests that never exercise
// substitution are unaffected.
func NewToolExecutor(runtime tool.ToolRuntime, eval expr.Evaluator) *ToolExecutor {
	return &ToolExecutor{runtime: runtime, evaluator: eval}
}

// NewToolExecutorWithSubstitution constructs a ToolExecutor that can also
// run execute.kind: runbook (substituted) actions: runner executes the
// substitute's flow nodes through the same nested-engine mechanism used by
// include/branch/iterate, parser parses the substitute runbook file
// (pkgsubst.Plan's structural/semantic validation path), and approvalGate
// gates execution when the composed governance requires approval.
func NewToolExecutorWithSubstitution(runtime tool.ToolRuntime, eval expr.Evaluator, runner SubStepRunner, parser parserpkg.Parser, approvalGate governance.ApprovalGate) *ToolExecutor {
	return &ToolExecutor{runtime: runtime, evaluator: eval, runner: runner, parser: parser, approvalGate: approvalGate}
}

// IsPureRunbookSubstitution reports whether this exact resolved tool action
// executes a frozen runbook in-process rather than crossing a tool transport.
func (e *ToolExecutor) IsPureRunbookSubstitution(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
) (bool, error) {
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || spec == nil {
		return false, fmt.Errorf("tool executor: invalid spec for step %s", step.ID)
	}
	toolName, err := resolveTemplate(e.evaluator, spec.Tool.Name, vars)
	if err != nil {
		return false, err
	}
	action := spec.Tool.Action
	if action == "" {
		action = "run"
	}
	action, err = resolveTemplate(e.evaluator, action, vars)
	if err != nil {
		return false, err
	}
	if frozenSubstitutionDefinition(ctx, toolName, action) != nil {
		return true, nil
	}
	lookup, ok := e.runtime.(tool.ToolDefLookup)
	if !ok {
		return false, nil
	}
	definition, found := lookup.LookupDef(toolName)
	if !found || definition == nil || definition.Actions[action] == nil {
		return false, nil
	}
	return definition.Actions[action].Execute.IsSubstitution(), nil
}

func frozenSubstitutionDefinition(ctx context.Context, toolName, actionName string) *schema.ToolDef {
	definition := engine.PlanToolsFromContext(ctx)[toolName]
	if definition != nil {
		action := definition.Actions[actionName]
		if action != nil && action.Execute.IsSubstitution() && action.FrozenSubstitution != nil {
			return definition
		}
	}
	return nil
}

func (e *ToolExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("tool executor: invalid spec for step %s", step.ID)
	}

	toolName, err := resolveTemplate(e.evaluator, spec.Tool.Name, vars)
	if err != nil {
		return nil, err
	}
	action := spec.Tool.Action
	if action == "" {
		action = "run"
	}
	if action, err = resolveTemplate(e.evaluator, action, vars); err != nil {
		return nil, err
	}
	engine.RecordToolPresentation(ctx, toolName, action, engine.PlanToolsFromContext(ctx)[toolName])
	if definition := frozenSubstitutionDefinition(ctx, toolName, action); definition != nil {
		return e.ExecuteFrozenSubstitution(ctx, step, vars, definition)
	}
	if e.runtime == nil {
		return nil, fmt.Errorf("tool executor: runtime not configured")
	}

	args := make(map[string]any)
	for k, v := range spec.Tool.Args {
		if s, ok := v.(string); ok {
			resolved, err := resolveTemplate(e.evaluator, s, vars)
			if err != nil {
				return nil, err
			}
			args[k] = resolved
			continue
		}
		args[k] = v
	}

	// resolvedActionDef is retained from the ToolDefLookup scan for
	// non-substituted invocations so the post-invoke output contract
	// enforcement step can access the action's declared outputs: block.
	// Nil means either the runtime does not implement ToolDefLookup (plain
	// test stubs that only implement Invoke) or the tool/action was not
	// found in the registry; both cases skip enforcement.
	var resolvedActionDef *tool.ToolAction
	dispatchClassification := "unspecified"
	dispatchEndpoint := "tool-runtime"

	// A substituted (execute.kind: runbook) action is dispatched through
	// the nested-runbook path instead of runtime.Invoke. Detection requires
	// the runtime to expose tool.ToolDefLookup (test fakes that only
	// implement Invoke fall through to the plain process path unchanged).
	if lookup, ok := e.runtime.(tool.ToolDefLookup); ok {
		if def, found := lookup.LookupDef(toolName); found && def != nil {
			if parsed, parseErr := url.Parse(def.URL); parseErr == nil && parsed.Hostname() != "" {
				dispatchEndpoint = "mcp-http:" + parsed.Hostname()
			} else if def.Transport != "" {
				dispatchEndpoint = "tool:" + string(def.Transport)
			}
			if actionDef, ok := def.Actions[action]; ok && actionDef != nil {
				if retained := actionDef.SchemaAction(); retained != nil {
					applyActionDefaults(retained, args)
					if actionDef.Execute.IsSubstitution() {
						if err := validateSubstitutionArgs(retained, args); err != nil {
							failed := newResult(step, engine.StepStatusFailed)
							failed.Error = err
							return failed, nil
						}
					}
				}
				if schemaAction := actionDef.SchemaAction(); schemaAction != nil &&
					schemaAction.Classification != nil && *schemaAction.Classification != "" {
					dispatchClassification = *schemaAction.Classification
				}
				// ENUM-008: reject a materialized (post-GIS-interpolation)
				// tool arg value that is not a declared enum member,
				// before dispatch/invocation. Type-before-enum ordering
				// (AR-ENUM-7): a non-string bound value is left to
				// transport type handling rather than being misreported
				// as an enum failure. Substitutions validate declared
				// native types above, before reaching this check.
				if aerr := CheckArgEnums(actionDef, args); aerr != nil {
					failed := newResult(step, engine.StepStatusFailed)
					failed.Error = aerr
					return failed, nil
				}
				if actionDef.Execute.IsSubstitution() {
					return e.executeSubstitution(ctx, step, vars, def, action, actionDef, args)
				}
				// Non-substituted path: retain the action def so the
				// post-invoke step can enforce the outputs: contract.
				resolvedActionDef = actionDef
			}
		}
	}

	if capturesWholeOutputs(step) && (resolvedActionDef == nil || len(resolvedActionDef.Outputs) == 0) {
		failed := newResult(step, engine.StepStatusFailed)
		failed.Error = fmt.Errorf("outputs: a declared semantic output contract is required before dispatch")
		return failed, nil
	}
	dispatch, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
		Classification: dispatchClassification, EndpointIdentity: dispatchEndpoint,
		RenderedRequest: map[string]any{"tool": toolName, "action": action, "args": args},
	})
	if err != nil {
		return nil, err
	}
	ctx = engine.WithPreparedDispatch(ctx, dispatch)
	engine.RecordRouteTestExternalDispatch(ctx)
	res, err := e.runtime.Invoke(ctx, toolName, action, args)
	if err != nil {
		failed := newResult(step, engine.StepStatusFailed)
		failed.Error = err
		if res != nil {
			failed.Output["stdout"] = res.Stdout
			failed.Output["stderr"] = res.Stderr
			failed.Output["exit_code"] = res.ExitCode
			for k, v := range res.Output {
				failed.Output[k] = v
			}
		}
		// Still populate captures with empty/zero values so downstream steps
		// that have continue_on_fail can reference these keys without template errors.
		for name, source := range step.Capture {
			switch source {
			case "stdout", "stderr":
				failed.Vars[name] = ""
			case "exitCode", "exit_code":
				failed.Vars[name] = -1
			default:
				failed.Vars[name] = ""
			}
		}
		return failed, nil
	}

	result := newResult(step, engine.StepStatusCompleted)
	result.Output["stdout"] = res.Stdout
	result.Output["stderr"] = res.Stderr
	result.Output["exit_code"] = res.ExitCode
	for k, v := range res.Output {
		result.Output[k] = v
	}

	validated, validationErr := e.validateCompletedResult(step, vars, toolName, action, resolvedActionDef, result)
	if validationErr == nil && validated != nil && validated.Status == engine.StepStatusCompleted && resolvedActionDef != nil {
		engine.ApproveToolPresentationOutputs(ctx)
	}
	return validated, validationErr
}

// ValidateSavedResult applies the normal post-invoke output contract and
// capture path to a completed result supplied by a zero-dispatch runtime.
func (e *ToolExecutor) ValidateSavedResult(ctx context.Context, step engine.ResolvedStep, vars map[string]any, result *engine.StepResult) (*engine.StepResult, error) {
	if result == nil {
		return nil, fmt.Errorf("tool executor: saved result is nil")
	}
	if result.Status != engine.StepStatusCompleted {
		return result, nil
	}
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("tool executor: invalid spec for step %s", step.ID)
	}
	toolName, err := resolveTemplate(e.evaluator, spec.Tool.Name, vars)
	if err != nil {
		return nil, err
	}
	action := spec.Tool.Action
	if action == "" {
		action = "run"
	}
	if action, err = resolveTemplate(e.evaluator, action, vars); err != nil {
		return nil, err
	}
	var actionDef *tool.ToolAction
	if lookup, ok := e.runtime.(tool.ToolDefLookup); ok {
		if def, found := lookup.LookupDef(toolName); found && def != nil {
			actionDef = def.Actions[action]
		}
	}
	engine.RecordToolPresentation(ctx, toolName, action, engine.PlanToolsFromContext(ctx)[toolName])
	validated, err := e.validateCompletedResult(step, vars, toolName, action, actionDef, result)
	if err == nil && validated != nil && validated.Status == engine.StepStatusCompleted && actionDef != nil {
		engine.ApproveToolPresentationOutputs(ctx)
	}
	return validated, err
}

func (e *ToolExecutor) ValidateFrozenSavedResult(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
	result *engine.StepResult,
	definition *schema.ToolDef,
) (*engine.StepResult, error) {
	if result == nil {
		return nil, errors.New("tool executor: saved result is nil")
	}
	if result.Status != engine.StepStatusCompleted {
		return result, nil
	}
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || spec == nil || definition == nil {
		return nil, fmt.Errorf("tool executor: invalid frozen tool result for step %s", step.ID)
	}
	toolName, err := resolveTemplate(e.evaluator, spec.Tool.Name, vars)
	if err != nil {
		return nil, err
	}
	if toolName != definition.Name {
		return nil, fmt.Errorf("tool executor: frozen tool definition mismatch for %q", toolName)
	}
	actionName := spec.Tool.Action
	if actionName == "" {
		actionName = "run"
	}
	actionName, err = resolveTemplate(e.evaluator, actionName, vars)
	if err != nil {
		return nil, err
	}
	action := definition.Actions[actionName]
	if action == nil {
		return nil, fmt.Errorf("tool executor: frozen action %s#%s is unavailable", toolName, actionName)
	}
	engine.RecordToolPresentation(ctx, toolName, actionName, definition)
	validated, validationErr := e.validateCompletedResult(step, vars, toolName, actionName, &tool.ToolAction{
		Outputs: schemaActionOutputs(action), OutputContract: action.OutputContract,
	}, result)
	if validationErr == nil && validated != nil && validated.Status == engine.StepStatusCompleted {
		engine.ApproveToolPresentationOutputs(ctx)
	}
	return validated, validationErr
}

func schemaActionOutputs(action *schema.ToolAction) map[string]*schema.ArgDef {
	if action == nil {
		return nil
	}
	if action.Result == nil {
		return action.Outputs
	}
	outputs := make(map[string]*schema.ArgDef, len(action.Outputs)+5)
	for name, definition := range action.Outputs {
		outputs[name] = definition
	}
	defaults := map[string]*schema.ArgDef{
		"success": {Type: "boolean"}, "row_count": {Type: "integer"},
		"columns": {Type: "array"}, "rows": {Type: "array"},
	}
	if action.Result.Metadata != "" {
		defaults["metadata"] = &schema.ArgDef{Type: "object", Optional: true}
	}
	for name, definition := range defaults {
		if _, exists := outputs[name]; !exists {
			outputs[name] = definition
		}
	}
	return outputs
}

func (e *ToolExecutor) validateCompletedResult(step engine.ResolvedStep, vars map[string]any, toolName, action string, resolvedActionDef *tool.ToolAction, result *engine.StepResult) (*engine.StepResult, error) {
	if capturesWholeOutputs(step) && result.Status == engine.StepStatusCompleted {
		var declarations map[string]*schema.ArgDef
		if resolvedActionDef != nil {
			declarations = resolvedActionDef.Outputs
		}
		public, err := projectPublicOutputs(declarations, result.Output)
		if err != nil {
			failed := newResult(step, engine.StepStatusFailed)
			failed.Error = err
			return failed, nil
		}
		result.PublicOutputs = public
	}
	if resolvedActionDef != nil {
		ignoreAdditional := resolvedActionDef.OutputContract != nil &&
			resolvedActionDef.OutputContract.AdditionalOutputs == schema.AdditionalOutputsIgnore
		validated, cerr := enforceOutputContractPolicy(toolName, action, resolvedActionDef.Outputs, result.Output, ignoreAdditional, capturesWholeOutputs(step))
		if cerr != nil {
			failed := newResult(step, engine.StepStatusFailed)
			failed.Error = cerr
			return failed, nil
		}
		result.Output = validated
	}

	if len(step.Capture) > 0 {
		captures, err := capture.New(vars, nil, nil).CaptureStep(nil, step, result)
		if err != nil {
			return nil, fmt.Errorf("tool executor: %w", err)
		}
		for name, val := range capture.ToAnyMap(captures) {
			result.Vars[name] = val
		}
	}

	return result, nil
}

// executeSubstitution runs a single execute.kind: runbook action: it plans
// the substitution via pkg/pkgsubst.Plan (cycle/depth checks, input/output
// contract validation, governance composition), enforces the composed
// policy's require_approval gate, runs the substitute's flow through
// SubStepRunner, evaluates the substitute's outputs: block against the
// merged child scope, and records the full effective governance on a
// tool/substituted trace event.
func (e *ToolExecutor) executeSubstitution(ctx context.Context, step engine.ResolvedStep, vars map[string]any, def *tool.ToolDef, actionName string, actionDef *tool.ToolAction, args map[string]any) (*engine.StepResult, error) {
	if e.runner == nil {
		return nil, fmt.Errorf("tool executor: step %s invokes action %q (execute.kind: runbook) but no SubStepRunner is configured for substitution", step.ID, actionName)
	}
	if e.parser == nil {
		return nil, fmt.Errorf("tool executor: step %s invokes action %q (execute.kind: runbook) but no substitution parser is configured", step.ID, actionName)
	}
	schemaAction := actionDef.SchemaAction()
	if schemaAction == nil {
		return nil, fmt.Errorf("tool executor: step %s: action %q has no retained schema definition; cannot plan substitution", step.ID, actionName)
	}

	st, hasState := substStateFromContext(ctx)
	frames := st.frames
	callerGov := st.governance
	if !hasState || callerGov == nil {
		// Top-level (non-nested) substitution call: the caller-effective
		// policy is the declaring tool's own governance block.
		callerGov = pkgsubst.EffectiveGovernanceFromTool(def.Governance)
	}

	planResult, perrs := pkgsubst.Plan(schemaAction, actionName, def.PackageName, def.Name, pkgsubst.PlanOptions{
		ToolFilePath:     def.SourcePath,
		PackageRoot:      def.PackageRoot,
		Frames:           frames,
		CallerGovernance: callerGov,
		Parser:           e.parser,
	})
	if len(perrs) > 0 {
		failed := newResult(step, engine.StepStatusFailed)
		failed.Error = perrs[0]
		msgs := make([]string, 0, len(perrs))
		for _, pe := range perrs {
			msgs = append(msgs, pe.Error())
		}
		failed.Output["substitution_errors"] = msgs
		return failed, nil
	}
	if planResult == nil {
		return nil, fmt.Errorf("tool executor: step %s: pkgsubst.Plan reported no substitution for action %q despite execute.kind: runbook", step.ID, actionName)
	}
	return e.executePlannedSubstitution(ctx, step, vars, def.Name, actionName, args, frames, planResult, schemaAction)
}

func (e *ToolExecutor) ExecuteFrozenSubstitution(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
	definition *schema.ToolDef,
) (*engine.StepResult, error) {
	if e.runner == nil {
		return nil, errors.New("tool executor: frozen substitution requires a SubStepRunner")
	}
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || spec == nil || definition == nil {
		return nil, fmt.Errorf("tool executor: invalid frozen substitution for step %s", step.ID)
	}
	toolName, err := resolveTemplate(e.evaluator, spec.Tool.Name, vars)
	if err != nil {
		return nil, err
	}
	if toolName != definition.Name {
		return nil, fmt.Errorf("tool executor: frozen tool definition mismatch for %q", toolName)
	}
	actionName := spec.Tool.Action
	if actionName == "" {
		actionName = "run"
	}
	actionName, err = resolveTemplate(e.evaluator, actionName, vars)
	if err != nil {
		return nil, err
	}
	action := definition.Actions[actionName]
	if action == nil || action.Execute == nil || !action.Execute.IsSubstitution() || action.FrozenSubstitution == nil {
		return nil, fmt.Errorf("tool executor: action %s#%s has no frozen substitution", toolName, actionName)
	}
	engine.RecordToolPresentation(ctx, toolName, actionName, definition)
	args := make(map[string]any, len(spec.Tool.Args))
	for name, value := range spec.Tool.Args {
		if text, isText := value.(string); isText {
			resolved, resolveErr := resolveTemplate(e.evaluator, text, vars)
			if resolveErr != nil {
				return nil, resolveErr
			}
			args[name] = resolved
			continue
		}
		args[name] = value
	}
	applyActionDefaults(action, args)
	if err := validateSubstitutionArgs(action, args); err != nil {
		failed := newResult(step, engine.StepStatusFailed)
		failed.Error = err
		return failed, nil
	}
	if err := CheckSchemaArgEnums(action, args); err != nil {
		failed := newResult(step, engine.StepStatusFailed)
		failed.Error = err
		return failed, nil
	}
	flow, err := plansnapshot.RestoreFlowClosure(action.FrozenSubstitution.ExecutableClosure)
	if err != nil {
		return nil, fmt.Errorf("tool executor: restore frozen substitution: %w", err)
	}
	substitute := &schema.Runbook{
		ID: action.FrozenSubstitution.RunbookID, Name: action.FrozenSubstitution.RunbookName,
		Flow: flow, Inputs: action.FrozenSubstitution.Inputs, Outputs: action.FrozenSubstitution.Outputs,
		Bindings:   action.FrozenSubstitution.Bindings,
		Governance: action.FrozenSubstitution.Governance,
	}
	state, hasState := substStateFromContext(ctx)
	frames := state.frames
	callerGovernance := state.governance
	if !hasState || callerGovernance == nil {
		callerGovernance = pkgsubst.EffectiveGovernanceFromTool(definition.Governance)
	}
	planned, planningErrors := pkgsubst.PlanFrozen(
		action, actionName, action.FrozenSubstitution.PackageName, definition.Name,
		action.FrozenSubstitution.RunbookPath, substitute,
		pkgsubst.PlanOptions{Frames: frames, CallerGovernance: callerGovernance},
	)
	if len(planningErrors) > 0 {
		failed := newResult(step, engine.StepStatusFailed)
		failed.Error = planningErrors[0]
		messages := make([]string, 0, len(planningErrors))
		for _, planningErr := range planningErrors {
			messages = append(messages, planningErr.Error())
		}
		failed.Output["substitution_errors"] = messages
		return failed, nil
	}
	if planned == nil {
		return nil, fmt.Errorf("tool executor: frozen action %s#%s is not a substitution", toolName, actionName)
	}
	return e.executePlannedSubstitution(ctx, step, vars, definition.Name, actionName, args, frames, planned, action)
}

func (e *ToolExecutor) executePlannedSubstitution(
	ctx context.Context,
	step engine.ResolvedStep,
	vars map[string]any,
	toolName string,
	actionName string,
	args map[string]any,
	frames []pkgsubst.Frame,
	planResult *pkgsubst.Result,
	actionDefinition *schema.ToolAction,
) (*engine.StepResult, error) {
	for name, input := range planResult.SubstituteRunbook.Inputs {
		if input != nil && input.Required {
			if value, present := args[name]; !present || value == nil {
				failed := newResult(step, engine.StepStatusFailed)
				failed.Error = fmt.Errorf("tool executor: required substitute input %q is missing", name)
				return failed, nil
			}
		}
	}

	// Enforce the composed policy's require_approval gate before running
	// the substitute body -- real, gate-backed enforcement, not merely a
	// value computed and recorded.
	if planResult.EffectiveGovernance.RequireApproval {
		gate := approvalGateFromContext(ctx)
		if gate == nil {
			gate = e.approvalGate
		}
		if gate == nil {
			failed := newResult(step, engine.StepStatusFailed)
			failed.Error = fmt.Errorf("tool executor: step %s: substituted action %s#%s requires approval but no ApprovalGate is configured", step.ID, toolName, actionName)
			return failed, nil
		}
		reason := fmt.Sprintf("substituted action %s#%s requires approval", toolName, actionName)
		if _, aerr := gate.RequestApproval(ctx, step.ID, reason); aerr != nil {
			denied := newResult(step, engine.StepStatusDenied)
			denied.Error = aerr
			return denied, nil
		}
	}

	// pkgsubst.validateInputs (invoked inside Plan) already guarantees the
	// substitute's inputs: keyset is exactly the action's args: keyset, so
	// the child scope is just the action's effective args, bound 1:1.
	childVars := make(map[string]any, len(args))
	for k, v := range args {
		childVars[k] = v
	}

	childFrames := make([]pkgsubst.Frame, 0, len(frames)+1)
	childFrames = append(childFrames, frames...)
	childFrames = append(childFrames, planResult.Frame)
	nestedCtx := withSubstState(ctx, substState{frames: childFrames, governance: planResult.EffectiveGovernance})

	invocationState := &RunbookInvocationState{}
	results, err := e.runner(nestedCtx, SubStepParent{
		RunbookInvocation: schema.InvocationForRunbook(planResult.SubstituteRunbook.Bindings, planResult.SubstituteRunbook.Outputs, planResult.SubstituteRunbook.Flow),
		InvocationState:   invocationState,
		ID:                step.ID,
		RunbookPath:       planResult.SubstitutePath,
		Kind:              "tool-substitution",
		NestDepth:         step.NestDepth + 1,
	}, planResult.SubstituteRunbook.Flow, childVars)
	if err != nil && !errors.Is(err, ErrSubRunFailed) {
		return nil, err
	}
	childError := err

	mergedVars := make(map[string]any, len(childVars))
	for k, v := range childVars {
		mergedVars[k] = v
	}
	for name, value := range invocationState.Bindings {
		mergedVars[name] = value
	}
	childStepResults := make(map[string]*engine.StepResult, len(results))
	failed := childError != nil
	for _, res := range results {
		if res == nil {
			continue
		}
		for k, v := range res.Vars {
			mergedVars[k] = v
		}
		if res.StepID != "" {
			childStepResults[res.StepID] = res
		}
		if res.Status == engine.StepStatusFailed {
			failed = true
			if childError == nil {
				childError = res.Error
			}
		}
	}

	status := engine.StepStatusCompleted
	if failed {
		status = engine.StepStatusFailed
	}
	result := newResult(step, status)
	result.Error = childError

	// Evaluate the substitute's outputs: block against the child flow's
	// results. An output's value is a YAWR Capture Path (e.g.
	// "step.<id>.json.<field>", per the ratified corpus fixture
	// convention) resolved via pkg/capture against the substitute's own
	// child step results -- this is the "stable replay boundary" style
	// resolution used everywhere else GCP paths are consumed (step
	// Capture blocks), not the plain ${...} templating mechanism (which
	// has no "step" root and cannot see raw per-step stdout/json). A
	// Values parseable as GCP paths resolve against the child run. Other
	// values use the template evaluator, which provides the canonical literal
	// and interpolation semantics for output declarations. The rendered value
	// is then coerced to the declared type via coerceOutputAny. The result
	// lands in result.Output, which is what the ratified outputs.<name>
	// GCP capture root (pkg/gcp/parser, pkg/capture) resolves against for
	// this step.
	capSvc := capture.New(mergedVars, childStepResults, nil)
	var published *engine.RunResults
	for _, child := range results {
		if child != nil && child.Results != nil {
			published = child.Results
		}
	}
	for _, name := range sortedOutputNames(planResult.SubstituteRunbook.Outputs) {
		if published != nil {
			if output, ok := published.Outputs[name]; ok {
				result.Output[name] = output.Value
			}
			continue
		}
		outDecl := planResult.SubstituteRunbook.Outputs[name]
		if outDecl == nil {
			continue
		}
		if outDecl.Value == "" && outDecl.ValueExpr == "" && outDecl.Optional {
			continue
		}
		var value any
		var everr error
		if outDecl.ValueExpr != "" {
			value, everr = resolveValueExpression(e.evaluator, outDecl.ValueExpr, mergedVars)
		} else if _, perr := gcpparser.Parse(outDecl.Value); perr == nil {
			value, everr = capSvc.ResolveSource(outDecl.Value, nil)
		} else {
			value, everr = resolveTemplate(e.evaluator, outDecl.Value, mergedVars)
		}
		if everr != nil {
			if outDecl.Optional {
				continue
			}
			result.Status = engine.StepStatusFailed
			result.Error = errors.Join(result.Error, fmt.Errorf("tool executor: step %s: substitute output %q: %w", step.ID, name, everr))
			continue
		}
		if capturesWholeOutputs(step) {
			if err := ValidateStrictValue(value, outDecl.Type, outDecl.Enum); err != nil {
				result.Status = engine.StepStatusFailed
				result.Error = errors.Join(result.Error, fmt.Errorf("outputs.%s: %w", name, err))
				continue
			}
		}
		coerced, cerr := coerceOutputAny(value, outDecl.Type)
		if cerr != nil {
			result.Status = engine.StepStatusFailed
			result.Error = errors.Join(result.Error, fmt.Errorf("tool executor: step %s: substitute output %q: %w", step.ID, name, cerr))
			continue
		}
		// Legacy substitutions without Results materialize outputs here.
		// Published child Results already passed strict type/enum validation.
		if len(outDecl.Enum) > 0 {
			if s, ok := coerced.(string); ok && !outDecl.Enum.Contains(s) {
				result.Status = engine.StepStatusFailed
				result.Error = errors.Join(result.Error, errkit.New("ENUM-009", fmt.Sprintf("step %s: substitute output %q value is not a declared enum member", step.ID, name)))
				continue
			}
		}
		result.Output[name] = coerced
	}

	if emit := EmitterFromContext(ctx); emit != nil {
		emit(string(trace.EventKindToolSubstituted), map[string]any{
			"tool":   toolName,
			"action": actionName,
			"depth":  len(childFrames),
			"effectiveGovernance": map[string]any{
				"require_approval": planResult.EffectiveGovernance.RequireApproval,
				"deny_commands":    planResult.EffectiveGovernance.DenyCommands,
				"deny_env_vars":    planResult.EffectiveGovernance.DenyEnvVars,
				"allow_commands":   planResult.EffectiveGovernance.AllowCommands,
			},
		})
	}

	// Valid declared outputs remain observable on failure, without inventing
	// values for absent/invalid outputs or replacing the original failure.
	if result.Status == engine.StepStatusFailed {
		for name, source := range step.Capture {
			one := step
			one.Capture = map[string]string{name: source}
			captures, captureErr := capture.New(vars, nil, nil).CaptureStep(nil, one, result)
			if captureErr == nil {
				for key, value := range capture.ToAnyMap(captures) {
					result.Vars[key] = value
				}
			}
		}
	} else {
		if capturesWholeOutputs(step) || published != nil {
			public, err := projectPublicOutputs(actionDefinition.Outputs, result.Output)
			if err != nil {
				result.Status, result.Outcome, result.Error = engine.StepStatusFailed, engine.StepOutcomeFailed, err
				return result, nil
			}
			result.PublicOutputs = public
		}
		captures, cerr := capture.New(vars, nil, nil).CaptureStep(nil, step, result)
		if cerr != nil {
			return nil, fmt.Errorf("tool executor: %w", cerr)
		}
		for name, val := range capture.ToAnyMap(captures) {
			result.Vars[name] = val
		}
	}

	engine.ApproveToolPresentationOutputs(ctx)
	return result, nil
}

func sortedOutputNames(outputs map[string]*schema.Output) []string {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CheckArgEnums returns an ENUM-008 error for the first materialized
// (post-GIS-interpolation) tool arg value that is bound to an
// enum-constrained action arg but is not one of its declared members. Only
// Exported (barbara-enum-mvp-implementation-gate.md R5) so
// internal/replay's ReplayExecutor can perform the identical check at the
// same moment the real ToolExecutor does, rather than bypassing it.
func CheckArgEnums(actionDef *tool.ToolAction, args map[string]any) error {
	for name, argDef := range actionDef.Args {
		if argDef == nil || len(argDef.Enum) == 0 {
			continue
		}
		v, ok := args[name]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return errkit.New("ENUM-008", fmt.Sprintf("tool arg %q must be a string for enum validation", name))
		}
		if !argDef.Enum.Contains(s) {
			return errkit.New("ENUM-008", fmt.Sprintf("tool arg %q value is not a declared enum member", name))
		}
	}
	return nil
}

func CheckSchemaArgEnums(actionDef *schema.ToolAction, args map[string]any) error {
	if actionDef == nil {
		return nil
	}
	for name, argDef := range actionDef.Args {
		if argDef == nil || len(argDef.Enum) == 0 {
			continue
		}
		value, found := args[name]
		if !found {
			continue
		}
		text, isText := value.(string)
		if !isText {
			return errkit.New("ENUM-008", fmt.Sprintf("tool arg %q must be a string for enum validation", name))
		}
		if !argDef.Enum.Contains(text) {
			return errkit.New("ENUM-008", fmt.Sprintf("tool arg %q value is not a declared enum member", name))
		}
	}
	return nil
}

// processLevelOutputs are the three channels the executor injects from the
// ToolResult wire protocol unconditionally (stdout, stderr, and exit_code).
// They are NOT part of the declared outputs: contract: every existing runbook
// captures them by name, so they always flow through the enforcement step.
// The rule: outputs: constrains only tool-defined semantic output keys; the
// process-level envelope is never flagged as undeclared or missing.
var processLevelOutputs = map[string]bool{
	"stdout":    true,
	"stderr":    true,
	"exit_code": true,
}

// enforceOutputContract validates and coerces the semantic outputs of a
// non-substituted tool invocation against the action's declared outputs: block.
// It uses the same coerceOutputAny semantics as the substituted path so both
// paths agree on type coercion.
//
// stdout, stderr, and exit_code are process-level channels that always pass
// through regardless of the outputs: declaration (see processLevelOutputs).
//
// An empty or nil declaredOutputs means the action is unconstrained: actual
// is returned unchanged. This covers all existing tools and actions that have
// no outputs: block on their non-substituted actions.
//
// On success, returns the validated output map with declared keys coerced to
// their declared types and process channels copied through unchanged.
// On failure, returns a descriptive error naming the tool, action, and every
// offending field (missing, undeclared, or type-incompatible).
//
// This strict entry point preserves the historical behavior: undeclared
// semantic keys REJECT. It delegates to enforceOutputContractPolicy with the
// additional-outputs drop policy disabled.
func enforceOutputContract(toolName, actionName string, declaredOutputs map[string]*schema.ArgDef, actual map[string]any) (map[string]any, error) {
	return enforceOutputContractPolicy(toolName, actionName, declaredOutputs, actual, false)
}

// enforceOutputContractPolicy is enforceOutputContract with the OPT-IN
// additional_outputs policy applied. When ignoreAdditionalOutputs is true
// (action declared output_contract.additional_outputs: ignore), undeclared
// raw semantic output keys are deliberately DROPPED: they neither raise a
// contract violation nor appear in the validated output, so they cannot be
// captured, templated, persisted, or passed downstream. This is a drop
// policy for provider-owned additive payloads, NOT an allow/pass-through and
// NOT an excuse for declaring fewer fields — every declared output is still
// fully enforced (missing required, type mismatch, broken projection all
// still fail), and process channels are unchanged.
func enforceOutputContractPolicy(toolName, actionName string, declaredOutputs map[string]*schema.ArgDef, actual map[string]any, ignoreAdditionalOutputs bool, preserveNull ...bool) (map[string]any, error) {
	if len(declaredOutputs) == 0 {
		return actual, nil
	}

	var errs []string

	declaredRawKeys := make(map[string]bool, len(declaredOutputs))
	for name, outDecl := range declaredOutputs {
		if outDecl == nil || outDecl.From == "" {
			declaredRawKeys[name] = true
			continue
		}
		declaredRawKeys[projectionRoot(outDecl.From)] = true
	}

	// Undeclared outputs: keys present in actual that are not process-level
	// channels and are not in the declared contract or used as projection roots.
	for k := range actual {
		if processLevelOutputs[k] {
			continue
		}
		if !declaredRawKeys[k] {
			if ignoreAdditionalOutputs {
				// Drop policy: the undeclared key is silently omitted from
				// the validated output below (validated only ever copies
				// process channels and declared outputs), so no action is
				// needed here beyond NOT flagging it as a violation.
				continue
			}
			errs = append(errs, fmt.Sprintf("undeclared output %q", k))
		}
	}

	// Validate and type-coerce each declared output. Process-level channels
	// are copied through unconditionally; they are never part of the contract.
	validated := make(map[string]any, len(actual))
	for k, v := range actual {
		if processLevelOutputs[k] {
			validated[k] = v
		}
	}
	for name, outDecl := range declaredOutputs {
		if outDecl == nil {
			continue
		}
		source := name
		if outDecl.From != "" {
			source = outDecl.From
		}
		v, ok := projectOutputValue(actual, source, preserveNull...)
		if !ok {
			if outDecl.Optional {
				continue
			}
			errs = append(errs, fmt.Sprintf("missing declared output %q", name))
			continue
		}
		coerced, cerr := coerceOutputAny(v, outDecl.Type)
		if cerr != nil {
			errs = append(errs, fmt.Sprintf("output %q: %v", name, cerr))
			continue
		}
		validated[name] = coerced
	}

	if len(errs) > 0 {
		sort.Strings(errs) // deterministic order for stable error messages
		return nil, fmt.Errorf("tool %s#%s output contract violation: %s",
			toolName, actionName, strings.Join(errs, "; "))
	}
	return validated, nil
}

// coerceOutputAny validates native structural values and applies only the
// scalar conversions allowed at this boundary: string forms to integer,
// number, or boolean; integral finite JSON numbers to integer; and exact
// integer values to finite numbers. It never stringifies objects or arrays.
func projectionRoot(path string) string {
	parts := splitProjectionPath(path)
	if len(parts) == 0 {
		return path
	}
	root := parts[0]
	if i := strings.Index(root, "["); i >= 0 {
		return root[:i]
	}
	return root
}

func splitProjectionPath(path string) []string {
	var parts []string
	start := 0
	depth := 0
	for i, r := range path {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '.':
			if depth == 0 {
				parts = append(parts, path[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, path[start:])
	return parts
}

func projectOutputValue(actual map[string]any, path string, preserveNull ...bool) (any, bool) {
	if path == "" {
		return nil, false
	}
	var cur any = actual
	for _, part := range splitProjectionPath(path) {
		field, selectorKey, selectorValues, ok := parseProjectionSegment(part)
		if !ok || field == "" {
			return nil, false
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[field]
		if !ok || cur == nil && (len(preserveNull) == 0 || !preserveNull[0]) {
			return nil, false
		}
		if selectorKey != "" {
			cur, ok = selectProjectedArrayElement(cur, selectorKey, selectorValues)
			if !ok {
				return nil, false
			}
		}
	}
	return cur, true
}

func parseProjectionSegment(segment string) (field, selectorKey string, selectorValues []string, ok bool) {
	open := strings.Index(segment, "[")
	if open < 0 {
		return segment, "", nil, true
	}
	if !strings.HasSuffix(segment, "]") || open == 0 {
		return "", "", nil, false
	}
	field = segment[:open]
	selector := segment[open+1 : len(segment)-1]
	eq := strings.Index(selector, "=")
	if eq <= 0 || eq == len(selector)-1 {
		return "", "", nil, false
	}
	selectorKey = selector[:eq]
	for _, value := range strings.Split(selector[eq+1:], "|") {
		if value == "" {
			return "", "", nil, false
		}
		selectorValues = append(selectorValues, value)
	}
	return field, selectorKey, selectorValues, true
}

func selectProjectedArrayElement(value any, key string, values []string) (any, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	for _, wanted := range values {
		for _, item := range items {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := obj[key].(string); ok && s == wanted {
				return obj, true
			}
		}
	}
	return nil, false
}

func coerceOutputAny(value any, declaredType string) (any, error) {
	switch declaredType {
	case "any":
		return value, nil
	case "object":
		if obj, ok := value.(map[string]any); ok {
			return obj, nil
		}
		return nil, outputTypeError(declaredType, value)
	case "array":
		switch arr := value.(type) {
		case []any:
			return arr, nil
		case []string:
			return arr, nil
		}
		return nil, outputTypeError(declaredType, value)
	case "", "string":
		if s, ok := value.(string); ok {
			return s, nil
		}
		return nil, outputTypeError(declaredType, value)
	case "int", "integer":
		return coerceOutputInteger(value, declaredType)
	case "bool", "boolean":
		if b, ok := value.(bool); ok {
			return b, nil
		}
		if rendered, ok := value.(string); ok {
			return coerceOutputValue(rendered, declaredType)
		}
		return nil, outputTypeError(declaredType, value)
	case "number", "float":
		return coerceOutputNumber(value, declaredType)
	default:
		return nil, fmt.Errorf("output declared unsupported type %q", declaredType)
	}
}

func coerceOutputInteger(value any, declaredType string) (any, error) {
	switch typed := value.(type) {
	case int:
		return typed, nil
	case int8:
		return int(typed), nil
	case int16:
		return int(typed), nil
	case int32:
		return int(typed), nil
	case int64:
		if int64(int(typed)) != typed {
			return nil, outputRangeError(declaredType, value)
		}
		return int(typed), nil
	case uint:
		if uint64(typed) > uint64(^uint(0)>>1) {
			return nil, outputRangeError(declaredType, value)
		}
		return int(typed), nil
	case uint8:
		return int(typed), nil
	case uint16:
		return int(typed), nil
	case uint32:
		if uint64(typed) > uint64(^uint(0)>>1) {
			return nil, outputRangeError(declaredType, value)
		}
		return int(typed), nil
	case uint64:
		if typed > uint64(^uint(0)>>1) {
			return nil, outputRangeError(declaredType, value)
		}
		return int(typed), nil
	case float32:
		return coerceFiniteFloatInteger(float64(typed), declaredType, value)
	case float64:
		return coerceFiniteFloatInteger(typed, declaredType, value)
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return coerceOutputInteger(integer, declaredType)
		}
		floating, err := typed.Float64()
		if err != nil {
			return nil, outputTypeError(declaredType, value)
		}
		return coerceFiniteFloatInteger(floating, declaredType, value)
	case string:
		return coerceOutputValue(typed, declaredType)
	default:
		return nil, outputTypeError(declaredType, value)
	}
}

func coerceFiniteFloatInteger(value float64, declaredType string, original any) (any, error) {
	limit := math.Ldexp(1, strconv.IntSize-1)
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return nil, outputTypeError(declaredType, original)
	}
	if value < -limit || value >= limit {
		return nil, outputRangeError(declaredType, original)
	}
	return int(value), nil
}

func coerceOutputNumber(value any, declaredType string) (any, error) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nil, outputTypeError(declaredType, value)
		}
		return typed, nil
	case float32:
		floating := float64(typed)
		if math.IsNaN(floating) || math.IsInf(floating, 0) {
			return nil, outputTypeError(declaredType, value)
		}
		return floating, nil
	case int:
		return exactSignedNumber(int64(typed), declaredType, value)
	case int8:
		return float64(typed), nil
	case int16:
		return float64(typed), nil
	case int32:
		return float64(typed), nil
	case int64:
		return exactSignedNumber(typed, declaredType, value)
	case uint:
		return exactUnsignedNumber(uint64(typed), declaredType, value)
	case uint8:
		return float64(typed), nil
	case uint16:
		return float64(typed), nil
	case uint32:
		return float64(typed), nil
	case uint64:
		return exactUnsignedNumber(typed, declaredType, value)
	case json.Number:
		floating, err := typed.Float64()
		if err != nil || math.IsNaN(floating) || math.IsInf(floating, 0) {
			return nil, outputTypeError(declaredType, value)
		}
		return floating, nil
	case string:
		return coerceOutputValue(typed, declaredType)
	default:
		return nil, outputTypeError(declaredType, value)
	}
}

func exactSignedNumber(value int64, declaredType string, original any) (any, error) {
	floating := float64(value)
	limit := math.Ldexp(1, 63)
	if floating < -limit || floating >= limit {
		return nil, outputRangeError(declaredType, original)
	}
	if int64(floating) != value {
		return nil, outputRangeError(declaredType, original)
	}
	return floating, nil
}

func exactUnsignedNumber(value uint64, declaredType string, original any) (any, error) {
	floating := float64(value)
	if floating >= math.Ldexp(1, 64) {
		return nil, outputRangeError(declaredType, original)
	}
	if uint64(floating) != value {
		return nil, outputRangeError(declaredType, original)
	}
	return floating, nil
}

func outputTypeError(declaredType string, value any) error {
	return fmt.Errorf("output declared type %q but value of type %T is incompatible", declaredType, value)
}

func outputRangeError(declaredType string, value any) error {
	return fmt.Errorf("output declared type %q but value %v is out of range", declaredType, value)
}

// coerceOutputValue converts a rendered (string) output value to the
// declared type. A conversion failure fails the step (via a returned
// error) rather than silently degrading to the raw rendered string --
// declaring outputs.<name>: 42 (type: int) that renders to a non-numeric
// string is a step-time contract violation, not a display nuance; the
// structural substitution contract (names/types present) is validated
// separately at plan time (pkgsubst), but coercion at output-materialization
// time can only be checked once the rendered value is known.
func coerceOutputValue(rendered string, declaredType string) (any, error) {
	switch declaredType {
	case "", "string":
		return rendered, nil
	case "int", "integer":
		n, err := strconv.Atoi(rendered)
		if err != nil {
			return nil, fmt.Errorf("output declared type %q but rendered value %q is not an integer: %w", declaredType, rendered, err)
		}
		return n, nil
	case "bool", "boolean":
		b, err := strconv.ParseBool(rendered)
		if err != nil {
			return nil, fmt.Errorf("output declared type %q but rendered value %q is not a boolean: %w", declaredType, rendered, err)
		}
		return b, nil
	case "number", "float":
		f, err := strconv.ParseFloat(rendered, 64)
		if err != nil {
			return nil, fmt.Errorf("output declared type %q but rendered value %q is not a number: %w", declaredType, rendered, err)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("output declared type %q but rendered value %q is not finite", declaredType, rendered)
		}
		return f, nil
	default:
		return nil, fmt.Errorf("output declared unsupported type %q", declaredType)
	}
}
