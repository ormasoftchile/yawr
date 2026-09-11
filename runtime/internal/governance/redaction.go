package governance

import (
	"fmt"
	"regexp"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

// Redactor applies a set of compiled redaction patterns to string values.
type Redactor struct {
	patterns []compiledPattern
}

type compiledPattern struct {
	re          *regexp.Regexp
	replacement string
}

// NewRedactor compiles all RedactionPatterns. Returns error if any pattern is invalid RE2.
func NewRedactor(patterns []*governance.RedactionPattern) (*Redactor, error) {
	compiled := make([]compiledPattern, 0, len(patterns))

	for i, p := range patterns {
		if p == nil {
			continue
		}

		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to compile redaction pattern %d (%q): %w", i, p.Pattern, err)
		}

		compiled = append(compiled, compiledPattern{
			re:          re,
			replacement: p.Replacement,
		})
	}

	return &Redactor{patterns: compiled}, nil
}

// RedactString applies all patterns to s, returning the scrubbed string and match count.
func (r *Redactor) RedactString(s string) (string, int) {
	matchCount := 0
	result := s

	for _, p := range r.patterns {
		if p.re.MatchString(result) {
			result = p.re.ReplaceAllString(result, p.replacement)
			matchCount++
		}
	}

	return result, matchCount
}

// RedactMap recursively walks a map and redacts all string values in-place.
// Returns total match count across all patterns and all values.
func (r *Redactor) RedactMap(m map[string]any) int {
	totalMatches := 0

	for k, v := range m {
		switch val := v.(type) {
		case string:
			redacted, matches := r.RedactString(val)
			if matches > 0 {
				m[k] = redacted
				totalMatches += matches
			}
		case map[string]any:
			totalMatches += r.RedactMap(val)
		case []any:
			for i, item := range val {
				if itemMap, ok := item.(map[string]any); ok {
					totalMatches += r.RedactMap(itemMap)
				} else if itemStr, ok := item.(string); ok {
					redacted, matches := r.RedactString(itemStr)
					if matches > 0 {
						val[i] = redacted
						totalMatches += matches
					}
				}
			}
		}
	}

	return totalMatches
}
