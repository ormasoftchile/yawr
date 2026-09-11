package engine

import (
	"context"
	"encoding/json"
	"errors"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	"github.com/ormasoftchile/yawr/runtime/internal/plansnapshot"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ValidateDurableHandoffPlan rejects protected content in authored handoff
// definitions before any durable executable snapshot is published.
func (e *impl) ValidateDurableHandoffPlan(
	ctx context.Context,
	plan *enginepkg.ExecutionPlan,
	opts enginepkg.RunOptions,
) error {
	if plan == nil || plan.Validation == nil {
		return errors.New("engine: validated execution plan is required")
	}
	if !planContainsHandoff(plan) {
		return nil
	}
	protection, complete := e.durableHandoffProtection(ctx, plan, opts)
	if !complete && internaldebugprotect.ProtectAllFromContext(ctx) {
		return errors.New("engine: handoff definitions cannot be proven free of protected content")
	}
	snapshot, err := plansnapshot.FromExecutionPlan(plan)
	if err != nil {
		return errors.New("engine: immutable handoff plan cannot be encoded")
	}
	encodedPlan, err := json.Marshal(snapshot)
	if err != nil {
		return errors.New("engine: immutable handoff plan cannot be encoded")
	}
	canonicalPlan, err := handoffProtectionSnapshot(encodedPlan)
	if err != nil {
		return errors.New("engine: immutable handoff plan cannot be scanned")
	}
	if err := internaldebugprotect.ValidateHandoffJSON(protection, canonicalPlan); err != nil {
		return errors.New("engine: immutable handoff plan contains protected content")
	}
	for _, step := range plan.Steps {
		if err := validateHandoffStepSpec(protection, step.Spec); err != nil {
			return err
		}
	}
	return nil
}

func handoffProtectionSnapshot(encoded []byte) (json.RawMessage, error) {
	return plansnapshot.CanonicalProtectionArtifact(encoded)
}

func planContainsHandoff(plan *enginepkg.ExecutionPlan) bool {
	if plan == nil {
		return false
	}
	for _, step := range plan.Steps {
		if stepSpecContainsHandoff(step.Spec) {
			return true
		}
	}
	return false
}

func stepSpecContainsHandoff(spec enginepkg.StepSpec) bool {
	switch typed := spec.(type) {
	case *schema.HandoffSpec:
		return typed != nil
	case *schema.IncludeSpec:
		return typed != nil && flowContainsHandoff(typed.ResolvedSteps)
	case *schema.BranchSpec:
		if typed != nil {
			for _, branch := range typed.Branches {
				if flowContainsHandoff(branch.Steps) {
					return true
				}
			}
		}
	case *schema.IterateNode:
		return typed != nil && flowContainsHandoff(typed.Steps)
	case *schema.ParallelNode:
		if typed != nil {
			for _, branch := range typed.Branches {
				if flowContainsHandoff(branch.Steps) {
					return true
				}
			}
		}
	case *schema.CompensateSpec:
		return typed != nil && flowContainsHandoff(typed.Compensate.Steps)
	}
	return false
}

func flowContainsHandoff(nodes []schema.FlowNode) bool {
	for index := range nodes {
		node := &nodes[index]
		if node.Step != nil && stepSpecContainsHandoff(stepSpecForHandoffProtection(node.Step)) ||
			node.Iterate != nil && stepSpecContainsHandoff(node.Iterate) ||
			node.Parallel != nil && stepSpecContainsHandoff(node.Parallel) {
			return true
		}
	}
	return false
}

// ValidateDurableHandoffArtifacts applies the same private run protection to
// serialized artifacts before a session store can publish them.
func (e *impl) ValidateDurableHandoffArtifacts(
	ctx context.Context,
	plan *enginepkg.ExecutionPlan,
	opts enginepkg.RunOptions,
	artifacts ...json.RawMessage,
) error {
	if plan == nil || plan.Validation == nil {
		return errors.New("engine: validated execution plan is required")
	}
	protection, complete := e.durableHandoffProtection(ctx, plan, opts)
	if !complete && internaldebugprotect.ProtectAllFromContext(ctx) {
		return errors.New("engine: durable artifacts cannot be proven free of protected content")
	}
	for _, artifact := range artifacts {
		candidate := artifact
		if isExecutionPlanSnapshot(artifact) {
			canonical, err := handoffProtectionSnapshot(artifact)
			if err != nil {
				return errors.New("engine: durable execution plan artifact cannot be scanned")
			}
			candidate = canonical
		}
		if err := internaldebugprotect.ValidateHandoffJSON(protection, candidate); err != nil {
			return errors.New("engine: durable artifact contains protected content")
		}
	}
	return nil
}

func isExecutionPlanSnapshot(artifact []byte) bool {
	var snapshot plansnapshot.SnapshotV1
	if json.Unmarshal(artifact, &snapshot) != nil || (snapshot.SchemaVersion != plansnapshot.SchemaVersionV1 && snapshot.SchemaVersion != plansnapshot.SchemaVersionV2) {
		return false
	}
	_, err := plansnapshot.Restore(snapshot)
	return err == nil
}

func (e *impl) durableHandoffProtection(
	ctx context.Context,
	plan *enginepkg.ExecutionPlan,
	opts enginepkg.RunOptions,
) (enginepkg.DebugProtection, bool) {
	run := enginepkg.NewRun(plan.RunID, plan, opts)
	protection := buildDebugProtection(ctx, plan, run.Vars)
	return precomputeDebugProtection(ctx, e.cfg.Executors, plan, run.Vars, protection)
}

func validateHandoffStepSpec(protection enginepkg.DebugProtection, spec enginepkg.StepSpec) error {
	switch typed := spec.(type) {
	case *schema.HandoffSpec:
		if typed == nil {
			return nil
		}
		request := enginepkg.HandoffRequest{
			TargetRunbook: typed.Handoff.Runbook,
			ReasonCode:    typed.Handoff.Reason.Code, ReasonSummary: typed.Handoff.Reason.Summary,
			ContextBindings: typed.Handoff.With, FactBindings: typed.Handoff.Facts,
		}
		if err := internaldebugprotect.ValidateHandoffRequest(protection, request); err != nil {
			return errors.New("engine: handoff definition contains protected content")
		}
	case *schema.IncludeSpec:
		if typed != nil {
			return validateHandoffFlow(protection, typed.ResolvedSteps)
		}
	case *schema.BranchSpec:
		if typed != nil {
			for _, branch := range typed.Branches {
				if err := validateHandoffFlow(protection, branch.Steps); err != nil {
					return err
				}
			}
		}
	case *schema.IterateNode:
		if typed != nil {
			return validateHandoffFlow(protection, typed.Steps)
		}
	case *schema.ParallelNode:
		if typed != nil {
			for _, branch := range typed.Branches {
				if err := validateHandoffFlow(protection, branch.Steps); err != nil {
					return err
				}
			}
		}
	case *schema.CompensateSpec:
		if typed != nil {
			return validateHandoffFlow(protection, typed.Compensate.Steps)
		}
	}
	return nil
}

func validateHandoffFlow(protection enginepkg.DebugProtection, nodes []schema.FlowNode) error {
	for index := range nodes {
		node := &nodes[index]
		if node.Step != nil {
			if err := validateHandoffStepSpec(protection, stepSpecForHandoffProtection(node.Step)); err != nil {
				return err
			}
		}
		if node.Iterate != nil {
			if err := validateHandoffStepSpec(protection, node.Iterate); err != nil {
				return err
			}
		}
		if node.Parallel != nil {
			if err := validateHandoffStepSpec(protection, node.Parallel); err != nil {
				return err
			}
		}
	}
	return nil
}

func stepSpecForHandoffProtection(step *schema.Step) enginepkg.StepSpec {
	if step == nil {
		return nil
	}
	switch step.Type {
	case schema.StepTypeHandoff:
		return step.HandoffSpec
	case schema.StepTypeInclude:
		return step.IncludeSpec
	case schema.StepTypeBranch:
		return step.BranchSpec
	case schema.StepTypeParallel:
		return step.ParallelSpec
	case schema.StepTypeCompensate:
		return step.CompensateSpec
	default:
		return nil
	}
}
