// Package semver implements strict SemVer 2.0.0 parsing, ordering, and the
// small conjunctive constraint grammar defined in
// The current version constraint grammar.
//
// This is deliberately not a general-purpose SemVer library: it implements
// exactly the subset ratified for the YAWR Tool Packages MVP (AR-TP-4) —
// strict versions (no leading "v"), single-comparator forms (^, ~, >=, <=,
// >, <, optional "="), space-separated conjunction (AND) of comparators,
// and no disjunction, wildcards, hyphen ranges, or partial versions.
package semver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

// Version is a parsed, strict SemVer 2.0.0 version.
type Version struct {
	Major      uint64
	Minor      uint64
	Patch      uint64
	Prerelease string // empty if none; dot-separated identifiers, verbatim
	Build      string // empty if none; ignored for ordering/satisfaction
	raw        string
}

// String returns the original input string.
func (v Version) String() string { return v.raw }

var versionRE = regexp.MustCompile(
	`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`,
)

// ParseVersion parses a strict SemVer 2.0.0 version string. A leading "v" is
// rejected, not stripped. Returns a PKG-025 error on any non-conformance.
func ParseVersion(s string) (Version, error) {
	if strings.HasPrefix(s, "v") || strings.HasPrefix(s, "V") {
		return Version{}, errkit.New("PKG-025", fmt.Sprintf("version %q: leading 'v' is rejected, not stripped; use strict SemVer 2.0.0", s))
	}
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, errkit.New("PKG-025", fmt.Sprintf("version %q is not strict SemVer 2.0.0 (MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD])", s))
	}
	major, _ := strconv.ParseUint(m[1], 10, 64)
	minor, _ := strconv.ParseUint(m[2], 10, 64)
	patch, _ := strconv.ParseUint(m[3], 10, 64)
	return Version{Major: major, Minor: minor, Patch: patch, Prerelease: m[4], Build: m[5], raw: s}, nil
}

// Compare returns -1, 0, or 1 as v is less than, equal to, or greater than
// other, per SemVer 2.0.0 §11 precedence. Build metadata is ignored.
func (v Version) Compare(other Version) int {
	if c := cmpUint(v.Major, other.Major); c != 0 {
		return c
	}
	if c := cmpUint(v.Minor, other.Minor); c != 0 {
		return c
	}
	if c := cmpUint(v.Patch, other.Patch); c != 0 {
		return c
	}
	return comparePrerelease(v.Prerelease, other.Prerelease)
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePrerelease implements SemVer 2.0.0 §11.4: a version without a
// prerelease has higher precedence than one with a prerelease, otherwise
// prerelease identifiers are compared dot-separated left to right.
func comparePrerelease(a, b string) int {
	if a == "" && b == "" {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	aIDs := strings.Split(a, ".")
	bIDs := strings.Split(b, ".")
	for i := 0; i < len(aIDs) && i < len(bIDs); i++ {
		c := compareIdentifier(aIDs[i], bIDs[i])
		if c != 0 {
			return c
		}
	}
	return cmpUint(uint64(len(aIDs)), uint64(len(bIDs)))
}

func compareIdentifier(a, b string) int {
	aNum, aIsNum := isNumericIdentifier(a)
	bNum, bIsNum := isNumericIdentifier(b)
	switch {
	case aIsNum && bIsNum:
		return cmpUint(aNum, bNum)
	case aIsNum && !bIsNum:
		return -1 // numeric identifiers always have lower precedence than alphanumeric
	case !aIsNum && bIsNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func isNumericIdentifier(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// sameMajorMinorPatch reports whether a and b share major.minor.patch.
func sameMajorMinorPatch(a, b Version) bool {
	return a.Major == b.Major && a.Minor == b.Minor && a.Patch == b.Patch
}
