package errkit

import (
	"errors"
	"testing"
)

// TestDINCsentinel verifies that every DINC-* sentinel has a correct code,
// class, and round-trips through Sentinel() and ClassSentinel().
func TestDINCsentinel(t *testing.T) {
	cases := []struct {
		sentinel *Error
		code     string
		class    string
		warning  bool
	}{
		{ErrDINC001, "DINC-001", "DINC", false},
		{ErrDINC002, "DINC-002", "DINC", false},
		{ErrDINC003, "DINC-003", "DINC", false},
		{ErrDINC004, "DINC-004", "DINC", false},
		{ErrDINC005, "DINC-005", "DINC", false},
		{ErrDINC006, "DINC-006", "DINC", false},
		{ErrDINCW007, "DINC-W007", "DINC-W", true},
		{ErrDINC008, "DINC-008", "DINC", false},
		{ErrDINC009, "DINC-009", "DINC", false},
		{ErrDINC010, "DINC-010", "DINC", false},
		{ErrDINC011, "DINC-011", "DINC", false},
		{ErrDINC012, "DINC-012", "DINC", false},
		{ErrDINC013, "DINC-013", "DINC", false},
		{ErrDINCW001, "DINC-W001", "DINC-W", true},
	}

	for _, tc := range cases {
		if tc.sentinel.Code() != tc.code {
			t.Errorf("sentinel Code() = %q, want %q", tc.sentinel.Code(), tc.code)
		}
		if tc.sentinel.Class() != tc.class {
			t.Errorf("sentinel %q Class() = %q, want %q", tc.code, tc.sentinel.Class(), tc.class)
		}
		// Sentinel() round-trip.
		got := Sentinel(tc.code)
		if got == nil {
			t.Errorf("Sentinel(%q) = nil", tc.code)
			continue
		}
		if !errors.Is(got, tc.sentinel) {
			t.Errorf("Sentinel(%q) not errors.Is with sentinel var", tc.code)
		}
		// IsWarning.
		wrapped := New(tc.code, "test message")
		if IsWarning(wrapped) != tc.warning {
			t.Errorf("IsWarning for %q = %v, want %v", tc.code, !tc.warning, tc.warning)
		}
	}
}

// TestDINCClassForCode verifies ClassForCode maps all DINC codes to the right class.
func TestDINCClassForCode(t *testing.T) {
	cases := map[string]string{
		"DINC-001":   "DINC",
		"DINC-012":   "DINC",
		"DINC-W001":  "DINC-W",
		"DINC-W999":  "DINC-W",
		"DINC-W001x": "DINC-W",
	}
	for code, want := range cases {
		got := ClassForCode(code)
		if got != want {
			t.Errorf("ClassForCode(%q) = %q, want %q", code, got, want)
		}
	}
}

// TestDINCClassSentinel verifies that DINC and DINC-W class sentinels are registered.
func TestDINCClassSentinel(t *testing.T) {
	for _, class := range []string{"DINC", "DINC-W"} {
		s := ClassSentinel(class)
		if s == nil {
			t.Errorf("ClassSentinel(%q) = nil", class)
			continue
		}
		if s.Class() != class {
			t.Errorf("ClassSentinel(%q).Class() = %q", class, s.Class())
		}
	}
}

// TestDINCInClasses verifies DINC and DINC-W appear in Classes().
func TestDINCInClasses(t *testing.T) {
	has := make(map[string]bool)
	for _, c := range Classes() {
		has[c] = true
	}
	for _, want := range []string{"DINC", "DINC-W"} {
		if !has[want] {
			t.Errorf("Classes() missing %q", want)
		}
	}
}

// TestDINCInCodes verifies all 13 DINC codes appear in Codes().
func TestDINCInCodes(t *testing.T) {
	has := make(map[string]bool)
	for _, c := range Codes() {
		has[c] = true
	}
	want := []string{
		"DINC-001", "DINC-002", "DINC-003", "DINC-004", "DINC-005",
		"DINC-006", "DINC-008", "DINC-009", "DINC-010",
		"DINC-011", "DINC-012", "DINC-013", "DINC-W001", "DINC-W007",
	}
	for _, code := range want {
		if !has[code] {
			t.Errorf("Codes() missing %q", code)
		}
	}
}
