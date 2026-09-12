package tool

// repo_boundary_test.go — repo-wide guard against cross-repo path leaks in
// test sources.
//
// A test that reads files from a sibling repository via a ".." traversal is
// non-hermetic: a detached worktree resolves "../sibling" to the real working
// tree of that other repo, including uncommitted edits, making our
// clean-checkout reproducibility guarantee meaningless.
//
// TestRepoBoundary_NoExternalPaths walks every *_test.go file in the
// repository and applies two independent checks:
//
//  1. Sibling-repo name check — fails if the file contains any banned literal
//     (e.g. the name of a known sibling repo assembled at runtime to avoid
//     triggering the guard from its own source text).
//
//  2. ".." traversal check — fails if the file contains the Go string literal
//     `".."` and is not in the explicit allowlist below.  Every allowlisted
//     file carries a mandatory comment that states WHY the use is safe.
//     A new `".."` use in a test file that is not allowlisted fails the check,
//     forcing the author to justify it explicitly rather than slip it in
//     silently.
//
// Both checks report the offending file's path relative to the repo root so
// the failure is actionable from any directory.
//
// The guard file excludes itself from both checks by name.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// siblingRepoPatterns holds substrings that indicate a cross-repo reference.
// Patterns are assembled at runtime (concatenated) so the guard file's own
// source text does not trigger the check.
var siblingRepoPatterns = []string{
	// The private companion repo whose testdata must not be read by yawr tests.
	"yawr" + "-private",
}

// dotDotAllowlist is the exhaustive set of test files that are permitted to
// contain the Go string literal `".."`. Every entry must carry a reason.
// Adding a new cross-repo read to any of these files is still a violation —
// the allowlist only permits intra-repo traversal (navigating from a package
// directory up to the repo root, then back down into committed testdata).
var dotDotAllowlist = map[string]string{
	// repoRoot() helpers — navigate from the test package dir to the repo root.
	// Pattern: filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	// All paths remain inside the repo; nothing is appended past the root.
	"cmd/tools/echo/echo_test.go":                     "repoRoot() helper: 3-level ascent to repo root",
	"cmd/tools/fail/fail_test.go":                     "repoRoot() helper: 3-level ascent to repo root",
	"cmd/tools/json-emitter/json_emitter_test.go":     "repoRoot() helper: 3-level ascent to repo root",
	"cmd/tools/jsonrpc-server/jsonrpc_server_test.go": "repoRoot() helper: 3-level ascent to repo root",
	"cmd/tools/slow/slow_test.go":                     "repoRoot() helper: 3-level ascent to repo root",
	"cmd/tools/stub/stub_test.go":                     "repoRoot() helper: 3-level ascent to repo root",
	"internal/extension/test_main_test.go":            "repoRoot() helper: 2-level ascent to repo root",
	"internal/tool/test_main_test.go":                 "repoRoot() helper: 2-level ascent to repo root",

	// Relative paths that navigate within the repo (ascend then descend into committed testdata).
	"cmd/yawr/run_test.go":           `r21FixturePath: filepath.Join("..","..",internal/parser/testdata/...)  — stays inside repo`,
	"internal/serve/preview_test.go": `filepath.Abs(filepath.Join("..","..",examples/...)) — stays inside repo`,

	// Test-data strings: ".." appears as a string value being tested, not a path operation.
	"pkg/pkgcatalog/refvalidate_test.go": `table-driven test data: ".." is a path segment under validation (TV-DYN-REF-017), not a filesystem read`,
}

// TestRepoBoundary_NoExternalPaths walks every *_test.go file in the
// repository and fails on any sibling-repo reference or un-allowlisted ".."
// traversal.
func TestRepoBoundary_NoExternalPaths(t *testing.T) {
	moduleRoot := repoRoot()
	rootOutput, err := exec.Command("git", "-C", moduleRoot, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	root := strings.TrimSpace(string(rootOutput))
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--", "runtime/**/*_test.go")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := bytes.Split(bytes.TrimSuffix(output, []byte{0}), []byte{0})
	if len(files) < 100 {
		t.Fatalf("tracked test source inventory is unexpectedly small: %d", len(files))
	}
	for _, raw := range files {
		relSlash := filepath.ToSlash(string(raw))
		if relSlash == "runtime/internal/tool/repo_boundary_test.go" || relSlash == "internal/tool/repo_boundary_test.go" {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(relSlash))
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			t.Errorf("ReadFile(%s): %v", relSlash, readErr)
			continue
		}
		for _, violation := range repoBoundaryViolations(strings.TrimPrefix(relSlash, "runtime/"), data) {
			t.Errorf("%s: %s", relSlash, violation)
		}
	}
}

func repoBoundaryViolations(relSlash string, data []byte) []string {
	var violations []string
	lower := strings.ToLower(string(data))
	for _, pat := range siblingRepoPatterns {
		if strings.Contains(lower, pat) {
			violations = append(violations, "contains cross-repo reference "+pat)
		}
	}
	if strings.Contains(string(data), `".."`) {
		if _, allowed := dotDotAllowlist[relSlash]; !allowed {
			violations = append(violations, `contains ".." path traversal`)
		}
	}
	return violations
}

func TestRepoBoundaryMutationDetectsExternalPaths(t *testing.T) {
	data := []byte(`package fixture
var sibling = "yawr-private"
var outside = filepath.Join("..", "fixture")
`)
	violations := repoBoundaryViolations("mutation/external_test.go", data)
	if len(violations) != 2 {
		t.Fatalf("mutation produced %d violations, want 2: %v", len(violations), violations)
	}
}
