package planner

// Tier 0 static preflight checks for runtime profiles.
//
// These checks are purely static — no network, no auth, no subprocess.
// They answer: "is this runbook configured to run in this context?"
//
// Three checks are implemented here:
//
//  1. AllowedEnvironments (PLAN-010 / ErrContextMismatch)
//     A tool's governance.allowed-environments list restricts which
//     ProfileContext values may invoke the tool. If the active profile's
//     context is not in the list, planning fails before step 1 executes.
//  2. Attendance mismatch (PLAN-011 / ErrAttendanceMismatch)
//     A profile that declares attendance: attended but names a context
//     that is structurally unattended (ci, headless-server) is rejected.
//     LIMITATION: the check is against declared state only. The repo
//     has no isatty() seam; TTYOutput is hardcoded true in run.go.
//     Real terminal detection is a Tier 1 concern deferred to a later
//     slice. This fact is documented in the error message.
//
//  3. Test-context transport rules (PLAN-012 / ErrTestContextBinding)
//     In context: test, transport rules are:
//       native   → always allowed (deterministic mocks)
//       mcp      → blocked by default; allowed with explicit
//                  transport.allow_subprocess_in_test: true
//       mcp-http → NEVER allowed. No override exists.
//     The opt-in for mcp subprocess is an AUDITABLE AUTHOR ASSERTION —
//     it does NOT sandbox the process. The subprocess inherits the full
//     parent environment (exec.Command + mergeEnv uses os.Environ()).
//     Do not describe it as hermetic or sandboxed in any message or doc.

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	plannerPkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// checkAttendancePreflight returns PLAN-011 if the profile declares
// attendance: attended but the profile's context is structurally unattended
// (ci, headless-server). Returns nil when the profile is nil, attendance
// is unattended, or the context supports interactive operation.
//
// This check is against declared state only. There is no isatty() in this
// codebase; TTYOutput is hardcoded to true in cmd/yawr/run.go. Real TTY
// detection is a Tier 1 concern.
func checkAttendancePreflight(profile *schema.RuntimeProfile) error {
	if profile == nil {
		return nil
	}
	if profile.Attendance != schema.ProfileAttendanceAttended {
		return nil
	}
	// CI and headless-server are structurally unattended contexts — there is
	// no interactive channel and no operator at the terminal.
	switch profile.Context {
	case schema.ProfileContextCI, schema.ProfileContextHeadlessServer:
		msg := fmt.Sprintf(
			"profile %q declares attendance: attended but context %q cannot provide an interactive channel\n"+
				"  use attendance: unattended for ci and headless-server profiles\n"+
				"  note: this check is against declared state only; TTY detection is not yet wired (Tier 1)",
			profile.ID, profile.Context,
		)
		return &plannerPkg.PlanError{
			Code:   errkit.Wrap("PLAN-011", msg, plannerPkg.ErrAttendanceMismatch),
			Detail: msg,
		}
	}
	return nil
}

// checkToolEnvironmentPreflight enforces per-tool Tier 0 preflight rules for
// the resolved tool definition. Called from resolveTool() after a successful
// registry lookup, before the definition is added to the plan's tools map.
// Returns nil when the profile is nil (no profile → no checks).
//
// Checks performed (in order):
//  1. AllowedEnvironments vs profile context (PLAN-010)
//  2. Test-context transport rules (PLAN-012)
//  3. Profile endpoint override vs allowed_hosts (PLAN-013)
func checkToolEnvironmentPreflight(stepID string, def *schema.ToolDef, profile *schema.RuntimeProfile) error {
	if profile == nil {
		return nil
	}

	// ── Check 1: AllowedEnvironments ────────────────────────────────────────
	if def.Governance != nil && len(def.Governance.AllowedEnvironments) > 0 {
		if !containsString(def.Governance.AllowedEnvironments, string(profile.Context)) {
			msg := fmt.Sprintf(
				"tool %q: allowed-environments does not include context %q\n"+
					"  allowed-environments: [%s]\n"+
					"  this tool is not configured to run in profile %q (context: %s)\n"+
					"  use `yawr plan --profile %s <runbook>` to see the full binding table",
				def.Name,
				profile.Context,
				strings.Join(def.Governance.AllowedEnvironments, ", "),
				profile.ID,
				profile.Context,
				profile.ID,
			)
			return &plannerPkg.PlanError{
				Code:   errkit.Wrap("PLAN-010", msg, plannerPkg.ErrContextMismatch),
				StepID: stepID,
				Detail: msg,
			}
		}
	}

	// ── Check 2: Test-context transport rules ───────────────────────────────
	if profile.Context != schema.ProfileContextTest {
		// ── Check 3: PLAN-013 endpoint override vs allowed_hosts ────────────
		// Only applies to mcp-http tools. The override host must be in the
		// tool definition's auth.allowed_hosts — Ratified Rule A: a profile
		// substitutes who acquires the token; it never changes where the token
		// may be sent. Enforced here (at plan time) so no override executes
		// without validation, satisfying the same-commit constraint.
		if len(profile.Tools) > 0 {
			if override, ok := profile.Tools[def.Name]; ok && override != nil && override.Endpoint != "" {
				if def.Transport.Type == schema.TransportMCPHTTP {
					if def.Transport.Auth == nil || len(def.Transport.Auth.AllowedHosts) == 0 {
						msg := fmt.Sprintf(
							"tool %q: profile endpoint override %q requires auth.allowed_hosts on the tool definition\n"+
								"  PLAN-013: endpoint overrides are only permitted when the tool definition declares\n"+
								"  auth.allowed_hosts — add the override host to allowed_hosts or configure auth:\n"+
								"  on the tool to enable PLAN-013 host validation",
							def.Name, override.Endpoint,
						)
						return &plannerPkg.PlanError{
							Code:   errkit.Wrap("PLAN-013", msg, plannerPkg.ErrEndpointHostNotAllowed),
							StepID: stepID,
							Detail: msg,
						}
					}
					u, parseErr := url.Parse(override.Endpoint)
					if parseErr != nil || u.Hostname() == "" {
						msg := fmt.Sprintf(
							"tool %q: profile endpoint override %q is not a valid URL with a hostname (PLAN-013)",
							def.Name, override.Endpoint,
						)
						return &plannerPkg.PlanError{
							Code:   errkit.Wrap("PLAN-013", msg, plannerPkg.ErrEndpointHostNotAllowed),
							StepID: stepID,
							Detail: msg,
						}
					}
					overrideHost := strings.ToLower(u.Hostname())
					for _, h := range def.Transport.Auth.AllowedHosts {
						if strings.ToLower(h) == overrideHost {
							return nil // override host is in allowed_hosts — permitted
						}
					}
					msg := fmt.Sprintf(
						"tool %q: profile endpoint override host %q is not in auth.allowed_hosts %v\n"+
							"  PLAN-013: a profile endpoint override must route to a host already declared\n"+
							"  in the tool definition's allowed_hosts — add %q to allowed_hosts, or\n"+
							"  remove the endpoint override from the profile",
						def.Name, u.Hostname(), def.Transport.Auth.AllowedHosts, u.Hostname(),
					)
					return &plannerPkg.PlanError{
						Code:   errkit.Wrap("PLAN-013", msg, plannerPkg.ErrEndpointHostNotAllowed),
						StepID: stepID,
						Detail: msg,
					}
				}
			}
		}
		return nil
	}
	mode := string(def.Transport.Type)
	switch mode {
	case string(schema.TransportMCPHTTP):
		msg := fmt.Sprintf(
			"tool %q: transport mode %q is never allowed in test context\n"+
				"  use transport: native with a runbook-backed mock for deterministic testing\n"+
				"  no override exists: mcp-http is unconditionally blocked in context: test",
			def.Name, mode,
		)
		return &plannerPkg.PlanError{
			Code:   errkit.Wrap("PLAN-012", msg, plannerPkg.ErrTestContextBinding),
			StepID: stepID,
			Detail: msg,
		}
	case string(schema.TransportMCP):
		if !profile.Transport.AllowSubprocessInTest {
			msg := fmt.Sprintf(
				"tool %q: transport mode %q is blocked in test context by default\n"+
					"  to allow an mcp subprocess in tests (e.g. for a hermetic fake MCP server),\n"+
					"  set transport.allow_subprocess_in_test: true in the runtime profile\n"+
					"  note: allow_subprocess_in_test is an auditable author assertion, NOT a sandbox\n"+
					"  the subprocess inherits the full parent environment (no network or env isolation)",
				def.Name, mode,
			)
			return &plannerPkg.PlanError{
				Code:   errkit.Wrap("PLAN-012", msg, plannerPkg.ErrTestContextBinding),
				StepID: stepID,
				Detail: msg,
			}
		}
	}
	// native transport is always allowed in test context.
	return nil
}

// containsString reports whether s is in ss (case-sensitive).
func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
