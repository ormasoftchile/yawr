package extension

import (
	"errors"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
)

// ErrCapabilityViolation is returned when a capability check fails.
var ErrCapabilityViolation = errors.New("extension capability violation")

// intersectGrants returns capabilities that are both declared and granted.
func intersectGrants(declared []string, granted []string) extension.CapabilitySet {
	declaredSet := extension.NewCapabilitySet(declared)
	result := make(extension.CapabilitySet)
	for _, cap := range granted {
		if declaredSet.Has(cap) {
			result[cap] = struct{}{}
		}
	}
	return result
}

// enforce checks that the extension has the required capability.
func enforce(caps extension.CapabilitySet, required string) error {
	if !caps.Has(required) {
		return ErrCapabilityViolation
	}
	return nil
}
