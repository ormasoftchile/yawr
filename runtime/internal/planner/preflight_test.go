package planner_test

// Preflight acceptance tests for Slice 5: Tier 0 static preflight.
//
// These tests validate the three profile-aware checks introduced in
// internal/planner/preflight.go, called from Plan() and resolveTool():
//
//  1. AllowedEnvironments enforcement (PLAN-010 / ErrContextMismatch)
//  2. Attendance mismatch (PLAN-011 / ErrAttendanceMismatch)
//  3. Test-context transport rules (PLAN-012 / ErrTestContextBinding)
//
// Regression guard: AllowedModes and AllowedEnvironments must not
// interfere with each other. This is the "round-2 near-miss" locked down
// per task spec.

import (
	"context"
	"errors"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerPkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ─── helpers ────────────────────────────────────────────────────────────────

// testProfile builds a minimal valid RuntimeProfile for the given context and
// attendance. It satisfies the schema validator (apiVersion, id required).
func testProfile(id string, ctx schema.ProfileContext, att schema.ProfileAttendance) *schema.RuntimeProfile {
	return &schema.RuntimeProfile{
		APIVersion: schema.RuntimeProfileAPIVersion,
		ID:         id,
		Context:    ctx,
		Attendance: att,
		Approval:   schema.ProfileApproval{},
	}
}

// toolRunbook returns a one-step runbook that calls toolName.actionName.
func toolRunbook(toolName, actionName string) *parser.ParsedRunbook {
	return &parser.ParsedRunbook{
		Source: "/test/runbook.yaml",
		Runbook: &schema.Runbook{
			ID: "test-runbook",
			Flow: []schema.FlowNode{
				{Step: &schema.Step{
					ID:   "step-1",
					Type: schema.StepTypeTool,
					ToolCall: &schema.ToolCallSpec{
						Tool: schema.ToolInvocation{
							Name:   toolName,
							Action: actionName,
						},
					},
				}},
			},
		},
	}
}

// makePlanner constructs a planner with the given profile and a single tool
// registered under "<toolName>/<actionName>".
func makePlanner(profile *schema.RuntimeProfile, toolName, actionName string, def *schema.ToolDef) plannerPkg.Planner {
	return planner.New(plannerPkg.Config{
		Loader:       &fakeLoader{runbooks: make(map[string]*parser.ParsedRunbook)},
		Tools:        &fakeRegistry{tools: map[string]*schema.ToolDef{toolName + "/" + actionName: def}},
		ExpandPolicy: expand.Policy{Default: expand.ModeEager},
		Profile:      profile,
	})
}

// toolDef builds a minimal ToolDef with the given transport mode and optional
// governance.
func toolDefWithTransport(name, mode string) *schema.ToolDef {
	return &schema.ToolDef{
		Name:      name,
		Transport: schema.TransportConfig{Type: schema.Transport(mode)},
		Actions: map[string]*schema.ToolAction{
			"run": {},
		},
	}
}

func toolDefWithGovernance(name, mode string, allowedEnvs []string, allowedModes []string) *schema.ToolDef {
	return &schema.ToolDef{
		Name:      name,
		Transport: schema.TransportConfig{Type: schema.Transport(mode)},
		Governance: &schema.ToolGovernance{
			AllowedEnvironments: allowedEnvs,
			AllowedModes:        allowedModes,
		},
		Actions: map[string]*schema.ToolAction{
			"run": {},
		},
	}
}

// ─── AllowedEnvironments (PLAN-010) ─────────────────────────────────────────

// TestPreflight_AllowedEnvironments_Mismatch verifies that PLAN-010 fires and
// wraps ErrContextMismatch when the profile's context is not in the tool's
// allowed-environments list.
func TestPreflight_AllowedEnvironments_Mismatch(t *testing.T) {
	ctx := context.Background()
	// Tool is allowed only in cli-operator; we run in headless-server.
	def := toolDefWithGovernance("icm", "mcp-http",
		[]string{"cli-operator"}, // AllowedEnvironments
		nil,                      // AllowedModes — orthogonal, must not matter
	)
	profile := testProfile("headless-server", schema.ProfileContextHeadlessServer, schema.ProfileAttendanceUnattended)
	p := makePlanner(profile, "icm", "run", def)

	_, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err == nil {
		t.Fatal("Plan: want error (context mismatch), got nil")
	}
	if !errors.Is(err, plannerPkg.ErrContextMismatch) {
		t.Errorf("errors.Is(err, ErrContextMismatch) = false; err = %v", err)
	}
	// PLAN-010 code must be present in the error chain.
	if !errors.Is(err, plannerPkg.ErrContextMismatch) {
		t.Errorf("expected ErrContextMismatch in chain")
	}
	t.Logf("error message (inspect): %v", err)
}

// TestPreflight_AllowedEnvironments_Match verifies that planning succeeds when
// the profile's context IS in the tool's allowed-environments list.
func TestPreflight_AllowedEnvironments_Match(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithGovernance("icm", "mcp-http",
		[]string{"cli-operator", "headless-server"},
		nil,
	)
	profile := testProfile("headless-server", schema.ProfileContextHeadlessServer, schema.ProfileAttendanceUnattended)
	p := makePlanner(profile, "icm", "run", def)

	plan, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("Plan: unexpected error: %v", err)
	}
	if _, ok := plan.Tools["icm"]; !ok {
		t.Error("plan.Tools missing 'icm'")
	}
}

// TestPreflight_AllowedEnvironments_NilProfile verifies that nil profile
// disables ALL profile checks and existing behavior is preserved.
func TestPreflight_AllowedEnvironments_NilProfile(t *testing.T) {
	ctx := context.Background()
	// Tool restricts to cli-operator only, but no profile is active.
	def := toolDefWithGovernance("icm", "mcp-http",
		[]string{"cli-operator"},
		nil,
	)
	p := makePlanner(nil, "icm", "run", def) // nil profile

	plan, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("Plan with nil profile: unexpected error: %v", err)
	}
	if _, ok := plan.Tools["icm"]; !ok {
		t.Error("plan.Tools missing 'icm' with nil profile")
	}
}

// TestPreflight_AllowedEnvironments_EmptyList verifies that an empty
// allowed-environments list imposes no restriction (tool is universally allowed).
func TestPreflight_AllowedEnvironments_EmptyList(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithGovernance("icm", "mcp-http",
		nil, // no AllowedEnvironments → no restriction
		nil,
	)
	profile := testProfile("ci", schema.ProfileContextCI, schema.ProfileAttendanceUnattended)
	p := makePlanner(profile, "icm", "run", def)

	if _, err := p.Plan(ctx, toolRunbook("icm", "run")); err != nil {
		t.Fatalf("Plan with empty AllowedEnvironments: unexpected error: %v", err)
	}
}

// ─── AllowedModes / AllowedEnvironments non-interference (regression) ────────

// TestPreflight_AllowedModes_DoesNotInterfereWithAllowedEnvironments is the
// round-2 near-miss regression lock. It verifies that AllowedModes and
// AllowedEnvironments are completely orthogonal:
//   - A tool with AllowedModes: ["real"] but no AllowedEnvironments passes
//     AllowedEnvironments check for ANY profile context.
//   - A tool with AllowedEnvironments: ["cli-operator"] but no AllowedModes
//     fails ONLY when context ≠ cli-operator, regardless of mode.
//   - A tool with BOTH set: environment check uses AllowedEnvironments,
//     AllowedModes is ignored by the preflight check (it is enforced elsewhere).
func TestPreflight_AllowedModes_DoesNotInterfereWithAllowedEnvironments(t *testing.T) {
	ctx := context.Background()

	t.Run("allowed_modes_only_no_env_restriction", func(t *testing.T) {
		// Tool has AllowedModes: ["real"] but no AllowedEnvironments.
		// Should plan successfully in any context.
		def := toolDefWithGovernance("kubectl", "mcp",
			nil,              // no AllowedEnvironments
			[]string{"real"}, // AllowedModes: ["real"]
		)
		for _, ctxVal := range []schema.ProfileContext{
			schema.ProfileContextCLIOperator,
			schema.ProfileContextCI,
			schema.ProfileContextHeadlessServer,
		} {
			profile := testProfile(string(ctxVal), ctxVal, schema.ProfileAttendanceUnattended)
			p := makePlanner(profile, "kubectl", "run", def)
			if _, err := p.Plan(ctx, toolRunbook("kubectl", "run")); err != nil {
				t.Errorf("context %q with AllowedModes:[\"real\"] only: unexpected error: %v", ctxVal, err)
			}
		}
	})

	t.Run("allowed_envs_only_no_modes", func(t *testing.T) {
		// Tool has AllowedEnvironments: ["cli-operator"] but no AllowedModes.
		// Should fail for ci, succeed for cli-operator.
		def := toolDefWithGovernance("kubectl", "mcp",
			[]string{"cli-operator"},
			nil, // no AllowedModes
		)
		profileCI := testProfile("ci", schema.ProfileContextCI, schema.ProfileAttendanceUnattended)
		p := makePlanner(profileCI, "kubectl", "run", def)
		if _, err := p.Plan(ctx, toolRunbook("kubectl", "run")); err == nil {
			t.Error("expected ErrContextMismatch for ci context; got nil")
		} else if !errors.Is(err, plannerPkg.ErrContextMismatch) {
			t.Errorf("want ErrContextMismatch; got %v", err)
		}

		profileCLI := testProfile("cli", schema.ProfileContextCLIOperator, schema.ProfileAttendanceAttended)
		pCLI := makePlanner(profileCLI, "kubectl", "run", def)
		if _, err := pCLI.Plan(ctx, toolRunbook("kubectl", "run")); err != nil {
			t.Errorf("cli-operator context: unexpected error: %v", err)
		}
	})

	t.Run("both_set_only_envs_checked_by_preflight", func(t *testing.T) {
		// Tool has both AllowedEnvironments: ["cli-operator"] and
		// AllowedModes: ["real"]. The preflight check must consult
		// AllowedEnvironments only. AllowedModes is orthogonal.
		def := toolDefWithGovernance("kubectl", "mcp",
			[]string{"cli-operator"},
			[]string{"real"},
		)
		profileCI := testProfile("ci", schema.ProfileContextCI, schema.ProfileAttendanceUnattended)
		p := makePlanner(profileCI, "kubectl", "run", def)
		if _, err := p.Plan(ctx, toolRunbook("kubectl", "run")); err == nil {
			t.Error("expected ErrContextMismatch for ci context with AllowedEnvs:[cli-operator]; got nil")
		} else if !errors.Is(err, plannerPkg.ErrContextMismatch) {
			t.Errorf("want ErrContextMismatch; got %v", err)
		}
	})
}

// ─── Test-context transport rules (PLAN-012) ─────────────────────────────────

// TestPreflight_TestContext_MCPHTTP_NeverAllowed verifies that mcp-http is
// unconditionally blocked in test context. allow_subprocess_in_test has no
// effect on mcp-http (it is only the mcp subprocess opt-in).
func TestPreflight_TestContext_MCPHTTP_NeverAllowed(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp-http")

	// Blocked: test profile without opt-in (default).
	profile := testProfile("test", schema.ProfileContextTest, schema.ProfileAttendanceUnattended)
	p := makePlanner(profile, "icm", "run", def)
	_, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err == nil {
		t.Fatal("expected ErrTestContextBinding for mcp-http in test context; got nil")
	}
	if !errors.Is(err, plannerPkg.ErrTestContextBinding) {
		t.Errorf("want ErrTestContextBinding; got %v", err)
	}

	// Still blocked even with allow_subprocess_in_test: true — that flag
	// only applies to mcp subprocess, not mcp-http.
	profileWithOpt := &schema.RuntimeProfile{
		APIVersion: schema.RuntimeProfileAPIVersion,
		ID:         "test",
		Context:    schema.ProfileContextTest,
		Attendance: schema.ProfileAttendanceUnattended,
		Transport:  schema.ProfileTransport{AllowSubprocessInTest: true},
	}
	pOpt := makePlanner(profileWithOpt, "icm", "run", def)
	_, err = pOpt.Plan(ctx, toolRunbook("icm", "run"))
	if err == nil {
		t.Fatal("mcp-http must NOT be allowed even with allow_subprocess_in_test: true")
	}
	if !errors.Is(err, plannerPkg.ErrTestContextBinding) {
		t.Errorf("want ErrTestContextBinding; got %v", err)
	}
}

// TestPreflight_TestContext_MCPSubprocess_BlockedByDefault verifies that mcp
// subprocess transport is blocked in test context unless
// transport.allow_subprocess_in_test is explicitly set.
func TestPreflight_TestContext_MCPSubprocess_BlockedByDefault(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp")

	profile := testProfile("test", schema.ProfileContextTest, schema.ProfileAttendanceUnattended)
	p := makePlanner(profile, "icm", "run", def)
	_, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err == nil {
		t.Fatal("expected ErrTestContextBinding for mcp subprocess in test context; got nil")
	}
	if !errors.Is(err, plannerPkg.ErrTestContextBinding) {
		t.Errorf("want ErrTestContextBinding; got %v", err)
	}
}

// TestPreflight_TestContext_MCPSubprocess_AllowedWithOptIn verifies that mcp
// subprocess transport is allowed in test context when
// transport.allow_subprocess_in_test is explicitly set to true.
func TestPreflight_TestContext_MCPSubprocess_AllowedWithOptIn(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp")

	profile := &schema.RuntimeProfile{
		APIVersion: schema.RuntimeProfileAPIVersion,
		ID:         "test",
		Context:    schema.ProfileContextTest,
		Attendance: schema.ProfileAttendanceUnattended,
		Transport:  schema.ProfileTransport{AllowSubprocessInTest: true},
	}
	p := makePlanner(profile, "icm", "run", def)
	plan, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("expected success with allow_subprocess_in_test: true; got: %v", err)
	}
	if _, ok := plan.Tools["icm"]; !ok {
		t.Error("plan.Tools missing 'icm'")
	}
}

// TestPreflight_TestContext_NativeAlwaysAllowed verifies that native transport
// is always allowed in test context regardless of other settings.
func TestPreflight_TestContext_NativeAlwaysAllowed(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "native")

	// Default test profile — no opt-ins.
	profile := testProfile("test", schema.ProfileContextTest, schema.ProfileAttendanceUnattended)
	p := makePlanner(profile, "icm", "run", def)
	plan, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("native transport in test context: unexpected error: %v", err)
	}
	if _, ok := plan.Tools["icm"]; !ok {
		t.Error("plan.Tools missing 'icm'")
	}
}

// ─── Attendance mismatch (PLAN-011) ──────────────────────────────────────────

// TestPreflight_Attendance_Mismatch_CI verifies that a profile declaring
// attendance: attended with context: ci is rejected (PLAN-011).
func TestPreflight_Attendance_Mismatch_CI(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp-http")

	profile := testProfile("ci", schema.ProfileContextCI, schema.ProfileAttendanceAttended)
	p := makePlanner(profile, "icm", "run", def)
	_, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err == nil {
		t.Fatal("expected ErrAttendanceMismatch for attended + ci; got nil")
	}
	if !errors.Is(err, plannerPkg.ErrAttendanceMismatch) {
		t.Errorf("want ErrAttendanceMismatch; got %v", err)
	}
	t.Logf("error message (inspect): %v", err)
}

// TestPreflight_Attendance_Mismatch_HeadlessServer verifies that attended +
// headless-server is also rejected.
func TestPreflight_Attendance_Mismatch_HeadlessServer(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp-http")

	profile := testProfile("hs", schema.ProfileContextHeadlessServer, schema.ProfileAttendanceAttended)
	p := makePlanner(profile, "icm", "run", def)
	_, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err == nil {
		t.Fatal("expected ErrAttendanceMismatch for attended + headless-server; got nil")
	}
	if !errors.Is(err, plannerPkg.ErrAttendanceMismatch) {
		t.Errorf("want ErrAttendanceMismatch; got %v", err)
	}
}

// TestPreflight_Attendance_OK_CLIOperator verifies that attended + cli-operator
// is accepted (the attended interactive case).
func TestPreflight_Attendance_OK_CLIOperator(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp")

	profile := testProfile("cli", schema.ProfileContextCLIOperator, schema.ProfileAttendanceAttended)
	p := makePlanner(profile, "icm", "run", def)
	_, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("attended + cli-operator: unexpected error: %v", err)
	}
}

// TestPreflight_Attendance_Unattended_CI verifies that unattended + ci is
// accepted (the canonical unattended pipeline case).
func TestPreflight_Attendance_Unattended_CI(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp-http")
	// Give it AllowedEnvironments: ["ci"] so the environment check passes too.
	def.Governance = &schema.ToolGovernance{AllowedEnvironments: []string{"ci"}}

	profile := testProfile("ci", schema.ProfileContextCI, schema.ProfileAttendanceUnattended)
	p := makePlanner(profile, "icm", "run", def)
	_, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("unattended + ci: unexpected error: %v", err)
	}
}

// TestPreflight_Attendance_NilProfile verifies that nil profile disables the
// attendance check too (existing behavior preserved).
func TestPreflight_Attendance_NilProfile(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp")
	p := makePlanner(nil, "icm", "run", def)
	if _, err := p.Plan(ctx, toolRunbook("icm", "run")); err != nil {
		t.Fatalf("nil profile: unexpected error: %v", err)
	}
}

// ─── Profile carried on plan metadata ────────────────────────────────────────

// TestPreflight_PlanMetadata_ProfileCarried verifies that the profile is set on
// plan.Metadata.Profile when a profile is supplied.
func TestPreflight_PlanMetadata_ProfileCarried(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp")

	profile := testProfile("cli", schema.ProfileContextCLIOperator, schema.ProfileAttendanceAttended)
	p := makePlanner(profile, "icm", "run", def)
	plan, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Metadata.Profile == nil {
		t.Fatal("plan.Metadata.Profile is nil; expected profile to be carried")
	}
	if plan.Metadata.Profile.ID != "cli" {
		t.Errorf("plan.Metadata.Profile.ID = %q; want %q", plan.Metadata.Profile.ID, "cli")
	}
}

// TestPreflight_PlanMetadata_NilProfileNotCarried verifies that nil profile
// is not set on plan.Metadata.Profile (remains nil as before).
func TestPreflight_PlanMetadata_NilProfileNotCarried(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("icm", "mcp")
	p := makePlanner(nil, "icm", "run", def)
	plan, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Metadata.Profile != nil {
		t.Errorf("plan.Metadata.Profile = %+v; want nil", plan.Metadata.Profile)
	}
}

// ─── Profile endpoint override host check (PLAN-013) ─────────────────────────

// toolDefMCPHTTPWithAuth builds a minimal mcp-http ToolDef with auth and
// allowed_hosts configured. Used for PLAN-013 tests.
func toolDefMCPHTTPWithAuth(name string, allowedHosts []string) *schema.ToolDef {
	return &schema.ToolDef{
		Name: name,
		Transport: schema.TransportConfig{
			Type: schema.TransportMCPHTTP,
			URL:  "https://" + allowedHosts[0] + "/v1/",
			Auth: &schema.AuthConfig{
				Provider:     "azure-cli",
				Scope:        "api://test/scope",
				AllowedHosts: allowedHosts,
			},
		},
		Actions: map[string]*schema.ToolAction{"run": {}},
	}
}

// profileWithEndpointOverride builds a CLI-operator profile with a per-tool
// endpoint override. attendance: unattended to avoid PLAN-011.
func profileWithEndpointOverride(toolName, endpoint string) *schema.RuntimeProfile {
	return &schema.RuntimeProfile{
		APIVersion: schema.RuntimeProfileAPIVersion,
		ID:         "override-profile",
		Context:    schema.ProfileContextCLIOperator,
		Attendance: schema.ProfileAttendanceUnattended,
		Tools: map[string]*schema.ProfileToolOverride{
			toolName: {Endpoint: endpoint},
		},
	}
}

// TestPreflight_PLAN013_EndpointOverrideHostNotInAllowedHosts verifies that
// planning fails with PLAN-013 / ErrEndpointHostNotAllowed when the profile's
// per-tool endpoint override routes to a host not declared in allowed_hosts.
func TestPreflight_PLAN013_EndpointOverrideHostNotInAllowedHosts(t *testing.T) {
	ctx := context.Background()
	def := toolDefMCPHTTPWithAuth("icm", []string{"icm-prod.azure-api.net"})
	profile := profileWithEndpointOverride("icm", "https://icm-staging.attacker.net/v1/")
	p := makePlanner(profile, "icm", "run", def)

	_, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err == nil {
		t.Fatal("Plan: want PLAN-013 error (override host not in allowed_hosts), got nil")
	}
	if !errors.Is(err, plannerPkg.ErrEndpointHostNotAllowed) {
		t.Errorf("errors.Is(err, ErrEndpointHostNotAllowed) = false; err = %v", err)
	}
	t.Logf("PLAN-013 message (inspect): %v", err)
}

// TestPreflight_PLAN013_EndpointOverrideHostInAllowedHosts verifies that
// planning succeeds when the profile's endpoint override host IS in allowed_hosts.
func TestPreflight_PLAN013_EndpointOverrideHostInAllowedHosts(t *testing.T) {
	ctx := context.Background()
	// allowed_hosts contains both prod and staging.
	def := toolDefMCPHTTPWithAuth("icm", []string{"icm-prod.azure-api.net", "icm-staging.azure-api.net"})
	// Override to staging — host IS in allowed_hosts.
	profile := profileWithEndpointOverride("icm", "https://icm-staging.azure-api.net/v1/")
	p := makePlanner(profile, "icm", "run", def)

	plan, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("Plan: unexpected error (override host is in allowed_hosts): %v", err)
	}
	if _, ok := plan.Tools["icm"]; !ok {
		t.Error("plan.Tools missing 'icm'")
	}
}

// TestPreflight_PLAN013_EndpointOverrideRequiresAuth verifies that an endpoint
// override on an mcp-http tool WITHOUT auth is rejected (no allowed_hosts to
// check against).
func TestPreflight_PLAN013_EndpointOverrideRequiresAuth(t *testing.T) {
	ctx := context.Background()
	// mcp-http tool with NO auth block.
	def := &schema.ToolDef{
		Name: "unauth-tool",
		Transport: schema.TransportConfig{
			Type: schema.TransportMCPHTTP,
			URL:  "https://unauth.example.com/v1/",
			// No Auth field → no AllowedHosts.
		},
		Actions: map[string]*schema.ToolAction{"run": {}},
	}
	profile := profileWithEndpointOverride("unauth-tool", "https://other.example.com/v1/")
	p := makePlanner(profile, "unauth-tool", "run", def)

	_, err := p.Plan(ctx, toolRunbook("unauth-tool", "run"))
	if err == nil {
		t.Fatal("Plan: want PLAN-013 error (no allowed_hosts on tool), got nil")
	}
	if !errors.Is(err, plannerPkg.ErrEndpointHostNotAllowed) {
		t.Errorf("want ErrEndpointHostNotAllowed; got %v", err)
	}
}

// TestPreflight_PLAN013_NoOverride_PassesThrough verifies that planning
// succeeds when the profile has NO endpoint override, even for an mcp-http
// tool with auth. PLAN-013 must not fire when there is no override.
func TestPreflight_PLAN013_NoOverride_PassesThrough(t *testing.T) {
	ctx := context.Background()
	def := toolDefMCPHTTPWithAuth("icm", []string{"icm-prod.azure-api.net"})
	// Profile with no per-tool overrides.
	profile := testProfile("ci", schema.ProfileContextCI, schema.ProfileAttendanceUnattended)
	p := makePlanner(profile, "icm", "run", def)

	plan, err := p.Plan(ctx, toolRunbook("icm", "run"))
	if err != nil {
		t.Fatalf("Plan with no endpoint override: unexpected error: %v", err)
	}
	if _, ok := plan.Tools["icm"]; !ok {
		t.Error("plan.Tools missing 'icm'")
	}
}

// TestPreflight_PLAN013_NilProfile_NotChecked verifies that nil profile
// bypasses PLAN-013 entirely (no override, no check).
func TestPreflight_PLAN013_NilProfile_NotChecked(t *testing.T) {
	ctx := context.Background()
	def := toolDefMCPHTTPWithAuth("icm", []string{"icm-prod.azure-api.net"})
	p := makePlanner(nil, "icm", "run", def)

	if _, err := p.Plan(ctx, toolRunbook("icm", "run")); err != nil {
		t.Fatalf("nil profile: unexpected error: %v", err)
	}
}

// TestPreflight_PLAN013_NonMCPHTTP_EndpointOverride_Ignored verifies that an
// endpoint override on a non-mcp-http tool (e.g. native) is silently ignored:
// it has no effect on a tool that doesn't use an HTTP URL.
func TestPreflight_PLAN013_NonMCPHTTP_EndpointOverride_Ignored(t *testing.T) {
	ctx := context.Background()
	def := toolDefWithTransport("kubectl", "native")
	// Profile with an endpoint override on a native tool — should be a no-op.
	profile := profileWithEndpointOverride("kubectl", "https://some-other-host.example.com/v1/")
	p := makePlanner(profile, "kubectl", "run", def)

	plan, err := p.Plan(ctx, toolRunbook("kubectl", "run"))
	if err != nil {
		t.Fatalf("endpoint override on native tool: unexpected error: %v", err)
	}
	if _, ok := plan.Tools["kubectl"]; !ok {
		t.Error("plan.Tools missing 'kubectl'")
	}
}
