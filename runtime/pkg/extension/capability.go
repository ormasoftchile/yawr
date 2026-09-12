package extension

// Capability constants (from spec §04).
const (
	CapabilityToolRegistration     = "capability/tool-registration"
	CapabilityPolicyContribution   = "capability/policy-contribution"
	CapabilityProviderRegistration = "capability/provider-registration"
	CapabilityReadEnv              = "capability/read-env"
	CapabilityReadFiles            = "capability/read-files"
	CapabilityExecProcess          = "capability/exec-process"
)

// CapabilitySet is a set of granted capabilities.
type CapabilitySet map[string]struct{}

// NewCapabilitySet constructs a set from a slice.
func NewCapabilitySet(caps []string) CapabilitySet {
	set := make(CapabilitySet)
	for _, cap := range caps {
		set[cap] = struct{}{}
	}
	return set
}

// Has reports whether the capability exists in the set.
func (cs CapabilitySet) Has(cap string) bool {
	_, ok := cs[cap]
	return ok
}
