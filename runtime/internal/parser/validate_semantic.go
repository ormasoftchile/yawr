package parser

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	parserPkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	regionsval "github.com/ormasoftchile/yawr/runtime/pkg/schema/regions/validator"
	"github.com/ormasoftchile/yawr/runtime/pkg/sensitive"
)

// validateSemantic performs cross-field semantic validation on a parsed
// runbook. It returns a ValidationErrors slice (nil if no errors) plus any
// non-fatal parse warnings (e.g. ENUM-W001) that MUST be surfaced to the
// caller but MUST NOT fail the parse.
func validateSemantic(rb *schema.Runbook, p platform.Platform) (ValidationErrors, []parserPkg.ParseWarning) {
	var errs ValidationErrors
	var warnings []parserPkg.ParseWarning
	if err := schema.ValidateTypedRunbook(rb); err != nil {
		errs = append(errs, verr("typed/admission", "bindings", err.Error()))
	}

	// Collect all step IDs and check for uniqueness.
	ids := make(map[string]int) // id → count
	collectStepIDs(rb.Flow, ids)

	for id, count := range ids {
		if id == "" {
			errs = append(errs, verr("step/empty-id", "flow", "step has empty id"))
			continue
		}
		if count > 1 {
			errs = append(errs, verr("step/duplicate-id", "flow."+id,
				fmt.Sprintf("step id %q appears %d times; step IDs must be unique within a runbook", id, count)))
		}
	}

	// Walk flow to validate step-level semantic rules.
	walkErrs := walkFlowNodes(rb.Flow, false, p, rb)
	errs = append(errs, walkErrs...)
	for name, output := range rb.Outputs {
		if output != nil && output.ValueExpr != "" {
			if _, err := gxlparser.Parse(output.ValueExpr); err != nil {
				errs = append(errs, verr("output/value-expr", "outputs."+name+".value_expr", err.Error()))
			}
		}
	}

	// Region manifest validation (no-op when rb.Regions is nil).
	for _, re := range regionsval.Validate(rb) {
		field := "regions"
		if re.RegionID != "" {
			field = "regions." + re.RegionID
		}
		if re.NodeID != "" {
			field += "." + re.NodeID
		}
		errs = append(errs, verr("regions/"+re.Code, field, re.Message))
	}

	// enum well-formedness/type-site rules at S3 (runbook inputs.<name>)
	// and S4 (runbook outputs.<name>): AR-ENUM-2 (ENUM-001), AR-ENUM-3
	// rules 5-7 (ENUM-003/004/005), AR-ENUM-4 (ENUM-W001). ENUM-002
	// (structural malformation) is already enforced earlier, either by
	// Phase 1's JSON Schema `enum` keyword (validate_structural.go's
	// enumAwareCode) or -- for any shape the JSON Schema layer cannot
	// itself express -- by schema.EnumConstraint's own UnmarshalYAML,
	// which already ran during the struct-decode step that produced rb.
	enumErrs, enumWarnings := validateEnumDeclarations(rb)
	errs = append(errs, enumErrs...)
	warnings = append(warnings, enumWarnings...)

	// Normalize path fields on all steps.
	normalizePathFields(rb.Flow, p)

	return errs, warnings
}

// walkFlowNodes recursively validates FlowNodes.
// inParallel is true when we are already inside a parallel branch (used for nested parallel detection).
func walkFlowNodes(nodes []schema.FlowNode, inParallel bool, p platform.Platform, rb *schema.Runbook) ValidationErrors {
	var errs ValidationErrors
	for i, fn := range nodes {
		switch {
		case fn.Step != nil:
			errs = append(errs, validateStep(fn.Step, inParallel, i, p, rb)...)
		case fn.Iterate != nil:
			for key, value := range fn.Iterate.CollectValues {
				if _, err := schema.TypedTreeReferences(value); err != nil {
					errs = append(errs, verr("iterate/collect-values", "flow."+fn.Iterate.ID+".collect_values."+key, err.Error()))
				}
				if _, duplicate := fn.Iterate.Collect[key]; duplicate {
					errs = append(errs, verr("iterate/duplicate-collection", "flow."+fn.Iterate.ID,
						fmt.Sprintf("%q is declared in both collect and collect_values", key)))
				}
			}
			errs = append(errs, walkFlowNodes(fn.Iterate.Steps, inParallel || fn.Iterate.Concurrency > 1, p, rb)...)
		case fn.Parallel != nil:
			if inParallel {
				errs = append(errs, verr("parallel/nested-forbidden",
					fmt.Sprintf("flow[%d](parallel id=%q)", i, fn.Parallel.ID),
					"a parallel node must not appear inside a parallel branch"))
			}
			errs = append(errs, validateParallelNode(fn.Parallel, i, p, rb)...)
		}
	}
	return errs
}

// validateStep validates a single step's semantic rules.
func validateStep(s *schema.Step, inParallel bool, idx int, p platform.Platform, rb *schema.Runbook) ValidationErrors {
	var errs ValidationErrors
	loc := fmt.Sprintf("flow[%d](id=%q)", idx, s.ID)

	// Nested parallel forbidden.
	if s.Type == schema.StepTypeParallel && inParallel {
		errs = append(errs, verr("parallel/nested-forbidden", loc,
			"a parallel step must not appear inside a parallel branch"))
	}

	// Validate type-specific rules.
	switch s.Type {
	case schema.StepTypeInclude:
		errs = append(errs, validateIncludeStep(s, loc)...)

	case schema.StepTypeWaitForEvent:
		if s.WaitForEventSpec != nil && s.WaitForEventSpec.Event.Source == schema.EventSourceSignal {
			if s.WaitForEventSpec.Event.ID != "" {
				allowed := p.AllowedSignals()
				if !containsSignal(allowed, s.WaitForEventSpec.Event.ID) {
					errs = append(errs, verr("signal/unsupported", loc+".event.id",
						fmt.Sprintf("signal %q is not in the OS allow-list %v", s.WaitForEventSpec.Event.ID, allowed)))
				}
			}
		}

	case schema.StepTypeBranch:
		if s.BranchSpec != nil && len(s.BranchSpec.Branches) == 0 {
			errs = append(errs, verr("branch/no-arms", loc,
				"branch step must have at least one arm in 'branches'"))
		}
		if s.BranchSpec != nil {
			fallbackCount := 0
			for armIndex, arm := range s.BranchSpec.Branches {
				armLoc := fmt.Sprintf("%s.branches[%d]", loc, armIndex)
				condition := strings.TrimSpace(arm.Condition)
				if arm.Else {
					fallbackCount++
					if condition != "" {
						errs = append(errs, verr("branch/fallback-condition", armLoc,
							"fallback arm cannot also declare 'condition'"))
					}
				} else if condition == "" {
					errs = append(errs, verr("branch/missing-condition", armLoc,
						"non-fallback branch arm requires 'condition'"))
				}
			}
			if fallbackCount > 1 {
				errs = append(errs, verr("branch/multiple-fallbacks", loc,
					"branch step may have at most one fallback arm"))
			}
		}

	case schema.StepTypeApprove:
		if s.ApproveSpec != nil {
			ap := s.ApproveSpec.Approvals
			if len(ap.Roles) == 0 && len(ap.Pool) == 0 {
				errs = append(errs, verr("approve/no-reviewers", loc+".approvals",
					"approve step must have at least one role or pool member in 'approvals'"))
			}
			// timeout and timeout_business_days are mutually exclusive.
			if s.Timeout != "" && s.ApproveSpec.TimeoutBusinessDays > 0 {
				errs = append(errs, verr("approve/timeout-conflict", loc,
					"'timeout' and 'timeout_business_days' are mutually exclusive"))
			}
			// timeout_business_days requires timezone.
			if s.ApproveSpec.TimeoutBusinessDays > 0 && s.ApproveSpec.Timezone == "" {
				errs = append(errs, verr("approve/missing-timezone", loc,
					"'timeout_business_days' requires 'timezone'"))
			}
		}

	case schema.StepTypeCollector:
		if s.CollectorSpec != nil && len(s.CollectorSpec.Fields) == 0 {
			errs = append(errs, verr("collector/no-fields", loc,
				"collector step must have at least one entry in 'fields'"))
		}

	case schema.StepTypeHostAction:
		errs = append(errs, validateHostActionStep(s, loc)...)

	case schema.StepTypeDecision:
		if s.DecisionSpec != nil {
			for j, route := range s.DecisionSpec.Routes {
				if route.Goto != "" {
					if !stepIDExists(rb.Flow, route.Goto) {
						errs = append(errs, verr("decision/invalid-goto",
							fmt.Sprintf("%s.routes[%d].goto", loc, j),
							fmt.Sprintf("goto target %q does not reference a valid step id", route.Goto)))
					}

				}
			}
		}

	case schema.StepTypeParallel:
		if inParallel {
			// Already reported above.
			break
		}
		// Walk branches.
		if s.ParallelSpec != nil {
			for branchIndex, branch := range s.ParallelSpec.Branches {
				branchErrs := walkFlowNodes(branch.Steps, true, p, rb)
				for _, e := range branchErrs {
					e.Field = fmt.Sprintf("%s.branches[%d].steps → %s", loc, branchIndex, e.Field)
					errs = append(errs, e)
				}
			}
		}
		// Don't fall through to walk branches again below.
		return errs

	case schema.StepTypeHandoff:
		if inParallel {
			errs = append(errs, verr(
				"handoff/parallel-forbidden", loc,
				"handoff steps are not allowed inside parallel branches",
			))
		}
		errs = append(errs, validateHandoffStep(s, loc, rb)...)
	}

	// Walk nested steps for branch/compensate/parallel (not parallel which returned early).
	if s.BranchSpec != nil && s.Type == schema.StepTypeBranch {
		for _, arm := range s.BranchSpec.Branches {
			errs = append(errs, walkFlowNodes(arm.Steps, inParallel, p, rb)...)
		}
	}
	if s.CompensateSpec != nil {
		errs = append(errs, walkFlowNodes(s.CompensateSpec.Compensate.Steps, inParallel, p, rb)...)
	}

	return errs
}

func validateHandoffStep(s *schema.Step, loc string, rb *schema.Runbook) ValidationErrors {
	if s.HandoffSpec == nil {
		return ValidationErrors{verr("handoff/missing", loc+".handoff", "handoff step requires a handoff block")}
	}
	handoff := s.HandoffSpec.Handoff
	var errs ValidationErrors
	if !schema.IsStaticHandoffTarget(handoff.Runbook) {
		errs = append(errs, verr("handoff/invalid-target", loc+".handoff.runbook", "handoff runbook must be a static relative .runbook.yaml or .yawr path"))
	}
	if strings.TrimSpace(handoff.Reason.Code) == "" || strings.TrimSpace(handoff.Reason.Summary) == "" {
		errs = append(errs, verr("handoff/invalid-reason", loc+".handoff.reason", "handoff reason code and summary are required"))
	}
	for field, expression := range handoff.With {
		if strings.TrimSpace(field) == "" || sensitive.Name(field) {
			errs = append(errs, verr("handoff/invalid-binding", loc+".handoff.with."+field, "handoff bindings require non-sensitive target input names"))
		}
		if source, ok := schema.HandoffBindingSource(expression); !ok || handoffSourceIsSensitive(source, rb) {
			errs = append(errs, verr("handoff/invalid-binding-source", loc+".handoff.with."+field, "handoff bindings require exact non-secret variable references"))
		}
	}
	for fact, expression := range handoff.Facts {
		if strings.TrimSpace(fact) == "" || sensitive.Name(fact) {
			errs = append(errs, verr("handoff/invalid-fact", loc+".handoff.facts."+fact, "handoff facts require non-sensitive names"))
		}
		if source, ok := schema.HandoffBindingSource(expression); !ok || handoffSourceIsSensitive(source, rb) {
			errs = append(errs, verr("handoff/invalid-fact-source", loc+".handoff.facts."+fact, "handoff facts require exact non-secret variable references"))
		}
	}
	return errs
}

func handoffSourceIsSensitive(source string, rb *schema.Runbook) bool {
	segments := strings.Split(source, ".")
	if len(segments) == 0 || segments[0] == "vars" {
		return true
	}
	for _, segment := range segments {
		if sensitive.Name(segment) {
			return true
		}
	}
	root := segments[0]
	if rb == nil {
		return false
	}
	if declaration := rb.Inputs[root]; declaration != nil && declaration.Type == "secret" {
		return true
	}
	if declaration := rb.Outputs[root]; declaration != nil && declaration.Type == "secret" {
		return true
	}
	return false
}

func cleanRelativePath(value string) string {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return clean
}

func validateHostActionStep(s *schema.Step, loc string) ValidationErrors {
	if s.HostActionSpec == nil {
		return ValidationErrors{verr("host-action/missing-request", loc+".host_action", "host_action step requires a host_action request")}
	}
	if strings.TrimSpace(s.HostActionSpec.HostAction.Capability) == "" {
		return ValidationErrors{verr("host-action/invalid-request", loc+".host_action.capability", "host_action capability is required")}
	}
	return nil
}

// validateParallelNode validates a top-level ParallelNode flow element.
func validateParallelNode(pn *schema.ParallelNode, idx int, p platform.Platform, rb *schema.Runbook) ValidationErrors {
	var errs ValidationErrors
	loc := fmt.Sprintf("flow[%d](parallel id=%q)", idx, pn.ID)

	if len(pn.Branches) == 0 {
		errs = append(errs, verr("parallel/no-branches", loc,
			"parallel node must have at least one branch"))
	}

	for i, branch := range pn.Branches {
		branchErrs := walkFlowNodes(branch.Steps, true, p, rb)
		for _, e := range branchErrs {
			e.Field = fmt.Sprintf("%s.branches[%d] → %s", loc, i, e.Field)
			errs = append(errs, e)
		}
	}
	return errs
}

// collectStepIDs gathers all step IDs in a flow into the provided map (id → count).
func collectStepIDs(nodes []schema.FlowNode, ids map[string]int) {
	for _, fn := range nodes {
		switch {
		case fn.Step != nil:
			ids[fn.Step.ID]++
			if fn.Step.BranchSpec != nil {
				for _, arm := range fn.Step.BranchSpec.Branches {
					collectStepIDs(arm.Steps, ids)
				}
			}
			if fn.Step.CompensateSpec != nil {
				collectStepIDs(fn.Step.CompensateSpec.Compensate.Steps, ids)
			}
		case fn.Iterate != nil:
			ids[fn.Iterate.ID]++
			collectStepIDs(fn.Iterate.Steps, ids)
		case fn.Parallel != nil:
			ids[fn.Parallel.ID]++
			for _, b := range fn.Parallel.Branches {
				collectStepIDs(b.Steps, ids)
			}
		}
	}
}

// stepIDExists checks whether a step ID is defined anywhere in the flow.
func stepIDExists(nodes []schema.FlowNode, id string) bool {
	for _, fn := range nodes {
		switch {
		case fn.Step != nil:
			if fn.Step.ID == id {
				return true
			}
			if fn.Step.BranchSpec != nil {
				for _, arm := range fn.Step.BranchSpec.Branches {
					if stepIDExists(arm.Steps, id) {
						return true
					}
				}
			}
			if fn.Step.CompensateSpec != nil {
				if stepIDExists(fn.Step.CompensateSpec.Compensate.Steps, id) {
					return true
				}
			}
		case fn.Iterate != nil:
			if fn.Iterate.ID == id {
				return true
			}
			if stepIDExists(fn.Iterate.Steps, id) {
				return true
			}
		case fn.Parallel != nil:
			if fn.Parallel.ID == id {
				return true
			}
			for _, b := range fn.Parallel.Branches {
				if stepIDExists(b.Steps, id) {
					return true
				}
			}
		}
	}
	return false
}

// normalizePathFields walks the flow and converts backslashes to forward slashes
// in all path-like fields (workdir, include.runbook).
func normalizePathFields(nodes []schema.FlowNode, p platform.Platform) {
	for i := range nodes {
		fn := &nodes[i]
		if fn.Step != nil {
			s := fn.Step
			if s.CLI != nil && s.CLI.Workdir != "" {
				s.CLI.Workdir = p.NormalizePath(s.CLI.Workdir)
			}
			if s.IncludeSpec != nil {
				s.IncludeSpec.Include.Runbook = p.NormalizePath(s.IncludeSpec.Include.Runbook)
			}
			if s.BranchSpec != nil {
				for j := range s.BranchSpec.Branches {
					normalizePathFields(s.BranchSpec.Branches[j].Steps, p)
				}
			}
			if s.CompensateSpec != nil {
				normalizePathFields(s.CompensateSpec.Compensate.Steps, p)
			}
		} else if fn.Iterate != nil {
			normalizePathFields(fn.Iterate.Steps, p)
		} else if fn.Parallel != nil {
			for j := range fn.Parallel.Branches {
				normalizePathFields(fn.Parallel.Branches[j].Steps, p)
			}
		}
	}
}

// containsSignal checks if name is in the allowed set (case-insensitive).
func containsSignal(allowed []string, name string) bool {
	upper := strings.ToUpper(name)
	for _, s := range allowed {
		if strings.ToUpper(s) == upper {
			return true
		}
	}
	return false
}

// validateEnumDeclarations runs the AR-ENUM-1..15 well-formedness/type-site
// gate over every S3 (runbook inputs.<name>) and S4 (runbook
// outputs.<name>) enum-constrained declaration in rb. Deterministic
// (sorted-name) iteration order matches every other semantic-validation
// pass in this file. Fatal errors (ENUM-001/003/004/005) are returned
// alongside any non-fatal ENUM-W001 warnings, kept in separate slices so
// callers never conflate the two.
func validateEnumDeclarations(rb *schema.Runbook) (ValidationErrors, []parserPkg.ParseWarning) {
	var errs ValidationErrors
	var warnings []parserPkg.ParseWarning
	for _, name := range sortedInputNames(rb.Inputs) {
		in := rb.Inputs[name]
		if in == nil || len(in.Enum) == 0 {
			continue
		}
		resolvedType := in.Type
		if resolvedType == "" {
			// AR-ENUM-2: §Input Declarations makes string the default
			// type when absent.
			resolvedType = "string"
		}
		appendEnumErrs(&errs, &warnings, "inputs."+name, resolvedType, in.Enum)
	}
	for _, name := range sortedOutputNames(rb.Outputs) {
		out := rb.Outputs[name]
		if out == nil || len(out.Enum) == 0 {
			continue
		}
		appendEnumErrs(&errs, &warnings, "outputs."+name, out.Type, out.Enum)
	}
	return errs, warnings
}

func appendEnumErrs(errs *ValidationErrors, warnings *[]parserPkg.ParseWarning, field, resolvedType string, enum schema.EnumConstraint) {
	fatal, fWarnings := schema.ValidateDeclaration(resolvedType, enum)
	if fatal != nil {
		*errs = append(*errs, verr(codeOf(fatal), field, fatal.Error()))
		return
	}
	for _, w := range fWarnings {
		*warnings = append(*warnings, parserPkg.ParseWarning{Field: field, Message: fmt.Sprintf("[%s] %s", codeOf(w), w.Error())})
	}
}

// codeOf extracts the machine-readable code from an errkit-typed error.
func codeOf(err error) string {
	if c, ok := err.(interface{ Code() string }); ok {
		return c.Code()
	}
	return "ENUM-000"
}

func sortedInputNames(m map[string]*schema.Input) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedOutputNames(m map[string]*schema.Output) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateIncludeStep enforces semantic rules on include steps beyond what the
// JSON Schema structural pass can express.
//
// Rules:
//  1. Exactly one of runbook / runbook_ref must be set (mutual exclusion).
//  2. resolve_from is required when runbook_ref is present and must equal "catalog".
//  3. on_not_found is only valid on dynamic sites (runbook_ref present).
//  4. resolve_from must not appear on static sites (runbook present).
func validateIncludeStep(s *schema.Step, loc string) ValidationErrors {
	if s.IncludeSpec == nil {
		return nil
	}
	inc := &s.IncludeSpec.Include
	var errs ValidationErrors

	hasStatic := inc.Runbook != ""
	hasDynamic := inc.RunbookRef != ""
	// intendedDynamic catches a literal empty runbook_ref: "" — the presence of
	// resolve_from or on_not_found is unambiguous evidence the author chose the
	// dynamic arm, so we don't fire "missing-target" and instead let the empty
	// value produce DINC-002 (parse-time for static literals, execution-time for
	// template refs).
	intendedDynamic := hasDynamic || inc.ResolveFrom != "" || inc.OnNotFound != ""

	switch {
	case hasStatic && hasDynamic:
		errs = append(errs, verr("include/ambiguous-target", loc+".include",
			"'runbook' and 'runbook_ref' are mutually exclusive; set exactly one"))
		return errs
	case !hasStatic && !hasDynamic && intendedDynamic:
		// Dynamic arm intended (resolve_from/on_not_found present) but runbook_ref
		// is explicitly empty. Emit DINC-002 here so the error code is correct
		// regardless of whether a runtime path ever reaches ValidateRenderedRef.
		errs = append(errs, verr("DINC-002", loc+".include.runbook_ref",
			"dynamic include: 'runbook_ref' is empty; catalog lookup cannot proceed"))
		return errs
	case !hasStatic && !hasDynamic:
		errs = append(errs, verr("include/missing-target", loc+".include",
			"include step must have exactly one of 'runbook' or 'runbook_ref'"))
		return errs // no further checks possible
	}

	if hasDynamic {
		// resolve_from required and must be "catalog".
		if inc.ResolveFrom == "" {
			errs = append(errs, verr("include/missing-resolve-from", loc+".include.resolve_from",
				"'resolve_from' is required when 'runbook_ref' is present"))
		} else if inc.ResolveFrom != schema.ResolveFromCatalog {
			errs = append(errs, verr("include/invalid-resolve-from", loc+".include.resolve_from",
				fmt.Sprintf("'resolve_from' must be %q; got %q", schema.ResolveFromCatalog, inc.ResolveFrom)))
		}
		// on_not_found must be one of the two legal values (schema enforces enum but
		// an unmarshal-only path may bypass schema; double-check here).
		if inc.OnNotFound != "" &&
			inc.OnNotFound != schema.OnNotFoundFail &&
			inc.OnNotFound != schema.OnNotFoundContinue {
			errs = append(errs, verr("include/invalid-on-not-found", loc+".include.on_not_found",
				fmt.Sprintf("'on_not_found' must be %q or %q; got %q",
					schema.OnNotFoundFail, schema.OnNotFoundContinue, inc.OnNotFound)))
		}
	}

	if hasStatic {
		// resolve_from and on_not_found are meaningless on static sites.
		if inc.ResolveFrom != "" {
			errs = append(errs, verr("include/static-resolve-from", loc+".include.resolve_from",
				"'resolve_from' is only valid when 'runbook_ref' is present"))
		}
		if inc.OnNotFound != "" {
			errs = append(errs, verr("include/static-on-not-found", loc+".include.on_not_found",
				"'on_not_found' is only valid when 'runbook_ref' is present"))
		}
	}

	// expand is rejected on dynamic sites.
	if hasDynamic && inc.Expand != "" {
		errs = append(errs, verr("include/dynamic-expand-forbidden", loc+".include.expand",
			"'expand' must not be set when 'runbook_ref' is present; dynamic includes are always resolved at execution time"))
	}

	return errs
}
