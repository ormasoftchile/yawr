package parser

import (
	"fmt"
	"strings"
)

// ValidationError is a structured parse or validation error.
type ValidationError struct {
	// Code is a machine-readable error code (e.g. "parallel/nested-forbidden").
	Code string
	// Field is the JSON-path-style location of the offending field (may be empty).
	Field string
	// Message is a human-readable description.
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("[%s] %s: %s", e.Code, e.Field, e.Message)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// ValidationErrors is a slice of *ValidationError that itself implements error.
type ValidationErrors []*ValidationError

func (ve ValidationErrors) Error() string {
	if len(ve) == 0 {
		return "no validation errors"
	}
	if len(ve) == 1 {
		return ve[0].Error()
	}
	msgs := make([]string, len(ve))
	for i, e := range ve {
		msgs[i] = "  - " + e.Error()
	}
	return fmt.Sprintf("%d validation errors:\n%s", len(ve), strings.Join(msgs, "\n"))
}

// verr is a convenience constructor for *ValidationError.
func verr(code, field, msg string) *ValidationError {
	return &ValidationError{Code: code, Field: field, Message: msg}
}
