// Package graphdoc defines the neutral, render-agnostic preview document
// produced from a runbook AST.
//
// A Document is a content-hashable structure that all renderers
// (prose, mermaid, asciigraph, graphjson) consume. It is purely structural:
// runtime status is overlaid separately via pkg/preview/runstate.
//
// Concepts:
//
//   - Frame  — one per included runbook (the root is a frame too). Frames
//     give every Node a stable address even when the same runbook is
//     included multiple times via different parents.
//   - Group  — a container of sibling nodes that share a structural meaning:
//     an iterate body, a parallel branch, a branch arm, a compensate body,
//     or the body of an include frame.
//   - Node   — one per visited flow step (cli, tool, branch, iterate,
//     parallel, include, choice, …). Iterate and parallel parents are
//     themselves nodes; their bodies live in child Groups.
//   - Edge   — typed connection between two nodes. Kinds: sequence,
//     branch-arm, parallel-branch, iterate-body, compensate, include.
//
// The Document is built by a flowwalk.Visitor (see Builder). By default the
// builder treats include steps as opaque leaves; set Builder.Recurse=true
// to inline included runbooks as nested frames.
package graphdoc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	regschema "github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// SchemaVersion is the semantic version of the Document JSON shape.
// Bump on breaking changes. Optional Node.Details fields are additive within v1.
const SchemaVersion = "1"

// GroupKind enumerates the structural roles a Group can play.
type GroupKind string

const (
	GroupIterateBody    GroupKind = "iterate-body"
	GroupParallelBranch GroupKind = "parallel-branch"
	GroupBranchArm      GroupKind = "branch-arm"
	GroupCompensateBody GroupKind = "compensate-body"
	GroupIncludeFrame   GroupKind = "include-frame"
)

// EdgeKind enumerates the typed connections between nodes.
type EdgeKind string

const (
	EdgeSequence       EdgeKind = "sequence"
	EdgeBranchArm      EdgeKind = "branch-arm"
	EdgeParallelBranch EdgeKind = "parallel-branch"
	EdgeIterateBody    EdgeKind = "iterate-body"
	EdgeCompensate     EdgeKind = "compensate"
	EdgeInclude        EdgeKind = "include"
)

// Document is the top-level preview structure. It is content-hashable:
// the Hash field is a SHA-256 over the canonical JSON of all other fields,
// computed by Builder.Build.
type Document struct {
	PresentationState *PresentationState  `json:"presentation_state,omitempty"`
	SchemaVersion     string              `json:"schema_version"`
	Hash              string              `json:"hash"`
	Runbook           RunbookRef          `json:"runbook"`
	Frames            []Frame             `json:"frames"`
	Groups            []Group             `json:"groups"`
	Nodes             []Node              `json:"nodes"`
	Edges             []Edge              `json:"edges"`
	Regions           *regschema.Manifest `json:"regions,omitempty"`
	// Inputs carries the root runbook's declared top-level input metadata
	// (AR-CE-2, T-CLIENT-ENUM-DTO): one entry per `inputs.<name>` key, in
	// name order for a stable, content-hashable document. This is
	// declaration metadata only (contract shape), not a live value or a
	// per-run binding -- it never changes across runs of the same
	// runbook source and is safe to render before any run starts.
	Inputs []InputDecl `json:"inputs,omitempty"`
}

// InputDecl is one runbook `inputs.<name>` declaration's client-facing
// metadata (AR-CE-2 §2, normative DTO shape). Fields mirror schema.Input
// verbatim except Enum/EnumRedacted/EnumMemberCount, which encode the C1
// redaction contract: EnumRedacted true means Enum MUST be absent (never
// `[]`, never null) and EnumMemberCount carries the raw count so a client
// can render "one of N permitted values" without ever seeing a member.
// enum is deliberately never merged into a generic "options" field here:
// AR-ENUM-1 kept collector options (UI-driven, {value,label}) and enum
// (contract-driven, values-only) distinct, and this DTO preserves that
// distinction at the wire layer (AR-CE-2 §2, CE-D-04).
type InputDecl struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
	// Enum is the declared-order, verbatim, unsorted, undeduplicated
	// member list (AR-ENUM-5, AR-CE-2). Absent (not `[]`/null) when the
	// input carries no enum constraint (AR-ENUM-11) or when redacted.
	Enum []string `json:"enum,omitempty"`
	// EnumRedacted is true when this declaration's enum is governed by a
	// redaction rule (C1); Enum MUST be omitted whenever this is true.
	EnumRedacted bool `json:"enumRedacted,omitempty"`
	// EnumMemberCount is len(Enum) before redaction, present only when
	// EnumRedacted is true (AR-CE-2 §2 table).
	EnumMemberCount int `json:"enumMemberCount,omitempty"`
}

// RunbookRef identifies the root runbook the document was built from.
type RunbookRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// Frame represents a single runbook scope. The root runbook is always
// the first frame; each include with Recurse=true adds another.
type Frame struct {
	Invocation                   *schema.RunbookInvocation `json:"invocation,omitempty"`
	ID                           string                    `json:"id"`
	QualifiedID                  string                    `json:"qualified_id,omitempty"`
	RunbookID                    string                    `json:"runbook_id"`
	RunbookPath                  string                    `json:"runbook_path"`
	ContentHash                  string                    `json:"content_hash,omitempty"`
	ParentIncludeNodeID          string                    `json:"parent_include_node_id,omitempty"`
	QualifiedParentIncludeNodeID string                    `json:"qualified_parent_include_node_id,omitempty"`
	Depth                        int                       `json:"depth"`
}

// Group is a container of sibling nodes that share a structural role.
type Group struct {
	ID                    string    `json:"id"`
	QualifiedID           string    `json:"qualified_id,omitempty"`
	Kind                  GroupKind `json:"kind"`
	ParentNodeID          string    `json:"parent_node_id"`
	QualifiedParentNodeID string    `json:"qualified_parent_node_id,omitempty"`
	FrameID               string    `json:"frame_id"`
	QualifiedFrameID      string    `json:"qualified_frame_id,omitempty"`
	Label                 string    `json:"label,omitempty"`
	Index                 int       `json:"index,omitempty"`
	Fallback              bool      `json:"fallback,omitempty"`
}

// Node is a single visited flow step.
type Node struct {
	ID               string       `json:"id"`
	QualifiedID      string       `json:"qualified_id,omitempty"`
	StepID           string       `json:"step_id,omitempty"`
	CallPath         []string     `json:"call_path,omitempty"`
	Kind             string       `json:"kind"`
	Title            string       `json:"title,omitempty"`
	FrameID          string       `json:"frame_id"`
	QualifiedFrameID string       `json:"qualified_frame_id,omitempty"`
	GroupID          string       `json:"group_id,omitempty"`
	QualifiedGroupID string       `json:"qualified_group_id,omitempty"`
	Order            int          `json:"order"`
	Concurrent       bool         `json:"concurrent,omitempty"`
	ToolName         string       `json:"tool_name,omitempty"`
	ToolAction       string       `json:"tool_action,omitempty"`
	Details          *StepDetails `json:"details,omitempty"`
	// Dynamic is true when this include node's target is resolved at
	// execution time (runbook_ref + resolve_from: catalog). Preview
	// cannot show the resolved child graph.
	Dynamic bool `json:"dynamic,omitempty"`
}

// Edge is a typed connection between two nodes.
type Edge struct {
	From          string   `json:"from"`
	QualifiedFrom string   `json:"qualified_from,omitempty"`
	To            string   `json:"to"`
	QualifiedTo   string   `json:"qualified_to,omitempty"`
	Kind          EdgeKind `json:"kind"`
	Label         string   `json:"label,omitempty"`
}

// computeHash returns a stable SHA-256 hex digest over the document content
// (everything except Hash itself).
func (d *Document) computeHash() (string, error) {
	clone := *d
	clone.Hash = ""
	clone.PresentationState = nil
	if len(d.Nodes) > 0 {
		clone.Nodes = append([]Node(nil), d.Nodes...)
	}
	for index := range clone.Nodes {
		details := clone.Nodes[index].Details
		if details != nil && details.CodePresentation != nil {
			copied := *details
			envelope := *details.CodePresentation
			envelope.PlanSnapshotDigest = ""
			copied.CodePresentation = &envelope
			clone.Nodes[index].Details = &copied
		}
	}
	clone.Runbook.Path = ""
	clone.Frames = append([]Frame(nil), d.Frames...)
	for index := range clone.Frames {
		clone.Frames[index].RunbookPath = ""
	}
	b, err := json.Marshal(&clone)
	if err != nil {
		return "", fmt.Errorf("graphdoc: marshal for hash: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ContentHash returns the canonical hash used by Builder for this document.
func (d *Document) ContentHash() (string, error) {
	if d == nil {
		return "", fmt.Errorf("graphdoc: nil document")
	}
	return d.computeHash()
}

func runbookContentHash(runbook *schema.Runbook) (string, error) {
	encoded, err := json.Marshal(runbook)
	if err != nil {
		return "", fmt.Errorf("graphdoc: marshal runbook content: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// RunbookContentHash returns the immutable source-definition hash used by
// graph frames.
func RunbookContentHash(runbook *schema.Runbook) (string, error) {
	if runbook == nil {
		return "", fmt.Errorf("graphdoc: nil runbook")
	}
	return runbookContentHash(runbook)
}
