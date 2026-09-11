package testutil

// SpecTag is the structured form of the // spec: {file} §{section} annotation.
// Tests use this to link implementation to normative spec rules.
//
// The spec-coverage tool locates calls to Tag() via AST analysis and builds
// a coverage matrix mapping spec rules → test functions.
type SpecTag struct {
	File    string // e.g., "03-schema-vnext.md"
	Section string // e.g., "§4.2"
	Rule    string // the MUST/MUST NOT text verbatim from the spec
}

// Tag is a no-op at runtime. Its sole purpose is to be discoverable by the
// spec-coverage tool, which finds these calls via static AST analysis.
//
// Usage inside a test function:
//
//	_ = testutil.Tag("03-schema-vnext.md", "§4.2", "step MUST have a type field")
func Tag(file, section, rule string) SpecTag {
	return SpecTag{File: file, Section: section, Rule: rule}
}
