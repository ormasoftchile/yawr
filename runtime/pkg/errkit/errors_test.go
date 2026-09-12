package errkit

import (
	"errors"
	"testing"
)

func TestSentinelsExposeClassAndCode(t *testing.T) {
	for _, code := range Codes() {
		sentinel := Sentinel(code)
		if sentinel == nil {
			t.Fatalf("Sentinel(%q) = nil", code)
		}
		if got := sentinel.Code(); got != code {
			t.Fatalf("Code() = %q, want %q", got, code)
		}
		if got := sentinel.Class(); got != ClassForCode(code) || got == "" {
			t.Fatalf("Class() = %q for %q", got, code)
		}
	}
}

func TestErrorsIsMatchesSentinel(t *testing.T) {
	wrapped := Wrap("GXL-PATH-001", "missing variable", errors.New("cause"))
	if !errors.Is(wrapped, ErrUnknownVariable) {
		t.Fatal("errors.Is did not match ErrUnknownVariable")
	}
	if errors.Is(wrapped, ErrTypeMismatch) {
		t.Fatal("errors.Is matched unrelated sentinel")
	}
}

func TestDeclaredClassesCovered(t *testing.T) {
	for _, class := range Classes() {
		sentinel := ClassSentinel(class)
		if sentinel == nil {
			t.Fatalf("ClassSentinel(%q) = nil", class)
		}
		if got := sentinel.Class(); got != class {
			t.Fatalf("Class() = %q, want %q", got, class)
		}
		if !errors.Is(New(class+"-999", "example"), sentinel) {
			t.Fatalf("errors.Is did not match class sentinel %s", class)
		}
	}
}

func TestCommonAliases(t *testing.T) {
	if ErrUnknownVariable.Code() != "GXL-PATH-001" {
		t.Fatalf("ErrUnknownVariable = %s", ErrUnknownVariable.Code())
	}
	if ErrTypeMismatch.Code() != "GXL-TYPE-001" {
		t.Fatalf("ErrTypeMismatch = %s", ErrTypeMismatch.Code())
	}
	if ErrParseInvalidEscape.Code() != "GXL-PARSE-004" {
		t.Fatalf("ErrParseInvalidEscape = %s", ErrParseInvalidEscape.Code())
	}
}
