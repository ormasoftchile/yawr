package governance

// GovernancePolicy is the interface implemented by all governance policy types.
// The runtime calls these methods during step pre-flight.
type GovernancePolicy interface {
	// CheckCommand evaluates argv[0] against the allowlist and denylist.
	// Returns (allowed, matchedRule) where matchedRule describes which rule matched.
	CheckCommand(command string) (allowed bool, matchedRule string)

	// FilterEnvVars returns the subset of vars that pass the env-var policy.
	// Removed variable names are collected in the returned slice.
	FilterEnvVars(vars map[string]string) (filtered map[string]string, blocked []string)

	// RedactionPatterns returns compiled redaction patterns for output scrubbing.
	RedactionPatterns() []*RedactionPattern
}

// AllowList is an ordered list of glob patterns that explicitly permit commands.
type AllowList struct {
	Patterns []string
}

// DenyList is an ordered list of glob patterns that explicitly block commands.
// Deny takes precedence over allow.
type DenyList struct {
	Patterns []string
}

// PolicyRule is a single governance rule contributed by an extension or host policy.
type PolicyRule struct {
	ID          string
	Description string
	Allow       AllowList
	Deny        DenyList
	DenyEnvVars []string
	Redact      []RedactionPattern
}

// RedactionPattern is a compiled pattern that scrubs sensitive values from captured output.
type RedactionPattern struct {
	Pattern     string
	Replacement string
}
