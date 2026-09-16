package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	gcpparser "github.com/ormasoftchile/yawr/runtime/pkg/gcp/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/gdp"
	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// validatePlan parses every GXL, GIS, and GCP-bearing field in plan.
func validatePlan(plan *engine.ExecutionPlan) (*engine.ValidatedPlan, error) {
	if err := ValidateToolScopes(plan); err != nil {
		return nil, err
	}
	if plan.ToolScopes != nil {
		hash, err := ScopedPlanHash(plan)
		if err != nil {
			return nil, err
		}
		plan.Metadata.PlanHash = hash
	}
	vp := &engine.ValidatedPlan{
		Source:          plan,
		RunbookID:       plan.Metadata.RunbookID,
		RunbookHash:     planHash(plan),
		GXLExprs:        make(map[engine.StepRef]*gxlparser.Expr),
		GISTemplates:    make(map[engine.StepRef]*gis.Template),
		GCPPaths:        make(map[engine.StepRef]*gcpparser.Path),
		GrammarVersions: engine.GrammarVersions{GXL: engine.GrammarVersionGXL, GIS: engine.GrammarVersionGIS, GCP: engine.GrammarVersionGCP},
		ValidatedAt:     time.Now().UTC(),
	}
	stepIDs := make(map[string]struct{}, len(plan.Steps))
	for _, step := range plan.Steps {
		stepIDs[step.ID] = struct{}{}
	}

	var errs []engine.PlanValidationError
	for _, tool := range validationToolDefinitions(plan) {
		if tool == nil {
			continue
		}
		if err := tool.ValidatePresentations(); err != nil {
			errs = append(errs, validationErr("", "presentation", "", err))
		}
	}
	for _, step := range plan.Steps {
		validateStep(vp, &errs, step, stepIDs)
	}
	validateHandoffConcurrency(plan, &errs)
	validateEnumConstraints(vp, &errs, plan)
	if len(errs) > 0 {
		return nil, &engine.PlanValidationMultiError{Errors: errs}
	}
	vp.ExpressionCount = engine.ExpressionCount{GXL: len(vp.GXLExprs), GIS: len(vp.GISTemplates), GCP: len(vp.GCPPaths)}
	plan.Metadata.PlanHash = vp.RunbookHash
	plan.Validation = vp
	return vp, nil
}

func validateHandoffConcurrency(plan *engine.ExecutionPlan, errs *[]engine.PlanValidationError) {
	if plan == nil {
		return
	}
	concurrentByDepth := make([]bool, 0)
	for _, step := range plan.Steps {
		if step.Depth < len(concurrentByDepth) {
			concurrentByDepth = concurrentByDepth[:step.Depth]
		}
		concurrent := false
		for _, ancestorConcurrent := range concurrentByDepth {
			concurrent = concurrent || ancestorConcurrent
		}
		if concurrent && step.Kind == string(schema.StepTypeHandoff) {
			*errs = append(*errs, validationErr(
				step.ID, "handoff", "", errors.New("handoff cannot execute beneath a concurrent ancestor"),
			))
		}
		currentConcurrent := step.Kind == "parallel"
		if iterate, ok := step.Spec.(*schema.IterateNode); ok && iterate != nil && iterate.Concurrency > 1 {
			currentConcurrent = true
		}
		if step.Depth == len(concurrentByDepth) {
			concurrentByDepth = append(concurrentByDepth, concurrent || currentConcurrent)
		}
	}
	for _, step := range plan.Steps {
		validateHandoffConcurrencySpec(step.Spec, false, step.ID, errs)
	}
}

func validateHandoffConcurrencySpec(
	spec engine.StepSpec,
	concurrent bool,
	path string,
	errs *[]engine.PlanValidationError,
) {
	switch typed := spec.(type) {
	case *schema.IncludeSpec:
		if typed != nil {
			validateHandoffConcurrencyFlow(typed.ResolvedSteps, concurrent, path+"/include", errs)
		}
	case *schema.BranchSpec:
		if typed != nil {
			for index, arm := range typed.Branches {
				validateHandoffConcurrencyFlow(arm.Steps, concurrent, fmt.Sprintf("%s/branch:%d", path, index), errs)
			}
		}
	case *schema.IterateNode:
		if typed != nil {
			validateHandoffConcurrencyFlow(typed.Steps, concurrent || typed.Concurrency > 1, path+"/iterate", errs)
		}
	case *schema.ParallelNode:
		if typed != nil {
			for index, branch := range typed.Branches {
				validateHandoffConcurrencyFlow(branch.Steps, true, fmt.Sprintf("%s/parallel:%d", path, index), errs)
			}
		}
	case *schema.CompensateSpec:
		if typed != nil {
			validateHandoffConcurrencyFlow(typed.Compensate.Steps, concurrent, path+"/compensate", errs)
		}
	}
}

func validateHandoffConcurrencyFlow(
	nodes []schema.FlowNode,
	concurrent bool,
	path string,
	errs *[]engine.PlanValidationError,
) {
	for index, node := range nodes {
		nodePath := fmt.Sprintf("%s/node:%d", path, index)
		switch {
		case node.Step != nil:
			if concurrent && node.Step.Type == schema.StepTypeHandoff {
				*errs = append(*errs, validationErr(
					node.Step.ID, "handoff", "", errors.New("handoff cannot execute beneath a concurrent ancestor"),
				))
			}
			if spec, _, err := specFromValidationFlowStep(node.Step); err == nil {
				validateHandoffConcurrencySpec(spec, concurrent, nodePath+"/"+node.Step.ID, errs)
			}
		case node.Iterate != nil:
			validateHandoffConcurrencySpec(node.Iterate, concurrent, nodePath+"/"+node.Iterate.ID, errs)
		case node.Parallel != nil:
			validateHandoffConcurrencySpec(node.Parallel, true, nodePath+"/"+node.Parallel.ID, errs)
		}
	}
}

func specFromValidationFlowStep(step *schema.Step) (engine.StepSpec, string, error) {
	if step == nil {
		return nil, "", errors.New("step is required")
	}
	switch step.Type {
	case schema.StepTypeInclude:
		return step.IncludeSpec, "include", nil
	case schema.StepTypeBranch:
		return step.BranchSpec, "branch", nil
	case schema.StepTypeParallel:
		return step.ParallelSpec, "parallel", nil
	case schema.StepTypeCompensate:
		return step.CompensateSpec, "compensate", nil
	default:
		return nil, "", nil
	}
}

// ValidateExecutionPlan compiles expression and capture metadata for a
// synthetic execution plan, such as a branch or include sub-engine plan.
func ValidateExecutionPlan(plan *engine.ExecutionPlan) error {
	_, err := validatePlan(plan)
	return err
}

func validateStep(vp *engine.ValidatedPlan, errs *[]engine.PlanValidationError, step engine.ResolvedStep, stepIDs map[string]struct{}) {
	addGIS(vp, errs, step.ID, "title", step.Name)
	addGXL(vp, errs, step.ID, "when", step.When)
	if len(step.Capture) > 0 {
		keys := sortedKeys(step.Capture)
		for _, name := range keys {
			ref := fmt.Sprintf("capture.%s", name)
			path := step.Capture[name]
			if step.Kind == string(schema.StepTypeInclude) && !strings.HasPrefix(path, "outputs.") || step.Kind == string(schema.StepTypeNoop) {
				addGIS(vp, errs, step.ID, ref, path)
				continue
			}
			parsed, ok := addGCP(vp, errs, step.ID, ref, path)
			if ok && parsed.Source.Kind == gdp.SourceStep {
				if _, exists := stepIDs[parsed.Source.StepID]; !exists {
					*errs = append(*errs, validationErr(step.ID, ref, path, errkit.New("GCP-PARSE-006", fmt.Sprintf("undefined step %q", parsed.Source.StepID))))
				}
			}
			if ok && parsed.Source.Kind == gdp.SourceOutputs {
				if !stepBindsCaptureOutput(vp, step, parsed.Source.Field) {
					*errs = append(*errs, validationErr(step.ID, ref, path, errkit.New("GCP-PARSE-001",
						fmt.Sprintf("outputs.%s is only valid in the capture: block of a bound tool action or host_action status output", parsed.Source.Field))))
				}
			}
		}
	}

	switch spec := step.Spec.(type) {
	case *schema.CLISpec:
		if spec == nil {
			break
		}
		addGIS(vp, errs, step.ID, "command", spec.Command)
		for i, arg := range spec.Args {
			addGIS(vp, errs, step.ID, fmt.Sprintf("args[%d]", i), arg)
		}
		addGIS(vp, errs, step.ID, "stdin", spec.Stdin)
		for _, k := range sortedKeys(spec.Env) {
			addGIS(vp, errs, step.ID, "env."+k, spec.Env[k])
		}
		if s, ok := spec.Run.(string); ok {
			addGIS(vp, errs, step.ID, "run", s)
		}
		if m, ok := spec.Run.(map[string]string); ok {
			for _, k := range sortedKeys(m) {
				addGIS(vp, errs, step.ID, "run."+k, m[k])
			}
		}
	case *schema.ToolCallSpec:
		if spec == nil {
			break
		}
		validateAnyGIS(vp, errs, step.ID, "tool.args", spec.Tool.Args)
	case *schema.HostActionSpec:
		if spec == nil {
			break
		}
		validateAnyGIS(vp, errs, step.ID, "host_action.request", spec.HostAction.Request)
	case *schema.HandoffSpec:
		if spec == nil {
			break
		}
		for _, key := range sortedKeys(spec.Handoff.With) {
			addGIS(vp, errs, step.ID, "handoff.with."+key, spec.Handoff.With[key])
		}
		for _, key := range sortedKeys(spec.Handoff.Facts) {
			addGIS(vp, errs, step.ID, "handoff.facts."+key, spec.Handoff.Facts[key])
		}
	case *schema.IncludeSpec:
		if spec == nil {
			break
		}
		addGXL(vp, errs, step.ID, "include.when", spec.Include.When)
		for _, k := range sortedKeys(spec.Include.With) {
			addGIS(vp, errs, step.ID, "include.with."+k, spec.Include.With[k])
		}
	case *schema.CollectorSpec:
		if spec == nil {
			break
		}
		addGIS(vp, errs, step.ID, "prompt", spec.Prompt)
		for i, field := range spec.Fields {
			base := fmt.Sprintf("collector.fields[%d]", i)
			addGXL(vp, errs, step.ID, base+".when", field.When)
			addGIS(vp, errs, step.ID, base+".label", field.Label)
			addGIS(vp, errs, step.ID, base+".hint", field.Hint)
			if s, ok := field.Default.(string); ok {
				addGIS(vp, errs, step.ID, base+".default", s)
			}
		}
	case *schema.BranchSpec:
		if spec == nil {
			break
		}
		for i, arm := range spec.Branches {
			addGXL(vp, errs, step.ID, fmt.Sprintf("branches[%d].condition", i), arm.Condition)
		}
	case *schema.IterateNode:
		if spec == nil {
			break
		}
		addGXL(vp, errs, step.ID, "until", spec.Until)
		addGIS(vp, errs, step.ID, "over", spec.Over)
		validateAnyGIS(vp, errs, step.ID, "collect_values", spec.CollectValues)
	case *schema.AssertSpec:
		if spec == nil {
			break
		}
		for i, a := range spec.Assert {
			addGIS(vp, errs, step.ID, fmt.Sprintf("assert[%d].subject", i), a.Subject)
			addGIS(vp, errs, step.ID, fmt.Sprintf("assert[%d].expected", i), a.Expected)
		}
	case *schema.DisplaySpec:
		if spec == nil {
			break
		}
		addGIS(vp, errs, step.ID, "display.content", spec.Display.Content)
	case *schema.WaitForEventSpec:
		if spec == nil {
			break
		}
		addGIS(vp, errs, step.ID, "event.id", spec.Event.ID)
		for _, k := range sortedKeys(spec.Event.Filter) {
			addGIS(vp, errs, step.ID, "event.filter."+k, spec.Event.Filter[k])
		}
	}
}

func stepBindsCaptureOutput(vp *engine.ValidatedPlan, step engine.ResolvedStep, outputName string) bool {
	if include, ok := step.Spec.(*schema.IncludeSpec); ok {
		if include.Include.IsDynamic() || include.LazyRunbookPath != "" {
			return outputName != ""
		}
		_, exists := include.ResolvedOutputs[outputName]
		return exists && outputName != ""
	}
	if step.Kind == string(schema.StepTypeHostAction) {
		return outputName == "status" || outputName == "result"
	}
	spec, ok := step.Spec.(*schema.ToolCallSpec)
	if !ok || vp == nil || vp.Source == nil {
		return false
	}
	toolDef := stepToolDefinition(vp.Source, step, spec)
	if toolDef == nil {
		return false
	}
	action, ok := toolDef.Actions[spec.Tool.Action]
	if !ok || action == nil {
		return false
	}
	outputs := internaltool.OutputsWithQueryResult(action.Outputs, action.Result)
	if outputName == "" {
		return len(outputs) > 0
	}
	_, ok = outputs[outputName]
	return ok
}

func addGXL(vp *engine.ValidatedPlan, errs *[]engine.PlanValidationError, stepID, field, src string) {
	if strings.TrimSpace(src) == "" {
		return
	}
	ast, err := gxlparser.Parse(src)
	if err != nil {
		*errs = append(*errs, validationErr(stepID, field, src, err))
		return
	}
	vp.GXLExprs[engine.StepRef{StepID: stepID, FieldPath: field}] = ast
}

func addGIS(vp *engine.ValidatedPlan, errs *[]engine.PlanValidationError, stepID, field, src string) {
	if src == "" {
		return
	}
	tmpl, err := gis.Parse(src)
	if err != nil {
		*errs = append(*errs, validationErr(stepID, field, src, err))
		return
	}
	vp.GISTemplates[engine.StepRef{StepID: stepID, FieldPath: field}] = tmpl
}

func addGCP(vp *engine.ValidatedPlan, errs *[]engine.PlanValidationError, stepID, field, src string) (*gcpparser.Path, bool) {
	if strings.TrimSpace(src) == "" {
		return nil, false
	}
	path, err := gcpparser.Parse(src)
	if err != nil {
		*errs = append(*errs, validationErr(stepID, field, src, err))
		return nil, false
	}
	vp.GCPPaths[engine.StepRef{StepID: stepID, FieldPath: field}] = path
	return path, true
}

func validateAnyGIS(vp *engine.ValidatedPlan, errs *[]engine.PlanValidationError, stepID, field string, v any) {
	switch x := v.(type) {
	case string:
		addGIS(vp, errs, stepID, field, x)
	case []any:
		for i, item := range x {
			validateAnyGIS(vp, errs, stepID, fmt.Sprintf("%s[%d]", field, i), item)
		}
	case []string:
		for i, item := range x {
			addGIS(vp, errs, stepID, fmt.Sprintf("%s[%d]", field, i), item)
		}
	case map[string]any:
		for _, k := range sortedAnyKeys(x) {
			validateAnyGIS(vp, errs, stepID, field+"."+k, x[k])
		}
	}
}

func validationErr(stepID, field, src string, err error) engine.PlanValidationError {
	out := engine.PlanValidationError{StepID: stepID, FieldPath: field, Source: src, Code: "PLAN-001", Err: err}
	var coder errkit.Coder
	if errors.As(err, &coder) {
		out.Code = coder.Code()
	}
	var classer errkit.Classer
	if errors.As(err, &classer) {
		out.Class = classer.Class()
	}
	return out
}

func planHash(plan *engine.ExecutionPlan) string {
	if plan.Metadata.PlanHash != "" {
		return plan.Metadata.PlanHash
	}
	if plan.RunbookPath != "" {
		if b, err := os.ReadFile(plan.RunbookPath); err == nil {
			sum := sha256.Sum256(b)
			return hex.EncodeToString(sum[:])
		}
	}
	b, _ := json.Marshal(plan)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func sortedAnyKeys(m map[string]any) []string { return sortedKeys(m) }
