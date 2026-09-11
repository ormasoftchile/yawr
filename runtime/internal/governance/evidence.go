package governance

import (
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

// EvidenceCollector builds Evidence records from evaluation results.
// This is a helper for constructing evidence in a consistent way.
type EvidenceCollector struct{}

// NewEvidenceCollector constructs an evidence collector.
func NewEvidenceCollector() *EvidenceCollector {
	return &EvidenceCollector{}
}

// CollectFromEvaluation extracts Evidence from an EvaluationResult.
// This is mainly a pass-through since EvaluationResult already contains Evidence.
func (c *EvidenceCollector) CollectFromEvaluation(result governance.EvaluationResult) governance.Evidence {
	return result.Evidence
}
