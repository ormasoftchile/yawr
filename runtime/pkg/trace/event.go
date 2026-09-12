package trace

import "encoding/json"

// EventKind is the type discriminator for all trace events.
// Values match the wire format exactly.
type EventKind string

const (
	// Plan lifecycle events.
	EventKindPlanValidated EventKind = "plan.validated"

	// Run lifecycle events.
	EventKindRunStarted             EventKind = "run/started"
	EventKindRunCompleted           EventKind = "run/completed"
	EventKindRunCancelled           EventKind = "run/cancelled"
	EventKindRunFailed              EventKind = "run/failed"
	EventKindRouteTestTargetReached EventKind = "route_test/target_reached"
	EventKindRunPausedAtBoundary    EventKind = "run/paused_at_boundary"
	EventKindExecutionCommitted     EventKind = "execution/committed"

	// Step lifecycle events.
	EventKindStepStarted   EventKind = "step/started"
	EventKindStepCompleted EventKind = "step/completed"
	EventKindStepFailed    EventKind = "step/failed"
	EventKindStepSkipped   EventKind = "step/skipped"
	EventKindStepRetrying  EventKind = "step/retrying"
	EventKindStepResumed   EventKind = "step/resumed"
	EventKindStepOutput    EventKind = "step/output"
	EventKindStepDelaying  EventKind = "step/delaying"

	// External event arrival (wait_for_event).
	EventKindEventReceived EventKind = "event/received"

	// Governance events.
	EventKindGovernanceCommandChecked    EventKind = "governance/command_checked"
	EventKindGovernanceApprovalRequested EventKind = "governance/approval_requested"
	EventKindGovernanceApprovalReceived  EventKind = "governance/approval_received"
	EventKindGovernanceRedactionApplied  EventKind = "governance/redaction_applied"

	// Extension events.
	EventKindExtensionLoaded   EventKind = "extension/loaded"
	EventKindExtensionUnloaded EventKind = "extension/unloaded"

	// Tool events.
	EventKindToolInvoked   EventKind = "tool/invoked"
	EventKindToolCompleted EventKind = "tool/completed"

	// Human-in-the-loop events.
	EventKindInputPrompted EventKind = "input/prompted"
	EventKindInputReceived EventKind = "input/received"

	// Saga / compensation events.
	EventKindSagaCompensationTriggered EventKind = "saga/compensation_triggered"
	EventKindSagaCompensationCompleted EventKind = "saga/compensation_completed"

	// Iterate iteration events. Emitted by the iterate executor before/after
	// each loop body invocation. Payload carries:
	//   step_id            string  // the iterate step's ID
	//   iteration_index    int     // 1-based
	//   iteration_total    int     // total items, or -1 if unknown
	//   as                 string  // the loop variable name (spec.As)
	//   value              any     // the current item bound to `as`
	// iteration/completed additionally carries:
	//   status             string  // "completed" | "failed"
	EventKindIterateIterationStarted   EventKind = "iterate/iteration_started"
	EventKindIterateIterationCompleted EventKind = "iterate/iteration_completed"

	// Package and substitution events.
	// §Tool Packages / §Action Substitution). Payload shapes are documented on
	// the corresponding Package*Payload / Substituted*Payload types below.
	EventKindPackageResolved                EventKind = "package/resolved"
	EventKindCatalogFrozen                  EventKind = "catalog/frozen"
	EventKindToolSubstituted                EventKind = "tool/substituted"
	EventKindGovernancePackageDriftAccepted EventKind = "governance/packageDriftAccepted"
	EventKindReplayPackageDrift             EventKind = "replay/packageDrift"

	// Dynamic include events.
	// include/resolved  — emitted once per execution of a dynamic include site
	//                     that successfully resolves a child runbook.
	// include/notFound  — emitted when resolution fails (catalog miss or error).
	// replay/dynamicIncludeDrift — emitted by the replay layer when a dynamic
	//                     include pin digest mismatches the current filesystem.
	EventKindIncludeResolved           EventKind = "include/resolved"
	EventKindIncludeNotFound           EventKind = "include/notFound"
	EventKindReplayDynamicIncludeDrift EventKind = "replay/dynamicIncludeDrift"

	// MCP HTTP auth events.
	// mcp/authAttached — emitted once per authenticated HTTP request, immediately
	//                    before the request is sent. Payload: url_host and scope
	//                    only — never the token value.
	// Host-not-in-allowed_hosts is a hard error (MCP-012), not an event.
	EventKindMCPAuthAttached EventKind = "mcp/authAttached"
)

// PackageResolvedPayload is the payload of package/resolved
// (§Package and Substitution Events). Emitted once per resolved package, at
// plan time, before any run/started side effects.
type PackageResolvedPayload struct {
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Digest            string   `json:"digest"`
	Root              string   `json:"root"`
	External          bool     `json:"external"`
	ConstraintSources []string `json:"constraintSources"`
	// Origin records where this package's effective requires[] binding
	// came from: "project" (the project's own .yawr/config.yaml) or
	// "package-map" (a --package-map override replaced the project
	// binding for this package name). Omitted (empty) when the run did
	// not go through the --package-map merge path at all. §7.4 makes
	// package provenance part of the evidence record; an override of
	// where a package came from is exactly the fact an auditor needs
	// so drift remains part of the evidence record.
	Origin string `json:"origin,omitempty"`
}

// CatalogFrozenPayload is the payload of catalog/frozen, emitted exactly
// once per run immediately after Phase C (catalog construction) completes
// and before Phase B (binding) begins.
type CatalogFrozenPayload struct {
	CatalogDigest string         `json:"catalogDigest"`
	ToolCount     int            `json:"toolCount"`
	Tiers         map[string]int `json:"tiers"`
}

// EffectiveGovernancePayload is the composed (effective) governance policy
// recorded on a tool/substituted event -- the audit question is "what
// policy actually applied", so it is recorded explicitly rather than
// derived after the fact.
type EffectiveGovernancePayload struct {
	RequireApproval bool     `json:"require_approval"`
	DenyCommands    []string `json:"deny_commands"`
	DenyEnvVars     []string `json:"deny_env_vars"`
	AllowCommands   []string `json:"allow_commands"`
	Redact          []string `json:"redact"`
	Capabilities    []string `json:"capabilities"`
}

// ToolSubstitutedPayload is the payload of tool/substituted, emitted for
// every substitution frame entered.
// §Action Substitution).
type ToolSubstitutedPayload struct {
	Tool                string                     `json:"tool"`
	Action              string                     `json:"action"`
	SubstituteDigest    string                     `json:"substituteDigest"`
	Depth               int                        `json:"depth"`
	EffectiveGovernance EffectiveGovernancePayload `json:"effectiveGovernance"`
}

// GovernancePackageDriftAcceptedPayload is the payload of
// governance/packageDriftAccepted, emitted whenever
// `yawr exec --resume --allow-package-drift` is used and a package/catalog
// digest mismatch was detected. Drift acceptance is an auditable act.
type GovernancePackageDriftAcceptedPayload struct {
	ExpectedCatalogDigest string `json:"expectedCatalogDigest"`
	ActualCatalogDigest   string `json:"actualCatalogDigest"`
	Operator              string `json:"operator"`
}

// ReplayPackageDriftPayload is the payload of replay/packageDrift, emitted
// (non-fatally) during replay mode when a recorded package or catalog
// digest does not match the current filesystem. Name is "*" for a
// catalog-level (rather than per-package) mismatch.
type ReplayPackageDriftPayload struct {
	Name           string `json:"name"`
	ExpectedDigest string `json:"expectedDigest"`
	ActualDigest   string `json:"actualDigest"`
}

// IncludeResolvedPayload is the payload of include/resolved,
// emitted once per execution of a dynamic include site that resolves
// successfully. An iterate body that resolves different targets per
// iteration emits one event per iteration.
type IncludeResolvedPayload struct {
	StepID              string                     `json:"step_id"`
	RenderedRef         string                     `json:"rendered_ref"`
	QualifiedID         string                     `json:"qualified_id"`
	AbsPath             string                     `json:"abs_path"`
	PackageName         string                     `json:"package_name"`
	PackageVersion      string                     `json:"package_version"`
	FileDigest          string                     `json:"file_digest"`
	PackageDigest       string                     `json:"package_digest"`
	Depth               int                        `json:"depth"`
	EffectiveGovernance EffectiveGovernancePayload `json:"effective_governance"`
}

// IncludeNotFoundPayload is the payload of include/notFound, emitted when a
// dynamic include site fails to resolve its rendered runbook_ref.
type IncludeNotFoundPayload struct {
	StepID      string `json:"step_id"`
	RenderedRef string `json:"rendered_ref"`
	ErrorCode   string `json:"error_code"`
	Reason      string `json:"reason"`
}

// ReplayDynamicIncludeDriftPayload is the payload of
// replay/dynamicIncludeDrift, emitted by the replay layer when a resolved
// dynamic include's recorded digest does not match the current filesystem.
type ReplayDynamicIncludeDriftPayload struct {
	StepID         string `json:"step_id"`
	QualifiedID    string `json:"qualified_id"`
	ExpectedDigest string `json:"expectedDigest"`
	ActualDigest   string `json:"actualDigest"`
}

// TraceEvent is the canonical event envelope written to the JSONL trace file.
// Every event carries this envelope regardless of kind.
type TraceEvent struct {
	EventID   string          `json:"event_id"`
	RunID     string          `json:"run_id"`
	RunbookID string          `json:"runbook_id"`
	Timestamp string          `json:"timestamp"` // RFC3339 with microsecond precision
	Kind      EventKind       `json:"kind"`
	Sequence  int64           `json:"sequence"`
	Payload   json.RawMessage `json:"payload"`

	// HMAC-SHA256 signature over the canonical JSON of all other fields.
	// Present only when YAWR_TRACE_KEY is set.
	Signature string `json:"sig,omitempty"`
}
