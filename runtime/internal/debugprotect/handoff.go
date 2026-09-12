package debugprotect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type handoffValueValidatorContextKey struct{}

type handoffValueValidator func(engine.HandoffRequest) error

// WithHandoffValueValidator carries a write-only handoff safety gate. The
// private protection values remain captured by the closure and cannot be read
// back from the executor context.
func WithHandoffValueValidator(ctx context.Context, protection engine.DebugProtection) context.Context {
	validator := func(request engine.HandoffRequest) error {
		return ValidateHandoffRequest(protection, request)
	}
	return context.WithValue(ctx, handoffValueValidatorContextKey{}, handoffValueValidator(validator))
}

// ValidateHandoffValuesFromContext submits evaluated values to the root-owned
// safety gate without exposing its private secret set.
func ValidateHandoffValuesFromContext(ctx context.Context, contextValues, facts map[string]any) error {
	return ValidateHandoffRequestFromContext(ctx, engine.HandoffRequest{Context: contextValues, Facts: facts})
}

// ValidateHandoffRequestFromContext submits every persisted handoff field to
// the write-only root safety gate without exposing private protection values.
func ValidateHandoffRequestFromContext(ctx context.Context, request engine.HandoffRequest) error {
	validator, _ := ctx.Value(handoffValueValidatorContextKey{}).(handoffValueValidator)
	if validator == nil {
		return nil
	}
	return validator(request)
}

// ValidateHandoffRequest rejects protected content anywhere in the durable
// control signal, including target, reason, provenance, context, and facts.
func ValidateHandoffRequest(protection engine.DebugProtection, request engine.HandoffRequest) error {
	if protectedHandoffBinding(protection, request.ContextBindings) ||
		protectedHandoffBinding(protection, request.FactBindings) {
		return errors.New("handoff provenance references protected content")
	}
	persisted := struct {
		TargetRunbook   string            `json:"target_runbook"`
		ReasonCode      string            `json:"reason_code"`
		ReasonSummary   string            `json:"reason_summary"`
		Context         map[string]any    `json:"context,omitempty"`
		Facts           map[string]any    `json:"facts,omitempty"`
		ContextBindings map[string]string `json:"context_bindings,omitempty"`
		FactBindings    map[string]string `json:"fact_bindings,omitempty"`
	}{
		TargetRunbook: request.TargetRunbook, ReasonCode: request.ReasonCode,
		ReasonSummary: request.ReasonSummary, Context: request.Context, Facts: request.Facts,
		ContextBindings: request.ContextBindings, FactBindings: request.FactBindings,
	}
	return validateHandoffContent(protection, persisted)
}

// ValidateHandoffValues rejects values covered by secret literals or runtime
// redaction policy. Errors deliberately contain no transferred values.
func ValidateHandoffValues(
	protection engine.DebugProtection,
	contextValues map[string]any,
	facts map[string]any,
) error {
	return validateHandoffContent(protection, struct {
		Context map[string]any `json:"context,omitempty"`
		Facts   map[string]any `json:"facts,omitempty"`
	}{contextValues, facts})
}

// ValidateHandoffFlow rejects protected authored handoff fields and binding
// provenance throughout a resolved executable closure.
func ValidateHandoffFlow(protection engine.DebugProtection, nodes []schema.FlowNode) error {
	for index := range nodes {
		node := &nodes[index]
		if node.Step != nil {
			if err := validateHandoffStep(protection, node.Step); err != nil {
				return err
			}
		}
		if node.Iterate != nil {
			if err := ValidateHandoffFlow(protection, node.Iterate.Steps); err != nil {
				return err
			}
		}
		if node.Parallel != nil {
			for _, branch := range node.Parallel.Branches {
				if err := ValidateHandoffFlow(protection, branch.Steps); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateHandoffStep(protection engine.DebugProtection, step *schema.Step) error {
	if step == nil {
		return nil
	}
	if step.HandoffSpec != nil {
		handoff := step.HandoffSpec.Handoff
		if err := ValidateHandoffRequest(protection, engine.HandoffRequest{
			TargetRunbook: handoff.Runbook, ReasonCode: handoff.Reason.Code,
			ReasonSummary:   handoff.Reason.Summary,
			ContextBindings: handoff.With, FactBindings: handoff.Facts,
		}); err != nil {
			return errors.New("handoff definition contains protected content")
		}
	}
	if step.IncludeSpec != nil {
		if err := ValidateHandoffFlow(protection, step.IncludeSpec.ResolvedSteps); err != nil {
			return err
		}
	}
	if step.BranchSpec != nil {
		for _, branch := range step.BranchSpec.Branches {
			if err := ValidateHandoffFlow(protection, branch.Steps); err != nil {
				return err
			}
		}
	}
	if step.ParallelSpec != nil {
		for _, branch := range step.ParallelSpec.Branches {
			if err := ValidateHandoffFlow(protection, branch.Steps); err != nil {
				return err
			}
		}
	}
	if step.CompensateSpec != nil {
		return ValidateHandoffFlow(protection, step.CompensateSpec.Compensate.Steps)
	}
	return nil
}

func validateHandoffContent(protection engine.DebugProtection, persisted any) error {
	if unsafeRuntimeValue(persisted) {
		return errors.New("handoff values contain an unsafe runtime value")
	}
	return validateRawHandoffContent(protection, reflect.ValueOf(persisted), 0)
}

// ValidateHandoffJSON scans already-validated JSON artifacts without treating
// decoded JSON depth or number wrappers as arbitrary runtime objects.
func ValidateHandoffJSON(protection engine.DebugProtection, artifacts ...json.RawMessage) error {
	for _, artifact := range artifacts {
		if !json.Valid(artifact) {
			return errors.New("handoff artifact is not valid JSON")
		}
		decoder := json.NewDecoder(strings.NewReader(string(artifact)))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return errors.New("handoff artifact is not valid JSON")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return errors.New("handoff artifact has trailing content")
		}
		if err := validateRawHandoffContent(protection, reflect.ValueOf(decoded), 0); err != nil {
			return err
		}
	}
	return nil
}

func validateRawHandoffContent(protection engine.DebugProtection, value reflect.Value, depth int) error {
	if !value.IsValid() {
		return nil
	}
	if depth > 64 {
		return errors.New("handoff content exceeds nesting limit")
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return validateRawHandoffContent(protection, value.Elem(), depth+1)
	}
	if value.Kind() == reflect.String {
		return validateHandoffString(protection, value.String())
	}
	switch value.Kind() {
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateRawHandoffContent(protection, iterator.Key(), depth+1); err != nil {
				return err
			}
			if err := validateRawHandoffContent(protection, iterator.Value(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if err := validateRawHandoffContent(protection, value.Index(index), depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if field.PkgPath != "" {
				continue
			}
			if err := validateRawHandoffContent(protection, value.Field(index), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateHandoffString(protection engine.DebugProtection, value string) error {
	for _, secret := range protection.SecretValues {
		if secret != "" && strings.Contains(value, secret) {
			return errors.New("handoff values contain protected content")
		}
	}
	if len(protection.RedactionPatterns) > 0 {
		redactor, compileErr := internalgovernance.NewRedactor(protection.RedactionPatterns)
		if compileErr != nil {
			return errors.New("handoff redaction policy is invalid")
		}
		if _, matches := redactor.RedactString(value); matches > 0 {
			return errors.New("handoff values match protected content")
		}
	}
	return nil
}

func protectedHandoffBinding(protection engine.DebugProtection, bindings map[string]string) bool {
	if len(bindings) == 0 || len(protection.ProtectedVars) == 0 {
		return false
	}
	protected := make(map[string]bool, len(protection.ProtectedVars))
	for _, name := range protection.ProtectedVars {
		protected[name] = true
	}
	for destination, source := range bindings {
		if protected[destination] {
			return true
		}
		if exactSource, ok := schema.HandoffBindingSource(source); ok {
			source = exactSource
		}
		for _, segment := range strings.Split(source, ".") {
			if protected[segment] {
				return true
			}
		}
	}
	return false
}

func unsafeRuntimeValue(value any) bool {
	return unsafeRuntimeReflect(reflect.ValueOf(value), 0)
}

func unsafeRuntimeReflect(value reflect.Value, depth int) bool {
	if !value.IsValid() || depth > 64 {
		return depth > 64
	}
	if value.CanInterface() {
		if _, ok := value.Interface().(error); ok {
			return true
		}
		if _, ok := value.Interface().(fmt.Stringer); ok && value.Kind() != reflect.String {
			return true
		}
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return false
		}
		return unsafeRuntimeReflect(value.Elem(), depth+1)
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if unsafeRuntimeReflect(iterator.Key(), depth+1) || unsafeRuntimeReflect(iterator.Value(), depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if unsafeRuntimeReflect(value.Index(index), depth+1) {
				return true
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).PkgPath == "" && unsafeRuntimeReflect(value.Field(index), depth+1) {
				return true
			}
		}
	}
	return false
}
