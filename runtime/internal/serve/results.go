package serve

import (
	"context"
	"encoding/json"

	"github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	"github.com/ormasoftchile/yawr/runtime/internal/resultsdelivery"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func (s *Server) addPublicResults(ctx context.Context, response map[string]any, record *engine.RunResults, recordErr error, status engine.RunStatus, plan *engine.ExecutionPlan, vars map[string]any, runID string, typed bool) {
	response["results"] = nil
	if plan == nil {
		protectionRequired := typed || record != nil || recordErr != nil
		if store, ok := s.store.(engine.DurableRunStore); ok {
			// Unpublished runs need the same frozen protection as published runs.
			protectionRequired = true
			frozen, err := store.LoadPlan(ctx, runID)
			if err == nil {
				plan = frozen
			}
		}
		if plan == nil && protectionRequired {
			response["vars"] = nil
			response["vars_unavailable"] = resultsdelivery.Missing("protection-unavailable")
			response["results_unavailable"] = resultsdelivery.Missing("protection-unavailable")
			return
		}
	}
	protection := engine.DebugProtection{}
	if plan != nil {
		protection = engine.ExtendDebugProtection(protection, vars, plan.Inputs, plan.Governance)
		invocation := typed || plan.Bindings != nil || record != nil
		for _, step := range plan.Steps {
			invocation = invocation || step.Kind == "results"
		}
		if invocation && (len(protection.ProtectedVars) > 0 || len(protection.RedactionPatterns) > 0) {
			if err := debugprotect.ValidateHandoffValues(protection, vars, nil); err != nil {
				response["vars"] = nil
				response["vars_unavailable"] = &resultsdelivery.Unavailable{Status: "redacted", Reason: "protected-content"}
			}
		}
	}
	body, unavailable := resultsdelivery.Prepare(record, recordErr, status, protection)
	if unavailable != nil {
		response["results_unavailable"] = unavailable
		return
	}
	if entry, ok := s.registry.Get(runID); ok {
		if validator, ok := entry.Handle.(interface{ ValidateResultsDelivery(json.RawMessage) error }); ok {
			if err := validator.ValidateResultsDelivery(body); err != nil {
				response["results_unavailable"] = &resultsdelivery.Unavailable{Status: "redacted", Reason: "protected-content"}
				return
			}
		}
	}
	response["results"] = body
}
