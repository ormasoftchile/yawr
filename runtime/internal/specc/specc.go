// Package specc implements the spec-coverage analysis tool.
// It scans spec/*.md for RFC 2119 keywords (MUST, MUST NOT, SHOULD, etc.)
// and cross-references test files tagged with `// spec: {file} §{section}` comments.
package specc

// Rule represents a single MUST/MUST NOT rule extracted from the spec.
type Rule struct {
	File    string
	Section string
	Text    string
}

// Coverage reports spec rule coverage from test tags.
type Coverage struct {
	Total   int
	Covered int
	Rules   []Rule
}

// Analyze scans specDir for rules and testDir for tags.
// Returns a Coverage report.
func Analyze(specDir, testDir string) (*Coverage, error) {
	// TODO: implement in Phase 1
	return nil, nil
}
