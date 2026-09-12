package executor

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// AssertExecutor evaluates assertions.
type AssertExecutor struct {
	evaluator expr.Evaluator
}

// NewAssertExecutor constructs an AssertExecutor.
func NewAssertExecutor(eval expr.Evaluator) *AssertExecutor {
	return &AssertExecutor{evaluator: eval}
}

func (e *AssertExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	_ = ctx
	spec, ok := step.Spec.(*schema.AssertSpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("assert executor: invalid spec for step %s", step.ID)
	}

	var failures []map[string]any
	for _, assertion := range spec.Assert {
		subject, err := resolveTemplate(e.evaluator, assertion.Subject, vars)
		if err != nil {
			return nil, err
		}
		expected, err := resolveTemplate(e.evaluator, assertion.Expected, vars)
		if err != nil {
			return nil, err
		}

		ok, err := evaluateAssertion(assertion.Type, subject, expected)
		if err != nil {
			return nil, err
		}
		if !ok {
			failures = append(failures, map[string]any{
				"type":     assertion.Type,
				"subject":  subject,
				"expected": expected,
			})
		}
	}

	if len(failures) > 0 {
		result := newResult(step, engine.StepStatusFailed)
		result.Output["failures"] = failures
		return result, nil
	}
	return newResult(step, engine.StepStatusCompleted), nil
}

func evaluateAssertion(kind string, subject string, expected string) (bool, error) {
	switch kind {
	case "eq":
		return subject == expected, nil
	case "ne":
		return subject != expected, nil
	case "contains":
		return strings.Contains(subject, expected), nil
	case "matches":
		re, err := regexp.Compile(expected)
		if err != nil {
			return false, err
		}
		return re.MatchString(subject), nil
	case "exists":
		return strings.TrimSpace(subject) != "", nil
	default:
		return false, fmt.Errorf("unknown assertion type: %s", kind)
	}
}
