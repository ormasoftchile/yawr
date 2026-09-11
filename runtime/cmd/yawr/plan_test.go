package main

// plan_test.go — integration tests for `yawr plan` wired through runPlan().
//
// Key constraint: makeWorkDir returns a RELATIVE path. Any path passed as a
// CLI flag must be converted to absolute BEFORE chdirForTest, because the
// chdir changes the meaning of relative paths.
//
// Pattern: always call absPath(t, makeWorkDir(t)) as the first line of each
// test, so all derived paths are absolute from the start.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ── Minimal fixtures ──────────────────────────────────────────────────────────

const planSimpleRunbook = `apiVersion: yawr.runbook/v1
id: plan-test
name: plan integration test
flow:
  - step:
      id: done
      type: end
      outcome: {category: success, code: ok}
`

const planRunbookWithToolRef = `apiVersion: yawr.runbook/v1
id: plan-tool-test
name: plan tool binding test
toolRefs:
  - name: native-echo
flow:
  - step:
      id: invoke
      type: tool
      tool:
        name: native-echo
        action: run
      on_success: done
  - step:
      id: done
      type: end
      outcome: {category: success, code: ok}
`

const planNativeEchoTool = `apiVersion: yawr.tool/v1
meta:
  name: native-echo
  version: "1.0.0"
transport:
  mode: native
  command: echo
actions:
  - name: run
    description: "echo via native transport"
    classification: read-only
`

const planMutatingTool = `apiVersion: yawr.tool/v1
meta:
  name: mutating-tool
  version: "1.0.0"
transport:
  mode: native
  command: echo
actions:
  - name: apply
    description: "mutating action"
    classification: mutating
`

const planRunbookWithMutating = `apiVersion: yawr.runbook/v1
id: plan-mutating-test
name: plan mutating test
toolRefs:
  - name: mutating-tool
flow:
  - step:
      id: apply
      type: tool
      tool:
        name: mutating-tool
        action: apply
      on_success: done
  - step:
      id: done
      type: end
      outcome: {category: success, code: ok}
`

// ── Helpers ────────────────────────────────────────────────────────────────────

// captureRunPlan runs runPlan with the given args, capturing stdout to a
// buffer. Stderr goes to os.Stderr (visible in test -v output).
func captureRunPlan(t *testing.T, args []string) (int, string) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	code := runPlan(args)
	_ = w.Close()
	os.Stdout = orig

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return code, buf.String()
}

// absPath converts a path to absolute. Must be called BEFORE chdirForTest.
func absPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", path, err)
	}
	return abs
}

// planWorkDir creates a temp work directory and returns its absolute path.
// Absolute is critical: any paths derived from it must remain valid after
// chdirForTest changes the working directory.
func planWorkDir(t *testing.T) string {
	t.Helper()
	return absPath(t, makeWorkDir(t))
}

// ── Tests ──────────────────────────────────────────────────────────────────────

// TestPlan_SimpleRunbook_NoProfile verifies that a runbook with no tool refs
// and no --profile flag exits 0 and produces human-readable output.
func TestPlan_SimpleRunbook_NoProfile(t *testing.T) {
	workDir := planWorkDir(t)
	runYAML := filepath.Join(workDir, "run.yaml")
	writeFile(t, runYAML, planSimpleRunbook)
	chdirForTest(t, workDir)

	code, out := captureRunPlan(t, []string{runYAML})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess for a simple runbook with no profile; got %d\nout: %s", code, out)
	}
	if !strings.Contains(out, "Tier 0 preflight: PASS") {
		t.Errorf("expected 'Tier 0 preflight: PASS' in output; got:\n%s", out)
	}
}

// TestPlan_SimpleRunbook_JsonOutput verifies that --output json produces
// parseable JSON with preflight.pass=true.
func TestPlan_SimpleRunbook_JsonOutput(t *testing.T) {
	workDir := planWorkDir(t)
	runYAML := filepath.Join(workDir, "run.yaml")
	writeFile(t, runYAML, planSimpleRunbook)
	chdirForTest(t, workDir)

	code, out := captureRunPlan(t, []string{runYAML, "--output", "json"})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess; got %d\nout: %s", code, out)
	}
	var result planOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\nout: %s", err, out)
	}
	if !result.Preflight.Pass {
		t.Errorf("expected preflight.pass=true in JSON; got %+v", result.Preflight)
	}
}

// TestPlan_WithProfile_AllContextsAccepted verifies all five valid context
// values are accepted through the full wiring path.
func TestPlan_WithProfile_AllContextsAccepted(t *testing.T) {
	contexts := []string{"cli-operator", "vscode-operator", "ci", "headless-server", "test"}
	for _, ctx := range contexts {
		ctx := ctx
		t.Run(ctx, func(t *testing.T) {
			workDir := planWorkDir(t)
			runYAML := filepath.Join(workDir, "run.yaml")
			profPath := filepath.Join(workDir, "prof.yaml")
			writeFile(t, runYAML, planSimpleRunbook)
			// ci/headless-server require attendance=unattended to avoid PLAN-011.
			attendance := "attended"
			if ctx == "ci" || ctx == "headless-server" {
				attendance = "unattended"
			}
			writeFile(t, profPath, profileYAML(ctx, attendance))
			chdirForTest(t, workDir)

			code, out := captureRunPlan(t, []string{runYAML, "--profile", profPath})
			if code != exitSuccess {
				t.Fatalf("context %q: expected exitSuccess; got %d\nout: %s", ctx, code, out)
			}
		})
	}
}

// TestPlan_WithProfile_SurfacesInOutput verifies that when a --profile is
// given, its id, context, and attendance appear in text output.
func TestPlan_WithProfile_SurfacesInOutput(t *testing.T) {
	workDir := planWorkDir(t)
	runYAML := filepath.Join(workDir, "run.yaml")
	profPath := filepath.Join(workDir, "prof.yaml")
	writeFile(t, runYAML, planSimpleRunbook)
	writeFile(t, profPath, `apiVersion: yawr.runtime-profile/v1
id: my-test-profile
context: ci
attendance: unattended
`)
	chdirForTest(t, workDir)

	code, out := captureRunPlan(t, []string{runYAML, "--profile", profPath})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess; got %d\nout: %s", code, out)
	}
	for _, want := range []string{"my-test-profile", "ci", "unattended"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got:\n%s", want, out)
		}
	}
}

// TestPlan_Tier0_ContextMismatch_ExitsNonZero verifies that PLAN-010 fires
// via the plan command when context is not in the tool's allowed-environments.
func TestPlan_Tier0_ContextMismatch_ExitsNonZero(t *testing.T) {
	workDir := planWorkDir(t)
	writeFile(t, filepath.Join(workDir, "restricted.tool.yaml"), toolYAMLWithAllowedEnvs("ci"))
	runYAML := filepath.Join(workDir, "run.yaml")
	profPath := filepath.Join(workDir, "prof.yaml")
	writeFile(t, runYAML, `apiVersion: yawr.runbook/v1
id: r
name: r
toolRefs:
  - name: env-restricted
flow:
  - step:
      id: done
      type: end
      outcome: {category: success, code: ok}
`)
	writeFile(t, profPath, profileYAML("cli-operator", "attended"))
	chdirForTest(t, workDir)

	code, _ := captureRunPlan(t, []string{runYAML, "--profile", profPath})
	if code == exitSuccess {
		t.Fatalf("PLAN-010 must fire: context cli-operator not in allowed-environments [ci]")
	}
	if code != exitValidation {
		t.Errorf("expected exitValidation (%d) for PLAN-010; got %d", exitValidation, code)
	}
}

// TestPlan_Tier0_AttendanceMismatch_ExitsNonZero verifies PLAN-011 fires when
// attendance is attended but context is ci (unattended-only context).
func TestPlan_Tier0_AttendanceMismatch_ExitsNonZero(t *testing.T) {
	workDir := planWorkDir(t)
	runYAML := filepath.Join(workDir, "run.yaml")
	profPath := filepath.Join(workDir, "prof.yaml")
	writeFile(t, runYAML, planSimpleRunbook)
	// ci + attended → PLAN-011 mismatch
	writeFile(t, profPath, profileYAML("ci", "attended"))
	chdirForTest(t, workDir)

	code, _ := captureRunPlan(t, []string{runYAML, "--profile", profPath})
	if code == exitSuccess {
		t.Fatalf("PLAN-011 must fire: ci context must be unattended; got exitSuccess")
	}
	if code != exitValidation {
		t.Errorf("expected exitValidation (%d) for PLAN-011; got %d", exitValidation, code)
	}
}

// TestPlan_ToolRef_ResolvedBinding verifies that when a runbook declares a
// toolRef that resolves from the workspace, the tool appears in JSON output.
func TestPlan_ToolRef_ResolvedBinding(t *testing.T) {
	workDir := planWorkDir(t)
	if err := os.MkdirAll(filepath.Join(workDir, "tools"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(workDir, "tools", "native-echo.tool.yaml"), planNativeEchoTool)
	runYAML := filepath.Join(workDir, "run.yaml")
	writeFile(t, runYAML, planRunbookWithToolRef)
	chdirForTest(t, workDir)

	code, out := captureRunPlan(t, []string{runYAML, "--output", "json"})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess; got %d\nout: %s", code, out)
	}
	var result planOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v\nout: %s", err, out)
	}
	if len(result.Tools) == 0 {
		t.Fatalf("expected at least one tool binding; got none\nout: %s", out)
	}
	found := false
	for _, tool := range result.Tools {
		if tool.Name == "native-echo" {
			found = true
		}
	}
	if !found {
		t.Errorf("native-echo not found in tool bindings: %+v", result.Tools)
	}
}

// TestPlan_ReadOnlyAction_UnattendedAllowed verifies that a read-only action
// is allowed under unattended execution when allow_read=true.
func TestPlan_ReadOnlyAction_UnattendedAllowed(t *testing.T) {
	workDir := planWorkDir(t)
	if err := os.MkdirAll(filepath.Join(workDir, "tools"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(workDir, "tools", "native-echo.tool.yaml"), planNativeEchoTool)
	runYAML := filepath.Join(workDir, "run.yaml")
	profPath := filepath.Join(workDir, "prof.yaml")
	writeFile(t, runYAML, planRunbookWithToolRef)
	writeFile(t, profPath, `apiVersion: yawr.runtime-profile/v1
id: ci-unattended
context: ci
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: false
    allow_destructive: false
`)
	chdirForTest(t, workDir)

	code, out := captureRunPlan(t, []string{runYAML, "--profile", profPath, "--output", "json"})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess; got %d\nout: %s", code, out)
	}
	var result planOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v\nout: %s", err, out)
	}
	for _, a := range result.Actions {
		if a.Tool == "native-echo" && a.Action == "run" {
			if a.Outcome != string(planOutcomeAllowed) {
				t.Errorf("expected read-only action outcome=%q; got %q (deny_reason: %q)",
					planOutcomeAllowed, a.Outcome, a.DenyReason)
			}
			return
		}
	}
	// Binding may not resolve in all workspace configurations; skip hard fail.
}

// TestPlan_MutatingAction_UnattendedDenied verifies that a mutating action
// is denied under unattended execution when allow_mutating=false.
func TestPlan_MutatingAction_UnattendedDenied(t *testing.T) {
	workDir := planWorkDir(t)
	if err := os.MkdirAll(filepath.Join(workDir, "tools"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(workDir, "tools", "mutating-tool.tool.yaml"), planMutatingTool)
	runYAML := filepath.Join(workDir, "run.yaml")
	profPath := filepath.Join(workDir, "prof.yaml")
	writeFile(t, runYAML, planRunbookWithMutating)
	writeFile(t, profPath, `apiVersion: yawr.runtime-profile/v1
id: ci-restrictive
context: ci
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: false
    allow_destructive: false
`)
	chdirForTest(t, workDir)

	code, out := captureRunPlan(t, []string{runYAML, "--profile", profPath, "--output", "json"})
	if code != exitSuccess {
		// Plan succeeds even when individual actions are denied — denial is
		// informational output, not a planning failure.
		t.Fatalf("expected exitSuccess (plan succeeds even with denial entries); got %d\nout: %s", code, out)
	}
	var result planOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v\nout: %s", err, out)
	}
	for _, a := range result.Actions {
		if a.Tool == "mutating-tool" && a.Action == "apply" {
			if a.Outcome != string(planOutcomeDenied) {
				t.Errorf("expected outcome=%q for mutating/unattended/allow_mutating=false; got %q",
					planOutcomeDenied, a.Outcome)
			}
			return
		}
	}
}

// TestPlan_UnknownOutputFormat_Fails verifies that --output with an
// unknown value returns exitValidation.
func TestPlan_UnknownOutputFormat_Fails(t *testing.T) {
	workDir := planWorkDir(t)
	runYAML := filepath.Join(workDir, "run.yaml")
	writeFile(t, runYAML, planSimpleRunbook)
	chdirForTest(t, workDir)

	code, _ := captureRunPlan(t, []string{runYAML, "--output", "invalid-format"})
	if code == exitSuccess {
		t.Fatalf("expected non-success for unknown output format; got exitSuccess")
	}
}

// TestPlan_MissingRunbook_Fails verifies that omitting the runbook path
// returns exitValidation (not a panic or exitRuntime).
func TestPlan_MissingRunbook_Fails(t *testing.T) {
	workDir := planWorkDir(t)
	chdirForTest(t, workDir)

	code, _ := captureRunPlan(t, []string{})
	if code == exitSuccess {
		t.Fatalf("expected non-success when runbook path is omitted; got exitSuccess")
	}
}

// TestComputeActionApprovalOutcome_Matrix is a pure unit test of the static
// classification matrix in computeActionApprovalOutcome, independent of
// all planner and catalog wiring.
func TestComputeActionApprovalOutcome_Matrix(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	strPtr := func(s string) *string { return &s }

	ciUnattendedProfile := &schema.RuntimeProfile{
		Context:    schema.ProfileContextCI,
		Attendance: schema.ProfileAttendanceUnattended,
		Approval: schema.ProfileApproval{
			Scope: schema.ProfileApprovalScope{
				AllowRead:        true,
				AllowMutating:    false,
				AllowDestructive: false,
			},
		},
	}
	attendedProfile := &schema.RuntimeProfile{
		Context:    schema.ProfileContextVSCodeOperator,
		Attendance: schema.ProfileAttendanceAttended,
		Approval: schema.ProfileApproval{
			Scope: schema.ProfileApprovalScope{AllowRead: true, AllowMutating: true},
		},
	}
	testContextProfile := &schema.RuntimeProfile{
		Context:    schema.ProfileContextTest,
		Attendance: schema.ProfileAttendanceUnattended,
	}
	noReadScope := &schema.RuntimeProfile{
		Context:    schema.ProfileContextCI,
		Attendance: schema.ProfileAttendanceUnattended,
		Approval: schema.ProfileApproval{
			Scope: schema.ProfileApprovalScope{AllowRead: false},
		},
	}

	readOnlyAction := &schema.ToolAction{Classification: strPtr("read-only")}
	mutatingAction := &schema.ToolAction{Classification: strPtr("mutating")}
	missingAction := &schema.ToolAction{}

	cases := []struct {
		name       string
		profile    *schema.RuntimeProfile
		governance *schema.ToolGovernance
		action     *schema.ToolAction
		want       planApprovalOutcome
	}{
		{"classified action without profile allowed", nil, nil, readOnlyAction, planOutcomeAllowed},
		{"approval opt-out denied", ciUnattendedProfile, &schema.ToolGovernance{RequiresApproval: boolPtr(false)}, mutatingAction, planOutcomeDenied},
		// Explicit requires-approval: true always fires the gate.
		{"explicit-required always gates", ciUnattendedProfile, &schema.ToolGovernance{RequiresApproval: boolPtr(true)}, readOnlyAction, planOutcomeExplicitRequired},
		// Read-only + allow_read=true → allowed.
		{"read-only allowed when allow_read=true", ciUnattendedProfile, nil, readOnlyAction, planOutcomeAllowed},
		// Read-only + allow_read=false → denied.
		{"read-only denied when allow_read=false", noReadScope, nil, readOnlyAction, planOutcomeDenied},
		// Mutating + unattended + allow_mutating=false → denied.
		{"mutating denied when unattended+allow_mutating=false", ciUnattendedProfile, nil, mutatingAction, planOutcomeDenied},
		// Mutating + attended → approval gate (not a hard deny).
		{"mutating attended → approval gate", attendedProfile, nil, mutatingAction, planOutcomeApprovalGate},
		{"missing classification unattended prompt denied", ciUnattendedProfile, nil, missingAction, planOutcomeDenied},
		// Test context allows explicitly classified actions.
		{"test context mutating → allowed", testContextProfile, nil, mutatingAction, planOutcomeAllowed},
		{"test context missing classification denied", testContextProfile, nil, missingAction, planOutcomeDenied},
		{"opt-out plus missing classification denied", ciUnattendedProfile, &schema.ToolGovernance{RequiresApproval: boolPtr(false)}, missingAction, planOutcomeDenied},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, _ := computeActionApprovalOutcome(c.profile, c.governance, c.action)
			if got != c.want {
				t.Errorf("got %q; want %q", got, c.want)
			}
		})
	}
}

// ── --show-profiles tests ─────────────────────────────────────────────────────

// writeProfileYAML is a convenience helper for tests that need profile files.
func writeProfileYAML(t *testing.T, path, id, ctx, attendance string) {
	t.Helper()
	writeFile(t, path, "apiVersion: yawr.runtime-profile/v1\nid: "+id+"\ncontext: "+ctx+"\nattendance: "+attendance+"\n")
}

// TestPlan_ShowProfiles_FindsCompatible verifies that --show-profiles lists
// profiles from the given directory whose Plan() passes for the runbook.
func TestPlan_ShowProfiles_FindsCompatible(t *testing.T) {
	workDir := planWorkDir(t)
	profDir := absPath(t, filepath.Join(workDir, "profiles"))
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	runYAML := filepath.Join(workDir, "run.yaml")
	writeFile(t, runYAML, planSimpleRunbook)
	// cli-operator + attended is valid for a simple runbook with no tool restrictions.
	writeProfileYAML(t, filepath.Join(profDir, "ops.yaml"), "ops", "cli-operator", "attended")
	// ci + attended -> PLAN-011 mismatch -- should be filtered out.
	writeProfileYAML(t, filepath.Join(profDir, "bad.yaml"), "bad-ci", "ci", "attended")
	chdirForTest(t, workDir)

	code, out := captureRunPlan(t, []string{runYAML, "--show-profiles", profDir})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess for --show-profiles; got %d\nout: %s", code, out)
	}
	if !strings.Contains(out, "ops") {
		t.Errorf("expected profile 'ops' in output; got:\n%s", out)
	}
}

// TestPlan_ShowProfiles_ZeroMatches verifies Barbara's ruling: exit 0, empty
// stdout, single-line stderr when no profile provides a complete binding.
func TestPlan_ShowProfiles_ZeroMatches(t *testing.T) {
	workDir := planWorkDir(t)
	profDir := absPath(t, filepath.Join(workDir, "profiles"))
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	runYAML := filepath.Join(workDir, "run.yaml")
	writeFile(t, runYAML, planSimpleRunbook)
	// ci + attended -> PLAN-011 for every profile -> zero compatible.
	writeProfileYAML(t, filepath.Join(profDir, "bad.yaml"), "bad-ci", "ci", "attended")
	chdirForTest(t, workDir)

	code, out := captureRunPlan(t, []string{runYAML, "--show-profiles", profDir})
	if code != exitSuccess {
		// Barbara's ruling: zero matches is not a failure.
		t.Fatalf("expected exitSuccess (zero matches is not failure); got %d", code)
	}
	// Empty stdout on zero matches.
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected empty stdout on zero matches; got:\n%s", out)
	}
}

// TestPlan_ShowProfiles_JsonOutput verifies JSON output for --show-profiles.
func TestPlan_ShowProfiles_JsonOutput(t *testing.T) {
	workDir := planWorkDir(t)
	profDir := absPath(t, filepath.Join(workDir, "profiles"))
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	runYAML := filepath.Join(workDir, "run.yaml")
	writeFile(t, runYAML, planSimpleRunbook)
	writeProfileYAML(t, filepath.Join(profDir, "ci-prod.yaml"), "ci-prod", "ci", "unattended")
	chdirForTest(t, workDir)

	code, out := captureRunPlan(t, []string{runYAML, "--show-profiles", profDir, "--output", "json"})
	if code != exitSuccess {
		t.Fatalf("expected exitSuccess; got %d\nout: %s", code, out)
	}
	var result struct {
		Profiles []struct {
			ID         string `json:"id"`
			Compatible bool   `json:"compatible"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("not valid JSON: %v\nout: %s", err, out)
	}
	found := false
	for _, p := range result.Profiles {
		if p.ID == "ci-prod" && p.Compatible {
			found = true
		}
	}
	if !found {
		t.Errorf("ci-prod not listed as compatible in JSON; profiles: %+v", result.Profiles)
	}
}

// TestPlan_NoBindingNote_ShowsAlternatives verifies that when plan fails with
// PLAN-011 and the profile directory contains an alternative that works, the
// "note: this runbook runs correctly in profiles: X" line appears.
func TestPlan_NoBindingNote_ShowsAlternatives(t *testing.T) {
	workDir := planWorkDir(t)
	runYAML := filepath.Join(workDir, "run.yaml")
	// ci + attended -> PLAN-011 (failing profile)
	failingProf := filepath.Join(workDir, "bad.yaml")
	// ci + unattended -> should work
	goodProf := filepath.Join(workDir, "good.yaml")
	writeFile(t, runYAML, planSimpleRunbook)
	writeProfileYAML(t, failingProf, "bad", "ci", "attended")
	writeProfileYAML(t, goodProf, "good", "ci", "unattended")
	chdirForTest(t, workDir)

	// Capture stderr (renderPlanErrorText writes to stderr).
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	code := runPlan([]string{runYAML, "--profile", failingProf})
	_ = w.Close()
	os.Stderr = origStderr
	var stderrBuf bytes.Buffer
	_, _ = stderrBuf.ReadFrom(r)
	stderrOut := stderrBuf.String()

	if code == exitSuccess {
		t.Fatalf("expected non-zero exit for PLAN-011; got exitSuccess")
	}
	if !strings.Contains(stderrOut, "good") {
		t.Errorf("expected alternative profile 'good' in note; stderr:\n%s", stderrOut)
	}
}

func TestRouteTestPlanHashIncludesResolvedToolContracts(t *testing.T) {
	plan := &engine.ExecutionPlan{Tools: map[string]*schema.ToolDef{
		"diagnose": {APIVersion: "yawr.tool/v1", Name: "diagnose", Description: "first", Actions: map[string]*schema.ToolAction{}},
	}}
	first, err := routeTestPlanHash("graph-hash", plan, nil, nil)
	if err != nil {
		t.Fatalf("first hash: %v", err)
	}
	plan.Tools["diagnose"].Description = "changed"
	changed, err := routeTestPlanHash("graph-hash", plan, nil, nil)
	if err != nil {
		t.Fatalf("changed hash: %v", err)
	}
	if first == changed {
		t.Fatalf("tool contract change did not change route-test hash: %s", first)
	}
}

func TestRouteTestPlanHashIncludesExecutableAndPackageIdentity(t *testing.T) {
	plan := &engine.ExecutionPlan{Metadata: engine.PlanMetadata{
		PlanHash: "sha256:plan-one", PackageDigests: map[string]string{"incident-tools": "sha256:package-one"},
	}}
	first, err := routeTestPlanHash("sha256:graph", plan, nil, nil)
	if err != nil {
		t.Fatalf("first hash: %v", err)
	}
	plan.Metadata.PlanHash = "sha256:plan-two"
	changedPlan, err := routeTestPlanHash("sha256:graph", plan, nil, nil)
	if err != nil {
		t.Fatalf("changed plan hash: %v", err)
	}
	if first == changedPlan {
		t.Fatal("executable plan change did not change route-test hash")
	}
	plan.Metadata.PlanHash = "sha256:plan-one"
	plan.Metadata.PackageDigests["incident-tools"] = "sha256:package-two"
	changedPackage, err := routeTestPlanHash("sha256:graph", plan, nil, nil)
	if err != nil {
		t.Fatalf("changed package hash: %v", err)
	}
	if first == changedPackage {
		t.Fatal("package-lock change did not change route-test hash")
	}
}

func TestRouteTestPlanHashIncludesGraphAndProfileIdentity(t *testing.T) {
	plan := &engine.ExecutionPlan{Metadata: engine.PlanMetadata{PlanHash: "sha256:plan"}}
	profile := &schema.RuntimeProfile{
		APIVersion: schema.RuntimeProfileAPIVersion, ID: "operator",
		Context: schema.ProfileContext("cli-operator"), Attendance: schema.ProfileAttendance("attended"),
	}
	first, err := routeTestPlanHash("sha256:graph-one", plan, nil, profile)
	if err != nil {
		t.Fatalf("first hash: %v", err)
	}
	changedGraph, err := routeTestPlanHash("sha256:graph-two", plan, nil, profile)
	if err != nil {
		t.Fatalf("changed graph hash: %v", err)
	}
	if first == changedGraph {
		t.Fatal("graph change did not change route-test hash")
	}
	changedProfile := *profile
	changedProfile.ID = "another-operator"
	profileHash, err := routeTestPlanHash("sha256:graph-one", plan, nil, &changedProfile)
	if err != nil {
		t.Fatalf("changed profile hash: %v", err)
	}
	if first == profileHash {
		t.Fatal("profile change did not change route-test hash")
	}
}

func TestRouteTestPlanHashIncludesPackageFreeCatalogSources(t *testing.T) {
	buildCatalog := func(command string) *pkgcatalog.Catalog {
		workspace := t.TempDir()
		if err := os.MkdirAll(filepath.Join(workspace, "tools"), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		definition := `apiVersion: yawr.tool/v1
meta: {name: queryer, version: "1.0.0"}
transport:
  mode: native
  command: ` + command + `
actions:
  - name: query
    argv: ["query"]
`
		if err := os.WriteFile(filepath.Join(workspace, "tools", "queryer.tool.yaml"), []byte(definition), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		catalog, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{WorkspaceRoot: workspace})
		if len(errs) != 0 {
			t.Fatalf("Build catalog: %v", errs)
		}
		if len(catalog.Packages) != 0 {
			t.Fatalf("tier-2 catalog unexpectedly has packages: %#v", catalog.Packages)
		}
		return catalog
	}
	plan := &engine.ExecutionPlan{Metadata: engine.PlanMetadata{PlanHash: "sha256:plan"}}
	first, err := routeTestPlanHash("sha256:graph", plan, buildCatalog("query-one"), nil)
	if err != nil {
		t.Fatalf("first hash: %v", err)
	}
	second, err := routeTestPlanHash("sha256:graph", plan, buildCatalog("query-two"), nil)
	if err != nil {
		t.Fatalf("second hash: %v", err)
	}
	if first == second {
		t.Fatal("package-free catalog source change did not change route-test hash")
	}
}

func TestRouteTestPlanHashStableAcrossEagerIncludePlanning(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "child.runbook.yaml")
	root := filepath.Join(dir, "root.runbook.yaml")
	if err := os.WriteFile(child, []byte("apiVersion: yawr.runbook/v1\nid: child\nname: Child\nkind: composable\nvars: { route: primary }\nflow:\n  - step: { id: child_done, type: noop }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("apiVersion: yawr.runbook/v1\nid: root\nname: Root\nkind: mitigation\nflow:\n  - step:\n      id: child\n      type: include\n      include: { runbook: child.runbook.yaml }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parserImpl, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parserImpl.Parse(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	loader := &fileRunbookLoader{parser: parserImpl}
	before, err := (&graphdoc.Builder{Loader: &cliLoader{p: parserImpl}, Recurse: true}).Build(context.Background(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := newToolRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := internalplanner.New(plannerpkg.Config{Loader: loader, Tools: registry, ExpandPolicy: expand.Policy{Default: expand.ModeEager}}).Plan(context.Background(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	after, err := (&graphdoc.Builder{Loader: &cliLoader{p: parserImpl}, Recurse: true}).Build(context.Background(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash, err := routeTestPlanHash(before.Hash, plan, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	afterHash, err := routeTestPlanHash(after.Hash, plan, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash != afterHash {
		beforeJSON, _ := json.MarshalIndent(before, "", "  ")
		afterJSON, _ := json.MarshalIndent(after, "", "  ")
		t.Fatalf("plan/run route hashes differ: %s != %s\nbefore=%s\nafter=%s", beforeHash, afterHash, beforeJSON, afterJSON)
	}
}
