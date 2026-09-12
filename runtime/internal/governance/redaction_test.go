package governance

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

func TestRedactor_SimplePattern(t *testing.T) {
	patterns := []*governance.RedactionPattern{
		{Pattern: "token=[A-Za-z0-9]+", Replacement: "token=[REDACTED]"},
	}

	redactor, err := NewRedactor(patterns)
	if err != nil {
		t.Fatalf("NewRedactor failed: %v", err)
	}

	input := "auth with token=abc123 here"
	output, matches := redactor.RedactString(input)

	if matches != 1 {
		t.Errorf("expected 1 match, got %d", matches)
	}
	if output != "auth with token=[REDACTED] here" {
		t.Errorf("unexpected output: %s", output)
	}
}

func TestRedactor_MultiplePatterns(t *testing.T) {
	patterns := []*governance.RedactionPattern{
		{Pattern: "password=\\w+", Replacement: "password=[REDACTED]"},
		{Pattern: "api_key=\\w+", Replacement: "api_key=[REDACTED]"},
	}

	redactor, err := NewRedactor(patterns)
	if err != nil {
		t.Fatalf("NewRedactor failed: %v", err)
	}

	input := "creds: password=secret123 and api_key=xyz789"
	output, matches := redactor.RedactString(input)

	if matches != 2 {
		t.Errorf("expected 2 matches, got %d", matches)
	}
	if output != "creds: password=[REDACTED] and api_key=[REDACTED]" {
		t.Errorf("unexpected output: %s", output)
	}
}

func TestRedactor_RecursiveMapRedaction(t *testing.T) {
	patterns := []*governance.RedactionPattern{
		{Pattern: "secret-\\w+", Replacement: "[REDACTED]"},
	}

	redactor, err := NewRedactor(patterns)
	if err != nil {
		t.Fatalf("NewRedactor failed: %v", err)
	}

	m := map[string]any{
		"level1": "has secret-abc",
		"level2": map[string]any{
			"nested": "also secret-xyz",
		},
	}

	matches := redactor.RedactMap(m)
	if matches != 2 {
		t.Errorf("expected 2 matches, got %d", matches)
	}
	if m["level1"] != "has [REDACTED]" {
		t.Errorf("level1 not redacted: %v", m["level1"])
	}
	nested := m["level2"].(map[string]any)
	if nested["nested"] != "also [REDACTED]" {
		t.Errorf("nested not redacted: %v", nested["nested"])
	}
}

func TestRedactor_NoMatch_NoChange(t *testing.T) {
	patterns := []*governance.RedactionPattern{
		{Pattern: "secret=\\w+", Replacement: "[REDACTED]"},
	}

	redactor, err := NewRedactor(patterns)
	if err != nil {
		t.Fatalf("NewRedactor failed: %v", err)
	}

	input := "this has no sensitive data"
	output, matches := redactor.RedactString(input)

	if matches != 0 {
		t.Errorf("expected 0 matches, got %d", matches)
	}
	if output != input {
		t.Errorf("output should be unchanged")
	}
}
