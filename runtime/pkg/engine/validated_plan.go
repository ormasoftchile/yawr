package engine

import (
	"fmt"
	"strings"
	"time"

	gcpparser "github.com/ormasoftchile/yawr/runtime/pkg/gcp/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	gxlparser "github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
)

const (
	GrammarVersionGXL = "1.0.0-draft"
	GrammarVersionGIS = "1.0.0-draft"
	GrammarVersionGCP = "1.0.0-draft"
)

// StepRef identifies the runbook field that produced a parsed expression.
type StepRef struct {
	StepID    string
	FieldPath string
}

// GrammarVersions records parser grammar versions used during plan validation.
type GrammarVersions struct {
	GXL string `json:"gxl"`
	GIS string `json:"gis"`
	GCP string `json:"gcp"`
}

// ExpressionCount summarizes how many expression-bearing fields were validated.
type ExpressionCount struct {
	GXL int `json:"gxl"`
	GIS int `json:"gis"`
	GCP int `json:"gcp"`
}

// EnumMeta records one enum-constrained declaration's contract metadata for
// ValidatedPlan carriage (AR-ENUM-10): the declared-order member list (or
// redacted per C1), the raw member count (always visible, even when
// redacted), and whether this declaration's member list was redacted.
type EnumMeta struct {
	// Members is the declared-order member list, or nil when Redacted.
	Members []string
	// MemberCount is len(Members) before any redaction is applied.
	MemberCount int
	// Redacted is true when this declaration is redact-governed (C1): a
	// tool arg/input matched by a governance redact rule, or listed as
	// sensitive. Members is nil and MUST NOT be reconstructed from
	// MemberCount when Redacted is true.
	Redacted bool
}

// ValidatedPlan records plan-time parser artifacts and validation metadata.
type ValidatedPlan struct {
	Source          *ExecutionPlan
	RunbookID       string
	RunbookHash     string
	GXLExprs        map[StepRef]*gxlparser.Expr
	GISTemplates    map[StepRef]*gis.Template
	GCPPaths        map[StepRef]*gcpparser.Path
	GrammarVersions GrammarVersions
	ExpressionCount ExpressionCount
	ValidatedAt     time.Time
	// EnumConstraints carries every enum-constrained declaration's
	// contract metadata, once per run (AR-ENUM-10), keyed by a stable
	// "site.declaration" path (e.g. "inputs.env_name", "outputs.drained",
	// "tool.kubectl.drain-node.args.force"). Never repeated in per-step
	// trace event payloads.
	EnumConstraints map[string]EnumMeta
}

// PlanValidationError describes one parse-gate failure.
type PlanValidationError struct {
	StepID    string
	FieldPath string
	Source    string
	Code      string
	Class     string
	Err       error
}

func (e PlanValidationError) Error() string {
	code := e.Code
	if code == "" {
		code = "PLAN-001"
	}
	loc := e.FieldPath
	if e.StepID != "" {
		loc = "step " + e.StepID + " " + loc
	}
	return fmt.Sprintf("%s: %s: %q: %v", code, loc, e.Source, e.Err)
}

func (e PlanValidationError) Unwrap() error { return e.Err }

// PlanValidationMultiError aggregates all parse-gate failures in stable order.
type PlanValidationMultiError struct {
	Errors []PlanValidationError
}

func (m *PlanValidationMultiError) Error() string {
	if m == nil || len(m.Errors) == 0 {
		return "plan validation failed"
	}
	parts := make([]string, 0, len(m.Errors))
	for _, e := range m.Errors {
		parts = append(parts, e.Error())
	}
	return fmt.Sprintf("PLAN-001: plan validation failed with %d error(s): %s", len(m.Errors), strings.Join(parts, "; "))
}

// ValidatedForTest marks a hand-built test or synthetic sub-plan as prevalidated.
func ValidatedForTest(plan *ExecutionPlan) *ExecutionPlan {
	if plan == nil {
		return nil
	}
	if plan.Metadata.PlanHash == "" {
		var identity strings.Builder
		fmt.Fprintf(&identity, "%s\x00%s", plan.RunbookPath, plan.Metadata.RunbookID)
		for _, step := range plan.Steps {
			fmt.Fprintf(&identity, "\x00%s\x00%s\x00%d", step.ID, step.Kind, step.Depth)
		}
		plan.Metadata.PlanHash = InteractionPayloadDigest([]byte(identity.String()))
	}
	if plan.Validation == nil {
		plan.Validation = &ValidatedPlan{
			Source:          plan,
			RunbookID:       plan.Metadata.RunbookID,
			RunbookHash:     plan.Metadata.PlanHash,
			GXLExprs:        map[StepRef]*gxlparser.Expr{},
			GISTemplates:    map[StepRef]*gis.Template{},
			GCPPaths:        map[StepRef]*gcpparser.Path{},
			GrammarVersions: GrammarVersions{GXL: GrammarVersionGXL, GIS: GrammarVersionGIS, GCP: GrammarVersionGCP},
			ValidatedAt:     time.Now().UTC(),
		}
	}
	return plan
}
