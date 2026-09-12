package schema

// StepType is the discriminated type of a step.
type StepType string

const (
	// Execution step types.
	StepTypeCLI     StepType = "cli"
	StepTypeTool    StepType = "tool"
	StepTypeInclude StepType = "include"

	// User input step types.
	StepTypeChoice     StepType = "choice"
	StepTypeDecision   StepType = "decision"
	StepTypeCollector  StepType = "collector"
	StepTypeHostAction StepType = "host_action"
	StepTypeHandoff    StepType = "handoff"

	// Flow control step types.
	StepTypeBranch   StepType = "branch"
	StepTypeIterate  StepType = "iterate"
	StepTypeParallel StepType = "parallel"

	// Governance step types.
	StepTypeApprove    StepType = "approve"
	StepTypeAssert     StepType = "assert"
	StepTypeCompensate StepType = "compensate"

	// Extension step type (v1 retained).
	StepTypeExtension StepType = "extension"

	// Synchronisation step types.
	StepTypeWaitForEvent StepType = "wait_for_event"

	// Terminal step types.
	StepTypeEnd StepType = "end"

	// Utility step types.
	StepTypeNoop    StepType = "noop"
	StepTypeDisplay StepType = "display"
	StepTypeAssign  StepType = "assign"
	StepTypeResults StepType = "results"
)

// Step is the unified step struct. The Type field discriminates which
// type-specific fields are populated.
type Step struct {
	// Common fields (available on all step types).
	ID               string                `yaml:"id"                         json:"id"`
	Type             StepType              `yaml:"type"                       json:"type"`
	Title            string                `yaml:"title,omitempty"            json:"title,omitempty"`
	Subtitle         string                `yaml:"subtitle,omitempty"         json:"subtitle,omitempty"`
	When             string                `yaml:"when,omitempty"             json:"when,omitempty"`
	Timeout          string                `yaml:"timeout,omitempty"          json:"timeout,omitempty"`
	Delay            string                `yaml:"delay,omitempty"            json:"delay,omitempty"`
	Retry            *RetryConfig          `yaml:"retry,omitempty"            json:"retry,omitempty"`
	Scope            string                `yaml:"scope,omitempty"            json:"scope,omitempty"`
	Export           []string              `yaml:"export,omitempty"           json:"export,omitempty"`
	Capture          map[string]string     `yaml:"capture,omitempty"          json:"capture,omitempty"`
	CaptureDefaults  map[string]any        `yaml:"capture_defaults,omitempty" json:"capture_defaults,omitempty"`
	Contract         *Contract             `yaml:"contract,omitempty"         json:"contract,omitempty"`
	RequiredEvidence []EvidenceRequirement `yaml:"required_evidence,omitempty" json:"required_evidence,omitempty"`
	OnError          string                `yaml:"on_error,omitempty"         json:"on_error,omitempty"`

	// Type-specific payloads — exactly one is non-nil for a valid step.
	CLI              *CLISpec          `yaml:",inline" json:"-"` // populated when Type == "cli"
	ToolCall         *ToolCallSpec     `yaml:",inline" json:"-"` // populated when Type == "tool"
	IncludeSpec      *IncludeSpec      `yaml:",inline" json:"-"` // populated when Type == "include"
	ChoiceSpec       *ChoiceSpec       `yaml:",inline" json:"-"` // populated when Type == "choice"
	DecisionSpec     *DecisionSpec     `yaml:",inline" json:"-"` // populated when Type == "decision"
	CollectorSpec    *CollectorSpec    `yaml:",inline" json:"-"` // populated when Type == "collector"
	HostActionSpec   *HostActionSpec   `yaml:",inline" json:"-"` // populated when Type == "host_action"
	HandoffSpec      *HandoffSpec      `yaml:",inline" json:"-"` // populated when Type == "handoff"
	BranchSpec       *BranchSpec       `yaml:",inline" json:"-"` // populated when Type == "branch"
	ParallelSpec     *ParallelNode     `yaml:",inline" json:"-"` // populated when Type == "parallel"
	ApproveSpec      *ApproveSpec      `yaml:",inline" json:"-"` // populated when Type == "approve"
	AssertSpec       *AssertSpec       `yaml:",inline" json:"-"` // populated when Type == "assert"
	CompensateSpec   *CompensateSpec   `yaml:",inline" json:"-"` // populated when Type == "compensate"
	WaitForEventSpec *WaitForEventSpec `yaml:",inline" json:"-"` // populated when Type == "wait_for_event"
	EndSpec          *EndSpec          `yaml:",inline" json:"-"` // populated when Type == "end"
	NoopSpec         *NoopSpec         `yaml:",inline" json:"-"` // populated when Type == "noop"
	DisplaySpec      *DisplaySpec      `yaml:",inline" json:"-"` // populated when Type == "display"
	AssignSpec       *AssignSpec       `yaml:",inline" json:"-"`
	ResultsSpec      *ResultsSpec      `yaml:",inline" json:"-"`
}

// RetryConfig configures automatic retry behaviour for a step.
type RetryConfig struct {
	Max         int    `yaml:"max"                    json:"max"`
	Interval    string `yaml:"interval,omitempty"     json:"interval,omitempty"`
	Backoff     string `yaml:"backoff,omitempty"      json:"backoff,omitempty"`
	MaxInterval string `yaml:"max_interval,omitempty" json:"max_interval,omitempty"`
	Jitter      bool   `yaml:"jitter,omitempty"       json:"jitter,omitempty"`
}

// Contract declares the behavioural contract of a step for governance.
type Contract struct {
	Effects       []string `yaml:"effects,omitempty"       json:"effects,omitempty"`
	Reads         []string `yaml:"reads,omitempty"         json:"reads,omitempty"`
	Writes        []string `yaml:"writes,omitempty"        json:"writes,omitempty"`
	Idempotent    bool     `yaml:"idempotent,omitempty"    json:"idempotent,omitempty"`
	Deterministic bool     `yaml:"deterministic,omitempty" json:"deterministic,omitempty"`
}
