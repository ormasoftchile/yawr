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
	"os"
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
	root := repoRoot()

	// skipDirs are directory names (not paths) to prune during the walk.
	skipDirs := map[string]bool{
		".git":         true,
		".testtools":   true,
		"vendor":       true,
		"node_modules": true,
	}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		// Relative path from repo root — used in error messages and allowlist keys.
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		// Normalise separator to forward slash so allowlist keys are OS-independent.
		relSlash := filepath.ToSlash(rel)

		// The guard file excludes itself.
		if d.Name() == "repo_boundary_test.go" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("ReadFile(%s): %v", relSlash, err)
			return nil
		}
		lower := strings.ToLower(string(data))

		// Check 1 — sibling-repo name patterns.
		for _, pat := range siblingRepoPatterns {
			if strings.Contains(lower, pat) {
				t.Errorf("%s: contains cross-repo reference %q — "+
					"tests must read only files inside the repo root; "+
					"move the fixture into testdata/ and delete the external path",
					relSlash, pat)
			}
		}

		// Check 2 — ".." traversal.
		if strings.Contains(string(data), `".."`) {
			reason, allowed := dotDotAllowlist[relSlash]
			if !allowed {
				t.Errorf("%s: contains \"..\" path traversal — "+
					"if this navigates within the repo, add it to dotDotAllowlist "+
					"in internal/tool/repo_boundary_test.go with a reason; "+
					"if it reads from a sibling repo, that is a hermeticity violation",
					relSlash)
			} else {
				t.Logf("%s: \"..\" allowed (%s)", relSlash, reason)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
}
