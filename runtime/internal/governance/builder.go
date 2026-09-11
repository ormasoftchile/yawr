package governance

import (
	"path"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// BuildPolicy constructs a concrete GovernancePolicy from one or more GovernanceConfig sources.
// Configs are applied in order: earlier configs are lower precedence.
// Merge semantics: deny is additive, allow is replaceable, approval is OR'd.
func BuildPolicy(configs ...*schema.GovernanceConfig) governance.GovernancePolicy {
	p := &policy{
		allow:           []string{},
		deny:            []string{},
		denyEnvVars:     []string{},
		redact:          []*governance.RedactionPattern{},
		requireApproval: false,
	}

	for _, cfg := range configs {
		if cfg == nil {
			continue
		}

		// Allow: last non-empty wins (step replaces runbook)
		if len(cfg.AllowCommands) > 0 {
			p.allow = cfg.AllowCommands
		}

		// Deny: union (additive)
		p.deny = append(p.deny, cfg.DenyCommands...)

		// DenyEnvVars: union (additive)
		p.denyEnvVars = append(p.denyEnvVars, cfg.DenyEnvVars...)

		// RequireApproval: OR (if any source requires it, the merged policy requires it)
		if cfg.RequireApproval {
			p.requireApproval = true
		}

		// Redact: concatenate (all patterns applied)
		for _, r := range cfg.Redact {
			p.redact = append(p.redact, &governance.RedactionPattern{
				Pattern:     r.Pattern,
				Replacement: r.Replace,
			})
		}
	}

	return p
}

// BuildEvaluator constructs a PolicyEvaluator from schema configs and an approval gate.
func BuildEvaluator(gate governance.ApprovalGate, configs ...*schema.GovernanceConfig) governance.PolicyEvaluator {
	policy := BuildPolicy(configs...)
	return NewEvaluator(policy, gate)
}

// policy is the concrete GovernancePolicy produced by BuildPolicy.
type policy struct {
	allow           []string // glob patterns (empty = permissive)
	deny            []string // glob patterns (always evaluated first)
	denyEnvVars     []string // glob patterns for env var names
	redact          []*governance.RedactionPattern
	requireApproval bool
}

// CheckCommand evaluates argv[0] against the allowlist and denylist.
// Returns (allowed, matchedRule) where matchedRule describes which rule matched.
func (p *policy) CheckCommand(command string) (allowed bool, matchedRule string) {
	// 1. Check deny list first (deny-wins)
	for _, pattern := range p.deny {
		if matched, _ := path.Match(pattern, command); matched {
			return false, "deny:" + pattern
		}
	}

	// 2. If allow list is empty, all commands are allowed (permissive mode)
	if len(p.allow) == 0 {
		return true, ""
	}

	// 3. If allow list is non-empty, command must match at least one pattern
	for _, pattern := range p.allow {
		if matched, _ := path.Match(pattern, command); matched {
			return true, "allow:" + pattern
		}
	}

	// 4. Command not in allow list → denied
	return false, "not-in-allowlist"
}

// FilterEnvVars returns the subset of vars that pass the env-var policy.
// Removed variable names are collected in the returned slice.
func (p *policy) FilterEnvVars(vars map[string]string) (filtered map[string]string, blocked []string) {
	if len(vars) == 0 {
		return make(map[string]string), nil
	}

	filtered = make(map[string]string)
	blocked = []string{}

	for k, v := range vars {
		isBlocked := false
		for _, pattern := range p.denyEnvVars {
			if matched, _ := path.Match(pattern, k); matched {
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

// RedactionPatterns returns compiled redaction patterns for output scrubbing.
func (p *policy) RedactionPatterns() []*governance.RedactionPattern {
	return p.redact
}

// CheckScript applies governance deny/allow checks against a run: script string.
// Unlike CheckCommand (which checks argv[0] only), this method checks the full
// script content so that patterns like "rm -rf *" can match "rm -rf /path/...".
// Path separators are intentionally permitted in the wildcard so operators can
// write meaningful deny patterns against shell invocations.
//
// CLIExecutor calls this via the ScriptChecker optional interface to avoid
// changing the public GovernancePolicy interface.
func (p *policy) CheckScript(script string) (allowed bool, matchedRule string) {
	for _, pattern := range p.deny {
		if scriptMatchPattern(pattern, script) {
			return false, "deny:" + pattern
		}
	}
	if len(p.allow) == 0 {
		return true, ""
	}
	// Allow list check: extract argv[0] from the script and test it.
	fields := strings.Fields(script)
	if len(fields) == 0 {
		return true, ""
	}
	// filepath.Base strips directory prefix from the binary name if present.
	base := fields[0]
	if idx := strings.LastIndexAny(base, "/\\"); idx >= 0 {
		base = base[idx+1:]
	}
	for _, pattern := range p.allow {
		if m, _ := path.Match(pattern, base); m {
			return true, "allow:" + pattern
		}
	}
	return false, "not-in-allowlist"
}

// scriptMatchPattern matches a governance deny pattern against a script string.
// It first tries the standard path.Match (which does not allow * to cross /),
// then applies a prefix match for patterns ending in * so that "rm -rf *"
// also matches "rm -rf /some/path/with/slashes".
func scriptMatchPattern(pattern, script string) bool {
	if m, _ := path.Match(pattern, script); m {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(script, prefix)
	}
	return false
}
