package engine

// StepSpec is the type-safe payload for a resolved step.
// Each concrete step type from pkg/schema implements this interface.
// The parser sets the concrete type; the engine dispatches on it.
type StepSpec interface {
	// StepKind returns the step type discriminator (e.g., "cli", "parallel", "wait_for_event").
	StepKind() string
}
