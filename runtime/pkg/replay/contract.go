// Package replay defines transport-neutral reviewed boundary fixtures shared
// by route tests and persisted investigation-session scenarios.
package replay

import (
	"encoding/json"
	"errors"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// Selector identifies one exact execution occurrence.
type Selector struct {
	QualifiedNodeID string                               `yaml:"qualified_node_id,omitempty" json:"qualified_node_id,omitempty"`
	CallPath        []string                             `yaml:"call_path,omitempty" json:"call_path,omitempty"`
	StructuralPath  []schema.DynamicIncludeFrameIdentity `yaml:"structural_path,omitempty" json:"structural_path,omitempty"`
	Step            string                               `yaml:"step" json:"step"`
	Phase           string                               `yaml:"phase" json:"phase"`
	Invocation      int                                  `yaml:"invocation" json:"invocation"`
	Attempt         int                                  `yaml:"attempt" json:"attempt"`
}

// Review records the human review required before saved boundary data may run.
type Review struct {
	State               string `yaml:"state" json:"state"`
	ReviewedBy          string `yaml:"reviewed_by" json:"reviewed_by"`
	ReviewedAt          string `yaml:"reviewed_at" json:"reviewed_at"`
	SensitivityReviewed bool   `yaml:"sensitivity_reviewed" json:"sensitivity_reviewed"`
}

// Source records where a reviewed fixture originated.
type Source struct {
	Kind          string `yaml:"kind" json:"kind"`
	RunID         string `yaml:"run_id,omitempty" json:"run_id,omitempty"`
	InteractionID string `yaml:"interaction_id,omitempty" json:"interaction_id,omitempty"`
	AnswerDigest  string `yaml:"answer_digest,omitempty" json:"answer_digest,omitempty"`
	CopiedAt      string `yaml:"copied_at,omitempty" json:"copied_at,omitempty"`
}

type HostActionResponse struct {
	Status string         `yaml:"status" json:"status"`
	Result map[string]any `yaml:"result,omitempty" json:"result,omitempty"`
}

type HostActionBinding struct {
	At         Selector           `yaml:"at" json:"at"`
	Capability string             `yaml:"capability,omitempty" json:"capability,omitempty"`
	Response   HostActionResponse `yaml:"response" json:"response"`
	Source     Source             `yaml:"source,omitempty" json:"source,omitempty"`
	Review     Review             `yaml:"review" json:"review"`
}

type InteractionBinding struct {
	At       Selector       `yaml:"at" json:"at"`
	Kind     string         `yaml:"kind" json:"kind"`
	Selected []string       `yaml:"selected,omitempty" json:"selected,omitempty"`
	Label    string         `yaml:"label,omitempty" json:"label,omitempty"`
	Values   map[string]any `yaml:"values,omitempty" json:"values,omitempty"`
	Source   Source         `yaml:"source,omitempty" json:"source,omitempty"`
	Review   Review         `yaml:"review" json:"review"`
}

type StepBinding struct {
	At      Selector       `yaml:"at" json:"at"`
	Kind    string         `yaml:"kind" json:"kind"`
	Status  string         `yaml:"status" json:"status"`
	Outcome string         `yaml:"outcome,omitempty" json:"outcome,omitempty"`
	Output  map[string]any `yaml:"output,omitempty" json:"output,omitempty"`
	Vars    map[string]any `yaml:"vars,omitempty" json:"vars,omitempty"`
	Source  Source         `yaml:"source,omitempty" json:"source,omitempty"`
	Review  Review         `yaml:"review" json:"review"`
}

type ApprovalBinding struct {
	At       Selector `yaml:"at" json:"at"`
	Approved bool     `yaml:"approved" json:"approved"`
	Approver string   `yaml:"approver" json:"approver"`
	Source   Source   `yaml:"source,omitempty" json:"source,omitempty"`
	Review   Review   `yaml:"review" json:"review"`
}

// DependencyLock binds a replay artifact to all execution-affecting inputs.
type DependencyLock struct {
	PlanHash          string `yaml:"plan_hash" json:"plan_hash"`
	GraphHash         string `yaml:"graph_hash,omitempty" json:"graph_hash,omitempty"`
	CatalogDigest     string `yaml:"catalog_digest,omitempty" json:"catalog_digest,omitempty"`
	PackageLockDigest string `yaml:"package_lock_digest,omitempty" json:"package_lock_digest,omitempty"`
	ToolDigest        string `yaml:"tool_digest,omitempty" json:"tool_digest,omitempty"`
	ProfileDigest     string `yaml:"profile_digest,omitempty" json:"profile_digest,omitempty"`
}

func DependencyLockForPlan(plan *engine.ExecutionPlan) (DependencyLock, error) {
	if plan == nil || plan.Metadata.PlanHash == "" {
		return DependencyLock{}, errors.New("replay: validated plan hash is required")
	}
	packageLock, err := json.Marshal(plan.Metadata.PackageDigests)
	if err != nil {
		return DependencyLock{}, err
	}
	tools, err := json.Marshal(plan.Tools)
	if err != nil {
		return DependencyLock{}, err
	}
	profile, err := json.Marshal(plan.Metadata.Profile)
	if err != nil {
		return DependencyLock{}, err
	}
	return DependencyLock{
		PlanHash:          plan.Metadata.PlanHash,
		GraphHash:         canonicalDependency(plan.Metadata.GraphContentHash, "graph"),
		CatalogDigest:     canonicalDependency(plan.Metadata.CatalogDigest, "catalog"),
		PackageLockDigest: engine.InteractionPayloadDigest(packageLock),
		ToolDigest:        engine.InteractionPayloadDigest(tools),
		ProfileDigest:     engine.InteractionPayloadDigest(profile),
	}, nil
}

func canonicalDependency(value, kind string) string {
	if value != "" {
		return value
	}
	encoded, _ := json.Marshal(map[string]string{"dependency": kind, "state": "absent"})
	return engine.InteractionPayloadDigest(encoded)
}

// ExpectedTransition verifies a natural cross-runbook transition during replay.
type ExpectedTransition struct {
	At            Selector `yaml:"at" json:"at"`
	TargetRunbook string   `yaml:"target_runbook" json:"target_runbook"`
	ReasonCode    string   `yaml:"reason_code" json:"reason_code"`
}
