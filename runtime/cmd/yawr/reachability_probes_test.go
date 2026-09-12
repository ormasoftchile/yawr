package main

// reachability_probes_test.go - CLI reachability probe functions.
//
// Each function here is a reachability probe: it calls runRun() and asserts
// that a specific schema field or CLI flag changes observable CLI behavior
// (exit code, stdout/stderr). If the production read of the field is removed,
// the probe MUST fail.
//
// Probes are unexported and referenced from reachabilityRegistry in
// reachability_registry_test.go. They are run as subtests of
// TestCLI_ReachabilityGate. See docs/reachability-gate.md for the convention.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testCLI_AllowedEnvironments_Reachable proves that
// tool.Governance.AllowedEnvironments is read by production code.
//
// Mechanism: the Tier 0 PLAN-010 check in internal/planner/preflight.go reads
// AllowedEnvironments and compares it to the profile's Context field. When the
// profile context is not in the allowed-environments list, the planner returns a
// planning error (exitValidation). This check reaches AllowedEnvironments only if
// cmd/yawr/run.go passes the loaded RuntimeProfile to plannerpkg.Config.Profile -
// the exact wiring gap that was dead in production before this was caught.
//
// Negative-control contract: if AllowedEnvironments is no longer read by any
// production code path reachable from runRun(), the planning check will not fire
// and the run will exit with exitSuccess (or exitRuntime), causing this test to
// fail with the diagnostic below.
func testCLI_AllowedEnvironments_Reachable(t *testing.T) {
	t.Helper()
	// Tool restricted to "ci" context only; profile says "cli-operator" → mismatch.
	workDir, profilePath := setupPlan010Dir(t, "cli-operator", "attended", "ci")
	chdirForTest(t, workDir)

	code := runRun([]string{
		"runbook.yaml",
		"--profile", profilePath,
		"--output", "quiet",
		"--trace", filepath.Join(workDir, "trace.jsonl"),
	})

	if code == exitSuccess {
		t.Fatal("AllowedEnvironments reachability FAILED: PLAN-010 did not fire " +
			"for a profile context not in allowed-environments - production code is " +
			"no longer reading this field. Check that cmd/yawr/run.go passes Profile " +
			"to plannerpkg.Config and that internal/planner/preflight.go still reads " +
			"tool.Governance.AllowedEnvironments in the Tier 0 check.")
	}
	if code != exitValidation {
		t.Errorf("expected exitValidation (%d) from PLAN-010, got %d - "+
			"the check may be firing but with the wrong exit code", exitValidation, code)
	}
}

// testCLI_ProfileToolOverrideEndpoint_Reachable proves that
// ProfileToolOverride.Endpoint is read by production code.
//
// Mechanism: PLAN-013 in internal/planner/preflight.go reads the profile's
// endpoint override for each mcp-http tool and validates that the override
// host is in def.Auth.AllowedHosts. When the override points to a host
// outside the allowed list, the planner returns a planning error (exitValidation).
//
// The probe constructs a runbook that uses an mcp-http tool whose allowed_hosts
// is [icm-prod.azure-api.net], then supplies a profile that overrides the
// endpoint to https://attacker.example.com/v1/ (not in allowed_hosts). The plan
// step must fail with PLAN-013.
//
// Negative-control contract: if ProfileToolOverride.Endpoint is no longer read
// (e.g. the effectiveURL = override.Endpoint line is removed from runtime.go or
// the PLAN-013 check is deleted from preflight.go), PLAN-013 will not fire and
// the run will proceed to a different exit code, failing this probe.
func testCLI_ProfileToolOverrideEndpoint_Reachable(t *testing.T) {
	t.Helper()

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs dir: %v", err)
	}
	chdirForTest(t, absDir)

	// Write a .tool.yaml for an mcp-http tool restricted to icm-prod.azure-api.net.
	toolDir := filepath.Join(dir, "tools", "mcp-stub")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatalf("mkdir tools/mcp-stub: %v", err)
	}
	writeFile(t, filepath.Join(toolDir, "mcp-stub.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: mcp-stub
  version: "1.0.0"
transport:
  mode: mcp-http
  url: https://icm-prod.azure-api.net/v1/
auth:
  allowed_hosts:
    - icm-prod.azure-api.net
  scope: api://icm-prod/.default
actions:
  - name: run
    description: "stub mcp-http action"
`)

	// Write a runbook that calls mcp-stub.
	writeFile(t, filepath.Join(absDir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: plan013-probe
name: "PLAN-013 reachability probe"
flow:
  - step:
      id: call
      type: tool
      tool:
        name: mcp-stub
        action: run
`)

	// Write a profile that overrides the endpoint to a disallowed host.
	profilePath := filepath.Join(dir, "attacker.profile.yaml")
	writeFile(t, profilePath, `apiVersion: yawr.runtime-profile/v1
id: attacker-override
context: cli-operator
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: true
    allow_destructive: true
tools:
  mcp-stub:
    endpoint: https://attacker.example.com/v1/
`)

	code := runRun([]string{
		filepath.Join(absDir, "runbook.yaml"),
		"--profile", profilePath,
		"--output", "quiet",
		"--trace", filepath.Join(absDir, "trace.jsonl"),
		"--package-map", filepath.Join(dir, "tools", "mcp-stub") + "=mcp-stub",
	})

	if code == exitSuccess {
		t.Fatal("ProfileToolOverride.Endpoint reachability FAILED: PLAN-013 did not fire " +
			"for an endpoint override to a host outside allowed_hosts - production code " +
			"is no longer reading ProfileToolOverride.Endpoint. Check that " +
			"internal/planner/preflight.go still reads the profile tool endpoint and that " +
			"internal/tool/runtime.go still sets effectiveURL = override.Endpoint.")
	}
	if code != exitValidation {
		t.Errorf("expected exitValidation (%d) from PLAN-013, got %d - "+
			"the check may be firing but with the wrong exit code", exitValidation, code)
	}
}

// testCLI_VSCodeMCPTransport_Reachable proves that transport mode vscode-mcp
// is wired into the scan and validation path.
//
// Mechanism: a .tool.yaml with mode: vscode-mcp is scanned. ValidateTransportConfig
// must accept it. A runbook that calls it is planned. PLAN-013 does NOT fire
// (no auth block). The tool is invoked - but since no bridge is running, the
// transport error (connection refused to 127.0.0.1:7779) is the expected outcome.
// The probe asserts that we reach the transport layer (exitRuntime), not a scan
// or validation failure (exitValidation).
//
// The negative control: if TransportVSCodeMCP is removed from mapTransport in
// scan.go, ScanDir will fail for any .tool.yaml with mode: vscode-mcp, returning
// exitValidation (scan error) rather than exitRuntime. The probe fails directionally.
func testCLI_VSCodeMCPTransport_Reachable(t *testing.T) {
	t.Helper()

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs dir: %v", err)
	}
	chdirForTest(t, absDir)

	toolDir := filepath.Join(absDir, "tools", "vscode-stub")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(toolDir, "vscode-stub.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: vscode-stub
  version: "1.0.0"
transport:
  mode: vscode-mcp
actions:
  - name: run
    description: "stub vscode-mcp action"
    classification: read-only
`)

	writeFile(t, filepath.Join(absDir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: vscode-mcp-probe
name: "vscode-mcp reachability probe"
flow:
  - step:
      id: call
      type: tool
      tool:
        name: vscode-stub
        action: run
`)

	profilePath := filepath.Join(absDir, "probe.profile.yaml")
	writeFile(t, profilePath, `apiVersion: yawr.runtime-profile/v1
id: probe-profile
context: cli-operator
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: true
    allow_destructive: true
`)

	stderr := captureStderr(t, func() int {
		return runRun([]string{
			filepath.Join(absDir, "runbook.yaml"),
			"--profile", profilePath,
			"--output", "quiet",
			"--trace", filepath.Join(absDir, "trace.jsonl"),
		})
	})
	code := runLast

	if code == exitValidation || strings.Contains(stderr, "unsupported transport \"vscode-mcp\"") {
		t.Fatal("TransportVSCodeMCP reachability FAILED: mode vscode-mcp caused a " +
			"validation/scan failure or never reached the transport layer - check that " +
			"TransportVSCodeMCP is added to mapTransport in scan.go and case " +
			"schema.TransportVSCodeMCP is in ValidateTransportConfig in validate_transport.go")
	}
	t.Logf("vscode-mcp probe: exit code %d (expected transport layer reach; stderr=%q)", code, stderr)
}

// testCLI_OutputContractEnforcement_Reachable proves that the declared
// outputs: contract on a non-substituted tool action is enforced at runtime
// (internal/executor/tool.go enforceOutputContract).
//
// Mechanism: a native tool declares outputs: { result: string }. The native
// transport never populates res.Output (it only captures stdout/stderr as
// plain text), so the declared "result" field is always missing at runtime.
// The executor must detect this violation and fail the step, causing exitFailure.
//
// Negative-control contract: if the enforceOutputContract call is removed
// from internal/executor/tool.go, the step succeeds despite the missing
// declared output (no Output map is checked), the runbook reaches its end
// step, and the CLI exits with exitSuccess — causing this probe to fail.
func testCLI_OutputContractEnforcement_Reachable(t *testing.T) {
	t.Helper()

	// "go version" is always available in a Go test environment, exits 0,
	// and writes to stdout. Native transport never parses stdout into
	// res.Output, so the declared "result" output is always absent.
	dir := makeWorkDir(t)
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs(dir): %v", err)
	}
	dir = abs

	writeFile(t, filepath.Join(dir, "cprobe-tool.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: cprobe-tool
  version: "1.0.0"
transport:
  mode: native
  command: go
actions:
  - name: check
    argv:
      - version
    description: "Output contract enforcement probe (native transport)"
    classification: read-only
    outputs:
      result:
        type: string
`)

	writeFile(t, filepath.Join(dir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: output-contract-probe
name: "output contract enforcement reachability probe"
flow:
  - step:
      id: call
      type: tool
      tool:
        name: cprobe-tool
        action: check
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	profilePath := filepath.Join(dir, "probe.profile.yaml")
	writeFile(t, profilePath, `apiVersion: yawr.runtime-profile/v1
id: contract-probe-profile
context: cli-operator
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: false
    allow_destructive: false
`)

	chdirForTest(t, dir)

	code := runRun([]string{
		"runbook.yaml",
		"--profile", profilePath,
		"--output", "quiet",
		"--trace", filepath.Join(dir, "trace.jsonl"),
	})

	if code == exitSuccess {
		t.Fatal("OutputContractEnforcement reachability FAILED: the step succeeded " +
			"despite declared output 'result' (type: string) not being returned by " +
			"the native transport. The enforceOutputContract call has been removed or " +
			"bypassed in internal/executor/tool.go. Restore the 'if resolvedActionDef != nil' " +
			"block after the runtime.Invoke result is assembled in Execute.")
	}
	if code != exitFailure {
		t.Logf("OutputContractEnforcement probe: expected exitFailure (%d) for missing "+
			"declared output, got %d (non-exitSuccess satisfies the reachability "+
			"invariant; check trace.jsonl for step error details)", exitFailure, code)
	}
}

// testCLI_VscodeTool_Reachable proves that ToolAction.VscodeTool is read by
// production code (ValidateVSCodeToolActions in internal/tool/scan.go).
//
// Mechanism: a vscode-mcp tool declares vscode_tool: "" on an action (an
// explicitly empty string). ValidateVSCodeToolActions must reject this at scan
// time with a validation error, causing exitValidation. An absent vscode_tool
// is valid (it falls back to the logical name), so only an explicit empty
// value is used as the trigger.
//
// Negative-control contract: if the ValidateVSCodeToolActions call is removed
// from ParseToolFile, the empty value is silently accepted, the tool is
// scanned, and the runbook proceeds to the transport layer where it fails
// because no bridge is running (exitRuntime). The probe expects exitValidation
// and gets exitRuntime — failing as intended.
func testCLI_VscodeTool_Reachable(t *testing.T) {
	t.Helper()

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs dir: %v", err)
	}
	chdirForTest(t, absDir)

	toolDir := filepath.Join(absDir, "tools", "vscode-vtool-probe")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(toolDir, "vtool-probe.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: vtool-probe
  version: "1.0.0"
transport:
  mode: vscode-mcp
actions:
  - name: run
    description: "vscode_tool reachability probe"
    classification: read-only
    vscode_tool: ""
`)

	writeFile(t, filepath.Join(absDir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: vscode-tool-probe
name: "vscode_tool reachability probe"
flow:
  - step:
      id: call
      type: tool
      tool:
        name: vtool-probe
        action: run
`)

	profilePath := filepath.Join(absDir, "probe.profile.yaml")
	writeFile(t, profilePath, `apiVersion: yawr.runtime-profile/v1
id: probe-profile
context: cli-operator
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: true
    allow_destructive: true
`)

	code := runRun([]string{
		filepath.Join(absDir, "runbook.yaml"),
		"--profile", profilePath,
		"--output", "quiet",
		"--trace", filepath.Join(absDir, "trace.jsonl"),
	})

	if code != exitValidation {
		t.Fatalf("VscodeTool reachability FAILED: expected exitValidation (%d) from "+
			"ValidateVSCodeToolActions rejecting vscode_tool: \"\" on action 'run', "+
			"got %d. If the validation call has been removed from ParseToolFile in "+
			"internal/tool/scan.go, restore: ValidateVSCodeToolActions(def.Transport, def.Actions).",
			exitValidation, code)
	}
	t.Logf("vscode_tool probe: exit code %d = exitValidation (ValidateVSCodeToolActions is active)", code)
}

// testCLI_VSCodeInput_Reachable proves that ToolAction.VSCodeInput.from is
// read by production code (ValidateVSCodeInputActions in
// internal/tool/scan.go).
//
// Mechanism: a vscode-mcp tool declares vscode_input with from: pointing to
// a non-declared arg ("nonexistent"). ValidateVSCodeInputActions must reject
// this at scan time (exitValidation).
//
// Negative-control contract: if the ValidateVSCodeInputActions call is
// removed from ParseToolFile, the invalid from: passes validation and the
// run proceeds to the transport layer, producing exitRuntime (no bridge
// running) — not exitValidation. The probe expects exitValidation; getting
// exitRuntime fails it.
func testCLI_VSCodeInput_Reachable(t *testing.T) {
	t.Helper()

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs dir: %v", err)
	}
	chdirForTest(t, absDir)

	toolDir := filepath.Join(absDir, "tools", "vscode-input-probe")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// vscode_input.incidentId.from points to "nonexistent" which is not a
	// declared arg — ValidateVSCodeInputActions must catch this at scan time.
	writeFile(t, filepath.Join(toolDir, "input-probe.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: input-probe
  version: "1.0.0"
transport:
  mode: vscode-mcp
actions:
  - name: get-incident
    description: "vscode_input reachability probe"
    classification: read-only
    args:
      incident_id:
        type: string
        required: true
    vscode_input:
      incidentId:
        from: nonexistent
        coerce: integer
        required: true
`)

	writeFile(t, filepath.Join(absDir, "runbook.yaml"), `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: vscode-input-probe
name: "vscode_input reachability probe"
flow:
  - step:
      id: call
      type: tool
      tool:
        name: input-probe
        action: get-incident
        args:
          incident_id: "42"
`)

	profilePath := filepath.Join(absDir, "probe.profile.yaml")
	writeFile(t, profilePath, `apiVersion: yawr.runtime-profile/v1
id: probe-profile
context: cli-operator
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: true
    allow_destructive: true
`)

	code := runRun([]string{
		filepath.Join(absDir, "runbook.yaml"),
		"--profile", profilePath,
		"--output", "quiet",
		"--trace", filepath.Join(absDir, "trace.jsonl"),
	})

	if code != exitValidation {
		t.Fatalf("VSCodeInput reachability FAILED: expected exitValidation (%d) from "+
			"ValidateVSCodeInputActions rejecting from: nonexistent (undeclared arg), "+
			"got %d. If the call was removed from ParseToolFile in "+
			"internal/tool/scan.go, restore: ValidateVSCodeInputActions(def.Transport, def.Actions).",
			exitValidation, code)
	}
	t.Logf("vscode_input probe: exit code %d = exitValidation (ValidateVSCodeInputActions is active)", code)
}

// testCLI_ServePackageMap_Reachable proves that serve --package-map
// (ExtraToolScanPaths) is read by newServingToolRegistry.
//
// Mechanism: we call newServingToolRegistry with a specific extra-path dir
// and verify the tool from that dir was bound. If ExtraToolScanPaths wiring
// is removed, the extra path is never scanned, and the registry retains
// whatever the base scan happened to find — failing the assertion.
func testCLI_ServePackageMap_Reachable(t *testing.T) {
	t.Helper()

	root := t.TempDir()
	pmDir := filepath.Join(root, "pm-override")
	if err := os.MkdirAll(pmDir, 0o755); err != nil {
		t.Fatalf("mkdir pmDir: %v", err)
	}

	const toolName = "pm-probe-tool"
	// Write an initial version in root (base scan).
	writeFile(t, filepath.Join(root, toolName+".tool.yaml"),
		"apiVersion: yawr.tool/v1\nmeta:\n  name: "+toolName+"\n  version: \"1.0.0\"\ntransport:\n  mode: vscode-mcp\nactions:\n  - name: call\n    description: \"base-scan\"\n    classification: read-only\n")
	// Write the override version in pmDir.
	writeFile(t, filepath.Join(pmDir, toolName+".tool.yaml"),
		"apiVersion: yawr.tool/v1\nmeta:\n  name: "+toolName+"\n  version: \"1.0.0\"\ntransport:\n  mode: vscode-mcp\nactions:\n  - name: call\n    description: \"pm-override\"\n    classification: read-only\n")

	reg, err := newServingToolRegistry(root, pmDir)
	if err != nil {
		t.Fatalf("newServingToolRegistry: %v", err)
	}
	key := toolName + "/call"
	def, ok := reg.tools[key]
	if !ok {
		t.Fatalf("serve --package-map reachability FAILED: tool %q not in registry", key)
	}
	desc := def.Actions["call"].Description
	if desc != "pm-override" {
		t.Fatalf("serve --package-map reachability FAILED: expected description %q "+
			"(from pm-override dir), got %q. The ExtraToolScanPaths wiring in "+
			"newServingToolRegistry (cmd/yawr/serve.go) is not reading the extra paths.",
			"pm-override", desc)
	}
	t.Logf("serve --package-map probe: bound description = %q (pm-override wins over base-scan)", desc)
}

// testCLI_ServePackageMapRequires_Reachable proves that requires: bindings in
// --package-map are retained as serve catalog inputs, not rejected or silently
// discarded.
func testCLI_ServePackageMap_Requires_Reachable(t *testing.T) {
	t.Helper()

	dir := makeWorkDir(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs dir: %v", err)
	}

	pmPath := filepath.Join(absDir, "requires-pm.yaml")
	writeFile(t, pmPath, "apiVersion: yawr.config/v1\nrequires:\n  - package: acme.tools\n    version: \"^1.0.0\"\n    path: ./vendor/pkg\n")

	pmCfg, err := loadPackageMap(pmPath)
	if err != nil {
		t.Fatalf("loadPackageMap: %v", err)
	}
	merged, _ := mergePackageBindings(nil, pmCfg.Requires)
	if len(merged) != 1 || merged[0].Package != "acme.tools" || merged[0].Path != "./vendor/pkg" {
		t.Fatalf("serve --package-map requires: reachability FAILED: binding not retained: %+v", merged)
	}
	t.Logf("serve --package-map requires: binding retained for per-run catalog input ✓")
}

// testCLI_ServePackageMapRequires_Reachable is the registry-matching alias.
func testCLI_ServePackageMapRequires_Reachable(t *testing.T) {
	testCLI_ServePackageMap_Requires_Reachable(t)
}
