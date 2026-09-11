package schema

import "encoding/json"

// LockedExport is a per-export digest entry in a yawr.package-lock/v1 file.
type LockedExport struct {
	ID     string `yaml:"id"     json:"id"`
	Path   string `yaml:"path"   json:"path"`
	Digest string `yaml:"digest" json:"digest"`
}

// LockedPackage is a single resolved package entry in a yawr.package-lock/v1
// file. Root is workspace-relative POSIX, or "external:sha256:<digest of
// the absolute realpath>" for packages resolved outside the workspace.
type LockedPackage struct {
	Name    string         `yaml:"name"    json:"name"`
	Version string         `yaml:"version" json:"version"`
	Root    string         `yaml:"root"    json:"root"`
	Digest  string         `yaml:"digest"  json:"digest"`
	Exports []LockedExport `yaml:"exports" json:"exports"`
}

// PackageLock is the parsed representation of .yawr/packages.lock.json
// (apiVersion: yawr.package-lock/v1). This is a generated, committed, frozen
// integrity record — not a solver output. See
// spec/schema-candidates/yawr-private-1990a6c/package-lock.v1.schema.json and
// design/yawr/sections/06-tool-runtime.tex §Package map / lock.
type PackageLock struct {
	Schema        string          `yaml:"$schema,omitempty" json:"$schema,omitempty"`
	APIVersion    string          `yaml:"apiVersion"        json:"apiVersion"`
	GeneratedBy   string          `yaml:"generatedBy"       json:"generatedBy"`
	Packages      []LockedPackage `yaml:"packages"          json:"packages"`
	CatalogDigest string          `yaml:"catalogDigest"     json:"catalogDigest"`
}

// PackageLockAPIVersion is the required apiVersion discriminator for
// .yawr/packages.lock.json.
const PackageLockAPIVersion = "yawr.package-lock/v1"

// LockedDynamicInclude records the resolution of one dynamic include site
// for deterministic replay and resume-drift detection. One record is
// appended per successful resolution, in resolution order, and persisted
// in PlanMetadata.DynamicIncludes via engine.RunStore.SaveState.
type LockedDynamicInclude struct {
	StepID             string                        `yaml:"step_id"          json:"step_id"`
	QualifiedNodeID    string                        `yaml:"qualified_node_id,omitempty" json:"qualified_node_id,omitempty"`
	Invocation         int                           `yaml:"invocation,omitempty" json:"invocation,omitempty"`
	Revision           int64                         `yaml:"revision,omitempty" json:"revision,omitempty"`
	StructuralPath     []DynamicIncludeFrameIdentity `yaml:"structural_path,omitempty" json:"structural_path,omitempty"`
	RenderedRef        string                        `yaml:"rendered_ref"      json:"rendered_ref"`
	QualifiedID        string                        `yaml:"qualified_id"      json:"qualified_id"`
	RunbookID          string                        `yaml:"runbook_id,omitempty" json:"runbook_id,omitempty"`
	RunbookName        string                        `yaml:"runbook_name,omitempty" json:"runbook_name,omitempty"`
	RunbookContentHash string                        `yaml:"runbook_content_hash,omitempty" json:"runbook_content_hash,omitempty"`
	AbsPath            string                        `yaml:"abs_path"          json:"abs_path"`
	PackageName        string                        `yaml:"package_name"      json:"package_name"`
	PackageVersion     string                        `yaml:"package_version"   json:"package_version"`
	FileDigest         string                        `yaml:"file_digest"       json:"file_digest"`
	PackageDigest      string                        `yaml:"package_digest"    json:"package_digest"`
	ExecutableClosure  json.RawMessage               `yaml:"-" json:"executable_closure,omitempty"`
	ResolvedInputs     map[string]*Input             `yaml:"-" json:"resolved_inputs,omitempty"`
	ResolvedBindings   []Binding                     `yaml:"-" json:"resolved_bindings,omitempty"`
	ResolvedOutputs    map[string]*Output            `yaml:"-" json:"resolved_outputs,omitempty"`
	ResolvedGovernance *GovernanceConfig             `yaml:"-" json:"resolved_governance,omitempty"`
}

type DynamicIncludeFrameIdentity struct {
	QualifiedNodeID string `yaml:"qualified_node_id" json:"qualified_node_id"`
	Kind            string `yaml:"kind" json:"kind"`
	BranchLabel     string `yaml:"branch_label,omitempty" json:"branch_label,omitempty"`
	IterationIndex  int    `yaml:"iteration_index,omitempty" json:"iteration_index,omitempty"`
	Invocation      int    `yaml:"invocation,omitempty" json:"invocation,omitempty"`
}
