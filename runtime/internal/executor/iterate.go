package executor

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// IterateExecutor loops over a collection and executes nested steps.
type IterateExecutor struct {
	evaluator expr.Evaluator
	condition expr.ConditionEvaluator
	runner    SubStepRunner
}

// NewIterateExecutor constructs an IterateExecutor.
func NewIterateExecutor(eval expr.Evaluator, cond expr.ConditionEvaluator, runner SubStepRunner) *IterateExecutor {
	return &IterateExecutor{evaluator: eval, condition: cond, runner: runner}
}

func (e *IterateExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.IterateNode)
	if !ok || spec == nil {
		return nil, fmt.Errorf("iterate executor: invalid spec for step %s", step.ID)
	}
	if e.runner == nil {
		return nil, fmt.Errorf("iterate executor: SubStepRunner is required")
	}
	for key := range spec.CollectValues {
		if _, duplicate := spec.Collect[key]; duplicate {
			return nil, fmt.Errorf("iterate executor: %q is declared in both collect and collect_values", key)
		}
	}

	over := spec.Over
	if e.evaluator != nil && over != "" {
		resolved, err := resolveTemplate(e.evaluator, over, vars)
		if err != nil {
			return nil, err
		}
		over = resolved
	}

	items, err := resolveIterateItems(over, vars, spec.Max)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		result := newResult(step, engine.StepStatusCompleted)
		for key := range spec.CollectValues {
			result.Vars[key] = []any{}
		}
		return result, nil
	}

	loopVar := spec.As
	if loopVar == "" {
		loopVar = "item"
	}

	// Dispatch to concurrent or sequential based on concurrency setting
	if spec.Concurrency > 1 {
		return e.executeConcurrent(ctx, step, spec, items, loopVar, vars)
	}
	return e.executeSequential(ctx, step, spec, items, loopVar, vars)
}

// executeSequential runs iterations sequentially (original behavior)
func (e *IterateExecutor) executeSequential(ctx context.Context, step engine.ResolvedStep, spec *schema.IterateNode, items []any, loopVar string, vars map[string]any) (*engine.StepResult, error) {
	workingVars := copyVars(vars)
	collected := map[string]any{}
	iterationVars := map[string]any{}
	iterations := 0
	status := engine.StepStatusCompleted
	terminal := false
	var terminalOutcomeCat, terminalOutcomeCode any
	emit := EmitterFromContext(ctx)
	total := len(items)

	for i, item := range items {
		iterations = i + 1
		workingVars[loopVar] = item
		workingVars["iteration"] = iterations

		iterStart := time.Now()

		if emit != nil {
			emit("iterate/iteration_started", map[string]any{
				"step_id":         step.ID,
				"iteration_index": iterations,
				"iteration_total": total,
				"as":              loopVar,
				"value":           item,
			})
		}

		results, err := e.runner(ctx, SubStepParent{
			ID: step.ID, Kind: "iterate", IterationIndex: iterations, NestDepth: step.NestDepth + 1,
		}, spec.Steps, copyVars(workingVars))
		if err != nil {
			if emit != nil {
				emit("iterate/iteration_completed", map[string]any{
					"step_id":         step.ID,
					"iteration_index": iterations,
					"iteration_total": total,
					"as":              loopVar,
					"value":           item,
					"status":          string(engine.StepStatusFailed),
					"duration_ms":     time.Since(iterStart).Milliseconds(),
				})
			}
			return nil, err
		}
		iterStatus := engine.StepStatusCompleted
		iterTerminal := false
		var iterOutcomeCat, iterOutcomeCode any
		for _, res := range results {
			if res == nil {
				continue
			}
			for k, v := range res.Vars {
				workingVars[k] = v
				iterationVars[k] = v
			}
			if res.Status == engine.StepStatusFailed {
				status = engine.StepStatusFailed
				iterStatus = engine.StepStatusFailed
			}
			if res.Output != nil {
				if t, ok := res.Output["terminal"].(bool); ok && t {
					iterTerminal = true
					if v, ok := res.Output["outcome_category"]; ok {
						iterOutcomeCat = v
					}
					if v, ok := res.Output["outcome_code"]; ok {
						iterOutcomeCode = v
					}
				}
			}
		}
		if iterTerminal {
			terminal = true
			terminalOutcomeCat = iterOutcomeCat
			terminalOutcomeCode = iterOutcomeCode
		}

		for key, tmpl := range spec.Collect {
			val := tmpl
			if e.evaluator != nil {
				resolved, err := resolveTemplate(e.evaluator, tmpl, workingVars)
				if err != nil {
					return nil, err
				}
				val = resolved
			}
			collected[key] = appendCollect(collected[key], val)
		}
		for key, template := range spec.CollectValues {
			value, err := resolveTypedValue(e.evaluator, template, workingVars)
			if err != nil {
				return nil, fmt.Errorf("iterate collect_values %q: %w", key, err)
			}
			collected[key] = appendCollect(collected[key], value)
		}

		if emit != nil {
			emit("iterate/iteration_completed", map[string]any{
				"step_id":         step.ID,
				"iteration_index": iterations,
				"iteration_total": total,
				"as":              loopVar,
				"value":           item,
				"status":          string(iterStatus),
				"duration_ms":     time.Since(iterStart).Milliseconds(),
			})
		}

		if spec.Until != "" {
			ok, err := evalCondition(e.condition, spec.Until, workingVars)
			if err != nil {
				return nil, err
			}
			if ok {
				break
			}
		}

		if spec.Max > 0 && iterations >= spec.Max {
			break
		}

		// A child set Output["terminal"]=true (e.g. a nested end
		// step or an include whose outcome gate fired). Stop
		// iterating and let the outer engine see the terminal
		// signal in this iterate's result.
		if terminal {
			break
		}
	}

	result := newResult(step, status)
	result.Output["iterations"] = iterations
	for k, v := range iterationVars {
		result.Vars[k] = v
	}
	for k, v := range collected {
		result.Vars[k] = v
	}
	if terminal {
		result.Output["terminal"] = true
		if terminalOutcomeCat != nil {
			result.Output["outcome_category"] = terminalOutcomeCat
		}
		if terminalOutcomeCode != nil {
			result.Output["outcome_code"] = terminalOutcomeCode
		}
	}
	return result, nil
}

// executeConcurrent runs iterations as a worker pool with fail-fast semantics
func (e *IterateExecutor) executeConcurrent(ctx context.Context, step engine.ResolvedStep, spec *schema.IterateNode, items []any, loopVar string, vars map[string]any) (*engine.StepResult, error) {
	// Create cancellable context for fail-fast error handling
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Worker pool semaphore
	sem := make(chan struct{}, spec.Concurrency)

	// Shared state protected by mutex
	var mu sync.Mutex
	collected := map[string]any{}
	typedCollections := make([]map[string]any, len(items))
	iterationVars := map[string]any{}
	status := engine.StepStatusCompleted
	var firstErr error
	iterations := 0

	// WaitGroup to track all workers
	var wg sync.WaitGroup

	// Note: Until condition is evaluated after each iteration completes in concurrent mode.
	// The evaluation order is non-deterministic across workers.
	for i, item := range items {
		// Check if we should stop due to error
		mu.Lock()
		if firstErr != nil {
			mu.Unlock()
			break
		}
		mu.Unlock()

		wg.Add(1)
		// Capture loop variables for goroutine (avoid data race)
		iterNum := i + 1
		itemCopy := item
		total := len(items)
		emit := EmitterFromContext(ctx)

		go func() {
			defer wg.Done()

			// Acquire semaphore
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }() // Release semaphore when done
			case <-runCtx.Done():
				return
			}

			// Check context before running
			select {
			case <-runCtx.Done():
				return
			default:
			}

			iterStart := time.Now()
			if emit != nil {
				emit("iterate/iteration_started", map[string]any{
					"step_id":         step.ID,
					"iteration_index": iterNum,
					"iteration_total": total,
					"as":              loopVar,
					"value":           itemCopy,
				})
			}

			// Build per-iteration variables (isolated from other goroutines)
			iterVars := copyVars(vars)
			iterVars[loopVar] = itemCopy
			iterVars["iteration"] = iterNum

			// Run iteration steps
			results, err := e.runner(engine.WithConcurrentExecution(runCtx), SubStepParent{
				ID: step.ID, Kind: "iterate", IterationIndex: iterNum, NestDepth: step.NestDepth + 1,
			}, spec.Steps, iterVars)

			// Determine per-iteration status before merging into shared state.
			iterStatus := engine.StepStatusCompleted
			if err != nil {
				iterStatus = engine.StepStatusFailed
			} else {
				for _, res := range results {
					if res != nil && res.Status == engine.StepStatusFailed {
						iterStatus = engine.StepStatusFailed
						break
					}
				}
			}
			iterDur := time.Since(iterStart).Milliseconds()

			// Update shared state
			mu.Lock()
			defer mu.Unlock()

			if emit != nil {
				emit("iterate/iteration_completed", map[string]any{
					"step_id":         step.ID,
					"iteration_index": iterNum,
					"iteration_total": total,
					"as":              loopVar,
					"value":           itemCopy,
					"status":          string(iterStatus),
					"duration_ms":     iterDur,
				})
			}

			// Fail-fast: record first error and cancel remaining workers
			if err != nil && firstErr == nil {
				firstErr = err
				status = engine.StepStatusFailed
				cancel()
				return
			}

			// Check step results for failure
			for _, res := range results {
				if res == nil {
					continue
				}
				// Merge vars from this iteration — include into iterVars so collect
				// expressions below can reference vars set by sub-steps.
				for k, v := range res.Vars {
					iterationVars[k] = v
					iterVars[k] = v
				}
				if res.Status == engine.StepStatusFailed {
					status = engine.StepStatusFailed
				}
			}

			// Collect aggregation (mutex-protected)
			for key, tmpl := range spec.Collect {
				val := tmpl
				if e.evaluator != nil {
					resolved, err := resolveTemplate(e.evaluator, tmpl, iterVars)
					if err != nil && firstErr == nil {
						firstErr = err
						status = engine.StepStatusFailed
						cancel()
						return
					}
					val = resolved
				}
				collected[key] = appendCollect(collected[key], val)
			}
			values := make(map[string]any, len(spec.CollectValues))
			for key, template := range spec.CollectValues {
				value, err := resolveTypedValue(e.evaluator, template, iterVars)
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("iterate collect_values %q: %w", key, err)
					}
					status = engine.StepStatusFailed
					cancel()
					return
				}
				values[key] = value
			}
			typedCollections[iterNum-1] = values

			iterations = iterNum

			// Until condition check (non-deterministic across workers)
			if spec.Until != "" {
				ok, err := evalCondition(e.condition, spec.Until, iterVars)
				if err != nil && firstErr == nil {
					firstErr = err
					status = engine.StepStatusFailed
					cancel()
					return
				}
				if ok {
					cancel() // Stop other workers
				}
			}
		}()
	}

	// Wait for all workers to complete
	wg.Wait()

	// Return first error if any occurred
	if firstErr != nil {
		return nil, firstErr
	}

	result := newResult(step, status)
	result.Output["iterations"] = iterations
	for k, v := range iterationVars {
		result.Vars[k] = v
	}
	for k, v := range collected {
		result.Vars[k] = v
	}
	for key := range spec.CollectValues {
		result.Vars[key] = []any{}
	}
	for _, values := range typedCollections {
		for key, value := range values {
			result.Vars[key] = appendCollect(result.Vars[key], value)
		}
	}
	return result, nil
}

func resolveIterateItems(over string, vars map[string]any, max int) ([]any, error) {
	trimmed := strings.TrimSpace(over)
	if strings.HasPrefix(trimmed, "$.") {
		key := strings.TrimPrefix(trimmed, "$.")
		val, ok := vars[key]
		if !ok {
			return nil, nil
		}
		if slice := valueToSlice(val); slice != nil {
			return slice, nil
		}
	}

	if trimmed == "" {
		if max <= 0 {
			return nil, nil
		}
		return rangeItems(max), nil
	}
	if n, err := strconv.Atoi(trimmed); err == nil {
		return rangeItems(n), nil
	}

	// Plain var name: if the resolved string matches a vars key, use the var's value.
	// This supports `over: services` (bare name) in addition to `over: "{{ .services }}"`.
	if val, ok := vars[trimmed]; ok {
		if slice := valueToSlice(val); slice != nil {
			return slice, nil
		}
		// If the var's value is a string (possibly CSV), substitute and fall through.
		if s, ok2 := val.(string); ok2 {
			trimmed = strings.TrimSpace(s)
		}
	}

	if strings.Contains(trimmed, ",") {
		parts := strings.Split(trimmed, ",")
		items := make([]any, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			items = append(items, part)
		}
		return items, nil
	}
	return []any{trimmed}, nil
}

func rangeItems(n int) []any {
	items := make([]any, 0, n)
	for i := 1; i <= n; i++ {
		items = append(items, i)
	}
	return items
}

func valueToSlice(val any) []any {
	switch v := val.(type) {
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out
	case []int:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out
	}

	rv := reflect.ValueOf(val)
	if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out
	}
	return nil
}

// appendCollect appends val to existing, accumulating items into a []any slice.
// This ensures that collect expressions produce a list across iterations rather
// than overwriting each other.
func appendCollect(existing any, val any) []any {
	switch v := existing.(type) {
	case []any:
		return append(v, val)
	case nil:
		return []any{val}
	default:
		return []any{v, val}
	}
}
