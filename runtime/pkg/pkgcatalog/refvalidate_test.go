package pkgcatalog

import (
	"testing"
)

// TestValidateRenderedRef covers the TV-DYN-REF cases and rejection rules.
func TestValidateRenderedRef(t *testing.T) {
	type tc struct {
		name     string
		ref      string
		wantKind RefValidationKind
	}

	cases := []tc{
		// Happy paths.
		{"bare id", "tsg-disk-pressure", RefValidationOK},
		{"package-qualified id", "contoso-tsgs/tsg-disk-pressure", RefValidationOK},
		{"bare with underscore", "tsg_disk_pressure", RefValidationOK},
		{"bare with digits", "tsg123", RefValidationOK},
		{"exactly one slash", "pkg/id", RefValidationOK},

		// Empty / whitespace (DINC-002 territory).
		{"empty string (TV-DYN-REF-011)", "", RefValidationEmpty},
		{"whitespace only (TV-DYN-REF-012)", "   ", RefValidationEmpty},
		{"tab only", "\t", RefValidationEmpty},

		// Path-like (DINC-001 territory).
		{"relative path ./x (TV-DYN-REF-003)", "./child", RefValidationInvalid},
		{"parent traversal ../x (TV-DYN-REF-004)", "../escape", RefValidationInvalid},
		{"absolute POSIX path (TV-DYN-REF-005)", "/etc/passwd", RefValidationInvalid},
		{"absolute Windows path (TV-DYN-REF-006)", `C:\runbooks\tsg.yaml`, RefValidationInvalid},
		{"backslash separator (TV-DYN-REF-007)", `contoso\tsg-disk`, RefValidationInvalid},
		{"three-segment path (TV-DYN-REF-008)", "pkg/sub/id", RefValidationInvalid},
		{"file:// scheme (TV-DYN-REF-009)", "file:///opt/runbooks/tsg.yaml", RefValidationInvalid},
		{"http:// scheme (TV-DYN-REF-010)", "http://internal/runbooks/tsg", RefValidationInvalid},
		{"https:// scheme", "https://example.com/tsg", RefValidationInvalid},
		{"null byte (TV-DYN-REF-013)", "tsg-disk\x00pressure", RefValidationInvalid},
		{"trailing slash (TV-DYN-REF-014)", "contoso-tsgs/", RefValidationInvalid},
		{"leading slash (TV-DYN-REF-015)", "/tsg-disk-pressure", RefValidationInvalid},
		{"dot segment (TV-DYN-REF-016)", ".", RefValidationInvalid},
		{"double-dot segment (TV-DYN-REF-017)", "..", RefValidationInvalid},
		{"dot as second segment (TV-DYN-REF-018)", "pkg/.", RefValidationInvalid},
		{"double-dot as second segment (TV-DYN-REF-019)", "pkg/..", RefValidationInvalid},
		{"RTL override U+202E (TV-DYN-REF-021)", "contoso/tsg\u202e", RefValidationInvalid},
		{"control char U+0001 (TV-DYN-REF-025)", "tsg\u0001disk", RefValidationInvalid},
		{"tab in id (TV-DYN-REF-026)", "tsg\tdisk", RefValidationInvalid},
		{"newline in id (TV-DYN-REF-027)", "tsg\ndisk", RefValidationInvalid},
		{"space in id (TV-DYN-REF-028)", "tsg disk", RefValidationInvalid},
		{"non-ASCII letter (homoglyph, Cyrillic с)", "\u0441ontoso/tsg", RefValidationInvalid},
		{"non-NFC NFD combining form", "cafe\u0301", RefValidationInvalid},
		{"at-sign in id", "pkg@version", RefValidationInvalid},
		{"colon in id", "pkg:id", RefValidationInvalid},

		// Length limit (TV-DYN-REF-024): 257 bytes of 'a' chars → invalid.
		{"too long id", string(func() []byte {
			b := make([]byte, 257)
			for i := range b {
				b[i] = 'a'
			}
			return b
		}()), RefValidationInvalid},

		// Edge: exactly at limit (256 ASCII chars is invalid because it's not a valid safe-id — all 'a').
		// 256 × 'a' is within limit so actually OK by length, but let's keep TV-DYN-REF-024 at 257.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, kind := ValidateRenderedRef(tc.ref)
			if kind != tc.wantKind {
				t.Errorf("ValidateRenderedRef(%q) kind = %v, want %v", tc.ref, kind, tc.wantKind)
			}
		})
	}
}

// TestValidateRenderedRefReason verifies that invalid cases return a non-empty reason.
func TestValidateRenderedRefReason(t *testing.T) {
	invalidCases := []string{"./child", "../escape", "/abs", `C:\win`, "pkg/sub/id", "http://x"}
	for _, ref := range invalidCases {
		reason, kind := ValidateRenderedRef(ref)
		if kind == RefValidationOK {
			t.Errorf("ValidateRenderedRef(%q) unexpectedly returned OK", ref)
			continue
		}
		if reason == "" {
			t.Errorf("ValidateRenderedRef(%q) kind=%v but reason is empty", ref, kind)
		}
	}
}
