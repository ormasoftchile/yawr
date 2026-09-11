package main

// reachability_registry_test.go — the enforcement mechanism for the reachability gate.
//
// Every schema field, CLI flag, or config key that has production behavior must
// appear here as either:
//   - statusReachable with a non-nil TestFunc that fails if the production read is removed, or
//   - statusKnownDead with a non-empty DeadReason citing the item that will wire it.
//
// There is no silent option. See specs/reachability-gate.md for the full convention.

import "testing"

// reachabilityStatus distinguishes live-and-proven features from explicitly-known-dead ones.
type reachabilityStatus int

const (
	statusReachable reachabilityStatus = iota
	statusKnownDead
)

// reachabilityEntry declares one schema field or CLI flag's reachability status.
type reachabilityEntry struct {
	// Feature is the human-readable name of the field (e.g. "AllowedEnvironments").
	Feature string

	// Status is either statusReachable or statusKnownDead.
	Status reachabilityStatus

	// TestFunc is the reachability probe. Required when Status == statusReachable.
	// It must call runRun() and assert a behavioral difference (exit code,
	// stdout/stderr, trace event) caused solely by the field under test.
	// It must FAIL if the production read of the field is removed.
	TestFunc func(t *testing.T)

	// DeadReason is required when Status == statusKnownDead. It must name the
	// Phase/Item that will wire the field and convert the entry to statusReachable.
	DeadReason string
}

// reachabilityRegistry is the authoritative list of schema fields and CLI features
// that require reachability proof.
//
// Four known-dead regressions are recorded here as the founding entries:
//   - AllowedEnvironments       now reachable via PLAN-010; probe below.
//   - RequiresCapabilities      DEAD; no active item covers it.
//   - ProfileToolOverride.Endpoint DEAD; David's Phase 1B Item 2 will wire it.
//   - Contract.Idempotent       DEAD; only read in tests, never in production logic.
var reachabilityRegistry = []reachabilityEntry{
	{
		// AllowedEnvironments is read by the Tier 0 PLAN-010 preflight check in
		// internal/planner/preflight.go. The planner config receives the profile via
		// cmd/yawr/run.go. This probe confirms the full wiring path is intact.
		Feature:  "AllowedEnvironments",
		Status:   statusReachable,
		TestFunc: testCLI_AllowedEnvironments_Reachable,
	},
	{
		Feature:    "RequiresCapabilities",
		Status:     statusKnownDead,
		DeadReason: "KNOWN-DEAD: tool.Governance.RequiresCapabilities is parsed and schema-validated but no production code branch reads it. No Phase 1B item covers activation. Must be wired before Phase 2 tooling work that depends on capability-gating.",
	},
	{
		// ProfileToolOverride.Endpoint is now execution-wired (Phase 1B Item 2,
		// commit 85bfa4a). Wiring path: cmd/yawr/run.go → adapter.WireOptions.Profile
		// → internal/adapter/wire.go SetProfile → internal/tool/runtime.go effectiveURL
		// in the mcp-http Invoke branch. PLAN-013 fires at plan time if the resolved
		// endpoint host is not in def.Auth.AllowedHosts — this is the observable
		// behavior the probe uses to confirm the field is read.
		Feature:  "ProfileToolOverride.Endpoint",
		Status:   statusReachable,
		TestFunc: testCLI_ProfileToolOverrideEndpoint_Reachable,
	},
	{
		Feature:  "TransportVSCodeMCP",
		Status:   statusReachable,
		TestFunc: testCLI_VSCodeMCPTransport_Reachable,
	},
	{
		Feature:    "Contract.Idempotent",
		Status:     statusKnownDead,
		DeadReason: "KNOWN-DEAD: schema.Contract.Idempotent is declared in pkg/schema/step.go and is referenced in unit tests (approval_enforcement_test.go:380) but no production code path branches on its value. Activation requires retry/idempotency enforcement in the engine. Planned for Phase 2.",
	},
	{
		// ToolAction.Outputs (non-substituted enforcement) is read by the
		// enforceOutputContract call in internal/executor/tool.go. The call
		// fires post-invoke for every non-substituted action whose tool def
		// declares an outputs: block. This probe confirms the wiring is intact
		// by running a mock mcp-http tool whose outputs: contract is violated
		// by the server's response; if the enforcement call is removed, the
		// step succeeds and the probe fails.
		Feature:  "ToolAction.Outputs (non-substituted enforcement)",
		Status:   statusReachable,
		TestFunc: testCLI_OutputContractEnforcement_Reachable,
	},
	{
		// vscode_tool is read by ValidateVSCodeToolActions, called from
		// ParseToolFile in internal/tool/scan.go. The check fires at scan time
		// for every tool file that declares vscode_tool on an action. This
		// probe confirms the wiring is intact by supplying a vscode-mcp tool
		// whose action declares vscode_tool: "" (explicitly empty). If the
		// ValidateVSCodeToolActions call is removed from ParseToolFile, the
		// empty value passes validation and the run reaches the transport layer
		// (exitRuntime) instead of failing at scan time (exitValidation).
		Feature:  "ToolAction.VscodeTool",
		Status:   statusReachable,
		TestFunc: testCLI_VscodeTool_Reachable,
	},
	{
		// ToolAction.VSCodeInput is read by ValidateVSCodeInputActions (scan
		// time, from: validation) and by applyVSCodeInputAdaptation (runtime,
		// before bridge wire). This probe confirms the scan-time path: a
		// vscode-mcp tool declares vscode_input with from: naming a
		// non-existent arg. ValidateVSCodeInputActions must reject it at scan
		// time (exitValidation). If the call is removed from ParseToolFile,
		// the bad from: passes validation and the run reaches the transport
		// layer — returning exitRuntime rather than exitValidation, failing the
		// probe.
		Feature:  "ToolAction.VSCodeInput",
		Status:   statusReachable,
		TestFunc: testCLI_VSCodeInput_Reachable,
	},
	{
		// serve --package-map is read in runServe (load at startup) and by
		// newServingToolRegistry (extra-path override). We probe it via the
		// binding-level unit test (TestServe_PackageMap_DeterminesToolBinding)
		// which is gated here so deleting the load path is caught.
		Feature:  "serve --package-map (ExtraToolScanPaths)",
		Status:   statusReachable,
		TestFunc: testCLI_ServePackageMap_Reachable,
	},
	{
		// serve --package-map requires: bindings are retained as per-run
		// catalog inputs. The reachability probe prevents regressing to the
		// old startup rejection or silent discard behavior.
		Feature:  "serve --package-map requires: catalog input",
		Status:   statusReachable,
		TestFunc: testCLI_ServePackageMapRequires_Reachable,
	},
}

// TestCLI_ReachabilityGate iterates reachabilityRegistry and enforces that every
// entry is either:
//   - statusReachable with a non-nil TestFunc (which is run as a subtest), or
//   - statusKnownDead with a non-empty DeadReason (logged, not failed).
//
// Any entry that violates these invariants fails CI immediately, making it
// impossible to silently introduce a dead field.
func TestCLI_ReachabilityGate(t *testing.T) {
	seen := make(map[string]bool)
	for _, entry := range reachabilityRegistry {
		if seen[entry.Feature] {
			t.Errorf("duplicate reachability entry: %q — each feature must appear exactly once", entry.Feature)
			continue
		}
		seen[entry.Feature] = true

		entry := entry // capture loop variable
		switch entry.Status {
		case statusReachable:
			t.Run(entry.Feature, func(t *testing.T) {
				if entry.TestFunc == nil {
					t.Fatalf("feature %q is marked statusReachable but TestFunc is nil — "+
						"add a CLI integration test in reachability_probes_test.go that "+
						"calls runRun() and fails if the production read is removed", entry.Feature)
				}
				entry.TestFunc(t)
			})
		case statusKnownDead:
			if entry.DeadReason == "" {
				t.Errorf("feature %q is marked statusKnownDead but DeadReason is empty — "+
					"document the Phase/Item that will wire it (see specs/reachability-gate.md)", entry.Feature)
				continue
			}
			// Log the known-dead entry so it is visible in test output and
			// cannot be silently ignored. This is a pass, not a failure.
			t.Logf("KNOWN-DEAD [%s]: %s", entry.Feature, entry.DeadReason)
		default:
			t.Errorf("feature %q has unknown reachabilityStatus %d", entry.Feature, entry.Status)
		}
	}
}
