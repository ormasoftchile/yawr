package plansnapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

const SchemaVersionV3 = "execution-plan/v3"
const SchemaVersionV4 = "execution-plan/v4"

var ErrUnpinnedInclude = errors.New("plan snapshot: unresolved include cannot be resumed safely")

type SnapshotV1 struct {
	ScopeBoundary    *engine.ToolScopeBoundary      `json:"scope_boundary,omitempty"`
	ToolScopes       *toolscope.Set                 `json:"tool_scopes,omitempty"`
	RootScopeID      string                         `json:"root_scope_id,omitempty"`
	SchemaVersion    string                         `json:"schema_version"`
	SnapshotDigest   string                         `json:"snapshot_digest"`
	RunID            string                         `json:"run_id,omitempty"`
	RunbookPath      string                         `json:"runbook_path"`
	Steps            []ResolvedStepSnapshotV1       `json:"steps"`
	Tools            map[string]*schema.ToolDef     `json:"tools,omitempty"`
	Providers        map[string]*schema.ProviderDef `json:"providers,omitempty"`
	GovernanceSource *schema.GovernanceConfig       `json:"governance,omitempty"`
	Metadata         engine.PlanMetadata            `json:"metadata"`
	Inputs           map[string]*schema.Input       `json:"inputs,omitempty"`
	Outputs          map[string]*schema.Output      `json:"outputs,omitempty"`
	Bindings         []schema.Binding               `json:"bindings,omitempty"`
}

func (snapshot *SnapshotV1) UnmarshalJSON(data []byte) error {
	if err := Preflight(data); err != nil {
		return err
	}
	type snapshotAlias SnapshotV1
	var decoded snapshotAlias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := expectJSONEOF(decoder); err != nil {
		return err
	}
	*snapshot = SnapshotV1(decoded)
	return nil
}

type ResolvedStepSnapshotV1 struct {
	LexicalScopeID   string                       `json:"lexical_scope_id,omitempty"`
	ToolBindingID    string                       `json:"tool_binding_id,omitempty"`
	ID               string                       `json:"id"`
	Name             string                       `json:"name"`
	Subtitle         string                       `json:"subtitle,omitempty"`
	Kind             string                       `json:"kind"`
	Spec             json.RawMessage              `json:"spec"`
	Capture          map[string]string            `json:"capture,omitempty"`
	CaptureDefaults  map[string]any               `json:"capture_defaults,omitempty"`
	Depth            int                          `json:"depth,omitempty"`
	NestDepth        int                          `json:"nest_depth,omitempty"`
	DisplayOrder     int                          `json:"display_order,omitempty"`
	Origin           string                       `json:"origin,omitempty"`
	OnError          string                       `json:"on_error,omitempty"`
	Timeout          string                       `json:"timeout,omitempty"`
	Delay            string                       `json:"delay,omitempty"`
	When             string                       `json:"when,omitempty"`
	Retry            *schema.RetryConfig          `json:"retry,omitempty"`
	Scope            string                       `json:"scope,omitempty"`
	Export           []string                     `json:"export,omitempty"`
	Contract         *schema.Contract             `json:"contract,omitempty"`
	RequiredEvidence []schema.EvidenceRequirement `json:"required_evidence,omitempty"`
	ParentID         string                       `json:"parent_id,omitempty"`
	ParentKind       string                       `json:"parent_kind,omitempty"`
	IncludeAlias     string                       `json:"include_alias,omitempty"`
	BranchLabel      string                       `json:"branch_label,omitempty"`
}

type includeSpecSnapshotV1 struct {
	TargetScopeID              string                    `json:"target_scope_id,omitempty"`
	ResolvedBindings           []schema.Binding          `json:"resolved_bindings,omitempty"`
	Include                    schema.IncludeConfig      `json:"include"`
	ResolvedSteps              []flowNodeSnapshotV1      `json:"resolved_steps,omitempty"`
	ResolvedRunbookPath        string                    `json:"resolved_runbook_path,omitempty"`
	ResolvedRunbookID          string                    `json:"resolved_runbook_id,omitempty"`
	ResolvedRunbookName        string                    `json:"resolved_runbook_name,omitempty"`
	ResolvedRunbookContentHash string                    `json:"resolved_runbook_content_hash,omitempty"`
	LazyRunbookPath            string                    `json:"lazy_runbook_path,omitempty"`
	LazyRunbookDigest          string                    `json:"lazy_runbook_digest,omitempty"`
	ResolvedInputs             map[string]*schema.Input  `json:"resolved_inputs,omitempty"`
	ResolvedOutputs            map[string]*schema.Output `json:"resolved_outputs,omitempty"`
	ResolvedGovernance         *schema.GovernanceConfig  `json:"resolved_governance,omitempty"`
}

type flowNodeSnapshotV1 struct {
	Step     *flowStepSnapshotV1 `json:"step,omitempty"`
	Iterate  *iterateSnapshotV1  `json:"iterate,omitempty"`
	Parallel *parallelSnapshotV1 `json:"parallel,omitempty"`
}

type flowStepSnapshotV1 struct {
	Common   schema.Step     `json:"common"`
	SpecKind string          `json:"spec_kind,omitempty"`
	Spec     json.RawMessage `json:"spec,omitempty"`
}

type branchSpecSnapshotV1 struct {
	Branches []branchArmSnapshotV1 `json:"branches"`
}

type branchArmSnapshotV1 struct {
	Condition string               `json:"condition,omitempty"`
	Else      bool                 `json:"else,omitempty"`
	Label     string               `json:"label,omitempty"`
	Steps     []flowNodeSnapshotV1 `json:"steps,omitempty"`
}

type compensateSpecSnapshotV1 struct {
	On    string               `json:"on,omitempty"`
	Steps []flowNodeSnapshotV1 `json:"steps"`
}

type iterateSnapshotV1 struct {
	ID            string               `json:"id"`
	Over          string               `json:"over,omitempty"`
	As            string               `json:"as,omitempty"`
	Max           int                  `json:"max,omitempty"`
	Until         string               `json:"until,omitempty"`
	Collect       map[string]string    `json:"collect,omitempty"`
	CollectValues map[string]any       `json:"collect_values,omitempty"`
	Concurrency   int                  `json:"concurrency,omitempty"`
	Steps         []flowNodeSnapshotV1 `json:"steps"`
}

type parallelSnapshotV1 struct {
	ID       string                     `json:"id"`
	Branches []parallelBranchSnapshotV1 `json:"branches"`
	Join     *schema.ParallelJoin       `json:"join,omitempty"`
}

type parallelBranchSnapshotV1 struct {
	Label string               `json:"label,omitempty"`
	Steps []flowNodeSnapshotV1 `json:"steps"`
}

func FromExecutionPlan(plan *engine.ExecutionPlan) (SnapshotV1, error) {
	if plan == nil {
		return SnapshotV1{}, errors.New("plan snapshot: plan is required")
	}
	if plan.Validation == nil {
		return SnapshotV1{}, errors.New("plan snapshot: refusing to snapshot an unvalidated plan")
	}
	if plan.Metadata.PlanHash == "" {
		return SnapshotV1{}, errors.New("plan snapshot: validated plan hash is required")
	}
	if err := planner.ValidateToolScopes(plan); err != nil {
		return SnapshotV1{}, err
	}
	if err := validateScopedArtifacts(plan); err != nil {
		return SnapshotV1{}, err
	}
	if plan.ToolScopes != nil {
		hash, err := planner.ScopedPlanHash(plan)
		if err != nil {
			return SnapshotV1{}, err
		}
		if hash != plan.Metadata.PlanHash {
			return SnapshotV1{}, errors.New("plan snapshot: scoped plan changed after validation")
		}
	}
	if err := validateFrozenToolSubstitutions(plan.Tools); err != nil {
		return SnapshotV1{}, err
	}

	steps := make([]ResolvedStepSnapshotV1, len(plan.Steps))
	for index, step := range plan.Steps {
		snapshot, err := snapshotStep(step)
		if err != nil {
			return SnapshotV1{}, fmt.Errorf("plan snapshot: step %d: %w", index, err)
		}
		steps[index] = snapshot
	}

	draft := SnapshotV1{
		ScopeBoundary: plan.ScopeBoundary,
		ToolScopes:    plan.ToolScopes, RootScopeID: plan.RootScopeID,
		SchemaVersion:    SchemaVersionV3,
		RunID:            plan.RunID,
		RunbookPath:      plan.RunbookPath,
		Steps:            steps,
		Tools:            plan.Tools,
		Providers:        plan.Providers,
		GovernanceSource: plan.GovernanceSource,
		Metadata:         plan.Metadata,
		Inputs:           plan.Inputs,
		Outputs:          plan.Outputs,
		Bindings:         plan.Bindings,
	}
	if plan.ToolScopes != nil {
		draft.SchemaVersion = SchemaVersionV4
	}
	canonical, err := cloneSnapshot(draft)
	if err != nil {
		return SnapshotV1{}, err
	}
	digest, err := snapshotDigest(canonical)
	if err != nil {
		return SnapshotV1{}, err
	}
	canonical.SnapshotDigest = digest
	return canonical, nil
}

func Restore(snapshot SnapshotV1) (*engine.ExecutionPlan, error) {
	if snapshot.SchemaVersion != SchemaVersionV3 && snapshot.SchemaVersion != SchemaVersionV4 {
		return nil, fmt.Errorf("plan snapshot: unsupported schema version %q", snapshot.SchemaVersion)
	}
	if (snapshot.SchemaVersion == SchemaVersionV4) != (snapshot.ToolScopes != nil && snapshot.RootScopeID != "") ||
		snapshot.SchemaVersion == SchemaVersionV3 && (snapshot.ToolScopes != nil || snapshot.RootScopeID != "") {
		return nil, errors.New("plan snapshot: scope tables require v4")
	}
	if snapshot.SnapshotDigest == "" {
		return nil, errors.New("plan snapshot: snapshot digest is required")
	}
	digest, err := snapshotDigest(snapshot)
	if err != nil {
		return nil, err
	}
	if digest != snapshot.SnapshotDigest {
		return nil, fmt.Errorf("plan snapshot: snapshot digest mismatch: got %q, want %q", digest, snapshot.SnapshotDigest)
	}
	if snapshot.RunbookPath == "" {
		return nil, errors.New("plan snapshot: runbook path is required")
	}
	if snapshot.Metadata.PlanHash == "" {
		return nil, errors.New("plan snapshot: plan hash is required")
	}

	owned, err := cloneSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	for index, pin := range owned.Metadata.DynamicIncludes {
		if err := ValidateDynamicIncludePin(pin, owned.ToolScopes); err != nil {
			return nil, fmt.Errorf("plan snapshot: dynamic include %d: %w", index, err)
		}
	}
	if err := validateFrozenToolSubstitutions(owned.Tools); err != nil {
		return nil, err
	}
	steps := make([]engine.ResolvedStep, len(owned.Steps))
	for index, step := range owned.Steps {
		restored, restoreErr := restoreStep(step)
		if restoreErr != nil {
			return nil, fmt.Errorf("plan snapshot: step %d: %w", index, restoreErr)
		}
		steps[index] = restored
	}

	expectedHash := owned.Metadata.PlanHash
	plan := &engine.ExecutionPlan{
		ScopeBoundary: owned.ScopeBoundary,
		ToolScopes:    owned.ToolScopes, RootScopeID: owned.RootScopeID,
		RunID:            owned.RunID,
		RunbookPath:      owned.RunbookPath,
		Steps:            steps,
		Tools:            owned.Tools,
		Providers:        owned.Providers,
		Governance:       internalgovernance.BuildPolicy(owned.GovernanceSource),
		GovernanceSource: owned.GovernanceSource,
		Metadata:         owned.Metadata,
		Inputs:           owned.Inputs,
		Outputs:          owned.Outputs,
		Bindings:         owned.Bindings,
	}
	if err := validateScopedArtifacts(plan); err != nil {
		return nil, err
	}
	if err := planner.ValidateExecutionPlan(plan); err != nil {
		return nil, fmt.Errorf("plan snapshot: restored plan validation failed: %w", err)
	}
	if plan.Metadata.PlanHash != expectedHash {
		return nil, fmt.Errorf("plan snapshot: plan hash mismatch: got %q, want %q", plan.Metadata.PlanHash, expectedHash)
	}
	return plan, nil
}

func ValidateResumeSafety(plan *engine.ExecutionPlan) error {
	return validateResumeSafety(plan, false)
}

// ValidateResumeSafetyForState permits unresolved dynamic include definitions
// only for writer-epoch checkpoints, where resolution and child progress are
// committed before child execution. Static lazy paths are never resumable:
// durable starts must replace them with a captured executable closure.
func ValidateResumeSafetyForState(plan *engine.ExecutionPlan, state engine.RunState) error {
	return validateResumeSafety(plan, state.WriterEpoch > 0)
}

func validateResumeSafety(plan *engine.ExecutionPlan, allowDeferredIncludes bool) error {
	if plan == nil {
		return errors.New("plan snapshot: plan is required")
	}
	if err := planner.ValidateToolScopes(plan); err != nil {
		return err
	}
	if err := validateScopedArtifacts(plan); err != nil {
		return err
	}
	if err := validateFrozenToolSubstitutions(plan.Tools); err != nil {
		return err
	}
	for _, step := range plan.Steps {
		if err := validateSpecResumeSafety(step.Spec, step.ID, allowDeferredIncludes); err != nil {
			return err
		}
	}
	return nil
}

func validateFrozenToolSubstitutions(tools map[string]*schema.ToolDef) error {
	for toolName, definition := range tools {
		if definition == nil {
			continue
		}
		for actionName, action := range definition.Actions {
			if action == nil || action.Execute == nil || !action.Execute.IsSubstitution() {
				continue
			}
			if err := ValidateFrozenToolSubstitution(action.FrozenSubstitution); err != nil {
				return fmt.Errorf("plan snapshot: tool %s action %s: %w", toolName, actionName, err)
			}
		}
	}
	return nil
}

func validateSpecResumeSafety(spec engine.StepSpec, path string, allowDeferredIncludes bool) error {
	switch typed := spec.(type) {
	case *schema.IncludeSpec:
		if typed == nil {
			return nil
		}
		if typed.Include.IsDynamic() && !allowDeferredIncludes {
			return fmt.Errorf("%w: step %q is dynamic and has no committed resolution revision", ErrUnpinnedInclude, path)
		}
		if !typed.Include.IsDynamic() && typed.Include.Runbook != "" &&
			typed.ResolvedRunbookPath == "" && len(typed.ResolvedSteps) == 0 {
			return fmt.Errorf("%w: step %q has no captured static include closure", ErrUnpinnedInclude, path)
		}
		if typed.LazyRunbookPath != "" {
			return fmt.Errorf("%w: step %q is lazy and has no committed closure revision", ErrUnpinnedInclude, path)
		}
		return validateFlowResumeSafety(typed.ResolvedSteps, path, allowDeferredIncludes)
	case *schema.BranchSpec:
		if typed == nil {
			return nil
		}
		for index, branch := range typed.Branches {
			if err := validateFlowResumeSafety(branch.Steps, fmt.Sprintf("%s/branch:%d", path, index), allowDeferredIncludes); err != nil {
				return err
			}
		}
	case *schema.IterateNode:
		if typed != nil {
			return validateFlowResumeSafety(typed.Steps, path+"/iterate", allowDeferredIncludes)
		}
	case *schema.ParallelNode:
		if typed == nil {
			return nil
		}
		for index, branch := range typed.Branches {
			if err := validateFlowResumeSafety(branch.Steps, fmt.Sprintf("%s/parallel:%d", path, index), allowDeferredIncludes); err != nil {
				return err
			}
		}
	case *schema.CompensateSpec:
		if typed != nil {
			return validateFlowResumeSafety(typed.Compensate.Steps, path+"/compensate", allowDeferredIncludes)
		}
	}
	return nil
}

func validateFlowResumeSafety(nodes []schema.FlowNode, path string, allowDeferredIncludes bool) error {
	for index, node := range nodes {
		nodePath := fmt.Sprintf("%s/node:%d", path, index)
		switch {
		case node.Step != nil:
			var spec engine.StepSpec
			switch node.Step.Type {
			case schema.StepTypeInclude:
				spec = node.Step.IncludeSpec
			case schema.StepTypeBranch:
				spec = node.Step.BranchSpec
			case schema.StepTypeParallel:
				spec = node.Step.ParallelSpec
			case schema.StepTypeCompensate:
				spec = node.Step.CompensateSpec
			default:
				continue
			}
			if spec == nil {
				return fmt.Errorf("plan snapshot: inspect %s: %s spec is required", nodePath, node.Step.Type)
			}
			if err := validateSpecResumeSafety(spec, nodePath+"/"+node.Step.ID, allowDeferredIncludes); err != nil {
				return err
			}
		case node.Iterate != nil:
			if err := validateSpecResumeSafety(node.Iterate, nodePath+"/"+node.Iterate.ID, allowDeferredIncludes); err != nil {
				return err
			}
		case node.Parallel != nil:
			if err := validateSpecResumeSafety(node.Parallel, nodePath+"/"+node.Parallel.ID, allowDeferredIncludes); err != nil {
				return err
			}
		default:
			return fmt.Errorf("plan snapshot: inspect %s: flow node has no definition", nodePath)
		}
	}
	return nil
}

func snapshotStep(step engine.ResolvedStep) (ResolvedStepSnapshotV1, error) {
	if step.ID == "" {
		return ResolvedStepSnapshotV1{}, errors.New("step id is required")
	}
	if step.Kind == "" {
		return ResolvedStepSnapshotV1{}, errors.New("step kind is required")
	}
	if step.Spec == nil {
		return ResolvedStepSnapshotV1{}, errors.New("step spec is required")
	}
	value := reflect.ValueOf(step.Spec)
	if value.Kind() == reflect.Ptr && value.IsNil() {
		return ResolvedStepSnapshotV1{}, errors.New("step spec is required")
	}
	if step.Spec.StepKind() != step.Kind {
		return ResolvedStepSnapshotV1{}, fmt.Errorf("step kind %q does not match spec kind %q", step.Kind, step.Spec.StepKind())
	}
	spec, err := encodeStepSpec(step.Spec)
	if err != nil {
		return ResolvedStepSnapshotV1{}, fmt.Errorf("encode %s spec: %w", step.Kind, err)
	}
	return ResolvedStepSnapshotV1{
		ID:              step.ID,
		Name:            step.Name,
		Subtitle:        step.Subtitle,
		Kind:            step.Kind,
		Spec:            spec,
		Capture:         step.Capture,
		CaptureDefaults: step.CaptureDefaults,
		Depth:           step.Depth,
		NestDepth:       step.NestDepth,
		DisplayOrder:    step.DisplayOrder,
		Origin:          step.Origin,
		OnError:         step.OnError,
		Timeout:         step.Timeout,
		Delay:           step.Delay,
		When:            step.When,
		Retry:           step.Retry,
		Scope:           step.Scope,
		LexicalScopeID:  step.LexicalScopeID, ToolBindingID: step.ToolBindingID,
		Export:           step.Export,
		Contract:         step.Contract,
		RequiredEvidence: step.RequiredEvidence,
		ParentID:         step.ParentID,
		ParentKind:       step.ParentKind,
		IncludeAlias:     step.IncludeAlias,
		BranchLabel:      step.BranchLabel,
	}, nil
}

func restoreStep(snapshot ResolvedStepSnapshotV1) (engine.ResolvedStep, error) {
	if snapshot.ID == "" {
		return engine.ResolvedStep{}, errors.New("step id is required")
	}
	spec, err := decodeStepSpec(snapshot.Kind, snapshot.Spec)
	if err != nil {
		return engine.ResolvedStep{}, err
	}
	return engine.ResolvedStep{
		ID:              snapshot.ID,
		Name:            snapshot.Name,
		Subtitle:        snapshot.Subtitle,
		Kind:            snapshot.Kind,
		Spec:            spec,
		Capture:         snapshot.Capture,
		CaptureDefaults: snapshot.CaptureDefaults,
		Depth:           snapshot.Depth,
		NestDepth:       snapshot.NestDepth,
		DisplayOrder:    snapshot.DisplayOrder,
		Origin:          snapshot.Origin,
		OnError:         snapshot.OnError,
		Timeout:         snapshot.Timeout,
		Delay:           snapshot.Delay,
		When:            snapshot.When,
		Retry:           snapshot.Retry,
		Scope:           snapshot.Scope,
		LexicalScopeID:  snapshot.LexicalScopeID, ToolBindingID: snapshot.ToolBindingID,
		Export:           snapshot.Export,
		Contract:         snapshot.Contract,
		RequiredEvidence: snapshot.RequiredEvidence,
		ParentID:         snapshot.ParentID,
		ParentKind:       snapshot.ParentKind,
		IncludeAlias:     snapshot.IncludeAlias,
		BranchLabel:      snapshot.BranchLabel,
	}, nil
}

func decodeStepSpec(kind string, raw json.RawMessage) (engine.StepSpec, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, fmt.Errorf("%s spec is required", kind)
	}
	if kind == "include" {
		var snapshot includeSpecSnapshotV1
		if err := decodeStrictJSON(raw, &snapshot); err != nil {
			return nil, fmt.Errorf("decode include spec: %w", err)
		}
		resolvedSteps, err := restoreFlowNodes(snapshot.ResolvedSteps)
		if err != nil {
			return nil, fmt.Errorf("decode include resolved steps: %w", err)
		}
		return &schema.IncludeSpec{
			TargetScopeID:              snapshot.TargetScopeID,
			Include:                    snapshot.Include,
			ResolvedSteps:              resolvedSteps,
			ResolvedRunbookPath:        snapshot.ResolvedRunbookPath,
			ResolvedRunbookID:          snapshot.ResolvedRunbookID,
			ResolvedRunbookName:        snapshot.ResolvedRunbookName,
			ResolvedRunbookContentHash: snapshot.ResolvedRunbookContentHash,
			LazyRunbookPath:            snapshot.LazyRunbookPath,
			LazyRunbookDigest:          snapshot.LazyRunbookDigest,
			ResolvedInputs:             snapshot.ResolvedInputs,
			ResolvedBindings:           snapshot.ResolvedBindings,
			ResolvedOutputs:            snapshot.ResolvedOutputs,
			ResolvedGovernance:         snapshot.ResolvedGovernance,
		}, nil
	}
	if kind == "branch" {
		var snapshot branchSpecSnapshotV1
		if err := decodeStrictJSON(raw, &snapshot); err != nil {
			return nil, fmt.Errorf("decode branch spec: %w", err)
		}
		branches := make([]schema.BranchArm, len(snapshot.Branches))
		for index, branch := range snapshot.Branches {
			steps, err := restoreFlowNodes(branch.Steps)
			if err != nil {
				return nil, fmt.Errorf("decode branch %d: %w", index, err)
			}
			branches[index] = schema.BranchArm{Condition: branch.Condition, Else: branch.Else, Label: branch.Label, Steps: steps}
		}
		return &schema.BranchSpec{Branches: branches}, nil
	}
	if kind == "compensate" {
		var snapshot compensateSpecSnapshotV1
		if err := decodeStrictJSON(raw, &snapshot); err != nil {
			return nil, fmt.Errorf("decode compensate spec: %w", err)
		}
		steps, err := restoreFlowNodes(snapshot.Steps)
		if err != nil {
			return nil, fmt.Errorf("decode compensate steps: %w", err)
		}
		return &schema.CompensateSpec{Compensate: schema.CompensateConfig{On: snapshot.On, Steps: steps}}, nil
	}
	if kind == "iterate" {
		var snapshot iterateSnapshotV1
		if err := decodeStrictJSON(raw, &snapshot); err != nil {
			return nil, fmt.Errorf("decode iterate spec: %w", err)
		}
		steps, err := restoreFlowNodes(snapshot.Steps)
		if err != nil {
			return nil, fmt.Errorf("decode iterate steps: %w", err)
		}
		return &schema.IterateNode{
			ID: snapshot.ID, Over: snapshot.Over, As: snapshot.As, Max: snapshot.Max,
			Until: snapshot.Until, Collect: snapshot.Collect, CollectValues: snapshot.CollectValues,
			Concurrency: snapshot.Concurrency, Steps: steps,
		}, nil
	}
	if kind == "parallel" {
		var snapshot parallelSnapshotV1
		if err := decodeStrictJSON(raw, &snapshot); err != nil {
			return nil, fmt.Errorf("decode parallel spec: %w", err)
		}
		branches := make([]schema.ParallelBranch, len(snapshot.Branches))
		for index, branch := range snapshot.Branches {
			steps, err := restoreFlowNodes(branch.Steps)
			if err != nil {
				return nil, fmt.Errorf("decode parallel branch %d: %w", index, err)
			}
			branches[index] = schema.ParallelBranch{Label: branch.Label, Steps: steps}
		}
		return &schema.ParallelNode{ID: snapshot.ID, Branches: branches, Join: snapshot.Join}, nil
	}

	var spec engine.StepSpec
	switch kind {
	case "cli":
		spec = &schema.CLISpec{}
	case "tool":
		spec = &schema.ToolCallSpec{}
	case "choice":
		spec = &schema.ChoiceSpec{}
	case "decision":
		spec = &schema.DecisionSpec{}
	case "collector":
		spec = &schema.CollectorSpec{}
	case "host_action":
		spec = &schema.HostActionSpec{}
	case "handoff":
		spec = &schema.HandoffSpec{}
	case "approve":
		spec = &schema.ApproveSpec{}
	case "assert":
		spec = &schema.AssertSpec{}
	case "wait_for_event":
		spec = &schema.WaitForEventSpec{}
	case "end":
		spec = &schema.EndSpec{}
	case "noop":
		spec = &schema.NoopSpec{}
	case "assign":
		spec = &schema.AssignSpec{}
	case "results":
		spec = &schema.ResultsSpec{}
	case "display":
		spec = &schema.DisplaySpec{}
	default:
		return nil, fmt.Errorf("unsupported step kind %q", kind)
	}
	if err := decodeStrictJSON(raw, spec); err != nil {
		return nil, fmt.Errorf("decode %s spec: %w", kind, err)
	}
	return spec, nil
}

func encodeStepSpec(spec engine.StepSpec) (json.RawMessage, error) {
	if include, ok := spec.(*schema.IncludeSpec); ok {
		resolvedSteps, err := snapshotFlowNodes(include.ResolvedSteps)
		if err != nil {
			return nil, fmt.Errorf("encode include resolved steps: %w", err)
		}
		return json.Marshal(includeSpecSnapshotV1{
			TargetScopeID: include.TargetScopeID,
			Include:       include.Include, ResolvedSteps: resolvedSteps,
			ResolvedRunbookPath:        include.ResolvedRunbookPath,
			ResolvedRunbookID:          include.ResolvedRunbookID,
			ResolvedRunbookName:        include.ResolvedRunbookName,
			ResolvedRunbookContentHash: include.ResolvedRunbookContentHash,
			LazyRunbookPath:            include.LazyRunbookPath, LazyRunbookDigest: include.LazyRunbookDigest,
			ResolvedInputs: include.ResolvedInputs, ResolvedOutputs: include.ResolvedOutputs,
			ResolvedBindings:   include.ResolvedBindings,
			ResolvedGovernance: include.ResolvedGovernance,
		})
	}
	if branch, ok := spec.(*schema.BranchSpec); ok {
		branches := make([]branchArmSnapshotV1, len(branch.Branches))
		for index, arm := range branch.Branches {
			steps, err := snapshotFlowNodes(arm.Steps)
			if err != nil {
				return nil, fmt.Errorf("encode branch %d: %w", index, err)
			}
			branches[index] = branchArmSnapshotV1{Condition: arm.Condition, Else: arm.Else, Label: arm.Label, Steps: steps}
		}
		return json.Marshal(branchSpecSnapshotV1{Branches: branches})
	}
	if compensate, ok := spec.(*schema.CompensateSpec); ok {
		steps, err := snapshotFlowNodes(compensate.Compensate.Steps)
		if err != nil {
			return nil, fmt.Errorf("encode compensate steps: %w", err)
		}
		return json.Marshal(compensateSpecSnapshotV1{On: compensate.Compensate.On, Steps: steps})
	}
	if iterate, ok := spec.(*schema.IterateNode); ok {
		steps, err := snapshotFlowNodes(iterate.Steps)
		if err != nil {
			return nil, fmt.Errorf("encode iterate steps: %w", err)
		}
		return json.Marshal(iterateSnapshotV1{
			ID: iterate.ID, Over: iterate.Over, As: iterate.As, Max: iterate.Max,
			Until: iterate.Until, Collect: iterate.Collect, CollectValues: iterate.CollectValues,
			Concurrency: iterate.Concurrency, Steps: steps,
		})
	}
	if parallel, ok := spec.(*schema.ParallelNode); ok {
		branches := make([]parallelBranchSnapshotV1, len(parallel.Branches))
		for index, branch := range parallel.Branches {
			steps, err := snapshotFlowNodes(branch.Steps)
			if err != nil {
				return nil, fmt.Errorf("encode parallel branch %d: %w", index, err)
			}
			branches[index] = parallelBranchSnapshotV1{Label: branch.Label, Steps: steps}
		}
		return json.Marshal(parallelSnapshotV1{ID: parallel.ID, Branches: branches, Join: parallel.Join})
	}
	switch spec.(type) {
	case *schema.CLISpec,
		*schema.ToolCallSpec,
		*schema.ChoiceSpec,
		*schema.DecisionSpec,
		*schema.CollectorSpec,
		*schema.HostActionSpec,
		*schema.HandoffSpec,
		*schema.ApproveSpec,
		*schema.AssertSpec,
		*schema.WaitForEventSpec,
		*schema.EndSpec,
		*schema.NoopSpec,
		*schema.AssignSpec,
		*schema.ResultsSpec,
		*schema.DisplaySpec:
		return json.Marshal(spec)
	default:
		return nil, fmt.Errorf("unsupported concrete step spec %T", spec)
	}
}

func snapshotFlowNodes(nodes []schema.FlowNode) ([]flowNodeSnapshotV1, error) {
	result := make([]flowNodeSnapshotV1, len(nodes))
	for index, node := range nodes {
		nonNil := 0
		if node.Step != nil {
			nonNil++
			snapshot, err := snapshotFlowStep(node.Step)
			if err != nil {
				return nil, fmt.Errorf("flow node %d: %w", index, err)
			}
			result[index].Step = &snapshot
		}
		if node.Iterate != nil {
			nonNil++
			raw, err := encodeStepSpec(node.Iterate)
			if err != nil {
				return nil, fmt.Errorf("flow node %d: %w", index, err)
			}
			var snapshot iterateSnapshotV1
			if err := decodeStrictJSON(raw, &snapshot); err != nil {
				return nil, fmt.Errorf("flow node %d: %w", index, err)
			}
			result[index].Iterate = &snapshot
		}
		if node.Parallel != nil {
			nonNil++
			raw, err := encodeStepSpec(node.Parallel)
			if err != nil {
				return nil, fmt.Errorf("flow node %d: %w", index, err)
			}
			var snapshot parallelSnapshotV1
			if err := decodeStrictJSON(raw, &snapshot); err != nil {
				return nil, fmt.Errorf("flow node %d: %w", index, err)
			}
			result[index].Parallel = &snapshot
		}
		if nonNil != 1 {
			return nil, fmt.Errorf("flow node %d has %d definitions, want 1", index, nonNil)
		}
	}
	return result, nil
}

func restoreFlowNodes(snapshots []flowNodeSnapshotV1) ([]schema.FlowNode, error) {
	result := make([]schema.FlowNode, len(snapshots))
	for index, snapshot := range snapshots {
		nonNil := 0
		if snapshot.Step != nil {
			nonNil++
			step, err := restoreFlowStep(*snapshot.Step)
			if err != nil {
				return nil, fmt.Errorf("flow node %d: %w", index, err)
			}
			result[index].Step = step
		}
		if snapshot.Iterate != nil {
			nonNil++
			raw, err := json.Marshal(snapshot.Iterate)
			if err != nil {
				return nil, err
			}
			spec, err := decodeStepSpec("iterate", raw)
			if err != nil {
				return nil, fmt.Errorf("flow node %d: %w", index, err)
			}
			result[index].Iterate = spec.(*schema.IterateNode)
		}
		if snapshot.Parallel != nil {
			nonNil++
			raw, err := json.Marshal(snapshot.Parallel)
			if err != nil {
				return nil, err
			}
			spec, err := decodeStepSpec("parallel", raw)
			if err != nil {
				return nil, fmt.Errorf("flow node %d: %w", index, err)
			}
			result[index].Parallel = spec.(*schema.ParallelNode)
		}
		if nonNil != 1 {
			return nil, fmt.Errorf("flow node %d has %d definitions, want 1", index, nonNil)
		}
	}
	return result, nil
}

func snapshotFlowStep(step *schema.Step) (flowStepSnapshotV1, error) {
	if step == nil {
		return flowStepSnapshotV1{}, errors.New("step is required")
	}
	spec, specKind, err := specFromFlowStep(step)
	if err != nil {
		return flowStepSnapshotV1{}, err
	}
	raw, err := encodeStepSpec(spec)
	if err != nil {
		return flowStepSnapshotV1{}, err
	}
	common := *step
	clearFlowStepSpecs(&common)
	return flowStepSnapshotV1{Common: common, SpecKind: specKind, Spec: raw}, nil
}

func restoreFlowStep(snapshot flowStepSnapshotV1) (*schema.Step, error) {
	step := snapshot.Common
	clearFlowStepSpecs(&step)
	spec, err := decodeStepSpec(snapshot.SpecKind, snapshot.Spec)
	if err != nil {
		return nil, err
	}
	if err := assignFlowStepSpec(&step, spec); err != nil {
		return nil, err
	}
	return &step, nil
}

func specFromFlowStep(step *schema.Step) (engine.StepSpec, string, error) {
	switch step.Type {
	case schema.StepTypeCLI:
		return requireFlowSpec(step.CLI, "cli")
	case schema.StepTypeTool:
		return requireFlowSpec(step.ToolCall, "tool")
	case schema.StepTypeInclude:
		return requireFlowSpec(step.IncludeSpec, "include")
	case schema.StepTypeChoice:
		return requireFlowSpec(step.ChoiceSpec, "choice")
	case schema.StepTypeDecision:
		return requireFlowSpec(step.DecisionSpec, "decision")
	case schema.StepTypeCollector:
		return requireFlowSpec(step.CollectorSpec, "collector")
	case schema.StepTypeHostAction:
		return requireFlowSpec(step.HostActionSpec, "host_action")
	case schema.StepTypeHandoff:
		return requireFlowSpec(step.HandoffSpec, "handoff")
	case schema.StepTypeBranch:
		return requireFlowSpec(step.BranchSpec, "branch")
	case schema.StepTypeParallel:
		return requireFlowSpec(step.ParallelSpec, "parallel")
	case schema.StepTypeApprove:
		return requireFlowSpec(step.ApproveSpec, "approve")
	case schema.StepTypeAssert:
		return requireFlowSpec(step.AssertSpec, "assert")
	case schema.StepTypeCompensate:
		return requireFlowSpec(step.CompensateSpec, "compensate")
	case schema.StepTypeWaitForEvent:
		return requireFlowSpec(step.WaitForEventSpec, "wait_for_event")
	case schema.StepTypeEnd:
		return requireFlowSpec(step.EndSpec, "end")
	case schema.StepTypeDisplay:
		return requireFlowSpec(step.DisplaySpec, "display")
	case schema.StepTypeNoop:
		if step.NoopSpec == nil {
			return &schema.NoopSpec{}, "noop", nil
		}
		return step.NoopSpec, "noop", nil
	case schema.StepTypeAssign:
		return step.AssignSpec, "assign", nil
	case schema.StepTypeResults:
		return &schema.ResultsSpec{}, "results", nil
	default:
		return nil, "", fmt.Errorf("unsupported flow step type %q", step.Type)
	}
}

func requireFlowSpec[T engine.StepSpec](spec T, kind string) (engine.StepSpec, string, error) {
	if reflect.ValueOf(spec).IsNil() {
		return nil, "", fmt.Errorf("%s spec is required", kind)
	}
	return spec, kind, nil
}

func assignFlowStepSpec(step *schema.Step, spec engine.StepSpec) error {
	switch step.Type {
	case schema.StepTypeCLI:
		step.CLI = spec.(*schema.CLISpec)
	case schema.StepTypeTool:
		step.ToolCall = spec.(*schema.ToolCallSpec)
	case schema.StepTypeInclude:
		step.IncludeSpec = spec.(*schema.IncludeSpec)
	case schema.StepTypeChoice:
		step.ChoiceSpec = spec.(*schema.ChoiceSpec)
	case schema.StepTypeDecision:
		step.DecisionSpec = spec.(*schema.DecisionSpec)
	case schema.StepTypeCollector:
		step.CollectorSpec = spec.(*schema.CollectorSpec)
	case schema.StepTypeHostAction:
		step.HostActionSpec = spec.(*schema.HostActionSpec)
	case schema.StepTypeHandoff:
		step.HandoffSpec = spec.(*schema.HandoffSpec)
	case schema.StepTypeBranch:
		step.BranchSpec = spec.(*schema.BranchSpec)
	case schema.StepTypeParallel:
		step.ParallelSpec = spec.(*schema.ParallelNode)
	case schema.StepTypeApprove:
		step.ApproveSpec = spec.(*schema.ApproveSpec)
	case schema.StepTypeAssert:
		step.AssertSpec = spec.(*schema.AssertSpec)
	case schema.StepTypeCompensate:
		step.CompensateSpec = spec.(*schema.CompensateSpec)
	case schema.StepTypeWaitForEvent:
		step.WaitForEventSpec = spec.(*schema.WaitForEventSpec)
	case schema.StepTypeEnd:
		step.EndSpec = spec.(*schema.EndSpec)
	case schema.StepTypeNoop:
		step.NoopSpec = spec.(*schema.NoopSpec)
	case schema.StepTypeAssign:
		step.AssignSpec = spec.(*schema.AssignSpec)
	case schema.StepTypeResults:
		step.ResultsSpec = spec.(*schema.ResultsSpec)
	case schema.StepTypeDisplay:
		step.DisplaySpec = spec.(*schema.DisplaySpec)
	default:
		return fmt.Errorf("unsupported flow step type %q", step.Type)
	}
	return nil
}

func clearFlowStepSpecs(step *schema.Step) {
	step.CLI = nil
	step.ToolCall = nil
	step.IncludeSpec = nil
	step.ChoiceSpec = nil
	step.DecisionSpec = nil
	step.CollectorSpec = nil
	step.HostActionSpec = nil
	step.HandoffSpec = nil
	step.BranchSpec = nil
	step.ApproveSpec = nil
	step.AssertSpec = nil
	step.CompensateSpec = nil
	step.WaitForEventSpec = nil
	step.EndSpec = nil
	step.NoopSpec = nil
	step.AssignSpec = nil
	step.ResultsSpec = nil
	step.DisplaySpec = nil
}

func decodeStrictJSON(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := expectJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

func cloneSnapshot(snapshot SnapshotV1) (SnapshotV1, error) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return SnapshotV1{}, fmt.Errorf("plan snapshot: encode: %w", err)
	}
	var clone SnapshotV1
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&clone); err != nil {
		return SnapshotV1{}, fmt.Errorf("plan snapshot: decode: %w", err)
	}
	if err := expectJSONEOF(decoder); err != nil {
		return SnapshotV1{}, fmt.Errorf("plan snapshot: decode: %w", err)
	}
	return clone, nil
}

func expectJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func snapshotDigest(snapshot SnapshotV1) (string, error) {
	snapshot.SnapshotDigest = ""
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("plan snapshot: calculate digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}
