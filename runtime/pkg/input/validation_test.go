package input

import (
	"strings"
	"testing"
)

func TestValidateFormResponse(t *testing.T) {
	minLength, maxLength := 2, 4
	min, max := 1.0, 5.0
	request := FormRequest{Fields: []FormField{
		{Name: "health", Type: "choice", Required: true, Options: []Option{{Value: "healthy"}, {Value: "unavailable"}}},
		{Name: "count", Type: "integer", Validation: &FormValidation{Min: min, Max: max, Step: 2}},
		{Name: "ratio", Type: "number"},
		{Name: "confirmed", Type: "boolean"},
		{Name: "tags", Type: "select", Multiple: true, Options: []Option{{Value: "a"}, {Value: "b"}}},
		{Name: "note", Type: "text", Validation: &FormValidation{MinLength: &minLength, MaxLength: &maxLength, Pattern: "^[a-z]+$"}},
		{Name: "credential", Type: "secret"},
		{Name: "day", Type: "date"},
		{Name: "at", Type: "datetime"},
	}}

	tests := []struct {
		name       string
		values     map[string]any
		wantErrors []string
	}{
		{
			name: "accepts complete typed values",
			values: map[string]any{
				"health": "healthy", "count": 3, "ratio": 1.5, "confirmed": false,
				"tags": []string{"a", "b"}, "note": "okay", "credential": "hidden",
				"day": "2026-08-28", "at": "2026-08-28T12:05:00Z",
			},
		},
		{name: "rejects unknown fields", values: map[string]any{"health": "healthy", "extra": "value"}, wantErrors: []string{"extra is not declared"}},
		{name: "requires declared values", values: map[string]any{}, wantErrors: []string{"health is required"}},
		{name: "enforces choice membership", values: map[string]any{"health": "degraded"}, wantErrors: []string{"health is not an allowed option"}},
		{name: "enforces primitive types", values: map[string]any{"health": "healthy", "count": 1.5, "ratio": true, "confirmed": "yes"}, wantErrors: []string{"count must be an integer", "ratio must be a number", "confirmed must be a boolean"}},
		{name: "enforces multiple values and membership", values: map[string]any{"health": "healthy", "tags": []any{"a", "c"}}, wantErrors: []string{"tags contains an unallowed option"}},
		{name: "rejects scalar for multiple field", values: map[string]any{"health": "healthy", "tags": "a"}, wantErrors: []string{"tags must be a list"}},
		{name: "enforces string constraints", values: map[string]any{"health": "healthy", "note": "A"}, wantErrors: []string{"note length below 2", "note does not match pattern"}},
		{name: "enforces numeric constraints", values: map[string]any{"health": "healthy", "count": 4}, wantErrors: []string{"count does not match step 2"}},
		{name: "enforces date formats", values: map[string]any{"health": "healthy", "day": "28/08/2026", "at": "tomorrow"}, wantErrors: []string{"day must be a date", "at must be a datetime"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errors := ValidateFormResponse(request, FormResponse{Values: test.values})
			if len(errors) != len(test.wantErrors) {
				t.Fatalf("errors = %v, want %v", errors, test.wantErrors)
			}
			for index, want := range test.wantErrors {
				if !strings.Contains(errors[index], want) {
					t.Fatalf("errors[%d] = %q, want substring %q", index, errors[index], want)
				}
			}
		})
	}
}
