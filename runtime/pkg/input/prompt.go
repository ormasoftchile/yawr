package input

import "context"

// PromptProvider collects user input for interactive steps.
// Implementations: TerminalInputProvider (stdin), FakePromptProvider (tests).
type PromptProvider interface {
	// PromptChoice presents options and returns the selected value(s).
	PromptChoice(ctx context.Context, req ChoiceRequest) (*ChoiceResponse, error)

	// PromptDecision presents routes and returns the selected route label.
	PromptDecision(ctx context.Context, req DecisionRequest) (*DecisionResponse, error)

	// PromptForm presents a multi-field form and returns all field values.
	PromptForm(ctx context.Context, req FormRequest) (*FormResponse, error)
}

// ChoiceRequest describes a choice prompt.
type ChoiceRequest struct {
	StepID   string
	Prompt   string
	Options  []Option
	Default  string
	Multiple bool
	Min      int
	Max      int
}

// Option is a single selectable option.
type Option struct {
	Label string
	Value string
	Hint  string
}

// ChoiceResponse holds the user's selection.
type ChoiceResponse struct {
	Selected []string
}

// DecisionRequest describes a decision prompt.
type DecisionRequest struct {
	StepID string
	Prompt string
	Routes []Route
}

// Route is a single route option.
type Route struct {
	Label string
	Hint  string
}

// DecisionResponse holds the user's route selection.
type DecisionResponse struct {
	Label string
}

// FormRequest describes a multi-field form.
type FormRequest struct {
	StepID string
	Prompt string
	Fields []FormField
}

// FormField describes one resolved field in a form.
type FormField struct {
	Name       string
	Type       string
	Label      string
	Required   bool
	Default    any
	Hint       string
	Options    []Option
	Multiple   bool
	Validation *FormValidation
	Ephemeral  bool
	// FromStep, when set, is a hint to the UI layer to pre-populate this field
	// with the captured stdout of the named preceding CLI step.
	FromStep string
}

// FormValidation contains constraints applied to a submitted field value.
type FormValidation struct {
	MinLength *int    `json:"min_length,omitempty"`
	MaxLength *int    `json:"max_length,omitempty"`
	Pattern   string  `json:"pattern,omitempty"`
	Format    string  `json:"format,omitempty"`
	Min       any     `json:"min,omitempty"`
	Max       any     `json:"max,omitempty"`
	Step      float64 `json:"step,omitempty"`
}

// FormResponse holds all field values.
type FormResponse struct {
	Values map[string]any
}
