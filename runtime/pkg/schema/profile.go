package schema

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// RuntimeProfileAPIVersion is the required apiVersion discriminator for
// runtime profile documents.
const RuntimeProfileAPIVersion = "yawr.runtime-profile/v1"

// ProfileContext enumerates the valid execution-host contexts for a runtime
// profile. These values describe WHERE the process is running, not the
// approval policy, attendance, or incident state. The set is closed; any
// value not in this list is rejected at validation time.
//
// Key semantic constraint: context is ORTHOGONAL to attendance. A VS Code
// host running an autonomous (unattended) process is context:
// vscode-operator + attendance: unattended. Operator contexts must NOT
// auto-imply attended. Do not derive attendance from context.
type ProfileContext string

const (
	ProfileContextCLIOperator    ProfileContext = "cli-operator"
	ProfileContextVSCodeOperator ProfileContext = "vscode-operator"
	ProfileContextCI             ProfileContext = "ci"
	ProfileContextHeadlessServer ProfileContext = "headless-server"
	ProfileContextTest           ProfileContext = "test"
)

// validProfileContexts is the exhaustive, closed set of accepted context
// values. Reject anything not in this list with a clear error.
var validProfileContexts = map[ProfileContext]bool{
	ProfileContextCLIOperator:    true,
	ProfileContextVSCodeOperator: true,
	ProfileContextCI:             true,
	ProfileContextHeadlessServer: true,
	ProfileContextTest:           true,
}

// ProfileAttendance enumerates whether a human operator is expected to be
// present during execution. It is top-level and orthogonal to context.
type ProfileAttendance string

const (
	ProfileAttendanceAttended   ProfileAttendance = "attended"
	ProfileAttendanceUnattended ProfileAttendance = "unattended"
)

var validProfileAttendance = map[ProfileAttendance]bool{
	ProfileAttendanceAttended:   true,
	ProfileAttendanceUnattended: true,
}

// ProfileApprovalScope defines which action classifications are allowed
// under this profile without additional approval gates.
type ProfileApprovalScope struct {
	AllowRead        bool `yaml:"allow_read"        json:"allow_read"`
	AllowMutating    bool `yaml:"allow_mutating"    json:"allow_mutating"`
	AllowDestructive bool `yaml:"allow_destructive" json:"allow_destructive"`
}

// ProfileApproval is the approval block of a RuntimeProfile.
type ProfileApproval struct {
	Scope ProfileApprovalScope `yaml:"scope" json:"scope"`
}

// ProfileTransport holds profile-level transport settings. A profile MUST
// NOT set a tool's transport mode; that would constitute transport-mode
// rewriting, which is forbidden (enforced at validation time). Profiles
// carry only configuration that is orthogonal to tool selection — test
// affordances (e.g. allow_subprocess_in_test) and future auth parameters.
type ProfileTransport struct {
	AllowSubprocessInTest bool `yaml:"allow_subprocess_in_test" json:"allow_subprocess_in_test"`
}

// ProfileToolOverride holds per-tool parameter overrides. A profile MUST
// NOT set a tool's transport mode; attempting to do so is rejected with a
// clear error at validation time (transport-mode rewriting is forbidden).
// Profiles may carry context, authentication parameters, and endpoint
// overrides; they do not re-resolve or override which package was selected.
type ProfileToolOverride struct {
	// Provider overrides the auth provider for this tool (Ratified Rule A:
	// a profile MAY substitute WHO acquires the token). When set, the
	// runtime uses this provider instead of the tool definition's
	// auth.provider. Scope and AllowedHosts are ALWAYS taken from the tool
	// definition — they can never be overridden by a profile.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`

	// Endpoint overrides the tool's connection endpoint (e.g. for IcM in
	// a staging vs. production profile). The override host MUST be present
	// in the tool definition's auth.allowed_hosts (PLAN-013), enforced at
	// planning time before execution begins.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`

	// Mode is present ONLY to allow the validator to detect and reject
	// transport-mode rewriting attempts. It is never read for execution.
	// If any profile document sets this field, ParseProfileFile returns
	// an error (PROF-001).
	Mode string `yaml:"mode,omitempty" json:"-"`
}

// RuntimeProfile is the parsed, validated representation of a
// yawr.runtime-profile/v1 document. Profiles are FLAT — there is no inheritance,
// no extends, no merging of parent profiles. Per-tool overrides are the
// only composition point.
//
// A profile parameterizes EXECUTION AFTER tool selection: it carries context,
// attendance declaration, approval scope, transport affordances, and per-tool
// parameter overrides. It does NOT re-resolve or override which package or
// tool definition was selected by --package-map or the project catalog.
// Any profile-aware resolution must consume plan.Tools (post-catalog,
// post-package-map), not re-resolve toolRefs independently.
type RuntimeProfile struct {
	APIVersion string                          `yaml:"apiVersion"          json:"apiVersion"`
	ID         string                          `yaml:"id"                  json:"id"`
	Context    ProfileContext                  `yaml:"context"             json:"context"`
	Attendance ProfileAttendance               `yaml:"attendance"          json:"attendance"`
	Approval   ProfileApproval                 `yaml:"approval"            json:"approval"`
	Transport  ProfileTransport                `yaml:"transport,omitempty" json:"transport,omitempty"`
	Tools      map[string]*ProfileToolOverride `yaml:"tools,omitempty"     json:"tools,omitempty"`
}

// ParseProfileFile reads a file from disk, parses it as a yawr.runtime-profile/v1
// document, and validates it. It follows the same load-then-validate pattern
// as loadConfigFile (cmd/yawr/packagemap.go) and ParseToolFile
// (internal/tool/scan.go): a missing file is a hard error, and a
// present-but-invalid file also fails immediately.
func ParseProfileFile(path string) (*RuntimeProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("runtime profile: %w", err)
	}
	p, err := ParseProfileBytes(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// ParseProfileBytes parses and validates a yawr.runtime-profile/v1 document from
// raw YAML bytes. It enforces:
//   - apiVersion must be "yawr.runtime-profile/v1"
//   - context must be one of the five canonical values
//   - attendance must be "attended" or "unattended"
//   - no tool override may set a transport mode (PROF-001)
func ParseProfileBytes(data []byte) (*RuntimeProfile, error) {
	var p RuntimeProfile
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&p); err != nil {
		return nil, fmt.Errorf("runtime profile: parse: %w", err)
	}
	if err := validateProfile(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// validateProfile checks all semantic constraints on a parsed RuntimeProfile.
func validateProfile(p *RuntimeProfile) error {
	if p.APIVersion != RuntimeProfileAPIVersion {
		return fmt.Errorf("runtime profile: apiVersion must be %q, got %q", RuntimeProfileAPIVersion, p.APIVersion)
	}
	if !validProfileContexts[p.Context] {
		return fmt.Errorf("runtime profile %q: unknown context %q; valid values: cli-operator, vscode-operator, ci, headless-server, test", p.ID, p.Context)
	}
	if !validProfileAttendance[p.Attendance] {
		return fmt.Errorf("runtime profile %q: unknown attendance %q; valid values: attended, unattended", p.ID, p.Attendance)
	}
	// PROF-001: reject any profile that attempts transport-mode rewriting.
	for toolName, override := range p.Tools {
		if override == nil {
			continue
		}
		if override.Mode != "" {
			return fmt.Errorf("runtime profile %q: tool %q sets transport mode %q; profiles must not rewrite transport modes (PROF-001)", p.ID, toolName, override.Mode)
		}
	}
	// Profile ID constraint (Barbara ratified 2026-08-17):
	//   pattern:  [a-z0-9][a-z0-9-]*
	//   max length: 64 characters
	// Profiles are stored as <id>.yaml so the ID is a filename stem; the
	// pattern matches valid filesystem identifiers without special chars.
	if p.ID == "" {
		return fmt.Errorf("runtime profile: id is required")
	}
	if !profileIDRegexp.MatchString(p.ID) {
		return fmt.Errorf("runtime profile %q: id must match [a-z0-9][a-z0-9-]* (got %q)", p.ID, p.ID)
	}
	if len(p.ID) > profileIDMaxLen {
		return fmt.Errorf("runtime profile %q: id exceeds maximum length of %d characters", p.ID, profileIDMaxLen)
	}
	return nil
}

// profileIDRegexp is the compiled pattern for profile ID validation.
// Barbara-ratified 2026-08-17: [a-z0-9][a-z0-9-]*, max 64 chars.
var profileIDRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

const profileIDMaxLen = 64

// DiscoverProfiles scans each directory in dirs for *.yaml files, attempts to
// parse each as a yawr.runtime-profile/v1 document, and returns all valid profiles
// found. Files that are valid YAML but not yawr.runtime-profile/v1 documents are
// silently skipped. Files that claim to be runtime profiles (correct apiVersion)
// but fail validation are reported as errors and excluded from the result.
//
// The returned error slice is never nil — it is empty when all candidates
// parse cleanly. Callers must check both slices.
func DiscoverProfiles(dirs []string) ([]*RuntimeProfile, []error) {
	var profiles []*RuntimeProfile
	var errs []error
	seen := make(map[string]bool)

	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		entries, readErr := fs.ReadDir(os.DirFS(dir), ".")
		if readErr != nil {
			errs = append(errs, fmt.Errorf("profile discovery: cannot read directory %q: %w", dir, readErr))
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
				continue
			}
			fullPath := filepath.Join(dir, name)
			if seen[fullPath] {
				continue
			}
			seen[fullPath] = true

			p, isProfile, parseErr := tryParseProfileFileForDiscovery(fullPath)
			if !isProfile {
				// Not a runtime-profile document — silently skip.
				continue
			}
			if parseErr != nil {
				errs = append(errs, fmt.Errorf("profile discovery: %s: %w", fullPath, parseErr))
				continue
			}
			profiles = append(profiles, p)
		}
	}
	return profiles, errs
}

// tryParseProfileFileForDiscovery reads a YAML file and attempts to
// interpret it as a yawr.runtime-profile/v1 document. It returns:
//
//   - (profile, true, nil) — valid profile
//   - (nil, false, nil)    — not a profile file (apiVersion mismatch or non-YAML)
//   - (nil, true, err)     — is a profile file but fails validation
//
// The isProfile=false fast path avoids flooding the user with errors about
// runbook.yaml, tool definitions, package manifests, etc. that share a
// directory with profile files.
func tryParseProfileFileForDiscovery(path string) (profile *RuntimeProfile, isProfile bool, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return nil, false, readErr
	}

	// Peek at apiVersion without full validation.
	var peek struct {
		APIVersion string `yaml:"apiVersion"`
	}
	if unmarshalErr := yaml.Unmarshal(data, &peek); unmarshalErr != nil {
		return nil, false, nil // malformed YAML — not a profile file
	}
	if peek.APIVersion != RuntimeProfileAPIVersion {
		return nil, false, nil // different document type — silently skip
	}

	// It claims to be a runtime profile. Validate fully.
	p, parseErr := ParseProfileBytes(data)
	if parseErr != nil {
		return nil, true, parseErr
	}
	return p, true, nil
}
