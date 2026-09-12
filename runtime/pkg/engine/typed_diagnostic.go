package engine

import "fmt"

// TypedDiagnostic deliberately never contains a raw runtime value.
type TypedDiagnostic struct {
	Code       string        `json:"code"`
	Origin     ResultsOrigin `json:"origin"`
	Path       string        `json:"path"`
	Expected   string        `json:"expected"`
	ActualType string        `json:"actual_type"`
	Preview    string        `json:"value_preview"`
	Cause      error         `json:"-"`
}

func (diagnostic *TypedDiagnostic) Error() string {
	return fmt.Sprintf("%s: %s: %s: expected %s, actual %s (value redacted)", diagnostic.Code, diagnostic.Origin.NodeID, diagnostic.Path, diagnostic.Expected, diagnostic.ActualType)
}

func (diagnostic *TypedDiagnostic) Unwrap() error { return diagnostic.Cause }
