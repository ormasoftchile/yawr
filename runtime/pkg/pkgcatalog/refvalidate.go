package pkgcatalog

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// maxRefLength is the maximum number of bytes permitted in a rendered runbook_ref.
const maxRefLength = 256

// ValidateRenderedRef checks that a rendered runbook_ref value is a syntactically
// valid runbook identity before any catalog lookup is attempted. The
// dynamic-include contract §1.2, a valid identity is either:
//
//   - a bare id matching ^[A-Za-z0-9_-]+$, or
//   - a package-qualified id exactly of the form <package>/<id> where both
//     segments match ^[A-Za-z0-9_-]+$.
//
// Anything that looks like a filesystem path, URL, or contains invalid
// characters is rejected with a descriptive error string. The caller is
// responsible for wrapping the returned message in the appropriate DINC-001
// or DINC-002 errkit error.
//
// Return values:
//   - ("", nil): value is valid and NFC-normalised; proceed to lookup.
//   - ("", ErrEmpty): value is empty or whitespace-only (DINC-002 territory).
//   - (reason, ErrInvalid): value is syntactically invalid (DINC-001 territory).
//
// The returned reason is a human-readable explanation suitable for an error message.
func ValidateRenderedRef(ref string) (reason string, kind RefValidationKind) {
	// Empty / whitespace-only → separate kind so callers can use DINC-002.
	if strings.TrimSpace(ref) == "" {
		return "empty or whitespace-only reference", RefValidationEmpty
	}

	// Length guard (covers TV-DYN-REF-024).
	if len(ref) > maxRefLength {
		return "reference exceeds maximum length", RefValidationInvalid
	}

	// Null byte.
	if strings.ContainsRune(ref, '\x00') {
		return "reference contains null byte", RefValidationInvalid
	}

	// Control characters and whitespace within string (catches tabs, newlines,
	// carriage returns, and all Unicode control / bidi-override codepoints).
	for i, r := range ref {
		_ = i
		if r == '/' {
			continue // slash handled below
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "reference contains control or formatting character", RefValidationInvalid
		}
		if unicode.IsSpace(r) {
			return "reference contains whitespace", RefValidationInvalid
		}
	}

	// Reject anything that starts with a URI scheme: <alpha>+://.
	if idx := strings.Index(ref, "://"); idx > 0 {
		// Verify the prefix is all alpha.
		allAlpha := true
		for _, r := range ref[:idx] {
			if !unicode.IsLetter(r) {
				allAlpha = false
				break
			}
		}
		if allAlpha {
			return "reference looks like a URL (contains ://)", RefValidationInvalid
		}
	}

	// Backslash → Windows path.
	if strings.ContainsRune(ref, '\\') {
		return "reference contains backslash (Windows path?)", RefValidationInvalid
	}

	// Count slashes; permit at most one (package-qualified form).
	slashCount := strings.Count(ref, "/")
	if slashCount > 1 {
		return "reference contains more than one slash (path-like)", RefValidationInvalid
	}

	// Starts or ends with slash.
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return "reference starts or ends with slash", RefValidationInvalid
	}

	// Split into segments and validate each.
	var segments []string
	if slashCount == 1 {
		parts := strings.SplitN(ref, "/", 2)
		segments = parts
	} else {
		segments = []string{ref}
	}

	for _, seg := range segments {
		if reason := validateSegment(seg); reason != "" {
			return reason, RefValidationInvalid
		}
	}

	// NFC normalisation check: the ref must already be valid UTF-8 and NFC.
	if !utf8.ValidString(ref) {
		return "reference is not valid UTF-8", RefValidationInvalid
	}
	if norm.NFC.String(ref) != ref {
		// Non-NFC input (e.g. NFD combining characters).
		// ruling (adopted: NFC normalisation at lookup; reject non-NFC at
		// syntax validation so callers get a clear error rather than a
		// confusing miss).
		return "reference is not NFC-normalised", RefValidationInvalid
	}

	// All checks passed.
	return "", RefValidationOK
}

// validateSegment checks that a single identity segment (no slash) conforms
// to the safe-id profile: ^[A-Za-z0-9_-]+$ after checking for dot-traversal.
func validateSegment(seg string) string {
	if seg == "" {
		return "identity segment is empty"
	}
	// Dot-traversal segments.
	if seg == "." || seg == ".." {
		return "identity segment is a path traversal component (. or ..)"
	}
	// Safe-id character profile: ASCII alphanumeric, underscore, hyphen.
	for _, r := range seg {
		if !isSafeIDRune(r) {
			return "reference contains character outside safe-id profile [A-Za-z0-9_-]"
		}
	}
	return ""
}

// isSafeIDRune reports whether r is permitted in an identity segment.
// Permitted: ASCII letters, digits, underscore, hyphen.
func isSafeIDRune(r rune) bool {
	return (r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9') ||
		r == '_' || r == '-'
}

// RefValidationKind categorises the outcome of ValidateRenderedRef.
type RefValidationKind int

const (
	// RefValidationOK means the ref is syntactically valid.
	RefValidationOK RefValidationKind = iota
	// RefValidationEmpty means the ref is empty or whitespace-only (DINC-002).
	RefValidationEmpty
	// RefValidationInvalid means the ref is syntactically invalid (DINC-001).
	RefValidationInvalid
)
