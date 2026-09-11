package evidence

import "time"

// EvidenceKind is the type discriminator for evidence items.
type EvidenceKind string

const (
	EvidenceKindText       EvidenceKind = "text"
	EvidenceKindChecklist  EvidenceKind = "checklist"
	EvidenceKindAttachment EvidenceKind = "attachment"
	EvidenceKindSnapshot   EvidenceKind = "snapshot"
)

// EvidenceRecord is a single piece of evidence captured at a step.
// Embedded in the step/completed trace event payload.
type EvidenceRecord struct {
	// Name is the evidence field name (e.g., "stdout", "observation", "screenshot").
	Name string `json:"name"`

	// Kind discriminates the evidence type.
	Kind EvidenceKind `json:"kind"`

	// Value holds text content (for Kind=text).
	Value string `json:"value,omitempty"`

	// Items holds checklist responses (for Kind=checklist).
	// Keys are checklist item labels; values are "checked" or "unchecked".
	Items map[string]string `json:"items,omitempty"`

	// Path is the relative path under attachments/ (for Kind=attachment).
	Path string `json:"path,omitempty"`

	// SHA256 is the hex-encoded SHA256 digest (for Kind=attachment).
	SHA256 string `json:"sha256,omitempty"`

	// Size is the file size in bytes (for Kind=attachment).
	Size int64 `json:"size,omitempty"`

	// CapturedAt is when this evidence was collected.
	CapturedAt time.Time `json:"captured_at"`
}

// EvidenceSet is an ordered collection of evidence records for a single step.
type EvidenceSet struct {
	StepID  string           `json:"step_id"`
	Records []EvidenceRecord `json:"records"`
}

// ChecklistItem is a single item in a checklist evidence prompt.
type ChecklistItem struct {
	Label    string `json:"label"`
	Required bool   `json:"required"`
}
