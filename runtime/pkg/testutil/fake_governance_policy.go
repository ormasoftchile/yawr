package testutil

import (
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
)

// FakeGovernancePolicy is a controllable GovernancePolicy for use in unit tests.
// It implements governance.GovernancePolicy.
type FakeGovernancePolicy struct {
	// AllowAll makes CheckCommand always return (true, "").
	AllowAll bool

	// DenyCommands lists commands that CheckCommand will deny.
	DenyCommands []string

	// DenyEnvPatterns lists env var name patterns to block.
	DenyEnvPatterns []string

	// Redactions are the redaction patterns to return.
	Redactions []*governance.RedactionPattern

	// CheckCommandCalls records all CheckCommand invocations.
	CheckCommandCalls []string
}

// Ensure FakeGovernancePolicy implements governance.GovernancePolicy at compile time.
var _ governance.GovernancePolicy = (*FakeGovernancePolicy)(nil)

// CheckCommand evaluates the command against the deny list.
// Records all calls in CheckCommandCalls.
func (f *FakeGovernancePolicy) CheckCommand(command string) (bool, string) {
	f.CheckCommandCalls = append(f.CheckCommandCalls, command)
	for _, deny := range f.DenyCommands {
		if deny == command {
			return false, "deny:" + command
		}
	}
	return true, ""
}

// FilterEnvVars returns the subset of vars that pass the env-var policy.
// Uses filepath.Match to evaluate patterns against variable names.
func (f *FakeGovernancePolicy) FilterEnvVars(vars map[string]string) (map[string]string, []string) {
	filtered := make(map[string]string)
	var blocked []string
	for k, v := range vars {
		isBlocked := false
		for _, p := range f.DenyEnvPatterns {
			if matched, _ := filepath.Match(p, k); matched {
				isBlocked = true
				break
			}
		}
		if isBlocked {
			blocked = append(blocked, k)
		} else {
			filtered[k] = v
		}
	}
	return filtered, blocked
}

// RedactionPatterns returns the configured redaction patterns.
func (f *FakeGovernancePolicy) RedactionPatterns() []*governance.RedactionPattern {
	return f.Redactions
}
