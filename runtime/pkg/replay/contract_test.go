package replay

import (
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestReviewedBoundaryContractRoundTripsExactSelector(t *testing.T) {
	binding := StepBinding{
		At:   Selector{CallPath: []string{"include", "branch"}, Step: "query", Phase: "execute", Invocation: 2, Attempt: 1},
		Kind: "tool", Status: "completed", Outcome: "success",
		Output: map[string]any{"count": float64(3)},
		Source: Source{Kind: "prior-run", RunID: "run-one", InteractionID: "occurrence-two"},
		Review: Review{State: "reviewed", ReviewedBy: "operator", ReviewedAt: "2026-08-31T12:00:00Z", SensitivityReviewed: true},
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var restored StepBinding
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(restored.At.CallPath) != 2 || restored.At.Step != "query" ||
		restored.At.Invocation != 2 || restored.Review.ReviewedBy != "operator" {
		t.Fatalf("restored binding = %#v", restored)
	}
}

func TestDependencyLockForPlanCanonicalizesAbsentGraphAndCatalog(t *testing.T) {
	lock, err := DependencyLockForPlan(&engine.ExecutionPlan{
		Metadata: engine.PlanMetadata{PlanHash: "plan"},
	})
	if err != nil {
		t.Fatalf("DependencyLockForPlan: %v", err)
	}
	if lock.GraphHash == "" || lock.CatalogDigest == "" || lock.GraphHash == lock.CatalogDigest {
		t.Fatalf("canonical graph/catalog locks = %q/%q", lock.GraphHash, lock.CatalogDigest)
	}
}
