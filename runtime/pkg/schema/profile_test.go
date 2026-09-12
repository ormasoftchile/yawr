package schema_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// validProfileYAML is a minimal valid yawr.runtime-profile/v1 document.
const validProfileYAML = `
apiVersion: yawr.runtime-profile/v1
id: vscode-autonomous
context: vscode-operator
attendance: unattended
approval:
  scope:
    allow_read: true
    allow_mutating: false
    allow_destructive: false
transport:
  allow_subprocess_in_test: false
tools:
  icm:
    endpoint: https://icm-mcp-prod.example.net/v1/
`

// TestParseProfile_ValidRoundTrip verifies a valid profile parses completely.
func TestParseProfile_ValidRoundTrip(t *testing.T) {
	p, err := schema.ParseProfileBytes([]byte(validProfileYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.ID != "vscode-autonomous" {
		t.Errorf("ID: got %q, want %q", p.ID, "vscode-autonomous")
	}
	if p.Context != schema.ProfileContextVSCodeOperator {
		t.Errorf("Context: got %q, want %q", p.Context, schema.ProfileContextVSCodeOperator)
	}
	if p.Attendance != schema.ProfileAttendanceUnattended {
		t.Errorf("Attendance: got %q, want %q", p.Attendance, schema.ProfileAttendanceUnattended)
	}
	if !p.Approval.Scope.AllowRead {
		t.Error("allow_read: got false, want true")
	}
	if p.Approval.Scope.AllowMutating {
		t.Error("allow_mutating: got true, want false")
	}
	icm, ok := p.Tools["icm"]
	if !ok {
		t.Error("tools.icm: not found")
	} else if icm.Endpoint != "https://icm-mcp-prod.example.net/v1/" {
		t.Errorf("tools.icm.endpoint: got %q", icm.Endpoint)
	}
}

// TestParseProfile_AllFiveContextsAccepted verifies each canonical context
// value is accepted.
func TestParseProfile_AllFiveContextsAccepted(t *testing.T) {
	contexts := []schema.ProfileContext{
		schema.ProfileContextCLIOperator,
		schema.ProfileContextVSCodeOperator,
		schema.ProfileContextCI,
		schema.ProfileContextHeadlessServer,
		schema.ProfileContextTest,
	}
	for _, ctx := range contexts {
		t.Run(string(ctx), func(t *testing.T) {
			yml := "apiVersion: yawr.runtime-profile/v1\nid: test\ncontext: " + string(ctx) + "\nattendance: attended\n"
			_, err := schema.ParseProfileBytes([]byte(yml))
			if err != nil {
				t.Errorf("context %q rejected unexpectedly: %v", ctx, err)
			}
		})
	}
}

// TestParseProfile_UnknownContextRejected verifies an unknown context value
// is rejected with a clear error.
func TestParseProfile_UnknownContextRejected(t *testing.T) {
	yml := "apiVersion: yawr.runtime-profile/v1\nid: test\ncontext: not-a-real-context\nattendance: attended\n"
	_, err := schema.ParseProfileBytes([]byte(yml))
	if err == nil {
		t.Fatal("expected error for unknown context, got nil")
	}
}

// TestParseProfile_UnknownAttendanceRejected verifies an unknown attendance
// value is rejected.
func TestParseProfile_UnknownAttendanceRejected(t *testing.T) {
	yml := "apiVersion: yawr.runtime-profile/v1\nid: test\ncontext: ci\nattendance: sometimes\n"
	_, err := schema.ParseProfileBytes([]byte(yml))
	if err == nil {
		t.Fatal("expected error for unknown attendance, got nil")
	}
}

// TestParseProfile_AttendanceOrthogonalToContext verifies that
// vscode-operator + unattended is valid and not coerced to attended.
// This is the canonical example from the spec: "An autonomous process
// running inside VS Code is context: vscode-operator + attendance: unattended".
func TestParseProfile_AttendanceOrthogonalToContext(t *testing.T) {
	yml := "apiVersion: yawr.runtime-profile/v1\nid: test\ncontext: vscode-operator\nattendance: unattended\n"
	p, err := schema.ParseProfileBytes([]byte(yml))
	if err != nil {
		t.Fatalf("vscode-operator + unattended rejected: %v", err)
	}
	if p.Context != schema.ProfileContextVSCodeOperator {
		t.Errorf("Context coerced: got %q, want vscode-operator", p.Context)
	}
	if p.Attendance != schema.ProfileAttendanceUnattended {
		t.Errorf("Attendance coerced: got %q, want unattended", p.Attendance)
	}
}

func TestParseProfile_UnknownFieldsRejected(t *testing.T) {
	yml := `
apiVersion: yawr.runtime-profile/v1
id: test
context: ci
attendance: unattended
extends: base-profile
approval:
  scope:
    allow_read: true
`
	if _, err := schema.ParseProfileBytes([]byte(yml)); err == nil {
		t.Fatal("expected unknown profile field to be rejected")
	}
}

// TestParseProfile_TransportModeRewriteRejected verifies that a profile
// attempting to set a tool's transport mode is rejected (PROF-001).
func TestParseProfile_TransportModeRewriteRejected(t *testing.T) {
	yml := `
apiVersion: yawr.runtime-profile/v1
id: test
context: ci
attendance: unattended
tools:
  icm:
    mode: stdio
`
	_, err := schema.ParseProfileBytes([]byte(yml))
	if err == nil {
		t.Fatal("expected PROF-001 error for transport-mode rewrite, got nil")
	}
}

// TestParseProfile_WrongAPIVersionRejected verifies an unknown apiVersion is
// rejected with a clear error.
func TestParseProfile_WrongAPIVersionRejected(t *testing.T) {
	yml := "apiVersion: runtime-profile/v2\nid: test\ncontext: ci\nattendance: unattended\n"
	_, err := schema.ParseProfileBytes([]byte(yml))
	if err == nil {
		t.Fatal("expected error for wrong apiVersion, got nil")
	}
}

// ── Profile ID validation ──────────────────────────────────────────────────────

// TestParseProfile_ValidIDs verifies that IDs matching [a-z0-9][a-z0-9-]* up
// to 64 characters are accepted.
func TestParseProfile_ValidIDs(t *testing.T) {
	validIDs := []string{
		"ci",
		"ci-prod",
		"vscode-autonomous",
		"a",
		"0",
		"abc123",
		"my-profile-01",
		// exactly 64 chars:
		"a234567890123456789012345678901234567890123456789012345678901234",
	}
	for _, id := range validIDs {
		yml := "apiVersion: yawr.runtime-profile/v1\nid: " + id + "\ncontext: ci\nattendance: unattended\n"
		_, err := schema.ParseProfileBytes([]byte(yml))
		if err != nil {
			t.Errorf("valid ID %q rejected: %v", id, err)
		}
	}
}

// TestParseProfile_InvalidIDs verifies that IDs not matching the pattern are
// rejected with a clear error.
func TestParseProfile_InvalidIDs(t *testing.T) {
	invalidIDs := []string{
		"",                    // empty
		"My-Profile",          // uppercase
		"-starts-with-hyphen", // hyphen first
		"has space",           // space
		"has/slash",           // slash
		"HAS_UNDERSCORE",      // underscore + uppercase
		// 65 chars:
		"a2345678901234567890123456789012345678901234567890123456789012345",
	}
	for _, id := range invalidIDs {
		yml := "apiVersion: yawr.runtime-profile/v1\nid: " + id + "\ncontext: ci\nattendance: unattended\n"
		_, err := schema.ParseProfileBytes([]byte(yml))
		if err == nil {
			t.Errorf("invalid ID %q was accepted; expected rejection", id)
		}
	}
}

// TestParseProfile_EmptyIDRejected is an explicit check that an absent/empty
// id field is rejected, separate from the pattern tests.
func TestParseProfile_EmptyIDRejected(t *testing.T) {
	yml := "apiVersion: yawr.runtime-profile/v1\ncontext: ci\nattendance: unattended\n"
	_, err := schema.ParseProfileBytes([]byte(yml))
	if err == nil {
		t.Fatal("expected error for missing id, got nil")
	}
}

// ── DiscoverProfiles ──────────────────────────────────────────────────────────

// TestDiscoverProfiles_FindsValidProfiles verifies that DiscoverProfiles
// returns profiles from a directory containing valid profile YAML files.
func TestDiscoverProfiles_FindsValidProfiles(t *testing.T) {
	dir := t.TempDir()
	writeProfileFile(t, dir, "ci-prod.yaml", `apiVersion: yawr.runtime-profile/v1
id: ci-prod
context: ci
attendance: unattended
`)
	writeProfileFile(t, dir, "vscode.yaml", `apiVersion: yawr.runtime-profile/v1
id: vscode
context: vscode-operator
attendance: attended
`)
	profiles, errs := schema.DiscoverProfiles([]string{dir})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}
	ids := make(map[string]bool)
	for _, p := range profiles {
		ids[p.ID] = true
	}
	if !ids["ci-prod"] || !ids["vscode"] {
		t.Errorf("expected ids {ci-prod, vscode}; got %v", ids)
	}
}

// TestDiscoverProfiles_SkipsNonProfileFiles verifies that non-profile YAML
// files (runbooks, tool definitions, package manifests) are silently skipped.
func TestDiscoverProfiles_SkipsNonProfileFiles(t *testing.T) {
	dir := t.TempDir()
	writeProfileFile(t, dir, "runbook.yaml", `apiVersion: yawr.runbook/v1
id: my-runbook
name: test
flow: []
`)
	writeProfileFile(t, dir, "good.yaml", `apiVersion: yawr.runtime-profile/v1
id: good
context: test
attendance: unattended
`)
	writeProfileFile(t, dir, "not-yaml.txt", `not yaml content at all`)

	profiles, errs := schema.DiscoverProfiles([]string{dir})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(profiles) != 1 || profiles[0].ID != "good" {
		t.Errorf("expected only 'good' profile; got %d profiles: %v", len(profiles), profiles)
	}
}

// TestDiscoverProfiles_ReportsErrorForInvalidProfile verifies that a YAML file
// with the correct apiVersion but invalid content (e.g. bad ID) is reported as
// an error, not silently skipped.
func TestDiscoverProfiles_ReportsErrorForInvalidProfile(t *testing.T) {
	dir := t.TempDir()
	writeProfileFile(t, dir, "bad.yaml", `apiVersion: yawr.runtime-profile/v1
id: "BAD UPPERCASE"
context: ci
attendance: unattended
`)
	_, errs := schema.DiscoverProfiles([]string{dir})
	if len(errs) == 0 {
		t.Fatal("expected error for invalid profile ID, got none")
	}
}

// TestDiscoverProfiles_EmptyDirReturnsNothing verifies that an empty directory
// returns an empty, non-error result.
func TestDiscoverProfiles_EmptyDirReturnsNothing(t *testing.T) {
	dir := t.TempDir()
	profiles, errs := schema.DiscoverProfiles([]string{dir})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(profiles) != 0 {
		t.Errorf("expected 0 profiles from empty dir; got %d", len(profiles))
	}
}

// TestDiscoverProfiles_NilDirs returns empty result without error.
func TestDiscoverProfiles_NilDirs(t *testing.T) {
	profiles, errs := schema.DiscoverProfiles(nil)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(profiles) != 0 {
		t.Errorf("expected 0 profiles for nil dirs; got %d", len(profiles))
	}
}

// writeProfileFile is a test helper that writes content to name inside dir.
func writeProfileFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writeProfileFile %s: %v", name, err)
	}
}
