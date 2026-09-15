package planner

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
)

// ValidateToolScopes rejects partial scope adoption, including legacy plans
// carrying new references. Structural bodies retain the declaring file scope.
func ValidateToolScopes(plan *engine.ExecutionPlan) error {
	if plan == nil {
		return fmt.Errorf("tool scope: plan required")
	}
	if (plan.ToolScopes == nil) != (plan.RootScopeID == "") {
		return fmt.Errorf("tool scope: complete scope set and root required")
	}
	if plan.ToolScopes != nil && !plan.ToolScopes.HasScope(plan.RootScopeID) {
		return fmt.Errorf("tool scope: missing root")
	}
	if boundary := plan.ScopeBoundary; boundary != nil {
		if plan.ToolScopes == nil || boundary.ParentID == "" || boundary.ParentKind == "" ||
			boundary.ScopeID != plan.RootScopeID {
			return fmt.Errorf("tool scope: invalid explicit subplan boundary")
		}
	}
	if plan.ToolScopes != nil {
		snapshot := plan.ToolScopes.Export()
		if snapshot.ProfileDigest != toolscope.ProfileDigest(plan.Metadata.Profile) {
			return fmt.Errorf("tool scope: runtime profile differs from frozen scope")
		}
		if plan.Metadata.CatalogDigest != snapshot.CatalogDigest {
			return fmt.Errorf("tool scope: catalog differs from frozen scope")
		}
	}
	parents := map[string]engine.ResolvedStep{}
	for _, step := range plan.Steps {
		expected := plan.RootScopeID
		if boundary := plan.ScopeBoundary; boundary != nil && step.Depth == 0 {
			if step.ParentID != boundary.ParentID || step.ParentKind != boundary.ParentKind {
				return fmt.Errorf("tool scope: root step %s differs from explicit subplan boundary", step.ID)
			}
		} else if step.ParentID != "" {
			parent, ok := parents[step.ParentID]
			if !ok && plan.ToolScopes != nil {
				return fmt.Errorf("tool scope: missing structural parent %s", step.ParentID)
			}
			if ok && plan.ToolScopes != nil && step.ParentKind != parent.Kind {
				return fmt.Errorf("tool scope: structural parent kind mismatch for %s", step.ID)
			}
			expected = parent.LexicalScopeID
			if include, ok := parent.Spec.(*schema.IncludeSpec); ok {
				expected = include.TargetScopeID
			}
			if plan.ScopeBoundary != nil && step.Depth > 0 && step.ParentID == "" {
				return fmt.Errorf("tool scope: nested step %s has no structural parent", step.ID)
			}
		}
		if step.LexicalScopeID != expected {
			return fmt.Errorf("tool scope: step %s has incorrect owner", step.ID)
		}
		if err := validateIncludeEdge(plan.ToolScopes, step.LexicalScopeID, step.ID, step.Spec); err != nil {
			return err
		}
		if err := validateScopedSpec(plan.ToolScopes, step.LexicalScopeID, step.ToolBindingID, step.Spec); err != nil {
			return fmt.Errorf("tool scope: step %s: %w", step.ID, err)
		}
		parents[step.ID] = step
	}
	return nil
}

func ValidateScopedFlow(nodes []schema.FlowNode, scopes *toolscope.Set, scopeID string) error {
	if scopes != nil && !scopes.HasScope(scopeID) || scopes == nil && scopeID != "" {
		return fmt.Errorf("tool scope: missing flow owner")
	}
	for _, node := range nodes {
		if node.Step != nil {
			step := node.Step
			if step.LexicalScopeID != scopeID {
				return fmt.Errorf("tool scope: nested step %s has incorrect owner", step.ID)
			}
			if err := validateIncludeEdge(scopes, scopeID, step.ID, specForStep(step)); err != nil {
				return err
			}
			if err := validateScopedSpec(scopes, scopeID, step.ToolBindingID, specForStep(step)); err != nil {
				return err
			}

		}
		if node.Iterate != nil {
			if err := ValidateScopedFlow(node.Iterate.Steps, scopes, scopeID); err != nil {
				return err
			}
		}
		if node.Parallel != nil {
			for _, branch := range node.Parallel.Branches {
				if err := ValidateScopedFlow(branch.Steps, scopes, scopeID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateIncludeEdge(scopes *toolscope.Set, owner, stepID string, spec engine.StepSpec) error {
	if include, ok := spec.(*schema.IncludeSpec); ok && include != nil && scopes != nil && include.Include.RunbookRef == "" {
		return scopes.ValidateInclude(owner, stepID, include.TargetScopeID)
	}
	return nil
}

func validateScopedSpec(scopes *toolscope.Set, owner, bindingID string, spec engine.StepSpec) error {
	if call, ok := spec.(*schema.ToolCallSpec); ok && call != nil {
		if scopes == nil {
			if bindingID != "" {
				return fmt.Errorf("legacy tool step has binding reference")
			}
			return nil
		}
		invocation, err := scopes.Resolve(owner, call.Tool.Name, call.Tool.Action)
		if err != nil {
			return err
		}
		if invocation.BindingID != bindingID {
			return fmt.Errorf("binding reference mismatch")
		}
	} else if bindingID != "" {
		return fmt.Errorf("non-tool step has binding reference")
	}
	switch spec := spec.(type) {
	case *schema.IncludeSpec:
		if spec == nil {
			return nil
		}
		if scopes == nil && spec.TargetScopeID != "" {
			return fmt.Errorf("legacy include has scope reference")
		}
		if scopes != nil {
			if spec.Include.RunbookRef != "" && spec.TargetScopeID == "" && len(spec.ResolvedSteps) == 0 {
				return nil
			}
			if !scopes.HasScope(spec.TargetScopeID) {
				return fmt.Errorf("include target scope missing")
			}
		}
		return ValidateScopedFlow(spec.ResolvedSteps, scopes, spec.TargetScopeID)
	case *schema.BranchSpec:
		if spec != nil {
			for _, arm := range spec.Branches {
				if err := ValidateScopedFlow(arm.Steps, scopes, owner); err != nil {
					return err
				}
			}
		}
	case *schema.IterateNode:
		if spec != nil {
			return ValidateScopedFlow(spec.Steps, scopes, owner)
		}
	case *schema.ParallelNode:
		if spec != nil {
			for _, branch := range spec.Branches {
				if err := ValidateScopedFlow(branch.Steps, scopes, owner); err != nil {
					return err
				}
			}
		}
	case *schema.CompensateSpec:
		if spec != nil {
			return ValidateScopedFlow(spec.Compensate.Steps, scopes, owner)
		}
	}
	return nil
}

// ScopedPlanHash covers executable fields normally omitted by schema JSON,
// including materialized include bodies. It never reads present-day source.
func ScopedPlanHash(plan *engine.ExecutionPlan) (string, error) {
	copy := *plan
	copy.Validation = nil
	copy.Governance = nil
	copy.Metadata.PlanHash = ""
	copy.RunID = ""
	copy.ToolScopes = nil
	body, err := json.Marshal(struct {
		Plan   any
		Scopes toolscope.Snapshot
	}{
		Plan: executableValue(reflect.ValueOf(copy)), Scopes: plan.ToolScopes.Export(),
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body)), nil
}

// Snapshot structural fields omit empty collections. Normalize only fields of
// executable records: values inside authored maps, arrays, or nonnil interfaces
// keep the meaningful distinctions between null, [], {}, false, and zero.
func emptyExecutableField(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return value.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return value.IsZero()
	}
	return false
}

func executableValue(value reflect.Value) any {
	if !value.IsValid() {
		return nil
	}
	// Bodyless flow steps may omit their union pointer in parsed source.
	// The durable flow codec restores the implicit empty spec; hash both
	// representations identically without mutating the captured source tree.
	if value.CanInterface() {
		if step, ok := value.Interface().(schema.Step); ok {
			switch step.Type {
			case schema.StepTypeNoop:
				if step.NoopSpec == nil {
					step.NoopSpec = &schema.NoopSpec{}
				}
			case schema.StepTypeResults:
				if step.ResultsSpec == nil {
					step.ResultsSpec = &schema.ResultsSpec{}
				}
			}
			value = reflect.ValueOf(step)
		}
	}
	// Durable codecs retain typed presence flags and canonical representations
	// (notably bindings, outputs, raw JSON, and time.Time). Do not hash their
	// implementation fields instead of their serialized meaning.
	if value.CanInterface() {
		if _, ok := value.Interface().(json.Marshaler); ok {
			return value.Interface()
		}
	}
	if value.Type() == reflect.TypeOf(json.Number("")) {
		return value.Interface()
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return nil
		}
		if _, executableSpec := value.Interface().(engine.StepSpec); !executableSpec {
			return value.Interface()
		}
		return executableValue(value.Elem())
	case reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		return executableValue(value.Elem())
	case reflect.Struct:
		fields := map[string]any{}
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() && !emptyExecutableField(value.Field(i)) {
				fields[value.Type().Field(i).Name] = executableValue(value.Field(i))
			}
		}

		return fields
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil
		}
		items := make([]any, value.Len())
		for i := range items {
			items[i] = executableValue(value.Index(i))
		}
		return items
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		fields := map[string]any{}
		iter := value.MapRange()
		for iter.Next() {
			fields[fmt.Sprint(iter.Key().Interface())] = executableValue(iter.Value())
		}
		return fields
	default:
		return value.Interface()
	}
}
