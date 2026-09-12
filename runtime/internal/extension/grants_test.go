package extension

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
)

func TestIntersectGrants_AllGranted(t *testing.T) {
	declared := []string{extension.CapabilityToolRegistration, extension.CapabilityPolicyContribution}
	granted := []string{extension.CapabilityToolRegistration, extension.CapabilityPolicyContribution}
	set := intersectGrants(declared, granted)
	if !set.Has(extension.CapabilityToolRegistration) || !set.Has(extension.CapabilityPolicyContribution) {
		t.Fatalf("expected all capabilities granted")
	}
}

func TestIntersectGrants_PartialGrant(t *testing.T) {
	declared := []string{extension.CapabilityToolRegistration, extension.CapabilityPolicyContribution}
	granted := []string{extension.CapabilityToolRegistration}
	set := intersectGrants(declared, granted)
	if !set.Has(extension.CapabilityToolRegistration) || set.Has(extension.CapabilityPolicyContribution) {
		t.Fatalf("expected partial grant")
	}
}

func TestIntersectGrants_NoneGranted(t *testing.T) {
	declared := []string{extension.CapabilityToolRegistration}
	granted := []string{extension.CapabilityPolicyContribution}
	set := intersectGrants(declared, granted)
	if len(set) != 0 {
		t.Fatalf("expected no grants")
	}
}
