package schema

import (
	"path/filepath"
	"strings"
)

// CLISpec holds the fields for a step of type "cli".
type CLISpec struct {
	Command string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"    json:"args,omitempty"`
	Run     any               `yaml:"run,omitempty"     json:"run,omitempty"` // string or map[string]string
	Shell   string            `yaml:"shell,omitempty"   json:"shell,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"     json:"env,omitempty"`
	Workdir string            `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	Stdin   string            `yaml:"stdin,omitempty"   json:"stdin,omitempty"`
}

// ToolCallSpec holds the fields for a step of type "tool".
type ToolCallSpec struct {
	Tool ToolInvocation `yaml:"tool" json:"tool"`
}

// ToolInvocation is the nested tool invocation block inside a tool step.
type ToolInvocation struct {
	Name    string         `yaml:"name"              json:"name"`
	Action  string         `yaml:"action"            json:"action"`
	Args    map[string]any `yaml:"args,omitempty"    json:"args,omitempty"`
	Version string         `yaml:"version,omitempty" json:"version,omitempty"`
}

// IncludeSpec holds the fields for a step of type "include".
type IncludeSpec struct {
	Include IncludeConfig `yaml:"include" json:"include"`

	// ResolvedSteps is populated by the planner (or sub-step expander)
	// with the loaded child runbook's flow nodes. The IncludeExecutor uses
	// these to invoke the SubStepRunner. It is empty in unresolved
	// IncludeSpec instances produced by the parser alone.
	ResolvedSteps []FlowNode `yaml:"-" json:"-"`

	// ResolvedRunbookPath identifies the immutable source of ResolvedSteps.
	// Nested handoffs resolve static targets relative to this path.
	ResolvedRunbookPath        string `yaml:"-" json:"-"`
	ResolvedRunbookID          string `yaml:"-" json:"-"`
	ResolvedRunbookName        string `yaml:"-" json:"-"`
	ResolvedRunbookContentHash string `yaml:"-" json:"-"`

	// LazyRunbookPath is the absolute filesystem path of the child
	// runbook to load *at execution time* when this include was deferred
	// by the planner (expand=lazy). Set only by the planner; never
	// serialized. Empty for eager-expanded includes.
	LazyRunbookPath string `yaml:"-" json:"-"`

	// LazyRunbookDigest binds deferred execution to the exact raw bytes that
	// existed when the durable run started. Empty is accepted only for
	// non-durable legacy plans, which resume validation rejects.
	LazyRunbookDigest string `yaml:"-" json:"-"`

	// ResolvedInputs, ResolvedOutputs, and ResolvedGovernance retain the child runbook's
	// protection metadata across the planner/executor boundary.
	ResolvedInputs     map[string]*Input  `yaml:"-" json:"-"`
	ResolvedBindings   []Binding          `yaml:"-" json:"-"`
	ResolvedOutputs    map[string]*Output `yaml:"-" json:"-"`
	ResolvedGovernance *GovernanceConfig  `yaml:"-" json:"-"`
}

// IncludeConfig is the nested include config block.
//
// An include site is either *static* or *dynamic*:
//
//   - static:  `runbook: relative/path.yaml` (or an imports: alias). The
//     target is known during planning and may be expanded eagerly or
//     lazily (see Expand).
//   - dynamic: `runbook_ref: "${expr}"` plus `resolve_from: catalog`. The
//     reference is rendered and resolved at *execution* time against the
//     approved runbook catalog (package exports). No filesystem path is
//     ever accepted from a runtime value.
//
// Exactly one of Runbook / RunbookRef may be set.
type IncludeConfig struct {
	Runbook string            `yaml:"runbook,omitempty" json:"runbook,omitempty"`
	With    map[string]string `yaml:"with,omitempty" json:"with,omitempty"`
	When    string            `yaml:"when,omitempty" json:"when,omitempty"`
	Gate    *GateSpec         `yaml:"gate,omitempty" json:"gate,omitempty"`

	// RunbookRef is a GIS template that renders, at execution time, to a
	// runbook *identity* (not a path): either a bare exported id
	// ("tsg-disk-pressure") or a package-qualified id
	// ("contoso-tsgs/tsg-disk-pressure"). Requires ResolveFrom.
	RunbookRef string `yaml:"runbook_ref,omitempty" json:"runbook_ref,omitempty"`

	// ResolveFrom names the approved resolution source for RunbookRef.
	// The only accepted value is ResolveFromCatalog ("catalog"); arbitrary
	// runtime filesystem paths are never a resolution source.
	ResolveFrom string `yaml:"resolve_from,omitempty" json:"resolve_from,omitempty"`

	// OnNotFound selects the behaviour when RunbookRef resolves to nothing
	// in the catalog: OnNotFoundFail (default) turns the step into a
	// failure, OnNotFoundContinue skips the step with status=skipped;
	// `runbook_found=false` is preserved in step vars so callers can branch on it.
	OnNotFound string `yaml:"on_not_found,omitempty" json:"on_not_found,omitempty"`

	// Expand overrides this include site's expansion mode. One of "",
	// "eager", "lazy", or "auto". See pkg/expand. Empty inherits from
	// the parent runbook (or the planner's policy default). Schema-rejected
	// when runbook_ref is present (B-14): setting expand on a dynamic
	// include site is a schema validation error.
	Expand string `yaml:"expand,omitempty" json:"expand,omitempty"`
}

// Accepted values for IncludeConfig.ResolveFrom and IncludeConfig.OnNotFound.
const (
	// ResolveFromCatalog resolves runbook_ref against the approved
	// runbook catalog built from package exports.
	ResolveFromCatalog = "catalog"

	// OnNotFoundFail is the default: an unresolvable runbook_ref fails
	// the include step.
	OnNotFoundFail = "fail"
	// OnNotFoundContinue skips the step with runbook_found=false
	// instead of failing, so callers can branch on the outcome.
	OnNotFoundContinue = "continue"
)

// IsDynamic reports whether this include site resolves its target at
// execution time from an approved catalog rather than from a statically
// known path.
func (c IncludeConfig) IsDynamic() bool { return c.RunbookRef != "" }

// Reference returns the authored reference for display purposes: the
// runbook_ref template for dynamic sites, the path/alias for static ones.
func (c IncludeConfig) Reference() string {
	if c.IsDynamic() {
		return c.RunbookRef
	}
	return c.Runbook
}

// NotFoundIsFatal reports whether an unresolvable dynamic reference must
// fail the step. Only an explicit on_not_found: continue opts out.
func (c IncludeConfig) NotFoundIsFatal() bool {
	return c.OnNotFound != OnNotFoundContinue
}

// GateSpec configures outcome-based short-circuit on an include step.
// When the included runbook completes with an outcome whose category
// matches any value in StopIf, the parent run stops without error.
type GateSpec struct {
	StopIf []string `yaml:"stop_if,omitempty" json:"stop_if,omitempty"`
}

// ChoiceSpec holds the fields for a step of type "choice".
type ChoiceSpec struct {
	Prompt        string         `yaml:"prompt"                   json:"prompt"`
	Options       []ChoiceOption `yaml:"options"                 json:"options"`
	Variable      string         `yaml:"variable"                 json:"variable"`
	Default       string         `yaml:"default,omitempty"        json:"default,omitempty"`
	Multiple      bool           `yaml:"multiple,omitempty"       json:"multiple,omitempty"`
	MinSelections int            `yaml:"min_selections,omitempty" json:"min_selections,omitempty"`
	MaxSelections int            `yaml:"max_selections,omitempty" json:"max_selections,omitempty"`
	Approvals     *ApprovalGate  `yaml:"approvals,omitempty"      json:"approvals,omitempty"`
}

// ChoiceOption is a single selectable option in a choice step.
type ChoiceOption struct {
	Label string `yaml:"label"          json:"label"`
	Value string `yaml:"value"          json:"value"`
	Hint  string `yaml:"hint,omitempty" json:"hint,omitempty"`
}

// DecisionSpec holds the fields for a step of type "decision".
type DecisionSpec struct {
	Prompt   string          `yaml:"prompt"             json:"prompt"`
	Routes   []DecisionRoute `yaml:"routes"             json:"routes"`
	Variable string          `yaml:"variable,omitempty" json:"variable,omitempty"`
}

// DecisionRoute is a single route option in a decision step.
type DecisionRoute struct {
	Label   string `yaml:"label"             json:"label"`
	Runbook string `yaml:"runbook,omitempty" json:"runbook,omitempty"`
	Goto    string `yaml:"goto,omitempty"    json:"goto,omitempty"`
	Hint    string `yaml:"hint,omitempty"    json:"hint,omitempty"`
}

// CollectorSpec holds the fields for a step of type "collector".
type CollectorSpec struct {
	Prompt    string           `yaml:"prompt"              json:"prompt"`
	Fields    []CollectorField `yaml:"fields"              json:"fields"`
	Approvals *ApprovalGate    `yaml:"approvals,omitempty" json:"approvals,omitempty"`
}

// HostActionSpec requests an allowlisted operation from a capable host integration.
type HostActionSpec struct {
	HostAction HostActionConfig `yaml:"host_action" json:"host_action"`
}

// HostActionConfig carries a logical capability and capability-owned payload.
// The host registry validates payload semantics before dispatch.
type HostActionConfig struct {
	Capability string         `yaml:"capability" json:"capability"`
	Request    map[string]any `yaml:"request" json:"request"`
}

// HandoffSpec transfers an allowlisted context into one statically named
// target runbook. The session coordinator resolves and starts the target.
type HandoffSpec struct {
	Handoff HandoffConfig `yaml:"handoff" json:"handoff"`
}

type HandoffConfig struct {
	Runbook string            `yaml:"runbook" json:"runbook"`
	Reason  HandoffReason     `yaml:"reason" json:"reason"`
	With    map[string]string `yaml:"with,omitempty" json:"with,omitempty"`
	Facts   map[string]string `yaml:"facts,omitempty" json:"facts,omitempty"`
}

type HandoffReason struct {
	Code    string `yaml:"code" json:"code"`
	Summary string `yaml:"summary" json:"summary"`
}

// IsStaticHandoffTarget reports whether target is a portable relative V1
// runbook path with no templates or directory escape.
func IsStaticHandoffTarget(target string) bool {
	if target == "" || target != strings.TrimSpace(target) || strings.Contains(target, "${") ||
		filepath.IsAbs(target) || strings.ContainsAny(target, "\\:*?\"<>|") || strings.HasPrefix(target, "/") ||
		!strings.HasSuffix(strings.ToLower(target), ".runbook.yaml") {
		return false
	}
	for _, current := range target {
		if current < 0x20 || current == 0x7f {
			return false
		}
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(target)))
	if clean != target || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == "" || strings.TrimRight(segment, " .") != segment || windowsReservedPathSegment(segment) {
			return false
		}
	}
	return true
}

func windowsReservedPathSegment(segment string) bool {
	name := strings.ToUpper(segment)
	if index := strings.IndexByte(name, '.'); index >= 0 {
		name = name[:index]
	}
	switch name {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$":
		return true
	}
	runes := []rune(name)
	if len(runes) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) {
		return runes[3] >= '1' && runes[3] <= '9' || runes[3] == '\u00b9' || runes[3] == '\u00b2' || runes[3] == '\u00b3'
	}
	return false
}

// HandoffBindingSource returns the variable path referenced by a V1 handoff
// binding. Handoffs intentionally reject interpolation and literals so their
// persisted provenance remains exact and auditable.
func HandoffBindingSource(expression string) (string, bool) {
	trimmed := strings.TrimSpace(expression)
	if len(trimmed) < 4 || !strings.HasPrefix(trimmed, "${") || !strings.HasSuffix(trimmed, "}") {
		return "", false
	}
	source := strings.TrimSpace(trimmed[2 : len(trimmed)-1])
	if source == "" || strings.ContainsAny(source, "{}[]()$ \t\r\n") {
		return "", false
	}
	for _, segment := range strings.Split(source, ".") {
		if segment == "" {
			return "", false
		}
		for index, current := range segment {
			if current >= 'a' && current <= 'z' || current >= 'A' && current <= 'Z' || current == '_' ||
				index > 0 && current >= '0' && current <= '9' || index > 0 && current == '-' {
				continue
			}
			return "", false
		}
	}
	return source, true
}

// CollectorField defines a single input field in a collector step.
type CollectorField struct {
	Name        string             `yaml:"name"                   json:"name"`
	Type        CollectorFieldType `yaml:"type"                   json:"type"`
	Label       string             `yaml:"label"                  json:"label"`
	Required    bool               `yaml:"required,omitempty"     json:"required,omitempty"`
	When        string             `yaml:"when,omitempty"         json:"when,omitempty"`
	Hint        string             `yaml:"hint,omitempty"         json:"hint,omitempty"`
	Default     any                `yaml:"default,omitempty"      json:"default,omitempty"`
	Validation  *FieldValidation   `yaml:"validation,omitempty"   json:"validation,omitempty"`
	Options     []ChoiceOption     `yaml:"options,omitempty"      json:"options,omitempty"`
	OptionsFrom *DynamicOptions    `yaml:"options_from,omitempty" json:"options_from,omitempty"`
	Multiple    bool               `yaml:"multiple,omitempty"     json:"multiple,omitempty"`
	Multiline   bool               `yaml:"multiline,omitempty"    json:"multiline,omitempty"`
	Ephemeral   bool               `yaml:"ephemeral,omitempty"    json:"ephemeral,omitempty"`
	// FromStep, when set, instructs the TUI to pre-populate this field with the
	// captured stdout of the named preceding CLI step. The engine ignores this
	// field; only the TUI layer acts on it.
	FromStep string               `yaml:"from_step,omitempty"    json:"from_step,omitempty"`
	Evidence *EvidenceRequirement `yaml:"evidence,omitempty"     json:"evidence,omitempty"`
}

// CollectorFieldType is the type of a collector field.
type CollectorFieldType string

const (
	FieldTypeText         CollectorFieldType = "text"
	FieldTypeNumber       CollectorFieldType = "number"
	FieldTypeInteger      CollectorFieldType = "integer"
	FieldTypeDate         CollectorFieldType = "date"
	FieldTypeDatetime     CollectorFieldType = "datetime"
	FieldTypeBoolean      CollectorFieldType = "boolean"
	FieldTypeSelect       CollectorFieldType = "select"
	FieldTypeAutocomplete CollectorFieldType = "autocomplete"
	FieldTypeFile         CollectorFieldType = "file"
	FieldTypeImage        CollectorFieldType = "image"
)

// FieldValidation holds type-specific validation constraints.
type FieldValidation struct {
	MinLength *int    `yaml:"min_length,omitempty" json:"min_length,omitempty"`
	MaxLength *int    `yaml:"max_length,omitempty" json:"max_length,omitempty"`
	Pattern   string  `yaml:"pattern,omitempty"    json:"pattern,omitempty"`
	Format    string  `yaml:"format,omitempty"     json:"format,omitempty"`
	Min       any     `yaml:"min,omitempty"        json:"min,omitempty"`
	Max       any     `yaml:"max,omitempty"        json:"max,omitempty"`
	Step      float64 `yaml:"step,omitempty"       json:"step,omitempty"`
}

// DynamicOptions configures a provider-backed options list.
type DynamicOptions struct {
	Provider string `yaml:"provider" json:"provider"`
	Field    string `yaml:"field"    json:"field"`
}

// BranchSpec holds the fields for a step of type "branch".
type BranchSpec struct {
	Branches []BranchArm `yaml:"branches" json:"branches"`
}

// BranchArm is a single conditional arm in a branch step.
type BranchArm struct {
	Condition string     `yaml:"condition,omitempty" json:"condition,omitempty"`
	Else      bool       `yaml:"else,omitempty"      json:"else,omitempty"`
	Label     string     `yaml:"label,omitempty"     json:"label,omitempty"`
	Steps     []FlowNode `yaml:"steps,omitempty"     json:"steps,omitempty"`
}

// IterateNode is the top-level iterate node in a flow array.
type IterateNode struct {
	ID            string            `yaml:"id"                   json:"id"`
	Over          string            `yaml:"over,omitempty"       json:"over,omitempty"`
	As            string            `yaml:"as,omitempty"         json:"as,omitempty"`
	Max           int               `yaml:"max,omitempty"        json:"max,omitempty"`
	Until         string            `yaml:"until,omitempty"      json:"until,omitempty"`
	Collect       map[string]string `yaml:"collect,omitempty"    json:"collect,omitempty"`
	CollectValues map[string]any    `yaml:"collect_values,omitempty" json:"collect_values,omitempty"`
	Concurrency   int               `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	Steps         []FlowNode        `yaml:"steps"                json:"steps"`
}

// ParallelNode is the top-level parallel node in a flow array.
type ParallelNode struct {
	ID       string           `yaml:"id"             json:"id"`
	Branches []ParallelBranch `yaml:"branches"       json:"branches"`
	Join     *ParallelJoin    `yaml:"join,omitempty" json:"join,omitempty"`
}

// ParallelBranch is a single concurrent branch in a parallel node.
type ParallelBranch struct {
	Label string     `yaml:"label,omitempty" json:"label,omitempty"`
	Steps []FlowNode `yaml:"steps"           json:"steps"`
}

// ParallelJoin configures the join semantics of a parallel node.
type ParallelJoin struct {
	WaitFor   string `yaml:"wait_for,omitempty"   json:"wait_for,omitempty"`
	OnFailure string `yaml:"on_failure,omitempty" json:"on_failure,omitempty"`
}

// ApproveSpec holds the fields for a step of type "approve".
type ApproveSpec struct {
	Approvals           ApprovalGate `yaml:"approvals"                       json:"approvals"`
	TimeoutBusinessDays int          `yaml:"timeout_business_days,omitempty" json:"timeout_business_days,omitempty"`
	Timezone            string       `yaml:"timezone,omitempty"              json:"timezone,omitempty"`
	BusinessCalendar    string       `yaml:"business_calendar,omitempty"     json:"business_calendar,omitempty"`
	OnTimeout           string       `yaml:"on_timeout,omitempty"            json:"on_timeout,omitempty"`
}

// ApprovalGate configures an approval gate on a step.
type ApprovalGate struct {
	Mode       string   `yaml:"mode,omitempty"        json:"mode,omitempty"`
	Roles      []string `yaml:"roles,omitempty"       json:"roles,omitempty"`
	Pool       []string `yaml:"pool,omitempty"        json:"pool,omitempty"`
	Required   int      `yaml:"required,omitempty"    json:"required,omitempty"`
	Min        int      `yaml:"min,omitempty"         json:"min,omitempty"`
	Timeout    string   `yaml:"timeout,omitempty"     json:"timeout,omitempty"`
	OnTimeout  string   `yaml:"on_timeout,omitempty"  json:"on_timeout,omitempty"`
	EscalateTo []string `yaml:"escalate_to,omitempty" json:"escalate_to,omitempty"`
}

// AssertSpec holds the fields for a step of type "assert".
type AssertSpec struct {
	Assert []Assertion `yaml:"assert" json:"assert"`
}

// Assertion is a single assertion in an assert step.
type Assertion struct {
	Type     string `yaml:"type"               json:"type"`
	Subject  string `yaml:"subject"            json:"subject"`
	Expected string `yaml:"expected,omitempty" json:"expected,omitempty"`
	Path     string `yaml:"path,omitempty"     json:"path,omitempty"`
}

// CompensateSpec holds the fields for a step of type "compensate".
type CompensateSpec struct {
	Compensate CompensateConfig `yaml:"compensate" json:"compensate"`
}

// CompensateConfig is the nested compensate config block.
type CompensateConfig struct {
	On    string     `yaml:"on,omitempty" json:"on,omitempty"`
	Steps []FlowNode `yaml:"steps"        json:"steps"`
}

// WaitForEventSpec holds the fields for a step of type "wait_for_event".
type WaitForEventSpec struct {
	Event     WaitEventConfig `yaml:"event"                json:"event"`
	OnTimeout string          `yaml:"on_timeout,omitempty" json:"on_timeout,omitempty"`
}

// WaitEventConfig is the event configuration block in a wait_for_event step.
type WaitEventConfig struct {
	Source        EventSource       `yaml:"source"                   json:"source"`
	ID            string            `yaml:"id,omitempty"             json:"id,omitempty"`
	Filter        map[string]string `yaml:"filter,omitempty"         json:"filter,omitempty"`
	PayloadSchema string            `yaml:"payload_schema,omitempty" json:"payload_schema,omitempty"`
}

// EventSource enumerates the valid sources for a wait_for_event step.
type EventSource string

const (
	EventSourceWebhook EventSource = "webhook"
	EventSourceMessage EventSource = "message"
	EventSourceSignal  EventSource = "signal"
	EventSourceChannel EventSource = "channel"
)

// EndSpec holds the fields for a step of type "end".
type EndSpec struct {
	Outcome        *OutcomeDeclaration `yaml:"outcome,omitempty" json:"outcome,omitempty"`
	PublishResults bool                `yaml:"publish_results,omitempty" json:"publish_results,omitempty"`
}

// OutcomeDeclaration declares the terminal outcome of a runbook.
type OutcomeDeclaration struct {
	Category string `yaml:"category,omitempty" json:"category,omitempty"`
	Code     string `yaml:"code,omitempty"     json:"code,omitempty"`
}

// NoopSpec holds the fields for a step of type "noop".
// A noop step performs no action but can apply delay, capture variables,
// and participate in conditional execution. All behavior is controlled by
// common Step fields (delay, capture, when, timeout).
type NoopSpec struct{}

// DisplaySpec holds the fields for a step of type "display".
// A display step renders template content to the operator's terminal without
// requiring any user input. Content is evaluated via Go text/template against
// the current variable scope.
type DisplaySpec struct {
	Display DisplayConfig `yaml:"display" json:"display"`
}

// DisplayConfig is the nested display config block.
type DisplayConfig struct {
	Content string `yaml:"content"          json:"content"`
	Format  string `yaml:"format,omitempty" json:"format,omitempty"` // "text" | "markdown"
}

// EvidenceKind enumerates the valid types of evidence that can be required.
type EvidenceKind string

const (
	EvidenceKindText       EvidenceKind = "text"       // Free-form text notes
	EvidenceKindChecklist  EvidenceKind = "checklist"  // Multi-item checklist
	EvidenceKindAttachment EvidenceKind = "attachment" // File/image upload
)

// EvidenceRequirement declares a required evidence artifact.
// Used in step-level enforcement (Step.RequiredEvidence) or field-level
// attachment (CollectorField.Evidence).
type EvidenceRequirement struct {
	Kind  EvidenceKind `yaml:"kind"            json:"kind"`
	Name  string       `yaml:"name"            json:"name"`
	Label string       `yaml:"label,omitempty" json:"label,omitempty"`
	Items []string     `yaml:"items,omitempty" json:"items,omitempty"` // Required for kind: checklist
}

// StepKind implementations for all concrete step types.

func (s *CLISpec) StepKind() string          { return "cli" }
func (s *ToolCallSpec) StepKind() string     { return "tool" }
func (s *IncludeSpec) StepKind() string      { return "include" }
func (s *ChoiceSpec) StepKind() string       { return "choice" }
func (s *DecisionSpec) StepKind() string     { return "decision" }
func (s *CollectorSpec) StepKind() string    { return "collector" }
func (s *HostActionSpec) StepKind() string   { return "host_action" }
func (s *HandoffSpec) StepKind() string      { return "handoff" }
func (s *BranchSpec) StepKind() string       { return "branch" }
func (s *IterateNode) StepKind() string      { return "iterate" }
func (s *ParallelNode) StepKind() string     { return "parallel" }
func (s *ApproveSpec) StepKind() string      { return "approve" }
func (s *AssertSpec) StepKind() string       { return "assert" }
func (s *CompensateSpec) StepKind() string   { return "compensate" }
func (s *WaitForEventSpec) StepKind() string { return "wait_for_event" }
func (s *EndSpec) StepKind() string          { return "end" }
func (s *NoopSpec) StepKind() string         { return "noop" }
func (s *DisplaySpec) StepKind() string      { return "display" }
