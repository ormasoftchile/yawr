package otel

// Attribute key constants following OTel semantic conventions and yawr conventions.
// All yawr-specific attributes use the "yawr.*" prefix.
const (
	// Run-level attributes (all spans).
	AttrRunID       = "yawr.run.id"
	AttrRunbookPath = "yawr.runbook.path"
	AttrRunMode     = "yawr.run.mode"
	AttrActor       = "yawr.actor"

	// Step-specific attributes.
	AttrStepID       = "yawr.step.id"
	AttrStepKind     = "yawr.step.kind"
	AttrStepIndex    = "yawr.step.index"
	AttrStepDuration = "yawr.step.duration_ms"

	// Tool-specific attributes.
	AttrToolName      = "yawr.tool.name"
	AttrToolAction    = "yawr.tool.action"
	AttrToolTransport = "yawr.tool.transport"

	// Input-specific attributes.
	AttrInputType     = "yawr.input.type"
	AttrInputProvider = "yawr.input.provider"
)
