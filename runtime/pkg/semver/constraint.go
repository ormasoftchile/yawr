package semver

import (
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

// comparatorKind enumerates the supported comparator forms.
type comparatorKind int

const (
	kindCaret comparatorKind = iota
	kindTilde
	kindGE
	kindLE
	kindGT
	kindLT
	kindExact
)

// comparator is one parsed element of a space-separated conjunctive
// constraint (e.g. "^1.4.0" or ">=1.4.0 <2.0.0").
type comparator struct {
	kind comparatorKind
	ver  Version
}

// Constraint is a parsed, conjunctive (AND) sequence of comparators.
type Constraint struct {
	raw         string
	comparators []comparator
}

// String returns the original constraint text.
func (c Constraint) String() string { return c.raw }

// unsupportedPatterns lists substrings whose presence identifies one of the
// explicitly unsupported constraint forms (PKG-003), so a precise diagnostic
// can be given instead of a generic parse failure.
var unsupportedPatterns = []struct {
	needle string
	reason string
}{
	{"||", "disjunction ('||') is not supported"},
	{"*", "wildcards ('*') are not supported"},
	{"!=", "'!=' is not supported"},
	{" - ", "hyphen ranges are not supported"},
	{"+", "build metadata ('+') in a constraint is not supported"},
}

// ParseConstraint parses a version constraint per the MVP grammar
// by the current version constraint grammar.
// Returns a PKG-003 error for any malformed or unsupported form.
func ParseConstraint(s string) (Constraint, error) {
	trimmed := s
	if trimmed == "" {
		return Constraint{}, errkit.New("PKG-003", "empty version constraint")
	}
	for _, u := range unsupportedPatterns {
		if strings.Contains(trimmed, u.needle) {
			return Constraint{}, errkit.New("PKG-003", fmt.Sprintf("constraint %q: %s", s, u.reason))
		}
	}
	// Reject any whitespace variant other than a single ASCII space between
	// comparators, and leading/trailing whitespace.
	if trimmed != strings.TrimSpace(trimmed) {
		return Constraint{}, errkit.New("PKG-003", fmt.Sprintf("constraint %q: leading/trailing whitespace is not supported", s))
	}
	if strings.ContainsAny(trimmed, "\t\n\r") {
		return Constraint{}, errkit.New("PKG-003", fmt.Sprintf("constraint %q: only a single ASCII space separates comparators", s))
	}

	parts := strings.Split(trimmed, " ")
	comparators := make([]comparator, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return Constraint{}, errkit.New("PKG-003", fmt.Sprintf("constraint %q: repeated or stray whitespace", s))
		}
		cmp, err := parseComparator(part, s)
		if err != nil {
			return Constraint{}, err
		}
		comparators = append(comparators, cmp)
	}
	return Constraint{raw: s, comparators: comparators}, nil
}

func parseComparator(part, whole string) (comparator, error) {
	var kind comparatorKind
	var verStr string
	switch {
	case strings.HasPrefix(part, "^"):
		kind, verStr = kindCaret, part[1:]
	case strings.HasPrefix(part, "~"):
		kind, verStr = kindTilde, part[1:]
	case strings.HasPrefix(part, ">="):
		kind, verStr = kindGE, part[2:]
	case strings.HasPrefix(part, "<="):
		kind, verStr = kindLE, part[2:]
	case strings.HasPrefix(part, ">"):
		kind, verStr = kindGT, part[1:]
	case strings.HasPrefix(part, "<"):
		kind, verStr = kindLT, part[1:]
	case strings.HasPrefix(part, "="):
		kind, verStr = kindExact, part[1:]
	default:
		kind, verStr = kindExact, part
	}
	if verStr == "" {
		return comparator{}, errkit.New("PKG-003", fmt.Sprintf("constraint %q: comparator %q has no version", whole, part))
	}
	// Partial versions (^1, ~1.2) are explicitly unsupported; ParseVersion's
	// strict MAJOR.MINOR.PATCH requirement already rejects them, but produce
	// the PKG-003 (malformed constraint) code rather than PKG-025 (version)
	// since the problem is the constraint shape, not a standalone version.
	v, err := ParseVersion(verStr)
	if err != nil {
		return comparator{}, errkit.New("PKG-003", fmt.Sprintf("constraint %q: comparator %q: %v", whole, part, err))
	}
	return comparator{kind: kind, ver: v}, nil
}

// Satisfies reports whether v satisfies every comparator in the constraint
// (conjunctive/AND), applying the prerelease rule: a version with a
// prerelease component only satisfies a comparator whose own version has a
// prerelease component sharing the same major.minor.patch.
func (c Constraint) Satisfies(v Version) bool {
	for _, cmp := range c.comparators {
		if !satisfiesOne(v, cmp) {
			return false
		}
	}
	if v.Prerelease != "" && !anyComparatorSharesPrerelease(c, v) {
		return false
	}
	return true
}

// anyComparatorSharesPrerelease implements the npm/Cargo prerelease rule:
// a prerelease version only satisfies the constraint at all if at least one
// comparator's own version is a prerelease sharing v's major.minor.patch.
func anyComparatorSharesPrerelease(c Constraint, v Version) bool {
	for _, cmp := range c.comparators {
		if cmp.ver.Prerelease != "" && sameMajorMinorPatch(cmp.ver, v) {
			return true
		}
	}
	return false
}

func satisfiesOne(v Version, cmp comparator) bool {
	switch cmp.kind {
	case kindExact:
		return v.Compare(cmp.ver) == 0
	case kindGE:
		return v.Compare(cmp.ver) >= 0
	case kindLE:
		return v.Compare(cmp.ver) <= 0
	case kindGT:
		return v.Compare(cmp.ver) > 0
	case kindLT:
		return v.Compare(cmp.ver) < 0
	case kindCaret:
		lower, upper := caretRange(cmp.ver)
		return v.Compare(lower) >= 0 && v.Compare(upper) < 0
	case kindTilde:
		lower, upper := tildeRange(cmp.ver)
		return v.Compare(lower) >= 0 && v.Compare(upper) < 0
	}
	return false
}

// caretRange returns [lower, upper) for "^ver" per the ratified 0.x narrowing:
// ^1.4.2 -> >=1.4.2 <2.0.0; ^0.4.2 -> >=0.4.2 <0.5.0; ^0.0.3 -> >=0.0.3 <0.0.4.
func caretRange(ver Version) (Version, Version) {
	lower := ver
	lower.Prerelease = ver.Prerelease
	upper := Version{}
	switch {
	case ver.Major > 0:
		upper = Version{Major: ver.Major + 1}
	case ver.Minor > 0:
		upper = Version{Major: 0, Minor: ver.Minor + 1}
	default:
		upper = Version{Major: 0, Minor: 0, Patch: ver.Patch + 1}
	}
	return ver, upper
}

// tildeRange returns [lower, upper) for "~ver": ~1.4.2 -> >=1.4.2 <1.5.0.
func tildeRange(ver Version) (Version, Version) {
	upper := Version{Major: ver.Major, Minor: ver.Minor + 1}
	return ver, upper
}

// Intersect returns a Constraint whose comparator list is the concatenation
// of c and other's comparators (i.e. the AND of both), per
// the rule that project and
// runbook-level requires: constraints on the same package are intersected.
// The caller is responsible for detecting an empty (unsatisfiable)
// intersection by testing candidate versions against Satisfies.
func (c Constraint) Intersect(other Constraint) Constraint {
	merged := make([]comparator, 0, len(c.comparators)+len(other.comparators))
	merged = append(merged, c.comparators...)
	merged = append(merged, other.comparators...)
	sep := " "
	raw := c.raw
	if raw == "" {
		raw = other.raw
	} else if other.raw != "" {
		raw = raw + sep + other.raw
	}
	return Constraint{raw: raw, comparators: merged}
}
